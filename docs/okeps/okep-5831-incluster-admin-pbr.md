# OKEP-5831: Add API to support in-cluster policy based routing

* Issue: [#5831](https://github.com/ovn-org/ovn-kubernetes/issues/5831)

## Problem Statement

The workloads running in ovn-kubernetes backed k8s cluster may need policy based routing to
access external services. For example, in a multi-tenant cluster, tenant Coke needs to go 
through gateway A to access internet, while tenant Pepsi needs to go through gateway B to access
internet. Both gateway A and B expose internal IP addresses to the clients, and clients can specify
the internal IPs as nexthop.

Custom routing is also needed when Istio CNI is used in Ambient mode, as ingress Kubelet healthcheck
probes via the mp0 interface will be SNAT'ed to 169.254.7.127, and the ingress-reply packet from the
pod (which would normally go via shared gateway) needs to route back towards mp0.

## Goals

* Reconcile in-cluster PBR rules based on CRD spec
* Reconcile address sets in response to Pod/Node/Namespace events
* Automatically use remote port's IP as nexthop if the specified nexthop is in the remote zone
* Properly configured CRD can make health check working for Istio/Ambient enabled pods, for interconnect mode only

## Non-Goals


## Introduction

The need for policy-based routing (PBR) within Kubernetes clusters has grown as network architectures become more complex, particularly in multi-tenant and hybrid network environments. Traditional routing assigns the same route to all workloads without regard to context such as namespace, pod, or tenant affinity. However, modern use cases often require that certain workloads use specific gateways or follow unique routing paths, for example to partition internet access by tenant, or to route workloads through VPNs/proxies.

This proposal introduces an API for cluster administrators to configure and manage in-cluster policy-based routing backed by Logical_Router_Policy in OVN. The intent is to allow for declarative, CRD-based configuration of routing policies that respond dynamically to changes in the cluster, such as pods being added, removed, or relocated.

## User-Stories/Use-Cases

Story 1: Internet access partitioning

As a cluster administrator, I want to specify gateway A for tenant Coke, gateway B for the tenant Pepsi, so that internet traffic can be
distributed among multiple gateways.
  
Story 2: VPN

As a cluster administrator, I want to specify policy based routing so that my workload can talk to external services through VPN.

Story 3: Readiness check with Istio CNI

As a cluster administrator, I want to enable Istio CNI while keep health check working by rerouting packets to node's management port, this applies to IC mode ovn-kubernetes only.

## Proposed Solution

We will introduce a new cluster-scoped CRD: `AdminPolicyBasedRoute`, to allow cluster administrators to declaratively define in-cluster PBR (Policy-Based Routing) rules, and add a new AdminPolicyBasedRouteController to reconcile these custom resources into OVN's [Logical_Router_Policy](https://man7.org/linux/man-pages/man5/ovn-nb.5.html#Logical_Router_Policy_TABLE).

For interconnect enabled clusters, if the nexthops specified in the CRD are in remote zones, the actual nexthop in Logical_Router_Policy record will be
replaced with the transit IP to the remote zone.

To address the routing issue for health check traffic under Istio/Ambient mode, we will introduce a new field `NextHopDevices` to allow users to specify a port name as nexthop, the controller needs to figure out the actual IP and apply it in Logical_Router_Policy record.

#### High-Level Workflow Diagram

```
+------------------+                          +--------------------+
| AdminPolicyBased |    watches CRUD       -> | APBR Controller    |
|   Route CRs      |------------------------->| (in ovnkube-master)|
+------------------+                          +--------------------+
                                                ^       |
                                                |       | applies
                       watches events           |       |
+------------------+                            |       v
|   Pod Events     |----------------------------+    +-----------------------------+
+------------------+                            |    | OVN NBDB (Logical_Router_   |
                                                |    |          Policy)            |
+------------------+                            |    +-----------------------------+
| Namespace Events |----------------------------+    |
+------------------+                            |    | updates
                                                |    v
+------------------+                            |    +-----------------------------+
|   Node Events    |----------------------------+    | OVN Address Sets            |
+------------------+                                 +-----------------------------+
```

### API Details

* A new API `AdminPolicyBasedRoute` under the `k8s.ovn.org/v1beta1` group will be added to  
  `go-controller/pkg/crd/adminpbr/v1beta1`. This would be a cluster-scoped CRD:

```go
import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crdtypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
)

type Status string

const (
	StatusSucceeded Status = "Succeeded"
	StatusFailed    Status = "Failed"
)

// AdminPolicyBasedRouteStatus describes the current status of the AdminPolicyBasedRoute.
type AdminPolicyBasedRouteStatus struct {
	// An array of Human-readable messages indicating details about the status of the object.
	// +optional
	Messages []string `json:"messages,omitempty"`

	// A concise indication of whether the AdminPolicyBasedRoute resource is applied or not
	// +optional
	Status Status `json:"status,omitempty"`
}

// AdminPolicyBasedRoute cluster-scoped API provides a way to influence routing decisions
// in the SDN network. The API can be used to match the packets originating from and/or
// -- kubernetes nodes, namespaces, pods -- and forward them to different destination for
// further processing.
//
// This API leverages OVN's Logical Router Policy feature. The AdminPolicyBasedRoute policies
// are translated to OVN policies and these policies override OVN's static routing decision.
// The priority of the policy rules is set to 80 (priority in range of 0 to 32,767, with
// numerically higher priority taking precedence over those with lower), and it processed
// after all the OVN K8s' pre-defined rules.
//
// +genclient
// +genclient:nonNamespaced
// +resource:path=adminpolicybasedroute
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=adminpbr,scope=Cluster
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type AdminPolicyBasedRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AdminPolicyBasedRouteSpec   `json:"spec,omitempty"`
	Status AdminPolicyBasedRouteStatus `json:"status,omitempty"`
}

// AdminPolicyBasedRouteSpec defines the desired state of cluster scoped routing policies
type AdminPolicyBasedRouteSpec struct {
	// networkSelectors selects the networks on which the pod need to be applied with the routing policies.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="networkSelectors are immutable"
	NetworkSelectors crdtypes.NetworkSelectors `json:"networkSelectors,omitempty"`

	// a collection of policy objects to influence routing
	Policies []RoutingPolicyRule `json:"policies"`
}

// RoutingPolicyRule is a single routing policy rule object
type RoutingPolicyRule struct {
	// From matches the packets for the routing policy
	From RoutingPolicyMatch `json:"from"`
	// To specifies destination addresses used to match the same in the packets
	To networkingv1.IPBlock `json:"to"`
	// NextHop defines where the matched packets should be forwarded
	NextHop RoutingPolicyNextHop `json:"nexthop"`
}

// RoutingPolicyMatch provides a way to select nodes, namespaces, and pods such that
// the packets originating from them will be subjected to routing policies.
// +kubebuilder:validation:MinProperties=1
type RoutingPolicyMatch struct {
	// NodeSelector matches the source packets only from the node(s) whose label
	// matches this definition. This field is optional.
	NodeSelector metav1.LabelSelector `json:"nodeSelector,omitempty"`
	// NamespaceSelector matches the source packets only from the namespace(s) whose label
	// matches this definition. This field is optional.
	NamespaceSelector metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	// PodSelector matches the source packets only from the pods whose label
	// matches this definition. This field is optional, and in case it is not set:
	// results in matching packets from all the pods in the namespace(s)
	// matched by the NamespaceSelector. In case it is set: is intersected with
	// the NamespaceSelector, thus matching the packets from the pods
	// (in the namespace(s) already matched by the NamespaceSelector) which
	// match this pod selector.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`
	// IPBlock matches the source packets from the pods with IP address in the specified
	// CIDR block. Use 0.0.0.0/0 to match all pods. This field is optional.
	// +optional
	IPBlock networkingv1.IPBlock `json:"ipBlock,omitempty"`
}

