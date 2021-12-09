package node

import (
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	defaultOpenFlowCookie = "0xdeff105"
	// ovsLocalPort is the name of the OVS bridge local port
	ovsLocalPort = "LOCAL"
	// ctMarkOVN is the conntrack mark value for OVN traffic
	ctMarkOVN = "0x1"
	// ctMarkHost is the conntrack mark value for host traffic
	ctMarkHost = "0x2"
)

var (
	HostMasqCTZone     = config.Default.ConntrackZone + 1 //64001
	OVNMasqCTZone      = HostMasqCTZone + 1               //64002
	HostNodePortCTZone = config.Default.ConntrackZone + 3 //64003
)

// nodePortWatcherIptables manages iptables rules for shared gateway
// to ensure that services using NodePorts are accessible.
type nodePortWatcherIptables struct {
}

func newNodePortWatcherIptables() *nodePortWatcherIptables {
	return &nodePortWatcherIptables{}
}

// nodePortWatcher manages OpenFlow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	dpuMode     bool
	gatewayIPv4 string
	gatewayIPv6 string
	ofportPhys  string
	ofportPatch string
	gwBridge    string
	// Map of service name to programmed iptables/OF rules
	serviceInfo     map[ktypes.NamespacedName]*serviceConfig
	serviceInfoLock sync.Mutex
	ofm             *openflowManager
	nodeIPManager   *addressManager
	watchFactory    factory.NodeWatchFactory
}

type serviceConfig struct {
	// Contains the current service
	service *kapi.Service
	// Were those rules etp:local + Host Networked rules
	etpHostRules bool
}

// updateServiceFlowCache handles managing shared gateway flows for ingress traffic towards kubernetes services
// (nodeport, external, ingress). By default incoming traffic into the node is steered directly into OVN.
// If a service has externalTrafficPolicy local, and has host-networked endpoints, traffic instead will be steered directly
// into the host.
// add parameter indicates if the flows should exist or be removed from the cache
// epHostLocal indicates if a host networked endpoint exists for this
// service func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add bool, epHostLocal bool) {
func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add bool, epHostLocal bool) {
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
				// Delete if needed and skip to next protocol
				if !add {
					npw.ofm.deleteFlowsByKey(key)
					continue
				}
				// (astoycos) TODO combine flow generation into a single function
				// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
				// set to Local, and the backend pod is HostNetworked. We need to add
				// Flows that will DNAT all traffic coming into nodeport to the nodeIP:Port and
				// ensure that the return traffic is UnDNATed to correct the nodeIP:Nodeport
				if epHostLocal {
					var nodeportFlows []string
					klog.V(5).Infof("Adding flows on breth0 for Nodeport Service %s in Namespace: %s since ExternalTrafficPolicy=local", service.Name, service.Namespace)
					// table 0, This rule matches on all traffic with dst port == NodePort, DNAT's it to the correct NodeIP
					// If ipv6 make sure to choose the ipv6 node address), and sends to table 6
					if strings.Contains(flowProtocol, "6") {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=[%s]:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
					} else {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
					}
					nodeportFlows = append(nodeportFlows,
						// table 6, Sends the packet to the host
						fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
							cookie),
						// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(zone=%d nat,table=7)",
							cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
						// table 7, Sends the packet back out eth0 to the external client
						fmt.Sprintf("cookie=%s, priority=110, table=7, "+
							"actions=output:%s", cookie, npw.ofportPhys))

					npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
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
			if len(ing.IP) > 0 {
				err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, epHostLocal, protocol, actions, ing.IP, "Ingress")
				if err != nil {
					klog.Errorf(err.Error())
				}
			}
		}
		// flows for externalIPs
		for _, externalIP := range service.Spec.ExternalIPs {
			err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, epHostLocal, protocol, actions, externalIP, "External")
			if err != nil {
				klog.Errorf(err.Error())
			}
		}
	}
}

