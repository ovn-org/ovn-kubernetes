# OKEP-4380: Network QoS Support

* Issue: [#4380](https://github.com/ovn-kubernetes/ovn-kubernetes/issues/4380)

## Problem Statement

The workloads running in Kubernetes, using OVN-Kubernetes (OVN-K8s) as a network plugin, might have
requirements in how their network traffic must be handled/differentiated compared to other workloads
within the same namespace or different namespaces. For example video streaming application needs low
latency and jitter whereas storage applications can tolerate packet loss. Hence, enforcing fair
share use of NIC's bandwidth on a K8s Node is essential in meeting these SLAs to provide better  
service quality.

Furthermore, some services in-zone (Physical Gateway to Internet) or in-cluster (Internet Gateway Pods)
would like to identify network traffic to provide differentiated services. To achieve this, it is
necessary to mark packets on the wire, enabling these services to apply differential treatment.  OVN natively
supports DSCP (Differentiated Services Code Point), 6-bit field in IP header, marking on IP
packets based on arbitrary match criteria for a logical switch.

## Goals

1. Provide a mechanism for users to set DSCP marking on egress east/west (pod to pod overlay)  
   traffic and egress north/south (pod to external underlay) traffic.
2. Provide a mechanism for users to set Metering on egress east/west and egress north/south
   traffic on the NIC on the K8s Node.
3. Provide above mechanisms on all networks attached to K8s Pods.

## Non-Goals

1. Ingress Network QoS.
2. Consolidating with current `kubernetes.io/egress-bandwidth` and `kubernetes.io/ingress-bandwidth`
   annotations. Nonetheless, the work done here does not interfere with the current bandwidth    
   enforcement mechanisms.
3. How the DSCP marking is handled by the physical network fabric is out-of-scope. It could be that
   the fabric could completely ignore the marking.

## Introduction

There are several techniques to ensure Quality of Service (QoS) for workloads running in a
Kubernetes (K8s) cluster. One method involves traffic policing or metering, where traffic is
regulated on the NIC based on a configured rate and burst limit. Any traffic exceeding the limit is
dropped. This metering capability is natively supported by OVN through OVS Meters. Another method is
traffic shaping, where excess traffic is buffered and transmitted later when bandwidth becomes
available, at the cost of higher latency. However, this traffic shaping technique is not supported
by OVN and, consequently, cannot be implemented in the OVN-K8s Network plugin. Since network  
virtualization in OVN occurs on the K8s node (also known as the OVN chassis), bandwidth
enforcement for matched traffic occurs on the node itself. This allows OVN-K8s to provide API to
regulate NIC's bandwidth between the workloads running on the same or different namespaces
within a K8s node, even before the traffic reaches network fabric through the overlay.
Additionally, the network fabric can do its own regulation of network bandwidth, however how it
is done is outside the scope of this proposal.

Another strategy for providing differential treatment to workload network traffic involves marking
packets using DSCP (a 6-bit field in the IP header). These marked packets can then be handled
differently by in-zone and in-cluster services. OVN supports this packet marking capability through
OVS, allowing traffic to be classified based on specific match criteria. OVN marks the inner
packet's IP header. So, the marking appears inside the GENEVE tunnel. There are ways to transfer
this marking to outer header and influence how the underlay network fabric should handle such
packets, however that is outside the scope of this proposal.

Kubernetes offers partial support for QoS features through annotations such
as `kubernetes.io/egress-bandwidth` and `kubernetes.io/ingress-bandwidth` at the Pod interface
level. However, these annotations lack fine-grained control, as they cannot target specific types of
traffic (e.g., video streaming) on an interface. The Network Plumbing Working Group (NPWG) has
extended these annotations to secondary networks, but they remain limited to interface-level
configurations without options for selecting a particular traffic flow. Additionally, Kubernetes
currently lacks an API for DSCP packet marking.

To address these limitations, this proposal introduces a NetworkQoS API that enables fine-grained
bandwidth enforcement and packet marking across all interfaces within a Pod.

The proposed solution works out-of-the box for the case where a node belongs to a single tenant and
the tenant's namespace admin sets the NetworkQos for all the Pods landing on that Node. Say, a node
is shared by more than one tenant (not a common scenario) and two tenant namespace admins compete with
each other on setting the egress bandwidth limit. In this case, the K8s provider will have to resort
to AdmissionWebhooks to either restrict the values that the tenant namespace admin can use or inject
a default NetworkQos object in the respective namespaces with predefined values.

## User-Stories/Use-Cases

#### Story 1

```text
+---------------------+                                                                                                    
|NS1/Pod1 (paid user) +--DSCP:20                                                                                           
+---------------------+     |         .-----.----------------------------.-----.    +--------------------+      .-------.  
                            +------> ;       :     Overlay Traffic      ;       :   |NS3/Internet Gateway|     /         \ 
                                     :       ;  Various DSCP marking    :       --->|   Forward + SNAT   +--->( Internet  )
                         DSCP:11--->  \     /                            \     /    |    to Underlay     |     `.       ,' 
+---------------------+     |          `---'------------------------------`---'     +--------------------+       `-----'   
|NS1/Pod2 (free user) +-----+                   .-------------------.                                                      
+---------------------+                   _.---'                     `----.                                                
                                         /        Physical Underlay        \                                               
                                        (     (unaware of DSCP marking)     )                                              
                                         `.                               ,'                                               
                                           `----.                   _.---'                                                 
                                                 `-----------------'                                                                                                                                                    
```

As a K8s Namespace Admin, I want to configure DSCP marking for egress east-west overlay traffic so that the
packet marking is carried from the source overlay pod to destination overlay pod, so that on the destination pod can
treat the incoming traffic differently.

For example: In the diagram above, Say Pod1 is a paid cloud gaming user and Pod2 is a free cloud gaming user.  I want
these two Pods to be treated differently by the InternetGateway application Pod. The packets leaving Pods Pod1 and Pod2
will be marked with DSCP value of 20 and 11 respectively. This marking will be retained on the overlay across the
fabric and arrive at the InternetGateway pod where the packets from the free user will be subjected queueing during peak times as
compared to the paid user who will not be subjected to any sort of queueing.

The namespace admin have the flexibility to define how they utilize the 6-bit DSCP field to meet their specific needs
for client/server traffic. They own both the client and server applications.

The end user is an individual who wants to play games in the cloud. The individual is a consumer of gaming services.

#### Story 2

As a K8s Namespace Admin, I want to enforce egress bandwidth limit (rate and burst) on the east/west and north/south
traffic emanating from the Pods on the same K8s node so that they use the underlying NIC fairly.

In the same diagram above, I want to limit the egress bandwidth from Pod2 where a free-user is present to not exceed
1Mbps rate and 1Mbps burst. However, the paid-user might not have any such limitations.

#### Story 3

As a K8s Namespace Admin, I want to define a catch-all NetworkQoS for all my Pods, and then have a more specific
NetworkQos for few Pods. As such, I need priorities to define this.

In the above diagram, I want all the Internet bound traffic from NS1/Pod1 and NS1/Pod2 to be bandwidth limited to
10Mbps. However, from the same set of Pods I want all the AWS S3 related traffic to be bandwidth limited to 100Mbps.

So, the namespace admin can create a catch-all NetworkQoS at priority 1 for all the Pods in NS1 heading
towards the Internet and create another NetworkQos at priority 2 to increase the egress bandwidth limit to AWS S3 IPs.

## Proposed Solution

The current EgressQoS is a namespace-scoped feature that enables DSCP marking for pod's egress
traffic directed towards dstCIDR. A namespace supports only one EgressQoS resource, named default
(any additional EgressQoS resources will be ignored). This enhancement proposes a replacement for
EgressQoS. By introducing a new CRD `NetworkQoS`, users could specify a DSCP value for packets  
originating from pods on a given namespace heading to a specified Namespace Selector, Pod
Selector, CIDR, Protocol and Port. This also supports metering for the packets by specifying  
bandwidth parameters `rate` and/or `burst`. The `priority` field enables one to define overlapping
rules such that the rule with higher priority (match could be generic) will override the rule
with lower priority (match will be specific. See: Story-3).

The CRD will be Namespaced, with multiple resources
allowed per namespace. The resources will be watched by OVN-K8s, which in turn will configure
OVN's [QoS Table](https://man7.org/linux/man-pages/man5/ovn-nb.5.html#NetworkQoS_TABLE). The  
`NetworkQoS` also has `status` field which is populated by OVN-K8s which helps users to identify
whether NetworkQoS rules are configured correctly in OVN or not.

### API Details

* A new API `NetworkQoS` under the `k8s.ovn.org/v1alpha1` group will be added to  
  `go-controller/pkg/crd/networkqos/v1alpha1`. This would be a namespace-scoped CRD:

```go
import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=networkqoses
// +kubebuilder::singular=networkqos
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.status"
// +kubebuilder:subresource:status
// NetworkQoS is a CRD that allows the user to define a DSCP marking and metering
// for pods ingress/egress traffic on its namespace to specified CIDRs,
// protocol and port. Traffic belong these pods will be checked against
// each Rule in the namespace's NetworkQoS, and if there is a match the traffic
// is marked with relevant DSCP value and enforcing specified policing
// parameters.
type NetworkQoS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Spec   `json:"spec,omitempty"`
	Status Status `json:"status,omitempty"`
}

// Spec defines the desired state of NetworkQoS
type Spec struct {
	// networkSelector selects the networks on which the pod IPs need to be added to the source address set.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="networkSelector is immutable"
	NetworkSelectors crdtypes.NetworkSelectors `json:"networkSelectors,omitempty"`

	// podSelector applies the NetworkQoS rule only to the pods in the namespace whose label
	// matches this definition. This field is optional, and in case it is not set
	// results in the rule being applied to all pods in the namespace.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`

	// priority is a value from 0 to 100 and represents the NetworkQoS' priority.
	// QoSes with numerically higher priority takes precedence over those with lower.
	// +kubebuilder:validation:Maximum:=100
	// +kubebuilder:validation:Minimum:=0
	Priority int `json:"priority"`

	// egress a collection of Egress NetworkQoS rule objects. A total of 20 rules will
	// be allowed in each NetworkQoS instance. The relative precedence of egress rules
	// within a single NetworkQos object (all of which share the priority) will be
	// determined by the order in which the rule is written. Thus, a rule that appears
	// first in the list of egress rules would take the lower precedence.
	// +kubebuilder:validation:MaxItems=20
	Egress []Rule `json:"egress"`
}

