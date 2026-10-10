package builder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/huhen/lampa-web-builder/internal/config"
	"github.com/huhen/lampa-web-builder/internal/state"
	"github.com/huhen/lampa-web-builder/internal/testutil"
)

const (
	c1 = "1111111111111111111111111111111111111111"
	c2 = "2222222222222222222222222222222222222222"
)

type fakeUpstream struct {
	mu          sync.Mutex
	head        string
	headErr     error
	cloneErr    error
	fetchErr    error
	headGate    chan struct{}
	headStarted chan struct{}
	clones      []string
}

func (f *fakeUpstream) LsRemoteHead(ctx context.Context) (string, error) {
	if f.headStarted != nil {
		select {
		case f.headStarted <- struct{}{}:
		default:
		}
	}
	if f.headGate != nil {
		select {
		case <-f.headGate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

func (f *fakeUpstream) EnsureCopy(_ context.Context, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones = append(f.clones, dir)
	return f.cloneErr
}

func (f *fakeUpstream) Fetch(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchErr
}

type fakeBuildRun struct {
	mu       sync.Mutex
	gate     chan struct{}
	failMsg  string
	starts   []string
	workdirs []string
}

func (f *fakeBuildRun) Run(ctx context.Context, workDir, commit, domain, archivePath string, log io.Writer) error {
	f.mu.Lock()
	f.starts = append(f.starts, domain+"@"+commit)
	f.workdirs = append(f.workdirs, workDir)
	fail, gate := f.failMsg, f.gate
	f.mu.Unlock()
	fmt.Fprintf(log, "fake build workdir=%s commit=%s domain=%s\n", workDir, commit, domain)
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The worker can wake up after cancel yet have the select pick the closed
	// gate (TestShutdownDoesNotPoisonBadCommit cancels and closes it at once).
	// A cancelled build must not report success, or waitBuildStatus(failed)
	// could flake and bad_commit could be poisoned by a shutdown.
	if err := ctx.Err(); err != nil {
		return err
	}
	if fail != "" {
		return errors.New(fail)
	}
	return os.WriteFile(archivePath, []byte("archive of "+domain), 0o644)
}

// StartsSnapshot returns a copy of the recorded "domain@commit" starts.
func (f *fakeBuildRun) StartsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.starts)
}

// WorkdirsSnapshot returns the working copies builds started in, in order.
func (f *fakeBuildRun) WorkdirsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.workdirs)
}

func newTestBuilder(t *testing.T, startWorker bool) (*Builder, *fakeUpstream, *fakeBuildRun, *state.Store) {
	t.Helper()
	cfg := config.Config{
		Listen:        ":0",
		APIKey:        "k",
		DataDir:       t.TempDir(),
		AssetsDir:     t.TempDir(),
		CacheSize:     2,
		DefaultDomain: "test.example",
	}
	store, err := state.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	git := &fakeUpstream{}
	pipe := &fakeBuildRun{}
	b := New(cfg, store, git, pipe)
	if startWorker {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { b.RunWorker(ctx); close(done) }()
		// The worker must be fully stopped before t.TempDir removal (cleanups
		// run LIFO): a test may return while a build it just triggered is
		// still running, and a live worker racing TempDir's RemoveAll makes
		// the cleanup flake with "directory not empty".
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("worker did not exit after cancel")
			}
		})
	}
	return b, git, pipe, store
}

func buildByID(st state.State, id string) (state.BuildInfo, bool) {
	for _, b := range st.Builds {
		if b.ID == id {
			return b, true
		}
	}
	return state.BuildInfo{}, false
}

func waitBuildStatus(t *testing.T, s *state.Store, id, want string) {
	t.Helper()
	testutil.WaitFor(t, func() bool {
		b, ok := buildByID(s.Get(), id)
		return ok && b.Status == want
	}, fmt.Sprintf("build %s -> %s", id, want))
}

func TestOrderBuildNoAvailableCommit(t *testing.T) {
	b, _, _, _ := newTestBuilder(t, false)
	if _, _, err := b.OrderBuild("d.example"); !errors.Is(err, ErrNoCommit) {
		t.Errorf("err = %v, want ErrNoCommit", err)
	}
}

