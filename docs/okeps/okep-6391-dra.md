# OKEP-6391: Dynamic Resource Allocation (DRA) for OVN-Kubernetes Networks

* Issue: [#6391](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6391)

## Problem Statement

Providing accelerated networking with ovn-k today requires integration with variuos components, like
- an external device plugin for device discovery and resource pool management (e.g. sriov-network-device-plugin)
- a mutating webhook to add resource requests to the pod spec based on NAD annotations (e.g. network-resources-injector)
- special multus parameters to pass though the device ID from the device plugin to the CNI plugin

Long-working sriov-network-device-plugin integration uses the device plugin framework, which is being deprecated
in favor of Kubernetes' Dynamic Resource Allocation (DRA) framework, which provides a more flexible and powerful way to manage devices in Kubernetes.
While a new dra-driver-sriov is being developed in the network plumbing group, it may be worth for OVN-Kubernetes to add
first-party DRA support for its own networks, to reduce the amount of components needed to run accelerated networking with OVN-K,
and to provide a more seamless and integrated experience for users of OVN-K's accelerated networking features.

## Goals

- Implement a DRA driver in ovn-k that covers full device lifecycle together with the CNI plugin of ovn-k
- Ensure peaceful co-existence with the other DRA drivers/device plugins in the cluster
- Keep supporting existing integrations and workflows
- Provide an easier way for cluster admins to configure and run accelerated networks
- Provide an easier way for users to request accelerated network attachments for their pods, with better integration with the DRA model and its scheduling capabilities

## Non-Goals

- Replacing the existing `k8s.v1.cni.cncf.io/resourceName` device-plugin integration.
- Implementing a generic, multi-network DRA driver that competes with
  k8s-network-plumbing-wg or Mellanox `dra-driver`. The driver implemented
  here is OVN-K-specific (`k8s.ovn.org`).
- Support new types of devices that are not supported by the existing integration, e.g. Infiniband or RDMA devices.
- Replacing the Multi-Network Policy / NetworkAttachmentDefinition CRD itself; only how a pod is matched to a device changes.

## Introduction

OVN-Kubernetes today binds a pod to an accelerated device for a NAD
through label-based device-plugin abstractions (`k8s.v1.cni.cncf.io/resourceName`)
that only works together with an external device discovery plugin, i.e.
[sriov-network-device-plugin](https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin).

Device plugins are getting replaced upstream by Kubernetes' Dynamic Resource Allocation (DRA, GA in v1.34), which lets a user request
a class of devices via a `ResourceClaim` / `ResourceClaimTemplate`, and use extra capabilities for topology-aware scheduling
on a node level (by matching device attributes like PCIe root between NIC and GPU) and in the future on a cross-node level
(PodGroup scheduling).

Network plumbing group is working on the [DRA implementation](https://github.com/k8snetworkplumbingwg/dra-driver-sriov),
at the same time it may be worth for OVN-Kubernetes to add first-party DRA support for its own networks.
An existing example of a DRA driver that handles full network device lifecycle is [dranet](https://github.com/kubernetes-sigs/dranet).
In the following sections I will compare different aspects of the device management using these options.

### Device discovery

The first thing a device plugin has to do is discover the devices on the node and publish them.

#### sriov-network-device-plugin
sriov-network-device-plugin only produces resource pools for devices matching given configuration.
[sriov-network-device-plugin ConfigMap](https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin#config-parameters)
defines the resource pools (one per "kind" of VF). 
Authored by the cluster admin and kept in sync with the hardware inventory.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
 name: sriovdp-config
 namespace: kube-system
data:
 config.json: |
   {
     "resourceList": [
        {
            "resourceName": "intel_sriov_dpdk",
            "resourcePrefix": "intel.com",
            "selectors": [{
                "vendors": ["8086"],
                "devices": ["154c", "10ed", "1889"],
                "drivers": ["vfio-pci"],
                "pfNames": ["enp0s0f0","enp2s2f1"],
                "needVhostNet": true
            }]
        },
        {
            "resourceName": "mlnx_sriov_rdma",
            "resourcePrefix": "mellanox.com",
            "selectors": [{
                "vendors": ["15b3"],
                "devices": ["1018"],
                "drivers": ["mlx5_ib"],
                "isRdma": true
            }]
        }
     ]
   }
```

#### dra-driver-sriov

dra-driver-sriov uses a similar filtering system that doesn't publish any resources unless a config is provided.
[SriovResourcePolicy](https://github.com/k8snetworkplumbingwg/dra-driver-sriov#resource-filtering-system)

```yaml
apiVersion: sriovnetwork.k8snetworkplumbingwg.io/v1alpha1
kind: SriovResourcePolicy
metadata:
  name: example-policy
  namespace: dra-driver-sriov
spec:
  nodeSelector:
    nodeSelectorTerms:
    - matchExpressions:
      - key: kubernetes.io/hostname
        operator: In
        values:
        - worker-node-1
  configs:
  - deviceAttributesSelector:
      matchLabels:
        pool: eth0-resource
    resourceFilters:
    - vendors: ["8086"]           # Intel devices only
      pfNames: ["eth0"]           # Physical Function name
  - deviceAttributesSelector:
      matchLabels:
        pool: eth1-resource
    resourceFilters:
    - vendors: ["8086"]
      pfNames: ["eth1"]
      drivers: ["vfio-pci"]       # Only VFIO-bound devices
```

#### ovn-k DRA plugin

sriov-network-device-plugin and dra-device-sriov use an opt-in approach (see [dra-driver-sriov design](https://github.com/k8snetworkplumbingwg/dra-driver-sriov/blob/main/docs/design/opt-in-advertisement.md)), 
so:
- no resources are visible by default
- figuring out the right selector may be complicated (you need to know the hex IDs of the devices, drivers, etc.)
- selector-based filtering needs a conflict resolution strategy when multiple selectors match the same device

For ovn-k device discovery, I propose to take an opt-out resource discovery, opt-in allocation approach:
- all devices ovn-k can handle (see the next sub-section [ovn-k supported devices](#ovn-k-supported-devices) for more details) are published by default, no extra config needed
- attributes are human-readable and self-describing (e.g. `k8s.ovn.org/pciVendor: "Intel Corporation"` instead of `vendors: ["8086"]`)
- to allocate resources an admin-managed `DeviceClass` has to be created, this is the opt-in allocation part
- filter-out devices that are not relevant for ovn-k (e.g. reserved for the host network or other CNIs) by using the `--dra-filter` CLI flag 
(can be always worked around by the right `DeviceClass` selector, but this flag still may be useful)

DRA model requires cluster admin to define `DeviceClass` to select a set of resources for allocation, which is more
similar to the `SriovResourcePolicy` and `resourceList` configurations, but instead of affecting resource discovery,
it works at the allocation level, which is more intuitive and less error-prone 
(you can have one `DeviceClass` for each type of device, and the same device can be selected by multiple classes if needed).
For the resources that never should be available to the plugin, an admin can filter them out with a simple CEL expression like 
`!("k8s.ovn.org/ifName" in attributes) || attributes["k8s.ovn.org/ifName"].StringValue != "eth0"` (drop `eth0` from the published slice).

Since `DeviceClass` is an admin-level resource, the same amount of configuration is needed to make devices available for allocation,,
but the resources are visible by default and can be allocated as soon as the right `DeviceClass` is created.

Running device discovery is not that hard as long as fetching device-specific attributes is handled by the shared libraries
(which may be used across different DRA drivers, e.g. dranet, sriov-network-device-plugin, dra-driver-sriov, ovn-k, etc.).
We can also ensure that ovn-k only publishes attributes that are relevant for ovn-k and its users, 
and not e.g. attributes related to Infiniband or RDMA capabilities. That should improve readability and reduce the amount
of data stored in the API server.

#### ovn-k supported devices

ovn-k CNI gets `deviceID` coming from the sriov-network-device-plugin through multus, but it doesn't handle all kinds of devices supported by sriov-network-device-plugin.
1. There is a check in the code only accepts PCI devices or SF Auxiliary devices (https://github.com/ovn-kubernetes/ovn-kubernetes/blob/2a72adf10859ac4e2f620bd9392f46b3a3346c23/go-controller/pkg/cni/cniserver.go#L233)
2. VFIO is not supported for auxiliary devices https://github.com/ovn-kubernetes/ovn-kubernetes/blob/2a72adf10859ac4e2f620bd9392f46b3a3346c23/go-controller/pkg/cni/helper_linux.go#L393-L395
3. Only switchdev VFs (with representors) are supported https://github.com/ovn-kubernetes/ovn-kubernetes/blob/2a72adf10859ac4e2f620bd9392f46b3a3346c23/go-controller/pkg/util/sriovnet_linux.go#L129

All these checks happen during CNI ADD, it would be better to run those checks before publishing the devices from the
ovn-k DRA driver to avoid publishing devices that will never be accepted by the CNI plugin.

### Multiple plugins

When different device plugins (or CNIs) are used to configure different types of devices in the same cluster,
(e.g. ovn-k is used with VFs as a primary network and ib-sriov-cni is used for Infiniband devices as a secondary network),
the sriov-network-device-plugin and dra-driver-sriov handle device discovery in a centralized manner, and then pass the allocated device
to the CNI plugin (I am ignoring the `STANDALONE` mode of dra-driver-sriov as it is orthogonal to ovn-k). 
This configuration is done at the NAD level, where the `resourceName` annotation is used to link
the NAD to the device plugin resource pool.

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  annotations:
    k8s.v1.cni.cncf.io/resourceName: nvidia.com/bf2
  name: ovn-primary
  namespace: default
spec:
  config: '{ "cniVersion" : "0.4.0", "name" : "ovn-primary", "net_attach_def_name":
    "default/ovn-primary", "type" : "ovn-k8s-cni-overlay", "logFile": "/var/log/ovn-kubernetes/ovn-k8s-cni-overlay.log",
    "logLevel": "5", "logfile-maxsize": 100, "logfile-maxbackups": 5, "logfile-maxage":
    5 }'
```

Specific CNIs can usually only handle specific types of devices, so if we pass the wrong device type to a CNI,
it will only fail at CNI ADD time, even though the error was made when the NAD was configured. This is the unavoidable 
side-effect of centralized device discovery.

When each device plugin handles device discovery for itself, it can also verify that the driver for a given device is
compatible with the CNI plugin that will configure it.

Consider we have the following DeviceClass (~= device pool) that selects all SR-IOV VFs, and a NAD that references it.
```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: network1
spec:
  selectors:
    - cel:
        expression: device.driver == "k8s.ovn.org"
    - cel:
        expression: device.attributes["k8s.ovn.org"].?isSriovVf.orValue(false) == true
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenantblue
  namespace: test
  annotations:
    k8s.ovn.org/deviceClass: network1
spec:
  config: |2
    {
            "cniVersion": "1.0.0",
            "name": "tenant-blue",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.200.0.0/16",
            "netAttachDefName": "test/tenantblue",
            "role": "secondary"
    }
```
when this NAD is handled by ovn-k, it can now also check that the deviceClass for this NAD selects devices 
with the right driver (`k8s.ovn.org` in this case), and if not, it can reject the NAD configuration with a clear error message.
(this may be implemented in different ways, but having `device.driver == <driver name>` is a normal practice for selecting
DRA devices, so we could just check for this filter and report error in UDN status).

### Pod configuration

Creating a pod with all the right devices is a bit complicated. For accelerated network attachment it has to have the
right amount of resource requests with the right type matching the `resourceName` annotation on the NAD.
The simplification of a pod creation is currently achieved by using pod webhooks that add `resources.requests` to the pod based on the NAD annotations,
e.g. by https://github.com/k8snetworkplumbingwg/network-resources-injector or by custom webhooks, but it is an extra component that
users have to deploy and maintain, and it also adds extra complexity and potential for errors (e.g. what if the webhook is down?).
With this, we only need to put `k8s.v1.cni.cncf.io/resourceName` on the NAD, and then request pod connection to the network 
by using `k8s.v1.cni.cncf.io/networks` annotation on the pod, and the webhook will take care of adding the right `resources.requests` 
to the pod spec based on the NAD annotations.

One interesting part of the contract here is container-specific resource requests. Most of the networking config
happens at the pod level, but some of it may be container-specific (e.g. VFIO devices can only be used by one container).
The mentioned webhook always adds requested resource to the first container, generating interesting dependencies across
components, for example [this part of kubevirt](https://github.com/kubevirt/kubevirt/blob/e72688dc2db45ac9457d4a4cf15b0b58c56cac21/pkg/virt-controller/services/template.go#L485)
makes sure the right container has index 0 to correctly pass resource requests.

Using DRA brings new requirements to the resource request process, that is not yet solved (for dra-driver-sriov too).
In the device plugin model, the only thing that is needed is a device itself, and in the DRA model we want to enable
new scheduling opportunities, e.g. cross-device attribute matching that is usually implemented at the `ResourceClaimTemplate`
level, e.g. (example from `dranet`)

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: 2-gpu-nic-aligned
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          count: 2
          selectors:
          - cel:
              expression: device.attributes["gpu.nvidia.com"].index <= 2
      - name: nic
        exactly:
          deviceClassName: dranet
          count: 2
          selectors:
          - cel:
              expression: device.attributes["dra.net"].rdma == true
      constraints:
      - matchAttribute: "resource.kubernetes.io/pcieRoot"
```

In this case there is no way to automatically generate `resourceClaims` for the pod based on the NAD annotations, 
since the claim template itself contains important information about how many devices should be allocated, what selectors should be used, etc., 
and this information is not present in the NAD annotations.
We could still use `deviceClass` annotation on the NAD to link it to the right allocated resource, but the actual claim template 
will have to be created by the user or some other controller, and the user will have to put `resourceClaims` reference to the pod spec manually.

For users that want the new DRA-enabled scheduling capabilities this shouldn't be a problem but if the same cluster still
has old users that simply want a VF allocated to their pod without caring about the scheduling capabilities,
we want to amke sure they won't need to change their workflow.
We could do so by replicating the existing webhook behaviour, but with 2 potential advantages:
- make pod mutation optional by setting an extra NAD annotation, e.g. `k8s.ovn.org/autoGenerateClaims: "true"`.
This makes mutating behaviour optional and more obvious, also allowing to disable it for any given NAD when needed.
Annotating old NADs should be the only change for the old users, and NAD setup is often handled by cluster admin.
- make mutating webhook a part of the ovn-k repo.
This simplifies testing and integration: instead of some external component mutating pods based on selector configured
in that component. For example, network-resources-injector has a namespace selector that is configured in the webhook itself,
which makes it static and invisible for non-admins.

To implement this we would need:
- a controller that will watch for NADs with `k8s.ovn.org/autoGenerateClaims: "true"` annotation,
and when it sees a new NAD, it will create a `ResourceClaimTemplate` for this NAD with the right `DeviceClass`
- a webhook that will mutate pods that request attachments to NADs with `k8s.ovn.org/autoGenerateClaims: "true"` annotation,
and add `resourceClaims` reference to the pod spec based on the NAD annotations

We also shouldn't have any conflicts with network-resources-injector running in parallel, since ovn-k-accelerated NADs
will have `k8s.ovn.org/deviceClass` annotation instead of `k8s.v1.cni.cncf.io/resourceName`, so the webhook will
ignore that NAD.

For comparison, this is the config currently used with sriov-network-device-plugin and network-resources-injector
```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenantblue
  namespace: test
  annotations:
    k8s.v1.cni.cncf.io/resourceName: resource1
spec:
  config: |2
    {
            "cniVersion": "1.0.0",
            "name": "tenant-blue",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.200.0.0/16",
            "netAttachDefName": "test/tenantblue",
            "role": "secondary"
    }
---
apiVersion: v1
kind: Pod
metadata:
  name: pod1
  namespace: test
  annotations:
    k8s.v1.cni.cncf.io/networks: "tenantblue, tenantblue"
spec:
  containers:
    - name: ctr1
      image: registry.k8s.io/e2e-test-images/agnhost:2.54
      securityContext:
        capabilities:
          add: [ "NET_ADMIN", "NET_RAW" ]
      command: ["/agnhost", "netexec"]
```

this is an ovn-k analogue of the same config, with the auto-generation of claim templates and pod mutation optimizations described above,
```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenantblue
  namespace: test
  annotations:
    # different annotation here
    k8s.ovn.org/deviceClass: network1
    # opt-in annotation for auto-generating claim templates and mutating pods
    k8s.ovn.org/autoGenerateClaims: true
spec:
  config: |2
    {
            "cniVersion": "1.0.0",
            "name": "tenant-blue",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.200.0.0/16",
            "netAttachDefName": "test/tenantblue",
            "role": "secondary"
    }
---
apiVersion: v1
kind: Pod
metadata:
  name: pod1
  namespace: test
  annotations:
    k8s.v1.cni.cncf.io/networks: "tenantblue, tenantblue"
spec:
  containers:
    - name: ctr1
      image: registry.k8s.io/e2e-test-images/agnhost:2.54
      securityContext:
        capabilities:
          add: [ "NET_ADMIN", "NET_RAW" ]
      command: ["/agnhost", "netexec"]
```
that will mutate pod's `spec.resourceClaims` and also auto-generate
```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  # same name as nad to make creating pods easier
  name: tenantblue
  namespace: test
spec:
  spec:
    devices:
      requests:
        - name: "tenantblue"
          exactly:
            deviceClassName: "network1"
```

but also for the DRA attribute matching they could do
```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenantblue
  namespace: test
  annotations:
    # just different annotation here
    k8s.ovn.org/deviceClass: network1
spec:
  config: |2
    {
            "cniVersion": "1.0.0",
            "name": "tenant-blue",
            "type": "ovn-k8s-cni-overlay",
            "topology":"layer2",
            "subnets": "10.200.0.0/16",
            "netAttachDefName": "test/tenantblue",
            "role": "secondary"
    }
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: 2-gpu-nic-aligned
spec:
  spec:
    devices:
      requests:
        - name: gpu
          exactly:
            deviceClassName: gpu.nvidia.com
            count: 2
            selectors:
              - cel:
                  expression: device.attributes["gpu.nvidia.com"].index <= 2
        - name: nic
          exactly:
            deviceClassName: network1
            count: 2
            selectors:
              - cel:
                  expression: device.attributes["k8s.ovn.org"].?isSriovVf.orValue(false) == true
      constraints:
        - matchAttribute: "resource.kubernetes.io/pcieRoot"
---
apiVersion: v1
kind: Pod
metadata:
  name: pod1
  namespace: test
  annotations:
    k8s.v1.cni.cncf.io/networks: "tenantblue, tenantblue"
spec:
  containers:
    - name: ctr1
      image: registry.k8s.io/e2e-test-images/agnhost:2.54
      securityContext:
        capabilities:
          add: [ "NET_ADMIN", "NET_RAW" ]
      command: ["/agnhost", "netexec"]
  resourceClaims:
    - name: aligned-devices
      # this already requests 2 NICs, so each NAD will get 1 NIC from the right deviceClass network1
      resourceClaimTemplateName: 2-gpu-nic-aligned
```

One drawback of this solution:
- pod will be in Pending state until the ResourceClaimTemplate is generated 
(not too bad though, as pod will just wait for the claim to be ready, and it will be ready as soon as the controller creates it)

Some pros compared to the sriov-network-device-plugin solution:
- no need to maintain a separate component (webhook) that adds resource requests to the pod spec based on NAD annotations
- explicit opt-in for pod mutator on per-network basis
- better integration with the DRA model, since users can create their own ResourceClaimTemplates with different selectors
instead of using an automatically-created one

### VFIO/device passthrough

For VFIO devices to work, container runtime needs to pass `/dev/vfio/vfio` and `/dev/vfio/<iommu-group>` to the container,
there multiple ways to do so.
sriov-network-device-plugin supports the `DeviceSpec` way which is only available to device plugins, and the [CDI](https://github.com/cncf-tags/container-device-interface)
way, which is also available to the DRA plugins and is used by the dra-driver-sriov.
This approach is also used by the [dra-example-driver](https://github.com/kubernetes-sigs/dra-example-driver).
It requires CDI to be enabled in the cluster, which is not done by default, but is likely to be enabled when DRA is used 
(usually for GPU config, e.g. https://github.com/NVIDIA/k8s-device-plugin/blob/main/docs/cdi.md).

Another way to mount the mentioned paths is by using NRI as dranet does, but NRI is too powerful just for that, 
as long as we handle the whole network lifecycle through CNI.

To support this type of devices, ovn-k will create its own CDIHandler, following dra-example-driver.
All it has to do is to mount specific paths to the container when a VFIO device is allocated.

### Device configuration

When device discovery is done in the same place as device configuration, we don't need to re-discover device attributes
at CNI ADD time based on the device ID passed from the device plugin, we can just look them up internally.
That would help reduce the amount of code and logic needed in the CNI plugin, and also reduce the amount of time needed for CNI ADD.

### Cross-component integration and development

So we have established that for now we need 3-4 components to run accelerated networking with OVN-K:
- ovn-k as a CNI
- sriov-network-device-plugin or dra-driver-sriov for resource discovery
- multus (both for multi-network support and for passing the allocated device ID from the device plugin to the CNI plugin)
- network-resources-injector or custom webhook for mutating pods to add resource requests based on NAD annotations

Each of those has to be configured the right way and have the right set of versions to work together.
With the ovn-k DRA integration we only need
- ovn-k with DRA enabled
- multus (multi-network only, no deviceID passing)

Now let's imagine we want to add a new device support, to do so we need to
- add new device discovery to the device plugin (sriov-network-device-plugin or dra-driver-sriov)
- potentially pass new attributes from the device plugin to the CNI plugin through multus (or most likely just re-discover
everything based on PCI ID of the new device)
- add new device type handling to the CNI plugin of ovn-k
- for VFIO-like case make sure network-resources-injector updates the right container with the resource request

If I want to add a support of the new device type for ovn-k DRA, I only need changes in ovn-k (and multus will never
need to change for that because we are not using if for device-related configuration).
See multus changes required to make dra-driver-sriov work for comparison https://github.com/k8snetworkplumbingwg/multus-cni/pull/1492.

New feature development will have to happen across multiple repositories, which is not only hard to implement, but also
very hard to test. Which brings me to the next point: testing.
Accelerated networking testing has been notoriously hard (and is mostly ignored), which leads to us often
introducing breaking changes or lack of support for the new features. See [Testing Details](#testing-details) for more details on how it can be improved.

## User-Stories/Use-Cases

**Story 1: AI workload aligns NIC and GPU on the same PCIe root.**

1. **As an** AI/HPC platform engineer running training jobs that move large
   tensors between GPU memory and the network (RDMA / GPUDirect),
2. **I want** the pod's SR-IOV VF and its GPU to share the same PCIe root
   complex on the chosen node, expressed as a single `ResourceClaim` that
   includes both a network device (driver `k8s.ovn.org`) and a GPU device
   (driver e.g. `gpu.nvidia.com`), with a constraint that the two devices'
   `resource.kubernetes.io/pcieRoot` attribute match,
3. **so that** traffic stays on the same root complex and
   avoids cross-socket QPI/UPI bottlenecks. The Kubernetes scheduler picks a
   node and a (NIC, GPU) pair satisfying the constraint atomically; both
   drivers' `PrepareResourceClaims` callbacks fire on that node; OVN-K's CNI
   wires the chosen VF into the pod's network attachment.

The OVN-K DRA driver attaches the canonical
`resource.kubernetes.io/pcieRoot` attribute to each PCI device; GPU DRA drivers expose the same
attribute. The user couples the two in one `ResourceClaim` with a
`MatchAttribute`:

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata: {name: ovnk-sriov-vf}
spec:
  selectors:
    - cel: {expression: 'device.driver == "k8s.ovn.org"'}
    - cel: {expression: 'device.attributes["k8s.ovn.org"].?isSriovVf.orValue(false) == true'}
---
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata: {name: gpu-h100}
spec:
  selectors:
    - cel: {expression: 'device.driver == "gpu.nvidia.com"'}
    - cel: {expression: 'device.attributes["gpu.nvidia.com"].?productName.orValue("") == "H100"'}
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata: {name: ai-pair, namespace: ml}
spec:
  spec:
    devices:
      requests:
        - name: nic
          exactly: {deviceClassName: ovnk-sriov-vf}
        - name: gpu
          exactly: {deviceClassName: gpu-h100}
      constraints:
        - matchAttribute: resource.kubernetes.io/pcieRoot
          requests: [nic, gpu]
```

## Proposed Solution

Run an in-process DRA kubelet plugin per node, owned by `ovnkube-node`, that
publishes the node's network devices as a `ResourceSlice` and accepts
`ResourceClaim` allocations. Plumb the resulting pod->device mapping into
OVN-K's CNI server via shared in-memory state. Link NAD/UDN objects to a
`DeviceClass` via the annotation `k8s.ovn.org/deviceClass`. On CNI Add, OVN-K
looks up the device allocated to the pod under the matching device class and
treats the PCI BDF as the `DeviceID` for its existing SR-IOV CNI flow.

### API Details

This OKEP introduces:

**1. One new annotation on `NetworkAttachmentDefinition` and
`UserDefinedNetwork` objects.**

```
k8s.ovn.org/deviceClass: <DeviceClass.metadata.name>
```

- Type: string. Cluster-scoped object name (no namespace).
- The referenced `DeviceClass` must select devices with
  `device.driver == "k8s.ovn.org"`; CEL selectors targeting other drivers
  will not match this OKEP's driver and the pod will fail scheduling with a
  standard DRA "cannot satisfy claim" error.

No changes to OVN-K's own CRDs (`UserDefinedNetwork`,
`ClusterUserDefinedNetwork`, etc.) other than the addition of this
annotation. No new fields on `NetConf`.

**2. DRA driver identity, attribute namespace, and well-known names.**

ovn-k will publish devices with `Driver` set to `k8s.ovn.org` like so

```yaml
- apiVersion: resource.k8s.io/v1
  kind: ResourceSlice
  metadata:
    name: my-cluster-worker-1.my-cluster.lab-k8s.ovn.org-c825s
    ownerReferences:
    - apiVersion: v1
      controller: true
      kind: Node
      name: my-cluster-worker-1.my-cluster.lab
      uid: 7d74abd5-702c-4b98-a919-3ea0d10b2748
  spec:
    devices:
    - attributes:
        k8s.ovn.org/ifName:
          string: eth0
        k8s.ovn.org/mac:
          string: 52:54:00:f3:33:a1
        k8s.ovn.org/mtu:
          int: 1500
        k8s.ovn.org/pciAddress:
          string: "0000:01:00.0"
        k8s.ovn.org/pciDevice:
          string: Virtio 1.0 network device
        k8s.ovn.org/pciVendor:
          string: Red Hat, Inc.
        k8s.ovn.org/sriov:
          bool: false
        k8s.ovn.org/state:
          string: up
        k8s.ovn.org/type:
          string: device
        resource.kubernetes.io/pcieRoot:
          string: pci0000:00
      name: pci-0000-01-00-0
    - attributes:
        k8s.ovn.org/ifName:
          string: eth1
        k8s.ovn.org/mac:
          string: 52:54:00:a6:1a:92
        k8s.ovn.org/mtu:
          int: 1500
        k8s.ovn.org/numaNode:
          int: 1
        k8s.ovn.org/pciAddress:
          string: "0000:29:00.0"
        k8s.ovn.org/pciDevice:
          string: 82576 Gigabit Network Connection
        k8s.ovn.org/pciVendor:
          string: Intel Corporation
        k8s.ovn.org/sriov:
          bool: true
        k8s.ovn.org/sriovVfs:
          int: 4
        k8s.ovn.org/state:
          string: up
        k8s.ovn.org/type:
          string: device
        resource.kubernetes.io/pcieRoot:
          string: pci0000:28
      name: pci-0000-29-00-0
    driver: k8s.ovn.org
    nodeName: my-cluster-worker-1.my-cluster.lab
    pool:
      generation: 1
      name: my-cluster-worker-1.my-cluster.lab
      resourceSliceCount: 1
```

**3. Configuration knobs.**

| Field | Type | Default | CLI flag | gcfg key |
|---|---|---|---|---|
| `OvnKubeNode.EnableDRA` | bool | **`false`** | `--enable-dra` | `enable-dra` |
| `OvnKubeNode.DRAFilter` | string | `""` | `--dra-filter` | `dra-filter` |

When `EnableDRA=false` the DRA driver is not started and the `draState`
passed to the CNI server is nil — the DRA branch in `cniserver.go` is then
never taken.

`DRAFilter` is a CEL expression evaluated against each discovered device's
`attributes` map (type `map[string]v1.DeviceAttribute`). Devices for which
the expression evaluates to `true` are published; an empty value disables
filtering. Example: `!("k8s.ovn.org/ifName" in attributes) ||
attributes["k8s.ovn.org/ifName"].StringValue != "eth0"` (drops `eth0` from
the published slice). The expression is compiled at startup;
syntax errors fail ovnkube-node startup.

### Implementation Details

The implementation lives in three packages that mirror the lifecycle of a
DRA-allocated device:

**`pkg/dra/` — the kubelet DRA plugin (per-node).** Started by `ovnkube-node`
when `--enable-dra` is set on the default node network controller. The
`NetworkDriver` struct registers a kubelet plugin under the driver name
`k8s.ovn.org`, discovers the node's network devices by walking PCI (via
`ghw`) and netlink, and publishes a `ResourceSlice` whose pool name equals
the node name. Device discovery stamps a fixed set of attributes
so `DeviceClass` selectors and per-claim
CEL selectors can match against any of them. An optional CEL filter
(`--dra-filter`) prunes the published list to what an administrator wants
to expose. Allocations are recorded in an in-process `SharedState`
(pod-UID → device map + per-device attribute shadow), which is the bridge
to the CNI server in the next package.

**`pkg/cni/` — the CNI server (per-node, same binary).** The CNI server
holds a reference to the `DRAState` interface exposed by the DRA plugin.
Before dispatching a CNI ADD, `setDeviceInfo` looks up the pod's NAD; if
the NAD carries `k8s.ovn.org/deviceClass` (and not the legacy
`k8s.v1.cni.cncf.io/resourceName`, which still wins for backward
compatibility), the server queries `SharedState.GetAllocatedDevices(podUID)`
to find the device that matches the requested `deviceClass` and pod
interface name, sets `req.CNIConf.DeviceID` to the device's PCI BDF. 
From this point the CNI flow is the same as before.

**`pkg/clustermanager/dra/` and `pkg/ovnwebhook/` — the cluster-side
ergonomics.** A reconciler in `clustermanager/dra` watches NADs in every
namespace and, for each NAD with `k8s.ovn.org/autoGenerateClaims: "true"` and 
`k8s.ovn.org/deviceClass` set (and no legacy `resourceName`), ensures a per-NAD `ResourceClaimTemplate` named
`<nadName>-claim` exists in the same namespace, owned by the NAD via
`ownerReferences`. The template's single device request references the
NAD's `deviceClass`. A mutating admission webhook (`pkg/ovnwebhook/
podclaiminjection.go`) registered on Pod CREATE looks up each NAD
referenced by the pod's `k8s.v1.cni.cncf.io/networks` annotation; for
those with `k8s.ovn.org/autoGenerateClaims`, it appends a `pod.spec.resourceClaims[]` entry
pointing at the template and adds the matching reference into each
targeted container's `resources.claims[]`.

### Testing Details

Testing accelerated networking upstream without any hardware is complicated, so I am in a
process of finding a way to run some tests u/s with the real hardware (similar to what we
have for DPU).
In the meantime, while working on PoC implementation I found it very useful using a 
`kcli` tool to run a cluster on VMs locally (following the sriov-network-operator test framework).
It allows creating virtualized devices and testing the full chain from resource discovery to
device configuration and network connectivity. One disadvantage is that the virtualized environment 
doesn't support switchdev VFs, which is the only mode ovn-k supports.
I managed to add legacy VF support for testing with a fairly small code change, which seems reasonable
for the benefits this e2e test provides. I will try to use self-hosted github runners that we already use
for perf testing to run `kcli` clusters and run some integration tests that way.

**Cross Feature Testing details**

- Compatibility with the existing `k8s.v1.cni.cncf.io/resourceName`
  annotation: pod with NAD having both annotations should pick the
  device-plugin path. Add a test covering legacy-precedence.
- Interconnect: each zone's DRA driver is independent; verify a pod
  scheduled to zone B does not see device allocations published by zone A.
- Multi-network: pod with one DRA-backed primary UDN + one DRA-backed
  secondary NAD; both `eth0` and `net1` should be plumbed correctly.
- DPU testing: we should be able to run DPU tests with DRA too. 

### Documentation Details

- New page under `docs/features/dra-device-allocation.md`:
  - Overview of DRA and the `k8s.ovn.org` driver.
  - Step-by-step "label a UDN as DRA-managed" walkthrough using
    `pkg/dra/test-yaml/resourceclaim2.yaml` as a reference.
  - Attribute reference table (matching the API Details table above).
  - Troubleshooting: how to inspect published `ResourceSlice`s
    (`kubectl get resourceslice`), how to find the allocation for a pod
    (`kubectl get resourceclaim -n <ns>`), and how to verify OVN-K used
    the right device (`kubectl get pod <pod> -o
    jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}'`).
- Add to `mkdocs.yml` under the User-Defined Networks section
  (`mkdocs.yml:135`):
  `- DRA Device Allocation: features/dra-device-allocation.md`.
- Add `okeps/okep-6391-dra.md` to the mkdocs okeps navigation list.

## Risks, Known Limitations and Mitigations

- **DRA API stability.** `resource.k8s.io/v1` is stable since K8s 1.34 but
  the kubelet plugin library
  (`k8s.io/dynamic-resource-allocation/kubeletplugin`) is still evolving.
  Mitigation: pin the dependency in `go.mod`; track upstream API changes in
  the version-skew section per OKEP review.
- **CRI runtime CDI enablement.** CDI must be enabled at the runtime
  level (`enable_cdi = true` for CRI-O, `enable_cdi = true` in the
  containerd CRI plugin), and `cdi_spec_dirs` must include
  `/var/run/cdi`. Without this the per-claim spec written by the driver
  is silently ignored — the consumer container starts without
  `/dev/vfio/*`, no error is logged on the spec writer side. Mitigation:
  document the runtime config; future ovnkube-node could verify at
  startup by reading the runtime's introspection (CRI `RuntimeConfig`
  RPC) and refusing to enable DRA if CDI is off.
- **Mutating webhook scope and failure mode.** The pod-claim-injection
  webhook fires on every Pod CREATE that carries the multus
  `k8s.v1.cni.cncf.io/networks` annotation, even when the referenced
  NADs are not DRA-managed. Mitigation: matchCondition narrows the call
  set to multus-attached pods, the per-NAD GET is cached, and the
  webhook is `failurePolicy: Ignore` so a webhook outage cannot block
  pod creation — at worst pods with a DRA NAD will fail to schedule
  with a clear "no resource claim" error.
- **Shell-quoting of `--dra-filter`.** The CEL expression contains spaces,
  parentheses, `!`, `|`, and quotes — all shell-significant. Mitigation:
  `dist/images/ovnkube.sh` builds the flag in a bash array
  (`dra_filter_flag=("--dra-filter=${ovn_dra_filter}")`) and passes it as
  `"${dra_filter_flag[@]}"`. Earlier word-splitting caused argv corruption
  that made the binary skip later flags and fall back to in-cluster SA
  auth, surfacing as misleading RBAC "forbidden" errors.
- **CEL filter compile errors fail startup fast.** A malformed
  `--dra-filter` expression causes ovnkube-node to exit at boot with a
  clear `failed to compile DRA filter` error. Mitigation: validate the
  expression in CI / dev clusters before deployment.
- **Conflict with device-plugin path.** If a NAD is mis-annotated with
  both `resourceName` and `deviceClass`, the legacy path wins silently
  in `setDeviceInfo`, and the template reconciler / webhook skip the NAD.
  Mitigation: documented; a validating admission webhook to reject NADs
  carrying both annotations is a follow-up.
- **Privileged-mode requirement.** DRA can't be enabled in
  `--unprivileged-mode` because the driver needs to host-mount sysfs and
  create the kubelet socket path. Mitigation: ovnkube-node fails fast at
  startup with a clear error.

## OVN-Kubernetes Version Skew

- **Introduced in:** v1.2.0 (target release; verify against repo
  milestones at OKEP-approval time).
- **Minimum Kubernetes version:** 1.34 (DRA GA in `resource.k8s.io/v1`).
- **Required ovnkube-node config:** `enable-dra=true` (default false).
- **Cluster mixed-version behaviour during upgrade:** because the DRA
  driver is node-local and self-contained, nodes running newer ovnkube-node
  versions begin publishing `ResourceSlice`s while older nodes do not. A
  pod scheduled to an older node with an OVN-K DRA `DeviceClass` will fail
  to be scheduled (no matching slices), which is the correct behaviour.
  The only centralized part is the cluster manager reconciler that
  creates ResourceClaimTemplates for the auto-generated claims.
  New DRA capabilities should only be used after the cluster is fully
  upgraded (or ensure cluster manager is upgraded first in case auto-generated
  claims need to work immediately).

## Backwards Compatibility

- The DRA feature is **additive**. Pods, NADs, and UDNs that don't use the
  new `k8s.ovn.org/deviceClass` annotation see no behavioural change.
- The legacy `k8s.v1.cni.cncf.io/resourceName` device-plugin path is
  preserved and takes precedence when both annotations are present on the
  same NAD. No deprecation is proposed in this OKEP.
- No CRD schema changes. No CNI binary protocol changes.
- The `EnableDRA` config knob defaults to `false`. Operators that want the
  feature flip it on via `--set global.enableDRA=true` (or the equivalent
  env-var path through the chart). The kind setup (`contrib/kind.sh`)
  defaults `ENABLE_DRA=true` for CI / dev convenience.
- New optional `DRAFilter` config (default empty) lets operators exclude
  devices from publication. No-op when unset.
- No on-disk per-pod state file is introduced; restart recovery uses the
  apiserver as the source of truth via `reconcileExistingClaims` +
  `claim.Status.Devices`.

## Alternatives

Use dra-driver-sriov for DRA support.

## References

- Kubernetes Dynamic Resource Allocation:
  - GA blog: https://kubernetes.io/blog/2025/10/01/kubernetes-v1-34-dra-ga/
  - API ref: https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/
  - kubelet plugin library:
    https://pkg.go.dev/k8s.io/dynamic-resource-allocation/kubeletplugin
- Related OKEPs:
  - OKEP-5193 (User Defined Networks) — the NAD/UDN model this OKEP
    extends.
