package store

import (
	"testing"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/model"
)

func vol(ns, name string, used, capacity int64, shared bool, node string) model.Volume {
	return model.Volume{
		Cluster: "c1", Namespace: ns, Name: name, Node: node,
		HasStats: true, UsedBytes: used, CapacityBytes: capacity,
		UsagePercent: float64(used) / float64(capacity) * 100,
		SharedFilesystem: shared, Status: model.StatusOK, StorageClass: "sc",
	}
}

// Several claims on one host disk each report that whole disk. Counting them
// per claim would report a 1 TiB node as several TiB of fleet capacity.
func TestTotalsCountSharedFilesystemOnce(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("a", "one", 500, 1000, true, "node1"),
		vol("b", "two", 500, 1000, true, "node1"),
		vol("c", "three", 500, 1000, true, "node1"),
	}})

	got := st.Query(Query{WarnThreshold: 75, CritThreshold: 90}).Totals
	if got.CapacityBytes != 1000 {
		t.Errorf("capacity = %d, want 1000 (one disk counted once)", got.CapacityBytes)
	}
	if got.UsedBytes != 500 {
		t.Errorf("used = %d, want 500", got.UsedBytes)
	}
	if got.UsagePercent != 50 {
		t.Errorf("usage = %v%%, want 50", got.UsagePercent)
	}
	if got.WithStats != 3 {
		t.Errorf("withStats = %d, want 3 (every claim still counts as a volume)", got.WithStats)
	}
}

// Distinct nodes are distinct filesystems even when they are the same size.
func TestTotalsSharedFilesystemsOnDifferentNodes(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("a", "one", 500, 1000, true, "node1"),
		vol("b", "two", 300, 1000, true, "node2"),
	}})
	got := st.Query(Query{}).Totals
	if got.CapacityBytes != 2000 {
		t.Errorf("capacity = %d, want 2000", got.CapacityBytes)
	}
	if got.UsedBytes != 800 {
		t.Errorf("used = %d, want 800", got.UsedBytes)
	}
}

func TestTotalsDedicatedVolumesAllCount(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("a", "one", 500, 1000, false, "node1"),
		vol("b", "two", 250, 1000, false, "node1"),
	}})
	got := st.Query(Query{}).Totals
	if got.CapacityBytes != 2000 || got.UsedBytes != 750 {
		t.Errorf("got capacity=%d used=%d, want 2000/750", got.CapacityBytes, got.UsedBytes)
	}
}

// An unmounted claim has unknown usage. Reporting it as 0% would tell an
// operator the volume is empty, which is a different and wrong statement.
func TestUnmountedIsUnknownNotZero(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		{Cluster: "c1", Namespace: "a", Name: "orphan", Status: model.StatusUnmounted, RequestedBytes: 5000},
	}})
	res := st.Query(Query{WarnThreshold: 75, CritThreshold: 90})
	if res.Totals.Unmounted != 1 {
		t.Errorf("unmounted = %d, want 1", res.Totals.Unmounted)
	}
	if res.Volumes[0].Severity != "unknown" {
		t.Errorf("severity = %q, want unknown", res.Volumes[0].Severity)
	}
	if res.Totals.WithStats != 0 {
		t.Errorf("withStats = %d, want 0", res.Totals.WithStats)
	}
}

// Inode exhaustion fails writes just like a full disk, so it must raise
// severity on its own even when there is plenty of space.
func TestInodeExhaustionRaisesSeverity(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{{
		Cluster: "c1", Namespace: "a", Name: "many-small-files",
		HasStats: true, UsedBytes: 10, CapacityBytes: 1000, UsagePercent: 1,
		Inodes: 100, InodesUsed: 96, InodesPercent: 96, Status: model.StatusOK,
	}}})
	res := st.Query(Query{WarnThreshold: 75, CritThreshold: 90})
	if res.Volumes[0].Severity != "critical" {
		t.Errorf("severity = %q, want critical (96%% of inodes used)", res.Volumes[0].Severity)
	}
}

