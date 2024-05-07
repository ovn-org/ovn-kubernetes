# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [EgressService](#egressservice)



#### EgressService



EgressService is a CRD that allows the user to request that the source
IP of egress packets originating from all of the pods that are endpoints
of the corresponding LoadBalancer Service would be its ingress IP.
In addition, it allows the user to request that egress packets originating from
all of the pods that are endpoints of the LoadBalancer service would use a different
network than the main one.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `EgressService` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EgressServiceSpec](#egressservicespec)_ |  |  |  |
| `status` _[EgressServiceStatus](#egressservicestatus)_ |  |  |  |


#### EgressServiceSpec



EgressServiceSpec defines the desired state of EgressService



_Appears in:_
- [EgressService](#egressservice)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sourceIPBy` _[SourceIPMode](#sourceipmode)_ | Determines the source IP of egress traffic originating from the pods backing the LoadBalancer Service.<br />When `LoadBalancerIP` the source IP is set to its LoadBalancer ingress IP.<br />When `Network` the source IP is set according to the interface of the Network,<br />leveraging the masquerade rules that are already in place.<br />Typically these rules specify SNAT to the IP of the outgoing interface,<br />which means the packet will typically leave with the IP of the node. |  | Enum: [LoadBalancerIP Network] <br /> |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | Allows limiting the nodes that can be selected to handle the service's traffic when sourceIPBy=LoadBalancerIP.<br />When present only a node whose labels match the specified selectors can be selected<br />for handling the service's traffic.<br />When it is not specified any node in the cluster can be chosen to manage the service's traffic. |  |  |
| `network` _string_ | The network which this service should send egress and corresponding ingress replies to.<br />This is typically implemented as VRF mapping, representing a numeric id or string name<br />of a routing table which by omission uses the default host routing. |  |  |


#### EgressServiceStatus



EgressServiceStatus defines the observed state of EgressService



_Appears in:_
- [EgressService](#egressservice)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `host` _string_ | The name of the node selected to handle the service's traffic.<br />In case sourceIPBy=Network the field will be set to "ALL". |  |  |


#### SourceIPMode

_Underlying type:_ _string_



_Validation:_
- Enum: [LoadBalancerIP Network]

_Appears in:_
- [EgressServiceSpec](#egressservicespec)



