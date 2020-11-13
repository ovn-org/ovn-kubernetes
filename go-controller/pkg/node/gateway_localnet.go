// +build linux

package node

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

const (
	// localnetGatewayNextHopPort is the name of the gateway port on the host to which all
	// the packets leaving/entering the OVN logical topology to the host will egress for shared gw mode
	localnetGatewayNextHopPort = "ovn-k8s-gw0"

	// Routing table for ExternalIP communication
	localnetGatewayExternalIDTable = "6"
)

func newLocalGateway(nodeName string, hostSubnets []*net.IPNet, gwNextHops []net.IP, gwIntf string, nodeAnnotator kube.Annotator, recorder record.EventRecorder, cfg *managementPortConfig) (*gateway, error) {
	gw := &gateway{}
	var gatewayIfAddrs []*net.IPNet
	for _, hostSubnet := range hostSubnets {
		// local gateway mode uses mp0 as default path for all ingress traffic into OVN
		var nextHop *net.IPNet
		if utilnet.IsIPv6CIDR(hostSubnet) {
			nextHop = cfg.ipv6.ifAddr
		} else {
			nextHop = cfg.ipv4.ifAddr
		}
		// gatewayIfAddrs are the OVN next hops via mp0
		gatewayIfAddrs = append(gatewayIfAddrs, util.GetNodeGatewayIfAddr(hostSubnet))

		// add iptables masquerading for mp0 to exit the host for egress
		cidr := nextHop.IP.Mask(nextHop.Mask)
		cidrNet := &net.IPNet{IP: cidr, Mask: nextHop.Mask}
		err := initLocalGatewayNATRules(types.K8sMgmtIntfName, cidrNet)
		if err != nil {
			return nil, fmt.Errorf("failed to add local NAT rules for: %s, err: %v", types.K8sMgmtIntfName, err)
		}
	}

	// OCP HACK
	// Do not configure OVS bridge for local gateway mode with a gateway iface of none
	// For SDN->OVN migration, see https://github.com/openshift/ovn-kubernetes/pull/281
	if gwIntf == "none" {
		var err error
		gw.initFunc, err = initSharedGatewayNoBridge(nodeName, hostSubnets, gwNextHops, nodeAnnotator)
		if err != nil {
			return nil, err
		}
		// END OCP HACK
	} else {
		bridgeName, uplinkName, _, _, err := gatewayInitInternal(
			nodeName, gwIntf, hostSubnets, gwNextHops, nodeAnnotator)
		if err != nil {
			return nil, err
		}

		gw.initFunc = func() error {
			klog.Info("Creating Local Gateway Openflow Manager")
			var err error
			gw.openflowManager, err = newLocalGatewayOpenflowManager(nodeName, bridgeName, uplinkName)
			return err
		}
	}

	if config.Gateway.NodeportEnable {
		localAddrSet, err := getLocalAddrs()
		if err != nil {
			return nil, err
		}

		if err := initLocalGatewayIPTables(); err != nil {
			return nil, err
		}
		if err := initRoutingRules(); err != nil {
			return nil, err
		}
		gw.localPortWatcher = newLocalPortWatcher(gatewayIfAddrs, recorder, localAddrSet)
	}

	return gw, nil
}

func getGatewayFamilyAddrs(gatewayIfAddrs []*net.IPNet) (string, string) {
	var gatewayIPv4, gatewayIPv6 string
	for _, gatewayIfAddr := range gatewayIfAddrs {
		if utilnet.IsIPv6(gatewayIfAddr.IP) {
			gatewayIPv6 = gatewayIfAddr.IP.String()
		} else {
			gatewayIPv4 = gatewayIfAddr.IP.String()
		}
	}
	return gatewayIPv4, gatewayIPv6
}

type localPortWatcher struct {
	recorder     record.EventRecorder
	gatewayIPv4  string
	gatewayIPv6  string
	localAddrSet map[string]net.IPNet
}

func newLocalPortWatcher(gatewayIfAddrs []*net.IPNet, recorder record.EventRecorder, localAddrSet map[string]net.IPNet) *localPortWatcher {
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gatewayIfAddrs)
	return &localPortWatcher{
		recorder:     recorder,
		gatewayIPv4:  gatewayIPv4,
		gatewayIPv6:  gatewayIPv6,
		localAddrSet: localAddrSet,
	}
}

