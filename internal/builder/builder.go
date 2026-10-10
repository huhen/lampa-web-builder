// Package builder orchestrates builds: a single worker goroutine runs ordered
// and test builds strictly one at a time; the poller and the check endpoint
// share one upstream-check cycle; cache retention keeps the effective cache
// size worth of builds.
package builder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/huhen/lampa-web-builder/internal/config"
	"github.com/huhen/lampa-web-builder/internal/pipeline"
	"github.com/huhen/lampa-web-builder/internal/state"
)

// Errors mapped by the API onto HTTP codes.
var (
	ErrTestInProgress = errors.New("test build in progress")
	ErrNoCommit       = errors.New("no available commit yet")
	ErrQueueFull      = errors.New("build queue is full")
	ErrCheckBusy      = errors.New("a check is already in progress")
)

// Upstream is the source-repository surface the builder needs.
type Upstream interface {
	LsRemoteHead(ctx context.Context) (string, error)
	EnsureCopy(ctx context.Context, dir string) error
	Fetch(ctx context.Context, dir string) error
}

// Builder wires configuration, state, upstream access and the build pipeline.
type Builder struct {
	cfg   config.Config
	store *state.Store
	git   Upstream
	build pipeline.BuildRunner
	log   *log.Logger

	mu            sync.Mutex
	cacheSize     int
	queue         *queue
	queued        map[string]string // domain -> build id (queued)
	runningID     string
	runningDomain string
	testPending   bool
	testCommit    string
	testRunning   bool
	wake          chan struct{}
	checkMu       sync.Mutex // one check cycle at a time
}

// New assembles a Builder.
func New(cfg config.Config, store *state.Store, git Upstream, build pipeline.BuildRunner) *Builder {
	// CACHE_SIZE drives both the queue capacity and eviction. Values above the
	// history cap would let trimHistory drop a record before its directory is
	// evicted, orphaning builds/<id>/ (issue #20); cap it here (issue #13).
	cacheSize := cfg.CacheSize
	if cacheSize > state.MaxHistory {
		cacheSize = state.MaxHistory
	}
	b := &Builder{
		cfg:       cfg,
		store:     store,
		git:       git,
		build:     build,
		log:       log.New(os.Stdout, "builder: ", log.LstdFlags),
		cacheSize: cacheSize,
		queue:     newQueue(cacheSize + 1),
		queued:    map[string]string{},
		wake:      make(chan struct{}, 1),
	}
	if cacheSize != cfg.CacheSize {
		b.log.Printf("CACHE_SIZE %d exceeds history cap %d; capping", cfg.CacheSize, state.MaxHistory)
	}
	return b
}

// APIKey returns the configured access key (used by the API middleware).
func (b *Builder) APIKey() string { return b.cfg.APIKey }

func (b *Builder) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Builder) setQueue() {
	if err := b.store.SetQueue(b.queue.Items()); err != nil {
		b.log.Printf("save queue: %v", err)
	}
}

// RunWorker processes test builds and queued ordered builds until ctx is
// cancelled, strictly one build at a time. Test builds win over the queue.
// It must be started exactly once: two workers would break the pairing
// between the queue and the queued map and run builds concurrently.
func (b *Builder) RunWorker(ctx context.Context) {
	// Orphans can outlive a restart: sweep once before serving any build.
	b.mu.Lock()
	b.sweepOrphans()
	b.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return
		}
		if commit, ok := b.claimTest(); ok {
			b.execTest(ctx, commit)
			continue
		}
		if domain, ok := b.queue.Pop(); ok {
			b.setQueue()
			b.execOrdered(ctx, domain)
			continue
		}
		select {
		case <-b.wake:
		case <-ctx.Done():
			return
		}
	}
}

// claimTest starts a pending test build. Test builds do not use the queue —
// they occupy the runner directly.
func (b *Builder) claimTest() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.testPending {
		return "", false
	}
	b.testPending = false
	b.testRunning = true
	return b.testCommit, true
}

