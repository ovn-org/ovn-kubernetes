# ovn-kubernetes

-----------------------

![Version: 1.0.0](https://img.shields.io/badge/Version-1.0.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 1.0.0](https://img.shields.io/badge/AppVersion-1.0.0-informational?style=flat-square)

**Homepage:** <https://ovn-kubernetes.io/>

## Source Code

* <https://github.com/ovn-org/ovn-kubernetes>
* <https://github.com/ovn-org/ovn>

## Introduction

This helm chart supports deploying OVN K8s CNI in a K8s cluster.

Open Virtual Networking (OVN) Kubernetes CNI is an open source networking and
network security solution for Kubernetes workloads. It leverages a distributed
OVN SDN control plane and per-node Open vSwitch (OVS) to provide network
virtualization and network connectivity to K8s Pods. It does so by creating a logical
network topology using logical constructs such as logical switches (Layer 2) and
logical routers (Layer 3). The Pod interfaces are represented by logical ports on
the logical switches. On these logical switch ports, one can specify IP network
information (IP address and MAC address), anti-spoofing rules (MAC and IP),
Security Groups, QoS configuration, and so on.

A port, either physical SR-IOV VF or virtual VETH, assigned to a Pod will be associated
with a corresponding logical port, this will result in applying all the logical port
configuration onto the physical port. The logical port becomes the API for
configuring the physical port.

In addition to providing overlay network connectivity for Pods in the K8s cluster,
OVN K8s CNI supports a plethora of advanced networking features, such as

```
- Optimized and Accelerated K8s Network Policy on Pod's traffic
- Optimized and Accelerated K8s Service Implementation (aka Load Balancers and NAT)
- Optimized and Accelerated Policy Based Routing
- Multi-Home Pods with an option for Secondary networks to be on a Layer-2
  Overlay (flat network), Layer-2 Underlay (VLAN-based) on private or public
  subnets.
- Optimized and Accelerated K8s Network Policy on Pod's secondary networks
```

Most of these services are distributed and implemented via a pipeline (series
of OpenFlow tables with OpenFlow flows) on local OVS switches. These OVS
pipelines are very amenable to offloading to NIC hardware, which should result
in the best possible networking performance and CPU savings on the host.

The OVN K8s CNI architecture is a layered architecture with OVS at the bottom,
followed by OVN, and finally OVN K8s CNI at the top. Each layer has several
K8s components - deployments, daemonsets, and statefulsets. Each component at
every layer is a subchart by itself. Based on the deployment needs, all or
some of these subcharts are installed to provide the aforementioned OVN K8s
CNI features, this can be done by editing `tags` section in values.yaml file.

## Quickstart:
Run script `helm/basic-deploy.sh` to set up a basic OVN/Kubernetes cluster.

## Manual steps:
- Disable IPv6 of `kind` docker network, otherwise ovnkube-node will fail to start
```
# docker network rm kind (delete `kind` network if it already exists)
# docker network create kind -o "com.docker.network.bridge.enable_ip_masquerade"="true" -o "com.docker.network.driver.mtu"="1500"
```

- Launch a Kind cluster without CNI and kubeproxy (additional controle-plane or worker nodes can be added)
```
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
```

- Optional: build local image and load it into Kind nodes
```
# cd dist/images
# make ubuntu
# docker tag ovn-kube-ubuntu:latest ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-ubuntu:master
# kind load docker-image ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-ubuntu:master
```

- Run `helm install` with propery `k8sAPIServer`, `ovnkube-identity.replicas`, image repo and tag
```
# cd helm/ovn-kubernetes
# helm install ovn-kubernetes . -f values.yaml --set k8sAPIServer="https://$(kubectl get pods -n kube-system -l component=kube-apiserver -o jsonpath='{.items[0].status.hostIP}'):6443" --set ovnkube-identity.replicas=$(kubectl get node -l node-role.kubernetes.io/control-plane --no-headers | wc -l) --set global.image.repository=ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-ubuntu --set global.image.tag=master
```

## Notes:
- Only following scenarios were tested with Kind cluster
  - ovs-node + ovnkube-node + ovnkube-db + ovnkube-master, with/without ovnkube-identity
  - ovs-node + ovnkube-node + ovnkube-db-raft + ovnkube-master, with/without ovnkube-identity

Following section describes the meaning of the values.

## Values

<table>
	<thead>
		<th>Key</th>
		<th>Type</th>
		<th>Default</th>
		<th>Description</th>
	</thead>
	<tbody>
		<tr>
			<td>global.aclLoggingRateLimit</td>
			<td>int</td>
			<td><pre lang="json">
20
</pre>
</td>
			<td>The largest number of messages per second that gets logged before drop @default 20</td>
		</tr>
		<tr>
			<td>global.disableForwarding</td>
			<td>string</td>
			<td><pre lang="">
false
</pre>
</td>
			<td>Controls if forwarding is allowed on OVNK controlled interfaces</td>
		</tr>
		<tr>
			<td>global.disableIfaceIdVer</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Deprecated: iface-id-ver is always enabled</td>
		</tr>
		<tr>
			<td>global.disablePacketMtuCheck</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Disables adding openflow flows to check packets too large to be delivered to OVN due to pod MTU being lower than NIC MTU</td>
		</tr>
		<tr>
			<td>global.disableSnatMultipleGws</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Whether to disable SNAT of egress traffic in namespaces annotated with routing-external-gws</td>
		</tr>
		<tr>
			<td>global.dockerConfigSecret</td>
			<td>object</td>
			<td><pre lang="json">
{
  "auth": "blah_blah_blah",
  "create": false,
  "registry": "ghcr.io"
}
</pre>
</td>
			<td>The secret used for pulling image. Use only if needed. Set create to have have secret created by helm</td>
		</tr>
		<tr>
			<td>global.egressIpHealthCheckPort</td>
			<td>int</td>
			<td><pre lang="json">
9107
</pre>
</td>
			<td>Configure EgressIP node reachability using gRPC on this TCP port</td>
		</tr>
		<tr>
			<td>global.emptyLbEvents</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>If set, then load balancers do not get deleted when all backends are removed</td>
		</tr>
		<tr>
			<td>global.enableAdminNetworkPolicy</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Whether or not to use Admin Network Policy CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableCompactMode</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Indicate if ovnkube run master and node in one process</td>
		</tr>
		<tr>
			<td>global.enableConfigDuration</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enables monitoring OVN-Kubernetes master and OVN configuration duration</td>
		</tr>
		<tr>
			<td>global.enableEgressFirewall</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use EgressFirewall CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableEgressIp</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use EgressIP CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableEgressQos</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use EgressQoS CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableEgressService</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use EgressService CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableHybridOverlay</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Whether or not to enable hybrid overlay functionality</td>
		</tr>
		<tr>
			<td>global.enableInterConnect</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to enable interconnecting multiple zones</td>
		</tr>
		<tr>
			<td>global.enableIpsec</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to enable IPsec</td>
		</tr>
		<tr>
			<td>global.enableLFlowCache</td>
			<td>bool</td>
			<td><pre lang="">
true
</pre>
</td>
			<td>Indicates if ovn-controller should enable/disable the logical flow in-memory cache when processing Southbound database logical flow changes</td>
		</tr>
		<tr>
			<td>global.enableMetricsScale</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enables metrics related to scaling</td>
		</tr>
		<tr>
			<td>global.enableMultiExternalGateway</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use AdminPolicyBasedExternalRoute CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableMultiNetwork</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to use multiple NetworkAttachmentDefinition CRD feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableMulticast</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enables multicast support between the pods within the same namespace</td>
		</tr>
		<tr>
			<td>global.enableOvnKubeIdentity</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Whether or not enable ovnkube identity webhook</td>
		</tr>
		<tr>
			<td>global.enablePersistentIPs</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to use the IPAMClaims CRD feature with ovn-kubernetes, thus granting persistent IPs across restarts / migration for KubeVirt VMs</td>
		</tr>
		<tr>
			<td>global.enableSsl</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Use SSL transport to NB/SB db and northd</td>
		</tr>
		<tr>
			<td>global.enableStatelessNetworkPolicy</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to use stateless network policy feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableSvcTemplate</td>
			<td>bool</td>
			<td><pre lang="json">
true
</pre>
</td>
			<td>Configure to use service template feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.encapPort</td>
			<td>int</td>
			<td><pre lang="json">
6081
</pre>
</td>
			<td>GENEVE UDP port (default 6081)</td>
		</tr>
		<tr>
			<td>global.extGatewayNetworkInterface</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The interface on nodes that will be used for external gateway network traffic</td>
		</tr>
		<tr>
			<td>global.gatewayMode</td>
			<td>string</td>
			<td><pre lang="json">
"shared"
</pre>
</td>
			<td>The gateway mode (shared or local), if not given, gateway functionality is disabled</td>
		</tr>
		<tr>
			<td>global.gatewayOpts</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Optional extra gateway options</td>
		</tr>
		<tr>
			<td>global.hybridOverlayNetCidr</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>A comma separated set of IP subnets and the associated hostsubnetlengths (eg, \"10.128.0.0/14/23,10.0.0.0/14/23\") to use with the extended hybrid network</td>
		</tr>
		<tr>
			<td>global.image.pullPolicy</td>
			<td>string</td>
			<td><pre lang="json">
"IfNotPresent"
</pre>
</td>
			<td>Image pull policy</td>
		</tr>
		<tr>
			<td>global.image.repository</td>
			<td>string</td>
			<td><pre lang="json">
"ghcr.io/ovn-org/ovn-kubernetes/ovn-kube-ubuntu"
</pre>
</td>
			<td>Image repository for ovn-kubernetes components</td>
		</tr>
		<tr>
			<td>global.image.tag</td>
			<td>string</td>
			<td><pre lang="json">
"master"
</pre>
</td>
			<td>Specify image tag to run</td>
		</tr>
		<tr>
			<td>global.imagePullSecretName</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The name of secret used for pulling image. Use only if needed</td>
		</tr>
		<tr>
			<td>global.ipfixCacheActiveTimeout</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Maximum period in seconds for which an IPFIX flow record is cached and aggregated before being sent @default 60</td>
		</tr>
		<tr>
			<td>global.ipfixCacheMaxFlows</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Maximum number of IPFIX flow records that can be cached at a time @default 0, meaning disabled</td>
		</tr>
		<tr>
			<td>global.ipfixSampling</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Rate at which packets should be sampled and sent to each target collector @default 400</td>
		</tr>
		<tr>
			<td>global.ipfixTargets</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>A comma separated set of IPFIX collectors to export flow data</td>
		</tr>
		<tr>
			<td>global.lFlowCacheLimit</td>
			<td>string</td>
			<td><pre lang="">
unlimited
</pre>
</td>
			<td>Maximum number of logical flow cache entries ovn-controller may create when the logical flow cache is enabled</td>
		</tr>
		<tr>
			<td>global.lFlowCacheLimitKb</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Maximum size of the logical flow cache (in KB) ovn-controller may create when the logical flow cache is enabled</td>
		</tr>
		<tr>
			<td>global.libovsdbClientLogFile</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Separate log file for libovsdb client </td>
		</tr>
		<tr>
			<td>global.monitorAll</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enable monitoring all data from SB DB instead of conditionally monitoring the data relevant to this node only @default true</td>
		</tr>
		<tr>
			<td>global.nbPort</td>
			<td>int</td>
			<td><pre lang="json">
6641
</pre>
</td>
			<td>Port of north bound ovsdb</td>
		</tr>
		<tr>
			<td>global.netFlowTargets</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>A comma separated set of NetFlow collectors to export flow data</td>
		</tr>
		<tr>
			<td>global.nodeMgmtPortNetdev</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The net device to be used for management port, will be renamed to ovn-k8s-mp0 and used to allow host network services and pods to access k8s pod and service networks</td>
		</tr>
		<tr>
			<td>global.ofctrlWaitBeforeClear</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>ovn-controller wait time in ms before clearing OpenFlow rules during start up @default 0</td>
		</tr>
		<tr>
			<td>global.remoteProbeInterval</td>
			<td>int</td>
			<td><pre lang="json">
100000
</pre>
</td>
			<td>OVN remote probe interval in ms  @default 100000</td>
		</tr>
		<tr>
			<td>global.sbPort</td>
			<td>int</td>
			<td><pre lang="json">
6642
</pre>
</td>
			<td>Port of south bound ovsdb</td>
		</tr>
		<tr>
			<td>global.sflowTargets</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>A comma separated set of SFlow collectors to export flow data</td>
		</tr>
		<tr>
			<td>global.unprivilegedMode</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>This allows ovnkube-node to run without SYS_ADMIN capability, by performing interface setup in the CNI plugin</td>
		</tr>
		<tr>
			<td>global.v4JoinSubnet</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The v4 join subnet used for assigning join switch IPv4 addresses</td>
		</tr>
		<tr>
			<td>global.v4MasqueradeSubnet</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The v4 masquerade subnet used for assigning masquerade IPv4 addresses</td>
		</tr>
		<tr>
			<td>global.v6JoinSubnet</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The v6 join subnet used for assigning join switch IPv6 addresses</td>
		</tr>
		<tr>
			<td>global.v6MasqueradeSubnet</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>The v6 masquerade subnet used for assigning masquerade IPv6 addresses</td>
		</tr>
		<tr>
			<td>k8sAPIServer</td>
			<td>string</td>
			<td><pre lang="json">
"https://172.25.0.2:6443"
</pre>
</td>
			<td>Endpoint of Kubernetes api server</td>
		</tr>
		<tr>
			<td>monitoring</td>
			<td>object</td>
			<td><pre lang="json">
{
  "commonServiceMonitorSelectorLabels": {
    "release": "kube-prometheus-stack"
  }
}
</pre>
</td>
			<td>prometheus monitoring related fields</td>
		</tr>
		<tr>
			<td>monitoring.commonServiceMonitorSelectorLabels</td>
			<td>object</td>
			<td><pre lang="json">
{
  "release": "kube-prometheus-stack"
}
</pre>
</td>
			<td>specify the labels for serviceMonitors to be selected for target discovery. Prometheus operator defines what namespaces and what servicemonitors within these namespaces must be selected for target discovery. The fields defined below helps in defining that.</td>
		</tr>
		<tr>
			<td>mtu</td>
			<td>int</td>
			<td><pre lang="json">
1400
</pre>
</td>
			<td>MTU of network interface in a Kubernetes pod</td>
		</tr>
		<tr>
			<td>ovnkube-identity.replicas</td>
			<td>int</td>
			<td><pre lang="json">
1
</pre>
</td>
			<td>number of ovnube-identity pods, co-located with kube-apiserver process, so need to be the same number of control plane nodes</td>
		</tr>
		<tr>
			<td>podNetwork</td>
			<td>string</td>
			<td><pre lang="json">
"10.244.0.0/16/24"
</pre>
</td>
			<td>IP range for Kubernetes pods, /14 is the top level range, under which each /23 range will be assigned to a node</td>
		</tr>
		<tr>
			<td>serviceNetwork</td>
			<td>string</td>
			<td><pre lang="json">
"10.96.0.0/16"
</pre>
</td>
			<td>A comma-separated set of CIDR notation IP ranges from which k8s assigns service cluster IPs. This should be the same as the value provided for kube-apiserver "--service-cluster-ip-range" option</td>
		</tr>
		<tr>
			<td>skipCallToK8s</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Whether or not call `lookup` Helm function, set it to `true` if you want to run `helm dry-run/template/lint`</td>
		</tr>
		<tr>
			<td>tags</td>
			<td>object</td>
			<td><pre lang="json">
{
  "ovn-ipsec": false,
  "ovnkube-control-plane": false,
  "ovnkube-db-raft": false,
  "ovnkube-node-dpu": false,
  "ovnkube-node-dpu-host": false,
  "ovnkube-single-node-zone": false,
  "ovnkube-zone-controller": false
}
</pre>
</td>
			<td>list of dependent subcharts that need to be installed for the given deployment mode, these subcharts haven't been tested yet.</td>
		</tr>
	</tbody>
</table>