func (l *localPortWatcher) AddService(svc *kapi.Service) {
	err := l.addService(svc)
	if err != nil {
		klog.Errorf("Error in adding service: %v", err)
	}
}

func (l *localPortWatcher) UpdateService(old, new *kapi.Service) {
	if reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return
	}
	err := l.deleteService(old)
	if err != nil {
		klog.Errorf("Error in deleting service - %v", err)
	}
	err = l.addService(new)
	if err != nil {
		klog.Errorf("Error in modifying service: %v", err)
	}
}

func (l *localPortWatcher) DeleteService(svc *kapi.Service) {
	err := l.deleteService(svc)
	if err != nil {
		klog.Errorf("Error in deleting service - %v", err)
	}
}

func getLocalAddrs() (map[string]net.IPNet, error) {
	localAddrSet := make(map[string]net.IPNet)
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		ip, ipNet, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, err
		}
		localAddrSet[ip.String()] = *ipNet
	}
	klog.V(5).Infof("Node local addresses initialized to: %v", localAddrSet)
	return localAddrSet, nil
}

func (l *localPortWatcher) networkHasAddress(ip net.IP) bool {
	for _, net := range l.localAddrSet {
		if net.Contains(ip) {
			return true
		}
	}
	return false
}

func (l *localPortWatcher) addService(svc *kapi.Service) error {
	iptRules := []iptRule{}
	isIPv6Service := utilnet.IsIPv6String(svc.Spec.ClusterIP)
	gatewayIP := l.gatewayIPv4
	if isIPv6Service {
		gatewayIP = l.gatewayIPv6
	}
	// holds map of external ips and if they are currently using routes
	routeUsage := make(map[string]bool)
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasClusterIP(svc) {
			// Fix Azure/GCP LoadBalancers. They will forward traffic directly to the node with the
			// dest address as the load-balancer ingress IP and port
			iptRules = append(iptRules, getLoadBalancerIPTRules(svc, port, svc.Spec.ClusterIP, port.Port)...)
		}

		if util.ServiceTypeHasNodePort(svc) {
			if err := util.ValidatePort(port.Protocol, port.NodePort); err != nil {
				klog.Warningf("Invalid service node port %s, err: %v", port.Name, err)
				continue
			}
			if gatewayIP != "" {
				iptRules = append(iptRules, getNodePortIPTRules(port, nil, svc.Spec.ClusterIP, port.Port)...)
				klog.V(5).Infof("Will add iptables rule for NodePort: %v and "+
					"protocol: %v", port.NodePort, port.Protocol)
			} else {
				klog.Warningf("No gateway of appropriate IP family for NodePort Service %s/%s %s",
					svc.Namespace, svc.Name, svc.Spec.ClusterIP)
			}
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if err := util.ValidatePort(port.Protocol, port.Port); err != nil {
				klog.Warningf("Invalid service port %s, err: %v", port.Name, err)
				break
			}
			if utilnet.IsIPv6String(externalIP) != isIPv6Service {
				klog.Warningf("Invalid ExternalIP %s for Service %s/%s with ClusterIP %s",
					externalIP, svc.Namespace, svc.Name, svc.Spec.ClusterIP)
				continue
			}
			if _, exists := l.localAddrSet[externalIP]; exists {
				if !util.IsClusterIPSet(svc) {
					serviceRef := kapi.ObjectReference{
						Kind:      "Service",
						Namespace: svc.Namespace,
						Name:      svc.Name,
					}
					l.recorder.Eventf(&serviceRef, kapi.EventTypeWarning, "UnsupportedServiceDefinition", "Unsupported service definition, headless service: %s with a local ExternalIP is not supported by ovn-kubernetes in local gateway mode", svc.Name)
					klog.Warningf("UnsupportedServiceDefinition event for service %s in namespace %s", svc.Name, svc.Namespace)
					continue
				}
				iptRules = append(iptRules, getExternalIPTRules(port, externalIP, svc.Spec.ClusterIP)...)
				klog.V(5).Infof("Will add iptables rule for ExternalIP: %s", externalIP)
			} else if l.networkHasAddress(net.ParseIP(externalIP)) {
				klog.V(5).Infof("ExternalIP: %s is reachable through one of the interfaces on this node, will skip setup", externalIP)
			} else {
				if gatewayIP != "" {
					routeUsage[externalIP] = true
				} else {
					klog.Warningf("No gateway of appropriate IP family for ExternalIP %s for Service %s/%s",
						externalIP, svc.Namespace, svc.Name)
				}
			}
		}
	}
	for externalIP := range routeUsage {
		if stdout, stderr, err := util.RunIP("route", "replace", externalIP, "via", gatewayIP, "dev", types.K8sMgmtIntfName, "table", localnetGatewayExternalIDTable); err != nil {
			klog.Errorf("Error adding routing table entry for ExternalIP %s, via gw: %s: stdout: %s, stderr: %s, err: %v", externalIP, gatewayIP, stdout, stderr, err)
		} else {
			klog.Infof("Successfully added route for ExternalIP: %s", externalIP)
		}
	}
	klog.Infof("Adding iptables rules: %v for service: %v", iptRules, svc.Name)
	return addIptRules(iptRules)
}

