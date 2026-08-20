package store

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/model"
)

func vol(ns, name string, used, capacity int64, shared bool, node string) model.Volume {
	return model.Volume{
		Cluster: "c1", Namespace: ns, Name: name, Node: node,
		HasStats: true, UsedBytes: used, CapacityBytes: capacity,
		UsagePercent:     float64(used) / float64(capacity) * 100,
		SharedFilesystem: shared, Status: model.StatusOK, StorageClass: "sc",
	}
}

// names is the shorthand the ordering tests compare against.
func names(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func ptr(f float64) *float64 { return &f }

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

// An unreported claim is in use but unmeasured. It must not land in the
// unmounted tile — that tile is what people go and delete — and it must stay
// out of the fleet aggregates, exactly like every other claim with no data.
func TestUnreportedIsCountedSeparatelyFromUnmounted(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		{Cluster: "c1", Namespace: "a", Name: "orphan", Status: model.StatusUnmounted, RequestedBytes: 5000},
		{Cluster: "c1", Namespace: "a", Name: "powerflex", Status: model.StatusUnreported, RequestedBytes: 8000},
	}})
	res := st.Query(Query{WarnThreshold: 75, CritThreshold: 90})

	if res.Totals.Unmounted != 1 {
		t.Errorf("unmounted = %d, want 1", res.Totals.Unmounted)
	}
	if res.Totals.Unreported != 1 {
		t.Errorf("unreported = %d, want 1", res.Totals.Unreported)
	}
	if res.Totals.WithStats != 0 {
		t.Errorf("withStats = %d, want 0", res.Totals.WithStats)
	}
	if res.Totals.UsedBytes != 0 || res.Totals.CapacityBytes != 0 {
		t.Errorf("a claim with no data contributed to the aggregates: used=%d capacity=%d",
			res.Totals.UsedBytes, res.Totals.CapacityBytes)
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

// The table pages through a filtered view, so a page has to be a window onto
// the sorted result — not a slice taken before sorting — and the numbers around
// it have to describe the whole match. Tiles that changed as you paged would be
// worse than no paging at all.
func TestPagingWindowsTheSortedResultAndLeavesTotalsWhole(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("a", "low", 100, 1000, false, "n1"),
		vol("a", "high", 900, 1000, false, "n2"),
		vol("a", "mid", 500, 1000, false, "n3"),
		vol("a", "top", 950, 1000, false, "n4"),
	}})

	all := st.Query(Query{Sort: "usage", Desc: true})
	if got := names(all.Volumes); len(got) != 4 {
		t.Fatalf("unpaged query returned %v", got)
	}

	page2 := st.Query(Query{Sort: "usage", Desc: true, Limit: 2, Offset: 2})

	if got, want := names(page2.Volumes), names(all.Volumes)[2:4]; !reflect.DeepEqual(got, want) {
		t.Errorf("page 2 = %v, want %v", got, want)
	}
	if page2.Matched != 4 {
		t.Errorf("matched = %d, want 4 — the count is of the match, not the page", page2.Matched)
	}
	if page2.Totals.Volumes != 4 || page2.Totals.CapacityBytes != all.Totals.CapacityBytes {
		t.Errorf("totals describe the page, not the fleet: %+v", page2.Totals)
	}
	if len(page2.Facets.Namespaces) != len(all.Facets.Namespaces) {
		t.Errorf("facets were narrowed to the page")
	}
}

