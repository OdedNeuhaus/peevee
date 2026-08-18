package collector

import "time"

// The subset of kubelet's /stats/summary payload that matters for volumes.
// Kubelet exposes this on every node and it is the one place where filesystem
// usage is reported uniformly for every CSI driver, which is what lets a single
// code path cover Trident, PowerFlex, local-path, NFS and the rest.
type statsSummary struct {
	Node struct {
		NodeName string `json:"nodeName"`
	} `json:"node"`
	Pods []podStats `json:"pods"`
}

type podStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"podRef"`
	Volumes []volumeStats `json:"volume"`
}

type volumeStats struct {
	Time           time.Time `json:"time"`
	AvailableBytes *int64    `json:"availableBytes"`
	CapacityBytes  *int64    `json:"capacityBytes"`
	UsedBytes      *int64    `json:"usedBytes"`
	InodesFree     *int64    `json:"inodesFree"`
	Inodes         *int64    `json:"inodes"`
	InodesUsed     *int64    `json:"inodesUsed"`
	Name           string    `json:"name"`
	PVCRef         *struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"pvcRef"`
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
