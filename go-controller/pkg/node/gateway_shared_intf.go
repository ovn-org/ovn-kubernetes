package node

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"

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
	ofm         *openflowManager
}

func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add bool) {
	var cookie, key string
	var err error

	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	var actions string
	if config.Gateway.DisablePacketMTUCheck {
		actions = fmt.Sprintf("output:%s", npw.ofportPatch)
	} else {
		// check packet length larger than MTU + eth header - vlan overhead
		// send to table 11 to check if it needs to go to kernel for ICMP needs frag
		actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
	}

	// cookie is only used for debugging purpose. so it is not fatal error if cookie is failed to be generated.
	for _, svcPort := range service.Spec.Ports {
		protocol := strings.ToLower(string(svcPort.Protocol))
		if svcPort.NodePort > 0 {
			flowProtocols := []string{}
			if config.IPv4Mode {
				flowProtocols = append(flowProtocols, protocol)
			}
			if config.IPv6Mode {
				flowProtocols = append(flowProtocols, protocol+"6")
			}
			for _, flowProtocol := range flowProtocols {
				cookie, err = svcToCookie(service.Namespace, service.Name, flowProtocol, svcPort.NodePort)
				if err != nil {
					klog.Warningf("Unable to generate cookie for nodePort svc: %s, %s, %s, %d, error: %v",
						service.Namespace, service.Name, flowProtocol, svcPort.Port, err)
					cookie = "0"
				}
				key = strings.Join([]string{"NodePort", service.Namespace, service.Name, flowProtocol, fmt.Sprintf("%d", svcPort.NodePort)}, "_")
				if !add {
					npw.ofm.deleteFlowsByKey(key)
				} else {
					npw.ofm.updateFlowCacheEntry(key, []string{
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, "+
							"actions=%s",
							cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, actions),
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_src=%d, "+
							"actions=output:%s",
							cookie, npw.ofportPatch, flowProtocol, svcPort.NodePort, npw.ofportPhys)})
				}
			}
		}

		// Flows for cloud load balancers on Azure/GCP
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
			cookie, err = svcToCookie(service.Namespace, service.Name, ingIP.String(), svcPort.Port)
			if err != nil {
				klog.Warningf("Unable to generate cookie for ingress svc: %s, %s, %s, %d, error: %v",
					service.Namespace, service.Name, ingIP.String(), svcPort.Port, err)
				cookie = "0"
			}
			flowProtocol := protocol
			nwDst := "nw_dst"
			nwSrc := "nw_src"
			if utilnet.IsIPv6String(ing.IP) {
				flowProtocol = protocol + "6"
				nwDst = "ipv6_dst"
				nwSrc = "ipv6_src"
			}
			key = strings.Join([]string{"Ingress", service.Namespace, service.Name, ingIP.String(), fmt.Sprintf("%d", svcPort.Port)}, "_")
			if !add {
				npw.ofm.deleteFlowsByKey(key)
			} else {
				npw.ofm.updateFlowCacheEntry(key, []string{
					fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
						"actions=%s",
						cookie, npw.ofportPhys, flowProtocol, nwDst, ing.IP, svcPort.Port, actions),
					fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_src=%d, "+
						"actions=output:%s",
						cookie, npw.ofportPatch, flowProtocol, nwSrc, ing.IP, svcPort.Port, npw.ofportPhys)})
			}
		}

		for _, externalIP := range service.Spec.ExternalIPs {
			flowProtocol := protocol
			nwDst := "nw_dst"
			nwSrc := "nw_src"
			if utilnet.IsIPv6String(externalIP) {
				flowProtocol = protocol + "6"
				nwDst = "ipv6_dst"
				nwSrc = "ipv6_src"
			}
			cookie, err = svcToCookie(service.Namespace, service.Name, externalIP, svcPort.Port)
			if err != nil {
				klog.Warningf("Unable to generate cookie for external svc: %s, %s, %s, %d, error: %v",
					service.Namespace, service.Name, externalIP, svcPort.Port, err)
				cookie = "0"
			}
			key := strings.Join([]string{"External", service.Namespace, service.Name, externalIP, fmt.Sprintf("%d", svcPort.Port)}, "_")
			if !add {
				npw.ofm.deleteFlowsByKey(key)
			} else {
				npw.ofm.updateFlowCacheEntry(key, []string{
					fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
						"actions=%s",
						cookie, npw.ofportPhys, flowProtocol, nwDst, externalIP, svcPort.Port, actions),
					fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_src=%d, "+
						"actions=output:%s",
						cookie, npw.ofportPatch, flowProtocol, nwSrc, externalIP, svcPort.Port, npw.ofportPhys)})
			}
		}
	}
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) {

	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	npw.updateServiceFlowCache(service, true)
	npw.ofm.requestFlowSync()
	addSharedGatewayIptRules(service)
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
	needFlowSync := false
	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		npw.updateServiceFlowCache(old, false)
		delSharedGatewayIptRules(old)
		needFlowSync = true
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		npw.updateServiceFlowCache(new, true)
		addSharedGatewayIptRules(new)
		needFlowSync = true
	}

	if needFlowSync {
		npw.ofm.requestFlowSync()
	}
}