// Paging past the end happens whenever the result set shrinks under an open
// page. It must come back empty rather than wrapping or panicking.
func TestOffsetPastTheEndReturnsNoRows(t *testing.T) {
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("a", "only", 100, 1000, false, "n1"),
	}})

	res := st.Query(Query{Limit: 25, Offset: 200})

	if len(res.Volumes) != 0 {
		t.Errorf("volumes = %v, want none", names(res.Volumes))
	}
	// A nil slice marshals to null, and the UI reads volumes.length.
	if res.Volumes == nil {
		t.Error("volumes is nil; it must serialise as [] rather than null")
	}
	if res.Matched != 1 {
		t.Errorf("matched = %d, want 1", res.Matched)
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

// The Usage column shows a percentage for volumes with stats and the status
// word for those without. Treating a missing percentage as 0% mixed those rows
// in among real numbers and left them in scrape order among themselves. They
// belong below every percentage in both directions, alphabetically by status,
// so toggling the column never floats "no data" to the top.
func TestUsageSortKeepsStatusRowsBelowPercentages(t *testing.T) {
	noStats := func(name string, status model.Status) model.Volume {
		return model.Volume{Cluster: "c1", Namespace: "ns", Name: name, Status: status, StorageClass: "sc"}
	}
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		vol("ns", "forty", 40, 100, false, "n"),
		noStats("p1", model.StatusPending),
		vol("ns", "ten", 10, 100, false, "n"),
		noStats("u1", model.StatusUnmounted),
		noStats("b1", model.StatusBlock),
		vol("ns", "twenty", 20, 100, false, "n"),
		noStats("p2", model.StatusPending),
	}})

	q := Query{Sort: "usage", WarnThreshold: 75, CritThreshold: 90}
	got := names(st.Query(q).Volumes)
	want := []string{"forty", "twenty", "ten", "b1", "p1", "p2", "u1"}
	if !slices.Equal(got, want) {
		t.Errorf("usage, highest first = %v, want %v", got, want)
	}

	q.Desc = true
	got = names(st.Query(q).Volumes)
	want = []string{"ten", "twenty", "forty", "b1", "p1", "p2", "u1"}
	if !slices.Equal(got, want) {
		t.Errorf("usage, lowest first = %v, want %v", got, want)
	}
}

// One claim name recurs across namespaces and clusters, so status and name
// together are not a unique key. Without a further tie-break the group is back
// to scrape order for exactly the rows this sort exists to stabilise.
func TestUsageSortBreaksTiesBeyondName(t *testing.T) {
	noStats := func(cluster, ns, name string) model.Volume {
		return model.Volume{Cluster: cluster, Namespace: ns, Name: name,
			Status: model.StatusUnmounted, StorageClass: "sc"}
	}
	st := New()
	st.Put(model.Snapshot{GeneratedAt: time.Now(), Volumes: []model.Volume{
		noStats("c2", "beta", "data"),
		noStats("c1", "beta", "data"),
		noStats("c1", "alpha", "data"),
	}})

	rows := st.Query(Query{Sort: "usage"}).Volumes
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Cluster + "/" + r.Namespace
	}
	want := []string{"c1/alpha", "c1/beta", "c2/beta"}
	if !slices.Equal(got, want) {
		t.Errorf("tie order = %v, want %v", got, want)
	}
}

// A volume with no projection is not an early deadline. Reversing the sorted
// slice to get ascending order used to put every unknown at the top, burying
// the real countdowns the column exists to surface.
func TestProjectionSortKeepsUnknownsLastBothWays(t *testing.T) {
	rows := []Row{
		{Volume: model.Volume{Name: "far", HasStats: true}, DaysUntilFull: ptr(30)},
		{Volume: model.Volume{Name: "none1", HasStats: true, UsagePercent: 10}},
		{Volume: model.Volume{Name: "soon", HasStats: true}, DaysUntilFull: ptr(2)},
		{Volume: model.Volume{Name: "none2", HasStats: true, UsagePercent: 90}},
	}

	cp := slices.Clone(rows)
	sortRows(cp, "daysUntilFull", false)
	if got, want := names(cp), []string{"soon", "far", "none2", "none1"}; !slices.Equal(got, want) {
		t.Errorf("soonest first = %v, want %v", got, want)
	}

	cp = slices.Clone(rows)
	sortRows(cp, "daysUntilFull", true)
	if got, want := names(cp), []string{"far", "soon", "none2", "none1"}; !slices.Equal(got, want) {
		t.Errorf("furthest first = %v, want %v", got, want)
	}
}

// Reversing the slice also reversed equal rows, so a column full of ties
// reshuffled every time the user toggled it.
func TestSortIsStableAcrossDirectionForEqualValues(t *testing.T) {
	rows := []Row{
		{Volume: model.Volume{Name: "a", HasStats: true, CapacityBytes: 100}},
		{Volume: model.Volume{Name: "b", HasStats: true, CapacityBytes: 100}},
		{Volume: model.Volume{Name: "c", HasStats: true, CapacityBytes: 100}},
	}

	cp := slices.Clone(rows)
	sortRows(cp, "capacity", false)
	asc := names(cp)

	cp = slices.Clone(rows)
	sortRows(cp, "capacity", true)
	if got := names(cp); !slices.Equal(got, asc) {
		t.Errorf("equal capacities reordered on toggle: %v then %v", asc, got)
	}
}
