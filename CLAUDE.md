# CLAUDE.md

Working notes for Peevee. Read this before changing anything; several decisions
here look arbitrary and are not.

## What this is

A read-only observability platform for PersistentVolumeClaim usage across many
Kubernetes clusters. It reads kubelet's `/stats/summary` through each cluster's
API server proxy, joins it to PVC inventory, serves a single-page UI, and remote
writes to Grafana Mimir.

Deep background: [docs/how-it-works.md](docs/how-it-works.md). Why a claim shows
no usage: [docs/troubleshooting.md](docs/troubleshooting.md).

## Commands

```bash
make check          # vet + go test ./... + helm lint  (what CI runs)
make build          # ./peevee
make image          # container image
go test ./internal/store/... -run TestTotals -v
helm template peevee charts/peevee --set config.remoteWrite.enabled=true
```

No Go toolchain locally? Everything runs in a container:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.24-alpine go test ./...
```

## Layout

```
cmd/peevee/main.go        wiring, flags, graceful shutdown
internal/
  config/                 YAML config, defaults, validation, Duration type
  cluster/                kubeconfig discovery, client pool, hot reload, health
  collector/              per-cluster scrape; joins PVC inventory + kubelet stats
  store/                  in-memory snapshot, per-volume history, filter/aggregate
  metrics/                snapshot → Prometheus samples (text + remote write)
  remotewrite/            hand-rolled protobuf + snappy → Mimir
  api/                    REST, SSE, /metrics, embedded UI, config redaction
  app/                    the collection loop
  model/                  shared types
