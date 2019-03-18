package cluster

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

func addService(service *kapi.Service, inport, outport, gwBridge string) {
	if service.Spec.Type != kapi.ServiceTypeNodePort {
		return
	}

	for _, svcPort := range service.Spec.Ports {
		if svcPort.Protocol != kapi.ProtocolTCP &&
			svcPort.Protocol != kapi.ProtocolUDP {
			continue
		}
		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("priority=100, in_port=%s, %s, tp_dst=%d, actions=%s",
				inport, protocol, svcPort.NodePort, outport))
		if err != nil {
			logrus.Errorf("Failed to add openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", gwBridge,
				svcPort.NodePort, stderr, err)
		}
	}
}

func deleteService(service *kapi.Service, inport, gwBridge string) {
	if service.Spec.Type != kapi.ServiceTypeNodePort {
		return
	}

	for _, svcPort := range service.Spec.Ports {
		if svcPort.Protocol != kapi.ProtocolTCP &&
			svcPort.Protocol != kapi.ProtocolUDP {
			continue
		}

		protocol := strings.ToLower(string(svcPort.Protocol))

		_, stderr, err := util.RunOVSOfctl("del-flows", gwBridge,
			fmt.Sprintf("in_port=%s, %s, tp_dst=%d",
				inport, protocol, svcPort.NodePort))
		if err != nil {
			logrus.Errorf("Failed to delete openflow flow on %s for nodePort "+
				"%d, stderr: %q, error: %v", gwBridge,
				svcPort.NodePort, stderr, err)
		}
	}
}

func syncServices(services []interface{}, gwBridge, gwIntf string) {
	// Get ofport of physical interface
	inport, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		logrus.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
		return
	}

	nodePorts := make(map[string]bool)
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			logrus.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		if service.Spec.Type != kapi.ServiceTypeNodePort ||
			len(service.Spec.Ports) == 0 {
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			port := svcPort.NodePort
			if port == 0 {
				continue
			}

			prot := svcPort.Protocol
			if prot != kapi.ProtocolTCP && prot != kapi.ProtocolUDP {
				continue
			}
			protocol := strings.ToLower(string(prot))
			nodePortKey := fmt.Sprintf("%s_%d", protocol, port)
			nodePorts[nodePortKey] = true
		}
	}

	stdout, stderr, err := util.RunOVSOfctl("dump-flows",
		gwBridge)
	if err != nil {
		logrus.Errorf("dump-flows failed: %q (%v)", stderr, err)
		return
	}
	flows := strings.Split(stdout, "\n")

	re, err := regexp.Compile(`tp_dst=(.*?)[, ]`)
	if err != nil {
		logrus.Errorf("regexp compile failed: %v", err)
		return
	}

	for _, flow := range flows {
		group := re.FindStringSubmatch(flow)
		if group == nil {
			continue
		}

		var key string
		if strings.Contains(flow, "tcp") {
			key = fmt.Sprintf("tcp_%s", group[1])
		} else if strings.Contains(flow, "udp") {
			key = fmt.Sprintf("udp_%s", group[1])
		} else {
			continue
		}

		if _, ok := nodePorts[key]; !ok {
			pair := strings.Split(key, "_")
			protocol, port := pair[0], pair[1]

			stdout, _, err := util.RunOVSOfctl(
				"del-flows", gwBridge,
				fmt.Sprintf("in_port=%s, %s, tp_dst=%s",
					inport, protocol, port))
			if err != nil {
				logrus.Errorf("del-flows of %s failed: %q",
					gwBridge, stdout)
			}
		}
	}
}

func nodePortWatcher(nodeName, gwBridge, gwIntf string, wf *factory.WatchFactory) error {
	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	patchPort := "patch-" + gwBridge + "_" + nodeName + "-to-br-int"
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		return fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	_, err = wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			addService(service, ofportPhys, ofportPatch, gwBridge)
		},
		UpdateFunc: func(old, new interface{}) {
		},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			deleteService(service, ofportPhys, gwBridge)
		},
	}, func(services []interface{}) {
		syncServices(services, gwBridge, gwIntf)
	})

	return err
}

