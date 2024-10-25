package ovn

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	v1 "k8s.io/api/core/v1"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (p testPod) addNetwork(
	netName, nadName, nodeSubnet, nodeMgtIP, nodeGWIP, podIP, podMAC, role string,
	tunnelID int,
	routes []util.PodRoute,
) {
	podInfo, ok := p.secondaryPodInfos[netName]
	if !ok {
		podInfo = &secondaryPodInfo{
			nodeSubnet:  nodeSubnet,
			nodeMgtIP:   nodeMgtIP,
			nodeGWIP:    nodeGWIP,
			role:        role,
			routes:      routes,
			allportInfo: map[string]portInfo{},
		}
		p.secondaryPodInfos[netName] = podInfo
	}

	prefixLen, ip := splitPodIPMaskLength(podIP)

	portName := util.GetSecondaryNetworkLogicalPortName(p.namespace, p.podName, nadName)
	podInfo.allportInfo[nadName] = portInfo{
		portUUID:  portName + "-UUID",
		podIP:     ip,
		podMAC:    podMAC,
		portName:  portName,
		tunnelID:  tunnelID,
		prefixLen: prefixLen,
	}
}

func (p testPod) getNetworkPortInfo(netName, nadName string) *portInfo {
	podInfo, ok := p.secondaryPodInfos[netName]
	if !ok {
		return nil
	}
	info, ok := podInfo.allportInfo[nadName]
	if !ok {
		return nil
	}

	return &info
}

func splitPodIPMaskLength(podIP string) (int, string) {
	var prefixLen int
	ip, ipNet, err := net.ParseCIDR(podIP)
	if err != nil || ipNet == nil {
		return 0, podIP // falling back to the test's default - e.g. 24 for v4 / 64 for v6
	}
	prefixLen, _ = ipNet.Mask.Size()
	return prefixLen, ip.String()
}

type option func(machine *secondaryNetworkExpectationMachine)

type secondaryNetworkExpectationMachine struct {
	fakeOvn               *FakeOVN
	pods                  []testPod
	gatewayConfig         *util.L3GatewayConfig
	isInterconnectCluster bool
}

func newSecondaryNetworkExpectationMachine(fakeOvn *FakeOVN, pods []testPod, opts ...option) *secondaryNetworkExpectationMachine {
	machine := &secondaryNetworkExpectationMachine{
		fakeOvn: fakeOvn,
		pods:    pods,
	}

	for _, opt := range opts {
		opt(machine)
	}
	return machine
}

func withGatewayConfig(config *util.L3GatewayConfig) option {
	return func(machine *secondaryNetworkExpectationMachine) {
		machine.gatewayConfig = config
	}
}

func withInterconnectCluster() option {
	return func(machine *secondaryNetworkExpectationMachine) {
		machine.isInterconnectCluster = true
	}
}