// RoutingPolicyNextHop specifies the next-hop IP address for the policy route.
type RoutingPolicyNextHop struct {
	//  NextHopIPs is the list of next-hop IP addresses for this route. With more than
	// one IP address, ECMP will kick in and one of the IP address will be selected
	// based on the 5-tuple hashing of the packet header. Currently, only one IP address
	// is supported. Furthermore, this IP address must be an overlay address, i.e., an
	// address within the OVN Logical Topology.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:Format=ipv4
	NextHopIPs []string `json:"ips,omitempty"`
	// NextHopDevices is a list of device, e.g. switch port, for this route
    NextHopDevices []*NetworkDevice `json:"devices,omitempty"`
}

type NetworkDevice struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +resource:path=adminpolicybasedroute
//+kubebuilder:object:root=true

// AdminPolicyBasedRouteList contains a list of AdminPolicyBasedRoute
type AdminPolicyBasedRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdminPolicyBasedRoute `json:"items"`
}
```

#### Example: Partitioned Internet Access

Below is an example of an `AdminPolicyBasedRoute` manifest used to partition internet egress for a namespace, forcing all traffic to a particular next hop. This allows administrators to direct the internet-bound traffic from pods in a given namespace through a specific gateway.

```yaml
apiVersion: k8s.ovn.org/v1beta1
kind: AdminPolicyBasedRoute
metadata:
  name: internet-egress-namespace1
