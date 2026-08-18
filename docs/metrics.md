# Metrics and Grafana Mimir

Peevee publishes the same series two ways: a `/metrics` endpoint for Prometheus
to scrape, and remote write into Mimir. They are independent — use either or
both.

## Enabling remote write

```yaml
config:
  remoteWrite:
    enabled: true
    url: http://mimir-nginx.mimir.svc.cluster.local/api/v1/push
    tenantId: platform
    externalLabels:
      source: peevee
```

The payload is a snappy-compressed protobuf `WriteRequest`, the standard
Prometheus remote write 1.0 protocol.

## Metric names

The filesystem series deliberately reuse **kubelet's own metric names**, so the
community Grafana dashboards and the kubernetes-mixin PVC alerts work against
them unchanged. Peevee adds a `cluster` label to make them fleet-wide.

| Metric | Notes |
|---|---|
| `kubelet_volume_stats_capacity_bytes` | Filesystem capacity |
| `kubelet_volume_stats_used_bytes` | Used bytes |
| `kubelet_volume_stats_available_bytes` | Free bytes |
| `kubelet_volume_stats_inodes{,_used,_free}` | Inode counts |
| `peevee_pvc_usage_ratio` | Used ÷ capacity, 0–1 |
| `peevee_pvc_inodes_ratio` | Inodes used ÷ total |
| `peevee_pvc_requested_bytes` | From the claim's spec — known even when unmounted |
| `peevee_pvc_provisioned_bytes` | From the claim's status |
| `peevee_pvc_growth_bytes_per_day` | Least-squares slope over retained history |
| `peevee_pvc_days_until_full` | Straight-line projection |
| `peevee_pvc_info` | Metadata to join on. **Stable labels only** |
| `peevee_pvc_status{status=…}` | State set: 1 for the active status, 0 for the rest |
| `peevee_cluster_up` | 1 if the API server answered |
| `peevee_cluster_pvcs`, `peevee_cluster_nodes_scraped`, `peevee_cluster_scrape_duration_seconds` | Per-cluster health |
| `peevee_scrape_duration_seconds`, `peevee_last_scrape_timestamp_seconds` | Collection health |
| `peevee_build_info` | Version and commit |

## Two deliberate design choices

**A claim with no statistics publishes no usage series at all.** No zeros are
emitted for `unmounted`, `pending` or `block` claims — a zero would be read as
an empty volume, which is a different and false statement. `peevee_pvc_requested_bytes`
is still published, because the requested size is known regardless and is what
capacity planning runs on.

**`status` is a state set, not a label on `peevee_pvc_info`.** A changing label
mints a new series on every transition, and the stale one keeps resolving for
the whole lookback window — so `status="pending"` would report claims that were
bound ten minutes ago. With a state set,
`peevee_pvc_status{status="pending"} == 1` is only ever true of claims that are
pending right now. Keep `peevee_pvc_info` to labels that are stable for the life
of a claim.

## Scraping instead

```yaml
serviceMonitor:
  enabled: true
  interval: 60s
```

## Alerts and queries

Ready-made rules: [`examples/mimir-alerts.yaml`](../examples/mimir-alerts.yaml)

```bash
mimirtool rules load --address=http://mimir:8080 --id=platform \
  examples/mimir-alerts.yaml
```

A set of worked queries — over-provisioning, unmounted waste, usage by storage
backend, growth — is in
[`examples/promql-cookbook.md`](../examples/promql-cookbook.md). Every query
there has been executed against a live Mimir, not just written down.
