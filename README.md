<p align="center">
  <img src="docs/assets/logo.png" alt="Peevee" width="132">
</p>

<h1 align="center">Peevee</h1>

<p align="center">
  <strong><code>df</code> for Kubernetes PVCs — across all your clusters.</strong>
</p>

<p align="center">
  <a href="https://github.com/OdedNeuhaus/peevee/releases"><img alt="release" src="https://img.shields.io/github/v/tag/OdedNeuhaus/peevee?label=release&color=B07C42"></a>
  <a href="https://github.com/OdedNeuhaus/peevee/pkgs/container/peevee"><img alt="image" src="https://img.shields.io/badge/ghcr.io-peevee-B07C42"></a>
  <a href="https://github.com/OdedNeuhaus/peevee/actions"><img alt="ci" src="https://github.com/OdedNeuhaus/peevee/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-B07C42"></a>
</p>

---

<img width="1872" height="792" alt="Peevee UI" src="https://github.com/user-attachments/assets/1091533c-962d-4d8d-a1ae-8468a29a7c4f" />

Peevee shows the **actual filesystem usage of every PersistentVolumeClaim you
operate**, from one place.

```text
CLUSTER    NAMESPACE    PVC              USED       CAPACITY    USAGE
prod-eu    postgres     postgres-data    82 GiB     100 GiB     82%
prod-us    gitlab       gitlab-data      31 GiB      50 GiB     62%
dev        redis        redis-data        4 GiB      20 GiB     20%
```

Kubernetes will happily tell you this:

```text
postgres-data    Bound    100Gi
```

It will not easily tell you this:

```text
postgres-data    82 / 100 GiB    82% full
```

**Peevee does.**

---

## Why Peevee?

PVC utilisation exists inside Kubernetes, but it is awkward to get to.

`kubectl get pvc` shows *provisioned* capacity — not how much space is used.
`kubectl top` covers CPU and memory — not storage. The real filesystem
statistics live on the **kubelet**, `kubectl` has no command for them, and
`metrics-server` throws them away.

Peevee collects those statistics and turns them into a centralised PVC
inventory.

- 🌐 **Multi-cluster** — every PVC from every cluster in one table
- 🚫 **Agentless** — nothing is installed on the observed clusters
- 💾 **Storage agnostic** — Ceph, PowerFlex, Trident, NFS, local disks; one code
  path, no per-driver logic, so a storage class it has never heard of works on
  day one
- 📊 **Actual usage** — used, available, capacity, utilisation, inodes
- 📈 **History & growth** — sparklines, growth rate, projected time-to-full
- 🔥 **Prometheus / Mimir** — expose or remote-write, using kubelet's own metric
  names so existing dashboards and mixin alerts work unchanged
- 🪶 **Lightweight** — one Go binary, no database, 9.8 MiB image, ~10 MiB of RAM
- 🔒 **Read-only** — Peevee never modifies a workload or a volume
- 🙈 **Honest about gaps** — a claim nobody mounts reports `unmounted`, one
  nothing measured reports `unreported`, never a misleading `0%`

---

## How it works

Peevee does not measure disk usage itself.

The kubelet already runs the equivalent of `df` on every mounted volume, once a
minute, and tags the result with the PVC it belongs to. Peevee reads that
through the Kubernetes API server and joins it with PVC metadata.

```text
                        ┌──────────────┐
                        │    Peevee    │
                        └──────┬───────┘
                               │
                        Kubernetes API
                               │
              ┌────────────────┴────────────────┐
              ▼                                 ▼
     PVC / PV / Pod / Node            nodes/<node>/proxy
           metadata                            │
              │                                ▼
              │                   kubelet /stats/summary
              │                                │
              │                        volume statistics
              │                                │
              └────────────────┬───────────────┘
                               ▼
                        join on pvcRef
                               │
                    ┌──────────┼──────────┐
                    ▼          ▼          ▼
                   UI      /metrics     Mimir
```

Concretely, Peevee reads:

```text
GET /api/v1/nodes/<node>/proxy/stats/summary
```

and extracts, per volume:

```text
capacityBytes    availableBytes    usedBytes
inodes           inodesFree        inodesUsed
```

Because these numbers come from `statfs` on the mounted filesystem rather than
from a particular CSI driver, Peevee needs no per-storage integrations.

Full detail, including what it *cannot* see and why:
**[docs/how-it-works.md](docs/how-it-works.md)**.

---

## Quick start

### One cluster

```bash
git clone https://github.com/OdedNeuhaus/peevee.git
cd peevee

helm install peevee ./charts/peevee \
  --namespace peevee --create-namespace \
  --set config.discovery.inClusterFallback=true
```

