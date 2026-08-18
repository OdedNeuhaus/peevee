// Package metrics turns a snapshot into Prometheus series, for both the local
// /metrics endpoint and the Mimir remote write push.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/model"
	"github.com/OdedNeuhaus/peevee/internal/remotewrite"
	"github.com/OdedNeuhaus/peevee/internal/store"
	"github.com/OdedNeuhaus/peevee/internal/version"
)

// The kubelet_volume_stats_* names deliberately match what kubelet itself
// exposes. Reusing them means the community Grafana dashboards and the
// kubernetes-mixin PVC alerts work against Mimir with no rewriting, with
// `cluster` as the extra dimension peevee adds.
const (
	metricCapacity   = "kubelet_volume_stats_capacity_bytes"
	metricUsed       = "kubelet_volume_stats_used_bytes"
	metricAvailable  = "kubelet_volume_stats_available_bytes"
	metricInodes     = "kubelet_volume_stats_inodes"
	metricInodesUsed = "kubelet_volume_stats_inodes_used"
	metricInodesFree = "kubelet_volume_stats_inodes_free"
)

// AllStatuses is every value the status state set publishes. It must list all
// of them, because a status that is never emitted as 0 would leave a stale 1
// behind after a claim moves away from it.
var AllStatuses = []model.Status{
	model.StatusOK,
	model.StatusUnmounted,
	model.StatusBlock,
	model.StatusPending,
	model.StatusStale,
	model.StatusError,
}

// Sample is one metric point before it is rendered or pushed.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
	Help   string
	Type   string
}

// Build converts a query result and cluster health into the full metric set.
func Build(res store.Result, health []cluster.Health, now time.Time) []Sample {
	out := make([]Sample, 0, len(res.Volumes)*8+len(health)*4+4)

	out = append(out, Sample{
		Name:   "peevee_build_info",
		Labels: map[string]string{"version": version.Version, "commit": version.Commit},
		Value:  1,
		Help:   "Build information for the running peevee instance.",
		Type:   "gauge",
	})
	out = append(out, Sample{
		Name:  "peevee_last_scrape_timestamp_seconds",
		Value: float64(res.GeneratedAt.Unix()),
		Help:  "Unix timestamp of the most recently completed collection cycle.",
		Type:  "gauge",
	})
	out = append(out, Sample{
		Name:  "peevee_scrape_duration_seconds",
		Value: float64(res.DurationMS) / 1000,
		Help:  "Wall clock duration of the most recent collection cycle across all clusters.",
		Type:  "gauge",
	})

	for _, h := range health {
		base := map[string]string{"cluster": h.Name, "endpoint": h.Endpoint}
		up := 0.0
		if h.Reachable {
			up = 1
		}
		out = append(out,
			Sample{Name: "peevee_cluster_up", Labels: base, Value: up,
				Help: "1 if the cluster's API server answered the most recent scrape, 0 otherwise.", Type: "gauge"},
			Sample{Name: "peevee_cluster_scrape_duration_seconds", Labels: map[string]string{"cluster": h.Name},
				Value: float64(h.ScrapeMillis) / 1000,
				Help:  "Duration of the most recent scrape of this cluster.", Type: "gauge"},
			Sample{Name: "peevee_cluster_pvcs", Labels: map[string]string{"cluster": h.Name}, Value: float64(h.PVCsTotal),
				Help: "Number of PersistentVolumeClaims observed in this cluster.", Type: "gauge"},
			Sample{Name: "peevee_cluster_nodes_scraped", Labels: map[string]string{"cluster": h.Name}, Value: float64(h.NodesScraped),
				Help: "Number of kubelets that returned volume statistics in this cluster.", Type: "gauge"},
		)
	}

	for _, v := range res.Volumes {
		// The kubelet-compatible label set, kept minimal so it joins cleanly
		// with series scraped from kubelet directly.
		kubeletLabels := map[string]string{
			"cluster":               v.Cluster,
			"namespace":             v.Namespace,
			"persistentvolumeclaim": v.Name,
		}
		if v.Node != "" {
			kubeletLabels["node"] = v.Node
		}

		// The richer label set for peevee's own series, where the storage
		// backend matters for slicing by Trident vs PowerFlex vs local disk.
		full := map[string]string{
			"cluster":               v.Cluster,
			"namespace":             v.Namespace,
			"persistentvolumeclaim": v.Name,
			"storageclass":          v.StorageClass,
		}
		if v.Provisioner != "" {
			full["provisioner"] = v.Provisioner
		}
		if v.Node != "" {
			full["node"] = v.Node
		}

		if v.HasStats {
			out = append(out,
				Sample{Name: metricCapacity, Labels: kubeletLabels, Value: float64(v.CapacityBytes),
					Help: "Capacity in bytes of the filesystem backing the volume.", Type: "gauge"},
				Sample{Name: metricUsed, Labels: kubeletLabels, Value: float64(v.UsedBytes),
					Help: "Number of used bytes in the volume.", Type: "gauge"},
				Sample{Name: metricAvailable, Labels: kubeletLabels, Value: float64(v.AvailableBytes),
					Help: "Number of available bytes in the volume.", Type: "gauge"},
				Sample{Name: metricInodes, Labels: kubeletLabels, Value: float64(v.Inodes),
					Help: "Maximum number of inodes in the volume.", Type: "gauge"},
				Sample{Name: metricInodesUsed, Labels: kubeletLabels, Value: float64(v.InodesUsed),
					Help: "Number of used inodes in the volume.", Type: "gauge"},
				Sample{Name: metricInodesFree, Labels: kubeletLabels, Value: float64(v.InodesFree),
					Help: "Number of free inodes in the volume.", Type: "gauge"},
				Sample{Name: "peevee_pvc_usage_ratio", Labels: full, Value: v.UsagePercent / 100,
					Help: "Used bytes divided by filesystem capacity, from 0 to 1.", Type: "gauge"},
				Sample{Name: "peevee_pvc_inodes_ratio", Labels: full, Value: v.InodesPercent / 100,
					Help: "Used inodes divided by total inodes, from 0 to 1.", Type: "gauge"},
			)
			if v.GrowthBytesPerDay != nil {
				out = append(out, Sample{
					Name: "peevee_pvc_growth_bytes_per_day", Labels: full, Value: *v.GrowthBytesPerDay,
					Help: "Least-squares growth rate of used bytes per day over the retained history.", Type: "gauge"})
			}
			if v.DaysUntilFull != nil {
				out = append(out, Sample{
					Name: "peevee_pvc_days_until_full", Labels: full, Value: *v.DaysUntilFull,
					Help: "Straight-line projection of days until the volume reaches capacity.", Type: "gauge"})
			}
		}

		// Requested size is known for every claim, mounted or not, and is what
		// capacity planning and quota reconciliation actually run on.
		out = append(out,
			Sample{Name: "peevee_pvc_requested_bytes", Labels: full, Value: float64(v.RequestedBytes),
				Help: "Storage requested by the claim's spec.", Type: "gauge"},
			Sample{Name: "peevee_pvc_provisioned_bytes", Labels: full, Value: float64(v.ProvisionedBytes),
				Help: "Storage the provisioner actually granted, from the claim's status.", Type: "gauge"},
		)

		// peevee_pvc_info deliberately carries only labels that are stable for
		// the life of a claim. Putting a changing value like status in here
		// would mint a brand new series on every transition and leave the old
		// one resolving for the whole lookback window, so a query for
		// status="pending" would report claims that are long since bound.
		info := map[string]string{
			"cluster":               v.Cluster,
			"namespace":             v.Namespace,
			"persistentvolumeclaim": v.Name,
			"storageclass":          v.StorageClass,
			"volumemode":            v.VolumeMode,
			"shared_filesystem":     strconv.FormatBool(v.SharedFilesystem),
		}
		if v.Provisioner != "" {
			info["provisioner"] = v.Provisioner
		}
		if v.VolumeName != "" {
			info["volumename"] = v.VolumeName
		}
		if v.Workload != "" {
			info["workload"] = v.Workload
		}
		out = append(out, Sample{Name: "peevee_pvc_info", Labels: info, Value: 1,
			Help: "Metadata for a claim, always 1. Join on cluster, namespace and persistentvolumeclaim.", Type: "gauge"})

		// Status is published as a state set instead: every possible value gets
		// a series, exactly one of which is 1. The series set stays constant, so
		// `peevee_pvc_status{status="pending"} == 1` is always true only of
		// claims that are pending right now.
		for _, st := range AllStatuses {
			value := 0.0
			if v.Status == st {
				value = 1
			}
			out = append(out, Sample{
				Name: "peevee_pvc_status",
				Labels: map[string]string{
					"cluster":               v.Cluster,
					"namespace":             v.Namespace,
					"persistentvolumeclaim": v.Name,
					"status":                string(st),
				},
				Value: value,
				Help:  "Current status of a claim as a state set: 1 for the active status, 0 for the others.",
				Type:  "gauge",
			})
		}
	}

	return out
}