func (l *localPortWatcher) deleteService(svc *kapi.Service) error {
	iptRules := []iptRule{}
	isIPv6Service := utilnet.IsIPv6String(svc.Spec.ClusterIP)
	gatewayIP := l.gatewayIPv4
	if isIPv6Service {
		gatewayIP = l.gatewayIPv6
	}
	// holds map of external ips and if they are currently using routes
	routeUsage := make(map[string]bool)
	// Note that unlike with addService we just silently ignore IPv4/IPv6 mismatches here
	for _, port := range svc.Spec.Ports {
		if util.ServiceTypeHasClusterIP(svc) {
			// Fix Azure/GCP LoadBalancers. They will forward traffic directly to the node with the
			// dest address as the load-balancer ingress IP and port
			iptRules = append(iptRules, getLoadBalancerIPTRules(svc, port, svc.Spec.ClusterIP, port.Port)...)
		}
		if util.ServiceTypeHasNodePort(svc) {
			if gatewayIP != "" {
				iptRules = append(iptRules, getNodePortIPTRules(port, nil, svc.Spec.ClusterIP, port.Port)...)
				klog.V(5).Infof("Will delete iptables rule for NodePort: %v and "+
					"protocol: %v", port.NodePort, port.Protocol)
			}
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			if utilnet.IsIPv6String(externalIP) != isIPv6Service {
				continue
			}
			if _, exists := l.localAddrSet[externalIP]; exists {
				iptRules = append(iptRules, getExternalIPTRules(port, externalIP, svc.Spec.ClusterIP)...)
				klog.V(5).Infof("Will delete iptables rule for ExternalIP: %s", externalIP)
			} else if l.networkHasAddress(net.ParseIP(externalIP)) {
				klog.V(5).Infof("ExternalIP: %s is reachable through one of the interfaces on this node, will skip cleanup", externalIP)
			} else {
				if gatewayIP != "" {
					routeUsage[externalIP] = true
				}
			}
		}
	}

	for externalIP := range routeUsage {
		if stdout, stderr, err := util.RunIP("route", "del", externalIP, "via", gatewayIP, "dev", types.K8sMgmtIntfName, "table", localnetGatewayExternalIDTable); err != nil {
			klog.Errorf("Error delete routing table entry for ExternalIP %s: stdout: %s, stderr: %s, err: %v", externalIP, stdout, stderr, err)
		} else {
			klog.Infof("Successfully deleted route for ExternalIP: %s", externalIP)
		}
	}

	klog.Infof("Deleting iptables rules: %v for service: %v", iptRules, svc.Name)
	return delIptRules(iptRules)
}

func (l *localPortWatcher) SyncServices(serviceInterface []interface{}) {
	removeStaleRoutes := func(keepRoutes []string) {
		stdout, stderr, err := util.RunIP("route", "list", "table", localnetGatewayExternalIDTable)
		if err != nil || stdout == "" {
			klog.Infof("No routing table entries for ExternalIP table %s: stdout: %s, stderr: %s, err: %v",
				localnetGatewayExternalIDTable, stdout, strings.Replace(stderr, "\n", "", -1), err)
			return
		}
		for _, existingRoute := range strings.Split(stdout, "\n") {
			isFound := false
			for _, keepRoute := range keepRoutes {
				if strings.Contains(existingRoute, keepRoute) {
					isFound = true
					break
				}
			}
			if !isFound {
				klog.Infof("Deleting stale routing rule: %s", existingRoute)
				if _, stderr, err := util.RunIP("route", "del", existingRoute, "table", localnetGatewayExternalIDTable); err != nil {
					klog.Errorf("Error deleting stale routing rule: stderr: %s, err: %v", stderr, err)
				}
			}
		}
	}
	keepIPTRules := []iptRule{}
	keepRoutes := []string{}
	for _, service := range serviceInterface {
		svc, ok := service.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v", serviceInterface)
			continue
		}
		gatewayIP := l.gatewayIPv4
		if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
			gatewayIP = l.gatewayIPv6
		}
		if gatewayIP != "" {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(svc, gatewayIP, nil)...)
		}
		keepRoutes = append(keepRoutes, svc.Spec.ExternalIPs...)
	}
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		recreateIPTRules("nat", chain, keepIPTRules)
		recreateIPTRules("filter", chain, keepIPTRules)
	}
	removeStaleRoutes(keepRoutes)
}

