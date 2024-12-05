# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)
- [ClusterUserDefinedNetworkList](#clusteruserdefinednetworklist)
- [UserDefinedNetwork](#userdefinednetwork)
- [UserDefinedNetworkList](#userdefinednetworklist)



#### CIDR

_Underlying type:_ _string_





_Appears in:_
- [DualStackCIDRs](#dualstackcidrs)
- [Layer3Subnet](#layer3subnet)



#### ClusterUserDefinedNetwork



ClusterUserDefinedNetwork describe network request for a shared network across namespaces.



_Appears in:_
- [ClusterUserDefinedNetworkList](#clusteruserdefinednetworklist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterUserDefinedNetwork` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterUserDefinedNetworkSpec](#clusteruserdefinednetworkspec)_ |  |  | Required: \{\} <br /> |
| `status` _[ClusterUserDefinedNetworkStatus](#clusteruserdefinednetworkstatus)_ |  |  |  |


#### ClusterUserDefinedNetworkList



ClusterUserDefinedNetworkList contains a list of ClusterUserDefinedNetwork.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `ClusterUserDefinedNetworkList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ClusterUserDefinedNetwork](#clusteruserdefinednetwork) array_ |  |  |  |


#### ClusterUserDefinedNetworkSpec



ClusterUserDefinedNetworkSpec defines the desired state of ClusterUserDefinedNetwork.



_Appears in:_
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | NamespaceSelector Label selector for which namespace network should be available for. |  | Required: \{\} <br /> |
| `network` _[NetworkSpec](#networkspec)_ | Network is the user-defined-network spec |  | Required: \{\} <br /> |


#### ClusterUserDefinedNetworkStatus



ClusterUserDefinedNetworkStatus contains the observed status of the ClusterUserDefinedNetwork.



_Appears in:_
- [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | Conditions slice of condition objects indicating details about ClusterUserDefineNetwork status. |  |  |


#### DualStackCIDRs

_Underlying type:_ _[CIDR](#cidr)_



_Validation:_
- MaxItems: 2
- MinItems: 1

_Appears in:_
- [Layer2Config](#layer2config)
- [Layer3Config](#layer3config)



#### Layer2Config







_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _[NetworkRole](#networkrole)_ | Role describes the network role in the pod.<br /><br />Allowed value is "Secondary".<br />Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network. |  | Enum: [Primary Secondary] <br />Required: \{\} <br /> |
| `mtu` _integer_ | MTU is the maximum transmission unit for a network.<br />MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network. |  | Maximum: 65536 <br />Minimum: 0 <br /> |
| `subnets` _[DualStackCIDRs](#dualstackcidrs)_ | Subnets are used for the pod network across the cluster.<br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br /><br />The format should match standard CIDR notation (for example, "10.128.0.0/16").<br />This field may be omitted. In that case the logical switch implementing the network only provides layer 2 communication,<br />and users must configure IP addresses for the pods. As a consequence, Port security only prevents MAC spoofing. |  | MaxItems: 2 <br />MinItems: 1 <br /> |
| `joinSubnets` _[DualStackCIDRs](#dualstackcidrs)_ | JoinSubnets are used inside the OVN network topology.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />This field is only allowed for "Primary" network.<br />It is not recommended to set this field without explicit need and understanding of the OVN network topology.<br />When omitted, the platform will choose a reasonable default which is subject to change over time. |  | MaxItems: 2 <br />MinItems: 1 <br /> |
| `ipamLifecycle` _[NetworkIPAMLifecycle](#networkipamlifecycle)_ | IPAMLifecycle controls IP addresses management lifecycle.<br /><br />The only allowed value is Persistent. When set, OVN Kubernetes assigned IP addresses will be persisted in an<br />`ipamclaims.k8s.cni.cncf.io` object. These IP addresses will be reused by other pods if requested.<br />Only supported when "subnets" are set. |  | Enum: [Persistent] <br /> |


#### Layer3Config







_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _[NetworkRole](#networkrole)_ | Role describes the network role in the pod.<br /><br />Allowed values are "Primary" and "Secondary".<br />Primary network is automatically assigned to every pod created in the same namespace.<br />Secondary network is only assigned to pods that use `k8s.v1.cni.cncf.io/networks` annotation to select given network. |  | Enum: [Primary Secondary] <br />Required: \{\} <br /> |
| `mtu` _integer_ | MTU is the maximum transmission unit for a network.<br /><br />MTU is optional, if not provided, the globally configured value in OVN-Kubernetes (defaults to 1400) is used for the network. |  | Maximum: 65536 <br />Minimum: 0 <br /> |
| `subnets` _[Layer3Subnet](#layer3subnet) array_ | Subnets are used for the pod network across the cluster.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />Given subnet is split into smaller subnets for every node. |  | MaxItems: 2 <br />MinItems: 1 <br /> |
| `joinSubnets` _[DualStackCIDRs](#dualstackcidrs)_ | JoinSubnets are used inside the OVN network topology.<br /><br />Dual-stack clusters may set 2 subnets (one for each IP family), otherwise only 1 subnet is allowed.<br />This field is only allowed for "Primary" network.<br />It is not recommended to set this field without explicit need and understanding of the OVN network topology.<br />When omitted, the platform will choose a reasonable default which is subject to change over time. |  | MaxItems: 2 <br />MinItems: 1 <br /> |


#### Layer3Subnet







_Appears in:_
- [Layer3Config](#layer3config)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidr` _[CIDR](#cidr)_ | CIDR specifies L3Subnet, which is split into smaller subnets for every node. |  |  |
| `hostSubnet` _integer_ | HostSubnet specifies the subnet size for every node.<br /><br />When not set, it will be assigned automatically. |  | Maximum: 127 <br />Minimum: 1 <br /> |


#### NetworkIPAMLifecycle

_Underlying type:_ _string_



_Validation:_
- Enum: [Persistent]

_Appears in:_
- [Layer2Config](#layer2config)

| Field | Description |
| --- | --- |
| `Persistent` |  |


#### NetworkRole

_Underlying type:_ _string_



_Validation:_
- Enum: [Primary Secondary]

_Appears in:_
- [Layer2Config](#layer2config)
- [Layer3Config](#layer3config)

| Field | Description |
| --- | --- |
| `Primary` |  |
| `Secondary` |  |


#### NetworkSpec



NetworkSpec defines the desired state of UserDefinedNetworkSpec.



_Appears in:_
- [ClusterUserDefinedNetworkSpec](#clusteruserdefinednetworkspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `topology` _[NetworkTopology](#networktopology)_ | Topology describes network configuration.<br /><br />Allowed values are "Layer3", "Layer2".<br />Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.<br />Layer2 topology creates one logical switch shared by all nodes. |  | Enum: [Layer2 Layer3] <br />Required: \{\} <br /> |
| `layer3` _[Layer3Config](#layer3config)_ | Layer3 is the Layer3 topology configuration. |  |  |
| `layer2` _[Layer2Config](#layer2config)_ | Layer2 is the Layer2 topology configuration. |  |  |


#### NetworkTopology

_Underlying type:_ _string_



_Validation:_
- Enum: [Layer2 Layer3]

_Appears in:_
- [NetworkSpec](#networkspec)
- [UserDefinedNetworkSpec](#userdefinednetworkspec)

| Field | Description |
| --- | --- |
| `Layer2` |  |
| `Layer3` |  |


#### UserDefinedNetwork



UserDefinedNetwork describe network request for a Namespace.



_Appears in:_
- [UserDefinedNetworkList](#userdefinednetworklist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `UserDefinedNetwork` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[UserDefinedNetworkSpec](#userdefinednetworkspec)_ |  |  | Required: \{\} <br /> |
| `status` _[UserDefinedNetworkStatus](#userdefinednetworkstatus)_ |  |  |  |


#### UserDefinedNetworkList



UserDefinedNetworkList contains a list of UserDefinedNetwork.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `UserDefinedNetworkList` | | |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[UserDefinedNetwork](#userdefinednetwork) array_ |  |  |  |


#### UserDefinedNetworkSpec



UserDefinedNetworkSpec defines the desired state of UserDefinedNetworkSpec.



_Appears in:_
- [UserDefinedNetwork](#userdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `topology` _[NetworkTopology](#networktopology)_ | Topology describes network configuration.<br /><br />Allowed values are "Layer3", "Layer2".<br />Layer3 topology creates a layer 2 segment per node, each with a different subnet. Layer 3 routing is used to interconnect node subnets.<br />Layer2 topology creates one logical switch shared by all nodes. |  | Enum: [Layer2 Layer3] <br />Required: \{\} <br /> |
| `layer3` _[Layer3Config](#layer3config)_ | Layer3 is the Layer3 topology configuration. |  |  |
| `layer2` _[Layer2Config](#layer2config)_ | Layer2 is the Layer2 topology configuration. |  |  |


#### UserDefinedNetworkStatus



UserDefinedNetworkStatus contains the observed status of the UserDefinedNetwork.



_Appears in:_
- [UserDefinedNetwork](#userdefinednetwork)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ |  |  |  |


