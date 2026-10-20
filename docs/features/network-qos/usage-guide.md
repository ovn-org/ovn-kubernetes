# Guide to Using Network QoS

## Contents

1. [Overview](#1-overview)  
2. [Create a Secondary Network (NAD)](#2-create-a-secondary-network)  
3. [Define a NetworkQoS Policy](#3-define-a-networkqos-policy)  
4. [Create Sample Pods and Verify the Configuration](#4-create-sample-pods-and-verify-the-configuration)
5. [Explain the NetworkQoS Object](#5-explain-the-networkqos-object)

## **1 Overview**

Differentiated Services Code Point (DSCP) marking and egress bandwidth metering let you prioritize or police specific traffic flows. The new **NetworkQoS** Custom Resource Definition (CRD) in [ovn-kubernetes](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/helm/ovn-kubernetes/crds/k8s.ovn.org_networkqoses.yaml) makes both features available to Kubernetes users on **all** pod interfaces—primary or secondary—without touching pod manifests.

This guide provides a step-by-step example of how to use this feature. Before you begin, ensure that you have a Kubernetes cluster configured with the ovn-kubernetes CNI. Since the examples use network attachments, you must run the cluster with multiple network support enabled. In a KinD cluster, you would use the following flags:

```bash
cd contrib
./kind-helm.sh -nqe -mne ;  #  --enable-network-qos --enable-multi-network
```

Always check the dependencies on the [Requirements page](../requirements.md)

## **2 Create a Secondary Network**

File: nad.yaml

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ovn-stream
  namespace: default
  labels:                   # label needed for NetworkQoS selector
    nad-type: ovn-kubernetes-nqos
spec:
  config: |2
    {
            "cniVersion": "1.0.0",
            "name": "ovn-stream",
            "type": "ovn-k8s-cni-overlay",
            "topology": "layer3",
            "subnets": "10.245.0.0/16/24",
            "mtu": 1300,
            "master": "eth1",
            "netAttachDefName": "default/ovn-stream"
    }
```
*Why the label?* `NetworkQoS` uses a label selector to find matching NADs. Without at least one label, the selector cannot match.

## **3 Define a NetworkQoS Policy**

File: nqos.yaml

```yaml
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  name: qos-external
  namespace: default
spec:
  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector: {}  # any namespace
      networkSelector:
        matchLabels:
          nad-type: ovn-kubernetes-nqos
  podSelector:
    matchLabels:
      nqos-app: bw-limited
  priority: 10              # higher value wins in a tie-break
  egress:
  - dscp: 20               
    bandwidth:
      burst: 100            # kilobits
      rate: 20000           # kbps
    classifier:
      to:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
          - 10.11.12.13/32
          - 172.16.0.0/12
          - 192.168.0.0/16
```
A full CRD template lives [here](https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/helm/ovn-kubernetes/crds/k8s.ovn.org_networkqoses.yaml).

The `egress` field is a list, allowing you to define multiple markings and bandwidth limits based on different classifiers.

Note that this configuration will apply to the NAD of pods based on the network selector, and only on pods that have the label `nqos-app: bw-limited`.

```bash
$ kubectl create -f nad.yaml && \
  kubectl create -f nqos.yaml

networkattachmentdefinition.k8s.cni.cncf.io/ovn-stream created
networkqos.k8s.ovn.org/qos-external created
```
At this point, the output from `kubectl get networkqoses` will look like this:

```bash
$ kubectl api-resources -owide | head -1 ; \
  kubectl api-resources -owide | grep NetworkQoS
NAME                                SHORTNAMES         APIVERSION                          NAMESPACED   KIND                               VERBS                                                        CATEGORIES
networkqoses                                           k8s.ovn.org/v1alpha1                true         NetworkQoS                         delete,deletecollection,get,list,patch,create,update,watch

$ kubectl get networkqoses qos-external -n default -owide
NAME           STATUS
qos-external   No NetworkQoS errors reported
```

A node only writes a `Ready-In-Zone-<node>` condition when it fails to apply a
NetworkQoS, and retracts it once it succeeds, so that the object does not grow a
condition per node of the cluster. `No NetworkQoS errors reported` therefore
means no node is reporting a failure rather than every node having confirmed it
applied the object: a node whose ovnkube-controller is down reports nothing.
`kubectl get networkqos <name> -o yaml` shows the conditions of the nodes that
are failing, up to 20 of them. A spec no node can apply is reported by a single
node, with a message saying it is not applied on any node. A node on which a
selected network is still being set up is not failing: it applies the NetworkQoS
as soon as the logical switch of that network is created on it. A node also
reports a failure when it cannot resolve the addresses of the pods a NetworkQoS
selects, for example because of a malformed pod annotation: the QoS rules stay
applied, but they match the addresses known before the failure until it is
resolved.

Versions that reported a condition for every node also reported success. An
upgraded node keeps the success condition it reported until the cluster manager
reports `No NetworkQoS errors reported`, which only an upgraded cluster manager
does, and then retracts it, so the cluster manager and the nodes can be upgraded
in any order. While the nodes are upgraded before the cluster manager, the
status of a NetworkQoS created in the meantime, or that recovered from a failure
on an upgraded node, reads empty until the cluster manager is upgraded: the
previous cluster manager waits for a condition from every node.

## **4 Create Sample Pods and Verify the Configuration**

### **4.1  Launch Test Pods**

To test this, let's create a pod using a helper function that allows us to add labels to it.

File: create_pod.source

```bash
create_pod() {
    local pod_name=${1:-pod0}
    local node_name=${2:-ovn-worker}
    local extra_labels=${3:-}

    NAMESPACE=$(kubectl config view --minify --output 'jsonpath={..namespace}')
    NAMESPACE=${NAMESPACE:-default}

    if ! kubectl get pod "$pod_name" -n "$NAMESPACE" &>/dev/null; then
        echo "Creating pod $pod_name in namespace $NAMESPACE..."

        # Prepare labels block
        labels_block="    name: $pod_name"
        if [[ -n "$extra_labels" ]]; then
            # Convert JSON string to YAML-compatible lines
            while IFS="=" read -r k v; do
                labels_block+="
    $k: $v"
            done < <(echo "$extra_labels" | jq -r 'to_entries|map("\(.key)=\(.value)")|.[]')
        fi

        # Generate the manifest
        cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  labels:
$labels_block
  annotations:
    k8s.v1.cni.cncf.io/networks: ovn-stream@eth1
spec:
  nodeSelector:
    kubernetes.io/hostname: $node_name
  containers:
  - name: $pod_name
    image: ghcr.io/nicolaka/netshoot:v0.13
    command: ["/bin/ash", "-c", "trap : TERM INT; sleep infinity & wait"]
EOF
    else
        echo "Pod $pod_name already exists."
    fi
}
```

```bash
$ create_pod pod0 && \
  create_pod pod1 ovn-worker '{"nqos-app":"bw-limited"}' && \
  create_pod pod2 ovn-worker2 '{"foo":"bar","nqos-app":"bw-limited"}' && \
  echo pods created

extract_pod_ip_from_annotation() {
    local pod_name="$1"
    local namespace="${2:-default}"
    local interface="${3:-eth1}"

    kubectl get pod "$pod_name" -n "$namespace" -o json |
        jq -r '.metadata.annotations["k8s.v1.cni.cncf.io/network-status"]' |
        jq -r --arg iface "$interface" '.[] | select(.interface == $iface) | .ips[0]'
}
```

```bash
NAMESPACE=$(kubectl config view --minify --output 'jsonpath={..namespace}') ; NAMESPACE=${NAMESPACE:-default}
DST_IP_POD0=$(extract_pod_ip_from_annotation pod0 $NAMESPACE eth1)
DST_IP_POD1=$(extract_pod_ip_from_annotation pod1 $NAMESPACE eth1)
DST_IP_POD2=$(extract_pod_ip_from_annotation pod2 $NAMESPACE eth1)

# Let's see the NAD IP addresses of the pods created
$ echo pod0 has ip $DST_IP_POD0 ; \
  echo pod1 has ip $DST_IP_POD1 ; \
  echo pod2 has ip $DST_IP_POD2

pod0 has ip 10.245.4.4
pod1 has ip 10.245.4.3
pod2 has ip 10.245.2.3
```

### **4.2  Checking Bandwidth**

`qos-external` limits **only** traffic on pods that carry `nqos-app=bw-limited`. That means:

* **pod1 → pod0**: *unlimited* (no matching label)
* **pod1 → pod2**: *rate-limited* to ≈ 20 Mbit/s

Follow these steps to verify it with `iperf3`.

```bash
# 1) Start an iperf server inside pod0 and pod2 (runs forever in background)
kubectl -n default exec pod0 -- iperf3 -s -p 5201 &
kubectl -n default exec pod2 -- iperf3 -s -p 5201 &

# 2) From pod1 → pod0  (EXPECTED ≈ line rate)
kubectl -n default exec pod1 -- iperf3 -c "$DST_IP_POD0"   -p 5201 -R -t 10

# 3) From pod1 → pod2  (EXPECTED ≈ 20 Mbit/s)
kubectl -n default exec pod1 -- iperf3 -c "$DST_IP_POD2"   -p 5201 -R -t 10
```

Sample output:

```
# to pod0 (unlimited)
[ ID] Interval           Transfer     Bitrate         Retr
[  5]   0.00-10.00  sec  37.2 GBytes  31.9 Gbits/sec  607             sender
[  5]   0.00-10.00  sec  37.2 GBytes  31.9 Gbits/sec                  receiver

# to pod1 (rate-limited)
[ ID] Interval           Transfer     Bitrate         Retr
[  5]   0.00-10.00  sec  20.8 MBytes  17.4 Mbits/sec  4056             sender
[  5]   0.00-10.00  sec  20.8 MBytes  17.4 Mbits/sec                  receiver
```

The sharp drop confirms that `NetworkQoS` is enforcing the **20 Mbit/s** rate limit only for pods matching the selector.

### **4.3  Packet Capture**

Generate ICMP traffic and observe DSCP markings in Geneve outer headers using `tcpdump -envvi eth0 geneve` inside the worker node's network namespace. Only flows involving label-matched pods (those with `nqos-app=bw-limited`) will show `tos 0x50` (DSCP 20).

```bash
# Run ping commands in the background, so we can look at packets they generate

# pod0 to pod2
nohup kubectl exec -i pod0 -- ping -c 3600 -q $DST_IP_POD2 >/dev/null 2>&1 &
# pod1 to pod2
nohup kubectl exec -i pod1 -- ping -c 3600 -q $DST_IP_POD2 >/dev/null 2>&1 &

sudo dnf install -y --quiet tcpdump ; # Install tcpdump, if needed

IPNS=$(docker inspect --format '{{ '{{' }} .State.Pid }}' ovn-worker)
sudo nsenter -t ${IPNS} -n tcpdump -envvi eth0 geneve
```

```
tcpdump: listening on eth0, link-type EN10MB (Ethernet), capture size 262144 bytes

**Pod0 to Pod2**: Notice that since pod0 does not have the label to match against NetworkQoS, its TOS is 0. However, pod2's response is DSCP marked (tos 0x50), since pod2 matches the NetworkQoS criteria with the label `nqos-app: bw-limited`.

12:46:30.755551 02:42:ac:12:00:06 > 02:42:ac:12:00:05, ethertype IPv4 (0x0800), length 156: (tos 0x0, ttl 64, id 26896, offset 0, flags [DF], proto UDP (17), length 142)
    172.18.0.6.38210 > 172.18.0.5.geneve: [bad udp cksum 0x58bb -> 0xc87d!] Geneve, Flags [C], vni 0x12, proto TEB (0x6558), options [class Open Virtual Networking (OVN) (0x102) type 0x80(C) len 8 data 00090006]
        0a:58:0a:f5:02:01 > 0a:58:0a:f5:02:03, ethertype IPv4 (0x0800), length 98: (tos 0x0, ttl 63, id 61037, offset 0, flags [DF], proto ICMP (1), length 84)
    10.245.4.4 > 10.245.2.3: ICMP echo request, id 14, seq 44, length 64

—

12:46:30.755694 02:42:ac:12:00:05 > 02:42:ac:12:00:06, ethertype IPv4 (0x0800), length 156: (tos 0x50, ttl 64, id 46220, offset 0, flags [DF], proto UDP (17), length 142)
    172.18.0.5.38210 > 172.18.0.6.geneve: [bad udp cksum 0x58bb -> 0xc47d!] Geneve, Flags [C], vni 0x12, proto TEB (0x6558), options [class Open Virtual Networking (OVN) (0x102) type 0x80(C) len 8 data 0004000a]
        0a:58:0a:f5:04:01 > 0a:58:0a:f5:04:04, ethertype IPv4 (0x0800), length 98: (tos 0x50, ttl 63, id 45002, offset 0, flags [none], proto ICMP (1), length 84)
    10.245.2.3 > 10.245.4.4: ICMP echo reply, id 14, seq 44, length 64

—---------

**Pod1 to Pod2**: Traffic is marked both ways (both pods have the matching label)

12:46:30.497289 02:42:ac:12:00:06 > 02:42:ac:12:00:05, ethertype IPv4 (0x0800), length 156: (tos 0x50, ttl 64, id 26752, offset 0, flags [DF], proto UDP (17), length 142)
    172.18.0.6.7856 > 172.18.0.5.geneve: [bad udp cksum 0x58bb -> 0x3f10!] Geneve, Flags [C], vni 0x12, proto TEB (0x6558), options [class Open Virtual Networking (OVN) (0x102) type 0x80(C) len 8 data 00090006]
        0a:58:0a:f5:02:01 > 0a:58:0a:f5:02:03, ethertype IPv4 (0x0800), length 98: (tos 0x50, ttl 63, id 21760, offset 0, flags [DF], proto ICMP (1), length 84)
    10.245.4.3 > 10.245.2.3: ICMP echo request, id 14, seq 56, length 64

—

12:46:30.497381 02:42:ac:12:00:05 > 02:42:ac:12:00:06, ethertype IPv4 (0x0800), length 156: (tos 0x50, ttl 64, id 46019, offset 0, flags [DF], proto UDP (17), length 142)
    172.18.0.5.7856 > 172.18.0.6.geneve: [bad udp cksum 0x58bb -> 0x3b11!] Geneve, Flags [C], vni 0x12, proto TEB (0x6558), options [class Open Virtual Networking (OVN) (0x102) type 0x80(C) len 8 data 0004000a]
        0a:58:0a:f5:04:01 > 0a:58:0a:f5:04:03, ethertype IPv4 (0x0800), length 98: (tos 0x50, ttl 63, id 3850, offset 0, flags [none], proto ICMP (1), length 84)
    10.245.2.3 > 10.245.4.3: ICMP echo reply, id 14, seq 56, length 64
```

## **5 Explain the NetworkQoS Object**

Below is an *abbreviated* map of the CRD schema returned by `kubectl explain networkqos --recursive` (v1alpha1). Use this as a quick reference. For the definitive specification, always consult the `kubectl explain` output or the CRD YAML in the ovn-kubernetes repository.

### **5.1  Top‑level `spec` keys**

| Field | Type | Required | Purpose |
| ----- | ----- | ----- | ----- |
| **podSelector** | `LabelSelector` | No | Selects pods whose traffic will be evaluated by the QoS rules. If empty, all pods in the namespace are selected. |
| **networkSelectors[]** | list `NetworkSelector` | No | Restricts the rule to traffic on specific networks. If absent, the rule applies to the cluster default network. *(See §5.2)* |
| **priority** | `int` (0 – 100) | **Yes** | Higher number → chosen first when multiple `NetworkQoS` objects match the same packet. Every rule of a higher priority takes precedence over every rule of a lower one. |
| **egress[]** | list `EgressRule` | **Yes** | One or more marking / policing rules. Later rules take precedence over earlier ones. *(See §5.3)* |

Note the square-bracket notation (`[]`) for **both** `egress` and `networkSelectors`—each is an array in the CRD.

---

### **5.2  Inside a `networkSelectors[]` entry**

Each list element tells the controller **where** the pods' egress traffic must flow in order to apply the rule. Exactly **one** selector type must be set.

| Key | Required | Description |
| :---- | :---- | :---- |
| `networkSelectionType` | **Yes** | Enum that declares which selector below is populated. NetworkQoS accepts `NetworkAttachmentDefinitions` and `ClusterUserDefinedNetworks`. |
| `networkAttachmentDefinitionSelector` | conditional | When `networkSelectionType=NetworkAttachmentDefinitions`. Selects NADs by **namespaceSelector** (required) *and* **networkSelector** (required). Both are ordinary `LabelSelectors`: an empty one selects everything. |
| `clusterUserDefinedNetworkSelector` | conditional | When `networkSelectionType=ClusterUserDefinedNetworks`. Selects ClusterUserDefinedNetworks by **networkSelector** (required). An empty selector selects all of them. |

**Typical usage** – `networkSelectionType: NetworkAttachmentDefinitions` + `networkAttachmentDefinitionSelector`.

Selected networks without IP addresses, such as localnet networks without IPAM,
are ignored: there are no pod addresses to match their traffic on.

---

### **5.3  Inside an `egress[]` rule**

| Field | Type | Required | Description |
| :---- | :---- | :---- | :---- |
| `dscp` | `int` (0 – 63) | **Yes** | DSCP value to stamp on the **inner** IP header. This value determines the traffic priority. |
| `bandwidth.rate` | `int` (kbps) | No | Sustained rate for the token-bucket policer (in kilobits per second). |
| `bandwidth.burst` | `int` (kilobits) | No | Maximum burst size that can accrue (in kilobits). |
| `classifier.to` / `classifier.from` | list `TrafficSelector` | No | CIDRs the packet destination (or source) must match. Each entry is an `ipBlock` supporting an `except` list. |
| `classifier.ports[]` | list | No | List of `{protocol, port}` tuples the packet must match; protocol is `TCP`, `UDP`, or `SCTP`, and is required when `port` is set. An entry without a port matches every port of its protocol. |

If **all** specified classifier conditions match, the packet gets the DSCP mark and/or bandwidth policer defined above. This allows for fine-grained control over which traffic flows receive QoS treatment.

`classifier.ports` can be used without `classifier.to` to match the selected
protocols and ports regardless of destination. Entries in the ports list are
alternatives: `{protocol: TCP}` includes every TCP port, even when another entry
specifies a particular TCP port.

Earlier versions ignored a port without a protocol, so `ports: [{port: 8080}]`
matched every port and protocol. NetworkQoS objects created that way now match
the port on TCP, UDP and SCTP. Earlier CRDs also admitted protocols such as
`TCPX`, which OVN cannot match on, without reporting an error. NetworkQoS
objects with such a protocol are now reported as invalid specs and are not
applied until they are fixed.

The current CRD rejects new objects with either. It also rejects a change to any
rule of an existing object with either until that port or protocol is fixed:
the rules form a single list, and Kubernetes only skips validating the parts of
an object that an update leaves unchanged. Helm installs the CRDs of its chart
but does not upgrade them, so a cluster upgraded with Helm keeps accepting both
until the new CRD is applied.

Versions that programmed one NetworkQoS controller per network did not mark or
meter the traffic between pods on different nodes of a layer2 network. It is
now marked and metered on the node of the sending pod, before it leaves the
node, as on other networks.

Packets sent to a Service whose selected pod is on the same node as the sending
pod go through the router of the network and pass the logical switch of that
node twice. A rule applies its bandwidth limit to them once. Versions that
programmed one NetworkQoS controller per network applied it twice. On the
default network, a rule sets their DSCP on both passes, so that an EgressQoS
selecting the same pods does not override it on the second pass. On
layer2 networks, packets a pod sends to the router itself, such as those to the
gateway address, are not marked. Rules on layer2 networks also see the first
packet of each connection to a Service before it is load balanced, with the
Service address as its destination, so a rule matching that address counts it
against its bandwidth limit as well.

For pod destinations, `podSelector: {}` selects all pods in the NetworkQoS
namespace. Adding `namespaceSelector: {}` extends that selection to all
namespaces. A destination entry with neither selectors nor an `ipBlock` leaves
the destination unrestricted. This is what the CRD and
[OKEP-4380](../../okeps/okep-4380-network-qos.md) specify: a selector that is
present but empty selects everything. Versions that programmed one NetworkQoS
controller per network deviated from it and ignored empty selectors: an entry
with `podSelector: {}` left the destination unrestricted, and
`namespaceSelector: {}` did not extend a pod selector beyond the NetworkQoS
namespace. NetworkQoS objects relying on that select different destinations
after an upgrade; review them before upgrading.

The controller canonicalizes `ipBlock.cidr` and `ipBlock.except` CIDRs before
building OVN matches. For example, `10.0.0.7/24` becomes `10.0.0.0/24`, and
expanded IPv6 addresses use compressed notation. A bare address, such as
`10.0.0.7`, stands for that single host. An `ipBlock` with an empty `cidr` leaves
the destination unrestricted. The exceptions must be of the IP family of the
`cidr`; an IPv4-mapped IPv6 prefix such as `::ffff:10.0.0.0/104` is an IPv6
prefix. The submitted resource is preserved; malformed CIDRs are reported
through the NetworkQoS failure status, and nothing of a NetworkQoS whose spec is
invalid stays programmed until the spec is fixed.
