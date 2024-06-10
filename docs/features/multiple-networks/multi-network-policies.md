# MultiNetworkPolicies
OVN-Kubernetes implements native support for
[multi-networkpolicy](https://github.com/k8snetworkplumbingwg/multi-networkpolicy),
an API providing
[network policy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
features for secondary networks.

To configure pod isolation, the user must:
- provision a `network-attachment-definition`.
- provision a `MultiNetworkPolicy` indicating to which secondary networks it
  applies via the
  [policy-for](https://github.com/k8snetworkplumbingwg/multi-networkpolicy#policy-for-annotation)
  annotation.

**NOTE:** the `OVN_MULTI_NETWORK_ENABLE` config flag must be enabled.

Please refer to the following example:
```yaml
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenant-blue
spec:
    config: '{
        "cniVersion": "0.4.0",
        "name": "tenant-blue",
        "netAttachDefName": "default/tenant-blue",
        "topology": "layer2",
        "type": "ovn-k8s-cni-overlay",
        "subnets": "192.168.100.0/24"
    }'
---
apiVersion: k8s.cni.cncf.io/v1beta2
kind: MultiNetworkPolicy
metadata:
  annotations:
    k8s.v1.cni.cncf.io/policy-for: default/tenant-blue # indicates the net-attach-defs this policy applies to
  name: allow-ports-same-ns
spec:
  podSelector:
    matchLabels:
      app: stuff-doer # the policy will **apply** to all pods with this label
  ingress:
  - ports:
    - port: 9000
      protocol: TCP
    from:
    - namespaceSelector:
        matchLabels:
          role: trusted # only pods on namespaces with this label will be allowed on port 9000
  policyTypes:
    - Ingress
```

Please note the `MultiNetworkPolicy` has the **exact same** API of the native
`networking.k8s.io/v1` `NetworkPolicy`object; check its documentation for more
information.

**Note:** `net-attach-def`s referred to by the `k8s.v1.cni.cncf.io/policy-for`
annotation without the subnet attribute defined are possible if the policy
**only features** `ipBlock` peers. If the `net-attach-def` features the
`subnet` attribute, it can also feature `namespaceSelectors` and `podSelectors`.
