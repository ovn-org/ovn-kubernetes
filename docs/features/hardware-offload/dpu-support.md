## DPU support

With the emergence of [Data Processing Units](https://blogs.nvidia.com/blog/2020/05/20/whats-a-dpu-data-processing-unit/) (DPUs),
NIC vendors can now offer greater hardware acceleration capability, flexibility and security.

It is desirable to leverage DPU in OVN-kubernetes to accelerate networking and secure the network control plane.

A DPU consists of:
- Industry-standard, high-performance, software-programmable multi-core CPU
- High-performance network interface
- Flexible and programmable acceleration engines

Similarly to Smart-NICs, a DPU follows the kernel switchdev model.
In this model, every VF/PF net-device on the host has a corresponding representor net-device existing
on the embedded CPU.

Any vendor that manufactures a DPU which supports the above model should work with current design.

Design document can be found [here](https://docs.google.com/document/d/11IoMKiohK7hIyIE36FJmwJv46DEBx52a4fqvrpCBBcg/edit?usp=sharing).

## OVN-Kubernetes in a DPU-Accelerated Environment

The **ovn-kubernetes** deployment will have two parts one on the host and another on the DPU side.

These aforementioned parts are expected to be deployed also on two different Kubernetes clusters, one for the host and another for the DPUs.

Always check the dependencies on the [Requirements page](../requirements.md)

### Host Cluster

#### OVN-Kubernetes control plane related component
- ovn-cluster-manager

#### OVN-Kubernetes components on a Standard Host (Non-DPU)
- local-nb-ovsdb
- local-sb-ovsdb
- run-ovn-northd
- ovnkube-controller-with-node
- ovn-controller
- ovs-metrics

#### OVN-Kubernetes component on a DPU-Enabled Host
- ovn-node

For detailed configuration of gateway interfaces in DPU host mode, see [DPU Gateway Interface Configuration](dpu-gateway-interface.md).

For CUDNs that use a dedicated external OVS bridge through the `Uplink` API,
see [Uplinks for User Defined Networks](../user-defined-networks/uplinks.md).
In DPU deployments, the DPU-host side discovers host interface state and the
DPU side resolves the local OVS bridge from that state.

### Simulated DPU (`simulate-dpu`)

Hardware DPUs rely on SR-IOV and switchdev metadata so OVN-Kubernetes can resolve representors, PCI relationships, and related details. For development and CI, the same code paths can run on **simulated** platforms (for example KinD or VMs using virtio instead of a real switchdev DPU NICs) where that metadata is absent or different.

The ovnkube-node flag **`--simulate-dpu`** (config file `[OvnKubeNode]` key `simulate-dpu`, Helm `global.simulateDpu`) turns on **simulated DPU operations** instead of the default switchdev-based implementation. When the node mode is `dpu` or `dpu-host`, this selects the simulated `DPUOps` backend. Operational parameters such as representor naming, device addressing, and host-representor discovery follow the patterns used in simulated environments (for example `rep<pfId>-<funcId>` style names and netdev-based identifiers) rather than sysfs/sriovnet switchdev discovery.

The flag has no effect in full (non-DPU) node mode. Use it together with the correct simulated topology (veth pairs, virtio links) and, on the DPU host, consider setting an explicit [`dpu-host-gateway-representor-interface`](dpu-gateway-interface.md#dpu-host-gateway-representor-interface) when nicstobridge is used (when there is no pre-provisioned OVS bridges).

### DPU Cluster

#### OVN-Kubernetes components
- local-nb-ovsdb
- local-sb-ovsdb
- run-ovn-northd
- ovnkube-controller-with-node
- ovn-controller
- ovs-metrics

## Host Node deletion

In DPU mode `ovnkube-controller-with-node` runs on the DPU but acts on behalf of
the host `Node` object (`K8S_NODE`), and the node-level annotations it owns
(`k8s.ovn.org/node-chassis-id`, `k8s.ovn.org/l3-gateway-config`,
`k8s.ovn.org/node-mgmt-port-mac-addresses`, ...) are written once at startup.
Deleting the host `Node` from the cluster does not stop the pod on the DPU the way
it would for a pod running on the deleted node itself.

When ovnkube-node in DPU mode observes the deletion of the `Node` it manages, it
exits. The container restarts and fails until the host `Node` is registered
again, then initializes against the new `Node` object and rewrites its
annotations. Expect the DPU-side ovnkube-node pod to crash-loop while the host
node is absent; this is the intended signal that the node is gone, not a fault
of the DPU.

## DPU health monitoring

OVN-Kubernetes uses a custom Kubernetes `Lease` in the `ovn-kubernetes` namespace to track the health of the DPU side of a trusted deployment.
The DPU host creates the lease and sets an owner reference to the Kubernetes `Node`, while ovnkube running on the DPU renews the lease on a regular interval.

Two ovnkube-node options control this behavior:
- `--dpu-node-lease-renew-interval` (seconds, default 10). Set to `0` to disable the health check.
- `--dpu-node-lease-duration` (seconds, default 40).

If the lease expires, the DPU host CNI server fails `ADD` requests immediately with `DPU Not Ready` and the `STATUS` command returns a CNI error with code `50` (The plugin is not available).
This causes the container runtime to report `NetworkReady=false`, preventing new workloads from landing on the affected host until the DPU becomes healthy again.