func (em *secondaryNetworkExpectationMachine) expectedLogicalSwitchesAndPorts(isPrimary bool) []libovsdbtest.TestData {
	data := []libovsdbtest.TestData{}
	for _, ocInfo := range em.fakeOvn.secondaryControllers {
		nodeslsps := make(map[string][]string)
		acls := make(map[string][]string)
		var switchName string
		for _, pod := range em.pods {
			podInfo, ok := pod.secondaryPodInfos[ocInfo.bnc.GetNetworkName()]
			if !ok {
				continue
			}
			subnets := podInfo.nodeSubnet
			var (
				subnet     *net.IPNet
				hasSubnets bool
			)
			if len(subnets) > 0 {
				subnet = ovntest.MustParseIPNet(subnets)
				hasSubnets = true
			}

			for nad, portInfo := range podInfo.allportInfo {
				portName := portInfo.portName
				var lspUUID string
				if len(portInfo.portUUID) == 0 {
					lspUUID = portName + "-UUID"
				} else {
					lspUUID = portInfo.portUUID
				}
				podAddr := fmt.Sprintf("%s %s", portInfo.podMAC, portInfo.podIP)
				lsp := newExpectedSwitchPort(lspUUID, portName, podAddr, pod, ocInfo.bnc, nad)

				if pod.noIfaceIdVer {
					delete(lsp.Options, "iface-id-ver")
				}
				if ocInfo.bnc.isLayer2Interconnect() {
					lsp.Options["requested-tnl-key"] = "1" // hardcode this for now.
				}
				data = append(data, lsp)
				switch ocInfo.bnc.TopologyType() {
				case ovntypes.Layer3Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(pod.nodeName)
					managementIP := managementPortIP(subnet)

					switchToRouterPortName := "stor-" + switchName
					switchToRouterPortUUID := switchToRouterPortName + "-UUID"
					data = append(data, newExpectedSwitchToRouterPort(switchToRouterPortUUID, switchToRouterPortName, pod, ocInfo.bnc, nad))
					nodeslsps[switchName] = append(nodeslsps[switchName], switchToRouterPortUUID)

					if em.gatewayConfig != nil {
						mgmtPortName := managementPortName(switchName)
						mgmtPortUUID := mgmtPortName + "-UUID"
						mgmtPort := expectedManagementPort(mgmtPortName, managementIP.String())
						data = append(data, mgmtPort)
						nodeslsps[switchName] = append(nodeslsps[switchName], mgmtPortUUID)
						const aclUUID = "acl1-UUID"
						data = append(
							data,
							allowAllFromMgmtPort(aclUUID, managementIP.String(), switchName),
						)
						acls[switchName] = append(acls[switchName], aclUUID)
					}
				case ovntypes.Layer2Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
					managementIP := managementPortIP(subnet)

					if em.gatewayConfig != nil {
						// there are multiple mgmt ports in the cluster, thus the ports must be scoped with the node name
						mgmtPortName := managementPortName(ocInfo.bnc.GetNetworkScopedName(nodeName))
						mgmtPortUUID := mgmtPortName + "-UUID"
						mgmtPort := expectedManagementPort(mgmtPortName, managementIP.String())
						data = append(data, mgmtPort)
						nodeslsps[switchName] = append(nodeslsps[switchName], mgmtPortUUID)

						networkSwitchToGWRouterLSPName := ovntypes.SwitchToRouterPrefix + switchName
						networkSwitchToGWRouterLSPUUID := networkSwitchToGWRouterLSPName + "-UUID"
						lsp := &nbdb.LogicalSwitchPort{
							UUID:      networkSwitchToGWRouterLSPUUID,
							Name:      networkSwitchToGWRouterLSPName,
							Addresses: []string{"router"},
							ExternalIDs: map[string]string{
								"k8s.ovn.org/topology": ocInfo.bnc.TopologyType(),
								"k8s.ovn.org/network":  ocInfo.bnc.GetNetworkName(),
							},
							Options: map[string]string{"router-port": ovntypes.RouterToSwitchPrefix + switchName},
							Type:    "router",
						}
						data = append(data, lsp)
						if util.IsNetworkSegmentationSupportEnabled() && ocInfo.bnc.IsPrimaryNetwork() {
							lsp.Options["requested-tnl-key"] = "25"
						}
						nodeslsps[switchName] = append(nodeslsps[switchName], networkSwitchToGWRouterLSPUUID)

						const aclUUID = "acl1-UUID"
						data = append(data, allowAllFromMgmtPort(aclUUID, managementIP.String(), switchName))
						acls[switchName] = append(acls[switchName], aclUUID)
					}

				case ovntypes.LocalnetTopology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)
				}
				nodeslsps[switchName] = append(nodeslsps[switchName], lspUUID)
			}

			var otherConfig map[string]string
			if hasSubnets {
				otherConfig = map[string]string{
					"subnet": subnet.String(),
				}
				if !ocInfo.bnc.IsPrimaryNetwork() {
					// FIXME: This is weird that for secondary networks that don't have
					// management ports these tests are expecting managementportIP to be
					// excluded for no reason.
					// FIXME2: Why are we setting exclude_ips on OVN switches when we don't
					// even use OVN IPAMs.
					otherConfig["exclude_ips"] = managementPortIP(subnet).String()
				}
			}

			// TODO: once we start the "full" SecondaryLayer2NetworkController (instead of just Base)
			// we can drop this, and compare all objects created by the controller (right now we're
			// missing all the meters, and the COPP)
			if ocInfo.bnc.TopologyType() == ovntypes.Layer2Topology {
				otherConfig = nil
			}

			data = append(data, &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: nodeslsps[switchName],
				ExternalIDs: map[string]string{
					ovntypes.NetworkExternalID:     ocInfo.bnc.GetNetworkName(),
					ovntypes.NetworkRoleExternalID: util.GetUserDefinedNetworkRole(isPrimary),
				},
				OtherConfig: otherConfig,
				ACLs:        acls[switchName],
			})
			if em.gatewayConfig != nil {
				if ocInfo.bnc.TopologyType() == ovntypes.Layer3Topology {
					data = append(data, expectedGWEntities(pod.nodeName, subnets, ocInfo.bnc, *em.gatewayConfig)...)
					data = append(data, expectedLayer3EgressEntities(ocInfo.bnc, *em.gatewayConfig, subnet)...)
				} else {
					data = append(data, expectedLayer2EgressEntities(ocInfo.bnc, *em.gatewayConfig, pod.nodeName)...)
				}
			}
			if em.isInterconnectCluster && ocInfo.bnc.TopologyType() == ovntypes.Layer3Topology {
				transitSwitchName := ocInfo.bnc.GetNetworkName() + "_transit_switch"
				data = append(data, &nbdb.LogicalSwitch{
					UUID: transitSwitchName + "-UUID",
					Name: transitSwitchName,
					OtherConfig: map[string]string{
						"mcast_querier":            "false",
						"mcast_flood_unregistered": "true",
						"interconn-ts":             transitSwitchName,
						"requested-tnl-key":        "16711685",
						"mcast_snoop":              "true",
					},
				})
			}
		}

	}
	return data
}

