# API Reference

## Packages
- [k8s.ovn.org/v1alpha1](#k8sovnorgv1alpha1)


## k8s.ovn.org/v1alpha1

Package v1alpha1 contains API Schema definitions for the ObservabilityConfig v1alpha1 API group

### Resource Types
- [ObservabilityConfig](#observabilityconfig)



#### FeatureConfig



FeatureConfig defines per-feature configuration, such as its sampling probability.



_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `probability` _integer_ | Probability is the probability of the feature being sampled in percent. |  | Maximum: 100 <br />Minimum: 0 <br />Required: \{\} <br />Required: \{\} <br /> |
| `feature` _[ObservabilityFeature](#observabilityfeature)_ | Feature is the Observability feature that should be sampled. |  | Enum: [NetworkPolicy AdminNetworkPolicy EgressFirewall UDNIsolation MulticastIsolation] <br />Required: \{\} <br />Required: \{\} <br /> |


#### Filter



Filter allows to apply ObservabilityConfig in a granular manner.
Currently, it supports node and namespace based filtering.
If both node and namespace filters are specified, they are logically ANDed.

_Validation:_
- MinProperties: 1

_Appears in:_
- [ObservabilitySpec](#observabilityspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | nodeSelector applies ObservabilityConfig only to nodes that match the selector. |  | Optional: \{\} <br /> |
| `namespaces` _string array_ | namespaces is a list of namespaces to which the ObservabilityConfig should be applied.<br />It only applies to the namespaced features, currently that includes NetworkPolicy and EgressFirewall. |  | MaxItems: 100 <br />MinItems: 1 <br />items:MaxLength: 63 <br />items:Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Optional: \{\} <br /> |


#### ObservabilityConfig



ObservabilityConfig describes the OVN Observability API, which binds
observed samples to a collector ID, for a given set of features and filters.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1alpha1` | | |
| `kind` _string_ | `ObservabilityConfig` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ObservabilitySpec](#observabilityspec)_ |  |  | Required: \{\} <br />Required: \{\} <br /> |
| `status` _[ObservabilityStatus](#observabilitystatus)_ |  |  | Optional: \{\} <br /> |


#### ObservabilityFeature

_Underlying type:_ _string_

ObservabilityFeature is the type of OVN features that can be sampled for observability.

_Validation:_
- Enum: [NetworkPolicy AdminNetworkPolicy EgressFirewall UDNIsolation MulticastIsolation]

_Appears in:_
- [FeatureConfig](#featureconfig)

| Field | Description |
| --- | --- |
| `NetworkPolicy` |  |
| `AdminNetworkPolicy` |  |
| `EgressFirewall` |  |
| `UDNIsolation` |  |
| `MulticastIsolation` |  |


#### ObservabilitySpec



ObservabilitySpec defines the desired state of ObservabilityConfig.



_Appears in:_
- [ObservabilityConfig](#observabilityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `collectorID` _integer_ | CollectorID is the OVN Sample_Collector set_id used to bind samples to a collector on<br />the consumer side (e.g. ovnkube-observ's -ovs-collector-id). The same collectorID may be<br />shared across multiple ObservabilityConfigs - typically to target different nodes - so a<br />consumer can pull the aggregated sample stream from every node using a single ID. Within a<br />single node, the same collectorID must map to a single probability for a given feature: avoid<br />two configs that apply to the same node and feature with the same collectorID but different<br />probabilities. |  | Maximum: 4.294967295e+09 <br />Minimum: 1 <br />Required: \{\} <br />Required: \{\} <br /> |
| `features` _[FeatureConfig](#featureconfig) array_ | Features is a list of Observability features that can generate samples and their probabilities for a given collector. |  | MinItems: 1 <br />Required: \{\} <br />Required: \{\} <br /> |
| `filter` _[Filter](#filter)_ | Filter allows to apply ObservabilityConfig in a granular manner. |  | MinProperties: 1 <br />Optional: \{\} <br /> |


#### ObservabilityStatus



ObservabilityStatus contains the observed status of the ObservabilityConfig.



_Appears in:_
- [ObservabilityConfig](#observabilityconfig)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ |  |  |  |


