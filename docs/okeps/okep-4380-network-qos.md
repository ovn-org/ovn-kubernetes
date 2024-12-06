# OKEP-4380: Network QoS Support

* Issue: [#4380](https://github.com/ovn-org/ovn-kubernetes/issues/4380)

## Problem Statement

The workloads running in Kubernetes using OVN-Kubernetes as a networking backend might have different requirements in handling network traffic. For example video streaming application needs low latency and jitter whereas storage application can tolerate with packet loss. Hence NetworkQoS is essential in meeting these SLAs to provide better service quality.

The workload taffic can be either east west (pod to pod traffic) or north south traffic (pod to external traffic) types in a Kubernetes cluster which is limited by finite bandwidth. So NetworkQoS must ensure high priority applications get the necessary NetworkQoS marking so that it can prevent network conjestion.

## Goals

- Provide a mechanism for users to set DSCP (Differentiated Services Code Point) marking on east west and north south traffic.
- Provide a mechanism for users to set Metering on east west and north south traffic.

## Non-Goals

- Ingress Network QoS.

- Consolidating with current `kubernetes.io/egress-bandwidth` and `kubernetes.io/ingress-bandwidth` annotations.
Nonetheless, the work done here does not interfere with the current bandwidth NetworkQoS mechanism.

- The DSCP marking does not need to be handled or acted upon by OVN-Kubernetes, just added to selected headers.

## User-Stories/Use-Cases
#### Story 1

As a user of OVN-Kubernetes, I want to configure DSCP values for east west and north south traffic.

#### Story 2

As a user of OVN-Kubernetes, I want to apply bandwidth limit (rate and burst) for east west and north south traffic.

## Proposed Solution

The current EgressQoS is a namespaced-scoped feature which enables DSCP marking for the pods egress traffic heading towards dstCIDR. A namespace supports having only one EgressQoS resource named default (other EgressQoSes will be ignored).
By introducing a new CRD `NetworkQoS`, users could specify a DSCP value for packets originating from pods on a given namespace heading to a specified Namespace Selector, Pod Selector, CIDR, Protocol and Port. This also supports metering for the packets by specifying bandwidth parameters `rate` and/or `burst`.
The CRD will be Namespaced, with multiple resources allowed per namespace.
The resources will be watched by ovn-k, which in turn will configure OVN's [QoS Table](https://man7.org/linux/man-pages/man5/ovn-nb.5.html#NetworkQoS_TABLE).
The `NetworkQoS` also has `status` field which is populated by ovn-k which helps users to identify whether NetworkQoS rules are configured correctly in OVN or not.

ovn-controller has this configuration, ovn-encap-tos:

```text
external_ids:ovn-encap-tos

ovn-encap-tos indicates the value to be applied to OVN tunnel interface’s option:tos as specified in the Open_vSwitch database Interface table. Please refer to Open VSwitch Manual for details.

   options : tos: optional string
          Optional. The value of the ToS bits to be set on the encapsulat‐
          ing packet. ToS is interpreted as DSCP and ECN  bits,  ECN  part
          must be zero. It may also be the word inherit, in which case the
          ToS will be copied from the inner packet if it is IPv4  or  IPv6
          (otherwise  it  will be 0). The ECN fields are always inherited.
          Default is 0.
```

which when set to inherit will take the marking from the inner header and apply it to the GENEVE packet. Therefore, east west traffic within the cluster can benefit
from this API and not just the north south traffic.

### API Details

* A new API `NetworkQoS` under the `k8s.ovn.org/v1` version will be added to `go-controller/pkg/crd/networkqos/v1`.
This would be a namespace-scoped CRD:

```go
import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// is marked with relevant DSCP value and enforcing specified policing/shaping
// parameters.
type NetworkQoS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   Spec   `json:"spec,omitempty"`
	Status Status `json:"status,omitempty"`
}

// Spec defines the desired state of NetworkQoS
type Spec struct {
	// netAttachRefs points to a list of NetworkAttachmentDefinition object,
	// to which the QoS Rules will be applied. The netAttachRefs can be of
	// type Layer-3, Layer-2 or Localnet. If not specified, the default cluster-wide
	//  network will be used.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="netAttachRefs is immutable"
	// +kubebuilder:validation:XValidation:rule="self.all(nad, nad.kind == 'NetworkAttachmentDefinition')", message="\"kind\" of netAttachRef must be \"NetworkAttachmentDefinition\""
	NetworkAttachmentRefs []corev1.ObjectReference `json:"netAttachRefs,omitempty"`

	// podSelector applies the NetworkQoS rule only to the pods in the namespace whose label
	// matches this definition. This field is optional, and in case it is not set
	// results in the rule being applied to all pods in the namespace.
	// +optional
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`

	// egress a collection of Egress NetworkQoS rule objects
	Egress []Rule `json:"egress"`
}

