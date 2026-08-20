# Troubleshooting

Peevee measures nothing itself: it reads what each node's kubelet reports and
joins it to the PVC inventory. Almost every "why is this claim not showing
usage" question is therefore one of three things — nobody mounts the claim, we
could not ask the node, or the node was asked and had nothing to say about that
volume.

## Start here: what does kubelet actually have?

Peevee cannot show a number kubelet never collected, so check the source before
anything else. Against the same cluster and node:

```bash
kubectl get --raw /api/v1/nodes/<node>/proxy/stats/summary |
  jq '.pods[].volume[]? | select(.pvcRef) | {pvc: .pvcRef.name, capacityBytes, usedBytes}'
```

- **The claim appears with `capacityBytes` and `usedBytes`** — kubelet has the
  data, so the problem is between us and the API server. Check the cluster panel
  for scrape errors and confirm the ServiceAccount has `nodes/proxy`.
- **The claim is missing, or present with null capacity** — kubelet has no data,
  and no change to Peevee can invent it. Continue below.
- **The command itself is denied** — that is the RBAC case, and Peevee's scrape
  is being denied in exactly the same way.

## A claim reports `error` naming its node

The node's kubelet could not be scraped, so nothing on it can report usage. The
message carries the reason, and the cluster panel lists every failing node:

| Message | Cause |
|---|---|
| not authorised to read `nodes/proxy` on this cluster | The ServiceAccount or kubeconfig lacks the `nodes/proxy` verb. See the RBAC section in the README. |
| credentials rejected by the API server | Expired or wrong token in the kubeconfig. |
| the API server has no proxy route to this kubelet | Usually a node that left the cluster between our node list and the scrape. |
| timed out reading kubelet stats | Node under load, or `collector.timeout` too tight for the cluster. |
| node is not Ready | Kubelet is down; its claims cannot be measured until it returns. |

This is a Peevee-side or cluster-side access problem, not a storage problem.

## A claim reports `unreported`

The node answered, and simply had no entry for that volume. The claim is mounted
and in use — this is not the `unmounted` bucket and nothing here should be
cleaned up.

When every claim from one provisioner in a cluster is silent while other drivers
report normally, Peevee names the driver instead of leaving each claim to look
individually broken:

```text
no claim provisioned by csi-vxflexos.dellemc.com reports statistics in this
cluster (0 of 47); the driver most likely does not advertise the CSI
GET_VOLUME_STATS capability
```

Kubelet only collects filesystem statistics for a CSI volume when the driver's
**node plugin advertises the `GET_VOLUME_STATS` capability**. A driver that does
not advertise it is never asked, so there is nothing to read — for that volume,
for `kubelet_volume_stats_*`, or for any other tool built on the same data.

### Dell PowerFlex (`csi-vxflexos.dellemc.com`)

PowerFlex advertises `GET_VOLUME_STATS` only when the node health monitor is
enabled. It is off by default, which leaves every PowerFlex claim unmeasured.
Requires driver **v2.1 or later**:

```yaml
# values.yaml for the csi-powerflex chart
node:
  healthMonitor:
    enabled: true
```

Apply it and restart the node pods:

```bash
kubectl rollout restart daemonset/vxflexos-node -n vxflexos
```

Statistics appear on the next kubelet aggregation, and Peevee picks them up on
its following scrape — no restart or configuration change on our side.

### Other drivers

The same pattern applies to any CSI driver with an optional stats capability;
check its chart for a `healthMonitor`, `volumeHealth` or
`csi-external-health-monitor` switch. Two cases are not fixable at all:

- **`hostPath` volumes.** Kubelet has no metrics provider for that plugin. Note
  that `local-path-provisioner` creates `local` volumes, which do report.
- **`volumeMode: Block`.** There is no filesystem to `statfs`; these report
  `block` rather than `unreported`.

## Usage looks wrong on `local-path`, `hostPath` or `ontap-nas-economy`

These hand out directories carved from one larger filesystem, so `statfs`
returns the **whole underlying disk** for every claim on it. Peevee flags those
rows as `shared` and counts each disk once in the fleet totals. The per-claim
percentage is of the host disk, not of the claim's request.

## Sparklines are short, or empty after a restart

History is in memory and is lost on restart; roughly 90 minutes are kept. Mimir
is the durable store — see [metrics.md](metrics.md) for remote write.

## Everything reports `error` in a whole cluster

Almost always missing `nodes/proxy`. Peevee lists every claim it discovers from
the API server, but without the kubelet proxy it can never attach usage to any
of them. Every claim names its node and the denied call, and the cluster panel
lists the failing nodes. Verify with the `kubectl get --raw` command above.