type Rule struct {
	// dscp marking value for matching pods' traffic.
	// +kubebuilder:validation:Maximum:=63
	// +kubebuilder:validation:Minimum:=0
	DSCP int `json:"dscp"`

	// classifier The classifier on which packets should match
	// to apply the NetworkQoS Rule.
	// This field is optional, and in case it is not set the rule is applied
	// to all egress traffic regardless of the destination.
	// +optional
	Classifier Classifier `json:"classifier"`

	// +optional
	Bandwidth Bandwidth `json:"bandwidth"`
}

type Classifier struct {
	// +optional
	To []Destination `json:"to"`

	// +optional
	Ports []*Port `json:"ports"`
}

// Bandwidth controls the maximum of rate traffic that can be sent
// or received on the matching packets.
type Bandwidth struct {
	// rate The value of rate limit in kbps. Traffic over the limit
	// will be dropped.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=4294967295
	// +optional
	Rate uint32 `json:"rate"`

	// burst The value of burst rate limit in kilobits.
	// This also needs rate to be specified.
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=4294967295
	// +optional
	Burst uint32 `json:"burst"`
}

// Port specifies destination protocol and port on which NetworkQoS
// rule is applied
type Port struct {
	// protocol (tcp, udp, sctp) that the traffic must match.
	// +kubebuilder:validation:Pattern=^TCP|UDP|SCTP$
	// +optional
	Protocol string `json:"protocol"`

	// port that the traffic must match
	// +kubebuilder:validation:Minimum:=1
	// +kubebuilder:validation:Maximum:=65535
	// +optional
	Port *int32 `json:"port"`
}

