# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group

### Resource Types
- [EgressQoS](#egressqos)



#### EgressQoS



EgressQoS is a CRD that allows the user to define a DSCP value
for pods egress traffic on its namespace to specified CIDRs.
Traffic from these pods will be checked against each EgressQoSRule in
the namespace's EgressQoS, and if there is a match the traffic is marked
with the relevant DSCP value.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `k8s.ovn.org/v1` | | |
| `kind` _string_ | `EgressQoS` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[EgressQoSSpec](#egressqosspec)_ |  |  |  |
| `status` _[EgressQoSStatus](#egressqosstatus)_ |  |  |  |


#### EgressQoSRule







_Appears in:_
- [EgressQoSSpec](#egressqosspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `dscp` _integer_ | DSCP marking value for matching pods' traffic. |  | Maximum: 63 <br />Minimum: 0 <br /> |
| `dstCIDR` _string_ | DstCIDR specifies the destination's CIDR. Only traffic heading<br />to this CIDR will be marked with the DSCP value.<br />This field is optional, and in case it is not set the rule is applied<br />to all egress traffic regardless of the destination. |  | Format: cidr <br /> |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | PodSelector applies the QoS rule only to the pods in the namespace whose label<br />matches this definition. This field is optional, and in case it is not set<br />results in the rule being applied to all pods in the namespace. |  |  |


#### EgressQoSSpec



EgressQoSSpec defines the desired state of EgressQoS



_Appears in:_
- [EgressQoS](#egressqos)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `egress` _[EgressQoSRule](#egressqosrule) array_ | a collection of Egress QoS rule objects |  |  |


#### EgressQoSStatus



EgressQoSStatus defines the observed state of EgressQoS



_Appears in:_
- [EgressQoS](#egressqos)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _string_ | A concise indication of whether the EgressQoS resource is applied with success. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#condition-v1-meta) array_ | An array of condition objects indicating details about status of EgressQoS object. |  |  |