spec:
  networkSelectors:
    - networkSelectionType: NetworkAttachmentDefinitions
      networkAttachmentDefinitionSelector:
        namespaceSelector:
          matchLabels: {}   # Empty selector will select all namespaces
        networkSelector:
          matchLabels:
            name: ovn-primary
  policies:
    - from:
        namespaceSelector:
          matchLabels:
            name: namespace1
        # may add nodeSelector and podSelector as well
      to:
	    cidr: 0.0.0.0/0
		except:
          - 10.0.0.0/8
      nextHop:
        ips:
          - "10.128.0.254"      # Next hop IP, typically the overlay IP of internet gateway pod
```

#### Example: Allowing Istio Health Check Traffic

```yaml
apiVersion: k8s.ovn.org/v1beta1
kind: AdminPolicyBasedRoute
metadata:
  name: istio-healthcheck-route
spec:
  networkSelectors:
    - networkSelectionType: NetworkAttachmentDefinitions
      networkAttachmentDefinitionSelector:
        namespaceSelector:
          matchLabels: {}   # Empty selector will select all namespaces
        networkSelector:
          matchLabels:
            name: ovn-primary
  policies:
    - from:
	    ipBlock:
		  cidr: 0.0.0.0/0
      to:
        cidr: 169.254.7.127/32
      nextHop:
        devices:
		  - name: k8s-{{NodeName}}
		    type: LogicalSwitchPort
```

### Implementation Details

#### Controller

A new `AdminPolicyBasedRouteController` will be added to `ovnkube-master` for central mode, and `ovnkube-node` for interconnect mode.

The controller will mainly:

1. Watch `AdminPolicyBasedRoute` CRs for CRUD operations
2. Reconcile changes into OVN Northbound DB as `Logical_Router_Policy` entries
3. Watch Pod, Namespace, and Node events to maintain Address Sets
4. Handle special cases for interconnect mode and management port routing
5. Periodically sync between CRs and OVN NB, and keep address sets updated

#### Core Components

**1. Controller Initialization (`go-controller/pkg/ovn/controller/adminpbr/`):**

```go
type Controller struct {
	controllerName string
	util.NetInfo
    // Kubernetes clients
    kube              kubernetes.Interface
    apbrClient        apbrclientset.Interface
    
    // OVN clients
    nbClient          libovsdbclient.Client
    
    // Caches
	internalState       *syncmap.SyncMap[*adminpbrState]
    adminpbrQueue       workqueue.TypedRateLimitingInterface[string]
	adminpbrLister      adminbprlister.AdminPolicyBasedRouteLister
	adminpbrCacheSynced cache.InformerSynced

    podLister           corev1listers.PodLister
	podCacheSynced      cache.InformerSynced
	podQueue            workqueue.TypedRateLimitingInterface[string]

	nodeLister           corev1listers.NodeLister
	nodeCacheSynced      cache.InformerSynced
	nodeQueue            workqueue.TypedRateLimitingInterface[string]

	nsLister             corev1listers.NamespaceLister
	nsCacheSynced        cache.InformerSynced
	nsQueue              workqueue.TypedRateLimitingInterface[string]

    nadLister            nadlisterv1.NetworkAttachmentDefinitionLister
	nadSynced            cache.InformerSynced

    // Address Set Manager
    addressSetFactory addressset.AddressSetFactory
    
    // Interconnect zone info (if enabled)
    zoneHandler       *interconnect.ZoneHandler
	isPodScheduledinLocalZone func(*corev1.Pod) bool
}
```

**2. Event Handling and Reconciliation Loop:**

When an `AdminPolicyBasedRoute` CR is created or updated:

```
1. Validate the CR spec (network attachment name, selectors, nexthop IPs, etc.)
2. For each policy rule in the CR:
   a. Build OVN match expression from From selectors (node/namespace/pod)
   b. Build Address Sets for dynamic pod/namespace/node matching
   c. Find what nexthop IP to use in the policy
      - If nexthop is an IP and it's in remote zone, find the node's transit port IP
	  - If nexthop device is provided, find out the IP of the device
   d. Add exclusion list to policy if there is any
   e. Create/Update Logical_Router_Policy in OVN NBDB
