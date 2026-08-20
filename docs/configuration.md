# Configuration

Configuration is a YAML file rendered from Helm values into a ConfigMap and
mounted read-only. Everything under `config:` in `values.yaml` maps one-to-one
onto it. [values.yaml](../charts/peevee/values.yaml) is commented throughout.

The **Configuration tab in the UI is read-only by design.** It shows exactly
what the running process loaded — in a structured view or as YAML you can paste
back into the chart. Making it writable would mean a browser and `helm upgrade`
both claiming to be the source of truth, and the next deploy silently reverting
whatever someone changed at 2am. The pod rolls automatically on a config
checksum change, so `helm upgrade` is always the way in.

## Reference

### `discovery`

| Key | Default | Notes |
|---|---|---|
| `dir` | `/etc/peevee/kubeconfigs` | Where the kubeconfig Secret is mounted |
| `allContexts` | `false` | Treat every context in a file as its own cluster |
| `reloadInterval` | `2m` | Rescan cadence; adding a cluster needs no restart |
| `inClusterFallback` | `false` | Also observe the cluster Peevee runs in, via its ServiceAccount |
| `fileExtensions` | `["", ".yaml", ".yml", ".conf", ".kubeconfig"]` | Which files to try |

**The file name becomes the cluster name** in the UI and in the `cluster` metric
label. Keep it stable — renaming a file starts a new series in Mimir.

### `collector`

| Key | Default | Notes |
|---|---|---|
| `interval` | `60s` | Matches kubelet's `volumeStatsAggPeriod`; going lower re-reads the same values |
| `timeout` | `30s` | Must not exceed `interval`, or scrapes overlap |
| `clusterConcurrency` | `4` | Clusters scraped at once |
| `nodeConcurrency` | `8` | Kubelets queried at once within a cluster |
| `includeNamespaces` | `[]` | Allow-list; mutually exclusive with the exclude list. Trailing `*` matches by prefix |
| `excludeNamespaces` | `[]` | Deny-list |
| `includeUnmounted` | `true` | Keep claims no pod mounts; they report `unmounted`, not a misleading 0%. Claims that are mounted but unmeasured (`unreported`) are always kept |
| `staleAfter` | `10m` | Older kubelet samples are marked `stale` |

### `remoteWrite`

Ships every series to Grafana Mimir, or anything speaking the Prometheus remote
write protocol. See [metrics.md](metrics.md).

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | |
| `url` | — | Required when enabled |
| `tenantId` | — | Sent as `X-Scope-OrgID`; Mimir rejects writes without it unless auth is off |
| `timeout` | `30s` | |
| `externalLabels` | `{}` | Attached to every series |
| `headers` | `{}` | Extra headers; secret-shaped names are redacted in the UI |
| `basicAuth` | — | `username` plus `passwordFile` |
| `bearerTokenFile` | — | |
| `tlsConfig` | — | `insecureSkipVerify`, `caFile`, `serverName` |
| `maxSamplesPerSend` | `500` | Batch size |
| `maxShards` | `2` | Batches pushed in parallel |
| `maxRetries` | `3` | Retries 5xx and 429 with backoff; never retries 4xx |

### `ui`

| Key | Default | Notes |
|---|---|---|
| `title` | `Peevee` | Shown in the header and browser tab |
| `thresholds.warning` | `75` | Percent; drives the amber state and "At risk only" |
| `thresholds.critical` | `90` | Percent; must be above `warning` |

### `clusters`

Per-cluster overrides keyed by the discovered name:

```yaml
clusters:
  - name: prod-eu
    displayName: Production (Frankfurt)
    labels: { tier: production }
    disabled: false
    insecureSkipTlsVerify: false
```

## Secrets

Credentials never belong in the ConfigMap. Two ways in:

**A mounted file**, via `secrets.existingSecret`, mounted at
`/etc/peevee/secrets`:

```yaml
config:
  remoteWrite:
    basicAuth:
      username: peevee
      passwordFile: /etc/peevee/secrets/mimir-password
secrets:
  existingSecret: mimir-credentials
```

**An environment variable**, which overrides the file:

```yaml
extraEnv:
  - name: PEEVEE_REMOTE_WRITE_PASSWORD
    valueFrom:
      secretKeyRef: { name: mimir-credentials, key: password }
```

Recognised: `PEEVEE_REMOTE_WRITE_URL`, `_TENANT_ID`, `_USERNAME`, `_PASSWORD`.

Both config views redact before rendering: passwords are dropped and headers
whose names look like credentials become `***redacted***`.

## Validation

Invalid configuration fails at startup with every problem listed at once, rather
than crashing on the first one at runtime. Checks include `timeout` not
exceeding `interval`, thresholds being ordered and in range, the namespace lists
being mutually exclusive, and `remoteWrite.url` being present and http(s) when
remote write is enabled.
