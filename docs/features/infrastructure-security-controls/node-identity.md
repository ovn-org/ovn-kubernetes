# OVN-Kubernetes Node Identity

## Introduction

The OVN-Kubernetes node identity feature introduces a per-node client certificate for ovnkube-node pods,
together with a [validating admission webhook](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/#validatingadmissionwebhook), 
enabling granular permission enforcement specific to ovn-kubernetes.
Previously, all `ovnkube-node` pods shared a common service account, 
with their API permissions managed only through Kubernetes [RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/) rules.\
The goal of this feature is to limit `ovnkube-node` permissions to the minimum required for networking management on a specific Kubernetes node.
We will mimic the [approach used by kubelet](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/) in which every node has a unique identity, 
and its API write requests are verified using a [NodeRestriction](https://github.com/kubernetes/kubernetes/blob/9e0569f2ed3934060fabe51be4e15232bbea3877/plugin/pkg/admission/noderestriction/admission.go) validating admission webhook.

Always check the dependencies on the [Requirements page](../requirements.md)

## Per-node client certificates

This process mimics the [bootstrap initialization](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/#bootstrap-initialization) in kubelet.
When the `ovnkube-node` starts for the first time, it uses the host's `kubeconfig` to create a [CertificateSigningRequest](https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/)
that requests a client certificate for the `system:ovn-node:<nodeName>` user in the `system:ovn-nodes` group.
This request is then signed by the [kubernetes.io/kube-apiserver-client](https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/#kubernetes-signers) signer.

For the certificate to be signed, it must first be approved.
The newly introduced `OVNKubeCSRController` component approves or denies `CertificateSigningRequests` created for users with the `system:ovn-node` prefix.
The `OVNKubeCSRController` performs several checks to validate the requested certificate, including:
- Ensuring that the node name extracted from the request matches the node name of the user that sent it.
- Verifying that the group is set to `system:ovn-nodes`.
- Checking that the certificate expiration does not exceed the maximum allowed duration.

Once the certificate is approved and signed, `ovnkube-node` uses it to communicate with the API server and request a new certificate upon expiration.
The RBAC rules are consistent across all `ovnkube-node` pods. We use the `system:ovn-nodes` group as the subject for Role/ClusterRole bindings to avoid duplication:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
    name: ovnkube-node
roleRef:
    name: ovnkube-node
    kind: ClusterRole
    apiGroup: rbac.authorization.k8s.io
subjects:
    - kind: Group
      name: system:ovn-nodes
      apiGroup: rbac.authorization.k8s.io
```
Note: By default, the generated certificates have a brief lifetime set (10min).
This is intentional as it helps ensure that the certificate rotation works seamlessly, but it might be adjusted in production environments.

## Validating Admission Webhook

The feature introduces validating webhooks for pod creation, updates to `pods` and
`pods/status`, and creation and updates of `nodes`, including `nodes/status`.

For cluster-wide pod annotation protection, see [Security](security.md#protected-pod-annotations).

Node annotations can also be written through the main Node resource, so both paths must be checked.
An API-server match condition skips the node webhook for other callers unless
an annotation with the `k8s.ovn.org/` prefix is added, changed, or removed.
Ordinary node registration without OVN annotations and unrelated label, taint,
and status updates therefore do not depend on webhook availability. Requests
using `system:ovn-node` credentials always reach the webhook so that their
metadata and status restrictions remain enforced. Matching requests fail closed
if the webhook cannot be reached. Custom deployments must include this match
condition along with the expanded resource rules.

The webhooks include the following checks for each `ovnkube-node` pod:

- Modifying annotations on pods hosted on its own node.
- Modifying annotations on its own node.
- Modifying only allowed annotations.
- Not modifying anything other than annotations.

The allowed annotations list contains both common and feature specific values:

 - By default, the webhook will verify common node annotations and pod/node annotations set by the ovnkube-node component.
 - When `enable-hybrid-overlay` parameter is provided the webhook will validate additional node annotations set by the ovnkube-node component for hybrid overlay.

Cluster-manager's allocation annotations, including `k8s.ovn.org/node-id` and
`k8s.ovn.org/udn-layer2-node-gateway-router-lrp-tunnel-ids`, are protected by a
separate [admission policy](index.md#controller-owned-node-annotations).
That protection remains active when NodeIdentity is disabled and authorizes
only the cluster-manager service account; `--extra-allowed-user` does not
override it. Node credentials cannot write these annotations, even on their
own node. Other users can update unrelated annotations and node status while
preserving protected annotations.

Custom deployments must update the node webhook rules to include `CREATE` and
`UPDATE` on `nodes`, and `UPDATE` on `nodes/status`. Configure all required trusted
controller identities before enabling these protections. Administrative repair
requires an identity authorized by every applicable webhook and admission
policy. Existing annotation values are not revalidated when they remain
unchanged; this protection does not repair preexisting corrupt allocations or
validate NAD/CNC resources.

When recreating a Node from saved YAML, normally omit protected OVN annotations
and let the controllers regenerate them. Replaying those annotations during
creation may be rejected even if they were valid on the original Node. See
[Controller-owned Node annotations](index.md#controller-owned-node-annotations)
for allocations protected independently of NodeIdentity.

The specific annotation values can be found in `go-controller/pkg/ovnwebhook/nodeadmission.go` and `go-controller/pkg/ovnwebhook/podadmission.go` files.

Some of the allowed annotations have additional checks; for instance, the IP addresses in [k8s.ovn.org/pod-networks](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/5d56a53df520a085e629cdc71be092afed9c3f0f/go-controller/pkg/util/pod_annotation.go#L20-L51)
must match the node's [k8s.ovn.org/node-subnets](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/5d56a53df520a085e629cdc71be092afed9c3f0f/go-controller/pkg/util/subnet_annotations.go#L15-L39) networks.


## DaemonSet

In KinD,
the feature is enabled by default and can be disabled with `--disable-ovnkube-identity` when creating the cluster.\
By default,
the webhook listens on the node's `localhost` address, so there has to be an instance running on the control-plane node:
```
kubectl get pod -lname=ovnkube-identity -n ovn-kubernetes  -o wide
NAME                                READY   STATUS    RESTARTS   AGE   IP           NODE                NOMINATED NODE   READINESS GATES
ovnkube-identity-57f9778d99-llfqz   1/1     Running   0          30m   172.18.0.3   ovn-control-plane   <none>           <none>
```
This approach allows us to collocate the webhook with the API server for better response times
and avoid relying on cluster networking.
