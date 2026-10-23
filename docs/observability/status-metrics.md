# Status metrics mode (OKEP-6414)

Opt-in mode that stops high-churn per-node status patches on selected CRDs and
exports sync outcomes as Prometheus metrics instead.

## Feature flag

| Flag | Default | Behaviour |
|------|---------|-----------|
| `--enable-status-metrics` / `OVN_ENABLE_STATUS_METRICS` / Helm `global.enableStatusMetrics` | `false` | Legacy per-node status shards + `StatusManager` rollup |
| (same, `true`) | | Per-node controllers export metrics only; cluster-manager rolls up a coarse summary from Prometheus when configured |

Prometheus query URL for cluster-manager rollup:

* CLI: `--metrics-status-prometheus-url`
* Env: `OVN_STATUS_METRICS_PROMETHEUS_URL`
* Helm: `global.statusMetricsPrometheusURL`

When the URL is empty or Prometheus is unreachable, per-node metrics are still
exported but cluster-manager **does not** patch the coarse CR summary.

Kind:

```bash
./contrib/kind.sh -esm
# optionally set OVN_STATUS_METRICS_PROMETHEUS_URL to a reachable Prometheus
```

## Metric families

Each gauge is `1` on success and `0` on failure. Labels are bounded (`node`,
`name`, and `namespace` for namespaced CRDs).

| CRD | Metric |
|-----|--------|
| EgressFirewall | `ovnkube_egressfirewall_sync_succeeded` |
| AdminNetworkPolicy | `ovnkube_adminnetworkpolicy_sync_succeeded` |
| BaselineAdminNetworkPolicy | `ovnkube_baselineadminnetworkpolicy_sync_succeeded` |
| EgressQoS | `ovnkube_egressqos_sync_succeeded` |
| NetworkQoS | `ovnkube_networkqos_sync_succeeded` |
| AdminPolicyBasedExternalRoute | `ovnkube_adminpolicybasedexternalroute_sync_succeeded` |

### Debug workflow (EgressFirewall example)

1. Read coarse summary: `kubectl get egressfirewall default -n prod -o jsonpath='{.status.status}'`
2. Find failing nodes:

   ```promql
   ovnkube_egressfirewall_sync_succeeded{namespace="prod", name="default"} == 0
   ```

3. Inspect logs on those nodes.

### Recording rule example

```promql
sum by (namespace, name) (ovnkube_egressfirewall_sync_succeeded == 0)
```

## Cluster-manager rollup

When metrics mode is on and Prometheus is available, cluster-manager queries
every ~30s and patches **only** the coarse summary when the aggregate changes
(`fieldManager: cluster-manager`):

* EgressFirewall / EgressQoS / NetworkQoS: `status.status`
* AdminPolicyBasedExternalRoute: `status.status` (`Success` / `Fail`)
* ANP / BANP: aggregate condition `Ready` (not `Ready-In-Zone-<node>`)

On failure, up to five failing node names (sorted) may appear in the summary
message where the status field is free-form text. Prometheus remains the source
of truth for the full node list. Deleted nodes are ignored via the live
relevant-node set even if stale series remain in Prometheus.

## Migration from per-node status

Before enabling metrics mode:

1. Confirm scrapes of `ovnkube-controller` metrics are working.
2. Update tooling that parsed `status.messages[]`, `Ready-In-Zone-*`, or
   node-named `managedFields` to use the metrics above (or the coarse summary).
3. Set `enableStatusMetrics: true` and, for kubectl/GitOps summaries, configure
   `statusMetricsPrometheusURL`.
4. On switchover, cluster-manager clears stale per-node status shards once.

Leave the flag off (default) to keep legacy behaviour.

## Scale validation

Scale validation (node churn × many CR instances, comparing patch rates and
apiserver/etcd CPU with the flag on vs off) is a manual / recorded benchmark;
see OKEP-6414. Thresholds are TBD.

## Grafana

A minimal panel query for failing EgressFirewalls:

```promql
sum by (namespace, name, node) (ovnkube_egressfirewall_sync_succeeded == 0)
```

Install alongside the existing SDN dashboard flow described in
[SDN Dashboard](sdn-dashboard.md).