// Destination describes a peer to apply NetworkQoS configuration for the outgoing traffic.
// Only certain combinations of fields are allowed.
// +kubebuilder:validation:XValidation:rule="!(has(self.ipBlock) && (has(self.podSelector) || has(self.namespaceSelector)))",message="Can't specify both podSelector/namespaceSelector and ipBlock"
type Destination struct {
	// podSelector is a label selector which selects pods. This field follows standard label
	// selector semantics; if present but empty, it selects all pods.
	//
	// If namespaceSelector is also set, then the NetworkQoS as a whole selects
	// the pods matching podSelector in the Namespaces selected by NamespaceSelector.
	// Otherwise it selects the pods matching podSelector in the NetworkQoS's own namespace.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty" protobuf:"bytes,1,opt,name=podSelector"`

	// namespaceSelector selects namespaces using cluster-scoped labels. This field follows
	// standard label selector semantics; if present but empty, it selects all namespaces.
	//
	// If podSelector is also set, then the NetworkQoS as a whole selects
	// the pods matching podSelector in the namespaces selected by namespaceSelector.
	// Otherwise it selects all pods in the namespaces selected by namespaceSelector.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty" protobuf:"bytes,2,opt,name=namespaceSelector"`

	// ipBlock defines policy on a particular IPBlock. If this field is set then
	// neither of the other fields can be.
	// +optional
	IPBlock *networkingv1.IPBlock `json:"ipBlock,omitempty" protobuf:"bytes,3,rep,name=ipBlock"`
}