Then:

```bash
kubectl -n peevee port-forward svc/peevee 8080:80
```

Open **http://localhost:8080**.

The chart pulls a published multi-arch image (`linux/amd64`, `linux/arm64`);
nothing to build:

```text
ghcr.io/odedneuhaus/peevee:0.1.1
```

---

## Multi-cluster

Peevee monitors many clusters from a single deployment.

For each cluster you want observed, run this **against that cluster** with admin
credentials. It creates a read-only ServiceAccount and prints a kubeconfig:

```bash
./scripts/create-remote-kubeconfig.sh prod-eu > kubeconfigs/prod-eu.yaml
./scripts/create-remote-kubeconfig.sh prod-us > kubeconfigs/prod-us.yaml
```

> If the API server URL in your context is not reachable from where Peevee runs,
> set `SERVER=https://prod-eu.example.com:6443` when running it.

Then install with the whole directory:

```bash
./scripts/install-from-dir.sh ./kubeconfigs peevee \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=peevee.example.com
```

**The file name becomes the cluster name**, in the UI and in the `cluster`
metric label:

```text
kubeconfigs/
├── prod-eu.yaml      →  prod-eu
├── prod-us.yaml      →  prod-us
└── staging.yaml      →  staging
```

Keep those names stable — renaming one starts a new series in Mimir.

Adding a cluster later needs no reinstall and no restart. Update the Secret, and
the directory is rescanned within `discovery.reloadInterval`:

```bash
kubectl -n peevee create secret generic peevee-kubeconfigs \
  --from-file=./kubeconfigs --dry-run=client -o yaml | kubectl apply -f -
```

> An existing admin kubeconfig will work, but a dedicated read-only Peevee
> ServiceAccount is strongly recommended.

---

## Required permissions

Each observed cluster grants a small, entirely read-only set:

```yaml
- apiGroups: [""]
  resources: [persistentvolumeclaims, persistentvolumes, pods, nodes, namespaces]
  verbs: [get, list]
- apiGroups: [""]
  resources: [nodes/proxy]
  verbs: [get]
- apiGroups: ["storage.k8s.io"]
  resources: [storageclasses]
  verbs: [get, list]
```

### Why `nodes/proxy`?

This is the one people forget, and the one that matters.

A PVC object does **not** carry its filesystem utilisation. That number only
exists on the node, so Peevee asks the API server to proxy the request to the
kubelet:

```text
/api/v1/nodes/<node>/proxy/stats/summary
```

Without `nodes/proxy`, Peevee still discovers every claim — but cannot tell how
full any of them are: every volume reports `error`, naming the node and the
denied call.

> `nodes/proxy` grants broader access to kubelet APIs than an ordinary read-only
> role. Treat Peevee's credentials accordingly, and prefer a dedicated
> ServiceAccount over an administrator kubeconfig.

---

## What Peevee shows

### Volumes

Summary tiles, then every claim across every cluster: cluster, namespace, PVC,
storage class, workload, node, capacity, used, available, utilisation, inode
utilisation, growth rate and projected time-to-full.

Filter by cluster, namespace, storage class or status; search across claim,
namespace, workload and node; sort any column. The table shows 50 rows at a
time, adjustable down to 25 or up to all of them, and the choice is remembered.
Click a row for the detail panel — inode usage, growth rate, history chart and
every field collected. `/` focuses search, `Esc` closes.

Sorting, filtering and the totals always run over every matching claim, not the
page you are looking at, and the CSV export is the whole match.

Claims nobody mounts are reported as **`unmounted`**, claims still waiting for a
volume as **`pending`**, and claims that are mounted but whose storage driver
publishes no statistics as **`unreported`**. None is shown as `0%`, and none is
counted in the fleet totals — a claim with no data is not an empty claim.

### Clusters

One card per cluster: reachable or not, Kubernetes version, how many kubelets
answered, collection duration, and the exact error when something is wrong.
Kubeconfigs that failed to parse are listed by name.

### Configuration

The effective configuration, structured or as YAML, plus live remote-write
status. Read-only by design — a browser and `helm upgrade` cannot both be the
source of truth. See [docs/configuration.md](docs/configuration.md).

### Live updates

The UI refreshes over **Server-Sent Events** when a collection completes, not on
a browser polling loop.

---

## Metrics

Peevee exposes Prometheus-compatible metrics at `/metrics` and can remote-write
them to Grafana Mimir. Metric names deliberately match kubelet's own, so the
community dashboards and the kubernetes-mixin PVC alerts work unchanged:

```promql
100 * kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes
```