func TestCheckTriggersTestBuildAndSwap(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	git.head = c1

	triggered, err := b.CheckNow(context.Background())
	if err != nil || !triggered {
		t.Fatalf("check = %v, %v; want triggered", triggered, err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "available commit set")
	if cur := store.Get().Current; cur != "b" {
		t.Errorf("current = %q, want b (swapped to the test copy)", cur)
	}

	// Ordered build for a fresh domain runs and lands in the cache.
	id, cached, err := b.OrderBuild("d.example")
	if err != nil || cached {
		t.Fatalf("order = %q, %v, %v; want fresh", id, cached, err)
	}
	waitBuildStatus(t, store, id, state.StatusSuccess)
	f, err := b.OpenArchive(id)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	if _, err := f.Stat(); err != nil {
		t.Errorf("archive missing: %v", err)
	}

	// Second order of the same domain is a cache hit with the same id.
	id2, cached, err := b.OrderBuild("d.example")
	if err != nil || !cached || id2 != id {
		t.Errorf("order = %q, %v, %v; want cached %q", id2, cached, err, id)
	}

	// A successful test build with the requested domain is also served from
	// the cache (spec requirement 7).
	id3, cached, err := b.OrderBuild("test.example")
	if err != nil || !cached {
		t.Errorf("test.example order = %q, %v, %v; want cached", id3, cached, err)
	}
}

func TestOrderRejectedWhileTestBuildPending(t *testing.T) {
	b, git, _, _ := newTestBuilder(t, false) // no worker: pending stays pending
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.OrderBuild("d.example"); !errors.Is(err, ErrTestInProgress) {
		t.Errorf("err = %v, want ErrTestInProgress", err)
	}
	// A second check while a test build is pending is also a 409-mapped error.
	if _, err := b.CheckNow(context.Background()); !errors.Is(err, ErrTestInProgress) {
		t.Errorf("err = %v, want ErrTestInProgress", err)
	}
}

func TestQueueCapacityAndDedup(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "available commit set")

	pipe.mu.Lock()
	pipe.gate = make(chan struct{})
	pipe.mu.Unlock()
	id1, _, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id1, state.StatusRunning)
	if id1b, cached, _ := b.OrderBuild("d1.example"); !cached || id1b != id1 {
		t.Errorf("dedup = %q,%v; want %q,true", id1b, cached, id1)
	}

	var ids []string
	for _, d := range []string{"d2.example", "d3.example", "d4.example"} {
		id, _, err := b.OrderBuild(d)
		if err != nil {
			t.Fatalf("order %s: %v", d, err)
		}
		ids = append(ids, id)
	}
	if _, _, err := b.OrderBuild("d5.example"); !errors.Is(err, ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}

	pipe.mu.Lock()
	close(pipe.gate)
	pipe.mu.Unlock()
	for _, id := range ids {
		waitBuildStatus(t, store, id, state.StatusSuccess)
	}
}

// Orders queued behind one gated build must start in FIFO order: the test
// build (startup) first, then d1, d2, d3. d2 and d3 both wait in the queue
// while d1 runs, so a back-pop regression in the queue would fail this
// (issue #12).
func TestQueueFIFO(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "test build done")

	pipe.mu.Lock()
	pipe.gate = make(chan struct{})
	pipe.mu.Unlock()
	id1, _, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id1, state.StatusRunning)
	id2, _, err := b.OrderBuild("d2.example")
	if err != nil {
		t.Fatal(err)
	}
	id3, _, err := b.OrderBuild("d3.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{id2, id3} {
		if _, ok := buildByID(store.Get(), id); !ok {
			t.Fatalf("queued record %s missing", id)
		}
	}
	pipe.mu.Lock()
	close(pipe.gate)
	pipe.mu.Unlock()
	for _, id := range []string{id1, id2, id3} {
		waitBuildStatus(t, store, id, state.StatusSuccess)
	}

	want := []string{
		"test.example@" + c1,
		"d1.example@" + c1,
		"d2.example@" + c1,
		"d3.example@" + c1,
	}
	if got := pipe.StartsSnapshot(); !slices.Equal(got, want) {
		t.Errorf("starts = %q, want %q (FIFO)", got, want)
	}
}

