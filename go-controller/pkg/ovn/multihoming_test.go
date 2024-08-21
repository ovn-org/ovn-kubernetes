package ovn

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type secondaryNetInfo struct {
	netName        string
	nadName        string
	subnets        string
	topology       string
	excludeSubnets string
	extraDest      string
	gateway        string
	isPrimary      bool
}

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

func (em *secondaryNetworkExpectationMachine) expectedLogicalSwitchesAndPorts() []libovsdbtest.TestData {
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
				data = append(data, lsp)
				switch ocInfo.bnc.TopologyType() {
				case ovntypes.Layer3Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(pod.nodeName)

					switchToRouterPortName := "stor-" + switchName
					switchToRouterPortUUID := switchToRouterPortName + "-UUID"
					data = append(data, newExpectedSwitchToRouterPort(switchToRouterPortUUID, switchToRouterPortName, pod, ocInfo.bnc, nad))
					nodeslsps[switchName] = append(nodeslsps[switchName], switchToRouterPortUUID)

					if em.gatewayConfig != nil {
						mgmtPortName := "k8s-isolatednet_test-node"
						mgmtPortUUID := mgmtPortName + "-UUID"
						mgmtPort := &nbdb.LogicalSwitchPort{UUID: mgmtPortUUID, Name: mgmtPortName, Addresses: []string{"02:03:04:05:06:07 192.168.0.2"}}
						data = append(data, mgmtPort)
						nodeslsps[switchName] = append(nodeslsps[switchName], mgmtPortUUID)
						const aclUUID = "acl1-UUID"
						data = append(data, allowAllFromMgmtPort(aclUUID, "192.168.0.2"))
						acls[switchName] = append(acls[switchName], aclUUID)
					}
				case ovntypes.Layer2Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
				case ovntypes.LocalnetTopology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)
				}
				nodeslsps[switchName] = append(nodeslsps[switchName], lspUUID)
			}
			var otherConfig map[string]string
			if ocInfo.bnc.TopologyType() == ovntypes.Layer3Topology {
				subnets := subnetsAsString(ocInfo.bnc.Subnets())
				sn := subnets[0]
				subnet := strings.TrimSuffix(sn, "/24")
				otherConfig = map[string]string{
					"exclude_ips": "192.168.0.2",
					"subnet":      subnet,
				}
			}
			data = append(data, &nbdb.LogicalSwitch{
				UUID:        switchName + "-UUID",
				Name:        switchName,
				Ports:       nodeslsps[switchName],
				ExternalIDs: map[string]string{ovntypes.NetworkExternalID: ocInfo.bnc.GetNetworkName()},
				OtherConfig: otherConfig,
				ACLs:        acls[switchName],
			})
			if em.gatewayConfig != nil {
				data = append(data, expectedGWEntities(pod.nodeName, ocInfo.bnc, *em.gatewayConfig)...)
				data = append(data, expectedLayer3EgressEntities(ocInfo.bnc, *em.gatewayConfig)...)
			}
			if em.isInterconnectCluster {
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
		"arp_proxy":   "0a:58:a9:fe:01:01 169.254.1.1 fe80::1 10.128.0.0/14",
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