// ToSeries converts samples into remote write series stamped with one timestamp,
// so every series in a cycle lands on the same millisecond in Mimir.
func ToSeries(samples []Sample, ts time.Time) []remotewrite.Series {
	ms := ts.UnixMilli()
	out := make([]remotewrite.Series, 0, len(samples))
	for _, s := range samples {
		labels := make([]remotewrite.Label, 0, len(s.Labels)+1)
		labels = append(labels, remotewrite.Label{Name: "__name__", Value: s.Name})
		for k, v := range s.Labels {
			if v == "" {
				// An empty label value is identical to an absent one in the
				// data model; sending it just wastes space in the index.
				continue
			}
			labels = append(labels, remotewrite.Label{Name: k, Value: v})
		}
		out = append(out, remotewrite.Series{Labels: labels, Value: s.Value, TimestampMS: ms})
	}
	return out
}

// WriteText renders samples in the Prometheus text exposition format.
func WriteText(sb *strings.Builder, samples []Sample) {
	// Group by name so each metric family emits HELP and TYPE exactly once,
	// which the exposition format requires.
	order := make([]string, 0)
	byName := map[string][]Sample{}
	for _, s := range samples {
		if _, ok := byName[s.Name]; !ok {
			order = append(order, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	sort.Strings(order)

	for _, name := range order {
		group := byName[name]
		if group[0].Help != "" {
			fmt.Fprintf(sb, "# HELP %s %s\n", name, escapeHelp(group[0].Help))
		}
		typ := group[0].Type
		if typ == "" {
			typ = "gauge"
		}
		fmt.Fprintf(sb, "# TYPE %s %s\n", name, typ)
		for _, s := range group {
			sb.WriteString(name)
			if len(s.Labels) > 0 {
				keys := make([]string, 0, len(s.Labels))
				for k := range s.Labels {
					if s.Labels[k] != "" {
						keys = append(keys, k)
					}
				}
				sort.Strings(keys)
				if len(keys) > 0 {
					sb.WriteByte('{')
					for i, k := range keys {
						if i > 0 {
							sb.WriteByte(',')
						}
						fmt.Fprintf(sb, "%s=\"%s\"", k, escapeLabel(s.Labels[k]))
					}
					sb.WriteByte('}')
				}
			}
			fmt.Fprintf(sb, " %s\n", formatFloat(s.Value))
		}
	}
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func escapeHelp(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}
