// Package collector scrapes PVC inventory and filesystem usage from every
// configured cluster and joins the two into a single snapshot.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/OdedNeuhaus/peevee/internal/cluster"
	"github.com/OdedNeuhaus/peevee/internal/config"
	"github.com/OdedNeuhaus/peevee/internal/model"
)

type Collector struct {
	pool *cluster.Pool
	cfg  config.Config
	log  *slog.Logger
}

func New(pool *cluster.Pool, cfg config.Config, log *slog.Logger) *Collector {
	return &Collector{pool: pool, cfg: cfg, log: log}
}

// Collect scrapes every cluster concurrently and returns a merged snapshot.
// A cluster that fails is recorded in Snapshot.Errors; the rest still report.
func (c *Collector) Collect(ctx context.Context) model.Snapshot {
	start := time.Now()
	clusters := c.pool.Clusters()

	var (
		mu      sync.Mutex
		volumes []model.Volume
		errs    = map[string]string{}
		sem     = make(chan struct{}, c.cfg.Collector.ClusterConcurrency)
		wg      sync.WaitGroup
	)

	for _, cl := range clusters {
		wg.Add(1)
		go func(cl *cluster.Cluster) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			cctx, cancel := context.WithTimeout(ctx, c.cfg.Collector.Timeout.D())
			defer cancel()

			clusterStart := time.Now()
			health := &cluster.Health{
				Name:        cl.Name,
				DisplayName: cl.DisplayName,
				Source:      cl.Source,
				Context:     cl.Context,
				Endpoint:    cl.Endpoint,
				Labels:      cl.Labels,
				LastCheck:   time.Now(),
			}

			vols, err := c.collectCluster(cctx, cl, health)
			health.ScrapeMillis = time.Since(clusterStart).Milliseconds()
			if err != nil {
				health.Reachable = false
				health.Error = err.Error()
				c.log.Error("cluster scrape failed", "cluster", cl.Name, "error", err)
			} else {
				health.Reachable = true
			}
			c.pool.SetHealth(health)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[cl.Name] = err.Error()
				return
			}
			volumes = append(volumes, vols...)
		}(cl)
	}
	wg.Wait()

	sort.Slice(volumes, func(i, j int) bool {
		a, b := volumes[i], volumes[j]
		if a.Cluster != b.Cluster {
			return a.Cluster < b.Cluster
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	return model.Snapshot{
		GeneratedAt: start,
		DurationMS:  time.Since(start).Milliseconds(),
		Volumes:     volumes,
		Errors:      errs,
	}
}

// collectCluster gathers inventory and usage for one cluster.
func (c *Collector) collectCluster(ctx context.Context, cl *cluster.Cluster, health *cluster.Health) ([]model.Volume, error) {
	if v, err := cl.Client.Discovery().ServerVersion(); err == nil {
		health.Version = v.GitVersion
	}

	pvcList, err := cl.Client.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list persistentvolumeclaims: %w", err)
	}

	// PVs give us the provisioner, which is the only reliable way to attribute a
	// volume to its storage backend when several classes share a driver.
	provisioners := map[string]string{}
	if pvList, err := cl.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err != nil {
		c.log.Warn("list persistentvolumes failed; provisioner will be blank", "cluster", cl.Name, "error", err)
	} else {
		for i := range pvList.Items {
			pv := &pvList.Items[i]
			provisioners[pv.Name] = provisionerOf(pv)
		}
	}

	pods, err := cl.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	mounts := indexMounts(pods.Items)

	nodes, err := cl.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	health.NodesTotal = len(nodes.Items)

	// Only ask nodes that actually host one of our PVCs, so a large cluster does
	// not pay for kubelet round-trips to nodes with no volumes on them.
	wanted := map[string]bool{}
	for _, m := range mounts {
		if m.node != "" {
			wanted[m.node] = true
		}
	}
	scrape := c.scrapeNodes(ctx, cl, nodes.Items, wanted)
	health.NodesScraped = scrape.scraped
	health.NodeErrors = scrape.failed

	staleAfter := c.cfg.Collector.StaleAfter.D()
	out := make([]model.Volume, 0, len(pvcList.Items))
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if !c.namespaceAllowed(pvc.Namespace) {
			continue
		}
		v := c.buildVolume(cl, pvc, provisioners, mounts, scrape, staleAfter)
		// includeUnmounted is about claims nobody uses. An unreported claim has
		// a live workload on it, so dropping it would hide a real volume behind
		// a setting that reads as "hide the idle ones".
		if !v.HasStats && !c.cfg.Collector.IncludeUnmounted && v.Status == model.StatusUnmounted {
			continue
		}
		out = append(out, v)
	}
	annotateSilentDrivers(out)
	health.PVCsTotal = len(out)
	return out, nil
}