func newExpectedSwitchPort(lspUUID string, portName string, podAddr string, pod testPod, netInfo util.NetInfo, nad string) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		UUID:      lspUUID,
		Name:      portName,
		Addresses: []string{podAddr},
		ExternalIDs: map[string]string{
			"pod":                       "true",
			"namespace":                 pod.namespace,
			ovntypes.NetworkExternalID:  netInfo.GetNetworkName(),
			ovntypes.NADExternalID:      nad,
			ovntypes.TopologyExternalID: netInfo.TopologyType(),
		},
		Options: map[string]string{
			"requested-chassis": pod.nodeName,
			"iface-id-ver":      pod.podName,
		},
		PortSecurity: []string{podAddr},
	}
}

func newExpectedSwitchToRouterPort(lspUUID string, portName string, pod testPod, netInfo util.NetInfo, nad string) *nbdb.LogicalSwitchPort {
	lrp := newExpectedSwitchPort(lspUUID, portName, "router", pod, netInfo, nad)
	lrp.ExternalIDs = nil
	lrp.Options = map[string]string{
		"router-port": "rtos-isolatednet_test-node",
	}
	lrp.PortSecurity = nil
	lrp.Type = "router"
	return lrp
}

func subnetsAsString(subnetInfo []config.CIDRNetworkEntry) []string {
	var subnets []string
	for _, cidr := range subnetInfo {
		subnets = append(subnets, cidr.String())
	}
	return subnets
}

func managementPortName(switchName string) string {
	return fmt.Sprintf("k8s-%s", switchName)
}

func expectedManagementPort(portName string, ip string) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		UUID:      portName + "-UUID",
		Addresses: []string{fmt.Sprintf("02:03:04:05:06:07 %s", ip)},
		Name:      portName,
	}
}

func gwRouterExternalIDs(netInfo util.NetInfo, gwConfig util.L3GatewayConfig) map[string]string {
	return map[string]string{
		ovntypes.NetworkExternalID:     netInfo.GetNetworkName(),
		ovntypes.NetworkRoleExternalID: getNetworkRole(netInfo),
		ovntypes.TopologyExternalID:    netInfo.TopologyType(),
		"physical_ip":                  hostPhysicalIP(gwConfig),
		"physical_ips":                 strings.Join(hostIPsFromGWConfig(gwConfig), ","),
	}
}

func hostPhysicalIP(gwConfig util.L3GatewayConfig) string {
	var physIP string
	if len(gwConfig.IPAddresses) > 0 {
		physIP = gwConfig.IPAddresses[0].IP.String()
	}
	return physIP
}

func hostIPsFromGWConfig(gwConfig util.L3GatewayConfig) []string {
	var hostIPs []string
	for _, ip := range append(gwConfig.IPAddresses, dummyMasqueradeIP()) {
		hostIPs = append(hostIPs, ip.IP.String())
	}
	return hostIPs
}

func newDummyGatewayManager(
	kube kube.InterfaceOVN,
	nbClient libovsdbclient.Client,
	netInfo util.NetInfo,
	factory *factory.WatchFactory,
	nodeName string,
) *GatewayManager {
	return NewGatewayManager(
		nodeName,
		"",
		kube,
		nbClient,
		netInfo,
		factory,
	)
}

func managementPortIP(subnet *net.IPNet) net.IP {
	return util.GetNodeManagementIfAddr(subnet).IP
}

func minimalFeatureConfig() *config.OVNKubernetesFeatureConfig {
	return &config.OVNKubernetesFeatureConfig{
		EnableNetworkSegmentation: true,
		EnableMultiNetwork:        true,
	}
}

func enableICFeatureConfig() *config.OVNKubernetesFeatureConfig {
	featConfig := minimalFeatureConfig()
	featConfig.EnableInterconnect = true
	return featConfig
}

func icClusterTestConfiguration() testConfiguration {
	return testConfiguration{
		configToOverride:   enableICFeatureConfig(),
		expectationOptions: []option{withInterconnectCluster()},
	}
}

func nonICClusterTestConfiguration() testConfiguration {
	return testConfiguration{}
}

