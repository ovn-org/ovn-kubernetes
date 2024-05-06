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
| `spec` _[AdminPolicyBasedExternalRouteSpec](#adminpolicybasedexternalroutespec)_ |  |  | Required: {} <br /> |
| `status` _[AdminPolicyBasedRouteStatus](#adminpolicybasedroutestatus)_ |  |  |  |


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
| `lastTransitionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#time-v1-meta)_ | Captures the time when the last change was applied. |  |  |
| `messages` _string array_ | An array of Human-readable messages indicating details about the status of the object. |  |  |
| `status` _[StatusType](#statustype)_ | A concise indication of whether the AdminPolicyBasedRoute resource is applied with success |  |  |


#### DynamicHop



DynamicHop defines the configuration for a dynamic external gateway interface.
These interfaces are wrapped around a pod object that resides inside the cluster.
The field NetworkAttachmentName captures the name of the multus network name to use when retrieving the gateway IP to use.
The PodSelector and the NamespaceSelector are mandatory fields.



_Appears in:_
- [ExternalNextHops](#externalnexthops)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | PodSelector defines the selector to filter the pods that are external gateways. |  | Required: {} <br /> |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NamespaceSelector defines a selector to filter the namespaces where the pod gateways are located. |  | Required: {} <br /> |
| `networkAttachmentName` _string_ | NetworkAttachmentName determines the multus network name to use when retrieving the pod IPs that will be used as the gateway IP.<br />When this field is empty, the logic assumes that the pod is configured with HostNetwork and is using the node's IP as gateway. |  |  |
| `bfdEnabled` _boolean_ | BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false. | false |  |


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
| `static` _[StaticHop](#statichop) array_ | StaticHops defines a slice of StaticHop. This field is optional. |  |  |
| `dynamic` _[DynamicHop](#dynamichop) array_ | DynamicHops defines a slices of DynamicHop. This field is optional. |  |  |


#### StaticHop



StaticHop defines the configuration of a static IP that acts as an external Gateway Interface. IP field is mandatory.



_Appears in:_
- [ExternalNextHops](#externalnexthops)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `ip` _string_ | IP defines the static IP to be used for egress traffic. The IP can be either IPv4 or IPv6. |  | Pattern: `^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$|^s*((([0-9A-Fa-f]{1,4}:){7}([0-9A-Fa-f]{1,4}|:))|(([0-9A-Fa-f]{1,4}:){6}(:[0-9A-Fa-f]{1,4}|((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3})|:))|(([0-9A-Fa-f]{1,4}:){5}(((:[0-9A-Fa-f]{1,4}){1,2})|:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3})|:))|(([0-9A-Fa-f]{1,4}:){4}(((:[0-9A-Fa-f]{1,4}){1,3})|((:[0-9A-Fa-f]{1,4})?:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){3}(((:[0-9A-Fa-f]{1,4}){1,4})|((:[0-9A-Fa-f]{1,4}){0,2}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){2}(((:[0-9A-Fa-f]{1,4}){1,5})|((:[0-9A-Fa-f]{1,4}){0,3}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(([0-9A-Fa-f]{1,4}:){1}(((:[0-9A-Fa-f]{1,4}){1,6})|((:[0-9A-Fa-f]{1,4}){0,4}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:))|(:(((:[0-9A-Fa-f]{1,4}){1,7})|((:[0-9A-Fa-f]{1,4}){0,5}:((25[0-5]|2[0-4]d|1dd|[1-9]?d)(.(25[0-5]|2[0-4]d|1dd|[1-9]?d)){3}))|:)))(%.+)?s*` <br />Required: {} <br /> |
| `bfdEnabled` _boolean_ | BFDEnabled determines if the interface implements the Bidirectional Forward Detection protocol. Defaults to false. | false |  |


#### StatusType

_Underlying type:_ _string_

StatusType defines the types of status used in the Status field. The value determines if the
deployment of the CR was successful or if it failed.



_Appears in:_
- [AdminPolicyBasedRouteStatus](#adminpolicybasedroutestatus)