That makes the obvious things alertable — a PVC over 80%, over 90%, or on
course to fill within seven days.

### Grafana Mimir

```yaml
config:
  remoteWrite:
    enabled: true
    url: http://mimir-nginx.mimir.svc.cluster.local/api/v1/push
    tenantId: platform
```

Metric names, alert rules and a PromQL cookbook:
**[docs/metrics.md](docs/metrics.md)**, with ready-made rules in
[examples/mimir-alerts.yaml](examples/mimir-alerts.yaml).

---

## Architecture

Peevee is deliberately simple.

```text
                       Peevee
                          │
         ┌────────────────┼────────────────┐
         ▼                ▼                ▼
     Cluster A        Cluster B        Cluster C
     API server       API server       API server
         │                │                │
         ▼                ▼                ▼
      kubelet          kubelet          kubelet
         │                │                │
         └────────────────┼────────────────┘
                          ▼
                       snapshot
                          │
                ┌─────────┼─────────┐
                ▼         ▼         ▼
                UI     /metrics    Mimir
```

There is no agent on the observed clusters, no database, no driver-specific
integration, and no direct connection from Peevee to a worker node's kubelet
port. Everything goes through the API servers, using kubeconfigs.

---

## Security

Peevee never writes to an observed cluster. Every verb it uses is `get` or
`list`.

The container runs non-root (uid 65532) on a distroless base, with a read-only
root filesystem, `RuntimeDefault` seccomp, no privilege escalation, all Linux
capabilities dropped, and no shell to exec into.

Peevee has **no built-in authentication**, deliberately — it is one process, not
an identity provider. It does display the storage layout of every cluster it can
reach, so put an auth proxy or an authenticated ingress in front of it:

```yaml
ingress:
  annotations:
    nginx.ingress.kubernetes.io/auth-url: https://oauth2-proxy.example.com/oauth2/auth
```

Sensitive values are redacted before any configuration reaches a browser.

---

## Development

```bash
make check     # vet, tests, helm lint — what CI runs
make build     # ./peevee
make image     # container image
```

The frontend has no JavaScript build pipeline, on purpose. `web/ui/` is
hand-written HTML, CSS and ES modules, embedded into the binary with `embed.FS`
— edit and rebuild. Architecture notes and the roadmap live in
[CLAUDE.md](CLAUDE.md).

---

## Project structure

```text
cmd/peevee/        entry point, wiring, graceful shutdown

internal/
├── api/           REST, SSE, /metrics, embedded UI
├── app/           the collection loop
├── cluster/       kubeconfig discovery, client pool, health
├── collector/     per-cluster scrape: PVC inventory + kubelet stats
├── config/        YAML config, defaults, validation
├── metrics/       snapshot → Prometheus samples
├── model/         shared types
├── remotewrite/   protobuf + snappy → Mimir
├── store/         in-memory snapshot, history, filter, aggregate
└── version/       build information

web/ui/            embedded web UI (no build step)
charts/peevee/     Helm chart
docs/              how it works, metrics, configuration, troubleshooting
scripts/           kubeconfig generation, install helper
examples/          alert rules, PromQL cookbook
```

---

## Limitations

Peevee reports what the kubelet can see, which means a PVC generally has to be
mounted for volume statistics to exist at all. Rather than pretend, those cases
are reported as:

```text
unmounted     nobody mounts it
unreported    mounted, but nothing measured it
```

Three more worth knowing up front:

- **A CSI driver only reports usage if it advertises `GET_VOLUME_STATS`.** Some
  ship with it off — Dell PowerFlex needs `node.healthMonitor.enabled`. Peevee
  says so when a whole driver is silent, but the fix is on the driver.
- **`hostPath` volumes report nothing.** The kubelet has no metrics provider for
  that plugin. (`local-path-provisioner` creates `local` volumes, which do
  report.)
- **Shared filesystems report the whole host disk** for every claim on it, so
  Peevee counts each disk once in the totals and flags those rows as shared.

Details and the rest: [docs/how-it-works.md](docs/how-it-works.md). When a claim
reports no usage and you expected one,
[docs/troubleshooting.md](docs/troubleshooting.md) walks through the three
causes.

---

## Contributing

Peevee is young, and contributions are welcome. Useful areas:

- additional PVC insights and right-sizing recommendations
- alerting and capacity forecasting
- UI improvements
- Kubernetes version compatibility testing
- large multi-cluster and high-claim-count testing
- documentation
- performance

Found a bug or have an idea? [Open an issue](https://github.com/OdedNeuhaus/peevee/issues).

---

## License

MIT — see [LICENSE](LICENSE).
