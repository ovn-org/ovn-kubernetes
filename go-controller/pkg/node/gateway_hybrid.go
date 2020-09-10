// +build linux

package node

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog"
	"net"
	"strings"

	utilnet "k8s.io/utils/net"
)

func (n *OvnNode) initHybridGateway(hostSubnets []*net.IPNet, gwIntf string, nodeAnnotator kube.Annotator, defaultGatewayIntf string) (postWaitFunc, error) {
	// Create a localnet OVS bridge.
	localnetBridgeName := "br-local"
	_, stderr, err := util.RunOVSVsctl("--may-exist", "add-br",
		localnetBridgeName)
	if err != nil {
		return nil, fmt.Errorf("failed to create localnet bridge %s"+
			", stderr:%s (%v)", localnetBridgeName, stderr, err)
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(n.name, localnetBridgeName, localnetBridgeName,
		util.PhysicalNetworkName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}
	_, err = util.LinkSetUp(localnetBridgeName)
	if err != nil {
		return nil, err
	}

	// Create a localnet bridge gateway port
	_, stderr, err = util.RunOVSVsctl(
		"--if-exists", "del-port", localnetBridgeName, legacyLocalnetGatewayNextHopPort,
		"--", "--may-exist", "add-port", localnetBridgeName, localnetGatewayNextHopPort,
		"--", "set", "interface", localnetGatewayNextHopPort, "type=internal",
		"mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		fmt.Sprintf("mac=%s", strings.ReplaceAll(localnetGatewayNextHopMac, ":", "\\:")))
	if err != nil {
		return nil, fmt.Errorf("failed to create localnet bridge gateway port %s"+
			", stderr:%s (%v)", localnetGatewayNextHopPort, stderr, err)
	}
	link, err := util.LinkSetUp(localnetGatewayNextHopPort)
	if err != nil {
		return nil, err
	}
	// Flush any addresses on localnetGatewayNextHopPort first
	if err := util.LinkAddrFlush(link); err != nil {
		return nil, err
	}

	var gatewayIfAddrs []*net.IPNet
	var nextHops []net.IP
	for _, hostSubnet := range hostSubnets {
		var gatewayIP, gatewayNextHop net.IP
		var gatewaySubnetMask net.IPMask
		if utilnet.IsIPv6CIDR(hostSubnet) {
			gatewayIP = net.ParseIP(v6localnetGatewayIP)
			gatewayNextHop = net.ParseIP(v6localnetGatewayNextHop)
			gatewaySubnetMask = net.CIDRMask(v6localnetGatewaySubnetPrefix, 128)
		} else {
			gatewayIP = net.ParseIP(v4localnetGatewayIP)
			gatewayNextHop = net.ParseIP(v4localnetGatewayNextHop)
			gatewaySubnetMask = net.CIDRMask(v4localnetGatewaySubnetPrefix, 32)
		}
		gatewayIfAddr := &net.IPNet{IP: gatewayIP, Mask: gatewaySubnetMask}
		gatewayNextHopIfAddr := &net.IPNet{IP: gatewayNextHop, Mask: gatewaySubnetMask}
		gatewayIfAddrs = append(gatewayIfAddrs, gatewayIfAddr)
		nextHops = append(nextHops, gatewayNextHop)

		if err = util.LinkAddrAdd(link, gatewayNextHopIfAddr); err != nil {
			return nil, err
		}
		if utilnet.IsIPv6CIDR(hostSubnet) {
			// TODO - IPv6 hack ... for some reason neighbor discovery isn't working here, so hard code a
			// MAC binding for the gateway IP address for now - need to debug this further
			if exists, _ := util.LinkNeighExists(link, gatewayIP, macAddress); !exists {
				err = util.LinkNeighAdd(link, gatewayIP, macAddress)
				if err == nil {
					klog.Infof("Added MAC binding for %s on %s", gatewayIP, localnetGatewayNextHopPort)
				} else {
					klog.Errorf("Error in adding MAC binding for %s on %s: %v", gatewayIP, localnetGatewayNextHopPort, err)
				}
			}
		}
	}

	n.initLocalEgressIP(gatewayIfAddrs, defaultGatewayIntf)

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, err
	}

	gwMacAddress, _ := net.ParseMAC(localnetGatewayMac)

	defaultCfg := &util.L3GatewayConfig{
		Mode:           config.GatewayModeLocal,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     gwMacAddress,
		IPAddresses:    gatewayIfAddrs,
		NextHops:       nextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
	}

	for _, ifaddr := range gatewayIfAddrs {
		err = initLocalGatewayNATRules(localnetGatewayNextHopPort, ifaddr.IP)
		if err != nil {
			return nil, fmt.Errorf("failed to add NAT rules for localnet gateway (%v)", err)
		}
	}

	var bridgeName string
	var uplinkName string
	var brCreated bool

	if bridgeName, _, err = util.RunOVSVsctl("--", "port-to-br", gwIntf); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err = util.GetNicName(bridgeName)
		if err != nil {
			return nil, err
		}
	} else if _, _, err := util.RunOVSVsctl("--", "br-exists", gwIntf); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err = util.NicToBridge(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %s to OVS bridge: %v",
				gwIntf, err)
		}
		uplinkName = gwIntf
		gwIntf = bridgeName
		brCreated = true
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err = getIntfName(gwIntf)
		if err != nil {
			return nil, err
		}
		bridgeName = gwIntf
	}

	// Now, we get IP addresses from OVS bridge. If IP does not exist,
	// error out.
	ips, err := getNetworkInterfaceIPAddresses(gwIntf)
	if err != nil {
		return nil, fmt.Errorf("failed to get interface details for %s (%v)",
			gwIntf, err)
	}

	if config.Gateway.NodeportEnable {
		localAddrSet, err := getLocalAddrs()
		if err != nil {
			return nil, err
		}
		err = n.watchLocalPorts(
			newLocalPortWatcherData(gatewayIfAddrs, n.recorder, localAddrSet, ips),
		)
		if err != nil {
			return nil, err
		}
	}

	ifaceID, macAddress, err = bridgedGatewayNodeSetup(n.name, bridgeName, gwIntf,
		util.HybridNetworkName, brCreated)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	err = util.SetL3HybridGatewayConfig(nodeAnnotator, defaultCfg, &util.L3GatewayConfig{
		Mode:        config.GatewayModeHybrid,
		ChassisID:   chassisID,
		InterfaceID: ifaceID,
		MACAddress:  macAddress,
		IPAddresses: ips,
	})

	if err != nil {
		return nil, err
	}

	return func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		if err := addDefaultConntrackRulesHybrid(n.name, bridgeName, uplinkName, n.stopChan); err != nil {
			return err
		}
		return nil
	}, nil
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to also handle unDNAT return traffic back out of the host
func addDefaultConntrackRulesHybrid(nodeName, gwBridge, gwIntf string, stopChan chan struct{}) error {
	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	localnetLpName := gwBridge + "_" + nodeName
	patchPort := "patch-" + localnetLpName + "-to-br-int"
	// Get ofport of patchPort, but before that make sure ovn-controller created
	// one for us (waits for about ovsCommandTimeout seconds)
	ofportPatch, stderr, err := util.RunOVSVsctl("wait-until", "Interface", patchPort, "ofport>0",
		"--", "get", "Interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("get", "interface", gwIntf, "ofport")
	if err != nil {
		return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// replace the left over OpenFlow flows with the FLOOD action flow
	_, stderr, err = util.AddFloodActionOFFlow(gwBridge)
	if err != nil {
		return fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", gwBridge, stderr, err)
	}

	nFlows := 0
	if config.IPv4Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=100, table=0, in_port=%s, ip, "+
				"actions=ct(commit, exec(load:0x1->NXM_NX_CT_LABEL), zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, table=0, in_port=%s, ip, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}
	if config.IPv6Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=100, table=0, in_port=%s, ipv6, "+
				"actions=ct(commit, exec(load:0x1->NXM_NX_CT_LABEL), zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, table=0, in_port=%s, ipv6, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}

	// table 0, packets coming from host should go out physical port
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=0, in_port=LOCAL, actions=output:%s",
			defaultOpenFlowCookie, ofportPhys))
	if err != nil {
		return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}
	nFlows++

	// table 0, packets coming from OVN that are not IP should go out of the host
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=99, table=0, in_port=%s, actions=output:%s",
			defaultOpenFlowCookie, ofportPatch, ofportPhys))
	if err != nil {
		return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}

	nFlows++

	// table 1, known connections with ct_label 1 go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_label=0x1, "+
			"actions=output:%s", defaultOpenFlowCookie, ofportPatch))
	if err != nil {
		return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	nFlows++

	// table 1, traffic to pod subnet go directly to OVN
	for _, clusterEntry := range config.Default.ClusterSubnets {
		cidr := clusterEntry.CIDR
		var ipPrefix string
		if cidr.IP.To4() != nil {
			ipPrefix = "ip"
		} else {
			ipPrefix = "ipv6"
		}
		mask, _ := cidr.Mask.Size()
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=3, table=1, %s, %s_dst=%s/%d, actions=output:%s",
				defaultOpenFlowCookie, ipPrefix, ipPrefix, cidr.IP, mask, ofportPatch))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

	}

	if config.IPv6Mode {
		// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
		// must flood icmpv6 traffic as it fails to create a CT entry
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=1, table=1,icmp6 actions=FLOOD", defaultOpenFlowCookie))
		if err != nil {
			return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}

	// table 1, all other connections go to host
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=LOCAL", defaultOpenFlowCookie))
	if err != nil {
		return fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	nFlows++

	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	go checkDefaultConntrackRules(gwBridge, gwIntf, patchPort, ofportPhys, ofportPatch, nFlows, stopChan)
	return nil
}
