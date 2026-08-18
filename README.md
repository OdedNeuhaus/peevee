<h1 align="center">Peevee</h1>

<p align="center">
  <em>PVC usage across every cluster you operate, in one table.</em><br>
  <sub><strong>PV + Eevee</strong> — it watches your PersistentVolumes, and it evolves into whatever storage backend you point it at.</sub>
</p>

---

Kubernetes will happily tell you a PVC is `Bound` and `10Gi`. It will not tell
you that it is 94% full. That number lives on the node, `kubectl` has no command
for it, and `metrics-server` throws it away.

Peevee collects it from every cluster you own and puts it in one place.

- **Every storage class.** Trident, PowerFlex, Ceph, NFS, local disks — one code
  path, no per-driver logic. A class it has never heard of works on day one.
- **Every cluster.** Drop a kubeconfig in a folder. Nothing is installed on the
  observed clusters, and no network route to their nodes is needed.
- **Honest about gaps.** A claim nobody mounts reports `unmounted`, never a
  misleading `0%`.
- **Alertable.** Remote-writes to Grafana Mimir using kubelet's own metric
  names, so existing dashboards and mixin alerts work unchanged.
- **One static binary.** ~31 MB distroless image, no database, ~11 MiB of RAM.

## How it works, briefly

Peevee doesn't measure anything itself. Kubelet already runs the equivalent of
`df` on every mounted volume, once a minute, and tags the result with the PVC it
belongs to. Peevee reads that through the API server proxy and joins it to the
PVC inventory.

```
kubeconfig ──► API server ──┬─► list PVCs, PVs, pods, nodes
                            └─► proxy → kubelet /stats/summary
                                        │
                            join on pvcRef
                                        │
                    snapshot ──► UI · /metrics · Mimir
```

Because it comes from `statfs`, it works for any driver that presents a
filesystem. Full detail, including what it *cannot* see and why:
**[docs/how-it-works.md](docs/how-it-works.md)**.

## Deploy

### A single cluster

```bash
helm install peevee ./charts/peevee \
  --namespace peevee --create-namespace \
  --set config.discovery.inClusterFallback=true

kubectl -n peevee port-forward svc/peevee 8080:80
```

### Many clusters

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
metric label. Keep it stable — renaming starts a new series in Mimir.

Adding a cluster later needs no reinstall and no restart. Update the Secret and
the directory is rescanned within `discovery.reloadInterval`:

```bash
kubectl -n peevee create secret generic peevee-kubeconfigs \
  --from-file=./kubeconfigs --dry-run=client -o yaml | kubectl apply -f -
```

### Permissions each observed cluster must grant

All read-only:

```yaml
- apiGroups: [""]
  resources: [persistentvolumeclaims, persistentvolumes, pods, nodes, namespaces]
  verbs: [get, list]
- apiGroups: [""]
  resources: [nodes/proxy]      # kubelet /stats/summary — where usage comes from
  verbs: [get]
- apiGroups: ["storage.k8s.io"]
  resources: [storageclasses]
  verbs: [get, list]
```

`nodes/proxy` is the one people forget. Without it every volume reports
`unmounted`.

### Grafana Mimir

```yaml
config:
  remoteWrite:
    enabled: true
    url: http://mimir-nginx.mimir.svc.cluster.local/api/v1/push
    tenantId: platform
```

Metric names, alert rules and a PromQL cookbook: **[docs/metrics.md](docs/metrics.md)**.

## Use

**Volumes** — summary tiles, then every claim. Filter by cluster, namespace,
storage class or status; search across claim, namespace, workload and node; sort
any column. Each row shows a usage bar, used/capacity, a sparkline and a
projected time-to-full. Click for the detail panel: inode usage, growth rate,
history chart, and every field collected. `/` focuses search, `Esc` closes.

**Clusters** — one card per cluster: reachable or not, version, how many
kubelets answered, scrape duration, and the exact error when something is wrong.
Kubeconfigs that failed to parse are listed by name.

**Configuration** — the effective config, structured or as YAML, plus remote
write status. Read-only by design; see
[docs/configuration.md](docs/configuration.md).

The table updates over server-sent events when a collection completes, not on a
polling timer.

## Security

Peevee has **no built-in authentication**, deliberately — it is one process, not
an identity provider. It does display the storage layout of every cluster it can
reach, so put an auth proxy in front of it:

```yaml
ingress:
  annotations:
    nginx.ingress.kubernetes.io/auth-url: https://oauth2-proxy.example.com/oauth2/auth
```

The container runs non-root on distroless with a read-only root filesystem, no
shell, and all capabilities dropped. It never writes to any cluster: every verb
it uses is `get` or `list`. Secrets are redacted before configuration reaches a
browser.

## Development

```bash
make check     # vet, tests, helm lint
make build     # ./peevee
make image     # container image
```

The UI has no build step. `web/ui/` is hand-written HTML, CSS and ES modules
embedded with `embed.FS` — edit and rebuild. Architecture notes and the roadmap
are in [CLAUDE.md](CLAUDE.md).

## License

MIT. See [LICENSE](LICENSE).

## The name

**Peevee** is *PV* said out loud, which happens to be *Eevee* — the Pokémon
defined by evolving into whichever type you give it. Same idea: one code path
reads kubelet, and it becomes a Trident monitor, a PowerFlex monitor or a
local-disk monitor depending only on what the cluster hands it.

The mark is an original drawing in Eevee's palette, built from a dozen SVG
shapes. Eevee and Pokémon are trademarks of Nintendo, Creatures Inc. and GAME
FREAK — fine for an internal tool, worth a thought before a conference booth.