// OrderBuild queues an ordered build of domain and returns its id. cached
// reports a cache hit or an already queued/running build of the domain.
func (b *Builder) OrderBuild(domain string) (id string, cached bool, err error) {
	st := b.store.Get()
	b.mu.Lock()
	defer b.mu.Unlock()
	// Busy outranks "no commit yet": while the very first test build is still
	// pending there is no available commit, and the caller must see 409, not
	// a permanent-looking "no commit".
	if b.testRunning || b.testPending {
		return "", false, ErrTestInProgress
	}
	if st.AvailableCommit == "" {
		return "", false, ErrNoCommit
	}
	if id, ok := b.queued[domain]; ok {
		return id, true, nil
	}
	if b.runningDomain == domain {
		return b.runningID, true, nil
	}
	if id, ok := b.cacheHit(st, domain); ok {
		return id, true, nil
	}
	if !b.queue.Push(domain) {
		return "", false, ErrQueueFull
	}
	id = state.NewBuildID(time.Now())
	rec := state.BuildInfo{
		ID: id, Kind: state.KindBuild, Domain: domain,
		Commit: st.AvailableCommit, Status: state.StatusQueued, CreatedAt: time.Now(),
	}
	if err := b.store.AddBuild(rec); err != nil {
		b.queue.Remove(domain)
		return "", false, err
	}
	b.queued[domain] = id
	b.setQueue()
	b.signal()
	return id, false, nil
}

// cacheHit returns the newest finished successful build of domain for the
// current available commit whose archive is still on disk (a successful test
// build with a matching domain counts too — spec requirement 7).
func (b *Builder) cacheHit(st state.State, domain string) (string, bool) {
	for i := len(st.Builds) - 1; i >= 0; i-- {
		bd := st.Builds[i]
		if bd.Status == state.StatusSuccess && bd.Domain == domain && bd.Commit == st.AvailableCommit {
			if _, err := os.Stat(b.archivePath(bd.ID)); err == nil {
				return bd.ID, true
			}
		}
	}
	return "", false
}

func (b *Builder) execOrdered(ctx context.Context, domain string) {
	b.mu.Lock()
	id := b.queued[domain]
	delete(b.queued, domain)
	b.runningID, b.runningDomain = id, domain
	b.mu.Unlock()
	if id == "" {
		b.log.Printf("queued domain %q has no build id; skipping", domain)
		return
	}
	st := b.store.Get()
	work := b.workDir(st.Current)
	// The record carries the commit from ordering time, but the build always
	// runs the latest available commit (spec §1.6). A test build may have
	// swapped the commit while this order sat in the queue — sync the record,
	// or its metadata would lie about the archive it owns.
	if err := b.store.UpdateBuild(id, func(bd *state.BuildInfo) {
		bd.Commit = st.AvailableCommit
	}); err != nil {
		b.log.Printf("sync commit for %s: %v", id, err)
	}
	// The current copy normally exists (it was the test copy before the last
	// swap); clone it if /data was wiped — spec §5 requires a buildable
	// current copy.
	if err := b.git.EnsureCopy(ctx, work); err != nil {
		b.failBuild(id, "clone upstream: "+err.Error())
	} else if err := b.runBuild(ctx, id, domain, work, st.AvailableCommit, nil); err != nil {
		b.log.Printf("build %s: %v", id, err)
	}
	b.mu.Lock()
	b.runningID, b.runningDomain = "", ""
	b.mu.Unlock()
	b.signal()
}

// upstreamError marks upstream access failures (clone/fetch): they prove
// nothing about the recipe, so they never land in bad_commit — the check
// re-arms instead and the commit is retried after the outage ends.
type upstreamError struct{ err error }

func (e upstreamError) Error() string { return e.err.Error() }
func (e upstreamError) Unwrap() error { return e.err }

// execTest runs a test build of commit on the non-current copy; on success
// the copy becomes current and available_commit advances.
//
// The runner is released as soon as the pipeline outcome is decided, before
// the outcome is recorded: consumers (API clients, tests) act on what they
// see in the state — a terminal build record, available_commit, bad_commit,
// latest_seen — and a visible outcome with the runner still marked busy would
// make an OrderBuild/CheckNow that follows it fail with ErrTestInProgress.
func (b *Builder) execTest(ctx context.Context, commit string) {
	st := b.store.Get()
	other := "b"
	if st.Current == "b" {
		other = "a"
	}
	work := b.workDir(other)

	id := state.NewBuildID(time.Now())
	rec := state.BuildInfo{
		ID: id, Kind: state.KindTest, Domain: b.cfg.DefaultDomain,
		Commit: commit, Status: state.StatusQueued, CreatedAt: time.Now(),
	}
	if err := b.store.AddBuild(rec); err != nil {
		b.releaseRunner()
		b.log.Printf("add test build record: %v", err)
		if ctx.Err() == nil {
			b.store.Update(func(s *state.State) { s.BadCommit = commit })
		}
		return
	}
	b.log.Printf("test build %s: commit %s on copy %s", id, short(commit), other)

	perr := b.runTestStages(ctx, id, work, commit)
	b.releaseRunner()

	if perr != nil {
		b.failBuild(id, perr.Error())
		// A cancelled shutdown proves nothing about the commit, and neither
		// does an upstream outage: bad_commit is for recipe failures only (an
		// outage re-arms latest_seen so the next check retries — the hash
		// would otherwise look already seen forever).
		if ctx.Err() == nil {
			if _, upstream := errors.AsType[upstreamError](perr); upstream {
				b.store.Update(func(s *state.State) { s.LatestSeenCommit = s.AvailableCommit })
			} else {
				b.store.Update(func(s *state.State) { s.BadCommit = commit })
			}
		}
		return
	}
	b.recordSuccess(id, func(s *state.State) {
		// The swap lands in one update: whoever sees the new available_commit
		// also sees the new current copy.
		s.AvailableCommit = commit
		s.BadCommit = ""
		s.Current = other
	})
}