func addDefaultConntrackRules(nodeName, gwBridge, gwIntf string) error {
	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	localnetLpName := gwBridge + "_" + nodeName
	patchPort := "patch-" + localnetLpName + "-to-br-int"
	// Get ofport of patchPort, but before that make sure ovn-controller created
	// one for us (waits for about ovsCommandTimeout seconds)
	ofportPatch, stderr, err := util.RunOVSVsctl("wait-until", "Interface", patchPort, "ofport>0",
		"--", "get", "Interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("Failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		return fmt.Errorf("Failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// table 0, packets coming from pods headed externally. Commit connections
	// so that reverse direction goes back to the pods.
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("priority=100, in_port=%s, ip, "+
			"actions=ct(commit, zone=%d), output:%s",
			ofportPatch, config.Default.ConntrackZone, ofportPhys))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}

	// table 0, packets coming from external. Send it through conntrack and
	// resubmit to table 1 to know the state of the connection.
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("priority=50, in_port=%s, ip, "+
			"actions=ct(zone=%d, table=1)", ofportPhys, config.Default.ConntrackZone))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}

	// table 1, established and related connections go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("priority=100, table=1, ct_state=+trk+est, "+
			"actions=output:%s", ofportPatch))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("priority=100, table=1, ct_state=+trk+rel, "+
			"actions=output:%s", ofportPatch))
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}

	// table 1, all other connections go to the bridge interface.
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		"priority=0, table=1, actions=output:LOCAL")
	if err != nil {
		return fmt.Errorf("Failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	return nil
}

func addStaticRouteToHost(nodeName, nicIP string) error {
	k8sClusterRouter, err := util.GetK8sClusterRouter()
	if err != nil {
		return err
	}

	k8sMgmtIntfName := util.GetK8sMgmtIntfName(nodeName)
	k8sMgmtIntfIPAddress, err := getIPv4Address(k8sMgmtIntfName)
	if err != nil {
		return fmt.Errorf("failed to get interface IP address for %s (%v)",
			k8sMgmtIntfName, err)
	}
	prefix := strings.Split(nicIP, "/")[0] + "/32"
	nexthop := strings.Split(k8sMgmtIntfIPAddress, "/")[0]
	_, stderr, err := util.RunOVNNbctl("--may-exist", "lr-route-add", k8sClusterRouter, prefix, nexthop)
	if err != nil {
		return fmt.Errorf("failed to add static route '%s via %s' for host %q on %s "+
			"stderr: %q, error: %v", nicIP, k8sMgmtIntfIPAddress, nodeName, k8sClusterRouter, stderr, err)
	}
	return nil
}

func initSharedGateway(
	nodeName string, clusterIPSubnet []string, subnet,
	gwNextHop, gwIntf string, gwVLANId uint, nodeportEnable bool,
	wf *factory.WatchFactory) (string, string, error) {
	var bridgeName string

	// Check to see whether the interface is OVS bridge.
	if _, _, err := util.RunOVSVsctl("--", "br-exists", gwIntf); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err = util.NicToBridge(gwIntf)
		if err != nil {
			return "", "", fmt.Errorf("failed to convert %s to OVS bridge: %v",
				gwIntf, err)
		}
	} else {
		intfName, err := getIntfName(gwIntf)
		if err != nil {
			return "", "", err
		}
		bridgeName = gwIntf
		gwIntf = intfName
	}

	// ovn-bridge-mappings maps a physical network name to a local ovs bridge
	// that provides connectivity to that network.
	_, stderr, err := util.RunOVSVsctl("set", "Open_vSwitch", ".",
		fmt.Sprintf("external_ids:ovn-bridge-mappings=%s:%s", util.PhysicalNetworkName, bridgeName))
	if err != nil {
		return "", "", fmt.Errorf("Failed to set ovn-bridge-mappings for ovs bridge %s"+
			", stderr:%s (%v)", bridgeName, stderr, err)
	}

	// Now, we get IP address from OVS bridge. If IP does not exist,
	// error out.
	ipAddress, err := getIPv4Address(bridgeName)
	if err != nil {
		return "", "", fmt.Errorf("Failed to get interface details for %s (%v)",
			bridgeName, err)
	}
	if ipAddress == "" {
		return "", "", fmt.Errorf("%s does not have a ipv4 address", bridgeName)
	}
	err = util.GatewayInit(clusterIPSubnet, nodeName, ipAddress, "",
		bridgeName, gwNextHop, subnet, gwVLANId, nodeportEnable)
	if err != nil {
		return "", "", fmt.Errorf("failed to init shared interface gateway: %v", err)
	}

	// Add static routes to OVN Cluster Router to enable pods on this Node to
	// reach the host IP
	err = addStaticRouteToHost(nodeName, ipAddress)
	if err != nil {
		return "", "", err
	}

	// Program cluster.GatewayIntf to let non-pod traffic to go to host
	// stack
	if err := addDefaultConntrackRules(nodeName, bridgeName, gwIntf); err != nil {
		return "", "", err
	}

	if nodeportEnable {
		// Program cluster.GatewayIntf to let nodePort traffic to go to pods.
		if err := nodePortWatcher(nodeName, bridgeName, gwIntf, wf); err != nil {
			return "", "", err
		}
	}

	return bridgeName, gwIntf, nil
}