// Status defines the observed state of NetworkQoS
type Status struct {
	// A concise indication of whether the NetworkQoS resource is applied with success.
	// +optional
	Status string `json:"status,omitempty"`

	// An array of condition objects indicating details about status of NetworkQoS object.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=networkqoses
// +kubebuilder::singular=networkqos
// NetworkQoSList contains a list of NetworkQoS
type NetworkQoSList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkQoS `json:"items"`
}
```

### Implementation Details

The new controller is introduced in OVN-Kubernetes which would watch `NetworkQoS`, `Pod` and `Node`
objects, which will create the relevant NetworkQoS objects and attach them to all the node local
switches in the cluster in OVN - resulting in the necessary flows to be programmed in OVS.

In order to not create an OVN NetworkQoS object per pod in the namespace, the controller will also
manage AddressSets. For each QoS rule specified in a given `NetworkQoS` it'll create an
AddressSet, adding only the pods whose label matches the PodSelector to it, making sure that  
new/updated/deleted matching pods are also added/updated/deleted accordingly. Rules that do not  
have a PodSelector will leverage the namespace's AddressSet.

Similarly, when `NetworkQoS` is created for Pods secondary network, OVN-K8s must create a new
AddressSet for every QoS rule. When no pod selector is specified, then it must contain all the  
pod's IP addresses that belong to the namespace and selected network. If only a set of pods are  
chosen via podSelector, then it must have IP addresses only for chosen pod(s).

For example, assuming there's a single node `node1` and the following `NetworkQoS` (maps to the Story-1 above)
is created:

```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1alpha1
metadata:
  name: qos-external-paid
  namespace: games
spec:
  podSelector:
    matchLabels:
      user-type: paid
  priority: 1
  egress:
    - dscp: 20
      classifier:
        to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
            - 10.0.0.0/8
            - 172.16.0.0/12
            - 192.168.0.0/16

---
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1alpha1
metadata:
  name: qos-external-free
  namespace: games
spec:
  podSelector:
    matchLabels:
      user-type: free
  priority: 2
  egress:
    - dscp: 11
      classifier:
        to:
          - ipBlock:
              cidr: 0.0.0.0/0
              except:
                - 10.0.0.0/8
                - 172.16.0.0/12
                - 192.168.0.0/16
```

the equivalent of:

```bash
ovn-nbctl qos-add node1 to-lport 10020 "ip4.src == <games-qos-external-paid address set> && ip4.dst != {10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16}" dscp=20
ovn-nbctl qos-add node1 to-lport 10040 "ip4.src == <games-qos-external-free address set> && ip4.dst != {10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16}" dscp=11
```

will be executed. The math for the priority is as described below:

1. we want to save the first 10K OVN priorities for future use.
2. we evaluate the priority based on the fact that we allow only 20 rules per QoS object, and we use the index of the rule within the object
   So, 10020 was derived like so => 10000 + NetworkQoS.priority * 20 + index(rule) => 10000 + 1 * 20 + 0 => 10020
   S0, 10040 was derived like so => 10000 + 2 * 20 + 0

Creating a new Pod in games namespace that matches the podSelector in either `qos-external-paid` or `qos-external-free`
results in its IPs being added to the corresponding Address Set.

Following example maps to the Story-2 above. It updates the above NetworkQoS objects to include the bandwidth fields. 
```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1alpha1
metadata:
  name: qos-external-free
  namespace: games