func TestBadCommitFlow(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	pipe.mu.Lock()
	pipe.failMsg = "patch does not apply"
	pipe.mu.Unlock()

	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().BadCommit == c1 }, "bad commit recorded")
	if st := store.Get(); st.AvailableCommit != "" || st.Current != "a" {
		t.Errorf("available=%q current=%q; want empty,a", st.AvailableCommit, st.Current)
	}

	// Same head again: nothing new to do.
	if triggered, err := b.CheckNow(context.Background()); err != nil || triggered {
		t.Errorf("check = %v, %v; want not triggered", triggered, err)
	}

	// Next upstream commit builds fine.
	pipe.mu.Lock()
	pipe.failMsg = ""
	pipe.mu.Unlock()
	git.head = c2
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c2 }, "available commit advanced")
	if st := store.Get(); st.BadCommit != "" {
		t.Errorf("bad commit must be reset on success, got %q", st.BadCommit)
	}
}

func TestEvictionKeepsPinnedTestBuild(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	b.cacheSize = 1
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "available commit set")
	var testID string
	testutil.WaitFor(t, func() bool {
		for _, bd := range store.Get().Builds {
			if bd.Kind == state.KindTest && bd.Status == state.StatusSuccess {
				testID = bd.ID
				return true
			}
		}
		return false
	}, "test build success")

	id1, _, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id1, state.StatusSuccess)
	id2, _, err := b.OrderBuild("d2.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id2, state.StatusSuccess)

	// Eviction runs right after the success record lands; wait for the
	// removal instead of racing it — the record is visible before evict()
	// finishes removing the dir.
	testutil.WaitFor(t, func() bool {
		_, err := os.Stat(b.buildDir(id1))
		return os.IsNotExist(err)
	}, "d1 dir evicted")
	for _, keep := range []string{testID, id2} {
		if _, err := os.Stat(b.buildDir(keep)); err != nil {
			t.Errorf("dir %s must survive: %v", keep, err)
		}
	}
}

func TestCheckBusy(t *testing.T) {
	b, git, _, _ := newTestBuilder(t, false)
	git.headStarted = make(chan struct{}, 1)
	git.headGate = make(chan struct{})
	git.head = c1

	done := make(chan struct{})
	go func() { b.CheckNow(context.Background()); close(done) }()
	// CheckNow takes checkMu before calling LsRemoteHead, so observing the
	// headStarted signal proves the lock is held — no sleep needed.
	select {
	case <-git.headStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first check never reached LsRemoteHead")
	}
	if _, err := b.CheckNow(context.Background()); !errors.Is(err, ErrCheckBusy) {
		t.Errorf("err = %v, want ErrCheckBusy", err)
	}
	close(git.headGate)
	<-done
}

func TestUpstreamErrorsRecordedInPoll(t *testing.T) {
	b, git, _, store := newTestBuilder(t, false)
	git.mu.Lock()
	git.headErr = errors.New("network down")
	git.mu.Unlock()
	if _, err := b.CheckNow(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if st := store.Get(); st.Poll.LastError == "" {
		t.Error("poll.last_error not recorded")
	}
	// Recovery clears the error.
	git.mu.Lock()
	git.headErr = nil
	git.head = c1
	git.mu.Unlock()
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := store.Get(); st.Poll.LastError != "" {
		t.Errorf("poll.last_error = %q, want empty", st.Poll.LastError)
	}
}

func TestStatusShape(t *testing.T) {
	b, git, _, _ := newTestBuilder(t, false)
	git.head = c1
	b.CheckNow(context.Background())
	st := b.Status()
	if st.Version != "" {
		t.Errorf("Status must not fill Version (the api adds it), got %q", st.Version)
	}
	if st.Poll.Interval != "0" {
		t.Errorf("poll.interval = %q, want 0", st.Poll.Interval)
	}
	if st.LatestSeenCommit != c1 {
		t.Errorf("latest_seen_commit = %q, want %s", st.LatestSeenCommit, c1)
	}
}

func TestOrderAfterSwapBuildsCurrentCommit(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "available commit set")

	// Hold the runner with d1 so that d2 stays queued, then push a new
	// upstream commit through a test build while d2 waits in the queue.
	pipe.mu.Lock()
	pipe.gate = make(chan struct{})
	pipe.mu.Unlock()
	id1, _, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id1, state.StatusRunning)
	if _, _, err := b.OrderBuild("d2.example"); err != nil {
		t.Fatal(err)
	}
	git.head = c2
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The gated d1 completes first, then the test build of c2 swaps the
	// commit, then d2 runs against the new available commit.
	pipe.mu.Lock()
	close(pipe.gate)
	pipe.mu.Unlock()
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c2 }, "available advanced to c2")

	var id string
	testutil.WaitFor(t, func() bool {
		bd, ok := findByDomain(store.Get(), "d2.example", state.StatusSuccess)
		if ok {
			id = bd.ID
		}
		return ok
	}, "d2 build success")
	bd, ok := buildByID(store.Get(), id)
	if !ok || bd.Commit != c2 {
		t.Errorf("d2 record commit = %q (found=%v), want %s (built after the swap)", bd.Commit, ok, c2)
	}
	// The archive d2 owns is really the one served from the cache.
	if id2, cached, err := b.OrderBuild("d2.example"); err != nil || !cached || id2 != id {
		t.Errorf("reorder = %q, %v, %v; want cache hit %q", id2, cached, err, id)
	}
}

