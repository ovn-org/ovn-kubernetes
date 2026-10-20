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
qos-external   NetworkQoS Destinations applied
```

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
| **networkSelectors[]** | list `NetworkSelector` | No | Restricts the rule to traffic on specific networks. If absent, the rule matches any interface. *(See §5.2)* |
| **priority** | `int` | **Yes** | Higher number → chosen first when multiple `NetworkQoS` objects match the same packet. |
| **egress[]** | list `EgressRule` | **Yes** | One or more marking / policing rules. Evaluated in the order listed. *(See §5.3)* |

Note the square-bracket notation (`[]`) for **both** `egress` and `networkSelectors`—each is an array in the CRD.

---

### **5.2  Inside a `networkSelectors[]` entry**

Each list element tells the controller **where** the pods' egress traffic must flow in order to apply the rule. Exactly **one** selector type must be set.

| Key | Required | Description |
| :---- | :---- | :---- |
| `networkSelectionType` | **Yes** | Enum that declares which selector below is populated. Common values: `NetworkAttachmentDefinitions`, `DefaultNetwork`, `SecondaryUserDefinedNetworks`, … |
| `networkAttachmentDefinitionSelector` | conditional | When `networkSelectionType=NetworkAttachmentDefinitions`. Selects NADs by **namespaceSelector** (required) *and* **networkSelector** (required). Both are ordinary `LabelSelectors`. |
| `secondaryUserDefinedNetworkSelector` | conditional | Used when `networkSelectionType=SecondaryUserDefinedNetworks`. Similar structure: required **namespaceSelector** & **networkSelector**. |
| `clusterUserDefinedNetworkSelector`, `primaryUserDefinedNetworkSelector` | conditional | Additional selector styles, each with required sub‑selectors as per the CRD. |

**Typical usage** – `networkSelectionType: NetworkAttachmentDefinitions` + `networkAttachmentDefinitionSelector`.

---

### **5.3  Inside an `egress[]` rule**

| Field | Type | Required | Description |
| :---- | :---- | :---- | :---- |
| `dscp` | `int` (0 – 63) | **Yes** | DSCP value to stamp on the **inner** IP header. This value determines the traffic priority. |
| `bandwidth.rate` | `int` (kbps) | No | Sustained rate for the token-bucket policer (in kilobits per second). |
| `bandwidth.burst` | `int` (kilobits) | No | Maximum burst size that can accrue (in kilobits). |
| `classifier.to` / `classifier.from` | list `TrafficSelector` | No | CIDRs the packet destination (or source) must match. Each entry is an `ipBlock` supporting an `except` list. |
| `classifier.ports[]` | list | No | List of `{protocol, port}` tuples the packet must match; protocol is `TCP`, `UDP`, or `SCTP`. |

If **all** specified classifier conditions match, the packet gets the DSCP mark and/or bandwidth policer defined above. This allows for fine-grained control over which traffic flows receive QoS treatment.
