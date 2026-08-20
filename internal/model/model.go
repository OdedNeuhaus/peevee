// Package model holds the types shared between the collector, the HTTP API and
// the remote write pipeline.
package model

import "time"

// Status describes why a volume does or does not have usage numbers. Keeping
// "no data" distinct from "0% used" is the whole point: a PVC nobody mounted is
// not an empty PVC, and reporting it as 0% would be a lie.
type Status string

const (
	// StatusOK means kubelet returned fresh filesystem statistics.
	StatusOK Status = "ok"
	// StatusUnmounted means the PVC is bound but no running pod mounts it, so
	// no kubelet reports on it. Usage is unknown, not zero.
	StatusUnmounted Status = "unmounted"
	// StatusUnreported means a pod does mount the claim and its node answered,
	// but kubelet had no statistics for this volume. That is a property of the
	// storage driver, not of the claim: a CSI driver whose node plugin does not
	// advertise GET_VOLUME_STATS is never asked for them. Calling this
	// "unmounted" sent people looking for an orphan that does not exist.
	StatusUnreported Status = "unreported"
	// StatusBlock means the PVC is a raw block device. There is no filesystem
	// for kubelet to measure; only the provisioned size is known.
	StatusBlock Status = "block"
	// StatusPending means the PVC has not been bound to a PV yet.
	StatusPending Status = "pending"
	// StatusStale means the newest sample is older than collector.staleAfter.
	StatusStale Status = "stale"
	// StatusError means the cluster or the owning node could not be scraped.
	StatusError Status = "error"
)

// Volume is one PVC in one cluster, joined with whatever kubelet knows about it.
type Volume struct {
	Cluster        string `json:"cluster"`
	ClusterDisplay string `json:"clusterDisplay,omitempty"`
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`

	StorageClass string `json:"storageClass"`
	// Provisioner is the CSI driver or in-tree plugin behind the PV, which is
	// how a Trident volume is told apart from a PowerFlex or local-path one.
	Provisioner string   `json:"provisioner,omitempty"`
	VolumeName  string   `json:"volumeName,omitempty"`
	VolumeMode  string   `json:"volumeMode,omitempty"`
	AccessModes []string `json:"accessModes,omitempty"`
	Phase       string   `json:"phase,omitempty"`

	// RequestedBytes is spec.resources.requests.storage: what the user asked for.
	RequestedBytes int64 `json:"requestedBytes"`
	// ProvisionedBytes is status.capacity.storage: what the provisioner granted.
	ProvisionedBytes int64 `json:"provisionedBytes"`

	// Filesystem statistics as reported by kubelet. Valid only when HasStats.
	HasStats       bool  `json:"hasStats"`
	CapacityBytes  int64 `json:"capacityBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	Inodes         int64 `json:"inodes"`
	InodesUsed     int64 `json:"inodesUsed"`
	InodesFree     int64 `json:"inodesFree"`

	// UsagePercent is used/capacity against the filesystem kubelet measured.
	UsagePercent float64 `json:"usagePercent"`
	// RequestedPercent is used/requested, i.e. usage against the quota the user
	// asked for. The two differ when the backing filesystem is not dedicated.
	RequestedPercent float64 `json:"requestedPercent"`
	// InodesPercent catches the classic "disk has room but the volume is full"
	// failure, where an app has exhausted inodes with many small files.
	InodesPercent float64 `json:"inodesPercent"`

	// SharedFilesystem is set when kubelet's capacity is much larger than the
	// requested size, meaning the PVC sits on a filesystem shared with others
	// (local-path and hostPath do this) and UsagePercent reflects the host disk.
	SharedFilesystem bool `json:"sharedFilesystem"`

	Node     string   `json:"node,omitempty"`
	Pods     []string `json:"pods,omitempty"`
	Workload string   `json:"workload,omitempty"`

	Status  Status    `json:"status"`
	Message string    `json:"message,omitempty"`
	Sampled time.Time `json:"sampled,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

// Key uniquely identifies a volume across all clusters.
func (v Volume) Key() string {
	return v.Cluster + "/" + v.Namespace + "/" + v.Name
}

// Snapshot is the result of one full collection cycle across every cluster.
type Snapshot struct {
	GeneratedAt time.Time `json:"generatedAt"`
	DurationMS  int64     `json:"durationMs"`
	Volumes     []Volume  `json:"volumes"`
	// Errors are cluster-level failures; per-volume problems ride on the volume.
	Errors map[string]string `json:"errors,omitempty"`
}

// Totals is the aggregate shown in the UI header tiles.
type Totals struct {
	Clusters       int     `json:"clusters"`
	Namespaces     int     `json:"namespaces"`
	Volumes        int     `json:"volumes"`
	WithStats      int     `json:"withStats"`
	Unmounted      int     `json:"unmounted"`
	Unreported     int     `json:"unreported"`
	Warning        int     `json:"warning"`
	Critical       int     `json:"critical"`
	CapacityBytes  int64   `json:"capacityBytes"`
	UsedBytes      int64   `json:"usedBytes"`
	RequestedBytes int64   `json:"requestedBytes"`
	UsagePercent   float64 `json:"usagePercent"`
}