// Unit counterpart of the integration swap test: the second test build must
// run on the copy the first one left free — b then a (issue #12).
func TestTestBuildSwapBack(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "first test build done")
	if cur := store.Get().Current; cur != "b" {
		t.Fatalf("current = %q, want b", cur)
	}

	git.head = c2
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c2 }, "second test build done")
	if cur := store.Get().Current; cur != "a" {
		t.Errorf("current = %q, want a (swapped back)", cur)
	}

	want := []string{b.workDir("b"), b.workDir("a")}
	if got := pipe.WorkdirsSnapshot(); !slices.Equal(got, want) {
		t.Errorf("test build workdirs = %q, want %q", got, want)
	}
}

func TestShutdownDoesNotPoisonBadCommit(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, false)
	git.head = c1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workerDone := make(chan struct{})
	go func() { b.RunWorker(ctx); close(workerDone) }()

	pipe.mu.Lock()
	pipe.gate = make(chan struct{})
	pipe.mu.Unlock()
	if triggered, err := b.CheckNow(context.Background()); err != nil || !triggered {
		t.Fatalf("check = %v, %v; want triggered", triggered, err)
	}
	var testID string
	testutil.WaitFor(t, func() bool {
		bd, ok := findByKindStatus(store.Get(), state.KindTest, state.StatusRunning)
		if ok {
			testID = bd.ID
		}
		return ok
	}, "test build running")

	// Shut down mid-build, then let the pipeline observe the cancelled ctx.
	cancel()
	pipe.mu.Lock()
	close(pipe.gate)
	pipe.mu.Unlock()
	waitBuildStatus(t, store, testID, state.StatusFailed)
	select {
	case <-workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit after ctx cancel")
	}

	if st := store.Get(); st.BadCommit != "" {
		t.Errorf("bad_commit = %q after shutdown, want empty (cancel is not proof of a bad commit)", st.BadCommit)
	}
}

func TestUpstreamCloneFailureRetries(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	git.mu.Lock()
	git.cloneErr = errors.New("clone boom")
	git.mu.Unlock()
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool {
		_, ok := findByKindStatus(store.Get(), state.KindTest, state.StatusFailed)
		return ok
	}, "test build failed")
	// A clone outage proves nothing about the patches: no bad_commit, and
	// latest_seen is re-armed to available so the head looks new again.
	if st := store.Get(); st.BadCommit != "" {
		t.Errorf("bad_commit = %q after clone failure, want empty", st.BadCommit)
	}
	testutil.WaitFor(t, func() bool { return store.Get().LatestSeenCommit == "" }, "latest_seen re-armed to available")

	// Once the infrastructure recovers, the same commit is retried.
	git.mu.Lock()
	git.cloneErr = nil
	git.mu.Unlock()
	triggered, err := b.CheckNow(context.Background())
	if err != nil || !triggered {
		t.Errorf("retry check = %v, %v; want triggered", triggered, err)
	}
}

func TestArchiveAndLogAvailabilityForFailedBuild(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	git.mu.Lock()
	git.cloneErr = errors.New("clone boom")
	git.mu.Unlock()
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var failedID string
	testutil.WaitFor(t, func() bool {
		bd, ok := findByKindStatus(store.Get(), state.KindTest, state.StatusFailed)
		if ok {
			failedID = bd.ID
		}
		return ok
	}, "test build failed")

	// A failed build owns no archive: 404 semantics for the API.
	if f, err := b.OpenArchive(failedID); !errors.Is(err, os.ErrNotExist) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenArchive(failed) err = %v, want os.ErrNotExist", err)
	}

	// A successful build's log opens: after the failed test build, the next
	// upstream commit builds fine and gets a log file.
	git.mu.Lock()
	git.cloneErr = nil
	git.mu.Unlock()
	git.head = c2
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var successID string
	testutil.WaitFor(t, func() bool {
		bd, ok := findByKindStatus(store.Get(), state.KindTest, state.StatusSuccess)
		if ok {
			successID = bd.ID
		}
		return ok
	}, "test build success")
	f, err := b.OpenLog(successID)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer f.Close()
	if _, err := f.Stat(); err != nil {
		t.Errorf("log file stat: %v", err)
	}
}

