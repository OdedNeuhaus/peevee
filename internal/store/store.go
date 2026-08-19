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
	T     time.Time `json:"t"`
	Used  int64     `json:"used"`
	Pct   float64   `json:"pct"`
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
	DaysUntilFull *float64 `json:"daysUntilFull,omitempty"`
	Spark         []float64 `json:"spark,omitempty"`
	Severity      string    `json:"severity"`
}

// Facets are the distinct values available for filtering, with counts.
type Facets struct {
	Clusters      []Facet `json:"clusters"`
	Namespaces    []Facet `json:"namespaces"`
	StorageClass  []Facet `json:"storageClasses"`
	Provisioners  []Facet `json:"provisioners"`
	Statuses      []Facet `json:"statuses"`
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
	// The Usage column shows a percentage when stats exist and the status word
	// otherwise. Status rows rank below every percentage and alphabetically
	// among themselves, so they read as a sorted group in both directions
	// rather than as 0% in arbitrary order. (Natural order is highest first;
	// a later reversal yields ascending, which is why the alphabet is reversed
	// here.)
	less := func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.HasStats != b.HasStats:
			return a.HasStats
		case !a.HasStats:
			if a.Status != b.Status {
				return a.Status > b.Status
			}
			return a.Name > b.Name
		}
		return a.UsagePercent > b.UsagePercent
	}
	switch key {
	case "name":
		less = func(i, j int) bool { return rows[i].Name < rows[j].Name }
	case "namespace":
		less = func(i, j int) bool { return rows[i].Namespace < rows[j].Namespace }
	case "cluster":
		less = func(i, j int) bool { return rows[i].Cluster < rows[j].Cluster }
	case "used":
		less = func(i, j int) bool { return rows[i].UsedBytes > rows[j].UsedBytes }
	case "capacity":
		less = func(i, j int) bool { return rows[i].CapacityBytes > rows[j].CapacityBytes }
	case "requested":
		less = func(i, j int) bool { return rows[i].RequestedBytes > rows[j].RequestedBytes }
	case "storageClass":
		less = func(i, j int) bool { return rows[i].StorageClass < rows[j].StorageClass }
	case "daysUntilFull":
		less = func(i, j int) bool {
			// Volumes with no projection sort last either way: an unknown is
			// never more urgent than a real countdown.
			a, b := rows[i].DaysUntilFull, rows[j].DaysUntilFull
			if a == nil && b == nil {
				return rows[i].UsagePercent > rows[j].UsagePercent
			}
			if a == nil {
				return false
			}
			if b == nil {
				return true
			}
			return *a < *b
		}
	case "usage", "":
		// default
	}
	sort.SliceStable(rows, less)
	if desc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
