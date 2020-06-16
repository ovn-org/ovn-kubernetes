package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	// defaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	defaultOpenFlowCookie = "0xdeff105"
)

func (npw *sharedGatewayNodePortWatcher) AddService(service *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(service) {
		return nil
	}

	for _, svcPort := range service.Spec.Ports {
		_, err := util.ValidateProtocol(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Skipping service add. Invalid service port %s: %v", svcPort.Name, err)
			continue
		}
		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("add-flow", npw.gwBridge,
			fmt.Sprintf("priority=100, in_port=%s, %s, tp_dst=%d, actions=%s",
				npw.ofPortPhys, protocol, svcPort.NodePort, npw.ofPortPatch))
		if err != nil {
			return fmt.Errorf("Failed to add openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", npw.gwBridge,
				svcPort.NodePort, stderr, err)
		}
	}

	addSharedGatewayIptRules(service, npw.nodeIP[0])
	return nil
}

func (npw *sharedGatewayNodePortWatcher) DeleteService(service *kapi.Service) error {
	if !util.ServiceTypeHasNodePort(service) {
		return nil
	}

	for _, svcPort := range service.Spec.Ports {
		_, err := util.ValidateProtocol(svcPort.Protocol)
		if err != nil {
			klog.Errorf("Skipping service delete. Invalid service port %s: %v", svcPort.Name, err)
			continue
		}

		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("del-flows", npw.gwBridge,
			fmt.Sprintf("in_port=%s, %s, tp_dst=%d",
				npw.ofPortPhys, protocol, svcPort.NodePort))
		if err != nil {
			return fmt.Errorf("Failed to delete openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", npw.gwBridge,
				svcPort.NodePort, stderr, err)
		}
	}
	delSharedGatewayIptRules(service, npw.nodeIP[0])
	return nil
}

type sharedGatewayNodePortWatcher struct {
	nodeName    string
	gwBridge    string
	gwIntf      string
	nodeIP      []*net.IPNet
	patchPort   string
	ofPortPatch string
	ofPortPhys  string
}

func newSharedGatewayNodePortWatcher(nodeName, gwBridge, gwIntf string, nodeIP []*net.IPNet) (nodePortWatcher, error) {
	npw := &sharedGatewayNodePortWatcher{
		nodeName: nodeName,
		gwBridge: gwBridge,
		gwIntf:   gwIntf,
		nodeIP:   nodeIP,
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	npw.patchPort = "patch-" + gwBridge + "_" + nodeName + "-to-br-int"
	// Get ofport of patchPort
	var stderr string
	var err error
	npw.ofPortPatch, stderr, err = util.RunOVSVsctl("--if-exists", "get",
		"interface", npw.patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			npw.patchPort, stderr, err)
	}

	// Get ofport of physical interface
	npw.ofPortPhys, stderr, err = util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		return nil, fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// In the shared gateway mode, the NodePort service is handled by the OpenFlow flows configured
	// on the OVS bridge in the host. These flows act only on the packets coming in from outside
	// of the node. If someone on the node is trying to access the NodePort service, those packets
	// will not be processed by the OpenFlow flows, so we need to add iptable rules that DNATs the
	// NodePortIP:NodePort to ClusterServiceIP:Port.
	err = createNodePortIptableChain()
	if err != nil {
		return nil, err
	}

	return npw, nil
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
func addDefaultConntrackRules(nodeName, gwBridge, gwIntf string) (*sharedGatewayHealthcheck, error) {
	hc := &sharedGatewayHealthcheck{
		gwBridge: gwBridge,
		gwIntf:   gwIntf,
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	localnetLpName := gwBridge + "_" + nodeName
	hc.patchPort = "patch-" + localnetLpName + "-to-br-int"
	// Get ofport of patchPort, but before that make sure ovn-controller created
	// one for us (waits for about ovsCommandTimeout seconds)
	var stderr string
	var err error
	hc.ofPortPatch, stderr, err = util.RunOVSVsctl("wait-until", "Interface", hc.patchPort, "ofport>0",
		"--", "get", "Interface", hc.patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("Failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", hc.patchPort, stderr, err)
	}

	// Get ofport of physical interface
	hc.ofPortPhys, stderr, err = util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		return nil, fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// replace the left over OpenFlow flows with the NORMAL action flow
	_, stderr, err = util.AddNormalActionOFFlow(gwBridge)
	if err != nil {
		return nil, fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", gwBridge, stderr, err)
	}

	hc.nFlows = 0
	// table 0, packets coming from pods headed externally. Commit connections
	// so that reverse direction goes back to the pods.
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
			"actions=ct(commit, zone=%d), output:%s",
			defaultOpenFlowCookie, hc.ofPortPatch, config.Default.ConntrackZone, hc.ofPortPhys))
	if err != nil {
		return nil, fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	hc.nFlows++

	// table 0, packets coming from external. Send it through conntrack and
	// resubmit to table 1 to know the state of the connection.
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
			"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, hc.ofPortPhys, config.Default.ConntrackZone))
	if err != nil {
		return nil, fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	hc.nFlows++

	// table 1, established and related connections go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_state=+trk+est, "+
			"actions=output:%s", defaultOpenFlowCookie, hc.ofPortPatch))
	if err != nil {
		return nil, fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	hc.nFlows++

	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_state=+trk+rel, "+
			"actions=output:%s", defaultOpenFlowCookie, hc.ofPortPatch))
	if err != nil {
		return nil, fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	hc.nFlows++

	// table 1, all other connections do normal processing
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))
	if err != nil {
		return nil, fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	hc.nFlows++

	return hc, nil
}