// releaseRunner marks the test-build runner free and wakes the worker.
func (b *Builder) releaseRunner() {
	b.mu.Lock()
	b.testRunning = false
	b.mu.Unlock()
	b.signal()
}

// runTestStages clones/fetches the non-current copy and runs the pipeline for
// the test build record id; upstream failures are wrapped in upstreamError.
func (b *Builder) runTestStages(ctx context.Context, id, work, commit string) error {
	if err := b.git.EnsureCopy(ctx, work); err != nil {
		return upstreamError{fmt.Errorf("clone upstream: %w", err)}
	}
	if err := b.git.Fetch(ctx, work); err != nil {
		return upstreamError{fmt.Errorf("fetch upstream: %w", err)}
	}
	return b.runPipeline(ctx, id, b.cfg.DefaultDomain, work, commit)
}

// runBuild executes the pipeline for build record id and records the outcome.
func (b *Builder) runBuild(ctx context.Context, id, domain, workDir, commit string, onSuccess func(*state.State)) error {
	perr := b.runPipeline(ctx, id, domain, workDir, commit)
	if perr != nil {
		return b.failBuild(id, perr.Error())
	}
	b.recordSuccess(id, onSuccess)
	return nil
}

// recordSuccess marks the build successful, applies the post-success state
// update and prunes the build directories.
func (b *Builder) recordSuccess(id string, onSuccess func(*state.State)) {
	if err := b.store.UpdateBuild(id, func(bd *state.BuildInfo) {
		now := time.Now()
		bd.Status = state.StatusSuccess
		bd.FinishedAt = &now
	}); err != nil {
		b.log.Printf("mark success %s: %v", id, err)
	}
	if onSuccess != nil {
		if err := b.store.Update(onSuccess); err != nil {
			b.log.Printf("post-success state update: %v", err)
		}
	}
	b.evict()
}

// runPipeline prepares the build directory and log, marks the record running
// and runs the pipeline. The outcome is returned unrecorded: callers decide
// when the terminal state becomes visible relative to the runner flags.
func (b *Builder) runPipeline(ctx context.Context, id, domain, workDir, commit string) error {
	if err := os.MkdirAll(b.buildDir(id), 0o755); err != nil {
		return fmt.Errorf("create build dir: %v", err)
	}
	logf, err := os.Create(b.logPath(id))
	if err != nil {
		return fmt.Errorf("create build log: %v", err)
	}
	defer logf.Close()
	fmt.Fprintf(logf, "build %s domain=%s commit=%s\n", id, domain, commit)

	if err := b.store.UpdateBuild(id, func(bd *state.BuildInfo) {
		now := time.Now()
		bd.Status = state.StatusRunning
		bd.StartedAt = &now
	}); err != nil {
		b.log.Printf("mark running %s: %v", id, err)
	}
	return b.build.Run(ctx, workDir, commit, domain, b.archivePath(id), logf)
}

func (b *Builder) failBuild(id, msg string) error {
	b.log.Printf("build %s failed: %s", id, msg)
	if err := b.store.UpdateBuild(id, func(bd *state.BuildInfo) {
		now := time.Now()
		bd.Status = state.StatusFailed
		bd.Error = msg
		bd.FinishedAt = &now
	}); err != nil {
		b.log.Printf("mark failed %s: %v", id, err)
	}
	// Symmetric to recordSuccess: every terminal build prunes, so a run of
	// failures cannot pile up directories until the next success (issue #15).
	b.evict()
	return errors.New(msg)
}

