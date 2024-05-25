# API Reference

## Packages
- [k8s.ovn.org/v1](#k8sovnorgv1)


## k8s.ovn.org/v1

Package v1 contains API Schema definitions for the network v1 API group





#### EgressFirewallDestination



EgressFirewallDestination is the target that traffic is either allowed or denied to

_Validation:_
- MaxProperties: 1
- MinProperties: 1

_Appears in:_
- [EgressFirewallRule](#egressfirewallrule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `cidrSelector` _string_ | cidrSelector is the CIDR range to allow/deny traffic to. If this is set, dnsName and nodeSelector must be unset. |  |  |
| `dnsName` _string_ | dnsName is the domain name to allow/deny traffic to. If this is set, cidrSelector and nodeSelector must be unset. |  | Pattern: `^([A-Za-z0-9-]+\.)*[A-Za-z0-9-]+\.?$` <br /> |
| `nodeSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.28/#labelselector-v1-meta)_ | nodeSelector will allow/deny traffic to the Kubernetes node IP of selected nodes. If this is set,<br />cidrSelector and DNSName must be unset. |  |  |


#### EgressFirewallPort



EgressFirewallPort specifies the port to allow or deny traffic to



_Appears in:_
- [EgressFirewallRule](#egressfirewallrule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `protocol` _string_ | protocol (tcp, udp, sctp) that the traffic must match. |  | Pattern: `^TCP|UDP|SCTP$` <br /> |
| `port` _integer_ | port that the traffic must match |  | Maximum: 65535 <br />Minimum: 1 <br /> |


#### EgressFirewallRule



EgressFirewallRule is a single egressfirewall rule object



_Appears in:_
- [EgressFirewallSpec](#egressfirewallspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[EgressFirewallRuleType](#egressfirewallruletype)_ | type marks this as an "Allow" or "Deny" rule |  | Pattern: `^Allow|Deny$` <br /> |
| `ports` _[EgressFirewallPort](#egressfirewallport) array_ | ports specify what ports and protocols the rule applies to |  |  |
| `to` _[EgressFirewallDestination](#egressfirewalldestination)_ | to is the target that traffic is allowed/denied to |  | MaxProperties: 1 <br />MinProperties: 1 <br /> |


#### EgressFirewallRuleType

_Underlying type:_ _string_

EgressNetworkFirewallRuleType indicates whether an EgressNetworkFirewallRule allows or denies traffic

_Validation:_
- Pattern: `^Allow|Deny$`

_Appears in:_
- [EgressFirewallRule](#egressfirewallrule)



#### EgressFirewallSpec



EgressFirewallSpec is a desired state description of EgressFirewall.



_Appears in:_
- [EgressFirewall](#egressfirewall)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `egress` _[EgressFirewallRule](#egressfirewallrule) array_ | a collection of egress firewall rule objects |  |  |


#### EgressFirewallStatus







_Appears in:_
- [EgressFirewall](#egressfirewall)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _string_ |  |  |  |
| `messages` _string array_ |  |  |  |


