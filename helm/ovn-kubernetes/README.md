# ovn-kubernetes

-----------------------

![Version: 1.4.0](https://img.shields.io/badge/Version-1.4.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 1.4.0](https://img.shields.io/badge/AppVersion-1.4.0-informational?style=flat-square)

**Homepage:** <https://ovn-kubernetes.io/>

## Source Code

* <https://github.com/ovn-kubernetes/ovn-kubernetes>
* <https://github.com/ovn-org/ovn>

This Helm chart deploys OVN-Kubernetes, an OVN-based CNI plugin for Kubernetes that provides network virtualization, network policy, services, and overlay networking for pods.

For installation instructions, see:

- [Launching OVN-Kubernetes with Helm](../../docs/installation/launching-ovn-kubernetes-with-helm.md) — KinD quickstart and step-by-step install.
- [Launching OVN-Kubernetes with kubeadm](../../docs/installation/INSTALL.KUBEADM.md) — multi-VM walkthrough on a `kubeadm`-bootstrapped cluster.

The rest of this README documents the chart values.

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
			<td>Whether to disable SNAT of egress traffic in namespaces with external gateways configured via AdminPolicyBasedExternalRoute CRs</td>
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
			<td>The secret used for pulling image. Use only if needed. Set create to have the secret created by helm</td>
		</tr>
		<tr>
			<td>global.dummyGatewayBridge</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Create a gateway bridge without a system uplink (for development and CI)</td>
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
			<td>global.enableConfigDuration</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enables monitoring OVN-Kubernetes control plane and OVN configuration duration</td>
		</tr>
		<tr>
			<td>global.enableDNSNameResolver</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to use DNSNameResolver feature with ovn-kubernetes</td>
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
			<td>global.enableNetworkSegmentation</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Configure to use user defined networks (UDN) feature with ovn-kubernetes</td>
		</tr>
		<tr>
			<td>global.enableNetworkQos</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>Enables network QoS support from/to pods</td>
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
			<td>Use SSL transport for the Egress IP gRPC health-check (NB cert paths are reused for this; OVN NB/SB DB connections are always unix sockets)</td>
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
			<td>global.hybridOverlaySubnetWriters</td>
			<td>list</td>
			<td><pre lang="json">
[]
</pre>
</td>
			<td>Users other than ovnkube-cluster-manager allowed to write the hybrid overlay node subnet annotation</td>
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
"ghcr.io/ovn-kubernetes/ovn-kubernetes/ovn-kube-ubuntu"
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
			<td>global.netFlowTargets</td>
			<td>string</td>
			<td><pre lang="json">
""
</pre>
</td>
			<td>A comma separated set of NetFlow collectors to export flow data</td>
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
"169.254.0.0/17"
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
"fd69::/112"
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
			<td>ovnkube-control-plane.replicas</td>
			<td>int</td>
			<td><pre lang="json">
1
</pre>
</td>
			<td>number of ovnkube-control-plane pods</td>
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
  "ovnkube-node-dpu-host": false,
  "ovnkube-single-node-zone-dpu": false
}
</pre>
</td>
			<td>list of dependent subcharts that need to be installed for the given deployment mode, these subcharts haven't been tested yet.</td>
		</tr>
	</tbody>
</table>
