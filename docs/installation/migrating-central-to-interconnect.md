# Migrating From Central Mode to Interconnect Mode

This guide covers moving an existing OVN-Kubernetes deployment from the
deprecated central OVN control plane to single-node zone interconnect mode.

Use this page as migration guidance; exact apply/reconcile commands depend on
your installer (Helm, operator, GitOps, or a distribution-specific tool).

## Scope

Central mode uses shared OVN Northbound and Southbound databases, usually with
RAFT, a central `ovn-northd`, and node `ovn-controller` processes connected to
the central Southbound database.

Interconnect mode distributes OVN state by zone. The currently supported
topology is single-node zone: each node is its own zone, with local OVN
databases, `ovn-northd`, `ovnkube-controller`, and `ovn-controller`. The
cluster-wide control plane shrinks to `ovnkube-control-plane`, which runs
`ovnkube-cluster-manager` for global allocation driven by the Kubernetes API.

The Helm chart installs interconnect mode with `values-single-node-zone.yaml`.

## Plan The Cutover

This migration does not require draining nodes or restarting user workloads.
It briefly disrupts the OVN-K control plane during the cutover. We recommend
planning a maintenance window to make sure existing workloads are not
affected.

If a rollback is required: reapplying the saved central manifests restores
the chart but does not unwind OVS bridge state on nodes that briefly ran
interconnect; a node-by-node OVS restart may be needed.

Before changing the deployment mode:

* Save the current manifests, Helm values, and any installer or GitOps inputs
  that render central mode.
* If you need the central NBDB/SBDB for rollback or post-incident debugging,
  back them up before cutting over. Interconnect rebuilds OVN state from the
  Kubernetes API and does not consume the central databases.
* Confirm the Kubernetes API holds the desired network state. Any manual OVN
  database objects or out-of-band changes must become supported Kubernetes
  resources, or be recreated after the migration.
* Keep pod CIDRs, service CIDRs, gateway mode, encapsulation port, feature
  gates, and masquerade/join subnets consistent with the central deployment
  unless the migration explicitly changes them.
* Check the transit subnets — `global.v4TransitSubnet` (default
  `100.88.0.0/16`) and `global.v6TransitSubnet` (default `fd97::/64`) — do
  not overlap your pod, service, or join CIDRs. Interconnect uses them to
  route between per-node zones; override if there's a conflict.

## Check Site-Specific Dependencies

Review anything coupled to the central database:

* Monitoring and alerts: drop checks for central NBDB/SBDB RAFT and the
  central `ovn-northd`; add checks for the per-node `nb-ovsdb`, `sb-ovsdb`,
  and `ovn-northd` containers in the `ovnkube-node` DaemonSet.
* Log collection: pick up logs from the new `ovnkube-node` containers.
* Certificates: if TLS was used only for cross-node OVN database traffic,
  those client/server certificates are no longer needed in single-node zone
  mode. Leave the issuing CA or PKI hierarchy in place until you've audited
  every other consumer.
* Floating IP / VRRP on Layer 2 secondary networks: central mode propagated
  VIP-to-MAC mappings through the central Southbound database, so only the
  VIP-bearing nodes needed addresses on the L2 NAD subnet. Interconnect has
  no central SB, so failover must rely on flooded GARP — every node that
  needs to learn the moving IP/MAC needs an address on that subnet. Size
  IPAM to the cluster node count (with headroom) rather than the VIP count;
  a `/24` that fit central may need `/20` or larger.
* Tests and operational checks: retire central-DB failover tests; add
  interconnect readiness, pod and service traffic, and site-specific
  secondary network or hardware-offload tests.

## Migrate

1. Confirm there are no critical OVN-Kubernetes alerts and the Kubernetes API
   is healthy.
2. Identify the nodes hosting `ovnkube-db` pods — one node in non-HA
   (`Deployment`), N nodes in HA/RAFT (`StatefulSet`). Both chart variants
   render pods under the same selector:

   ```sh
   kubectl -n ovn-kubernetes get pod -l name=ovnkube-db -o wide
   ```

   Record the node names from the `NODE` column; step 4 cleans NBDB/SBDB
   files on each of them.

3. Scale the central control plane to zero and wait for pods to exit.
   `ovnkube-db` ships as a `Deployment` (non-HA) or `StatefulSet` (HA/RAFT);
   the snippet below detects and scales whichever exists:

   ```sh
   kubectl -n ovn-kubernetes scale deploy/ovnkube-master --replicas=0
   if kubectl -n ovn-kubernetes get deploy/ovnkube-db >/dev/null 2>&1; then
     kubectl -n ovn-kubernetes scale deploy/ovnkube-db --replicas=0
   else
     kubectl -n ovn-kubernetes scale statefulset/ovnkube-db --replicas=0
   fi
   kubectl -n ovn-kubernetes wait --for=delete pod -l name=ovnkube-db --timeout=2m
   kubectl -n ovn-kubernetes wait --for=delete pod -l name=ovnkube-master --timeout=2m
   ```

   For GitOps, do the equivalent scale-down or removal of `ovnkube-db`,
   `ovnkube-master`, and any central `ovn-northd` resource.