func TestPollerImmediateCheckThenExit(t *testing.T) {
	b, git, _, store := newTestBuilder(t, false)
	git.head = c1 // PollInterval is 0 in the test config

	done := make(chan struct{})
	go func() { b.RunPoller(context.Background()); close(done) }()
	testutil.WaitFor(t, func() bool { return store.Get().LatestSeenCommit == c1 }, "startup check ran")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunPoller must exit when PollInterval is 0")
	}
}

// RunPoller must also check on every tick, not only at startup; the startup
// check of c1 triggers a test build, so the second signal out of
// LsRemoteHead proves the ticker path ran (issue #12).
func TestPollerTicker(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	b.cfg.PollInterval = 10 * time.Millisecond
	git.head = c1
	// Buffered so fast ticks cannot drop the signals the test consumes (any
	// size >= 1 works).
	git.headStarted = make(chan struct{}, 8)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunPoller(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("RunPoller did not exit after cancel")
		}
	})

	for i := 0; i < 2; i++ {
		select {
		case <-git.headStarted:
		case <-time.After(5 * time.Second):
			t.Fatalf("LsRemoteHead call %d never happened", i+1)
		}
	}
	// The startup check completed the first test build before the ticker could
	// reach LsRemoteHead again (CheckNow returns early while a test build is
	// pending), so the second signal above is what proves the ticker path ran.
	// CheckNow records poll.last_checked after any successful LsRemoteHead —
	// the startup check alone already sets it — so this assertion does not
	// independently prove the ticker ran; it only pins that a successful check
	// writes the poll bookkeeping.
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "startup test build done")
	if store.Get().Poll.LastChecked.IsZero() {
		t.Error("check did not record poll.last_checked")
	}
}

func TestOpenRejectsBadID(t *testing.T) {
	b, _, _, _ := newTestBuilder(t, false)
	// Ids come from the request path and %2F is unwrapped by the mux, so a
	// crafted id must be rejected before any filesystem access.
	if f, err := b.OpenLog("../etc"); !errors.Is(err, os.ErrNotExist) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenLog(../etc) err = %v, want os.ErrNotExist", err)
	}
	if f, err := b.OpenArchive("../x"); !errors.Is(err, os.ErrNotExist) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenArchive(../x) err = %v, want os.ErrNotExist", err)
	}
	if bd, ok := b.Build("../x"); ok {
		t.Errorf(`Build("../x") = %+v, true; want false`, bd)
	}
}

func findByDomain(st state.State, domain, status string) (state.BuildInfo, bool) {
	for i := len(st.Builds) - 1; i >= 0; i-- {
		if st.Builds[i].Domain == domain && st.Builds[i].Status == status {
			return st.Builds[i], true
		}
	}
	return state.BuildInfo{}, false
}

func findByKindStatus(st state.State, kind, status string) (state.BuildInfo, bool) {
	for i := len(st.Builds) - 1; i >= 0; i-- {
		if st.Builds[i].Kind == kind && st.Builds[i].Status == status {
			return st.Builds[i], true
		}
	}
	return state.BuildInfo{}, false
}