// flow generation for LB and ExternalIP flow is essentially the same, so avoid code duplication with
// this method
func (npw *nodePortWatcher) createLbAndExternalSvcFlows(service *kapi.Service, svcPort *kapi.ServicePort, add bool, epHostLocal bool, protocol string, actions string, ipAddress string, ipType string) error {
	if net.ParseIP(ipAddress) == nil {
		return fmt.Errorf("failed to parse %s IP: %q", ipType, ipAddress)
	}
	flowProtocol := protocol
	nwDst := "nw_dst"
	nwSrc := "nw_src"
	if utilnet.IsIPv6String(ipAddress) {
		flowProtocol = protocol + "6"
		nwDst = "ipv6_dst"
		nwSrc = "ipv6_src"
	}
	cookie, err := svcToCookie(service.Namespace, service.Name, ipAddress, svcPort.Port)
	if err != nil {
		klog.Warningf("Unable to generate cookie for %s svc: %s, %s, %s, %d, error: %v",
			ipType, service.Namespace, service.Name, ipAddress, svcPort.Port, err)
		cookie = "0"
	}
	key := strings.Join([]string{ipType, service.Namespace, service.Name, ipAddress, fmt.Sprintf("%d", svcPort.Port)}, "_")
	// Delete if needed and skip to next protocol
	if !add {
		npw.ofm.deleteFlowsByKey(key)
		return nil
	}
	// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
	// set to Local, and the backend pod is HostNetworked. We need to add
	// Flows that will DNAT all external traffic destined for the lb/externalIP service
	// to the nodeIP / nodeIP:port of the host networked backend.
	// And then ensure that return traffic is UnDNATed correctly back
	// to the ingress / external IP
	if epHostLocal {
		var nodeportFlows []string
		klog.V(5).Infof("Adding flows on breth0 for %s Service %s in Namespace: %s since ExternalTrafficPolicy=local", ipType, service.Name, service.Namespace)
		// table 0, This rule matches on all traffic with dst ip == LoadbalancerIP / extenalIP, DNAT's it to the correct NodeIP
		// If ipv6 make sure to choose the ipv6 node address for rule
		if strings.Contains(flowProtocol, "6") {
			nodeportFlows = append(nodeportFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=[%s]:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, ipAddress, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
		} else {
			nodeportFlows = append(nodeportFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, ipAddress, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
		}
		nodeportFlows = append(nodeportFlows,
			// table 6, Sends the packet to the host
			fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
				cookie),
			// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
			fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
				cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
			// table 7, the packet back out eth0 to the external client
			fmt.Sprintf("cookie=%s, priority=110, table=7, "+
				"actions=output:%s", cookie, npw.ofportPhys))

		npw.ofm.updateFlowCacheEntry(key, nodeportFlows)

	} else {
		npw.ofm.updateFlowCacheEntry(key, []string{
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
				"actions=%s",
				cookie, npw.ofportPhys, flowProtocol, nwDst, ipAddress, svcPort.Port, actions),
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_src=%d, "+
				"actions=output:%s",
				cookie, npw.ofportPatch, flowProtocol, nwSrc, ipAddress, svcPort.Port, npw.ofportPhys),
			npw.generateArpBypassFlow(protocol, ipAddress, cookie)})
	}

	return nil
}

// generate ARP/NS bypass flow which will send the ARP/NS request everywhere *but* to OVN
// OpenFlow will not do hairpin switching, so we can safely add the origin port to the list of ports, too
func (npw *nodePortWatcher) generateArpBypassFlow(protocol string, ipAddr string, cookie string) string {
	addrResDst := "arp_tpa"
	addrResProto := "arp, arp_op=1"
	if utilnet.IsIPv6String(ipAddr) {
		addrResDst = "nd_target"
		addrResProto = "icmp6, icmp_type=135, icmp_code=0"
	}

	var arpFlow string
	var arpPortsFiltered []string
	arpPorts, err := util.GetOpenFlowPorts(npw.gwBridge, false)
	if err != nil {
		// in the odd case that getting all ports from the bridge should not work,
		// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
		klog.Warningf("Unable to get port list from bridge. Using ovsLocalPort as output only: error: %v",
			err)
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, ovsLocalPort)
	} else {
		// cover the case where breth0 has more than 3 ports, e.g. if an admin adds a 4th port
		// and the ExternalIP would be on that port
		// Use all ports except for ofPortPhys and the ofportPatch
		// Filtering ofPortPhys is for consistency / readability only, OpenFlow will not send
		// out the in_port normally (see man 7 ovs-actions)
		for _, port := range arpPorts {
			if port == npw.ofportPatch || port == npw.ofportPhys {
				continue
			}
			arpPortsFiltered = append(arpPortsFiltered, port)
		}
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, strings.Join(arpPortsFiltered, ","))
	}

	return arpFlow
}

