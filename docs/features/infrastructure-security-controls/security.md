# OVN-Kubernetes Security

## Protecting controller-owned annotations

OVN-Kubernetes uses Kubernetes annotations to record network allocations and
other controller-managed state. Its security approach is to restrict writes to
the annotations it owns to the identities responsible for maintaining them.

## Protected pod annotations

The pod admission webhook protects these controller-owned annotations:

- `k8s.ovn.org/pod-networks`: network allocation results.
- `k8s.ovn.org/dpu.connection-details`: connection details written by the host CNI.
- `k8s.ovn.org/dpu.connection-status`: connection status written by the DPU controller.

These components write the annotations after the pod exists.

The `k8s.ovn.org/pod-networks` annotation records controller-generated network
allocation results. Allowing a pod creator to supply those results would let the
caller claim an allocation instead of obtaining one through the network
controllers.

The OVN-Kubernetes pod admission webhook enforces these rules:

- Pod creation must not supply any of the protected annotations, even with an empty value.
  This also applies to workload templates and restored pod manifests.
- After creation, only authorized network identities may add, change, or remove
  these annotations, through either `pods` or `pods/status`.
- Other users may update a pod while retaining the protected annotations unchanged.

Static address requests and IPAM claim references remain supported through
network selection inputs. Controllers populate the allocation result afterward.
During KubeVirt migration, OVN-Kubernetes copies the source allocation to the
target pod after creation. This annotation protection does not authenticate
KubeVirt-owned identity or migration metadata.

These protections require the OVN-Kubernetes admission webhook to be enabled.
Custom webhook configurations must register `CREATE` and `UPDATE` operations for
`pods`, and `UPDATE` for `pods/status`. Updating the webhook binary alone does not
extend admission coverage. Self-pod exclusions must not bypass validation when
either the old or new pod contains a protected annotation.

The checks apply to admission requests; they do not retroactively validate
annotations on existing pods.

## Per-node security

[Node identity](node-identity.md) gives each `ovnkube-node` a distinct client
certificate and restricts its annotation writes to its own node and pods hosted
there. Its admission checks also limit which annotations that identity may
modify and validate selected annotation values. See the node identity document
for certificate provisioning, webhook deployment, and per-node restrictions.