// TestOrderedBuildSurvivesHistoryTrim reproduces issue #5: while an order
// waits in the queue, >= MaxHistory records may be added; the old trim
// dropped the queued record, so the build would run into an id that no
// longer exists (permanent 404, zombie dedup, orphaned archive). Active
// records must survive the whole wait.
func TestOrderedBuildSurvivesHistoryTrim(t *testing.T) {
	b, _, _, store := newTestBuilder(t, false)
	if err := store.Update(func(s *state.State) { s.AvailableCommit = c1 }); err != nil {
		t.Fatal(err)
	}
	id1, cached, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	if cached {
		t.Fatal("first order must not be cached")
	}

	// 50 dummy successful records: with the old trim, adding them drops
	// the queued record (the oldest in history).
	// Their finish times are in the past: finishing after the order's own
	// build would push it out of the newest-CACHE_SIZE window and retention
	// would evict the archive the final check asserts on.
	base := time.Now()
	for i := 0; i < 50; i++ {
		now := base.Add(time.Duration(-50+i) * time.Second)
		rec := state.BuildInfo{
			ID: state.NewBuildID(now), Kind: state.KindBuild,
			Domain: fmt.Sprintf("x%d.example", i+1), Commit: c1,
			Status: state.StatusSuccess, CreatedAt: now,
		}
		rec.FinishedAt = &now
		if err := store.AddBuild(rec); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := buildByID(store.Get(), id1); !ok {
		t.Fatalf("queued record %s was trimmed from history", id1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunWorker(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not exit after cancel")
		}
	})
	waitBuildStatus(t, store, id1, state.StatusSuccess)
	if _, err := os.Stat(b.archivePath(id1)); err != nil {
		t.Errorf("archive for %s: %v", id1, err)
	}
}

// TestOrderRebuildsWhenArchiveEvicted: an evicted successful build keeps its
// record in history but loses its archive dir. A new order of the same
// (domain, commit) must not be a cache hit and must queue a rebuild.
func TestOrderRebuildsWhenArchiveEvicted(t *testing.T) {
	b, git, _, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "available commit set")

	id1, _, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id1, state.StatusSuccess)
	id2, _, err := b.OrderBuild("d2.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id2, state.StatusSuccess)
	id3, _, err := b.OrderBuild("d3.example")
	if err != nil {
		t.Fatal(err)
	}
	waitBuildStatus(t, store, id3, state.StatusSuccess)

	// Eviction runs right after the success record lands; wait for the
	// removal instead of racing it. CacheSize=2 keeps d2 and d3, so d1's
	// dir is gone while its record stays in history.
	testutil.WaitFor(t, func() bool {
		_, err := os.Stat(b.buildDir(id1))
		return os.IsNotExist(err)
	}, "d1 dir evicted")
	// Eviction removes the archive dir only — the record must stay in
	// history for the cache-hit path below to be meaningful.
	if _, ok := buildByID(store.Get(), id1); !ok {
		t.Fatalf("record %s must survive eviction", id1)
	}

	id4, cached, err := b.OrderBuild("d1.example")
	if err != nil {
		t.Fatal(err)
	}
	if cached || id4 == id1 {
		t.Fatalf("rebuilt order = %q, cached=%v; want fresh id, cached=false", id4, cached)
	}
	waitBuildStatus(t, store, id4, state.StatusSuccess)
	if _, err := os.Stat(b.archivePath(id4)); err != nil {
		t.Errorf("rebuild archive: %v", err)
	}
}

