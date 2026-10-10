package builder

import (
	"sort"
	"time"

	"github.com/huhen/lampa-web-builder/internal/state"
)

// EvictPlan returns ids of cached (successful) builds to delete: all but the
// newest cacheSize by finish time. The newest successful test build is pinned
// and never evicted, even beyond the size. Failed builds own no archive and
// are never touched. Metadata of evicted builds stays in state.json.
func EvictPlan(builds []state.BuildInfo, cacheSize int) []string {
	var ok []state.BuildInfo
	for _, b := range builds {
		if b.Status == state.StatusSuccess {
			ok = append(ok, b)
		}
	}
	sort.Slice(ok, func(i, j int) bool {
		ti, tj := finishTime(ok[i]), finishTime(ok[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return ok[i].ID < ok[j].ID
	})
	keep := map[string]bool{}
	for i, b := range ok {
		if i < cacheSize {
			keep[b.ID] = true
		}
	}
	for _, b := range ok {
		if b.Kind == state.KindTest {
			keep[b.ID] = true
			break
		}
	}
	// Evicted ids come back oldest first: iterate the sorted slice from the
	// tail (beyond the kept prefix) toward the newest.
	var evict []string
	for i := len(ok) - 1; i >= cacheSize; i-- {
		if !keep[ok[i].ID] {
			evict = append(evict, ok[i].ID)
		}
	}
	return evict
}

func finishTime(b state.BuildInfo) time.Time {
	if b.FinishedAt != nil {
		return *b.FinishedAt
	}
	return time.Time{}
}

// failedDirsKept is how many failed build directories stay on disk (newest
// first by finish time) for post-mortem; a retrying client would otherwise
// leave one builds/<id>/ directory per failed attempt forever.
const failedDirsKept = 5

// FailedEvictPlan returns ids of failed builds whose directories should be
// removed: all but the newest keep by finish time. Records without a
// FinishedAt sort last, i.e. are evicted first — a build whose outcome was
// never recorded keeps no valuable artifact. Successful builds are ignored —
// cache retention (EvictPlan) owns those.
func FailedEvictPlan(builds []state.BuildInfo, keep int) []string {
	var failed []state.BuildInfo
	for _, b := range builds {
		if b.Status == state.StatusFailed {
			failed = append(failed, b)
		}
	}
	sort.Slice(failed, func(i, j int) bool {
		ti, tj := finishTime(failed[i]), finishTime(failed[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return failed[i].ID < failed[j].ID
	})
	var evict []string
	for i := len(failed) - 1; i >= keep; i-- {
		evict = append(evict, failed[i].ID)
	}
	return evict
}