4. On each node from step 2, remove the stale central NBDB/SBDB files. Without
   this, the new per-node `nb-ovsdb`/`sb-ovsdb` would load central state from
   disk when interconnect starts.

   ```sh
   # on each node from step 2
   sudo rm -f /var/lib/openvswitch/ovnnb_db.db /var/lib/openvswitch/ovnsb_db.db
   ```

   Do not remove the OVS system database (`conf.db`) in the same directory.

   If scripting across many nodes is easier than targeting only the step 2
   set — or if earlier interconnect attempts may have left files behind —
   running the `rm -f` on every node is safe: the path is empty on nodes
   that never hosted central databases.
5. Switch from the central chart to the interconnect chart and reconcile. For
   the bundled Helm chart, render with `values-single-node-zone.yaml`. The
   resulting workloads are `Deployment/ovnkube-control-plane`,
   `DaemonSet/ovnkube-node` (running per-zone `nb-ovsdb`, `sb-ovsdb`,
   `ovn-northd`, `ovnkube-controller`, `ovn-controller`), and
   `DaemonSet/ovs-node`. Verify readiness in the next section.

## Verify

Confirm the target workloads are present and rolled out:

```sh
kubectl -n ovn-kubernetes rollout status deploy/ovnkube-control-plane --timeout=5m
kubectl -n ovn-kubernetes rollout status ds/ovnkube-node --timeout=5m
kubectl -n ovn-kubernetes rollout status ds/ovs-node --timeout=5m
```

Confirm each node has zone and transit annotations:

```sh
kubectl get nodes -o custom-columns='NAME:.metadata.name,ZONE:.metadata.annotations.k8s\.ovn\.org/zone-name,TRANSIT:.metadata.annotations.k8s\.ovn\.org/node-transit-switch-port-ifaddr'
```

Both columns should be non-empty on every node. `ZONE` is set by
`ovnkube-node`; `TRANSIT` is allocated by `ovnkube-cluster-manager` from the
transit subnet. An empty value means the responsible component isn't running
yet or the rollout hasn't finished.

Confirm local OVN databases respond on every `ovnkube-node` pod:

```sh
for p in $(kubectl -n ovn-kubernetes get pods -l app=ovnkube-node -o name); do
  kubectl -n ovn-kubernetes exec "${p}" -c nb-ovsdb -- ovn-nbctl --no-leader-only show >/dev/null
  kubectl -n ovn-kubernetes exec "${p}" -c sb-ovsdb -- ovn-sbctl --no-leader-only show >/dev/null
done
```

Run a workload smoke test that covers the paths the cluster depends on:

* pod-to-pod on the same node
* pod-to-pod across nodes
* ClusterIP service traffic
* NodePort or LoadBalancer traffic if used
* egress traffic
* network policy enforcement
* EgressIP, EgressFirewall, EgressQoS, AdminNetworkPolicy, multiple networks,
  UDNs, or other enabled features
* Layer 2 floating-IP failover and GARP learning if used
* hardware-offload or performance checks if used

For a simple pod and service check:

```sh
kubectl create ns ovn-ic-smoke
kubectl -n ovn-ic-smoke run web --image=registry.k8s.io/e2e-test-images/agnhost:2.45 --labels=app=web -- netexec --http-port=8080
kubectl -n ovn-ic-smoke wait --for=condition=Ready pod/web --timeout=2m
kubectl -n ovn-ic-smoke expose pod web --port=80 --target-port=8080
kubectl -n ovn-ic-smoke create job client --image=curlimages/curl:8.11.1 -- sh -c 'curl -sfm 5 http://web && curl -sfm 5 http://web.ovn-ic-smoke.svc.cluster.local'
kubectl -n ovn-ic-smoke wait --for=condition=Complete job/client --timeout=2m
kubectl delete ns ovn-ic-smoke
```

Use `spec.nodeName` or a node selector to pin smoke pods when you need to
force a same-node or cross-node path.

## Decommission Central Infrastructure

Once interconnect is stable and rollback is no longer required:

* Remove the central NBDB/SBDB workload (`Deployment/ovnkube-db` in non-HA or
  `StatefulSet/ovnkube-db` in HA/RAFT) along with any external database hosts.
* Remove the central `ovn-northd` and `ovnkube-master` resources.
* Remove unused OVN database certificates, secrets, and PKI roles.
* Remove central-database dashboards, alerts, playbooks, tests, and runbooks.

