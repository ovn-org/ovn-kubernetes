# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [AdminPolicyBasedExternalRoute](#adminpolicybasedexternalroute)



#### AdminPolicyBasedExternalRoute



AdminPolicyBasedExternalRoute is a CRD allowing the cluster administrators to configure policies for external gateway IPs to be applied to all the pods contained in selected namespaces.
Egress traffic from the pods that belong to the selected namespaces to outside the cluster is routed through these external gateway IPs.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `AdminPolicyBasedExternalRoute` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AdminPolicyBasedExternalRouteSpec](#adminpolicybasedexternalroutespec)_ |  |  | Required: \{\} <br />Required: \{\} <br /> |
| `status` _[AdminPolicyBasedRouteStatus](#adminpolicybasedroutestatus)_ |  |  | Optional: \{\} <br /> |


#### AdminPolicyBasedExternalRouteSpec



AdminPolicyBasedExternalRouteSpec defines the desired state of AdminPolicyBasedExternalRoute



_Appears in:_
- [AdminPolicyBasedExternalRoute](#adminpolicybasedexternalroute)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `from` _[ExternalNetworkSource](#externalnetworksource)_ | From defines the selectors that will determine the target namespaces to this CR. |  |  |
| `nextHops` _[ExternalNextHops](#externalnexthops)_ | NextHops defines two types of hops: Static and Dynamic. Each hop defines at least one external gateway IP. |  | MinProperties: 1 <br /> |


#### AdminPolicyBasedRouteStatus



AdminPolicyBasedRouteStatus contains the observed status of the AdminPolicyBased route types.



_Appears in:_
- [AdminPolicyBasedExternalRoute](#adminpolicybasedexternalroute)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `lastTransitionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#time-v1-meta)_ | Captures the time when the last change was applied. |  | Optional: \{\} <br /> |
| `messages` _string array_ | An array of Human-readable messages indicating details about the status of the object. |  | Optional: \{\} <br /> |
| `status` _[StatusType](#statustype)_ | A concise indication of whether the AdminPolicyBasedRoute resource is applied with success |  | Optional: \{\} <br /> |


#### DynamicHop



DynamicHop defines the configuration for a dynamic external gateway interface.
These interfaces are wrapped around a pod object that resides inside the cluster.
The field NetworkAttachmentName captures the name of the multus network name to use when retrieving the gateway IP to use.
The PodSelector and the NamespaceSelector are mandatory fields.



_Appears in:_
- [ExternalNextHops](#externalnexthops)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | PodSelector defines the selector to filter the pods that are external gateways. |  | Required: \{\} <br />Required: \{\} <br /> |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NamespaceSelector defines a selector to filter the namespaces where the pod gateways are located. |  | Required: \{\} <br />Required: \{\} <br /> |
| `networkAttachmentName` _string_ | NetworkAttachmentName determines the multus network name to use when retrieving the pod IPs that will be used as the gateway IP.<br />When this field is empty, the logic assumes that the pod is configured with HostNetwork and is using the node's IP as gateway. |  | Optional: \{\} <br /> |
| `bfdEnabled` _boolean_ | BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false. | false | Optional: \{\} <br /> |


#### ExternalNetworkSource



ExternalNetworkSource contains the selectors used to determine the namespaces where the policy will be applied to



_Appears in:_
- [AdminPolicyBasedExternalRouteSpec](#adminpolicybasedexternalroutespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NamespaceSelector defines a selector to be used to determine which namespaces will be targeted by this CR |  |  |


#### ExternalNextHops



ExternalNextHops contains slices of StaticHops and DynamicHops structures. Minimum is one StaticHop or one DynamicHop.

_Validation:_
- MinProperties: 1

_Appears in:_
- [AdminPolicyBasedExternalRouteSpec](#adminpolicybasedexternalroutespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `static` _[StaticHop](#statichop) array_ | StaticHops defines a slice of StaticHop. This field is optional.<br />The limit is an API bound, not a guarantee of dataplane capacity. |  | MaxItems: 1000 <br /> |
| `dynamic` _[DynamicHop](#dynamichop) array_ | DynamicHops defines a slice of DynamicHop. This field is optional. |  |  |


#### StaticHop



StaticHop defines the configuration of a static IP that acts as an external Gateway Interface. IP field is mandatory.



_Appears in:_
- [ExternalNextHops](#externalnexthops)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ip` _string_ | IP defines the static IP to be used for egress traffic. It must be a valid IPv4 or IPv6 address; malformed values are rejected during admission. |  | MaxLength: 45 <br />Required: \{\} <br />Required: \{\} <br /> |
| `bfdEnabled` _boolean_ | BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false. | false | Optional: \{\} <br /> |


#### StatusType

_Underlying type:_ _string_

StatusType defines the types of status used in the Status field. The value determines if the
deployment of the CR was successful or if it failed.



_Appears in:_
- [AdminPolicyBasedRouteStatus](#adminpolicybasedroutestatus)

| Field | Description |
| --- | --- |
| `Success` |  |
| `Fail` |  |


