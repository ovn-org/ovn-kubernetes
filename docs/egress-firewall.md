# EgressFirewall

## Introduction

The EgressFirewall feature enables a cluster administrator to
limit the external hosts that a pod in a project can access.
The EgressFirewall object rules apply to all pods that share
the namespace with the egressfirewall object. A namespace only
supports having one EgressFirewallObject.

## Example

The yaml  below is an example of a simple egressFirewall object

```yaml
kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: default
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: 1.2.3.0/24
  - type: Allow
    to:
      cidrSelector: 4.5.6.0/24
    ports:
      - protocol: UDP
        port: 55
  - type: Deny
    to:
      cidrSelector: 0.0.0.0/0
```



This example allows Pods in the default namespace to connect to
any external host within the range 1.2.3.0 to 1.2.3.255 and in addtion
allows traffic to 4.5.6.0 to 4.5.6.255 only for the UDP protocol on port
number 55 and denies traffic to all other external hosts. The ports 
section is optional and allows the user to specify specific ports 
to and protocols to allow or deny traffic.

The priority of a rule is determined by its placement in the egress
array. An earlier rule is processed before a later rule. In the 
previous example, if the rules are reversed, all traffic is denied,
including any traffic to hosts in the 1.2.3.0/24 CIDR block.

