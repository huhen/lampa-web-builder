package pipeline

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// depsCacheKept is how many resolved lockfiles stay in the cache; the oldest by
// mtime are removed first. Deleting an entry is safe by design: the next build
// under that key resolves again.
const depsCacheKept = 20

// depsEntry is one cached resolved lockfile, as listed for eviction.
type depsEntry struct {
	name    string
	modTime time.Time
}

// depsEvictPlan returns the names to delete from a cache listing, oldest first,
// keeping the newest keep entries. Equal mtimes break by name so the plan is
// deterministic. The policy is pure — the same shape as builder.EvictPlan — so
// it is testable without a filesystem.
func depsEvictPlan(entries []depsEntry, keep int) []string {
	sorted := append([]depsEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].modTime.Equal(sorted[j].modTime) {
			return sorted[i].modTime.After(sorted[j].modTime)
		}
		return sorted[i].name < sorted[j].name
	})
	if keep < 0 {
		keep = 0
	}
	// Iterate the sorted slice from the tail so evicted names come back oldest
	// first (the newest keep entries are its prefix).
	var evict []string
	for i := len(sorted) - 1; i >= keep; i-- {
		evict = append(evict, sorted[i].name)
	}
	return evict
}

// depsLockPath is where the resolved lockfile for a DepsStamp key lives.
func depsLockPath(depsDir, key string) string {
	return filepath.Join(depsDir, key+".lock.json")
}

// loadCachedLock returns the resolved lockfile frozen for key. A missing file
// is a miss, not an error; any other read failure is returned so the caller can
// warn and fall back to the seed.
func loadCachedLock(depsDir, key string) (raw []byte, ok bool, err error) {
	raw, err = os.ReadFile(depsLockPath(depsDir, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return raw, true, nil
}

// effectiveLock picks the lockfile npm is given: the resolution frozen in the
// cache for key when one exists, the seed otherwise. A cache read failure warns
// and falls back to the seed — that costs a resolve, it does not fail a build.
func (p *Pipeline) effectiveLock(seed []byte, key string, log io.Writer) []byte {
	cached, ok, err := loadCachedLock(p.DepsDir, key)
	switch {
	case err != nil:
		fmt.Fprintf(log, "WARN: deps cache read failed: %v — using the pinned lockfile\n", err)
		return seed
	case ok:
		fmt.Fprintf(log, "deps: cached lockfile %s\n", stampShort([]byte(key)))
		return cached
	default:
		fmt.Fprintf(log, "deps: pinned lockfile %s — no cached resolution for this manifest\n",
			stampShort([]byte(key)))
		return seed
	}
}

// storeCachedLock freezes raw as the resolved lockfile for key and drops the
// oldest entries beyond depsCacheKept, returning how many it removed. The write
// goes through a temp file and rename so a crash cannot leave a torn entry that
// npm ci would later choke on (the same idiom as gitops.EnsureCopy, issue #10).
func storeCachedLock(depsDir, key string, raw []byte) (pruned int, err error) {
	if err := os.MkdirAll(depsDir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(depsDir, "tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	if err := os.Rename(tmpName, depsLockPath(depsDir, key)); err != nil {
		os.Remove(tmpName)
		return 0, err
	}
	evict, err := depsCacheEvictPlan(depsDir)
	if err != nil {
		return 0, err
	}
	for _, name := range evict {
		if err := os.Remove(filepath.Join(depsDir, name)); err != nil {
			return 0, err
		}
	}
	return len(evict), nil
}

// depsCacheEvictPlan lists the entries the cache must drop to stay within
// depsCacheKept, oldest first.
func depsCacheEvictPlan(depsDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(depsDir, "*.lock.json"))
	if err != nil {
		return nil, err
	}
	entries := make([]depsEntry, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // vanished under us; nothing to plan for it
			}
			return nil, err
		}
		entries = append(entries, depsEntry{name: filepath.Base(path), modTime: info.ModTime()})
	}
	return depsEvictPlan(entries, depsCacheKept), nil
}
