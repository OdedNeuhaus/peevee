// Package store keeps the most recent snapshot in memory, together with a short
// history per volume so the UI can show a trend and a time-to-full projection.
package store

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/model"
)

// historyDepth is how many samples are retained per volume. At the default 60s
// interval this is a rolling few hours, enough for a meaningful growth slope
// without turning the process into a time series database.
const historyDepth = 180

// Point is one retained usage reading.
type Point struct {
	T    time.Time `json:"t"`
	Used int64     `json:"used"`
	Pct  float64   `json:"pct"`
}

type Store struct {
	mu       sync.RWMutex
	snapshot model.Snapshot
	history  map[string][]Point
	// updated fires on every new snapshot, so SSE clients can be woken without
	// polling the store.
	subscribers map[chan struct{}]struct{}
}

func New() *Store {
	return &Store{
		history:     map[string][]Point{},
		subscribers: map[chan struct{}]struct{}{},
	}
}

func (s *Store) Put(snap model.Snapshot) {
	s.mu.Lock()
	s.snapshot = snap

	seen := make(map[string]struct{}, len(snap.Volumes))
	for _, v := range snap.Volumes {
		key := v.Key()
		seen[key] = struct{}{}
		if !v.HasStats {
			continue
		}
		pts := append(s.history[key], Point{T: snap.GeneratedAt, Used: v.UsedBytes, Pct: v.UsagePercent})
		if len(pts) > historyDepth {
			pts = pts[len(pts)-historyDepth:]
		}
		s.history[key] = pts
	}
	// Drop history for volumes that no longer exist, so a churn-heavy cluster
	// does not leak memory one deleted PVC at a time.
	for key := range s.history {
		if _, ok := seen[key]; !ok {
			delete(s.history, key)
		}
	}
	subs := make([]chan struct{}, 0, len(s.subscribers))
	for ch := range s.subscribers {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default: // a slow client keeps its existing pending wakeup
		}
	}
}

func (s *Store) Snapshot() model.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshot
}

func (s *Store) History(key string) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pts := s.history[key]
	out := make([]Point, len(pts))
	copy(out, pts)
	return out
}

func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
		close(ch)
	}
}

// Query is the set of filters the UI can apply.
type Query struct {
	Clusters      []string
	Namespaces    []string
	StorageClass  []string
	Provisioner   []string
	Status        []string
	Search        string
	MinUsage      float64
	OnlyAtRisk    bool
	Sort          string
	Desc          bool
	Limit         int
	Offset        int
	WarnThreshold float64
	CritThreshold float64
}

// Result is a filtered view plus everything the UI needs to render around it.
type Result struct {
	GeneratedAt time.Time         `json:"generatedAt"`
	DurationMS  int64             `json:"durationMs"`
	Volumes     []Row             `json:"volumes"`
	Totals      model.Totals      `json:"totals"`
	Facets      Facets            `json:"facets"`
	Matched     int               `json:"matched"`
	Errors      map[string]string `json:"errors,omitempty"`
}

// Row is a volume decorated with the derived numbers the UI displays.
type Row struct {
	model.Volume
	// GrowthBytesPerDay is the least-squares slope of recent usage. It is nil
	// when there is not enough history to say anything honest about a trend.
	GrowthBytesPerDay *float64 `json:"growthBytesPerDay,omitempty"`
	// DaysUntilFull is a projection from that slope, omitted when usage is flat
	// or shrinking. It is a straight-line estimate, not a forecast.
	DaysUntilFull *float64  `json:"daysUntilFull,omitempty"`
	Spark         []float64 `json:"spark,omitempty"`
	Severity      string    `json:"severity"`
}

// Facets are the distinct values available for filtering, with counts.
type Facets struct {
	Clusters     []Facet `json:"clusters"`
	Namespaces   []Facet `json:"namespaces"`
	StorageClass []Facet `json:"storageClasses"`
	Provisioners []Facet `json:"provisioners"`
	Statuses     []Facet `json:"statuses"`
}

type Facet struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func (s *Store) Query(q Query) Result {
	s.mu.RLock()
	snap := s.snapshot
	histCopy := make(map[string][]Point, len(s.history))
	for k, v := range s.history {
		histCopy[k] = v
	}
	s.mu.RUnlock()

	// Facets are computed over everything, so the filter dropdowns keep showing
	// the other options rather than collapsing to whatever is already selected.
	facets := buildFacets(snap.Volumes)

	rows := make([]Row, 0, len(snap.Volumes))
	for _, v := range snap.Volumes {
		if !q.matches(v) {
			continue
		}
		row := Row{Volume: v, Severity: severity(v, q.WarnThreshold, q.CritThreshold)}
		if q.OnlyAtRisk && row.Severity != "warning" && row.Severity != "critical" {
			continue
		}
		decorate(&row, histCopy[v.Key()])
		rows = append(rows, row)
	}

	totals := computeTotals(rows)
	matched := len(rows)
	sortRows(rows, q.Sort, q.Desc)

	if q.Offset > 0 {
		if q.Offset >= len(rows) {
			rows = nil
		} else {
			rows = rows[q.Offset:]
		}
	}
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}

	return Result{
		GeneratedAt: snap.GeneratedAt,
		DurationMS:  snap.DurationMS,
		Volumes:     rows,
		Totals:      totals,
		Facets:      facets,
		Matched:     matched,
		Errors:      snap.Errors,
	}
}

