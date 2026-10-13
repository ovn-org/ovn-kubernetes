# Pod Creation Workflow

This document describes how a pod gets its network configured in
OVN-Kubernetes, from the moment it is scheduled to a node until it has
a working network interface with connectivity.

## Overview

Pod networking is a **two-phase, loosely coupled** process:

1. **Control plane (ovnkube-controller)** — watches the Pod API, allocates
   IPs, writes the pod annotation, and creates OVN NorthBound DB objects.
2. **Data plane (CNI on the node)** — the container runtime (via the CRI,
   often through Multus) invokes the CNI plugin, which waits for the
   annotation, plumbs the veth/OVS interface, and returns IPs to the
   runtime. The CNI plugin binary is a lightweight shim that forwards
   requests to the CNI server running inside the ovnkube-node container
   via a Unix socket.

   In the default (privileged) mode, the CNI server in ovnkube-node performs
   interface plumbing and returns a full CNI result to the shim. With
   `--unprivileged-mode`, ovnkube-node runs without `SYS_ADMIN`: the CNI
   server still waits for the annotation and returns the allocated IP/MAC
   details, but the shim itself creates the veth, configures the pod
   netns, and plugs the host end into OVS.

The controller must write the annotation **before** CNI ADD can succeed.
The CNI server polls for the annotation every 200 ms for up to 2 minutes.
See the [End-to-End Timeline](#end-to-end-timeline) below for the full sequence.

## Phase 1: Control Plane — Pod Reconciliation

### Event Handling

When a pod is created, the Kubernetes informer fires an Add event. If the
pod is not yet scheduled (`spec.nodeName` empty), logical port creation is
deferred. When the scheduler assigns a node, that Update event triggers
port setup. The controller ensures that transient failures are retried
with exponential backoff. Host-network pods are skipped (they do not need
an OVN logical port).

At controller startup, node reconciliation runs before pod watching so
each node's logical switch exists before pods are attached to it.
Services are started after nodes (and before pods) during that same
startup sequence to avoid load-balancer churn; individual pod adds do
not wait on Services.

The control-plane call sequence (`ensurePod` → `addLogicalPort` →
`addLogicalPortToNetwork`) is shown in the
[End-to-End Timeline](#end-to-end-timeline).

### IP Allocation (IPAM)

`allocatePodAnnotation()` runs the following allocation logic:

1. If the pod already has an annotation (e.g. after a restart), the
   existing IPs are reserved in IPAM to prevent duplicates.
2. If an existing LogicalSwitchPort is found in OVN NB, its addresses
   are recovered.
3. Otherwise, `assignPodAddresses()` allocates the next available IP(s)
   from the node's subnet via the LogicalSwitchManager:

```text
assignPodAddresses(switchName)
  → lsManager.AllocateNextIPs(switchName)
    → subnet.Allocator.AllocateNextIPs()
      → ip.Range.AllocateNext()
```

The MAC address is derived deterministically from the first allocated IP
using `util.IPAddrToHWAddr()`.

The allocated addresses are written to the pod as the
`k8s.ovn.org/pod-networks` annotation. Example of a pod on the default
network:

```yaml
annotations:
  k8s.ovn.org/pod-networks: |
    {
      "default": {
        "ip_addresses": ["10.244.0.3/24"],
        "mac_address": "0a:58:0a:f4:00:03",
        "gateway_ips": ["10.244.0.1"],
        "routes": [
          {"dest": "10.244.0.0/16", "nextHop": "10.244.0.1"},
          {"dest": "10.96.0.0/16", "nextHop": "10.244.0.1"},
          {"dest": "169.254.0.5/32", "nextHop": "10.244.0.1"},
          {"dest": "100.64.0.0/16", "nextHop": "10.244.0.1"}
        ],
        "ip_address": "10.244.0.3/24",
        "gateway_ip": "10.244.0.1",
        "role": "primary"
      }
    }
```

### OVN NorthBound DB Objects

`addLogicalPortToNetwork()` creates/updates the following in a single
OVSDB transaction:

* **LogicalSwitchPort** — Named `<namespace>_<pod-name>`, attached to the
  node's logical switch. Addresses set to `"<mac> <ip1> <ip2>"`. Port
  security enabled.
* **Namespace port group** — The LSP UUID is added to the namespace's port
  group (used by network policies).
* **External IDs** — `namespace`, `pod=true`.
* **Options** — pod UID in `iface-id-ver`, and `requested-chassis` (node
  chassis UUID) for binding.

```go
lsp := &nbdb.LogicalSwitchPort{
    Name:         "namespace_podname",
    Addresses:    []string{"0a:58:0a:f4:00:05 10.244.0.5"},
    PortSecurity: []string{"0a:58:0a:f4:00:05 10.244.0.5"},
    ExternalIDs: map[string]string{
        "namespace": namespace,
        "pod":       "true",
    },
    Options: map[string]string{
        "iface-id-ver":      string(pod.UID),
        "requested-chassis": chassisID, // from k8s.ovn.org/node-chassis-id
    },
}
```

Additional OVN objects may be created depending on features:

* **SNAT rules** — if multi-gateway with no external gateway annotation
* **Gateway routes** — if namespace has external/pod gateway annotations
* **DHCP options** — for live-migratable pods

> [!NOTE]
> NetworkPolicy ACLs are **not** created during pod add. They are managed
> separately by the network policy controller when a pod matches a policy
> selector — the pod's LSP is added to the policy's port group, which
> already has ACLs attached. If NetworkPolicy is enabled for a namespace,
> the pod is also placed into that namespace's default deny port group
> during pod configuration so a default deny is in effect when the pod
> comes up.

## Phase 2: Data Plane — CNI ADD

### CNI Shim

The CNI binary (`CmdAdd`) is invoked by the container runtime (for example
containerd or CRI-O), typically via Multus as the CNI meta-plugin — not
directly by kubelet. It acts as a thin shim that forwards the request over
a Unix domain socket to the CNI server running inside ovnkube-node. In
privileged mode it returns the CNI result from the server unchanged. In
unprivileged mode (`--unprivileged-mode`) the server returns only pod
interface info (no result), and the shim performs interface plumbing itself.

### CNI Server

The CNI server listens on a root-only Unix socket and dispatches ADD/DEL
requests. `cmdAdd()` performs the following steps:

1. Get pod from the Kubernetes API.
2. Wait for `k8s.ovn.org/pod-networks` annotation to appear (`isOvnReady`
   check). This is the synchronization point with the control plane.
3. Parse the annotation into IP/MAC/gateway configuration.
4. In privileged mode, call `ConfigureInterface()` and return the CNI
   result. In unprivileged mode, return the pod interface info so the
   shim can configure the interface.

### Interface Plumbing

`ConfigureInterface()` and `ConfigureOVS()` (run by the CNI server in
privileged mode, or by the CNI shim in unprivileged mode) perform the
following steps:

1. Create a veth pair — one end placed in the pod's network namespace
   (`eth0`), the other kept on the host.
2. Configure the pod namespace — assign IP addresses, set up default
   route via the gateway, configure any additional routes.
3. Add to OVS — `ConfigureOVS()` adds the host-side veth to `br-int`
   with external IDs. In privileged mode this is done via libovsdb (not
   `ovs-vsctl`); unprivileged mode falls back to `ovs-vsctl`. Either way
   the equivalent configuration is:

```bash
# Equivalent of the OVS port/interface configuration applied by ConfigureOVS():
ovs-vsctl add-port br-int abcd1234ef \
  -- set interface abcd1234ef \
       external_ids:iface-id="default_my-pod" \
       external_ids:iface-id-ver="pod-uid-here" \
       external_ids:sandbox="container-id" \
       external_ids:ip_addresses="10.244.0.5/24" \
       external_ids:attached_mac="0a:58:0a:f4:00:05"
```

4. Wait for OVN binding — poll OVS until `ovn-installed=true` is set
   on the interface by ovn-controller, confirming that OpenFlow rules are
   programmed and the port is active.
5. Return CNI result — interfaces, IPs, MAC, and gateway are returned to
   the container runtime through the CNI shim (routes were already applied
   in the pod netns in step 2). Example result for the default network:

```json
{
  "cniVersion": "1.1.0",
  "interfaces": [
    {
      "name": "12939f8d3d906f0"
    },
    {
      "name": "eth0",
      "mac": "0a:58:0a:f4:00:03",
      "sandbox": "/var/run/netns/..."
    }
  ],
  "ips": [
    {
      "interface": 1,
      "address": "10.244.0.3/24",
      "gateway": "10.244.0.1"
    }
  ]
}
```

Example of an OVS interface after binding (`ovn-installed=true`):

```bash
$ ovs-vsctl list interface 12939f8d3d906f0
name            : "12939f8d3d906f0"
type            : ""
ofport          : 5
mtu             : 1400
link_state      : up
status          : {driver_name=veth, driver_version="1.0"}
external_ids    : {
    attached_mac="0a:58:0a:f4:00:03",
    iface-id="default_workflow-test",
    iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
    ip_addresses="10.244.0.3/24",
    ovn-installed="true",
    ovn-installed-ts="1784626958272",
    pod-if-name="eth0",
    sandbox="12939f8d3d906f0675c2d7a8733a4c0fe8ec128ca5506f329ffb91987e544369"
}
```

Corresponding Southbound DB port binding:

```bash
$ ovn-sbctl list port_binding default_workflow-test
_uuid               : <uuid>
chassis             : 789baf7d-f678-42aa-a0e4-b85427878d64
logical_port        : "default_workflow-test"
mac                 : ["0a:58:0a:f4:00:03 10.244.0.3"]
external_ids        : {namespace=default, pod="true"}
options             : {iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
                       requested-chassis="636c9bc8-453a-4c6b-9404-201d034700dc"}
port_security       : ["0a:58:0a:f4:00:03 10.244.0.3"]
requested_chassis   : 789baf7d-f678-42aa-a0e4-b85427878d64
tunnel_key          : 4
type                : ""
up                  : true
```

Note `up: true`, and that `logical_port` / `iface-id` and `iface-id-ver` match
across OVS and SBDB.

The Northbound DB LSP reflects the same `up: true` from SBDB:

```bash
$ ovn-nbctl list logical_switch_port default_workflow-test
_uuid               : <uuid>
addresses           : ["0a:58:0a:f4:00:03 10.244.0.3"]
enabled             : []
external_ids        : {namespace=default, pod="true"}
name                : "default_workflow-test"
options             : {iface-id-ver="69611cd1-cfcd-414f-afea-5669dc947fb1",
                       requested-chassis="636c9bc8-453a-4c6b-9404-201d034700dc"}
port_security       : ["0a:58:0a:f4:00:03 10.244.0.3"]
up                  : true
```

## End-to-End Timeline

```mermaid
flowchart TB
    subgraph ControlPlane["Phase 1: Control Plane (ovnkube-controller)"]
        direction TB
        A["Pod Add/Update (informer; after scheduled)"]
        B["ensurePod()"]
        C["ensureLocalZonePod()"]
        D["addLogicalPort()"]
        E["addLogicalPortToNetwork()"]
        F["allocatePodAnnotation() — IPAM"]
        G["assignPodAddresses() → AllocateNextIPs()"]
        H["IPAddrToHWAddr() — derive MAC from IP"]
        I["updatePodAnnotationWithRetry()"]
        J["Patch pod: k8s.ovn.org/pod-networks (IPs, MAC, gateways, routes)"]
        K["Create LogicalSwitchPort in OVN NBDB"]
        L["Set Addresses + PortSecurity on LSP"]
        M["Add LSP UUID to namespace port group"]
        N["TransactAndCheckAndSetUUIDs()"]
        A --> B --> C --> D --> E --> F --> G --> H --> I --> J --> K --> L --> M --> N
    end

    subgraph OVNStack["OVN Pipeline (asynchronous)"]
        direction TB
        O["NBDB change notification"]
        P["ovn-northd compiles logical flows"]
        Q["Logical flows written to SouthBound DB"]
        R["SBDB change notification"]
        S["ovn-controller translates to OpenFlow"]
        T["OpenFlow rules installed on br-int"]
        U["Set external_ids:ovn-installed=true on OVS port"]
        O --> P --> Q --> R --> S --> T --> U
    end

    subgraph DataPlane["Phase 2: Data Plane (CNI on node)"]
        direction TB
        V["CRI/Multus calls CNI ADD"]
        W["ovn-k8s-cni-overlay (shim binary)"]
        X["HTTP POST to CNI server via Unix socket"]
        Y["GetPodWithAnnotations() — poll every 200ms"]
        Z["Wait up to 2 minutes for annotation"]
        AA["k8s.ovn.org/pod-networks annotation found"]
        AB["setupInterface() — enter pod netns"]
        AC["Create veth pair (eth0 ↔ host-veth)"]
        AD["Assign IPs, MAC, routes, gateway in pod netns"]
        AE["ConfigureOVS() — add port on br-int (libovsdb)"]
        AF["Set external_ids: iface-id, iface-id-ver, sandbox"]
        AG["Poll OVS for ovn-installed=true"]
        AH["ovn-controller confirms port binding"]
        AI["Build CNI Result: IPs, MAC, gateway, routes"]
        AJ["Return result to CRI/Multus"]
        AK["Pod containers start"]
        V --> W --> X --> Y --> Z --> AA --> AB --> AC --> AD --> AE --> AF --> AG --> AH --> AI --> AJ --> AK
    end

    J -.->|"unblocks"| AA
    N --> O
    U -.->|"sets ovn-installed"| AH
```

### How the phases connect

The critical handoff between the control plane and the data plane is the
**`k8s.ovn.org/pod-networks` pod annotation**:

* The **controller writes** it after IPAM allocation (during pod reconciliation), and CNI waits on this annotation before interface plumbing.
* The **CNI server blocks** on it before continuing with interface setup.

The OVN pipeline (**ovn-northd → ovn-controller**) runs asynchronously between
these two phases:

* **ovn-northd** compiles LogicalSwitchPort changes into logical flows in SBDB.
* **ovn-controller** translates those flows into OpenFlow rules on `br-int` and
  sets `external_ids:ovn-installed=true` on the OVS interface.
* The CNI server's **`ovn-installed` wait** cannot succeed until that step completes.

### What happens if things go wrong

* If the **controller is slow** (backlog, IPAM exhausted, node switch not ready),
  T1 is delayed and the CNI server times out at T5 after ~2 minutes waiting
  for the annotation.
* If **ovn-controller is slow** (SBDB unreachable, flow computation failure),
  T4 is delayed and the CNI server times out at T6 waiting for
  `ovn-installed=true`.
* In both cases, the container runtime receives a CNI error and retries
  sandbox/network setup.

## Failure Modes

* **CNI ADD timeout** — if the controller has not written the annotation
  within ~2 minutes (e.g. IPAM exhausted, node switch not ready, controller
  backlog), CNI returns an error and the container runtime retries.
* **ovn-installed timeout** — if ovn-controller cannot bind the port (e.g.
  SBDB unreachable, flow computation failure), the CNI server times out.
* **IPAM exhaustion** — `AllocateNextIPs` fails when the node subnet is
  full. The controller re-queues the pod with exponential backoff.

## Key Packages

* `go-controller/pkg/ovn` — Pod reconciliation, IPAM, OVN NBDB objects
* `go-controller/pkg/cni` — CNI shim, CNI server, interface/OVS plumbing
* `go-controller/pkg/allocator` — IP allocation
* `go-controller/pkg/util` — Pod annotation marshalling
