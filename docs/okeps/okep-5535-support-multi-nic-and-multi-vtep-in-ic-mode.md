# OKEP-5535: Support Multi-NIC and Multi-VTEP in OVN-Kubernetes IC Mode

* Issue: [#5535](https://github.com/ovn-org/ovn-kubernetes/issues/5535)

## Problem Statement

OVN-Kubernetes Interconnect (IC) mode currently cannot reliably support multi-VTEP setups.
Nodes with multiple encapsulation endpoints cannot consistently select the correct VTEP per pod/network,
so traffic may use the wrong tunnel endpoint. This breaks hardware-offload-oriented setups and can
cause incorrect routing behavior.

## Goals

* Support multiple VTEPs in IC mode for nodes with multiple SR-IOV NICs
* Ensure correct overlay traffic encapsulation per pod interface
* Use the existing SR-IOV PF-mapping based behavior (`external_ids:ovn-pf-encap-ip-mapping`)
  to determine a VF's encap IP
* Only publish `encap_ip` in a pod's network annotation when its resolved VF encap IP differs from
  the node's default encap IP (`external_ids:ovn-encap-ip-default`); nodes with no SR-IOV NIC or
  only a single SR-IOV NIC, and clusters where no node has multiple SR-IOV NICs, are entirely
  unaffected.

## Non-Goals

* Add non-GENEVE tunnel type support as part of this OKEP
* Per-network (NAD/UDN) VTEP selection: `encap_ip` is derived purely from the VF's owning PF, so
  UDN/NAD-level VTEP selection is not in scope of this design
* Support multi-VTEP encap-IP resolution for SR-IOV resource pools containing VFs from
  multiple PFs

## Introduction

This OKEP targets multi-SR-IOV-NIC deployments in IC mode, where a node has multiple encapsulation
endpoints. Because VFs are tied to specific PFs, hardware offload requires their traffic to be
steered through the VTEP of the correct PF.

For example, assume a node has 3 SR-IOV NICs with PFs `enp1s0f0`, `enp2s0f0`, and `enp3s0f0`.
Each PF has an associated bridge interface (`brenp*`) with an IP address used as encap IP:

```bash
$ ip -br addr
brenp1s0f0           UNKNOWN        10.0.0.2/16
brenp2s0f0           UNKNOWN        10.0.0.3/16
brenp3s0f0           UNKNOWN        10.0.0.4/16

$ ovs-vsctl get open . external_ids:ovn-encap-ip
"10.0.0.2,10.0.0.3,10.0.0.4"

$ ovs-vsctl get open . external_ids:ovn-encap-ip-default
"10.0.0.2"

$ ovs-vsctl get open . external_ids:ovn-pf-encap-ip-mapping
"enp1s0f0:10.0.0.2,enp2s0f0:10.0.0.3,enp3s0f0:10.0.0.4"
```

`external_ids:ovn-encap-ip-default` designates the default encap IP.
`external_ids:ovn-pf-encap-ip-mapping` stores the PF-to-encap-IP mapping.

When a pod's VIF is backed by a VF, its encap IP is derived from the entry for the PF that owns
the VF (see the
[Multi-VTEP Documentation](https://ovn-kubernetes.io/features/multiple-networks/multi-vtep) for
background).

In the current **IC mode** implementation, multi-VTEP steering does not work correctly:

- **Layer 3 topology**: Each Transit Switch has only a **single remote LSP per remote node**,
  representing just one of the node's VTEPs. A static route directs all traffic destined for the
  remote node's subnet to this single LSP. As a result, **all traffic goes through one VTEP**,
  regardless of which VTEP should have been used.

- **Layer 2 topology**: Each remote VIF has its own remote LSP, but because IC mode doesn't have a
  centralized Southbound database, nodes cannot see the `encap_ip` values of remote VIFs.

To address these limitations, the VF's encap IP is published when required in the pod's
`k8s.ovn.org/pod-networks` annotation. The local `ovnkube-controller` resolves it via
`external_ids:ovn-pf-encap-ip-mapping` once it observes the pod locally (see
[Implementation Details](#implementation-details) for how it locates the VF, and how the timing
of the annotation update differs between topologies).

`encap_ip` is only needed, and only added, for pods whose resolved VF encap IP differs from the
node's default encap IP (`external_ids:ovn-encap-ip-default`) -- remote nodes already route to the
default encap IP for any VIF without its own `encap_ip`, so publishing a value that matches it
would be redundant. The resulting publication behavior depends on the node's SR-IOV NIC count:
- A node with **no SR-IOV NIC** doesn't have `external_ids:ovn-pf-encap-ip-mapping` set at all, so
  resolution is a cheap no-op and pods keep the current behavior, with no `encap_ip` field in
  their network annotation.
- A node with a **single SR-IOV NIC** is a single-VTEP node: `external_ids:ovn-pf-encap-ip-mapping`
  has one PF entry, which is treated as the node's default encap IP. Its VF-backed pods therefore
  don't need `encap_ip` in their network annotation.
- A node with **multiple SR-IOV NICs** is a true multi-VTEP node: `external_ids:ovn-pf-encap-ip-mapping`
  has multiple PF entries. A VF-backed pod needs `encap_ip` set only when its VF's PF maps to a
  non-default encap IP; pods whose VF happens to map to the default encap IP don't need it either.

Consequently, if no node in a cluster has multiple SR-IOV NICs, this feature is fully inactive
cluster-wide.

## User-Stories/Use-Cases

* Story 1: Hardware Offload with Multi-SR-IOV-NIC and Multi-VTEP in IC Mode

  As a network administrator, I want each pod’s traffic to be encapsulated using the VTEP IP of the
  PF that owns its associated VF, so that hardware offload works reliably and efficiently in
  multi-NIC environments.

## Proposed Solution

### API Details

No new CRD schema is introduced by this OKEP.

This design reuses the existing VTEP CR in `Unmanaged` mode (introduced by OKEP-5088: EVPN Support)
as an optional input to group a node's encap IPs by name, which is used to allocate a stable tunnel
ID per encap IP.

The API-level behavior added by this OKEP is annotation-based:
- `k8s.ovn.org/vteps` on Node (per-node discovered VTEPs and allocated tunnel IDs, consumed for
  Transit Switch port tunnel-ID allocation)

A VF's `encap_ip` is always determined via the existing SR-IOV PF-mapping mechanism
(`external_ids:ovn-pf-encap-ip-mapping`); unmanaged VTEPs and `k8s.ovn.org/vteps` are only used for
tunnel-ID allocation and do not affect encap IP selection.

### Implementation Details

In the current OVN-Kubernetes implementation, each node exposes its encap IPs via the
`k8s.ovn.org/node-encap-ips` annotation and its L3 network subnets via `k8s.ovn.org/node-subnets`.
For example, a multi-VTEP node `node-a` might have:

```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'
k8s.ovn.org/node-subnets: '{
  "default":["10.1.7.0/24"],
  "net-red":["10.2.13.0/24"],
  "net-blue":["10.3.17.0/24"]}'
```

The PF-to-encap-IP mapping in OVS DB is:
```bash
$ ovs-vsctl get open . external_ids:ovn-encap-ip-default
"10.0.0.2"

$ ovs-vsctl get open . external_ids:ovn-pf-encap-ip-mapping
"enp1s0f0:10.0.0.2,enp2s0f0:10.0.0.3,enp3s0f0:10.0.0.4"
```

`10.0.0.2` (PF `enp1s0f0`) is node-a's default encap IP.

#### Allocating Tunnel IDs for Additional VTEPs

For multi-VTEP nodes, administrators need to define unmanaged VTEP objects that group node encap IP ranges by VTEP name.
For example, for `node-a` with `k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'`:

```yaml
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep0
spec:
  mode: Unmanaged
  cidrs:
    - "10.0.0.2/32"
---
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep1
spec:
  mode: Unmanaged
  cidrs:
    - "10.0.0.3/32"
---
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: vtep2
spec:
  mode: Unmanaged
  cidrs:
    - "10.0.0.4/32"
```

`ovnkube-cluster-manager` matches the node encap IPs against the unmanaged VTEP CIDRs and writes
`k8s.ovn.org/vteps`. The first entry of `k8s.ovn.org/node-encap-ips` is the node's default encap IP
and takes `k8s.ovn.org/node-id` as its tunnel ID; the remaining encap IPs each get a newly allocated
tunnel ID.

This requires a change to how `k8s.ovn.org/node-encap-ips` is written. Today `ovnkube-node` builds
the value from a set and emits it sorted, so the order configured via `--encap-ip` is lost. The
annotation must instead preserve the configured order, so that its first entry is always the node's
default encap IP -- the same address `ovnkube-node` sets as `external_ids:ovn-encap-ip-default`.

After these VTEP objects are defined and processed, `node-a` would carry:

```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'
k8s.ovn.org/vteps: '{"vtep0":{"ips":["10.0.0.2"],"tunnel-id":6},"vtep1":{"ips":["10.0.0.3"],"tunnel-id":7},"vtep2":{"ips":["10.0.0.4"],"tunnel-id":8}}'
```

#### Publishing Non-Default VIF Encap IPs

In IC mode, a VIF's encap IP is not visible to remote nodes when it differs from the node's
default encap IP, so the pod needs to expose it in the `k8s.ovn.org/pod-networks` annotation.

When a pod's VIF is backed by a VF, `ovnkube-controller` resolves `encap_ip` from the node's
`external_ids:ovn-pf-encap-ip-mapping`. To find the VF for a pod network attachment, it reads the
NAD's `k8s.v1.cni.cncf.io/resourceName` annotation. If the annotation identifies an SR-IOV VF
pool, it queries the kubelet PodResources API for the first device allocated from that resource.
It then maps the VF's PCI address to its owning PF's uplink representor and looks up the
corresponding encap IP.

For example, suppose Pod-a is attached to two NADs that both specify the SR-IOV resource
`nvidia.com/asap2_vf`. The pod receives two VFs from that resource pool, one per attachment.
When resolving `encap_ip` for either attachment, `ovnkube-controller` calls the kubelet
PodResources `List` gRPC API and gets:

```json
{
  "name": "pod-a",
  "namespace": "default",
  "containers": [
    {
      "name": "pod-a",
      "devices": [
        {
          "resource_name": "nvidia.com/asap2_vf",
          "device_ids": ["0000:04:01.0"]
        },
        {
          "resource_name": "nvidia.com/asap2_vf",
          "device_ids": ["0000:04:01.2"]
        }
      ]
    }
  ]
}
```

This response shows that the pod has two devices from `nvidia.com/asap2_vf`, but it does not
identify which device belongs to which network attachment.

When `ovnkube-controller` starts processing a pod after it is scheduled on the local node, the
kubelet may not yet have allocated the pod's VF devices. In that case, the controller retries the
PodResources API call until the allocation is available. For the default network and Layer 3
networks, this can delay the pod network annotation, CNI ADD, and therefore pod startup. For Layer
2 networks, `ovnkube-cluster-manager` has already written the IP annotation, so the delay is
limited to publishing `encap_ip` and programming remote connectivity.

`ovnkube-controller` therefore filters by matching `resource_name` and uses the first
`device_ids` entry -- here, `0000:04:01.0` -- to resolve `encap_ip` for **both** attachments. See
[Risks, Known Limitations and Mitigations](#risks-known-limitations-and-mitigations) for the
limitation this "first device" heuristic introduces.

When it writes `encap_ip` into the pod's network annotation depends on the network's topology:

- **Default network and Layer 3 networks**: `ovnkube-controller` itself allocates the pod's IP and
  writes the pod's network annotation when handling the pod's create/update event, so it resolves
  and sets `encap_ip` as part of that same write -- no extra Pod patch is needed.
- **Layer 2 networks**: `ovnkube-cluster-manager` allocates the pod's IP and writes the pod's network
  annotation instead, since Layer 2 IPAM is cluster-wide. `ovnkube-controller` only reads that
  annotation when handling the pod's create/update event, and cannot resolve `encap_ip` from
  the control plane. Once `ovnkube-controller`, running on the pod's node, observes that the pod
  is local and its network annotation (with its IP) already exists, it resolves `encap_ip` as
  described above and patches it into the annotation in a separate Pod patch request.


An alternative is to resolve `encap_ip` during CNI ADD CMD. However, CNI ADD runs after pod IP
allocation and must issue a separate Pod patch to publish `encap_ip`. This increases Kubernetes API
server churn and adds implementation complexity for Layer 3 networks. See
[Resolve `encap_ip` During CNI ADD CMD](#resolve-encap_ip-during-cni-add-cmd).


Assuming networks defined by NADs with sriov resource are mapped to PFs as below:

| Network  | Cluster Subnet | PF       | Encap IP |
| -------- | -------------- | -------- | -------- |
| default  | 10.1.0.0/16/24 | enp1s0f0 | 10.0.0.2 |
| net-red  | 10.2.0.0/16/24 | enp2s0f0 | 10.0.0.3 |
| net-blue | 10.3.0.0/16/24 | enp3s0f0 | 10.0.0.4 |

Based on the network-to-PF mapping above, Pod-a on node-a would have:

```yaml
k8s.ovn.org/pod-networks: '{
  "default": {
    "ip_addresses": ["10.1.7.35/24"],
    ...
  },
  "default/net-red": {
    "ip_addresses": ["10.2.13.4/24"],
    "encap_ip": "10.0.0.3",
    ...
  },
  "default/net-blue": {
    "ip_addresses": ["10.3.17.5/24"],
    "encap_ip": "10.0.0.4",
    ...
  }
}'
```

Note that `default` has no `encap_ip`: its VF's PF (`enp1s0f0`) resolves to `10.0.0.2`, which is
node-a's default encap IP (`external_ids:ovn-encap-ip-default`), so publishing it would be
redundant -- remote nodes already route to `10.0.0.2` by default. `net-red` and `net-blue` do get
`encap_ip` set, since their PFs (`enp2s0f0`, `enp3s0f0`) resolve to non-default encap IPs.

#### Layer 2 topology solution

For Layer 2 networks, each remote pod's VIF is represented by a remote LSP in the
`layer2_ovn_layer2_switch`. To enable correct encapsulation, the local `ovnkube-controller` sets
`options:requested-encap-ip` on the remote LSP based on `encap_ip` in the remote pod's network
annotation. If the annotation does not contain `encap_ip`, the option is omitted and OVN uses the 
remote node's default encap IP.

Note that `requested-encap-ip` is not set on local LSPs.

For example, assume `net-blue` is a Layer 2 network and Pod-a is scheduled on node-a.
When `ovnkube-controller` on node-b processes this remote pod, it reads the `encap_ip` (`10.0.0.4`)
from Pod-a's `k8s.ovn.org/pod-networks` annotation and sets `requested-encap-ip` on the remote LSP:

```yaml
 switch f99af2b3-8586-460b-940d-98c8adc3b2d2 (ovn.blue.layer2_ovn_layer2_switch)
    port default.ovn.blue_test_pod-a
        type: remote
        addresses: ["0a:58:0a:03:11:05 10.3.17.5"]
        options: {requested-encap-ip="10.0.0.4", ...}
```

Then, `ovn-northd` uses this option to set the Port_Binding's `encap` field to the matching Encap
entry for node-a:

```bash
$ ovn-sbctl show
Chassis node-a
    hostname: node-a
    Encap geneve
        ip: "10.0.0.2"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.3"
        options: {csum="true"}
    Encap geneve
        ip: "10.0.0.4"
        options: {csum="true"}
    Port_Binding default.ovn.blue_test_pod-a
...

$ ovn-sbctl list Port_Binding default.ovn.blue_test_pod-a
_uuid               : 04fd102a-6679-4920-ad6c-663c52df4161
chassis             : a02a2083-6bd9-47f6-ab34-730322756c9e
datapath            : 6413cf4a-838f-481f-b4b9-1ac9ab467cb9
encap               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
logical_port        : default.ovn.blue_test_pod-a
tunnel_key          : 11
...

$ ovn-sbctl list encap 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
_uuid               : 4a64dec0-b51c-4e7b-bf0f-629cce23bf42
chassis_name        : node-a
ip                  : "10.0.0.4"
options             : {csum="true"}
type                : geneve
...
```

With the `encap` field set, node-b's `ovn-controller` knows to use the tunnel with
`remote_ip=10.0.0.4` when sending traffic to Pod-a on `net-blue`.

#### Layer 3 topology solution

In IC mode, Layer 3 networks use Transit Switches to forward traffic between nodes. Each remote
node is represented by a remote LSP in the network's Transit Switch.

For example, on node-b, the following remote LSPs represent node-a on three networks:

```
switch 8d303553-017f-45e4-8fb4-1fe549875eb1 (transit_switch)
   port tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]

switch ef9a91a3-85ec-476f-972c-de4de105ff3c (net.red_transit_switch)
   port net.red_tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]

switch a50d8fc0-b0fd-49b8-b5d0-cd718860bf9d (net.blue_transit_switch)
   port net.blue_tstor-node-a
        type: remote
        addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]
```

Currently, each Transit Switch has only one remote LSP per node, even if that node has multiple
VTEPs. Additionally, these remote LSPs do not have the `encap` field set in their Port_Binding
entries. This means all traffic to a remote node goes through a single VTEP, breaking multi-VTEP
functionality.

##### Supporting Multiple Transit Ports Per Encap IP

Currently, a remote Transit Switch port uses the node's `k8s.ovn.org/node-id` as its tunnel key.
This limits each node to one Transit Switch port and one VTEP.

To support multiple VTEPs, each multi-VTEP node needs a `k8s.ovn.org/vteps` annotation that maps
its encap IPs to tunnel IDs. `ovnkube-cluster-manager` is the sole owner of this annotation. It
matches the IPs in `k8s.ovn.org/node-encap-ips` against unmanaged VTEP CIDRs and writes both `ips`
and `tunnel-id` in a single update.

The first entry in `k8s.ovn.org/node-encap-ips` must be the default encap IP. Its VTEP uses
`k8s.ovn.org/node-id` as the tunnel ID. `ovnkube-cluster-manager` allocates a new tunnel ID from
the node ID space for each additional encap IP.

For example, node-a could have the following annotations:
```yaml
k8s.ovn.org/node-id: "6"
k8s.ovn.org/node-encap-ips: '["10.0.0.2","10.0.0.3","10.0.0.4"]'
k8s.ovn.org/vteps: '{
  "vtep0":{"ips":["10.0.0.2"],"tunnel-id":6},
  "vtep1":{"ips":["10.0.0.3"],"tunnel-id":7},
  "vtep2":{"ips":["10.0.0.4"],"tunnel-id":8}}'
```

This results in one Transit Switch port per encap IP:

| LSP                   | Encap IP   | Tunnel Key  | TSP Address      |
|-----------------------|------------|-------------|------------------|
| tstor-node-a          | 10.0.0.2   | 6           | 100.88.0.6       |
| tstor-node-a_tun7     | 10.0.0.3   | 7           | 100.88.0.7       |
| tstor-node-a_tun8     | 10.0.0.4   | 8           | 100.88.0.8       |

For single-VTEP nodes, the existing Transit Switch port name `tstor-<node-name>` is unchanged. For
multi-VTEP nodes, each port name is based on its tunnel ID:
- If the tunnel ID equals `k8s.ovn.org/node-id`, use `tstor-<node-name>`.
- Otherwise, use `tstor-<node-name>_tun<tunnel-id>`.

##### Transit Switch Ports for Local Multi-VTEP Nodes

For single-VTEP nodes, the existing behavior remains unchanged.

For multi-VTEP nodes, the default `router` Transit Switch port is always created during node
initialization, as in the single-VTEP case. It uses `k8s.ovn.org/node-id` as its tunnel ID and the
node's default encap IP, whose `k8s.ovn.org/vteps` entry has the same tunnel ID. This provides
connectivity as soon as the node joins the cluster, even if no pod uses the default encap IP.

When a local pod uses a non-default encap IP, `ovnkube-controller` creates an additional `router`
Transit Switch port for that encap IP on demand and sets `options:requested-encap-ip` on the LSP.
`ovn-northd` uses this option to set the corresponding `Port_Binding.encap`.

For example, the cluster router and Transit Switch for node-a's default network have the default
port (tunnel 6, encap IP `10.0.0.2`) as soon as node-a joins the cluster. If a pod on node-a then
uses the non-default encap IP `10.0.0.3`, the ports for tunnel 7 are created on demand:

```
router 91efa770-d146-428e-bae0-5a394dbbcdc8 (ovn_cluster_router)
    port rtots-node-a
        mac: "0a:58:64:58:00:06"
        ipv6-lla: "fe80::858:64ff:fe58:6"
        networks: ["100.88.0.6/16"]
    port rtots-node-a_tun7
        mac: "0a:58:64:58:00:07"
        ipv6-lla: "fe80::858:64ff:fe58:7"
        networks: ["100.88.0.7/16"]

switch fe6b6dec-b8a3-4199-b6f6-f64d02389f28 (transit_switch)
    port tstor-node-a
        type: router
        router-port: rtots-node-a
        options: {requested-encap-ip="10.0.0.2"}
    port tstor-node-a_tun7
        type: router
        router-port: rtots-node-a_tun7
        options: {requested-encap-ip="10.0.0.3"}
```

`rtots-node-a` and `tstor-node-a` exist as soon as node-a joins the cluster.
`rtots-node-a_tun7` and `tstor-node-a_tun7` are created only after a pod uses the non-default encap
IP `10.0.0.3`. Ports for tunnel 8 are created in the same way when a pod uses encap IP `10.0.0.4`.

##### Transit Switch Ports for Remote Multi-VTEP Nodes

For single-VTEP nodes, the existing behavior remains unchanged.

For multi-VTEP remote nodes, one `remote` Transit Switch port -- using `k8s.ovn.org/node-id` as its
tunnel ID and the node's default encap IP (as defined above) -- is always created as soon as the
remote node joins the cluster, together with a **static route** to the remote node's subnet
using this default port's address as the nexthop. The port has `options:requested-encap-ip` set to
the default encap IP. This does not depend on which encap IP the first pod on the remote node uses.

If a pod on the remote node uses a non-default encap IP, `ovnkube-controller` creates an additional
`remote` Transit Switch port for that encap IP on demand (if one does not already exist), sets
`options:requested-encap-ip` on the port, and adds a **`/32` static route** for the pod IP using the
new port's address as the nexthop. This `/32` route takes precedence over the default node subnet route
through longest-prefix match.

- **As soon as `node-a` (`k8s.ovn.org/node-id: "6"`, default encap IP `10.0.0.2`) joins the
  cluster**, `node-b` creates the always-present default port and route:
  ```
  switch 514642eb-9ee5-4026-830c-153923417892 (transit_switch)
      port tstor-node-a
          type: remote
          addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]
          options: {requested-encap-ip="10.0.0.2"}
  ```
  ```
  IPv4 Routes
  Route Table <main>:
        10.1.7.0/24         100.88.0.6 dst-ip
  ```
- **If a Pod (`10.1.7.5`) is later scheduled on `node-a` with `encap_ip: 10.0.0.3`** (non-default,
  tunnel 7), `node-b` creates a second remote Transit Switch port and a `/32` route for that pod:
  ```
  switch 514642eb-9ee5-4026-830c-153923417892 (transit_switch)
      port tstor-node-a
          type: remote
          addresses: ["0a:58:64:58:00:06 100.88.0.6/16"]
          options: {requested-encap-ip="10.0.0.2"}
      port tstor-node-a_tun7
          type: remote
          addresses: ["0a:58:64:58:00:07 100.88.0.7/16"]
          options: {requested-encap-ip="10.0.0.3"}
  ```
  ```
  IPv4 Routes
  Route Table <main>:
        10.1.7.5/32         100.88.0.7 dst-ip
        10.1.7.0/24         100.88.0.6 dst-ip
  ```
- **If more Pods on `node-a` use the default encap IP `10.0.0.2`**, no new port or route is needed
  -- they're already covered by the always-present default port/route.
- **If more Pods on `node-a` use the same non-default encap IP `10.0.0.3`**, only a new `/32` route
  is added per pod (pointing to the already-created `tstor-node-a_tun7`); no new port is needed.
- **If a Pod on `node-a` uses a different non-default encap IP** (e.g. `10.0.0.4`, tunnel 8), a
  third remote Transit Switch port `tstor-node-a_tun8` is created on demand, along with a `/32`
  route for that pod.

### Testing Details

* Unit Testing details
  - Verify that Pod annotations include the correct `encap_ip` values for VF-backed pods, resolved
    by `ovnkube-controller` from the node's `external_ids:ovn-pf-encap-ip-mapping` when handling
    the pod's create/update event.
  - Verify that a remote Layer 2 Pod's logical switch port has `options:requested-encap-ip` set from
    the Pod's `encap_ip`.
  - Verify that the appropriate static routes are added to the OVN Cluster Router:
    - A node subnet route through the default Transit Switch port.
    - A `/32` route for each remote Pod using a non-default encap IP.

* E2E Testing details
  Multi-VTEP relies on the PF-to-encap-IP mapping, while `kind` uses veth interfaces and cannot
  exercise this code path. Validating hardware offload requires a real SR-IOV multi-NIC setup,
  which is not available in the upstream CI/CD pipeline. This feature will therefore be tested in
  downstream environments with the required hardware.

## Risks, Known Limitations and Mitigations

* **PodResources API has no per-network visibility.** As described in
  [Implementation Details](#implementation-details), the kubelet PodResources API only reports
  the devices allocated to a pod grouped by resource name; it doesn't indicate which network
  attachment (NAD) each individual device was allocated for. When resolving `encap_ip` for a
  given attachment, `ovnkube-controller` therefore always uses the *first* device ID returned for
  the attachment's `resourceName`.

  This introduces a limitation: **all VFs in a resource pool exposed under a single
  `resourceName` must belong to the same PF**. If a resource pool aggregates VFs from multiple
  PFs (e.g. one SR-IOV device plugin pool spanning two different NICs), a pod using multiple
  devices from that pool for different network attachments could have `encap_ip` resolved from
  the wrong VF/PF for some of those attachments, since the "first device" heuristic cannot tell
  them apart.

  * *Mitigation*: cluster/hardware administrators deploying multi-VTEP with SR-IOV must configure
    one SR-IOV device plugin resource pool (`resourceName`) per PF/NIC, not a pool that spans
    multiple PFs. Under this constraint, any device picked from the pool always maps to the same,
    correct encap IP.

* **Per-pod `/32` routes may not scale for large non-default-VTEP workloads.** A `/32` static route
  is created for each remote pod that uses a non-default encap IP, so the route count grows with
  the number of such pods. This can increase NBDB size and controller reconciliation cost.

  * *Mitigation*: pods using the default encap IP remain covered by the existing node-subnet route,
    and pods sharing a non-default encap IP reuse the same Transit Switch port. This design targets
    deployments where the number of multi-VTEP nodes and workloads using non-default encap IPs is
    limited.

## Backwards Compatibility

* `encap_ip` is an additive, `omitempty` field in the `k8s.ovn.org/pod-networks` annotation; pods
  without it (existing pods, or new pods on nodes/clusters where this feature doesn't apply)
  behave exactly as before, and no consumer of the annotation is required to understand the field
  unless multi-VTEP is actually in use.
* `encap_ip` is only ever resolved and added for pods whose resolved VF encap IP differs from the
  node's default encap IP (`external_ids:ovn-encap-ip-default`); see [Introduction](#introduction)
  for the full breakdown by SR-IOV NIC count. Nodes without any SR-IOV NIC, and single-SR-IOV-NIC
  nodes (whose one PF is necessarily the default), never hit this case, so pods on them keep the
  current behavior unchanged, with no `encap_ip` field added. Only nodes with multiple SR-IOV NICs
  (true multi-VTEP) can have VF-backed pods whose encap IP differs from the default, and only
  those exercise the new Transit Switch port/route behavior below.
* Consequently, upgrading a cluster where no node has multiple SR-IOV NICs is fully
  backwards-compatible and does not change any observable pod annotation, OVN logical topology,
  or routing behavior: `k8s.ovn.org/vteps` is never populated, no additional Transit Switch ports
  or static routes described in
  [Supporting Multiple Transit Ports Per Encap IP](#supporting-multiple-transit-ports-per-encap-ip)
  are created, and existing single-VTEP Transit Switch port naming (`tstor-<node-name>`) is
  unchanged.
* This design reuses the existing VTEP CRD (in `Unmanaged` mode, from OKEP-5088) without any
  schema changes, so no CRD migration is required.

## Alternatives

### Resolve `encap_ip` During CNI ADD CMD

An alternative is to resolve `encap_ip` in CNI ADD CMD, after the SR-IOV device plugin has allocated
a VF to the pod. The CNI obtains the VF's PCI device ID from the CNI request, maps it to the owning
PF's uplink representor, and looks up the corresponding encap IP in
`external_ids:ovn-pf-encap-ip-mapping`. It configures the OVS interface with that encap IP and
patches the pod's `k8s.ovn.org/pod-networks` annotation to publish `encap_ip` for remote nodes.

Because the CNI runs after the pod network annotation is allocated, publishing `encap_ip` requires
an additional Pod patch request for every applicable CNI ADD. At scale, these extra API requests
can increase Kubernetes API server load and create additional annotation-update churn.

### Use a Dedicated Encap-IP-to-Tunnel-ID Annotation

The current VTEP-based approach does not use the VTEP name. Transit Switch ports are named based
on their tunnel IDs, and pods specify `encap_ip` rather than a VTEP name. Requiring a VTEP CR
therefore adds configuration and controller dependencies without providing a specific benefit for
multi-VTEP. The existing VTEP controllers are also tied to EVPN, while multi-VTEP uses GENEVE and
must work independently of EVPN.

An alternative is to replace the VTEP CR and `k8s.ovn.org/vteps` annotation with a dedicated
`k8s.ovn.org/node-encap-ip-configs` annotation. Each entry contains an encap IP and its tunnel ID:

```yaml
k8s.ovn.org/node-encap-ip-configs: '[
  {"encap-ip":"10.0.0.2","tunnel-id":6},
  {"encap-ip":"10.0.0.3","tunnel-id":7},
  {"encap-ip":"10.0.0.4","tunnel-id":8}]'
```

`ovnkube-cluster-manager` adds this annotation to the node using `k8s.ovn.org/node-encap-ips` and
`k8s.ovn.org/node-id`. The default encap IP uses the node ID, and each additional encap IP receives
a newly allocated ID from the node ID space. The new annotation preserves the encap IP order from
`k8s.ovn.org/node-encap-ips` and can be extended with additional fields later.

The dedicated annotation avoids the VTEP CR and controller dependencies and has a single owner by
construction. The trade-off is introducing a new Node annotation instead of reusing the existing
VTEP API.

## References

Multi-VTEP Documentation: https://ovn-kubernetes.io/features/multiple-networks/multi-vtep