spec:
  podSelector:
    matchLabels:
      user-type: free
  priority: 2
  egress:
    - dscp: 11
      bandwidth:
        burst: 1000000 # in kbps
        rate: 1000000  # in kbps      
      classifier:
        to:
          - ipBlock:
              cidr: 0.0.0.0/0
              except:
                - 10.0.0.0/8
                - 172.16.0.0/12
                - 192.168.0.0/16
```

In the above `qos-external-free` NetworkQoS example, all the pods in games namespace with `user-type: free` label
will have bandwidth limited to specified burst/rate towards the Internet. Such traffic will also have DSCP marking of
11. The equivalent of:

```bash
ovn-nbctl qos-add node1 to-lport 10040 "ip4.src == <games-qos-external-free address set> && ip4.dst != {10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16}" rate=20000 burst=100 dscp=11
```

will be executed.

In addition, the controller will watch nodes to decide if further updates are needed, for example:
when another node `node2` joins the cluster, the controller will attach the existing `NetworkQoS`
object to its node local switch.

The `NetworkQoS` is supported on pod's secondary networks. That may also be a User Defined Network.
Consider the following example:

```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1alpha1
metadata:
  name: qos-external-free
  namespace: games
spec:
  networkSelectors:
    - networkSelectionType: NetworkAttachmentDefinitions
      networkAttachmentDefinitionSelector:
        namespaceSelector:
          matchLabels: {}   # Empty selector will select all namespaces
        networkSelector:
          matchLabels:
            name: ovn-storage
  priority: 2
  egress:
    - dscp: 11
      classifier:
        to:
          - ipBlock:
              cidr: 0.0.0.0/0
        ports:
        - protocol: TCP
          port: 80
        - protocol: TCP
          port: 443
```

This creates a new AddressSet adding default namespace pod(s) IP associated with ovn-storage
secondary network, using NAD. The equivalent of:

```bash
ovn-nbctl qos-add node1 to-lport 10040 "ip4.src == <games_ovn-storage_network address set> && ip4.dst == 0.0.0.0/0" dscp=11
```

will be executed.

IPv6 will also be supported, given the following `NetworkQoS`:

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  name: default
  namespace: default
spec:
  priority: 3
  egress:
    - dscp: 48
      classifier:
        to:
          - ipBlock:
              cidr: 2001:0db8:85a3:0000:0000:8a2e:0370:7330/124
```

and a single pod with the IP `fd00:10:244:2::3` in the namespace, the controller will create the
relevant NetworkQoS object that will result in a similar flow to this on the pod's node:

```bash
 cookie=0x6d99cb18, duration=63.310s, table=18, n_packets=0, n_bytes=0, idle_age=63, priority=555,ipv6,metadata=0x4,ipv6_src=fd00:10:244:2::3,ipv6_dst=2001:db8:85a3::8a2e:370:7330/124 actions=mod_nw_tos:192,resubmit(,19)
```

### Testing Details

* Unit tests coverage

* Validate NetworkQoS `status` fields are populated correctly.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is marked with the correct DSCP value
  by creating and deleting `NetworkQoS`, setting up src pods and destination pods.
  * Traffic to the all targeted pod IPs should be marked.
  * Traffic to the targeted pod IPs, Protocol should be marked.
  * Traffic to the targeted pod IPs, Protocol and Port should be marked.
  * Traffic to an pod IP address not contained in the destination pod selector, Protocol and Port
    should not be marked.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is marked with the correct DSCP value
  by creating and deleting `NetworkQoS`, setting up src pods and host-networked destination pods.
  * Traffic to the specified CIDR should be marked.
  * Traffic to the specified CIDR, Protocol should be marked.
  * Traffic to the specified CIDR, Protocol and Port should be marked.
  * Traffic to an address not contained in the CIDR, Protocol and Port should not be marked.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is enforced with bandwidth limit by
  creating and deleting `NetworkQoS`, setting up src pods and destination pods.
  * Traffic to the all targeted pod IPs should be rate limited with specified bandwidth
    parameters.
  * Traffic to the targeted pod IPs, Protocol should be rate limited with specified bandwidth
    parameters.
  * Traffic to the targeted pod IPs, Protocol and Port should be rate limited with specified
    bandwidth parameters.
  * Traffic to an pod IP address not contained in the destination pod selector, Protocol and Port
    should not be rate limited with specified bandwidth parameters.

### Documentation Details

To be discussed.

## Risks, Known Limitations and Mitigations

## OVN-Kubernetes Version Skew

To be discussed.

## Alternatives

N/A

## References