type sharedGatewayHealthcheck struct {
	gwBridge    string
	gwIntf      string
	patchPort   string
	ofPortPhys  string
	ofPortPatch string
	nFlows      int
}

func (n *OvnNode) initSharedGateway(subnet *net.IPNet, gwNextHop net.IP, gwIntf string,
	nodeAnnotator kube.Annotator) (postWaitFunc, error) {
	var bridgeName string
	var uplinkName string
	var brCreated bool
	var err error

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

	// Now, we get IP address from OVS bridge. If IP does not exist,
	// error out.
	ipAddress, err := getIPv4Address(gwIntf)
	if err != nil {
		return nil, fmt.Errorf("Failed to get interface details for %s (%v)",
			gwIntf, err)
	}
	if ipAddress == nil {
		return nil, fmt.Errorf("%s does not have a ipv4 address", gwIntf)
	}

	ifaceID, macAddress, err := bridgedGatewayNodeSetup(n.name, bridgeName, gwIntf,
		util.PhysicalNetworkName, brCreated)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	err = setupLocalNodeAccessBridge(n.name, subnet)
	if err != nil {
		return nil, err
	}
	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, err
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &util.L3GatewayConfig{
		Mode:           config.GatewayModeShared,
		ChassisID:      chassisID,
		InterfaceID:    ifaceID,
		MACAddress:     macAddress,
		IPAddresses:    []*net.IPNet{ipAddress},
		NextHops:       []net.IP{gwNextHop},
		NodePortEnable: config.Gateway.NodeportEnable,
		VLANID:         &config.Gateway.VLANID,
	})
	if err != nil {
		return nil, err
	}

	return func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		var err error
		if n.sharedGatewayHealthcheck, err = addDefaultConntrackRules(n.name, bridgeName, uplinkName); err != nil {
			return err
		}

		if config.Gateway.NodeportEnable {
			// Program cluster.GatewayIntf to let nodePort traffic to go to pods.
			n.npw, err = newSharedGatewayNodePortWatcher(n.name, bridgeName, uplinkName, []*net.IPNet{ipAddress})
			if err != nil {
				return err
			}
		}
		return nil
	}, nil
}

func cleanupSharedGateway() error {
	// NicToBridge() may be created before-hand, only delete the patch port here
	stdout, stderr, err := util.RunOVSVsctl("--columns=name", "--no-heading", "find", "port",
		"external_ids:ovn-localnet-port!=_")
	if err != nil {
		return fmt.Errorf("Failed to get ovn-localnet-port port stderr:%s (%v)", stderr, err)
	}
	ports := strings.Fields(strings.Trim(stdout, "\""))
	for _, port := range ports {
		_, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", strings.Trim(port, "\""))
		if err != nil {
			return fmt.Errorf("Failed to delete port %s stderr:%s (%v)", port, stderr, err)
		}
	}

	// Get the OVS bridge name from ovn-bridge-mappings
	stdout, stderr, err = util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("Failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	bridgeName := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network == util.PhysicalNetworkName {
			bridgeName = m[1]
			break
		}
	}
	if len(bridgeName) == 0 {
		return nil
	}

	_, stderr, err = util.AddNormalActionOFFlow(bridgeName)
	if err != nil {
		return fmt.Errorf("Failed to replace-flows on bridge %q stderr:%s (%v)", bridgeName, stderr, err)
	}

	deleteNodePortIptableChain()
	return nil
}