func (npw *nodePortWatcher) DeleteService(service *kapi.Service) {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	npw.updateServiceFlowCache(service, false)
	npw.ofm.requestFlowSync()
	delSharedGatewayIptRules(service)
}

func (npw *nodePortWatcher) SyncServices(services []interface{}) {
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		npw.updateServiceFlowCache(service, true)
	}

	npw.ofm.requestFlowSync()
	syncSharedGatewayIptRules(services)
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
func newSharedGatewayOpenFlowManager(patchPort, macAddress, gwBridge, gwIntf string, ips []*net.IPNet) (*openflowManager, error) {
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", gwIntf, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwIntf, stderr, err)
	}

	HostMasqCTZone := config.Default.ConntrackZone + 1
	OVNMasqCTZone := HostMasqCTZone + 1
	var dftFlows []string

	// table 0, we check to see if this dest mac is the shared mac, if so flood to both ports
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=output:%s,output:LOCAL",
			defaultOpenFlowCookie, ofportPhys, macAddress, ofportPatch))

	if config.IPv4Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))

		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=60, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
				"actions=output:LOCAL", defaultOpenFlowCookie, ofportPhys, macAddress, config.Default.EncapPort))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=55, in_port=%s, udp, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofportPhys, config.Default.EncapPort))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))

		physicalIP, err := util.MatchIPNetFamily(false, ips)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofportPatch, types.V4HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, types.V4OVNMasqueradeIP, OVNMasqCTZone))
	}
	if config.IPv6Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofportPatch, config.Default.ConntrackZone, ofportPhys))

		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=60, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
				"actions=output:LOCAL", defaultOpenFlowCookie, ofportPhys, macAddress, config.Default.EncapPort))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=55, in_port=%s, udp6, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofportPhys, config.Default.EncapPort))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofportPhys, config.Default.ConntrackZone))

		physicalIP, err := util.MatchIPNetFamily(true, ips)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofportPatch, types.V6HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ipv6, ipv6_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, types.V6OVNMasqueradeIP, OVNMasqCTZone))
	}

	var protoPrefix string
	var masqIP string

	// table 0, packets coming from Host -> Service
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = "ip"
			masqIP = types.V4HostMasqueradeIP
		} else {
			protoPrefix = "ipv6"
			masqIP = types.V6HostMasqueradeIP
		}

		// table 0, Host -> OVN towards SVC, SNAT to special IP
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, %s, %s_dst=%s,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				defaultOpenFlowCookie, protoPrefix, protoPrefix, svcCIDR, HostMasqCTZone, masqIP))

		// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
				"actions=ct(zone=%d,nat,table=3)",
				defaultOpenFlowCookie, ofportPatch, protoPrefix, protoPrefix, svcCIDR,
				protoPrefix, masqIP, HostMasqCTZone))
	}

	var actions string
	if config.Gateway.DisablePacketMTUCheck {
		actions = fmt.Sprintf("output:%s", ofportPatch)
	} else {
		// check packet length larger than MTU + eth header - vlan overhead
		// send to table 11 to check if it needs to go to kernel for ICMP needs frag
		actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
	}

	if config.IPv4Mode {
		// table 1, established and related connections in zone 64000 go to OVN
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, "+
				"actions=%s",
				defaultOpenFlowCookie, actions))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, "+
				"actions=%s",
				defaultOpenFlowCookie, actions))
	}

	if config.IPv6Mode {
		// table 1, established and related connections in zone 64000 go to OVN
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, "+
				"actions=%s",
				defaultOpenFlowCookie, actions))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+rel, "+
				"actions=%s",
				defaultOpenFlowCookie, actions))
	}

	if config.Gateway.DisableSNATMultipleGWs {
		// table 1, traffic to pod subnet go directly to OVN
		// check packet length larger than MTU + eth header - vlan overhead
		// send to table 11 to check if it needs to go to kernel for ICMP needs frag/packet too big
		for _, clusterEntry := range config.Default.ClusterSubnets {
			cidr := clusterEntry.CIDR
			var ipPrefix string
			if utilnet.IsIPv6CIDR(cidr) {
				ipPrefix = "ipv6"
			} else {
				ipPrefix = "ip"
			}
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=15, table=1, %s, %s_dst=%s, "+
					"actions=%s",
					defaultOpenFlowCookie, ipPrefix, ipPrefix, cidr, actions))
		}
	}

	// table 1, we check to see if this dest mac is the shared mac, if so send to host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:LOCAL",
			defaultOpenFlowCookie, macAddress))

	if config.IPv6Mode {
		// REMOVEME(trozet) when https://bugzilla.kernel.org/show_bug.cgi?id=11797 is resolved
		// must flood icmpv6 Route Advertisement and Neighbor Advertisement traffic as it fails to create a CT entry
		for _, icmpType := range []int{types.RouteAdvertisementICMPType, types.NeighborAdvertisementICMPType} {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=14, table=1,icmp6,icmpv6_type=%d actions=FLOOD",
					defaultOpenFlowCookie, icmpType))
		}

		// We send BFD traffic both on the host and in ovn
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:LOCAL",
				defaultOpenFlowCookie, ofportPhys, ofportPatch))
	}

	if config.IPv4Mode {
		// We send BFD traffic both on the host and in ovn
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:LOCAL",
				defaultOpenFlowCookie, ofportPhys, ofportPatch))
	}

	// New dispatch table 11
	// packets larger than known acceptable MTU need to go to kernel to create ICMP frag needed
	if !config.Gateway.DisablePacketMTUCheck {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
				"actions=LOCAL", defaultOpenFlowCookie))
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=1, table=11, "+
				"actions=output:%s", defaultOpenFlowCookie, ofportPatch))

	}
	// table 1, all other connections do normal processing
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=2, "+
			"actions=mod_dl_dst=%s,output:%s", defaultOpenFlowCookie, macAddress, ofportPatch))

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst=%s,output:LOCAL",
			defaultOpenFlowCookie, macAddress))

	// table 4, hairpinned pkts that need to go from OVN -> Host
	// We need to SNAT and masquerade OVN GR IP, send to table 3 for dispatch to Host
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ip,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, OVNMasqCTZone, types.V4OVNMasqueradeIP))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ipv6, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, OVNMasqCTZone, types.V6OVNMasqueradeIP))
	}
	// table 5, Host Reply traffic to hairpinned svc, need to unDNAT, send to table 2
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ip, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, HostMasqCTZone))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ipv6, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, HostMasqCTZone))
	}

	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		gwBridge:    gwBridge,
		physIntf:    gwIntf,
		patchIntf:   patchPort,
		ofportPhys:  ofportPhys,
		ofportPatch: ofportPatch,
		flowCache:   make(map[string][]string),
		flowMutex:   sync.Mutex{},
		flowChan:    make(chan struct{}, 1),
	}

	ofm.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	ofm.updateFlowCacheEntry("DEFAULT", dftFlows)

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf string, nodeAnnotator kube.Annotator) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	bridgeName, uplinkName, macAddress, ips, err := gatewayInitInternal(
		nodeName, gwIntf, subnets, gwNextHops, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	patchPort := "patch-" + bridgeName + "_" + nodeName + "-to-br-int"

	// add masquerade subnet route to avoid zeroconf routes
	if config.IPv4Mode {
		bridgeLink, err := util.LinkSetUp(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("unable to find shared gw bridge interface: %s", bridgeName)
		}
		v4nextHops, err := util.MatchIPFamily(false, gwNextHops)
		if err != nil {
			return nil, fmt.Errorf("no valid ipv4 next hop exists: %v", err)
		}
		_, masqIPNet, _ := net.ParseCIDR(types.V4MasqueradeSubnet)
		if exists, err := util.LinkRouteExists(bridgeLink, v4nextHops[0], masqIPNet); err == nil && !exists {
			err = util.LinkRoutesAdd(bridgeLink, v4nextHops[0], []*net.IPNet{masqIPNet}, 0)
			if err != nil {
				if os.IsExist(err) {
					klog.V(5).Infof("Ignoring error %s from 'route add %s via %s'",
						err.Error(), masqIPNet, v4nextHops[0])
				} else {
					return nil, fmt.Errorf("unable to add OVN masquerade route to host, error: %v", err)
				}
			}
		} else if err != nil {
			return nil, fmt.Errorf("failed to check if route exists for masquerade subnet, error: %v", err)
		}
	}

	gw.readyFunc = func() (bool, error) {
		return gatewayReady(patchPort)
	}

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Shared Gateway Openflow Manager")
		var err error

		gw.openflowManager, err = newSharedGatewayOpenFlowManager(patchPort, macAddress.String(), bridgeName, uplinkName, ips)
		if err != nil {
			return err
		}

		if config.Gateway.NodeportEnable {
			klog.Info("Creating Shared Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(patchPort, bridgeName, uplinkName, gw.openflowManager)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}
		return nil
	}

	klog.Info("Shared Gateway Creation Complete")
	return gw, nil
}

func newNodePortWatcher(patchPort, gwBridge, gwIntf string, ofm *openflowManager) (*nodePortWatcher, error) {
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get",
		"interface", patchPort, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get",
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
		ofm:         ofm,
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

func svcToCookie(namespace string, name string, token string, port int32) (string, error) {
	id := fmt.Sprintf("%s%s%s%d", namespace, name, token, port)
	h := fnv.New64a()
	_, err := h.Write([]byte(id))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("0x%x", h.Sum64()), nil
}
