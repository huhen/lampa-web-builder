package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOpenEmptyInitializes(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st := s.Get()
	if st.Current != "a" {
		t.Errorf("Current = %q, want a", st.Current)
	}
}

func TestUpdatePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	err = s.Update(func(st *State) {
		st.Current = "b"
		st.AvailableCommit = "c1"
		// latest_seen == available: restart recovery would otherwise re-arm an
		// unseen commit (see TestRestartRearmsUnseenCommit), and this test is
		// about plain persistence, not recovery.
		st.LatestSeenCommit = "c1"
		st.BadCommit = "cbad"
		st.Poll = PollInfo{LastChecked: now, LastError: "boom"}
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st := s2.Get()
	if st.Current != "b" || st.AvailableCommit != "c1" || st.LatestSeenCommit != "c1" || st.BadCommit != "cbad" {
		t.Errorf("state not persisted: %+v", st)
	}
	if !st.Poll.LastChecked.Equal(now) || st.Poll.LastError != "boom" {
		t.Errorf("poll info not persisted: %+v", st.Poll)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Update(func(st *State) { st.AvailableCommit = "x" }); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file %q after save", e.Name())
		}
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.AddBuild(BuildInfo{ID: "b1", Kind: KindBuild, Domain: "d.example", Status: StatusQueued, CreatedAt: time.Now()})
	s.AddBuild(BuildInfo{ID: "b2", Kind: KindTest, Domain: "t.example", Status: StatusRunning, CreatedAt: time.Now()})
	s.AddBuild(BuildInfo{ID: "b3", Kind: KindBuild, Domain: "d.example", Status: StatusSuccess, CreatedAt: time.Now()})
	s.Update(func(st *State) { st.Queue = []string{"d.example"}; st.AvailableCommit = "keep" })

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st := s2.Get()
	if len(st.Queue) != 0 {
		t.Errorf("queue must burn on restart, got %v", st.Queue)
	}
	if st.AvailableCommit != "keep" {
		t.Errorf("AvailableCommit = %q, want keep", st.AvailableCommit)
	}
	byID := map[string]BuildInfo{}
	for _, b := range st.Builds {
		byID[b.ID] = b
	}
	if b := byID["b1"]; b.Status != StatusFailed || b.Error != "restart" {
		t.Errorf("b1 = %+v, want failed/restart", b)
	}
	if b := byID["b2"]; b.Status != StatusFailed || b.Error != "restart" {
		t.Errorf("b2 = %+v, want failed/restart", b)
	}
	if b := byID["b3"]; b.Status != StatusSuccess {
		t.Errorf("b3 must stay success, got %s", b.Status)
	}
}

func TestAddBuildTrimsHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Terminal records: the history cap applies to terminal records only
	// (issue #5), so the overflow that must be trimmed has to be terminal.
	for i := 0; i < MaxHistory+10; i++ {
		if err := s.AddBuild(BuildInfo{ID: "b", Kind: KindBuild, Status: StatusSuccess, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Get().Builds); got != MaxHistory {
		t.Errorf("history length = %d, want %d", got, MaxHistory)
	}
}

func TestUpdateBuild(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.AddBuild(BuildInfo{ID: "b1", Status: StatusQueued})
	err = s.UpdateBuild("b1", func(b *BuildInfo) {
		b.Status = StatusSuccess
		now := time.Now()
		b.FinishedAt = &now
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := s.UpdateBuild("nope", func(b *BuildInfo) { b.Status = StatusFailed }); err == nil {
		t.Error("expected error for unknown id")
	}
	got := s.Get()
	if got.Builds[0].Status != StatusSuccess {
		t.Errorf("status = %s, want success", got.Builds[0].Status)
	}
}

func TestNewBuildID(t *testing.T) {
	re := regexp.MustCompile(`^b-\d{8}-\d{6}-[0-9a-f]{4}$`)
	now := time.Date(2026, 10, 7, 15, 30, 0, 0, time.UTC)
	if id := NewBuildID(now); !re.MatchString(id) || id[:17] != "b-20261007-153000" {
		t.Errorf("NewBuildID = %q, want b-20261007-153000-xxxx", id)
	}
	// Only 2 random bytes: 100 draws in the same second collide with ~7%
	// probability (birthday), so assert sanity instead of strict uniqueness —
	// distinct seconds make real collisions negligible.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		seen[NewBuildID(now)] = true
	}
	if len(seen) < 2 {
		t.Errorf("NewBuildID produced %d distinct ids out of 100, want at least 2", len(seen))
	}
}

func TestOpenCorruptStateJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("[]{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected error for corrupt state.json")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error must mention parse, got: %v", err)
	}
}

func TestRestartRearmsUnseenCommit(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A restart in the middle of the test build of c2: the head was seen but
	// never became available, and no bad commit was recorded.
	if err := s.Update(func(st *State) {
		st.AvailableCommit = "c1"
		st.LatestSeenCommit = "c2"
		st.BadCommit = ""
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if st := s2.Get(); st.LatestSeenCommit != "c1" {
		t.Errorf("latest_seen = %q after restart, want re-armed back to available %q", st.LatestSeenCommit, "c1")
	}
}

func TestRestartKeepsFailedTestSkip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// The last test build failed: latest_seen == bad_commit is a deliberate
	// skip and must survive the restart.
	if err := s.Update(func(st *State) {
		st.AvailableCommit = "c1"
		st.LatestSeenCommit = "c2"
		st.BadCommit = "c2"
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if st := s2.Get(); st.LatestSeenCommit != "c2" {
		t.Errorf("latest_seen = %q after restart, want %q kept (latest == bad stays skipped)", st.LatestSeenCommit, "c2")
	}
}

// TestAddBuildKeepsActiveRecords: the history cap applies to terminal
// (success/failed) records only; a queued record survives even when it is
// the oldest in history (issue #5).
func TestAddBuildKeepsActiveRecords(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Now()

	q := BuildInfo{
		ID: NewBuildID(base), Kind: KindBuild, Domain: "q.example",
		Commit: "c", Status: StatusQueued, CreatedAt: base,
	}
	if err := s.AddBuild(q); err != nil {
		t.Fatalf("add queued: %v", err)
	}
	var firstTerm, lastTerm string
	for i := 1; i <= 51; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		rec := BuildInfo{
			ID: NewBuildID(now), Kind: KindBuild, Domain: fmt.Sprintf("d%d.example", i),
			Commit: "c", Status: StatusSuccess, CreatedAt: now,
		}
		rec.FinishedAt = &now
		if err := s.AddBuild(rec); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		if i == 1 {
			firstTerm = rec.ID
		}
		if i == 51 {
			lastTerm = rec.ID
		}
	}
	st := s.Get()
	if _, ok := findByID(st.Builds, q.ID); !ok {
		t.Fatalf("queued record %s was trimmed", q.ID)
	}
	if _, ok := findByID(st.Builds, firstTerm); ok {
		t.Errorf("oldest terminal %s must be trimmed", firstTerm)
	}
	if _, ok := findByID(st.Builds, lastTerm); !ok {
		t.Errorf("newest terminal %s must be kept", lastTerm)
	}
	term := 0
	for _, b := range st.Builds {
		if b.Status == StatusSuccess || b.Status == StatusFailed {
			term++
		}
	}
	if term != MaxHistory {
		t.Errorf("terminal records = %d, want %d", term, MaxHistory)
	}
	if len(st.Builds) != MaxHistory+1 {
		t.Errorf("history len = %d, want %d", len(st.Builds), MaxHistory+1)
	}
}

func findByID(builds []BuildInfo, id string) (BuildInfo, bool) {
	for _, b := range builds {
		if b.ID == id {
			return b, true
		}
	}
	return BuildInfo{}, false
}

// TestOpenWritesStateFileOnFirstRun: an operator inspecting a fresh volume
// must see state.json without waiting for the first update (issue #9).
func TestOpenWritesStateFileOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Errorf("state.json must exist after Open on an empty data dir: %v", err)
	}
}

// TestOpenReadErrorHasContext: a failure reading state.json must name the
// operation phase and the path (issue #9).
func TestOpenReadErrorHasContext(t *testing.T) {
	dir := t.TempDir()
	// A directory where state.json should be makes ReadFile fail with EISDIR.
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected an error when state.json is a directory")
	}
	if !strings.Contains(err.Error(), "load state") || !strings.Contains(err.Error(), "state.json") {
		t.Errorf("error must name the phase and the path, got: %v", err)
	}
}

// TestOpenMkdirErrorHasContext: a failure to prepare the data dir must name
// the path and the operation, not bubble a bare syscall error (issue #9).
func TestOpenMkdirErrorHasContext(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "data")
	// A file where the data dir should be makes MkdirAll fail.
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(blocker)
	if err == nil {
		t.Fatal("expected an error when the data dir path is a regular file")
	}
	if !strings.Contains(err.Error(), "state dir") || !strings.Contains(err.Error(), blocker) {
		t.Errorf("error must name the operation and the path, got: %v", err)
	}
}

// TestBuildInfoQueuedJSONOmitsTimes: a queued record has no started_at,
// finished_at or error in its JSON — the API contract, which the spec example
// must reflect (issue #13).
func TestBuildInfoQueuedJSONOmitsTimes(t *testing.T) {
	raw, err := json.Marshal(BuildInfo{ID: "b-x", Kind: KindBuild, Domain: "d.example", Commit: "c", Status: StatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, k := range []string{"started_at", "finished_at", "error"} {
		if strings.Contains(s, k) {
			t.Errorf("queued record JSON must omit %q: %s", k, s)
		}
	}
}
