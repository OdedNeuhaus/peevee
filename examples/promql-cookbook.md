# PromQL cookbook

Queries against the series peevee writes to Mimir. Every query here has been
executed against a live Mimir instance, not just written down. Set the tenant header to
whatever `remoteWrite.tenantId` is, e.g. `-H "X-Scope-OrgID: platform"`.

### The 20 fullest volumes in the fleet
```promql
topk(20, kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes)
```

### Storage requested per cluster, in TiB
```promql
sum by (cluster) (peevee_pvc_requested_bytes) / 1024^4
```

### Requested vs actually used, per team namespace
The gap is over-provisioning: storage paid for and never written to.
```promql
sum by (namespace) (peevee_pvc_requested_bytes)
  - sum by (namespace) (kubelet_volume_stats_used_bytes)
```

### Which storage backends carry the most data
`storageclass` and `provisioner` live on `peevee_pvc_info`, so join to them.
```promql
sum by (storageclass) (
  kubelet_volume_stats_used_bytes
  * on (cluster, namespace, persistentvolumeclaim) group_left (storageclass)
    peevee_pvc_info
)
```

### Claims nobody is mounting, and what they cost
`and on (...)` filters one series by another without pulling in its labels,
which is what you want here: `group_left` expects label names, not an expression.
```promql
peevee_pvc_requested_bytes
  and on (cluster, namespace, persistentvolumeclaim)
    (peevee_pvc_status{status="unmounted"} == 1)
```

### Total wasted capacity from unmounted claims, in GiB
```promql
sum(
  peevee_pvc_requested_bytes
    and on (cluster, namespace, persistentvolumeclaim)
      (peevee_pvc_status{status="unmounted"} == 1)
) / 1024^3
```

### Fastest growing volumes
```promql
topk(10, peevee_pvc_growth_bytes_per_day)
```

### Volumes that will fill before the weekend
```promql
peevee_pvc_days_until_full < 3
```

### Growth measured from the raw series instead of peevee's projection
Useful when you want a window other than peevee's retained history.
```promql
predict_linear(kubelet_volume_stats_used_bytes[6h], 4 * 24 * 3600)
  > kubelet_volume_stats_capacity_bytes
```

### Volumes on a shared host filesystem
Their percentage is of the node's disk, not of the claim's request, so exclude
them when reporting per-claim utilisation.
```promql
peevee_pvc_info{shared_filesystem="true"}
```

### Per-claim utilisation, excluding shared filesystems
```promql
(kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes)
  * on (cluster, namespace, persistentvolumeclaim) group_left
    peevee_pvc_info{shared_filesystem="false"}
```

### Collection health
```promql
peevee_cluster_up                        # 1 per reachable cluster
peevee_cluster_scrape_duration_seconds   # how slow a cluster is to scrape
peevee_cluster_nodes_scraped             # kubelets that answered
time() - peevee_last_scrape_timestamp_seconds
```