// getAndDeleteServiceInfo returns the serviceConfig for a service and if it exists and then deletes the entry
func (npw *nodePortWatcher) getAndDeleteServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	delete(npw.serviceInfo, index)
	return out, exists
}

// getServiceInfo returns the serviceConfig for a service and if it exists
func (npw *nodePortWatcher) getServiceInfo(index ktypes.NamespacedName) (out *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()
	out, exists = npw.serviceInfo[index]
	return out, exists
}

// getAndSetServiceInfo creates and sets the serviceConfig, returns if it existed and whatever was there
func (npw *nodePortWatcher) getAndSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, etpHostRules bool) (old *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	old, exists = npw.serviceInfo[index]
	npw.serviceInfo[index] = &serviceConfig{service: service, etpHostRules: etpHostRules}
	return old, exists
}

// addOrSetServiceInfo creates and sets the serviceConfig if it doesn't exist
func (npw *nodePortWatcher) addOrSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, etpHostRules bool) (exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if _, exists := npw.serviceInfo[index]; !exists {
		// Only set this if it doesn't exist
		npw.serviceInfo[index] = &serviceConfig{service: service, etpHostRules: etpHostRules}
		return false
	}
	return true

}

// updateServiceInfo sets the serviceConfig for a service and returns the existing serviceConfig, if inputs are nil
// do not update those fields, if it does not exist return nil.
func (npw *nodePortWatcher) updateServiceInfo(index ktypes.NamespacedName, service *kapi.Service, etpHostRules *bool) (old *serviceConfig, exists bool) {

	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if old, exists = npw.serviceInfo[index]; !exists {
		klog.V(5).Infof("No serviceConfig found for service %s in namespace %s", index.Name, index.Namespace)
		return nil, exists
	}

	if service != nil {
		npw.serviceInfo[index].service = service
	}

	if etpHostRules != nil {
		npw.serviceInfo[index].etpHostRules = *etpHostRules
	}

	return old, exists
}

// addServiceRules ensures the correct iptables rules and OpenFlow physical
// flows are programmed for a given service and hostNetwork endpoint configuration
func addServiceRules(service *kapi.Service, hasHostNet bool, npw *nodePortWatcher) {
	if util.ServiceExternalTrafficPolicyLocal(service) && hasHostNet {
		klog.V(5).Infof("Adding externalTrafficPolicy:local and hostNetworked rules for %v", service)
		// For dpu or Full mode
		if npw != nil {
			npw.updateServiceFlowCache(service, true, true)
			npw.ofm.requestFlowSync()
			// Dont touch iptables if in dpuMode
			if !npw.dpuMode {
				addSharedGatewayIptRules(service, true)
			}
			return
		}
		// For Host Only Mode
		addSharedGatewayIptRules(service, true)
	} else {
		// For dpu or Full mode
		if npw != nil {
			npw.updateServiceFlowCache(service, true, false)
			npw.ofm.requestFlowSync()
			if !npw.dpuMode {
				addSharedGatewayIptRules(service, false)
			}
			return
		}
		// For Host Only Mode
		addSharedGatewayIptRules(service, false)
	}
}