func (q Query) matches(v model.Volume) bool {
	if len(q.Clusters) > 0 && !contains(q.Clusters, v.Cluster) {
		return false
	}
	if len(q.Namespaces) > 0 && !contains(q.Namespaces, v.Namespace) {
		return false
	}
	if len(q.StorageClass) > 0 && !contains(q.StorageClass, v.StorageClass) {
		return false
	}
	if len(q.Provisioner) > 0 && !contains(q.Provisioner, v.Provisioner) {
		return false
	}
	if len(q.Status) > 0 && !contains(q.Status, string(v.Status)) {
		return false
	}
	if q.MinUsage > 0 && (!v.HasStats || v.UsagePercent < q.MinUsage) {
		return false
	}
	if q.Search != "" {
		needle := strings.ToLower(q.Search)
		hay := strings.ToLower(strings.Join([]string{
			v.Name, v.Namespace, v.Cluster, v.StorageClass, v.Provisioner, v.Workload, v.Node, v.VolumeName,
		}, " "))
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

func severity(v model.Volume, warn, crit float64) string {
	if !v.HasStats {
		return "unknown"
	}
	// Inode exhaustion fails writes exactly like a full disk, so it escalates
	// severity on its own even when there is plenty of space left.
	worst := v.UsagePercent
	if v.InodesPercent > worst {
		worst = v.InodesPercent
	}
	switch {
	case crit > 0 && worst >= crit:
		return "critical"
	case warn > 0 && worst >= warn:
		return "warning"
	default:
		return "ok"
	}
}

// decorate attaches the trend line and projection derived from history.
func decorate(r *Row, pts []Point) {
	if len(pts) == 0 {
		return
	}
	spark := make([]float64, 0, len(pts))
	for _, p := range pts {
		spark = append(spark, p.Pct)
	}
	r.Spark = spark

	// Two points spanning less than a few minutes cannot support a daily growth
	// rate; refusing to guess is better than printing a wild number.
	if len(pts) < 3 {
		return
	}
	span := pts[len(pts)-1].T.Sub(pts[0].T)
	if span < 5*time.Minute {
		return
	}

	slope := leastSquaresSlope(pts)
	perDay := slope * float64(24*time.Hour/time.Second)
	r.GrowthBytesPerDay = &perDay

	if perDay <= 0 || !r.HasStats || r.CapacityBytes <= 0 {
		return
	}
	remaining := float64(r.CapacityBytes - r.UsedBytes)
	if remaining <= 0 {
		zero := 0.0
		r.DaysUntilFull = &zero
		return
	}
	days := remaining / perDay
	if math.IsInf(days, 0) || math.IsNaN(days) || days > 3650 {
		return
	}
	r.DaysUntilFull = &days
}

// leastSquaresSlope returns bytes per second of growth across the retained points.
func leastSquaresSlope(pts []Point) float64 {
	n := float64(len(pts))
	t0 := pts[0].T
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range pts {
		x := p.T.Sub(t0).Seconds()
		y := float64(p.Used)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	denom := n*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / denom
}

func computeTotals(rows []Row) model.Totals {
	t := model.Totals{Volumes: len(rows)}
	clusters := map[string]struct{}{}
	namespaces := map[string]struct{}{}
	// Several PVCs can sit on one shared host filesystem, and each of them
	// reports that whole filesystem's used and capacity bytes. Adding them up
	// per claim would multiply one disk by the number of claims on it, so each
	// distinct filesystem is counted exactly once.
	sharedFS := map[string]struct{}{}

	for _, r := range rows {
		clusters[r.Cluster] = struct{}{}
		namespaces[r.Cluster+"/"+r.Namespace] = struct{}{}
		t.RequestedBytes += r.RequestedBytes
		switch r.Severity {
		case "critical":
			t.Critical++
		case "warning":
			t.Warning++
		}
		if !r.HasStats {
			if r.Status == model.StatusUnmounted {
				t.Unmounted++
			}
			continue
		}

		t.WithStats++
		if r.SharedFilesystem {
			key := r.Cluster + "/" + r.Node + "/" + strconv.FormatInt(r.CapacityBytes, 10)
			if _, seen := sharedFS[key]; seen {
				continue
			}
			sharedFS[key] = struct{}{}
		}
		t.UsedBytes += r.UsedBytes
		t.CapacityBytes += r.CapacityBytes
	}

	t.Clusters = len(clusters)
	t.Namespaces = len(namespaces)
	if t.CapacityBytes > 0 {
		t.UsagePercent = float64(t.UsedBytes) / float64(t.CapacityBytes) * 100
	}
	return t
}

func buildFacets(vols []model.Volume) Facets {
	cl, ns, sc, pr, st := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, v := range vols {
		cl[v.Cluster]++
		ns[v.Namespace]++
		sc[v.StorageClass]++
		if v.Provisioner != "" {
			pr[v.Provisioner]++
		}
		st[string(v.Status)]++
	}
	return Facets{
		Clusters:     toFacets(cl),
		Namespaces:   toFacets(ns),
		StorageClass: toFacets(sc),
		Provisioners: toFacets(pr),
		Statuses:     toFacets(st),
	}
}

func toFacets(m map[string]int) []Facet {
	out := make([]Facet, 0, len(m))
	for k, v := range m {
		out = append(out, Facet{Value: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func sortRows(rows []Row, key string, desc bool) {
	// Direction is applied inside each comparator rather than by reversing the
	// sorted slice at the end. Some rows have no value in the column at all — a
	// volume with no stats has no percentage, one with no history has no
	// countdown — and those are pinned below the real values in both
	// directions. A whole-slice reversal would float exactly those rows to the
	// top, which is the opposite of what "sort by most urgent, ascending"
	// should mean. Reversal also flipped ties, so equal rows reshuffled on
	// every toggle.
	dir := 1
	if desc {
		dir = -1
	}

	// Higher first in the natural direction: the interesting end of a number
	// column is the big end.
	num := func(x, y float64) int {
		switch {
		case x > y:
			return -1
		case x < y:
			return 1
		}
		return 0
	}
	// A to Z in the natural direction.
	str := func(x, y string) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	// Ordering for rows that carry no value in the sorted column. It never
	// takes dir, so the group reads identically whichever way the column
	// points. Name alone is not unique — the same claim name recurs across
	// namespaces and clusters — so it falls through to both.
	unknown := func(a, b Row) int {
		if c := str(string(a.Status), string(b.Status)); c != 0 {
			return c
		}
		if c := str(a.Name, b.Name); c != 0 {
			return c
		}
		if c := str(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return str(a.Cluster, b.Cluster)
	}
	// The Usage column shows a percentage when stats exist and the status word
	// otherwise. A missing percentage is not 0%, so status rows never mix in
	// among the numbers.
	usageWith := func(a, b Row, d int) int {
		if a.HasStats != b.HasStats {
			if a.HasStats {
				return -1
			}
			return 1
		}
		if !a.HasStats {
			return unknown(a, b)
		}
		return num(a.UsagePercent, b.UsagePercent) * d
	}
	usage := func(a, b Row) int { return usageWith(a, b, dir) }

	cmp := usage
	switch key {
	case "name":
		cmp = func(a, b Row) int { return str(a.Name, b.Name) * dir }
	case "namespace":
		cmp = func(a, b Row) int { return str(a.Namespace, b.Namespace) * dir }
	case "cluster":
		cmp = func(a, b Row) int { return str(a.Cluster, b.Cluster) * dir }
	case "used":
		cmp = func(a, b Row) int { return num(float64(a.UsedBytes), float64(b.UsedBytes)) * dir }
	case "capacity":
		cmp = func(a, b Row) int { return num(float64(a.CapacityBytes), float64(b.CapacityBytes)) * dir }
	case "requested":
		cmp = func(a, b Row) int { return num(float64(a.RequestedBytes), float64(b.RequestedBytes)) * dir }
	case "storageClass":
		cmp = func(a, b Row) int { return str(a.StorageClass, b.StorageClass) * dir }
	case "daysUntilFull":
		cmp = func(a, b Row) int {
			// Volumes with no projection sort last in both directions: an
			// unknown is never more urgent than a real countdown, and it does
			// not become the least urgent thing when the column is reversed
			// either. Among themselves they fall back to usage in its natural
			// direction, so the pinned block does not reshuffle on a toggle.
			x, y := a.DaysUntilFull, b.DaysUntilFull
			switch {
			case x == nil && y == nil:
				return usageWith(a, b, 1)
			case x == nil:
				return 1
			case y == nil:
				return -1
			}
			return num(*y, *x) * dir // soonest first, so smaller is "higher"
		}
	case "usage", "":
		// default
	}

	sort.SliceStable(rows, func(i, j int) bool { return cmp(rows[i], rows[j]) < 0 })
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
