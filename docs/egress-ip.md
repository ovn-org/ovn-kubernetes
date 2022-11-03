# EgressIP

## Introduction

The EgressIP feature enables a cluster administrator to
assign an egress IP address for traffic leaving the cluster from a namespace or from specific pods in a namespace.

For more info, consider looking at the following links:
- [Egress IP CRD](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/crd/egressip/v1/types.go#L47)
- [Assigning an egress IP address](https://docs.okd.io/latest/networking/ovn_kubernetes_network_provider/assigning-egress-ips-ovn.html)
- [Managing Egress IP in OpenShift 4 with OVN Kubernetes](https://rcarrata.com/openshift/egress-ip-ovn/)


## Example

The yamls below are examples of egressIP resources:

Use 192.168.127.10 or 192.168.127.11 from pods of namespaces that have `env=qa` label.

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: egress-project1
spec:
    egressIPs:
    - 192.168.127.10
    - 192.168.127.11
    namespaceSelector:
        matchLabels:
            env: qa
```

Use 10.0.254.100 as egress IP from pods that are not in the `environment=development` namespaces and have the label `app=web`.

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: egressip-prod
spec:
    egressIPs:
    - 10.0.254.100
    namespaceSelector:
        matchExpressions:
        - key: environment
          operator: NotIn
          values:
          - development
    podSelector:
        matchLabels:
            app: web
```

## Egress Nodes

In order to select which node(s) may be used as egress, the following label must be added to the `node` resource:

```shell
kubectl label nodes <node_name> k8s.ovn.org/egress-assignable=""
```

## Egress IP reachability

Once a node has been labeled with `k8s.ovn.org/egress-assignable`, the EgressIP operator in the leader ovnkube-master pod will periodically check if that node is
usable. EgressIPs assigned to a node that is no longer reachable will get revalidated and moved to another useable node.

Egress nodes normally have multiple IP addresses. For sake of Egress IP reachability, [the management](https://github.com/ovn-org/ovn-kubernetes/pull/2495) (aka internal SDN) addresses of the node are the ones used. In deployments of ovn-kubernetes this is known to be the `ovn-k8s-mp0` interface of a node.

Even though the periodic checking of egress nodes is hard coded to trigger [every 5 seconds](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/egressip.go#L2206), there are attributes that the user can set:

- egressIPTotalTimeout
- gRPC vs. DISCARD port

### egressIPTotalTimeout

This attribute specifies the maximum amount of time, in seconds, that the egressIP operator will wait until it declares the node unreachable. The default value is [1 second](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/config/config.go#L866).

This value can be set in the following ways:
- ovnkube binary flag: `--egressip-reachability-total-timeout=<TIMEOUT>`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
egressip-reachability-total-timeout=123
```

**Note:** Using value `0` will skip reachability. Use this to assume that egress nodes are available.

### gRPC vs. DISCARD port

Up until recently, the only method available for determining if an egress node was reachable relied on the `TCP port unreachable` icmp response from the probed node. The TCP port 9 (aka DISCARD) is the port used for that.

[Later implementation](https://github.com/ovn-org/ovn-kubernetes/pull/3100) of ovn-kubernetes is capable of leveraging secure gRPC sessions in order to probe nodes. That requires the `ovnkube node` pods to be listening on a pre-specified TCP port, in addition to configuring the `ovnkube master` pod(s).

This value can be set in the following ways:
- ovnkube binary flag: `--egressip-node-healthcheck-port=<TCP_PORT>`
- inside config specified by `--config-file` flag:
```
[ovnkubernetesfeature]
egressip-node-healthcheck-port=9107
```

**Note:** If not specifying a value, or using `0` as the `egressip-node-healthcheck-port` will make Egress IP reachability probe the egress nodes using the DISCARD port method. Unlike egressip-reachability-total-timeout, it is important that both node and master pods of ovnkube get configured with the same value!

#### Additional details on the implementation of the gRPC probing:

- If available, the session uses the [same TLS certs](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/egressip_healthcheck.go#L78) used by ovnkube to connect to the northbound OVSDB server. Conversely, an insecure gRPC session is used when no certs are specified.
- The [message used for probing](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/health.proto#L6) is the [standard service health](https://github.com/grpc/grpc/blob/master/src/proto/grpc/health/v1/health.proto) specified in gRPC.
- [Special care was taken into consideration](https://github.com/ovn-org/ovn-kubernetes/blob/82f167a3920c8c3cd0687ceb3e7a5ba64372be69/go-controller/pkg/ovn/healthcheck/egressip_healthcheck.go#L193-L195) to handle cases when the gRPC session bounced for normal reasons. EgressIP implementation will not declare a node unreachable under these circumstances.