// delServiceRules deletes all possible iptables rules and OpenFlow physical
// flows for a service
func delServiceRules(service *kapi.Service, npw *nodePortWatcher) {
	if npw != nil {
		npw.updateServiceFlowCache(service, false, false)
		npw.ofm.requestFlowSync()
		if !npw.dpuMode {
			// Always try and delete all rules here
			delSharedGatewayIptRules(service, true)
			delSharedGatewayIptRules(service, false)
		}
		return
	}

	// For host only node always try and delete rules here
	// externalTrafficPolicy is not implemented for dpu mode
	delSharedGatewayIptRules(service, false)
}

func serviceUpdateNeeded(old, new *kapi.Service) bool {
	return reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) &&
		reflect.DeepEqual(new.Spec.ExternalTrafficPolicy, old.Spec.ExternalTrafficPolicy)
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) {
	var etpHostRules bool
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}

	klog.V(5).Infof("Adding service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	ep, err := npw.watchFactory.GetEndpoint(service.Namespace, service.Name)
	if err != nil {
		klog.V(5).Infof("No endpoint found for service %s in namespace %s during service Add", service.Name, service.Namespace)
		// No endpoints exist yet so default to false
		etpHostRules = false
	} else {
		etpHostRules = hasHostNetworkEndpoints(ep, &npw.nodeIPManager.addresses)
	}

	// If something didn't already do it add correct Service rules
	if exists := npw.addOrSetServiceInfo(name, service, etpHostRules); !exists {
		klog.V(5).Infof("Service Add %s event in namespace %s came before endpoint event setting svcConfig", service.Name, service.Namespace)
		addServiceRules(service, etpHostRules, npw)
	} else {
		klog.V(5).Infof("Rules already programmed for %s in namespace %s", service.Name, service.Namespace)
	}
}

func (npw *nodePortWatcher) UpdateService(old, new *kapi.Service) {
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if serviceUpdateNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return
	}

	// Update the service in svcConfig if we need to so that other handler
	// threads do the correct thing, leave etpHostRules alone in the cache
	svcConfig, exists := npw.updateServiceInfo(name, new, nil)
	if !exists {
		klog.V(5).Infof("Service %s in namespace %s was deleted during service Update", old.Name, old.Namespace)
		return
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		// Delete old rules if needed, but don't delete svcConfig
		// so that we don't miss any endpoint update events here
		klog.V(5).Infof("Deleting old service rules for: %v", old)
		delServiceRules(old, npw)
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		klog.V(5).Infof("Adding new service rules for: %v", new)
		addServiceRules(new, svcConfig.etpHostRules, npw)
	}
}

func (npw *nodePortWatcher) DeleteService(service *kapi.Service) {
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}

	klog.V(5).Infof("Deleting service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	if svcConfig, exists := npw.getAndDeleteServiceInfo(name); exists {
		delServiceRules(svcConfig.service, npw)
	} else {
		klog.Warningf("Deletion failed No service found in cache for endpoint %s in namespace %s", service.Name, service.Namespace)
	}

}

func (npw *nodePortWatcher) SyncServices(services []interface{}) {
	keepIPTRules := []iptRule{}
	for _, serviceInterface := range services {
		name := ktypes.NamespacedName{Namespace: serviceInterface.(*kapi.Service).Namespace, Name: serviceInterface.(*kapi.Service).Name}

		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		ep, err := npw.watchFactory.GetEndpoint(service.Namespace, service.Name)
		if err != nil {
			klog.V(5).Infof("No endpoint found for service %s in namespace %s during sync", service.Name, service.Namespace)
			continue
		}

		hasHostNet := hasHostNetworkEndpoints(ep, &npw.nodeIPManager.addresses)
		npw.getAndSetServiceInfo(name, service, hasHostNet)
		// Delete OF rules for service if they exist
		npw.updateServiceFlowCache(service, false, hasHostNet)
		npw.updateServiceFlowCache(service, true, hasHostNet)
		// Add correct iptables rules only for Full mode
		if !npw.dpuMode {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, hasHostNet)...)
		}
	}
	// sync OF rules once
	npw.ofm.requestFlowSync()
	// sync IPtables rules once only for Full mode
	if !npw.dpuMode {
		for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
			recreateIPTRules("nat", chain, keepIPTRules)
		}
	}
}

