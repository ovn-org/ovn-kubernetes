package node

import (
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"regexp"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	defaultOpenFlowCookie = "0xdeff105"
)

// nodePortWatcher manages OpenfLow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	ofportPhys  string
	ofportPatch string
	gwBridge    string
	nodeIP      *net.IPNet
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) {
	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	for _, svcPort := range service.Spec.Ports {
		protocol := strings.ToLower(string(svcPort.Protocol))
		protocol6 := protocol + "6"
		if svcPort.NodePort > 0 {
			if config.IPv4Mode {
				_, stderr, err := util.RunOVSOfctl("add-flow", npw.gwBridge,
					fmt.Sprintf("priority=100, in_port=%s, %s, tp_dst=%d, actions=%s",
						npw.ofportPhys, protocol, svcPort.NodePort, npw.ofportPatch))
				if err != nil {
					klog.Errorf("Failed to add openflow flow on %s for nodePort: "+
						"%d, stderr: %q, error: %v", npw.gwBridge, svcPort.NodePort, stderr, err)
				}
			}
			if config.IPv6Mode {
				_, stderr, err := util.RunOVSOfctl("add-flow", npw.gwBridge,
					fmt.Sprintf("priority=100, in_port=%s, %s, tp_dst=%d, actions=%s",
						npw.ofportPhys, protocol6, svcPort.NodePort, npw.ofportPatch))
				if err != nil {
					klog.Errorf("Failed to add openflow flow on %s for nodePort: "+
						"%d, stderr: %q, error: %v", npw.gwBridge, svcPort.NodePort, stderr, err)
				}
			}
		}
		// Flows for cloud load balancers on Azure/GCP
		flows := make([]string, 0)

		// Established traffic is handled by default conntrack rules
		// NodePort/Ingress access in the OVS bridge will only ever come from outside of the host
		for _, ing := range service.Status.LoadBalancer.Ingress {
			if ing.IP == "" {
				continue
			}
			ingIP := net.ParseIP(ing.IP)
			if ingIP == nil {
				klog.Errorf("Failed to parse ingress IP: %s", ing.IP)
				continue
			}
			cookie, err := svcToCookie(service.Name, ingIP, svcPort.Port)
			if err != nil {
				klog.Errorf("Unable to generate cookie for svc: %s, %s, %d, error: %v",
					service.Name, ingIP.String(), svcPort.Port, err)
				continue
			}
			flowProtocol := protocol
			nwDst := "nw_dst"
			if utilnet.IsIPv6String(ing.IP) {
				flowProtocol = protocol6
				nwDst = "ipv6_dst"
			}
			// Send directly to OVN. OVN GR will have load balancer for Ingress IP Service
			flows = append(flows,
				fmt.Sprintf("cookie=%s,priority=100,in_port=%s,%s,%s=%s,tp_dst=%d, actions=%s",
					cookie, npw.ofportPhys, flowProtocol, nwDst, ing.IP, svcPort.Port, npw.ofportPatch))
		}

		for _, flow := range flows {
			_, stderr, err := util.RunOVSOfctl("add-flow", npw.gwBridge, flow)
			if err != nil {
				klog.Errorf("Failed to add openflow flow on %s for ingress, stderr: %q, error: %v, flow: %s",
					npw.gwBridge, stderr, err, flow)
			}
		}

		for _, externalIP := range service.Spec.ExternalIPs {
			flowProtocol := protocol
			nwDst := "nw_dst"
			if utilnet.IsIPv6String(externalIP) {
				flowProtocol = protocol6
				nwDst = "ipv6_dst"
			}
			_, stderr, err := util.RunOVSOfctl("add-flow", npw.gwBridge,
				fmt.Sprintf("priority=100, in_port=%s, %s, %s=%s, tp_dst=%d, actions=%s",
					npw.ofportPhys, flowProtocol, nwDst, externalIP, svcPort.Port, npw.ofportPatch))
			if err != nil {
				klog.Errorf("Failed to add openflow flow on %s for ExternalIP: "+
					"%s, stderr: %q, error: %v", npw.gwBridge, externalIP, stderr, err)
			}
		}
	}

	addSharedGatewayIptRules(service, npw.nodeIP)
}

func (npw *nodePortWatcher) UpdateService(old, new *kapi.Service) {
	if reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return
	}
	npw.DeleteService(old)
	npw.AddService(new)
}

