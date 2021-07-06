# Multicast

## Introduction
IP multicast enables data to be delivered to multiple IP addresses
simultaneously.
Multicast can distribute data one-to-many or many-to-many. For this to happen,
the 'receivers' join a multicast group, and the sender(s) send data to it.
In other words, multicast filtering is achieved by dynamic group control
management.

The multicast group membership is implemented with IGMP. For details, check RFCs
[1112](https://datatracker.ietf.org/doc/html/rfc1112)
and [2236](https://datatracker.ietf.org/doc/html/rfc2236).

## Configuring multicast on the cluster

### Enabling multicast per namespace
The multicast traffic between pods in the cluster is blocked by default; it can
be enabled **per namespace** - but it **cannot** be enabled cluster wide.

To enable multicast support on a given namespace, you need to annotate the
namespace:

```bash
$ kubectl annotate namespace <namespace name> \
    k8s.ovn.org/multicast-enabled=true
```