// annotateSilentDrivers looks for provisioners that reported nothing at all in
// this cluster. One claim with no statistics says little; every claim from one
// driver having none, while other drivers in the same cluster report normally,
// is the driver not advertising GET_VOLUME_STATS. Deriving it from the data
// keeps peevee free of per-driver knowledge and catches hostPath and Trident
// the same way it catches PowerFlex.
func annotateSilentDrivers(vols []model.Volume) {
	type tally struct{ reporting, silent int }
	byProvisioner := map[string]*tally{}

	for _, v := range vols {
		if v.Provisioner == "" {
			continue
		}
		t := byProvisioner[v.Provisioner]
		if t == nil {
			t = &tally{}
			byProvisioner[v.Provisioner] = t
		}
		switch {
		case v.HasStats:
			t.reporting++
		case v.Status == model.StatusUnreported:
			t.silent++
		}
	}

	for i := range vols {
		v := &vols[i]
		if v.Status != model.StatusUnreported {
			continue
		}
		t := byProvisioner[v.Provisioner]
		if t == nil || t.reporting > 0 {
			continue
		}
		v.Message = fmt.Sprintf(
			"no claim provisioned by %s reports statistics in this cluster (%d of %d); the driver most likely does not advertise the CSI GET_VOLUME_STATS capability",
			v.Provisioner, t.reporting, t.reporting+t.silent)
	}
}

// buildVolume joins one PVC with its mount and kubelet statistics.
func (c *Collector) buildVolume(
	cl *cluster.Cluster,
	pvc *corev1.PersistentVolumeClaim,
	provisioners map[string]string,
	mounts map[string]mountInfo,
	scrape scrapeResult,
	staleAfter time.Duration,
) model.Volume {
	v := model.Volume{
		Cluster:        cl.Name,
		ClusterDisplay: cl.DisplayName,
		Namespace:      pvc.Namespace,
		Name:           pvc.Name,
		VolumeName:     pvc.Spec.VolumeName,
		Phase:          string(pvc.Status.Phase),
		Labels:         pvc.Labels,
	}
	if pvc.Spec.StorageClassName != nil {
		v.StorageClass = *pvc.Spec.StorageClassName
	}
	if v.StorageClass == "" {
		// An empty class is legal and means "the default class"; naming it
		// keeps the UI filter from showing a blank entry.
		v.StorageClass = "(none)"
	}
	v.Provisioner = provisioners[pvc.Spec.VolumeName]
	if pvc.Spec.VolumeMode != nil {
		v.VolumeMode = string(*pvc.Spec.VolumeMode)
	} else {
		v.VolumeMode = string(corev1.PersistentVolumeFilesystem)
	}
	for _, am := range pvc.Spec.AccessModes {
		v.AccessModes = append(v.AccessModes, string(am))
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		v.RequestedBytes = q.Value()
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		v.ProvisionedBytes = q.Value()
	}

	key := pvc.Namespace + "/" + pvc.Name
	if m, ok := mounts[key]; ok {
		v.Pods = m.pods
		v.Node = m.node
		v.Workload = m.workload
	}

	sample, hasSample := scrape.samples[key]

	switch {
	case pvc.Status.Phase != corev1.ClaimBound:
		v.Status = model.StatusPending
		v.Message = fmt.Sprintf("claim is %s", strings.ToLower(string(pvc.Status.Phase)))
		return v

	case v.VolumeMode == string(corev1.PersistentVolumeBlock):
		// Raw block devices have no filesystem, so kubelet cannot measure them.
		// Whatever is inside is the workload's business, not ours.
		v.Status = model.StatusBlock
		v.Message = "raw block volume; filesystem usage is not observable from outside the workload"
		return v

	case !hasSample:
		// A claim whose node we never managed to ask is not an orphan. Calling
		// it "unmounted" hides a denied nodes/proxy call behind a status that
		// reads as a storage problem, so name the node and the reason.
		if reason, failed := scrape.failed[v.Node]; v.Node != "" && failed {
			v.Status = model.StatusError
			v.Message = fmt.Sprintf("node %s could not be scraped: %s", v.Node, reason)
			return v
		}
		if len(v.Pods) == 0 {
			v.Status = model.StatusUnmounted
			v.Message = "no running pod mounts this claim, so no kubelet reports its usage"
			return v
		}
		// Mounted, node scraped fine, still no sample. The claim is in use and
		// nothing about it is wrong, so it is not "unmounted"; the driver never
		// published a measurement. annotateSilentDrivers may sharpen this.
		v.Status = model.StatusUnreported
		v.Message = "mounted and its node was scraped, but kubelet reported no statistics for it; the storage driver may not publish volume stats"
		return v
	}

	v.HasStats = true
	v.CapacityBytes = sample.capacity
	v.UsedBytes = sample.used
	v.AvailableBytes = sample.available
	v.Inodes = sample.inodes
	v.InodesUsed = sample.inodesUsed
	v.InodesFree = sample.inodesFree
	v.Sampled = sample.time
	if sample.node != "" {
		v.Node = sample.node
	}

	v.UsagePercent = percent(v.UsedBytes, v.CapacityBytes)
	v.RequestedPercent = percent(v.UsedBytes, v.RequestedBytes)
	v.InodesPercent = percent(v.InodesUsed, v.Inodes)

	// When kubelet's filesystem is materially bigger than what was requested,
	// the PVC is not on a dedicated volume: local-path and hostPath report the
	// whole node disk. Flagging it stops "3% used" from being read as headroom
	// on a claim that is actually capped at its requested size.
	if v.RequestedBytes > 0 && v.CapacityBytes > 0 {
		if float64(v.CapacityBytes) > float64(v.RequestedBytes)*1.5 {
			v.SharedFilesystem = true
			v.Message = "backing filesystem is shared with the node, so percentage is of the host disk"
		}
	}

	v.Status = model.StatusOK
	if staleAfter > 0 && !sample.time.IsZero() && time.Since(sample.time) > staleAfter {
		v.Status = model.StatusStale
		v.Message = fmt.Sprintf("kubelet sample is %s old", time.Since(sample.time).Truncate(time.Second))
	}
	return v
}