func (npw *nodePortWatcher) DeleteService(service *kapi.Service) {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	for _, svcPort := range service.Spec.Ports {
		protocol := strings.ToLower(string(svcPort.Protocol))
		protocol6 := protocol + "6"
		if svcPort.NodePort > 0 {
			if config.IPv4Mode {
				_, stderr, err := util.RunOVSOfctl("del-flows", npw.gwBridge,
					fmt.Sprintf("in_port=%s, %s, tp_dst=%d",
						npw.ofportPhys, protocol, svcPort.NodePort))
				if err != nil {
					klog.Errorf("Failed to delete openflow flow on %s for nodePort: "+
						"%d, stderr: %q, error: %v", npw.gwBridge, svcPort.NodePort, stderr, err)
				}
			}
			if config.IPv6Mode {
				_, stderr, err := util.RunOVSOfctl("del-flows", npw.gwBridge,
					fmt.Sprintf("in_port=%s, %s, tp_dst=%d",
						npw.ofportPhys, protocol6, svcPort.NodePort))
				if err != nil {
					klog.Errorf("Failed to delete openflow flow on %s for nodePort: "+
						"%d, stderr: %q, error: %v", npw.gwBridge, svcPort.NodePort, stderr, err)
				}
			}
		}
		for _, externalIP := range service.Spec.ExternalIPs {
			if err := util.ValidatePort(svcPort.Protocol, svcPort.Port); err != nil {
				klog.Errorf("Skipping service delete, for svc: %s, err: %v", svcPort.Name, err)
				continue
			}
			flowProtocol := protocol
			nw_dst := "nw_dst"
			if utilnet.IsIPv6String(externalIP) {
				flowProtocol = protocol6
				nw_dst = "ipv6_dst"
			}
			_, stderr, err := util.RunOVSOfctl("del-flows", npw.gwBridge,
				fmt.Sprintf("in_port=%s, %s, %s=%s, tp_dst=%d",
					npw.ofportPhys, flowProtocol, nw_dst, externalIP, svcPort.Port))
			if err != nil {
				klog.Errorf("Failed to delete openflow flow on %s for ExternalIP: "+
					"%s, stderr: %q, error: %v", npw.gwBridge, externalIP, stderr, err)
			}
		}
		for _, ing := range service.Status.LoadBalancer.Ingress {
			if ing.IP == "" {
				continue
			}
			ingIP := net.ParseIP(ing.IP)
			if ingIP == nil {
				klog.Errorf("Failed to parse ingress IP: %s", ing.IP)
				continue
			}
			cookie, err := svcToCookie(service.Name, ingIP, svcPort.Port)
			if err != nil {
				klog.Errorf("Unable to generate cookie for svc: %s, %s, %d, error: %v",
					service.Name, ingIP.String(), svcPort.Port, err)
			}
			_, stderr, err := util.RunOVSOfctl("del-flows", npw.gwBridge,
				fmt.Sprintf("cookie=%s/-1", cookie))
			if err != nil {
				klog.Errorf("Failed to delete openflow flow on %s for Ingress: "+
					"%s, stderr: %q, error: %v", npw.gwBridge, ingIP, stderr, err)
			}
		}
	}

	delSharedGatewayIptRules(service, npw.nodeIP)
}