func (npw *nodePortWatcher) AddEndpoints(ep *kapi.Endpoints) {
	var etpHostRules bool
	name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}

	svc, err := npw.watchFactory.GetService(ep.Namespace, ep.Name)
	if err != nil {
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		klog.V(5).Infof("No service found for endpoint %s in namespace %s during add", ep.Name, ep.Namespace)
		return
	}

	if !util.ServiceTypeHasClusterIP(svc) || !util.IsClusterIPSet(svc) {
		return
	}

	klog.V(5).Infof("Adding endpoints %s in namespace %s", ep.Name, ep.Namespace)
	etpHostRules = hasHostNetworkEndpoints(ep, &npw.nodeIPManager.addresses)

	// Here we make sure the correct rules are programmed whenever an AddEndpoint
	// event is received, only alter flows if we need to, i.e if cache wasn't
	// set or if it was and etpHostRules state changed, to prevent flow churn
	out, exists := npw.getAndSetServiceInfo(name, svc, etpHostRules)
	if !exists {
		klog.V(5).Infof("Endpoint %s ADD event in namespace %s is creating rules", ep.Name, ep.Namespace)
		addServiceRules(svc, etpHostRules, npw)
		return
	}

	if out.etpHostRules != etpHostRules {
		klog.V(5).Infof("Endpoint %s ADD event in namespace %s is updating rules", ep.Name, ep.Namespace)
		delServiceRules(svc, npw)
		addServiceRules(svc, etpHostRules, npw)
	}

}

func (npw *nodePortWatcher) DeleteEndpoints(ep *kapi.Endpoints) {
	var etpHostRules = false

	klog.V(5).Infof("Deleting endpoints %s in namespace %s", ep.Name, ep.Namespace)
	// remove rules for endpoints and add back normal ones
	name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
	if svcConfig, exists := npw.updateServiceInfo(name, nil, &etpHostRules); exists {
		// Lock the cache mutex here so we don't miss a service delete during an endpoint delete
		// we have to do this because deleting and adding iptables rules is slow.
		npw.serviceInfoLock.Lock()
		defer npw.serviceInfoLock.Unlock()

		delServiceRules(svcConfig.service, npw)
		addServiceRules(svcConfig.service, etpHostRules, npw)
	}
}

func (npw *nodePortWatcher) UpdateEndpoints(old *kapi.Endpoints, new *kapi.Endpoints) {
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if reflect.DeepEqual(new.Subsets, old.Subsets) {
		return
	}

	klog.V(5).Infof("Updating endpoints %s in namespace %s", old.Name, old.Namespace)

	// Delete old endpoint rules and add normal ones back
	if len(new.Subsets) == 0 {
		if _, exists := npw.getServiceInfo(name); exists {
			npw.DeleteEndpoints(old)
		}
	}

	// Update rules if hasHostNetworkEndpoints status changed
	etpHostRulesNew := hasHostNetworkEndpoints(new, &npw.nodeIPManager.addresses)
	if hasHostNetworkEndpoints(old, &npw.nodeIPManager.addresses) != etpHostRulesNew {
		npw.DeleteEndpoints(old)
		npw.AddEndpoints(new)
	}
}

func (npwipt *nodePortWatcherIptables) AddService(service *kapi.Service) {
	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	addServiceRules(service, false, nil)
}

func (npwipt *nodePortWatcherIptables) UpdateService(old, new *kapi.Service) {
	if serviceUpdateNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		delServiceRules(old, nil)
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		addServiceRules(new, false, nil)
	}
}

func (npwipt *nodePortWatcherIptables) DeleteService(service *kapi.Service) {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}
	delServiceRules(service, nil)
}

