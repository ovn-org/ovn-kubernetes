# DNS name resolover

## Introduction

The DNS name resolver feature allows a cluster administrator to enable integration of
EgressFirewall with CoreDNS. Once enabled the EgressFirewall DNS rules will be resolved
using CoreDNS and the underlying address set will be updated accordingly. Additionally,
if any other pod does the DNS resolution of DNS names used in the EgressFirewall DNS
rules, then the underlying address sets will also be updated if there is any change in
IP addresses.

The DNS name resolver feature also adds the support of using wildcard DNS names in
EgressFirewall DNS name rules. The wildcard (`*`) will match only one label (subdomain).
For example, `*.example.com` will match `sub1.example.com` and will not match
`sub2.sub1.example.com`.

The [`DNSNameResolver`](https://github.com/openshift/api/tree/ef21ee7c3d0590ac431e81059172615e2addbbe3/network/v1alpha1/zz_generated.crd-manifests)
CRD will be used by ovnk to get the latest IP addresses corresponding to a DNS name when
the feature is enabled. However, the cluster administrator does not need to interact with
the CR. 

## Configuring DNS name resolover in a cluster

The DNS name resolver should be configured in the following components for it to work
in a cluster:

### [DNSNameResolver operator](https://github.com/openshift/coredns-ocp-dnsnameresolver/tree/main/operator)

The DNSNameResolver operator should also be deployed using `make deploy`. Before deploying
the operator, correct values should be provided for the following arguments:
- `coredns-namespace`
- `coredns-service-name`
- `coredns-port`
- `dns-name-resolver-namespace` [this should be the namespace where ovnk is deployed]

**NOTE:** The DNSNameResolver CRD should be installed using `make install` before deploying
the operator.

### [CoreDNS plugin](https://github.com/openshift/coredns-ocp-dnsnameresolver)

In addition to deploying the operator, the `ocp_dnsnameresolver` external plugin should be
[enabled](https://coredns.io/2017/07/25/compile-time-enabling-or-disabling-plugins/) on the
CoreDNS pods. In the `Corefile`, the plugin should be configured to watch for the namespace
where ovnk is deployed.

```
ocp_dnsnameresolver {
    namespaces <ovnk-namespace>
}
```

### OVNK

In ovnk, the feature is gated by config flag. In order to create a cluster with dns name
resolver feature enabled, use the `--enable-dns-name-resolver` option in cluster-manager
and ovnkube-controller.

The following RBAC permissions should be added to cluster-manager:

```yaml
- apiGroups: ["network.openshift.io"]
  resources:
  - dnsnameresolvers
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
```

The following RBAC permissions should be added to ovnkube-node:

```yaml
- apiGroups: ["network.openshift.io"]
  resources:
  - dnsnameresolvers
  verbs:
  - get
  - list
  - watch
```