func icClusterWithDisableSNATTestConfiguration() testConfiguration {
	return testConfiguration{
		configToOverride:   enableICFeatureConfig(),
		expectationOptions: []option{withInterconnectCluster()},
		gatewayConfig:      &config.GatewayConfig{DisableSNATMultipleGWs: true},
	}
}

func newMultiHomedPod(testPod testPod, multiHomingConfigs ...secondaryNetInfo) *v1.Pod {
	pod := newPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
	var secondaryNetworks []nadapi.NetworkSelectionElement
	for _, multiHomingConf := range multiHomingConfigs {
		if multiHomingConf.isPrimary {
			continue // these will be automatically plugged in
		}
		nadNamePair := strings.Split(multiHomingConf.nadName, "/")
		ns := pod.Namespace
		attachmentName := multiHomingConf.nadName
		if len(nadNamePair) > 1 {
			ns = nadNamePair[0]
			attachmentName = nadNamePair[1]
		}
		nse := nadapi.NetworkSelectionElement{
			Name:      attachmentName,
			Namespace: ns,
		}
		secondaryNetworks = append(secondaryNetworks, nse)
	}
	serializedNetworkSelectionElements, _ := json.Marshal(secondaryNetworks)
	pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: string(serializedNetworkSelectionElements)}
	if config.OVNKubernetesFeature.EnableInterconnect {
		dummyOVNNetAnnotations := dummyOVNPodNetworkAnnotations(testPod.secondaryPodInfos, multiHomingConfigs)
		if dummyOVNNetAnnotations != "{}" {
			pod.Annotations["k8s.ovn.org/pod-networks"] = dummyOVNNetAnnotations
		}
	}
	return pod
}

func dummyOVNPodNetworkAnnotations(secondaryPodInfos map[string]*secondaryPodInfo, multiHomingConfigs []secondaryNetInfo) string {
	var ovnPodNetworksAnnotations []byte
	podAnnotations := map[string]podAnnotation{}
	for i, netConfig := range multiHomingConfigs {
		// we need to inject a dummy OVN annotation into the pods for each multihoming config
		// for layer2 topology since allocating the annotation for this cluster configuration
		// is performed by cluster manager - which doesn't exist in the unit tests.
		if netConfig.topology == ovntypes.Layer2Topology {
			portInfo := secondaryPodInfos[netConfig.netName].allportInfo[netConfig.nadName]
			podAnnotations[netConfig.nadName] = dummyOVNPodNetworkAnnotationForNetwork(portInfo, netConfig, i+1)
		}
	}

	var err error
	ovnPodNetworksAnnotations, err = json.Marshal(podAnnotations)
	if err != nil {
		panic(fmt.Errorf("failed to marshal the pod annotations: %w", err))
	}
	return string(ovnPodNetworksAnnotations)
}

func dummyOVNPodNetworkAnnotationForNetwork(portInfo portInfo, netConfig secondaryNetInfo, tunnelID int) podAnnotation {
	role := ovntypes.NetworkRoleSecondary
	if netConfig.isPrimary {
		role = ovntypes.NetworkRolePrimary
	}
	var gateways []string
	for _, subnetStr := range strings.Split(netConfig.clustersubnets, ",") {
		subnet := testing.MustParseIPNet(subnetStr)
		gateways = append(gateways, util.GetNodeGatewayIfAddr(subnet).IP.String())
	}
	ip := testing.MustParseIP(portInfo.podIP)
	_, maskSize := util.GetIPFullMask(ip).Size()
	ipNet := net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(portInfo.prefixLen, maskSize),
	}
	return podAnnotation{
		IPs:      []string{ipNet.String()},
		MAC:      util.IPAddrToHWAddr(ip).String(),
		Gateways: gateways,
		Routes:   nil, // TODO: must add here the expected routes.
		TunnelID: tunnelID,
		Role:     role,
	}
}

// Internal struct used to marshal PodAnnotation to the pod annotationç
// Copied from pkg/util/pod_annotation.go
type podAnnotation struct {
	IPs      []string   `json:"ip_addresses"`
	MAC      string     `json:"mac_address"`
	Gateways []string   `json:"gateway_ips,omitempty"`
	Routes   []podRoute `json:"routes,omitempty"`

	IP      string `json:"ip_address,omitempty"`
	Gateway string `json:"gateway_ip,omitempty"`

	TunnelID int    `json:"tunnel_id,omitempty"`
	Role     string `json:"role,omitempty"`
}

// Internal struct used to marshal PodRoute to the pod annotation
// Copied from pkg/util/pod_annotation.go
type podRoute struct {
	Dest    string `json:"dest"`
	NextHop string `json:"nextHop"`
}