func (npwipt *nodePortWatcherIptables) SyncServices(services []interface{}) {
	keepIPTRules := []iptRule{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// Add correct iptables rules
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, false)...)
	}

	// sync IPtables rules once
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		recreateIPTRules("nat", chain, keepIPTRules)
	}
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newSharedGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration) (*openflowManager, error) {
	dftFlows, err := flowsForDefaultBridge(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch,
		gwBridge.ofPortHost, gwBridge.ips)
	if err != nil {
		return nil, err
	}
	dftCommonFlows := commonFlows(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch,
		gwBridge.ofPortHost)
	dftFlows = append(dftFlows, dftCommonFlows...)

	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge:         gwBridge,
		externalGatewayBridge: exGWBridge,
		flowCache:             make(map[string][]string),
		flowMutex:             sync.Mutex{},
		exGWFlowCache:         make(map[string][]string),
		exGWFlowMutex:         sync.Mutex{},
		flowChan:              make(chan struct{}, 1),
	}

	ofm.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	ofm.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if exGWBridge != nil {
		ofm.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		exGWBridgeDftFlows := commonFlows(exGWBridge.ofPortPhys, exGWBridge.macAddress.String(),
			exGWBridge.ofPortPatch, exGWBridge.ofPortHost)
		ofm.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

func flowsForDefaultBridge(ofPortPhys, bridgeMacAddress, ofPortPatch, ofPortHost string, bridgeIPs []*net.IPNet) ([]string, error) {
	var dftFlows []string
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
				"actions=output:%s", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
				ofPortHost))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

		// table0, Geneve packets coming from LOCAL. Skip conntrack and go directly to external
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp, udp_dst=%d, "+
				"actions=output:%s", defaultOpenFlowCookie, ovsLocalPort, config.Default.EncapPort, ofPortPhys))

		physicalIP, err := util.MatchIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofPortPatch, types.V4HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, types.V4OVNMasqueradeIP, OVNMasqCTZone))
	}
	if config.IPv6Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=205, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
				"actions=output:%s", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort,
				ofPortHost))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

		// table0, Geneve packets coming from LOCAL. Skip conntrack and send to external
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=200, in_port=%s, udp6, udp_dst=%d, "+
				"actions=output:%s", defaultOpenFlowCookie, ovsLocalPort, config.Default.EncapPort, ofPortPhys))

		physicalIP, err := util.MatchIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
				"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
				defaultOpenFlowCookie, ofPortPatch, types.V6HostMasqueradeIP, physicalIP.IP,
				HostMasqCTZone, physicalIP.IP))

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, types.V6OVNMasqueradeIP, OVNMasqCTZone))
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
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				defaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix, svcCIDR, HostMasqCTZone, masqIP))

		// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
				"actions=ct(zone=%d,nat,table=3)",
				defaultOpenFlowCookie, ofPortPatch, protoPrefix, protoPrefix, svcCIDR,
				protoPrefix, masqIP, HostMasqCTZone))
	}

	var actions string
	if config.Gateway.DisablePacketMTUCheck {
		actions = fmt.Sprintf("output:%s", ofPortPatch)
	} else {
		// check packet length larger than MTU + eth header - vlan overhead
		// send to table 11 to check if it needs to go to kernel for ICMP needs frag
		actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
	}

	if config.IPv4Mode {
		// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
				"actions=%s",
				defaultOpenFlowCookie, ctMarkOVN, actions))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
				"actions=%s",
				defaultOpenFlowCookie, ctMarkOVN, actions))

		// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
				"actions=output:%s",
				defaultOpenFlowCookie, ctMarkHost, ofPortHost))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
				"actions=output:%s",
				defaultOpenFlowCookie, ctMarkHost, ofPortHost))
	}

	if config.IPv6Mode {
		// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, ct_mark=%s, "+
				"actions=%s",
				defaultOpenFlowCookie, ctMarkOVN, actions))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+rel, ct_mark=%s, "+
				"actions=%s",
				defaultOpenFlowCookie, ctMarkOVN, actions))

		// table 1, established and related connections in zone 64000 with ct_mark ctMarkHost go to host
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip6, ct_state=+trk+est, ct_mark=%s, "+
				"actions=output:%s",
				defaultOpenFlowCookie, ctMarkHost, ofPortHost))

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, table=1, ip6, ct_state=+trk+rel, ct_mark=%s, "+
				"actions=output:%s",
				defaultOpenFlowCookie, ctMarkHost, ofPortHost))
	}

	// table 1, we check to see if this dest mac is the shared mac, if so send to host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:%s",
			defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=2, "+
			"actions=mod_dl_dst=%s,output:%s", defaultOpenFlowCookie, bridgeMacAddress, ofPortPatch))

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst=%s,output:%s",
			defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

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
	return dftFlows, nil
}