3. Update CR status with success/error messages
```

When Pod/Namespace/Node events occur:

```
1. Identify which AdminPolicyBasedRoute CRs are affected by label matching
2. Update the corresponding OVN Address Sets
3. OVN automatically applies updated routing to matching packets
```

**3. Address Set Management:**

For each `RoutingPolicyMatch` selector combination, the controller maintains OVN Address Sets containing the IPs of matching resources, the OVN Address Sets will be associated with `RoutingPolicyMatch` by placing `AdminPolicyBasedRoute` name and hash of `RoutingPolicyMatch` into ExternalIDs in Address Sets.

Address Sets are dynamically updated when:
- Pods matching the selector are added/removed/updated
- Namespaces matching the selector are added/removed/updated  
- Nodes matching the selector are added/removed/updated

**4. OVN Logical Router Policy Creation:**

Each `RoutingPolicyRule` is translated to an OVN `Logical_Router_Policy` with:

- **Priority:** 80 (processed after OVN-K8s internal rules)
- **Match:** Constructed from From and To fields
  ```
  match: "ip4.src == {$uuid} && ip4.dst == <to.cidr> && ip4.dst != {exclusion_list}"
  ```
- **Action:** `reroute`
- **Nexthops:** Resolved IP address(es) from NextHop field
- **External IDs:** Tagged with `{"k8s.ovn.org/owner": "/<cr-name>", "hash": "hashed uuid"}`

Note: Hash will be calculated based on all fields of the CR

**5. Nexthop Resolution:**

The controller implements special nexthop handling:
a) **NextHopDevices:**
   - Validate the device type is `LogicalSwitchPort`, other types are not supported.
   - Replace the templating variables with actual values if there is any in the device name.
   - Find the device by name, then get the IP and generate route policy with the IP as nexthop. 
   
b) **Interconnect Remote Zone Handling:**
   - When nexthop IP is in a remote zone (in interconnect mode)
   - Controller replaces nexthop with the transit switch port IP to that zone
```
+-------------------------------+           +-----------------------------+
|         Node A                |           |          Node B             |
| +--------------------------+  |           | +------------------------+  |
| |      Source Pod          |  |           | |   Router Pod           |  |
| | IP: 10.1.1.10            |  |           | | IP: 10.2.2.20          |  |
| +-----------+--------------+  |           | +------------------------+  |
|             |                 |           |          |                  |
|   src==10.1.1.10              |           |          |                  |
|   nexthop==10.2.2.20          |           |          |                  |
|             |                 |           |          |                  |
|       Logical_Router_Policy   |           |          |                  |
|   (match: traffic from source pod)        |          |                  |
|             |                             |          |                  |
| reroute via transit switch port  +-------------------|----------------> |
| (nexthop: Transit Switch Port IP of Node B)          |                  |
|             |                                        |                  |
+-------------|----------------------------------------|------------------+
              |                                        |
       +-------------+                                 |
       | Transit     |                                 |
       | Switch Port |----------------------------------
       | of Node B   |
       +-------------+

**6. Cleanup and Garbage Collection:**

When an `AdminPolicyBasedRoute` CR is deleted:
1. Remove all associated `Logical_Router_Policy` entries from OVN NBDB
2. Delete all associated Address Sets
3. Update status and emit events

When an `AdminPolicyBasedRoute` CR is updated:
1. Reconcile the latest manifest, update OVN with CR name and hash values
2. Go through the OVN records by CR name and hash, remove records with stale hash values

#### Configuration

Add a new feature gate flag `--enable-admin-pbr` (default: true) to turn on/off the feature.

### Testing Details

* Unit tests coverage
- General unit test cases for functions
- Ginkgo BDD test cases to validate controller behaviors
- Make sure status is updated as expected.

* E2E Testing details
- Create 3 pods to simulate source, destination and nexthop, configure NAT on the nexthop
- Apply a `AdminPolicyBasedRoute` CR to redirect packets from source to go through nexthop to reach destination
- Run ping from source, and run tcpdump on nexthop pod, packets are expected to be captured
- Change the pod selector in CR to disqualify the source pod, run above test again, packets should not be captured

### Documentation Details
* ovn-kubernetes.io will be updated with a user guide

## Risks, Known Limitations and Mitigations

## OVN-Kubernetes Version Skew
Targeting version 1.4.

## Backwards Compatibility

## Alternatives


## References
- https://man7.org/linux/man-pages/man5/ovn-nb.5.html#Logical_Router_Policy_TABLE
- https://ambientmesh.io/docs/operations/troubleshooting/#readiness-probes-fail-with-ztunnel