// TestFormatInterval: the status example renders "6h", not Go's "6h0m0s"
// (issue #13); zero components are dropped.
func TestFormatInterval(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0"},
		{6 * time.Hour, "6h"},
		{90 * time.Minute, "1h30m"},
		{45 * time.Minute, "45m"},
		{30 * time.Second, "30s"},
		{1500 * time.Millisecond, "1.5s"},
		{60 * time.Second, "1m"},
		{60*time.Second + 500*time.Millisecond, "1m"},
	}
	for _, c := range cases {
		if got := formatInterval(c.d); got != c.want {
			t.Errorf("formatInterval(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestNewClampsCacheSize: CACHE_SIZE above the history cap would let history
// trim records before retention evicts their directories (issue #20's root);
// the constructor caps it at MaxHistory (issue #13). The oversized case
// exercises the clamp; the at-cap and below-cap cases pin that values at or
// under the cap pass through unchanged. (The ">" vs ">=" operator is
// indistinguishable at the boundary in this test: both leave the value at
// MaxHistory, and the capping log is gated on the value changing, so neither
// operator logs there.)
func TestNewClampsCacheSize(t *testing.T) {
	cases := []struct {
		name      string
		cacheSize int
		want      int
	}{
		{"oversized", state.MaxHistory + 10, state.MaxHistory},
		{"at cap", state.MaxHistory, state.MaxHistory},
		{"below cap", state.MaxHistory - 1, state.MaxHistory - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{
				Listen: ":0", APIKey: "k", DataDir: t.TempDir(), AssetsDir: t.TempDir(),
				CacheSize: c.cacheSize, DefaultDomain: "test.example",
			}
			store, err := state.Open(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			b := New(cfg, store, &fakeUpstream{}, &fakeBuildRun{})
			if b.cacheSize != c.want {
				t.Errorf("cacheSize = %d, want %d", b.cacheSize, c.want)
			}
			// The queue is sized from the same value: capacity = cacheSize + 1.
			if b.queue.cap != c.want+1 {
				t.Errorf("queue cap = %d, want %d", b.queue.cap, c.want+1)
			}
		})
	}
}

// TestFailedDirsPrunedWithoutSuccess reproduces issue #15: a run of failed
// builds used to accumulate directories until the next *successful* build ran
// FailedEvictPlan. failBuild now prunes like recordSuccess does, so at most
// failedDirsKept failed directories stay on disk with no success in between.
func TestFailedDirsPrunedWithoutSuccess(t *testing.T) {
	b, git, pipe, store := newTestBuilder(t, true)
	git.head = c1
	if _, err := b.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, func() bool { return store.Get().AvailableCommit == c1 }, "test build done")

	pipe.mu.Lock()
	pipe.failMsg = "boom"
	pipe.mu.Unlock()

	const attempts = failedDirsKept + 2
	var firstID string
	for i := 0; i < attempts; i++ {
		id, _, err := b.OrderBuild(fmt.Sprintf("d%d.example", i))
		if err != nil {
			t.Fatalf("order %d: %v", i, err)
		}
		if i == 0 {
			firstID = id
		}
		waitBuildStatus(t, store, id, state.StatusFailed)
	}

	// The oldest failure is beyond the kept tail: its directory must be gone
	// without any successful build ever running afterwards. The record lands
	// before evict() finishes removing the dir, hence WaitFor, not a Stat.
	testutil.WaitFor(t, func() bool {
		_, err := os.Stat(b.buildDir(firstID))
		return os.IsNotExist(err)
	}, "oldest failed dir pruned")

	kept := 0
	for _, bd := range store.Get().Builds {
		if bd.Status != state.StatusFailed {
			continue
		}
		if _, err := os.Stat(b.buildDir(bd.ID)); err == nil {
			kept++
		}
	}
	if kept > failedDirsKept {
		t.Errorf("failed dirs kept = %d, want <= %d", kept, failedDirsKept)
	}
}

// TestSweepOrphans pins the rule of issue #20's sweep: a directory whose id is
// absent from history is removed, a directory of a live record survives, and
// an entry outside the id pattern (not ours to judge) is left alone.
func TestSweepOrphans(t *testing.T) {
	b, _, _, store := newTestBuilder(t, false)
	liveID := "b-20260101-000000-aaaa"
	orphanID := "b-20260101-000000-bbbb"
	if err := store.AddBuild(state.BuildInfo{
		ID: liveID, Kind: state.KindBuild, Domain: "live.example", Commit: c1,
		Status: state.StatusSuccess, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{liveID, orphanID} {
		if err := os.MkdirAll(b.buildDir(id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(b.buildsDir(), "not-a-build-dir")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	strayFile := filepath.Join(b.buildsDir(), "b-20260101-000000-dddd")
	if err := os.WriteFile(strayFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	b.mu.Lock()
	b.sweepOrphans()
	b.mu.Unlock()

	if _, err := os.Stat(b.buildDir(orphanID)); !os.IsNotExist(err) {
		t.Errorf("orphan dir must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(strayFile); !os.IsNotExist(err) {
		t.Errorf("stray file with a build-id name must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(b.buildDir(liveID)); err != nil {
		t.Errorf("live build dir must survive: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("entry outside the id pattern must survive: %v", err)
	}

	// An absent builds/ directory is not an error: on a fresh builder (no
	// build has created builds/ yet) the sweep returns without creating it.
	b2, _, _, _ := newTestBuilder(t, false)
	b2.mu.Lock()
	b2.sweepOrphans()
	b2.mu.Unlock()
	if _, err := os.Stat(b2.buildsDir()); !os.IsNotExist(err) {
		t.Errorf("sweep on a fresh builder must not create the builds dir, stat err = %v", err)
	}
}

// TestEvictSweepsTrimmedRecordDir reproduces issue #20 end to end: a record
// can be dropped by trimHistory while its directory is still inside the
// retention window of its own kind, so neither EvictPlan nor FailedEvictPlan
// targets it. evict() must sweep it.
func TestEvictSweepsTrimmedRecordDir(t *testing.T) {
	b, _, _, store := newTestBuilder(t, false)
	base := time.Now()
	successID := "b-20260101-000000-aaaa"
	if err := os.MkdirAll(b.buildDir(successID), 0o755); err != nil {
		t.Fatal(err)
	}
	fin := base
	if err := store.AddBuild(state.BuildInfo{
		ID: successID, Kind: state.KindBuild, Domain: "keep.example", Commit: c1,
		Status: state.StatusSuccess, CreatedAt: base, FinishedAt: &fin,
	}); err != nil {
		t.Fatal(err)
	}

	// MaxHistory terminal records after it push the success record out of
	// history (trim keys on append order). All of them are failed, so the
	// success record stays within its own retention window (fewer than
	// CACHE_SIZE successes) and EvictPlan never targets it — the orphan case.
	for i := 0; i < state.MaxHistory; i++ {
		now := base.Add(time.Duration(i+1) * time.Second)
		rec := state.BuildInfo{
			ID: fmt.Sprintf("b-20260101-%06d-f%03d", i, i), Kind: state.KindBuild,
			Domain: "f.example", Commit: c1, Status: state.StatusFailed,
			CreatedAt: now, FinishedAt: &now,
		}
		if err := store.AddBuild(rec); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := buildByID(store.Get(), successID); ok {
		t.Fatal("success record must have been trimmed from history")
	}

	b.evict()

	if _, err := os.Stat(b.buildDir(successID)); !os.IsNotExist(err) {
		t.Errorf("trimmed record's dir must be swept, stat err = %v", err)
	}
}

// TestWorkerStartupSweepsOrphans: an orphan left on disk by a previous run
// must be gone once the worker starts, before it picks up any build (issue #20).
func TestWorkerStartupSweepsOrphans(t *testing.T) {
	b, _, _, _ := newTestBuilder(t, false) // worker not started yet
	orphanID := "b-20260101-000000-cccc"
	if err := os.MkdirAll(b.buildDir(orphanID), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.RunWorker(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not exit after cancel")
		}
	})

	testutil.WaitFor(t, func() bool {
		_, err := os.Stat(b.buildDir(orphanID))
		return os.IsNotExist(err)
	}, "startup sweep removed the orphan")
}

// TestOpenArchiveSurvivesEviction pins the property the fix rests on (issue
// #27): a file opened under b.mu keeps serving after eviction removes the
// directory (POSIX unlink semantics), so an in-flight request never sees a
// false 404.
func TestOpenArchiveSurvivesEviction(t *testing.T) {
	b, _, _, store := newTestBuilder(t, false)
	id := "b-20260101-000000-aaaa"
	if err := os.MkdirAll(b.buildDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.archivePath(id), []byte("archive bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.AddBuild(state.BuildInfo{
		ID: id, Kind: state.KindBuild, Domain: "d.example", Commit: c1,
		Status: state.StatusSuccess, CreatedAt: now, FinishedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}

	f, err := b.OpenArchive(id)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()

	// Eviction removes the directory; the open descriptor must keep serving.
	if err := os.RemoveAll(b.buildDir(id)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read after removal: %v", err)
	}
	if string(got) != "archive bytes" {
		t.Errorf("read %q, want %q", got, "archive bytes")
	}
	// A fresh request after eviction correctly reports 404.
	if f2, err := b.OpenArchive(id); !errors.Is(err, os.ErrNotExist) {
		if f2 != nil {
			f2.Close()
		}
		t.Errorf("reopen err = %v, want os.ErrNotExist", err)
	}
}

// TestOpenArchiveRejectsActiveRecord: a queued/running record has no servable
// archive even if a file happens to exist on disk — the status check must win
// (issue #27).
func TestOpenArchiveRejectsActiveRecord(t *testing.T) {
	b, _, _, store := newTestBuilder(t, false)
	id := "b-20260101-000000-bbbb"
	if err := os.MkdirAll(b.buildDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.archivePath(id), []byte("stale bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBuild(state.BuildInfo{
		ID: id, Kind: state.KindBuild, Domain: "d.example", Commit: c1,
		Status: state.StatusQueued, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if f, err := b.OpenArchive(id); !errors.Is(err, os.ErrNotExist) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenArchive(queued) err = %v, want os.ErrNotExist", err)
	}
}