func commonFlows(ofPortPhys, bridgeMacAddress, ofPortPatch, ofPortHost string) []string {
	var dftFlows []string
	maxPktLength := getMaxFrameLength()

	// table 0, we check to see if this dest mac is the shared mac, if so flood to both ports
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=output:%s,output:%s",
			defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, ofPortPatch, ofPortHost))

	if config.IPv4Mode {
		// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
				defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))

		// table 0, packets coming from host Commit connections with ct_mark ctMarkHost
		// so that reverse direction goes back to the host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
				defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state and mark of the connection.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
	}
	if config.IPv6Mode {
		// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
				defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))

		// table 0, packets coming from host. Commit connections with ct_mark ctMarkHost
		// so that reverse direction goes back to the host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
				"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
				defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state and mark of the connection.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
	}

	var actions string
	if config.Gateway.DisablePacketMTUCheck {
		actions = fmt.Sprintf("output:%s", ofPortPatch)
	} else {
		// check packet length larger than MTU + eth header - vlan overhead
		// send to table 11 to check if it needs to go to kernel for ICMP needs frag
		actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
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
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:%s",
			defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

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
			fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:%s",
				defaultOpenFlowCookie, ofPortPhys, ofPortPatch, ofPortHost))
	}

	if config.IPv4Mode {
		// We send BFD traffic both on the host and in ovn
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:%s",
				defaultOpenFlowCookie, ofPortPhys, ofPortPatch, ofPortHost))
	}

	// New dispatch table 11
	// packets larger than known acceptable MTU need to go to kernel to create ICMP frag needed
	if !config.Gateway.DisablePacketMTUCheck {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
				"actions=output:%s", defaultOpenFlowCookie, ofPortHost))
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=1, table=11, "+
				"actions=output:%s", defaultOpenFlowCookie, ofPortPatch))

	}
	// table 1, all other connections do normal processing
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))

	return dftFlows
}

