package builder

import (
	"testing"
	"time"

	"github.com/huhen/lampa-web-builder/internal/state"
)

func build(id, kind, status string, finished time.Time) state.BuildInfo {
	b := state.BuildInfo{ID: id, Kind: kind, Status: status, Domain: id + ".example"}
	if status == state.StatusSuccess {
		b.FinishedAt = &finished
	}
	return b
}

func TestEvictPlanKeepsNewest(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		build("old1", state.KindBuild, state.StatusSuccess, base.Add(-3*time.Hour)),
		build("old2", state.KindBuild, state.StatusSuccess, base.Add(-2*time.Hour)),
		build("new1", state.KindBuild, state.StatusSuccess, base.Add(-1*time.Hour)),
		build("new2", state.KindBuild, state.StatusSuccess, base),
	}
	got := EvictPlan(builds, 2)
	want := []string{"old1", "old2"}
	if len(got) != len(want) {
		t.Fatalf("evict = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evict = %v, want %v", got, want)
		}
	}
}

func TestEvictPlanIgnoresFailed(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		build("f1", state.KindBuild, state.StatusFailed, base),
		build("f2", state.KindBuild, state.StatusFailed, base),
	}
	if got := EvictPlan(builds, 1); len(got) != 0 {
		t.Errorf("failed builds must not be evicted, got %v", got)
	}
}

func TestEvictPlanPinsNewestTestBuild(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		build("test1", state.KindTest, state.StatusSuccess, base.Add(-3*time.Hour)),
		build("b1", state.KindBuild, state.StatusSuccess, base.Add(-2*time.Hour)),
		build("b2", state.KindBuild, state.StatusSuccess, base.Add(-1*time.Hour)),
	}
	// cacheSize=2 keeps b1,b2; test1 survives only because of the pin.
	got := EvictPlan(builds, 2)
	if len(got) != 0 {
		t.Errorf("pinned test build must not be evicted, got %v", got)
	}
}

func TestEvictPlanPinMovesToNewestTest(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		build("test1", state.KindTest, state.StatusSuccess, base.Add(-4*time.Hour)),
		build("test2", state.KindTest, state.StatusSuccess, base.Add(-3*time.Hour)),
		build("b1", state.KindBuild, state.StatusSuccess, base.Add(-2*time.Hour)),
		build("b2", state.KindBuild, state.StatusSuccess, base.Add(-1*time.Hour)),
	}
	// Pin is now test2. cacheSize=2 keeps the newest two by time (b1, b2);
	// test1 is not pinned and beyond the size -> evicted; test2 pinned -> kept.
	got := EvictPlan(builds, 2)
	want := []string{"test1"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("evict = %v, want %v", got, want)
	}
}

func TestEvictPlanEmpty(t *testing.T) {
	if got := EvictPlan(nil, 5); len(got) != 0 {
		t.Errorf("evict = %v, want empty", got)
	}
}

func failedBuild(id string, finished time.Time) state.BuildInfo {
	tm := finished
	return state.BuildInfo{
		ID: id, Kind: state.KindBuild, Status: state.StatusFailed,
		Domain: id + ".example", FinishedAt: &tm,
	}
}

func TestFailedEvictPlanKeepsNewest(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		failedBuild("f1", base.Add(-3*time.Hour)),
		failedBuild("f2", base.Add(-2*time.Hour)),
		failedBuild("f3", base.Add(-1*time.Hour)),
		failedBuild("f4", base),
	}
	got := FailedEvictPlan(builds, 2)
	want := []string{"f1", "f2"} // oldest first, beyond the kept prefix
	if len(got) != len(want) {
		t.Fatalf("failed evict = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failed evict = %v, want %v", got, want)
		}
	}
	if got := FailedEvictPlan(builds, 10); len(got) != 0 {
		t.Errorf("keep above the count must evict nothing, got %v", got)
	}
}

func TestFailedEvictPlanIgnoresSuccess(t *testing.T) {
	base := time.Now()
	builds := []state.BuildInfo{
		build("s1", state.KindBuild, state.StatusSuccess, base.Add(-2*time.Hour)),
		build("s2", state.KindTest, state.StatusSuccess, base.Add(-1*time.Hour)),
	}
	if got := FailedEvictPlan(builds, 0); len(got) != 0 {
		t.Errorf("successful builds are retention's business, got %v", got)
	}
}

func TestFailedEvictPlanNilFinishInTail(t *testing.T) {
	base := time.Now()
	nilFinish := state.BuildInfo{ID: "f-running", Kind: state.KindBuild, Status: state.StatusFailed}
	builds := []state.BuildInfo{
		failedBuild("f1", base),
		nilFinish,
	}
	// A record without FinishedAt sorts last in the newest-first order, so
	// it is evicted first and f1 stays within keep=1.
	got := FailedEvictPlan(builds, 1)
	if len(got) != 1 || got[0] != "f-running" {
		t.Errorf("failed evict = %v, want [f-running]", got)
	}
}
