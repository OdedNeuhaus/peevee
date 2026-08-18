package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/model"
	"github.com/OdedNeuhaus/peevee/internal/store"
)

func result(vols ...store.Row) store.Result {
	return store.Result{GeneratedAt: time.Now(), Volumes: vols}
}

func find(samples []Sample, name string, match map[string]string) []Sample {
	var out []Sample
next:
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		for k, v := range match {
			if s.Labels[k] != v {
				continue next
			}
		}
		out = append(out, s)
	}
	return out
}

// The state set must publish every status, with exactly one set to 1. Emitting
// only the active status would leave the previous one resolving to 1 for the
// whole lookback window.
func TestStatusIsAStateSetWithExactlyOneActive(t *testing.T) {
	row := store.Row{Volume: model.Volume{
		Cluster: "c1", Namespace: "ns", Name: "claim", Status: model.StatusUnmounted,
	}}
	samples := Build(result(row), nil, time.Now())

	got := find(samples, "peevee_pvc_status", nil)
	if len(got) != len(AllStatuses) {
		t.Fatalf("emitted %d status series, want %d (one per possible status)", len(got), len(AllStatuses))
	}

	var active []string
	for _, s := range got {
		if s.Value == 1 {
			active = append(active, s.Labels["status"])
		} else if s.Value != 0 {
			t.Errorf("status series has value %v, want 0 or 1", s.Value)
		}
	}
	if len(active) != 1 || active[0] != string(model.StatusUnmounted) {
		t.Errorf("active statuses = %v, want exactly [unmounted]", active)
	}
}

// Anything that changes over a claim's life belongs outside the info metric,
// or every transition mints a new series that outlives the truth.
func TestInfoCarriesOnlyStableLabels(t *testing.T) {
	row := store.Row{Volume: model.Volume{
		Cluster: "c1", Namespace: "ns", Name: "claim",
		Status: model.StatusOK, Phase: "Bound", StorageClass: "trident-nas",
	}}
	samples := Build(result(row), nil, time.Now())

	info := find(samples, "peevee_pvc_info", nil)
	if len(info) != 1 {
		t.Fatalf("expected exactly one info series, got %d", len(info))
	}
	for _, churny := range []string{"status", "phase"} {
		if v, ok := info[0].Labels[churny]; ok {
			t.Errorf("info carries changing label %s=%q; it belongs in a state set", churny, v)
		}
	}
	if info[0].Labels["storageclass"] != "trident-nas" {
		t.Errorf("info lost its stable storageclass label")
	}
}

// A claim with no kubelet statistics must not publish usage series at all.
// A zero would be read as an empty volume, which is a different claim entirely.
func TestUnmountedPublishesNoUsageSeries(t *testing.T) {
	row := store.Row{Volume: model.Volume{
		Cluster: "c1", Namespace: "ns", Name: "orphan",
		Status: model.StatusUnmounted, RequestedBytes: 1024,
	}}
	samples := Build(result(row), nil, time.Now())

	for _, name := range []string{
		"kubelet_volume_stats_used_bytes",
		"kubelet_volume_stats_capacity_bytes",
		"peevee_pvc_usage_ratio",
	} {
		if got := find(samples, name, nil); len(got) != 0 {
			t.Errorf("%s was published for a claim with no statistics", name)
		}
	}
	// Requested size is known regardless, and is what capacity planning uses.
	if got := find(samples, "peevee_pvc_requested_bytes", nil); len(got) != 1 {
		t.Errorf("requested bytes should still be published, got %d series", len(got))
	}
}

func TestKubeletCompatibleNamesAndValues(t *testing.T) {
	row := store.Row{Volume: model.Volume{
		Cluster: "prod", Namespace: "db", Name: "pgdata", Node: "node-7",
		Status: model.StatusOK, HasStats: true,
		UsedBytes: 250, CapacityBytes: 1000, AvailableBytes: 750,
		UsagePercent: 25,
	}}
	samples := Build(result(row), nil, time.Now())

	used := find(samples, "kubelet_volume_stats_used_bytes", nil)
	if len(used) != 1 || used[0].Value != 250 {
		t.Fatalf("used bytes series = %+v", used)
	}
	// These label names are what kubelet itself uses, which is what lets the
	// community dashboards work against these series unchanged.
	for _, want := range []string{"namespace", "persistentvolumeclaim", "cluster", "node"} {
		if used[0].Labels[want] == "" {
			t.Errorf("missing label %q on kubelet-compatible series", want)
		}
	}
	ratio := find(samples, "peevee_pvc_usage_ratio", nil)
	if len(ratio) != 1 || ratio[0].Value != 0.25 {
		t.Errorf("usage ratio = %+v, want 0.25", ratio)
	}
}

// Remote write requires a __name__ label and drops empty label values.
func TestToSeriesAddsNameAndDropsEmptyLabels(t *testing.T) {
	series := ToSeries([]Sample{{
		Name:   "peevee_pvc_usage_ratio",
		Labels: map[string]string{"cluster": "prod", "node": ""},
		Value:  0.5,
	}}, time.Unix(1700000000, 0))

	if len(series) != 1 {
		t.Fatalf("expected 1 series, got %d", len(series))
	}
	names := map[string]string{}
	for _, l := range series[0].Labels {
		names[l.Name] = l.Value
	}
	if names["__name__"] != "peevee_pvc_usage_ratio" {
		t.Errorf("__name__ = %q", names["__name__"])
	}
	if _, ok := names["node"]; ok {
		t.Error("empty label value should be dropped, not sent as an empty string")
	}
	if series[0].TimestampMS != 1700000000000 {
		t.Errorf("timestamp = %d, want milliseconds", series[0].TimestampMS)
	}
}

// The text exposition format requires HELP and TYPE once per family, not per series.
func TestWriteTextEmitsOneHelpPerFamily(t *testing.T) {
	samples := []Sample{
		{Name: "m", Labels: map[string]string{"a": "1"}, Value: 1, Help: "help text", Type: "gauge"},
		{Name: "m", Labels: map[string]string{"a": "2"}, Value: 2, Help: "help text", Type: "gauge"},
	}
	var sb strings.Builder
	WriteText(&sb, samples)
	out := sb.String()

	if n := strings.Count(out, "# HELP m "); n != 1 {
		t.Errorf("HELP appeared %d times, want 1", n)
	}
	if n := strings.Count(out, "# TYPE m "); n != 1 {
		t.Errorf("TYPE appeared %d times, want 1", n)
	}
	if !strings.Contains(out, `m{a="1"} 1`) {
		t.Errorf("missing rendered series in:\n%s", out)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	var sb strings.Builder
	WriteText(&sb, []Sample{{
		Name:   "m",
		Labels: map[string]string{"msg": `a "quoted" \ value`},
		Value:  1,
	}})
	if !strings.Contains(sb.String(), `msg="a \"quoted\" \\ value"`) {
		t.Errorf("label value not escaped: %s", sb.String())
	}
}