// evict prunes build directories: cache retention removes the directories of
// successful builds beyond the effective cache size (the pinned newest test
// build survives)
// and failedDirsKept bounds how many failed builds keep theirs. Planning and
// deletion happen under b.mu so a concurrent OrderBuild cannot observe an
// archive that is being removed a moment later (and vice versa).
// Last, sweepOrphans removes directories whose record trimHistory already
// dropped, which no eviction plan can see (issue #20).
func (b *Builder) evict() {
	b.mu.Lock()
	defer b.mu.Unlock()
	builds := b.store.Get().Builds
	remove := func(ids []string) {
		for _, id := range ids {
			if err := os.RemoveAll(b.buildDir(id)); err != nil {
				b.log.Printf("evict %s: %v", id, err)
			} else {
				b.log.Printf("evicted build %s", id)
			}
		}
	}
	remove(EvictPlan(builds, b.cacheSize))
	remove(FailedEvictPlan(builds, failedDirsKept))
	// Last: remove directories no plan can see because trimHistory already
	// dropped their record (issue #20).
	b.sweepOrphans()
}

// sweepOrphans removes build directories whose id is absent from history.
// trimHistory can drop a record while its directory is still inside the
// retention window (its plan keys on finish time, trim on creation order), so
// neither eviction plan sees it and the directory would stay orphaned forever
// (issue #20). The invariant is "a builds/<id>/ dir exists iff the record id
// is in history": records are added before their directory is created, and
// active records are never trimmed, so a live directory is never swept.
// Caller must hold b.mu (both call sites do); taking it here would deadlock.
func (b *Builder) sweepOrphans() {
	known := map[string]bool{}
	for _, bd := range b.store.Get().Builds {
		known[bd.ID] = true
	}
	entries, err := os.ReadDir(b.buildsDir())
	if err != nil {
		if !os.IsNotExist(err) { // a fresh /data has no builds dir yet
			b.log.Printf("sweep builds dir: %v", err)
		}
		return
	}
	for _, e := range entries {
		if known[e.Name()] || !buildIDPattern.MatchString(e.Name()) {
			continue
		}
		if err := os.RemoveAll(b.buildDir(e.Name())); err != nil {
			b.log.Printf("sweep orphan %s: %v", e.Name(), err)
			continue
		}
		b.log.Printf("swept orphan build dir %s", e.Name())
	}
}

// CheckNow runs one upstream check cycle (spec §8): look at the upstream head
// and, on a new unseen commit, request a test build. Shared by the poller and
// POST /api/v1/check. While an ordered build runs, the test build simply
// waits for the same runner.
func (b *Builder) CheckNow(ctx context.Context) (triggered bool, err error) {
	b.mu.Lock()
	busy := b.testRunning || b.testPending
	b.mu.Unlock()
	if busy {
		return false, ErrTestInProgress
	}
	if !b.checkMu.TryLock() {
		return false, ErrCheckBusy
	}
	defer b.checkMu.Unlock()

	hash, err := b.git.LsRemoteHead(ctx)
	if err != nil {
		if uerr := b.store.Update(func(s *state.State) { s.Poll.LastError = err.Error() }); uerr != nil {
			b.log.Printf("save poll error: %v", uerr)
		}
		return false, err
	}
	if err := b.store.Update(func(s *state.State) {
		s.Poll.LastChecked = time.Now()
		s.Poll.LastError = ""
	}); err != nil {
		b.log.Printf("save poll info: %v", err)
	}

	st := b.store.Get()
	if hash == st.LatestSeenCommit {
		return false, nil
	}
	if err := b.store.Update(func(s *state.State) { s.LatestSeenCommit = hash }); err != nil {
		b.log.Printf("save latest seen: %v", err)
	}
	if hash == st.BadCommit {
		// Already proven broken: wait for the next upstream commit.
		b.log.Printf("upstream head %s is the known bad commit; skipping test build", short(hash))
		return false, nil
	}
	b.mu.Lock()
	b.testPending = true
	b.testCommit = hash
	b.mu.Unlock()
	b.signal()
	return true, nil
}

// RunPoller checks upstream immediately at startup and then on every tick
// until ctx is done. With PollInterval 0 only the initial check runs; the
// periodic check is then available via POST /api/v1/check. It must be
// started exactly once — a second instance would double the check rate.
func (b *Builder) RunPoller(ctx context.Context) {
	b.pollCheck(ctx, "startup")
	if b.cfg.PollInterval == 0 {
		return
	}
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.pollCheck(ctx, "ticker")
		}
	}
}

func (b *Builder) pollCheck(ctx context.Context, source string) {
	if _, err := b.CheckNow(ctx); err != nil {
		// Busy/already-pending is normal while builds run; real errors are
		// already in poll.last_error but log them for the operator too.
		if !errors.Is(err, ErrTestInProgress) && !errors.Is(err, ErrCheckBusy) {
			b.log.Printf("poll(%s): %v", source, err)
		}
	}
}