func (npw *nodePortWatcher) SyncServices(services []interface{}) {
	ports := make(map[string]string)
	ingressCookies := make(map[string]struct{})
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		for _, svcPort := range service.Spec.Ports {
			if svcPort.NodePort > 0 {
				protocol := strings.ToLower(string(svcPort.Protocol))
				nodePortKey := fmt.Sprintf("%s_%d", protocol, svcPort.NodePort)
				ports[nodePortKey] = ""
			}
			for _, externalIP := range service.Spec.ExternalIPs {
				protocol := strings.ToLower(string(svcPort.Protocol))
				externalPortKey := fmt.Sprintf("%s_%d", protocol, svcPort.Port)
				ports[externalPortKey] = externalIP
			}
			for _, ing := range service.Status.LoadBalancer.Ingress {
				ingIP := net.ParseIP(ing.IP)
				if ingIP == nil {
					klog.Errorf("Failed to parse ingress IP: %s", ing.IP)
					continue
				}
				cookie, err := svcToCookie(service.Name, ingIP, svcPort.Port)
				if err != nil {
					klog.Errorf("Unable to generate cookie for svc: %s, %s, %d, error: %v",
						service.Name, ingIP.String(), svcPort.Port, err)
				}
				ingressCookies[cookie] = struct{}{}
			}
		}
	}

	syncSharedGatewayIptRules(services, npw.nodeIP)

	stdout, stderr, err := util.RunOVSOfctl("dump-flows",
		npw.gwBridge)
	if err != nil {
		klog.Errorf("dump-flows failed: %q (%v)", stderr, err)
		return
	}
	flows := strings.Split(stdout, "\n")

	re, err := regexp.Compile(`tp_dst=(.*?)[, ]`)
	if err != nil {
		klog.Errorf("Regexp compile failed: %v", err)
		return
	}

	for _, flow := range flows {
		group := re.FindStringSubmatch(flow)
		if group == nil {
			continue
		}

		var key string
		if strings.Contains(flow, "tcp6") {
			key = fmt.Sprintf("tcp6_%s", group[1])
		} else if strings.Contains(flow, "udp6") {
			key = fmt.Sprintf("udp6_%s", group[1])
		} else if strings.Contains(flow, "sctp6") {
			key = fmt.Sprintf("sctp6_%s", group[1])
		} else if strings.Contains(flow, "tcp") {
			key = fmt.Sprintf("tcp_%s", group[1])
		} else if strings.Contains(flow, "udp") {
			key = fmt.Sprintf("udp_%s", group[1])
		} else if strings.Contains(flow, "sctp") {
			key = fmt.Sprintf("sctp_%s", group[1])
		} else {
			continue
		}

		// FIXME (trozet) this code is buggy, the else statement can never be reached, so external ip flows will hang
		// around on the switch after sync
		// refactor this into flow caching later
		if externalIP, ok := ports[key]; !ok {
			pair := strings.Split(key, "_")
			protocol, port := pair[0], pair[1]
			if externalIP == "" {
				// Check if this is a known cloud load balancer flow
				for cookie := range ingressCookies {
					if strings.Contains(flow, cookie) {
						continue
					}
				}
				stdout, _, err := util.RunOVSOfctl(
					"del-flows", npw.gwBridge,
					fmt.Sprintf("in_port=%s, %s, tp_dst=%s",
						npw.ofportPhys, protocol, port))
				if err != nil {
					klog.Errorf("del-flows of %s failed: %q",
						npw.gwBridge, stdout)
				}
			} else {
				nw_dst := "nw_dst"
				flowProtocol := protocol
				if utilnet.IsIPv6String(externalIP) {
					nw_dst = "ipv6_dst"
					flowProtocol = protocol + "6"
				}
				stdout, _, err := util.RunOVSOfctl(
					"del-flows", npw.gwBridge,
					fmt.Sprintf("in_port=%s, %s, %s=%s, tp_dst=%s",
						npw.ofportPhys, flowProtocol, nw_dst, externalIP, port))
				if err != nil {
					klog.Errorf("del-flows of %s failed: %q",
						npw.gwBridge, stdout)
				}
			}
		}
	}
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to also handle unDNAT return traffic back out of the host
func newSharedGatewayOpenFlowManager(nodeName, macAddress, gwBridge, gwIntf string) (*openflowManager, error) {
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

	// replace the left over OpenFlow flows with the NORMAL action flow
	_, stderr, err = util.AddOFFlowWithSpecificAction(gwBridge, util.NormalAction)
	if err != nil {
		return nil, fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", gwBridge, stderr, err)
	}

	nFlows := 0
	// table 0, we check to see if this dest mac is the shared mac, if so flood to both ports
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=output:%s,output:LOCAL",
			defaultOpenFlowCookie, ofportPhys, macAddress, ofportPatch))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}
	nFlows++

	if config.IPv4Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))
		if err != nil {
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
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
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))
		if err != nil {
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))
		if err != nil {
			return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
				"error: %v", gwBridge, stderr, err)
		}
		nFlows++
	}

	// table 1, established and related connections go to pod
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_state=+trk+est, "+
			"actions=output:%s", defaultOpenFlowCookie, ofportPatch))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	nFlows++

	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=100, table=1, ct_state=+trk+rel, "+
			"actions=output:%s", defaultOpenFlowCookie, ofportPatch))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
			"error: %v", gwBridge, stderr, err)
	}
	nFlows++

	if config.Gateway.DisableSNATMultipleGWs {
		// table 1, traffic to pod subnet go directly to OVN
		for _, clusterEntry := range config.Default.ClusterSubnets {
			cidr := clusterEntry.CIDR
			var ipPrefix string
			if utilnet.IsIPv6CIDR(cidr) {
				ipPrefix = "ipv6"
			} else {
				ipPrefix = "ip"
			}
			_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
				fmt.Sprintf("cookie=%s, priority=15, table=1, %s, %s_dst=%s, actions=output:%s",
					defaultOpenFlowCookie, ipPrefix, ipPrefix, cidr, ofportPatch))
			if err != nil {
				return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
					"error: %v", gwBridge, stderr, err)
			}
			nFlows++
		}
	}

	// table 1, we check to see if this dest mac is the shared mac, if so send to host
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:LOCAL",
			defaultOpenFlowCookie, macAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, error: %v", gwBridge, stderr, err)
	}
	nFlows++

	if config.IPv6Mode {
		// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
		// must flood icmpv6 Route Advertisement and Neighbor Advertisement traffic as it fails to create a CT entry
		for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
			_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
				fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=FLOOD",
					defaultOpenFlowCookie, icmpType))
			if err != nil {
				return nil, fmt.Errorf("failed to add openflow flow to %s, stderr: %q, "+
					"error: %v", gwBridge, stderr, err)
			}
			nFlows++
		}
	}

	// table 1, all other connections do normal processing
	_, stderr, err = util.RunOVSOfctl("add-flow", gwBridge,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))
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

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf string, nodeAnnotator kube.Annotator) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	bridgeName, uplinkName, macAddress, ips, err := gatewayInitInternal(
		nodeName, gwIntf, subnets, gwNextHops, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Shared Gateway Openflow Manager")
		var err error

		gw.openflowManager, err = newSharedGatewayOpenFlowManager(nodeName, macAddress.String(), bridgeName, uplinkName)
		if err != nil {
			return err
		}

		if config.Gateway.NodeportEnable {
			klog.Info("Creating Shared Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(nodeName, bridgeName, uplinkName, ips[0])
			if err != nil {
				return err
			}
		}
		return nil
	}

	klog.Info("Shared Gateway Creation Complete")
	return gw, nil
}