func percent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// volumeSample is one kubelet reading for a PVC.
type volumeSample struct {
	capacity   int64
	used       int64
	available  int64
	inodes     int64
	inodesUsed int64
	inodesFree int64
	time       time.Time
	node       string
}

// scrapeResult is one round of kubelet queries: the samples that came back, and
// the nodes that could not be asked. Which nodes failed is not a diagnostic
// detail — it is the reason a claim on one of them has no sample, and reporting
// that claim as "unmounted" sends people hunting for an orphan that is really a
// denied nodes/proxy call or a node that is down.
type scrapeResult struct {
	samples map[string]volumeSample
	// failed maps node name to why it could not be scraped.
	failed  map[string]string
	scraped int
}

// scrapeNodes queries every relevant kubelet concurrently through the API server
// proxy, which means peevee needs no network path to the nodes themselves.
func (c *Collector) scrapeNodes(ctx context.Context, cl *cluster.Cluster, nodes []corev1.Node, wanted map[string]bool) scrapeResult {
	var (
		mu  sync.Mutex
		res = scrapeResult{samples: map[string]volumeSample{}, failed: map[string]string{}}
		wg  sync.WaitGroup
		sem = make(chan struct{}, c.cfg.Collector.NodeConcurrency)
	)

	for i := range nodes {
		node := nodes[i]
		// An empty wanted set means we could not tell which nodes matter, so
		// fall back to asking all of them.
		if len(wanted) > 0 && !wanted[node.Name] {
			continue
		}
		if !nodeReady(&node) {
			// A NotReady kubelet answers nothing, so record it as the reason
			// rather than letting its claims look unmounted.
			res.failed[node.Name] = "node is not Ready"
			continue
		}

		wg.Add(1)
		go func(nodeName string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			samples, err := c.nodeVolumeStats(ctx, cl, nodeName)
			if err != nil {
				c.log.Warn("kubelet stats unavailable",
					"cluster", cl.Name, "node", nodeName, "error", err)
				mu.Lock()
				defer mu.Unlock()
				res.failed[nodeName] = scrapeError(err)
				return
			}

			mu.Lock()
			defer mu.Unlock()
			res.scraped++
			for key, s := range samples {
				// A ReadWriteMany volume is reported by every node that mounts
				// it. Keep the highest usage so we never under-report.
				if prev, ok := res.samples[key]; ok && prev.used >= s.used {
					continue
				}
				res.samples[key] = s
			}
		}(node.Name)
	}
	wg.Wait()
	return res
}

// scrapeError turns a kubelet proxy failure into something a person reading one
// table cell can act on. A denied nodes/proxy call is by far the most common
// cause and looks nothing like a driver problem, so it is called out by name.
func scrapeError(err error) string {
	msg := err.Error()
	switch {
	case apierrors.IsForbidden(err):
		return "not authorised to read nodes/proxy on this cluster"
	case apierrors.IsUnauthorized(err):
		return "credentials rejected by the API server"
	case apierrors.IsNotFound(err):
		return "the API server has no proxy route to this kubelet"
	case apierrors.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded):
		return "timed out reading kubelet stats"
	}
	// Long client-go errors wrap to several lines in a drawer; the first line
	// carries the cause.
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return msg
}

