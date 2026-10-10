// Package state persists builder state in state.json on the data volume.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Build kinds.
const (
	KindTest  = "test"
	KindBuild = "build"
)

// Build statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

// MaxHistory is how many terminal (success/failed) build records state.json
// keeps. Active (queued/running) records are always kept on top of the cap —
// a queued order must keep its record for the whole wait, or the builder
// would build into an id that no longer exists (issue #5).
const MaxHistory = 50

// BuildInfo is one build record; its JSON shape is also the API response of
// GET /api/v1/builds/{id}.
type BuildInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"` // test | build
	Domain     string     `json:"domain"`
	Commit     string     `json:"commit"`
	Status     string     `json:"status"` // queued | running | success | failed
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// PollInfo is the upstream polling bookkeeping.
type PollInfo struct {
	LastChecked time.Time `json:"last_checked"`
	LastError   string    `json:"last_error,omitempty"`
}

// State is the persisted builder state (spec §10).
type State struct {
	Current          string      `json:"current"` // "a" | "b"
	AvailableCommit  string      `json:"available_commit"`
	LatestSeenCommit string      `json:"latest_seen_commit"`
	BadCommit        string      `json:"bad_commit"`
	Poll             PollInfo    `json:"poll"`
	Queue            []string    `json:"queue"`
	Builds           []BuildInfo `json:"builds"`
}

// NewBuildID returns b-<yyyyMMdd-HHmmss>-<4 hex> (clock + random suffix).
func NewBuildID(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return fmt.Sprintf("b-%s-%s", now.Format("20060102-150405"), hex.EncodeToString(b[:]))
}

// Store is a mutex-guarded State persisted atomically (tmp file + rename).
type Store struct {
	mu   sync.Mutex
	path string
	st   State
}

// Open loads state.json from dataDir (creating it on first run), applies
// restart recovery and returns the store.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("prepare state dir %s: %w", dataDir, err)
	}
	s := &Store{path: filepath.Join(dataDir, "state.json")}
	raw, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		s.st = State{Current: "a"}
		// New store, not shared yet — no lock needed (see save).
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("write initial %s: %w", s.path, err)
		}
	case err != nil:
		return nil, fmt.Errorf("load state: %w", err)
	default:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
		if s.st.Current != "b" {
			s.st.Current = "a"
		}
		s.recoverRestart()
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("write %s: %w", s.path, err)
		}
	}
	return s, nil
}

// recoverRestart burns the queue and fails in-flight builds: the queue is not
// persistent and nothing survives a restart mid-build.
func (s *Store) recoverRestart() {
	s.st.Queue = nil
	now := time.Now()
	for i := range s.st.Builds {
		b := &s.st.Builds[i]
		if b.Status == StatusQueued || b.Status == StatusRunning {
			b.Status = StatusFailed
			b.Error = "restart"
			b.FinishedAt = &now
		}
	}
	// A restart in the middle of a test build leaves latest_seen advanced with
	// nothing available: the upstream head would compare equal to latest_seen
	// and the commit would never be tested. Re-arm it by resetting latest_seen
	// to the available commit. When the last test failed (latest_seen ==
	// bad_commit) the skip must stay, so that pair is left alone.
	if s.st.LatestSeenCommit != s.st.AvailableCommit && s.st.LatestSeenCommit != s.st.BadCommit {
		s.st.LatestSeenCommit = s.st.AvailableCommit
	}
}

// Get returns a deep copy of the state.
func (s *Store) Get() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Queue = append([]string(nil), s.st.Queue...)
	st.Builds = make([]BuildInfo, len(s.st.Builds))
	for i, b := range s.st.Builds {
		nb := b
		// Copy the timestamps too, or a caller writing through the returned
		// pointer would mutate the store outside the mutex.
		if b.StartedAt != nil {
			t := *b.StartedAt
			nb.StartedAt = &t
		}
		if b.FinishedAt != nil {
			t := *b.FinishedAt
			nb.FinishedAt = &t
		}
		st.Builds[i] = nb
	}
	return st
}

// Update applies fn to the state and saves it.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	return s.save()
}

// AddBuild appends a build record, trims the history and saves.
func (s *Store) AddBuild(b BuildInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Builds = append(s.st.Builds, b)
	s.trimHistory()
	return s.save()
}

// trimHistory drops the oldest terminal (success/failed) records beyond
// MaxHistory; active (queued/running) records are never trimmed. "Oldest"
// is append (creation) order, not finish time. Caller must hold s.mu.
func (s *Store) trimHistory() {
	terminal := 0
	for _, b := range s.st.Builds {
		if b.Status == StatusSuccess || b.Status == StatusFailed {
			terminal++
		}
	}
	drop := terminal - MaxHistory
	if drop <= 0 {
		return
	}
	kept := make([]BuildInfo, 0, len(s.st.Builds)-drop)
	for _, b := range s.st.Builds {
		if drop > 0 && (b.Status == StatusSuccess || b.Status == StatusFailed) {
			drop--
			continue
		}
		kept = append(kept, b)
	}
	s.st.Builds = kept
}

// UpdateBuild mutates the build record with the given id; unknown ids are an
// error.
func (s *Store) UpdateBuild(id string, fn func(*BuildInfo)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Builds {
		if s.st.Builds[i].ID == id {
			fn(&s.st.Builds[i])
			return s.save()
		}
	}
	return fmt.Errorf("build %s not found", id)
}

// SetQueue persists the queue contents for observability only — it is never
// restored across restarts.
func (s *Store) SetQueue(items []string) error {
	return s.Update(func(st *State) { st.Queue = append([]string(nil), items...) })
}

// save writes state.json atomically; caller must hold s.mu, except in Open
// where the store is not shared yet. The temp file is fsynced before the
// rename so a crash cannot leave a durable name pointing at unsynced (possibly
// empty) contents; the directory is fsynced after the rename so the rename
// itself survives a crash (issue #9).
func (s *Store) save() error {
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(s.path))
}

// syncDir fsyncs a directory so a rename in it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