func newNodePortWatcher(nodeName, gwBridge, gwIntf string, nodeIP *net.IPNet) (*nodePortWatcher, error) {
	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	patchPort := "patch-" + gwBridge + "_" + nodeName + "-to-br-int"
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", gwIntf, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	// In the shared gateway mode, the NodePort service is handled by the OpenFlow flows configured
	// on the OVS bridge in the host. These flows act only on the packets coming in from outside
	// of the node. If someone on the node is trying to access the NodePort service, those packets
	// will not be processed by the OpenFlow flows, so we need to add iptable rules that DNATs the
	// NodePortIP:NodePort to ClusterServiceIP:Port.
	if err := initSharedGatewayIPTables(); err != nil {
		return nil, err
	}

	npw := &nodePortWatcher{
		ofportPhys:  ofportPhys,
		ofportPatch: ofportPatch,
		gwBridge:    gwBridge,
		nodeIP:      nodeIP,
	}
	return npw, nil
}

func cleanupSharedGateway() error {
	// NicToBridge() may be created before-hand, only delete the patch port here
	stdout, stderr, err := util.RunOVSVsctl("--columns=name", "--no-heading", "find", "port",
		"external_ids:ovn-localnet-port!=_")
	if err != nil {
		return fmt.Errorf("failed to get ovn-localnet-port port stderr:%s (%v)", stderr, err)
	}
	ports := strings.Fields(strings.Trim(stdout, "\""))
	for _, port := range ports {
		_, stderr, err := util.RunOVSVsctl("--if-exists", "del-port", strings.Trim(port, "\""))
		if err != nil {
			return fmt.Errorf("failed to delete port %s stderr:%s (%v)", port, stderr, err)
		}
	}

	// Get the OVS bridge name from ovn-bridge-mappings
	stdout, stderr, err = util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".",
		"external_ids:ovn-bridge-mappings")
	if err != nil {
		return fmt.Errorf("failed to get ovn-bridge-mappings stderr:%s (%v)", stderr, err)
	}
	// skip the existing mapping setting for the specified physicalNetworkName
	bridgeName := ""
	bridgeMappings := strings.Split(stdout, ",")
	for _, bridgeMapping := range bridgeMappings {
		m := strings.Split(bridgeMapping, ":")
		if network := m[0]; network == types.PhysicalNetworkName {
			bridgeName = m[1]
			break
		}
	}
	if len(bridgeName) == 0 {
		return nil
	}

	_, stderr, err = util.AddOFFlowWithSpecificAction(bridgeName, util.NormalAction)
	if err != nil {
		return fmt.Errorf("failed to replace-flows on bridge %q stderr:%s (%v)", bridgeName, stderr, err)
	}

	cleanupSharedGatewayIPTChains()
	return nil
}

func svcToCookie(name string, ip net.IP, port int32) (string, error) {
	id := fmt.Sprintf("%s%s%d", name, ip.String(), port)
	h := fnv.New64a()
	_, err := h.Write([]byte(id))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("0x%x", h.Sum64()), nil
}