func setBridgeOfPorts(bridge *bridgeConfiguration) error {
	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", bridge.patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", bridge.patchPort, stderr, err)
	}

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", bridge.uplinkName, "ofport")
	if err != nil {
		return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			bridge.uplinkName, stderr, err)
	}
	bridge.ofPortPatch = ofportPatch
	bridge.ofPortPhys = ofportPhys

	// Get ofport represeting the host. That is, host representor port in case of DPUs, ovsLocalPort otherwise.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		var stderr string
		hostRep, err := util.GetDPUHostInterface(bridge.bridgeName)
		if err != nil {
			return err
		}

		bridge.ofPortHost, stderr, err = util.RunOVSVsctl("get", "interface", hostRep, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of host interface %s, stderr: %q, error: %v",
				hostRep, stderr, err)
		}
	} else {
		bridge.ofPortHost = ovsLocalPort
	}

	return nil
}

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet, nodeAnnotator kube.Annotator, kube kube.Interface, cfg *managementPortConfig, watchFactory factory.NodeWatchFactory) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, subnets, gwNextHops, gwIPs, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if config.OvnKubeNode.Mode == types.NodeModeFull {
		// add masquerade subnet route to avoid zeroconf routes
		err = addMasqueradeRoute(gwBridge.bridgeName, gwNextHops)
		if err != nil {
			return nil, err
		}
	}

	if exGwBridge != nil {
		gw.readyFunc = func() (bool, error) {
			ready, err := gatewayReady(gwBridge.patchPort)
			if err != nil {
				return false, err
			}
			exGWReady, err := gatewayReady(exGwBridge.patchPort)
			if err != nil {
				return false, err
			}
			return ready && exGWReady, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			return gatewayReady(gwBridge.patchPort)
		}
	}

	// OCP HACK -- block MCS ports
	rules := []iptRule{}
	if config.IPv4Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv4)
	}
	if config.IPv6Mode {
		generateBlockMCSRules(&rules, iptables.ProtocolIPv6)
	}
	if err := addIptRules(rules); err != nil {
		return nil, fmt.Errorf("failed to setup MCS-blocking rules: %w", err)
	}
	// END OCP HACK

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Shared Gateway Openflow Manager")
		err := setBridgeOfPorts(gwBridge)
		if err != nil {
			return err
		}
		if exGwBridge != nil {
			err = setBridgeOfPorts(exGwBridge)
			if err != nil {
				return err
			}
		}
		gw.openflowManager, err = newSharedGatewayOpenFlowManager(gwBridge, exGwBridge)
		if err != nil {
			return err
		}

		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory)

		if config.Gateway.NodeportEnable {
			klog.Info("Creating Shared Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge.patchPort, gwBridge.bridgeName, gwBridge.uplinkName, gwBridge.ips, gw.openflowManager, gw.nodeIPManager, watchFactory)
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

func newNodePortWatcher(patchPort, gwBridge, gwIntf string, ips []*net.IPNet, ofm *openflowManager,
	nodeIPManager *addressManager, watchFactory factory.NodeWatchFactory) (*nodePortWatcher, error) {
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
	// NodePortIP:NodePort to ClusterServiceIP:Port. We don't need to do this while
	// running on DPU or on DPU-Host.
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		if config.Gateway.Mode == config.GatewayModeLocal {
			if err := initLocalGatewayIPTables(); err != nil {
				return nil, err
			}
		} else if config.Gateway.Mode == config.GatewayModeShared {
			if err := initSharedGatewayIPTables(); err != nil {
				return nil, err
			}
		}
	}

	// used to tell addServiceRules which rules to add
	dpuMode := false
	if config.OvnKubeNode.Mode != types.NodeModeFull {
		dpuMode = true
	}

	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(ips)

	npw := &nodePortWatcher{
		dpuMode:       dpuMode,
		gatewayIPv4:   gatewayIPv4,
		gatewayIPv6:   gatewayIPv6,
		ofportPhys:    ofportPhys,
		ofportPatch:   ofportPatch,
		gwBridge:      gwBridge,
		serviceInfo:   make(map[ktypes.NamespacedName]*serviceConfig),
		nodeIPManager: nodeIPManager,
		ofm:           ofm,
		watchFactory:  watchFactory,
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

func addMasqueradeRoute(netIfaceName string, nextHops []net.IP) error {
	// only apply when ipv4mode is enabled
	if !config.IPv4Mode {
		return nil
	}
	netIfaceLink, err := util.LinkSetUp(netIfaceName)
	if err != nil {
		return fmt.Errorf("unable to find shared gw bridge interface: %s", netIfaceName)
	}
	v4nextHops, err := util.MatchIPFamily(false, nextHops)
	if err != nil {
		return fmt.Errorf("no valid ipv4 next hop exists: %v", err)
	}
	_, masqIPNet, _ := net.ParseCIDR(types.V4MasqueradeSubnet)
	err = util.LinkRoutesAddOrUpdateMTU(netIfaceLink, v4nextHops[0], []*net.IPNet{masqIPNet}, config.Default.RoutableMTU)
	if err != nil {
		return fmt.Errorf("unable to add OVN masquerade route to host, error: %v", err)
	}
	return nil
}