web/ui/                   hand-written HTML/CSS/JS, embedded — NOT build output
charts/peevee/            Helm chart
scripts/                  kubeconfig generation, install helper
docs/, examples/          reference material, alert rules, PromQL cookbook
```

## Invariants

Breaking any of these silently produces wrong answers, which is worse than
crashing. Each has a test.

**1. Never report "no data" as zero.**
A claim nobody mounts is not an empty claim. `unmounted`, `unreported`,
`pending` and `block` carry no usage numbers, publish no usage series, and are
excluded from aggregates. `store_test.go:TestUnmountedIsUnknownNotZero`,
`store_test.go:TestUnreportedIsCountedSeparatelyFromUnmounted`,
`metrics_test.go:TestUnmountedPublishesNoUsageSeries`.

**2. Count each shared filesystem once in aggregates.**
`local-path` and friends report the whole host disk for every claim on it.
Summing per claim multiplies one disk by the number of claims. Dedupe key is
`(cluster, node, capacity)`. `store_test.go:TestTotalsCountSharedFilesystemOnce`.

**3. `peevee_pvc_info` carries only labels stable for a claim's lifetime.**
Anything changing — `status`, `phase` — mints a new series per transition, and
the stale one keeps resolving for the lookback window. Status is a state set
instead. `metrics_test.go:TestInfoCarriesOnlyStableLabels`.

**4. The state set emits every status, including the zeros.**
Emitting only the active one leaves the previous status stuck at 1.
`metrics_test.go:TestStatusIsAStateSetWithExactlyOneActive`.

**5. Redact before any config reaches a browser, and never mutate the original.**
The config struct's *yaml* tags include the remote write password that the json
tags drop, so the YAML view is the riskier path. `redact()` must copy — clearing
the password in place breaks remote write auth on the next push.
`api_test.go:TestRedactDoesNotMutateOriginal`.

**6. Remote write labels must be sorted by name.** Mimir rejects the request
otherwise. `proto_test.go`.

**7. Config is read-only in the UI.** Deliberate. A browser and `helm upgrade`
cannot both be the source of truth. Do not add write endpoints without
revisiting this with the user.

**8. Peevee never writes to an observed cluster.** Every verb is `get` or
`list`. Keep it that way; the RBAC in the chart and in
`scripts/create-remote-kubeconfig.sh` must stay in sync.

## Things that will bite you

- **Kubelet caches volume stats for 1 minute** (`volumeStatsAggPeriod`). Polling
  faster re-reads identical values and inflates the sparkline with duplicate
  points. One fresh sample per volume per minute is a hard ceiling.
- **`hostPath` PVs report nothing.** Kubelet has no metrics provider for that
  plugin. `local-path-provisioner` creates `local` volumes, which do report.
- **A CSI driver reports usage only if it advertises `GET_VOLUME_STATS`.**
  Kubelet never asks otherwise, so no amount of work here produces a number.
  PowerFlex gates it behind `node.healthMonitor.enabled` (issue #4). These land
  as `unreported`, and `collector.annotateSilentDrivers` names the driver when
  none of its claims report. Keep that inference data-driven — no per-driver
  code.
- **`[hidden]` in CSS.** Any rule setting `display` beats the browser's
  `display:none` for the attribute. There is a global
  `[hidden] { display: none !important; }` — do not remove it.
- **History is in memory.** Restart loses every sparkline. Mimir is the durable
  store.
- **Two logical clusters can point at one physical cluster** (the dev setup does
  this). The shared-filesystem dedupe is per cluster, so a shared node disk gets
  counted once per logical cluster.

## Known issues

**1. Misleading requested-percentage on shared filesystems.**
For a `shared` volume, `usedBytes` is the whole host disk, so `requestedPercent`
is arithmetic on unrelated numbers. The drawer currently says *"Against the
request it is at 1456%"*, which reads as if the claim blew past its quota. It
should say per-claim consumption is **unknowable** via `statfs`.
`web/ui/app.js` (drawer `sharedFilesystem` note), `model.Volume.RequestedPercent`.

**2. Default collector interval is below kubelet's cadence.**
`charts/peevee/values.yaml` ships `60s`, matching kubelet — but confirm dev
overrides do too. Anything below 60s doubles API traffic for no new data.

## Roadmap

Rough priority order. Each notes where the work lands.

### 1. Query Mimir for history instead of the in-memory ring
The biggest single upgrade. Sparklines and projections currently span ~90
minutes and die on restart. Reading `kubelet_volume_stats_used_bytes` back from
Mimir gives weeks of history and survives restarts.
→ New `internal/history` with a Mimir query client; `store.Row.Spark` and the
`/api/v1/history` handler read from it when configured, falling back to memory.
Reuse `remotewrite` config for the endpoint and tenant.

### 2. Right-sizing recommendations
The highest-value feature for a platform team. `requested` vs long-run peak
`used` shows exactly which claims are over-provisioned and by how much, with the
GiB reclaimable per namespace. Depends on (1) for a trustworthy peak.
→ `internal/store` for the calculation, a new UI column or a dedicated tab.

### 3. Cost attribution
Map storage class → currency per GiB per month in config, then show spend per
namespace, team label, or cluster. Turns "94% full" into a budget conversation.
→ `config.StorageClassCosts`, aggregation in `store`, a `peevee_pvc_cost_*`
series.

### 4. Authentication
Currently none, by design, with an auth proxy assumed. If it needs to reach a
wider audience, OIDC with group-based read/admin roles.
→ New `internal/auth` middleware in `api.Handler()`; chart values for the
provider. Revisit invariant 7 if admin roles arrive.

### 5. Orphaned-claim workflow
`unmounted` claims are money leaking. Show age-since-last-mount, and generate
(never apply) the cleanup manifests.
→ Needs to persist "last seen mounted", which memory-only state cannot do —
depends on (1) or a small local store.

### 6. Scale hardening for 10k+ claims
The table renders every row and the snapshot is copied per query. Virtualise the
table, paginate server-side, and avoid the full-slice copy in `store.Query`.
→ `web/ui/app.js`, `internal/store/store.go`.

### 7. StorageClass and backend-pool capacity
Per-claim usage misses the pool filling up underneath. Trident and PowerFlex
expose aggregate capacity through their own APIs.
→ Optional per-driver plugins under `internal/backends`. This is the first
feature that breaks the "no per-driver code" property — keep it strictly
additive and optional.

### 8. Smaller, worth doing
- **RBAC preflight**: probe `nodes/proxy` on cluster registration. Per-scrape
  failures already surface per volume and in the cluster panel; a preflight
  would catch it once at startup instead of once per claim.
- **Saved views**: persist filter combinations in the URL, so a link to
  "critical in prod" is shareable. `web/ui/app.js` only.
- **Grafana dashboard JSON** shipped in `examples/`.
- **VolumeSnapshot awareness**: snapshots consume backend capacity and are
  invisible here.
- **kubectl plugin** (`kubectl peevee`) reusing `internal/collector`.

## Conventions

- Comments explain **why**, not what. If a line looks odd, say what would break
  without it.
- Every non-obvious behaviour gets a test, named for the behaviour rather than
  the function (`TestUnmountedIsUnknownNotZero`, not `TestBuildVolume`).
- No new dependencies without a reason. Current set is client-go, yaml.v3 and
  snappy — the remote write protobuf is hand-rolled to avoid protoc.
- The UI stays build-step free. No npm, no bundler, no CDN.
- Chart values are commented in `values.yaml` itself; that file doubles as the
  configuration reference.
