# How Peevee measures PVC usage

## The short version

Peevee does not measure anything itself. It asks each node's kubelet what the
filesystem reports, and joins that to the PVC inventory from the API server.

```
       ┌──────────── per cluster, every interval ────────────┐
       │                                                     │
  kubeconfig ──► API server ──┬─► list PVCs, PVs, pods, nodes │
                              │                               │
                              └─► proxy to each kubelet:      │
                                  /stats/summary              │
       └─────────────────────────────────────────────────────┘
                              │
                              ▼
                    join on pvcRef ──► snapshot ──► UI + /metrics + Mimir
```

1. A pod mounts a PVC, so it becomes a real directory on a real node.
2. Kubelet calls `statfs()` on it — the syscall behind `df` — once per minute,
   and caches the result.
3. Kubelet publishes it at `/stats/summary`, tagged with a `pvcRef` naming the
   claim it belongs to.
4. Peevee reads that **through the API server proxy**
   (`/api/v1/nodes/<node>/proxy/stats/summary`), so it needs no network route to
   your nodes and nothing installed on the observed clusters.

```json
{
  "time": "2026-08-18T18:53:19Z",
  "capacityBytes": 1081101176832,
  "usedBytes":      160012247040,
  "inodesUsed":           449148,
  "pvcRef": { "name": "data-postgres-0", "namespace": "team-payments" }
}
```

## Why this covers every storage class

`statfs` reports whichever filesystem the volume actually sits on. Trident,
PowerFlex, Ceph, NFS and local disks all present a mounted filesystem, so they
all report through this one interface with **no per-driver code**. A storage
class Peevee has never heard of works on day one.

For a real CSI driver each PVC has its own filesystem, so capacity is
effectively the quota and the percentage is true per-claim usage.

## What it cannot see, and says so

Keeping "no data" distinct from "0% used" is the point: an orphaned claim is not
an empty one.

| Situation | Status | Why |
|---|---|---|
| No running pod mounts the claim | `unmounted` | Nothing has it mounted, so nobody is measuring it |
| `volumeMode: Block` | `block` | No filesystem to `statfs` |
| Claim not bound yet | `pending` | Nothing exists to measure |
| Sample older than `staleAfter` | `stale` | Shown with its age rather than as current |
| Cluster or node unreachable | `error` | Recorded per cluster; the rest still report |

**`hostPath` PVs report nothing at all.** Kubelet has no metrics provider for
that volume plugin, so a `hostPath` volume returns no statistics even while a
pod writes to it. Peevee reports it as `unmounted` with the message *"mounted,
but the node reported no filesystem statistics"*. Note that
`local-path-provisioner` creates `local` volumes, not `hostPath`, and those do
report.

## Shared filesystems

`local-path`, `hostPath` and Trident's `ontap-nas-economy` hand out directories
or qtrees carved from one larger filesystem. `statfs` then reports **the whole
underlying filesystem** for every claim on it.

Peevee detects this heuristically — kubelet capacity more than 1.5× the
requested size — flags the row `shared`, and explains it in the detail panel.
Fleet aggregates count each such filesystem **once** rather than once per claim,
which would otherwise multiply one disk by the number of claims on it.

For these volumes the percentage shown is of the underlying filesystem. That is
still the number that predicts write failures, because these provisioners
usually enforce no quota. **The per-claim consumption is genuinely unknowable
through `statfs`.**

## Sampling cadence

Kubelet recalculates volume statistics on `volumeStatsAggPeriod`, **1 minute by
default**, and serves a cached copy in between. Five requests a second apart
return byte-identical results.

**One fresh data point per volume per minute is a hard ceiling.** Polling faster
just re-reads the same value. Every sample carries kubelet's own timestamp, which
is what drives the `stale` status.

## Trend and projection

Peevee keeps up to 180 `(time, usedBytes)` points per volume in memory, fits a
least-squares line, and projects:

```
growthBytesPerDay = slope(usedBytes over time) × 86400
daysUntilFull     = (capacity − used) ÷ growthBytesPerDay
```

It refuses to answer unless there are at least 3 points spanning at least 5
minutes, never projects a shrinking volume, and caps at 10 years.

Two honest limits:

- **History is in memory only** and is lost on restart. Mimir is the durable
  store; `predict_linear` over hours there beats a 90-minute local window.
- **Straight-line extrapolation is naive.** Real databases grow in steps — WAL,
  compaction, retention cycles — so treat it as a planning hint, not a forecast.

## Why `kubectl` cannot show you this

Usage was never part of the PVC object. Its status carries only
`accessModes`, `capacity` and `phase` — declared facts, no telemetry.

Writing usage back would mean every kubelet issuing an API write per volume per
minute, hammering etcd with data that is stale on arrival. Kubernetes
deliberately keeps metrics out of the object model, which is also why
`kubectl get pod` shows no CPU.

There is no `kubectl top pvc`. And `metrics-server` reads **the same kubelet
endpoint Peevee reads** — it just extracts CPU and memory and discards the
volume section. The numbers are one HTTP call from your API server and nothing
in the standard toolchain consumes them. That gap is why this exists.

## Inodes

A filesystem has two independent budgets: bytes, and **inodes** — one per file,
directory or symlink, regardless of size. On ext4 the count is fixed at format
time, typically one inode per ~16 KB of space.

When inodes run out, writes fail with `ENOSPC` — **the identical error as a full
disk** — while `df -h` still shows space free. The rule of thumb:

> If your average file is larger than the bytes-per-inode ratio, you run out of
> space first. If it is smaller, you run out of inodes first.

Peevee collects inode counts alongside bytes and takes **whichever budget is
worse** when assigning severity, so a volume at 5% disk and 95% inodes is
correctly critical.