func initRoutingRules() error {
	stdout, stderr, err := util.RunIP("rule")
	if err != nil {
		return fmt.Errorf("error listing routing rules, stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, fmt.Sprintf("from all lookup %s", localnetGatewayExternalIDTable)) {
		if stdout, stderr, err := util.RunIP("rule", "add", "from", "all", "table", localnetGatewayExternalIDTable); err != nil {
			return fmt.Errorf("error adding routing rule for ExternalIP table (%s): stdout: %s, stderr: %s, err: %v", localnetGatewayExternalIDTable, stdout, stderr, err)
		}
	}
	return nil
}

func cleanupLocalnetGateway(physnet string) error {
	stdout, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if physnet == m[0] {
			bridgeName := m[1]
			_, stderr, err = util.RunOVSVsctl("--", "--if-exists", "del-br", bridgeName)
			if err != nil {
				return fmt.Errorf("failed to ovs-vsctl del-br %s stderr:%s (%v)", bridgeName, stderr, err)
			}
			break
		}
	}
	return err
}

func getLoadBalancerIPTRules(svc *kapi.Service, svcPort kapi.ServicePort, gatewayIP string, targetPort int32) []iptRule {
	var rules []iptRule
	ingPort := fmt.Sprintf("%d", svcPort.Port)
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		rules = append(rules, iptRule{
			table: "nat",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "DNAT", "--to-destination", util.JoinHostPortInt32(gatewayIP, targetPort),
			},
		})
		rules = append(rules, iptRule{
			table: "filter",
			chain: iptableNodePortChain,
			args: []string{
				"-d", ing.IP,
				"-p", string(svcPort.Protocol), "--dport", ingPort,
				"-j", "ACCEPT",
			},
		})
	}
	return rules
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to also handle unDNAT return traffic back out of the host
func newLocalGatewayOpenflowManager(nodeName, gwBridge, gwIntf string) (*openflowManager, error) {
	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	localnetLpName := gwBridge + "_" + nodeName
	patchPort := "patch-" + localnetLpName + "-to-br-int"
	// Get ofport of patchPort, but before that make sure ovn-controller created
	// one for us (waits for about ovsCommandTimeout seconds)
	ofportPatch, stderr, err := util.RunOVSVsctl("wait-until", "Interface", patchPort, "ofport>0",
		"--", "get", "Interface", patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("get", "interface", gwIntf, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// replace the left over OpenFlow flows with the FLOOD action flow
	_, stderr, err = util.AddOFFlowWithSpecificAction(gwBridge, util.FloodAction)
	if err != nil {
		return nil, fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", gwBridge, stderr, err)
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
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, table=0, in_port=%s, ip, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))
		if err != nil {
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
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
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, table=0, in_port=%s, ipv6, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))
		if err != nil {
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}

	// table 0, packets coming from host should go out physical port
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=0, in_port=LOCAL, actions=output:%s",
			defaultOpenFlowCookie, ofportPhys))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}
	nFlows++

	// table 0, packets coming from OVN that are not IP should go out of the host
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=99, table=0, in_port=%s, actions=output:%s",
			defaultOpenFlowCookie, ofportPatch, ofportPhys))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}

	nFlows++

	// table 1, known connections with ct_label 1 go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_label=0x1, "+
			"actions=output:%s", defaultOpenFlowCookie, ofportPatch))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
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
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
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
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}

	// table 1, all other connections go to host
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=LOCAL", defaultOpenFlowCookie))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	nFlows++

	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	return &openflowManager{
		gwBridge, gwIntf, patchPort, ofportPhys, ofportPatch, nFlows,
	}, nil
}