func TestFilters(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("prod", "db-data", 900, 1000, false, "n1"),
		vol("stage", "cache", 100, 1000, false, "n1"),
	}})

	if got := st.Query(Query{Namespaces: []string{"prod"}}); len(got.Volumes) != 1 || got.Volumes[0].Name != "db-data" {
		t.Errorf("namespace filter returned %d rows", len(got.Volumes))
	}
	if got := st.Query(Query{Search: "CACHE"}); len(got.Volumes) != 1 {
		t.Errorf("search should be case-insensitive, got %d rows", len(got.Volumes))
	}
	if got := st.Query(Query{MinUsage: 50}); len(got.Volumes) != 1 || got.Volumes[0].Name != "db-data" {
		t.Errorf("minUsage filter returned %d rows", len(got.Volumes))
	}
	if got := st.Query(Query{OnlyAtRisk: true, WarnThreshold: 75, CritThreshold: 90}); len(got.Volumes) != 1 {
		t.Errorf("atRisk filter returned %d rows", len(got.Volumes))
	}
	// Facets describe the whole snapshot, so a filtered view still offers the
	// other values to switch to.
	if got := st.Query(Query{Namespaces: []string{"prod"}}); len(got.Facets.Namespaces) != 2 {
		t.Errorf("facets = %d, want 2 regardless of the active filter", len(got.Facets.Namespaces))
	}
}

// Growth is only projected from enough history over a long enough span; a
// confident number derived from two nearby points would be made up.
func TestProjectionNeedsEnoughHistory(t *testing.T) {
	st := New()
	base := time.Now().Add(-2 * time.Hour)

	for i := 0; i < 2; i++ {
		st.Put(model.Snapshot{
			GeneratedAt: base.Add(time.Duration(i) * time.Minute),
			Volumes:     []model.Volume{vol("a", "growing", int64(100+i*10), 1000, false, "n1")},
		})
	}
	if got := st.Query(Query{}).Volumes[0]; got.GrowthBytesPerDay != nil {
		t.Errorf("two samples should not yield a growth rate, got %v", *got.GrowthBytesPerDay)
	}

	// Ten samples an hour apart: 10 bytes/hour = 240 bytes/day.
	st = New()
	for i := 0; i < 10; i++ {
		st.Put(model.Snapshot{
			GeneratedAt: base.Add(time.Duration(i) * time.Hour),
			Volumes:     []model.Volume{vol("a", "growing", int64(100+i*10), 1000, false, "n1")},
		})
	}
	got := st.Query(Query{}).Volumes[0]
	if got.GrowthBytesPerDay == nil {
		t.Fatal("expected a growth rate from 10 hourly samples")
	}
	if diff := *got.GrowthBytesPerDay - 240; diff > 1 || diff < -1 {
		t.Errorf("growth = %v bytes/day, want ~240", *got.GrowthBytesPerDay)
	}
	// 810 bytes remain at 240/day.
	if got.DaysUntilFull == nil {
		t.Fatal("expected a days-until-full projection")
	}
	if d := *got.DaysUntilFull; d < 3.3 || d > 3.5 {
		t.Errorf("daysUntilFull = %v, want ~3.375", d)
	}
}

// A shrinking volume has no meaningful "full in N days", and claiming one
// would be worse than showing nothing.
func TestShrinkingVolumeHasNoProjection(t *testing.T) {
	st := New()
	base := time.Now().Add(-10 * time.Hour)
	for i := 0; i < 10; i++ {
		st.Put(model.Snapshot{
			GeneratedAt: base.Add(time.Duration(i) * time.Hour),
			Volumes:     []model.Volume{vol("a", "shrinking", int64(900-i*10), 1000, false, "n1")},
		})
	}
	if got := st.Query(Query{}).Volumes[0]; got.DaysUntilFull != nil {
		t.Errorf("shrinking volume projected to fill in %v days", *got.DaysUntilFull)
	}
}

// History for a deleted PVC must not accumulate forever.
func TestHistoryEvictedWhenVolumeDisappears(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{vol("a", "temp", 1, 10, false, "n1")}})
	if len(st.History("c1/a/temp")) != 1 {
		t.Fatal("expected history to be recorded")
	}
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{}})
	if len(st.History("c1/a/temp")) != 0 {
		t.Error("history should be dropped once the volume is gone")
	}
}