// Status is the view for GET /api/v1/status (Version is added by the API).
type Status struct {
	Version          string   `json:"version"`
	AvailableCommit  string   `json:"available_commit"`
	LatestSeenCommit string   `json:"latest_seen_commit"`
	BadCommit        string   `json:"bad_commit"`
	Poll             PollView `json:"poll"`
}

// PollView is the poll section of the status.
type PollView struct {
	Interval    string    `json:"interval"`
	LastChecked time.Time `json:"last_checked"`
	LastError   string    `json:"last_error,omitempty"`
}

// Status returns the current status snapshot.
func (b *Builder) Status() Status {
	st := b.store.Get()
	return Status{
		AvailableCommit:  st.AvailableCommit,
		LatestSeenCommit: st.LatestSeenCommit,
		BadCommit:        st.BadCommit,
		Poll: PollView{
			Interval:    formatInterval(b.cfg.PollInterval),
			LastChecked: st.Poll.LastChecked,
			LastError:   st.Poll.LastError,
		},
	}
}

// buildIDPattern matches the ids state.NewBuildID produces. Ids arrive from
// the request path and Go's mux unwraps %2F inside a segment, so anything
// else (e.g. "../etc") is rejected before it reaches the filesystem and
// cannot escape the builds directory.
var buildIDPattern = regexp.MustCompile(`^b-\d{8}-\d{6}-[0-9a-f]{4}$`)

// findBuild returns the record with the given id from st.
func findBuild(st state.State, id string) (state.BuildInfo, bool) {
	for i := len(st.Builds) - 1; i >= 0; i-- {
		if st.Builds[i].ID == id {
			return st.Builds[i], true
		}
	}
	return state.BuildInfo{}, false
}

// Build returns a build record from history. The id must match the pattern ids
// are minted with; callers pass it from the request path.
func (b *Builder) Build(id string) (state.BuildInfo, bool) {
	if !buildIDPattern.MatchString(id) {
		return state.BuildInfo{}, false
	}
	return findBuild(b.store.Get(), id)
}

// OpenArchive opens the archive of a successful build under b.mu, so eviction
// cannot remove it between the readiness check and the open (issue #27). The
// returned file stays readable even if the directory is removed afterwards
// (POSIX unlink semantics). Callers must close it. os.ErrNotExist covers a
// malformed id, a record that is not a successful build, and a missing file.
func (b *Builder) OpenArchive(id string) (*os.File, error) {
	if !buildIDPattern.MatchString(id) {
		return nil, os.ErrNotExist
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bd, ok := findBuild(b.store.Get(), id)
	if !ok || bd.Status != state.StatusSuccess {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(b.archivePath(id))
	if err != nil {
		return nil, os.ErrNotExist
	}
	return f, nil
}

// OpenLog opens the build log under b.mu, so eviction cannot remove it between
// the check and the open (issue #27). Unlike the archive, any build kind may
// own a log; only the id pattern and file presence are checked. Callers must
// close it.
func (b *Builder) OpenLog(id string) (*os.File, error) {
	if !buildIDPattern.MatchString(id) {
		return nil, os.ErrNotExist
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.Open(b.logPath(id))
	if err != nil {
		return nil, os.ErrNotExist
	}
	return f, nil
}

func (b *Builder) buildsDir() string { return filepath.Join(b.cfg.DataDir, "builds") }
func (b *Builder) buildDir(id string) string {
	return filepath.Join(b.buildsDir(), id)
}
func (b *Builder) archivePath(id string) string {
	return filepath.Join(b.buildDir(id), "archive.tar.gz")
}
func (b *Builder) logPath(id string) string { return filepath.Join(b.buildDir(id), "build.log") }
func (b *Builder) workDir(copyID string) string {
	return filepath.Join(b.cfg.DataDir, "work", "repo-"+copyID)
}

// formatInterval renders a poll interval in the compact form the status
// example uses ("6h", "1h30m", "30s") rather than Go's "6h0m0s": zero
// components are dropped, and a fractional second part is kept ("1.5s", and
// "1m30.5s") when at least one whole second remains; a sub-second remainder is
// dropped when no whole second renders ("1m0.5s" is "1m"). Purely sub-second
// values fall back to the full rendering.
func formatInterval(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	var b strings.Builder
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m := d % time.Hour / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if rem := d % time.Minute; rem >= time.Second {
		fmt.Fprintf(&b, "%d", rem/time.Second)
		if ms := int(rem % time.Second / time.Millisecond); ms > 0 {
			b.WriteString("." + strings.TrimRight(fmt.Sprintf("%03d", ms), "0"))
		}
		b.WriteByte('s')
	}
	if b.Len() == 0 {
		return d.String()
	}
	return b.String()
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
