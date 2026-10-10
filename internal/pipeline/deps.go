package pipeline

import (
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