func (c *Collector) nodeVolumeStats(ctx context.Context, cl *cluster.Cluster, node string) (map[string]volumeSample, error) {
	raw, err := cl.Client.CoreV1().RESTClient().
		Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats", "summary").
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}

	var summary statsSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decode kubelet summary: %w", err)
	}

	out := map[string]volumeSample{}
	for _, pod := range summary.Pods {
		for _, vol := range pod.Volumes {
			if vol.PVCRef == nil || vol.PVCRef.Name == "" {
				continue
			}
			// Kubelet emits an entry with no capacity for volumes it could not
			// stat; treating that as a real 0-byte volume would be wrong.
			if vol.CapacityBytes == nil && vol.UsedBytes == nil {
				continue
			}
			key := vol.PVCRef.Namespace + "/" + vol.PVCRef.Name
			s := volumeSample{
				capacity:   deref(vol.CapacityBytes),
				used:       deref(vol.UsedBytes),
				available:  deref(vol.AvailableBytes),
				inodes:     deref(vol.Inodes),
				inodesUsed: deref(vol.InodesUsed),
				inodesFree: deref(vol.InodesFree),
				time:       vol.Time,
				node:       node,
			}
			if prev, ok := out[key]; ok && prev.used >= s.used {
				continue
			}
			out[key] = s
		}
	}
	return out, nil
}

func nodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// mountInfo records which pods mount a claim and where they run.
type mountInfo struct {
	pods     []string
	node     string
	workload string
}

// indexMounts maps "namespace/pvc" to the pods using it. Only pods that are
// actually running can produce kubelet statistics.
func indexMounts(pods []corev1.Pod) map[string]mountInfo {
	out := map[string]mountInfo{}
	for i := range pods {
		pod := &pods[i]
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim == nil {
				continue
			}
			key := pod.Namespace + "/" + vol.PersistentVolumeClaim.ClaimName
			m := out[key]
			m.pods = append(m.pods, pod.Name)
			// Prefer a running pod's node: that is the kubelet with the stats.
			if m.node == "" || pod.Status.Phase == corev1.PodRunning {
				if pod.Spec.NodeName != "" {
					m.node = pod.Spec.NodeName
				}
			}
			if m.workload == "" {
				m.workload = workloadOf(pod)
			}
			out[key] = m
		}
	}
	for k, m := range out {
		sort.Strings(m.pods)
		out[k] = m
	}
	return out
}

// workloadOf names the controller behind a pod, so the UI can say "prometheus"
// rather than "prometheus-server-7d9f8c6b5-x2k9p".
func workloadOf(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			// A ReplicaSet is named <deployment>-<podtemplatehash>; trimming the
			// hash recovers the Deployment without another API call.
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 {
				return "Deployment/" + ref.Name[:idx]
			}
			return "ReplicaSet/" + ref.Name
		default:
			return ref.Kind + "/" + ref.Name
		}
	}
	return ""
}

func provisionerOf(pv *corev1.PersistentVolume) string {
	if pv.Spec.CSI != nil {
		return pv.Spec.CSI.Driver
	}
	// In-tree and host-local volume sources predate CSI but still show up in
	// real clusters, so name them rather than reporting an empty provisioner.
	switch {
	case pv.Spec.NFS != nil:
		return "nfs"
	case pv.Spec.HostPath != nil:
		return "hostpath"
	case pv.Spec.Local != nil:
		return "local"
	case pv.Spec.ISCSI != nil:
		return "iscsi"
	case pv.Spec.FC != nil:
		return "fibrechannel"
	case pv.Spec.RBD != nil:
		return "rbd"
	}
	if p, ok := pv.Annotations["pv.kubernetes.io/provisioned-by"]; ok {
		return p
	}
	return ""
}

func (c *Collector) namespaceAllowed(ns string) bool {
	if len(c.cfg.Collector.IncludeNamespaces) > 0 {
		return matchAny(ns, c.cfg.Collector.IncludeNamespaces)
	}
	if len(c.cfg.Collector.ExcludeNamespaces) > 0 {
		return !matchAny(ns, c.cfg.Collector.ExcludeNamespaces)
	}
	return true
}

// matchAny supports an exact name or a trailing "*" prefix glob, which covers
// the common "everything under openshift-" case without a regex dependency.
func matchAny(s string, patterns []string) bool {
	for _, p := range patterns {
		if p == s {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(s, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}