type Rule struct {
	// priority The NetworkQoS rule’s priority. Rules with numerically higher
	// priority take precedence over those with lower. If two NetworkQoS
	// rules with the same priority both match, then the one
	// actually applied to a packet is undefined.
	// +kubebuilder:validation:Maximum:=32767
	// +kubebuilder:validation:Minimum:=0
	Priority int `json:"priority"`

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
	Port Port `json:"port"`
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
	Port int32 `json:"port"`
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

* Ensure backward compatibility support available for `EgressQoS` API.

### Implementation Details

The new controller is introduced in OVN-Kubernetes which would watch `NetworkQoS` in addition to `EgressQoS`, `Pod` and `Node` objects, which will create the relevant NetworkQoS objects and attach them to all of the node local switches in the cluster in OVN - resulting in the necessary flows to be programmed in OVS.

In order to not create an OVN NetworkQoS object per pod in the namespace, the controller will also manage AddressSets.
For each QoS rule specified in a given `NetworkQoS` it'll create an AddressSet, adding only the pods whose label matches the PodSelector to it, making sure that new/updated/deleted matching pods are also added/updated/deleted accordingly. Rules that do not have a PodSelector will leverage the namespace's AddressSet.

Similarly when `NetworkQoS` is created for Pods secondary network, OVNK must create a new AddressSet for every QoS rule. When no pod selector is specified, then it must contain all of the pod's IP addresses that belong to the namespace and selected network. If only a set of pods are chosen via podSelector, then it must have IP addresses only for chosen pod(s).

For example, using LGW mode and assuming there's a single node `node1` and the following `NetworkQoS` is created:

```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: qos-external-foo
  namespace: default
spec:
  podSelector:
    matchLabels:
      priority: Critical
  egress:
  - priority: 100
    dscp: 59
    classifier:
      port:
        protocol: TCP
        port: 22
      to:
      - ipBlock:
          cidr: 1.2.3.4/32
  - priority: 10
    dscp: 46
    classifier:
      to:
      - ipBlock:
          cidr: 1.2.3.4/32
---
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: qos-external-bar
  namespace: default
spec:
  egress:
  - priority: 0
    dscp: 30
    classifier:
      to:
      - ipBlock:
          cidr: 0.0.0.0/0
```
the equivalent of:
```bash
ovn-nbctl qos-add node1 to-lport 100 "ip4.src == <default_ns-qos-external-foo address set> && ip4.dst == 1.2.3.4/32 && tcp && tcp.dst == 22" dscp=59
ovn-nbctl qos-add node1 to-lport 10 "ip4.src == <default_ns-qos-external-foo address set> && ip4.dst == 1.2.3.4/32" dscp=46
ovn-nbctl qos-add node1 to-lport 0 "ip4.src == <default_ns address set> && ip4.dst == 0.0.0.0/0" dscp=30
```
will be executed.
The podSelector in `qos-external-foo` NetworkQoS object is applied for both first and second NetworkQoS rules. So creating a new Pod that matches this selector in the namespace results in its IPs being added to corresponding Address Set.
The `qos-external-bar` doesn't have podSelector, so NetworkQoS rule specified in this object is applied to all pods in the `default` namespace.

```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: qos-storage-traffic
  namespace: games
spec:
  podSelector:
    matchLabels:
      app: storage-client
  egress:
  - priority: 40
    dscp: 59
    bandwidth:
      burst: 100
      rate: 20000
    classifier:
      port:
        protocol: TCP
        port: 471
      to:
      - podSelector:
          matchLabels:
            app: storage-target
        namespaceSelector:
          matchLabels:
            name: storage-target
---
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: qos-stream-traffic
  namespace: games
spec:
  podSelector:
    matchLabels:
      app: stream-client
  egress:
  - priority: 40
    dscp: 33
    classifier:
      port:
        protocol: UDP
        port: 399
      to:
      - podSelector:
          matchLabels:
            app: stream-client
        namespaceSelector:
          matchLabels:
            name: stream-target
```
In the above `qos-storage-traffic` NetworkQoS example, The tcp traffic with dst port 471 from pods with label `app=storage-client` in `games` namespace will have dscp set to 59 and bandwidth limited to specified burst/rate towards pods having label `app=storage-target` residing on the namespace with label `name=storage-target`.
the equivalent of:
```bash
ovn-nbctl qos-add node1 to-lport 40 "ip4.src == <games_ns-qos-storage-client-pods address set> && ip4.dst == <storage-target_ns-qos-app-storage-target-pods address set> && tcp && tcp.dst == 471" dscp=59 rate=20000 burst=100
```
will be executed.

For `qos-stream-traffic` NetworkQoS example, The udp traffic with dst port 399 from pods with label `app=stream-client` in `games` namespace will have dscp set to 33 towards pods with label `app=stream-client` residing on the namespace with label `name=stream-target`.
the equivalent of:
```bash
ovn-nbctl qos-add node1 to-lport 40 "ip4.src == <games_ns-qos-stream-client-pods address set> && ip4.dst == <stream-target_ns-qos-app-stream-target-pods address set> && tcp && tcp.dst == 399" dscp=33
```
will be executed.

In addition it'll watch nodes to decide if further updates are needed, for example:
when another node `node2` joins the cluster, the controller will attach the existing `NetworkQoS` object to its node local switch.

The `NetworkQoS` is supported on pod's secondary networks. That may also be a User Defined Network. Consider the following example:

```yaml
kind: NetworkQoS
apiVersion: k8s.ovn.org/v1
metadata:
  name: qos-external-bar
  namespace: default
spec:
  netAttachRefs:
  - kind: NetworkAttachmentDefinition
    namespace: default
    name: ovn-storage
  egress:
  - priority: 100
    dscp: 30
    classifier:
      to:
      - ipBlock:
          cidr: 0.0.0.0/0
```
This creates a new AddressSet adding default namespace pod(s) IP associated with ovn-stream secondary network, using NAD.
the equivalent of:
```bash
ovn-nbctl qos-add node1 to-lport 100 "ip4.src == <default_ns_ovn-stream_network address set> && ip4.dst == 0.0.0.0/0" dscp=30
```
will be executed.

IPv6 will also be supported, given the following `NetworkQoS`:
```yaml
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  name: default
  namespace: default
spec:
  egress:
  - priority: 100
    dscp: 48
    classifier:
      to:
      - ipBlock:
          cidr: 2001:0db8:85a3:0000:0000:8a2e:0370:7330/124
```
and a single pod with the IP `fd00:10:244:2::3` in the namespace, the controller will create the relevant NetworkQoS object that will result in a similar flow to this on the pod's node:
```bash
 cookie=0x6d99cb18, duration=63.310s, table=18, n_packets=0, n_bytes=0, idle_age=63, priority=555,ipv6,metadata=0x4,ipv6_src=fd00:10:244:2::3,ipv6_dst=2001:db8:85a3::8a2e:370:7330/124 actions=mod_nw_tos:192,resubmit(,19)
```

### Testing Details

* Unit tests coverage

* Validate NetworkQoS `status` fields are populated correctly.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is marked with the correct DSCP value by creating and deleting `NetworkQoS`, setting up src pods and destination pods.
  * Traffic to the all targeted pod IPs should be marked.
  * Traffic to the targeted pod IPs, Protocol should be marked.
  * Traffic to the targeted pod IPs, Protocol and Port should be marked.
  * Traffic to an pod IP address not contained in the destination pod selector, Protocol and Port should not be marked.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is marked with the correct DSCP value by creating and deleting `NetworkQoS`, setting up src pods and host-networked destination pods.
  * Traffic to the specified CIDR should be marked.
  * Traffic to the specified CIDR, Protocol should be marked.
  * Traffic to the specified CIDR, Protocol and Port should be marked.
  * Traffic to an address not contained in the CIDR, Protocol and Port should not be marked.

* IPv4/IPv6 E2E that validates egress traffic from a namespace is enforced with bandwidth limit by creating and deleting `NetworkQoS`, setting up src pods and destination pods.
  * Traffic to the all targeted pod IPs should be rate limited with specified bandwidth parameters.
  * Traffic to the targeted pod IPs, Protocol should be rate limited with specified bandwidth parameters.
  * Traffic to the targeted pod IPs, Protocol and Port should be rate limited with specified bandwidth parameters.
  * Traffic to an pod IP address not contained in the destination pod selector, Protocol and Port should not be rate limited with specified bandwidth parameters.

### Documentation Details

To be discussed.

## Risks, Known Limitations and Mitigations

## OVN Kubernetes Version Skew

To be discussed.

## Alternatives

N/A

## References
