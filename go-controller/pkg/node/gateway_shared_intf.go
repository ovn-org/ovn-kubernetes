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
)

// nodePortWatcher manages OpenFlow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	gatewayIPv4 string
	gatewayIPv6 string
	ofportPhys  string
	ofportPatch string
	gwBridge    string
	// The services Map is required to decide if we care about a given Endpoint event
	// services map[ktypes.NamespacedName]*kapi.Service
	services      sync.Map
	ofm           *openflowManager
	nodeIPManager *addressManager
	watchFactory  factory.ObjectCacheInterface
}

// With The external Traffic policy feature this logic got much more complicated
// Now we have Multiple K8s Object triggers (i.e Services and Endpoints) That must
// Prompt a single event function -->
// func updateServiceFlowCache(service *kapi.Service, add bool, epHostLocal bool)
//
// To handle this, two signal variables are used, add and epHostLocal
// If add==false all breth0 flows are deleted for the service
// If add==true breth0 flows are created for the service +
// 		if epHostLocal==False && ETP==Local it means the svc has no Host Endpoints
//         so we add only a single flow to on breth0 to steer traffic into OVN-K
//      if epHostLocal==True && ETP==Local it means the svc has Host endpoints
//         so we add 4 flows onto breth0 to allow external-> SVC traffic to
//         Completely bypass OVN.

// This function is used to manage the flows programmed in br-ex for OVN-K8's Nodeport, ExternalIP, and
// Loadbalancer type services. It also consumes an `epHostLocal` argument to signal that the specified service
// has has host-networked backends
// It also introduces two new flow tables
// table 6: Handles ingress traffic for externalTrafficPolicy:local services with Host networked backends
// table 7: Handles ingress return traffic externalTrafficPolicy:local services with Host
func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add bool, epHostLocal bool) {
	var cookie, key string
	var err error
	var HostNodePortCTZone = config.Default.ConntrackZone + 3 //64003

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

				// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
				// set to Local, and the backend pod is HostNetworked. We need to add
				// Flows that will DNAT all traffic coming into nodeport to the nodeIP:Port and
				// ensure that the return traffic is UnDNATed to correct the nodeIP:Nodeport
				if epHostLocal {
					var nodeportFlows []string
					klog.V(2).Infof("Adding flows on br-ex for Nodeport Service %s in Namespace: %s since ExternalTrafficPolicy=local", service.Name)
					// table 0, This rule matches on all traffic with dst port == NodePort, DNAT's it to the correct NodeIP
					// If ipv6 make sure to choose the ipv6 node address), and sends to table 6
					if strings.Contains(flowProtocol, "6") {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
					} else {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
					}
					nodeportFlows = append(nodeportFlows,
						// table 6, Sends the packet to the host
						fmt.Sprintf("cookie=%s, priority=100, table=6, actions=output:LOCAL",
							cookie),
						// table 7, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						fmt.Sprintf("cookie=%s, priority=100, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
							cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
						// table 7, Sends the packet back out eth0 to the external client
						fmt.Sprintf("cookie=%s, priority=100, table=7, "+
							"actions=output:%s", cookie, npw.ofportPhys))

					npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
				} else {
					klog.V(2).Infof("Updating to standard flows on br-ex for Nodeport Service %s", service.Name)
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
			// Delete if needed and skip to next protocol
			if !add {
				npw.ofm.deleteFlowsByKey(key)
				continue
			}

			// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
			// set to Local, and the backend pod is HostNetworked. We need to add
			// Flows that will DNAT all external traffic destined for the lb service
			// to the nodeIP and ensure That return traffic is UnDNATed correctly back
			// to the ingress ip
			if epHostLocal {
				var nodeportFlows []string
				klog.V(2).Infof("Loadbalancer Service %s has externalTrafficPolicy==Local and node local endpoints", service.Name)
				// table 0, This rule matches on all traffic with dst port == LoadbalancerIP, DNAT's it to the correct NodeIP
				// If ipv6 make sure to choose the ipv6 node address for rule
				if strings.Contains(flowProtocol, "6") {
					nodeportFlows = append(nodeportFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
							cookie, npw.ofportPhys, flowProtocol, nwDst, ing.IP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
				} else {
					nodeportFlows = append(nodeportFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
							cookie, npw.ofportPhys, flowProtocol, nwDst, ing.IP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
				}
				nodeportFlows = append(nodeportFlows,
					// table 6, Sends the packet to the host
					fmt.Sprintf("cookie=%s, priority=100, table=6, actions=output:LOCAL",
						cookie),
					// table 7, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
					fmt.Sprintf("cookie=%s, priority=100, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
						cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
					// table 7, the packet back out eth0 to the external client
					fmt.Sprintf("cookie=%s, priority=100, table=7, "+
						"actions=output:%s", cookie, npw.ofportPhys))

				npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
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
			// Delete if needed and skip to next protocol
			if !add {
				npw.ofm.deleteFlowsByKey(key)
				continue
			}
			// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
			// set to Local, and the backend pod is HostNetworked. We need to add
			// Flows that will DNAT all external traffic destined for externalIP service
			// to the nodeIP:port of the host networked backend. And Then ensure That return
			// traffic is UnDNATed correctly back to the external IP
			if epHostLocal {
				var nodeportFlows []string
				klog.V(2).Infof("ExternalIP Service %s has externalTrafficPolicy==Local and node local endpoints", service.Name)
				// table 0, This rule matches on all traffic with dst ip == externalIP and DNAT's it to the correct NodeIP
				// If ipv6 make sure to choose the ipv6 node address for rule
				if strings.Contains(flowProtocol, "6") {
					nodeportFlows = append(nodeportFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
							cookie, npw.ofportPhys, flowProtocol, nwDst, externalIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
				} else {
					nodeportFlows = append(nodeportFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
							cookie, npw.ofportPhys, flowProtocol, nwDst, externalIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
				}
				nodeportFlows = append(nodeportFlows,
					// table 6, Sends the packet to the host
					fmt.Sprintf("cookie=%s, priority=100, table=6, actions=output:LOCAL",
						cookie),
					// table 7, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
					fmt.Sprintf("cookie=%s, priority=100, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
						cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
					// table 7, Sends the packet back out eth0 to the external client
					fmt.Sprintf("cookie=%s, priority=100, table=7, "+
						"actions=output:%s", cookie, npw.ofportPhys))

				npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
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

	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	// Add services to npw struct so that the service object can be used in Endpoint Watcher functions
	// The service can only be deleted from the npw struct upon Endpoint Deletion
	// if the endpoint add event already created the service just exit
	if _, exists := npw.services.Load(name); exists {
		npw.services.Store(name, service)
		npw.updateServiceFlowCache(service, true, false)
		npw.ofm.requestFlowSync()
		addSharedGatewayIptRules(service, false, npw.nodeIPManager.addresses.List())
	}
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
		npw.updateServiceFlowCache(old, false, false)
		delSharedGatewayIptRules(old, false, npw.nodeIPManager.addresses.List())
		needFlowSync = true
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		npw.updateServiceFlowCache(new, true, false)
		addSharedGatewayIptRules(new, false, npw.nodeIPManager.addresses.List())
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
	npw.updateServiceFlowCache(service, false, false)
	npw.ofm.requestFlowSync()
	delSharedGatewayIptRules(service, false, npw.nodeIPManager.addresses.List())
}

func (npw *nodePortWatcher) SyncServices(services []interface{}) {
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		npw.updateServiceFlowCache(service, true, false)
	}

	npw.ofm.requestFlowSync()
	syncSharedGatewayIptRules(services, false, npw.nodeIPManager.addresses.List())
}

// For endpoint Event functions and exernalTrafficPolicy:local
//		An action only ever occurs (i.e epHostLocal=True) if
//			1. The Service Exists &&
// 			2. Svc's ETP==Local &&
//			3. Svc Has host networked Backends
//      Otherwise the endpoint event is ignored
//
//      The action ensures the correct flows and IPtables rules are programmed

// Add new Local Traffic flows if endpoints are local
func (npw *nodePortWatcher) AddEndpoints(ep *kapi.Endpoints) {
	name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
	// Endpoint add Event can arrive before service add event, get and create it if it doesn't exist
	// AND if externalTrafficPolicy:local for the service
	if svc, exists := npw.services.Load(name); exists && util.ServiceExternalTrafficPolicyLocal(svc.(*kapi.Service)) {
		if countHostNeworkEndpoints(ep, &npw.nodeIPManager.addresses) {
			klog.V(5).Infof("Service %s has ExternalTrafficPolicy=local and local Endpoints, Updating flow Cache", ep.Name)
			// Update Service Cache with special flows
			npw.updateServiceFlowCache(svc.(*kapi.Service), true, true)
			npw.ofm.requestFlowSync()
			// Service was already created so now delete old rules and add correct IPtables rules
			delSharedGatewayIptRules(svc.(*kapi.Service), false, npw.nodeIPManager.addresses.List())
			addSharedGatewayIptRules(svc.(*kapi.Service), true, npw.nodeIPManager.addresses.List())
		}
	} else {
		// create service if it does not exist yet
		klog.V(5).Infof("Endpoint: %s event in namespace: %s arrived before service creation", ep.Name, ep.Namespace)
		svc, err := npw.watchFactory.GetService(ep.Namespace, ep.Name)
		if err != nil {
			// This is not necessarily an error. For e.g when there are endpoints
			// without a corresponding service.
			klog.V(5).Infof("No service found for endpoint %s in namespace %s", ep.Name, ep.Namespace)
			return
		}
		npw.services.Store(name, svc)
		npw.AddService(svc)

	}
}

func (npw *nodePortWatcher) DeleteEndpoints(ep *kapi.Endpoints) {
	name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
	if svc, exists := npw.services.Load(name); exists && util.ServiceExternalTrafficPolicyLocal(svc.(*kapi.Service)) {
		if !countHostNeworkEndpoints(ep, &npw.nodeIPManager.addresses) {
			klog.V(5).Infof("Service %s has ExternalTrafficPolicy=local and no local Endpoints, Updating flow Cache", ep.Name)
			npw.updateServiceFlowCache(svc.(*kapi.Service), false, true)
			npw.ofm.requestFlowSync()
			delSharedGatewayIptRules(svc.(*kapi.Service), true, npw.nodeIPManager.addresses.List())
		}
	}
	// We add this only when a Service is created, delete only when endpoint delete event is received
	npw.services.Delete(name)
}

func (npw *nodePortWatcher) UpdateEndpoints(old *kapi.Endpoints, new *kapi.Endpoints) {
	name := ktypes.NamespacedName{Namespace: new.Namespace, Name: new.Name}
	if svc, exists := npw.services.Load(name); exists && util.ServiceExternalTrafficPolicyLocal(svc.(*kapi.Service)) {
		needFlowSync := false

		// We had special ExternalTrafficPolicy:local flows but now we don't
		if countHostNeworkEndpoints(old, &npw.nodeIPManager.addresses) && !countHostNeworkEndpoints(new, &npw.nodeIPManager.addresses) {
			klog.V(5).Infof("Service %s has ExternalTrafficPolicy=local, had local Endpoints, but now does not Updating flow Cache", new.Name)
			// Delete ETP flows
			npw.updateServiceFlowCache(svc.(*kapi.Service), false, true)
			needFlowSync = true
			// Delete Special Iptables Rules
			delSharedGatewayIptRules(svc.(*kapi.Service), true, npw.nodeIPManager.addresses.List())
			// If There are still non-local eps Re-sync with normal nodeport service flows
			if len(new.Subsets) > 0 {
				klog.V(5).Infof("Service %s has ExternalTrafficPolicy=local, had local Endpoints, and still has normal ones Updating flow Cache", new.Name)
				// Update flow cache with standard flows
				npw.updateServiceFlowCache(svc.(*kapi.Service), true, false)
				addSharedGatewayIptRules(svc.(*kapi.Service), false, npw.nodeIPManager.addresses.List())
			}
		}
		// Add Special ETP flows if there are now host endpoints
		if !countHostNeworkEndpoints(old, &npw.nodeIPManager.addresses) && countHostNeworkEndpoints(new, &npw.nodeIPManager.addresses) {
			// Update flow cache with special ones
			klog.V(5).Infof("Service %s has ExternalTrafficPolicy=local and didn't have local Endpoints but now does Updating flow Cache", new.Name)
			npw.updateServiceFlowCache(svc.(*kapi.Service), true, true)
			needFlowSync = true
			// delete standard IpTRules to add special ones
			delSharedGatewayIptRules(svc.(*kapi.Service), false, npw.nodeIPManager.addresses.List())
			addSharedGatewayIptRules(svc.(*kapi.Service), true, npw.nodeIPManager.addresses.List())
		}

		if needFlowSync {
			npw.ofm.requestFlowSync()
		}
	}
}

// since we share the host's k8s node IP, add OpenFlow flows to br-ex
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT

func newSharedGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration) (*openflowManager, error) {
	dftFlows, err := flowsForDefaultBridge(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch, gwBridge.ips)
	if err != nil {
		return nil, err
	}
	dftCommonFlows := commonFlows(gwBridge.ofPortPhys, gwBridge.macAddress.String(), gwBridge.ofPortPatch)
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
		exGWBridgeDftFlows := commonFlows(exGWBridge.ofPortPhys, exGWBridge.macAddress.String(), exGWBridge.ofPortPatch)
		ofm.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

func flowsForDefaultBridge(ofPortPhys, bridgeMacAddress, ofPortPatch string, bridgeIPs []*net.IPNet) ([]string, error) {
	HostMasqCTZone := config.Default.ConntrackZone + 1
	OVNMasqCTZone := HostMasqCTZone + 1
	var dftFlows []string
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=60, in_port=%s, dl_dst=%s, udp, udp_dst=%d, "+
				"actions=output:LOCAL", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=55, in_port=%s, udp, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

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
			fmt.Sprintf("cookie=%s, priority=500, in_port=LOCAL, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, types.V4OVNMasqueradeIP, OVNMasqCTZone))
	}
	if config.IPv6Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=60, in_port=%s, dl_dst=%s, udp6, udp_dst=%d, "+
				"actions=output:LOCAL", defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, config.Default.EncapPort))
		// perform NORMAL action otherwise.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=55, in_port=%s, udp6, udp_dst=%d, "+
				"actions=NORMAL", defaultOpenFlowCookie, ofPortPhys, config.Default.EncapPort))

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

	// table 1, we check to see if this dest mac is the shared mac, if so send to host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:LOCAL",
			defaultOpenFlowCookie, bridgeMacAddress))

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=2, "+
			"actions=mod_dl_dst=%s,output:%s", defaultOpenFlowCookie, bridgeMacAddress, ofPortPatch))

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],mod_dl_dst=%s,output:LOCAL",
			defaultOpenFlowCookie, bridgeMacAddress))

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

func commonFlows(ofPortPhys, bridgeMacAddress, ofPortPatch string) []string {
	var dftFlows []string
	maxPktLength := getMaxFrameLength()

	// table 0, we check to see if this dest mac is the shared mac, if so flood to both ports
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=output:%s,output:LOCAL",
			defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, ofPortPatch))

	if config.IPv4Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ofPortPhys))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
				"actions=ct(zone=%d, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
	}
	if config.IPv6Mode {
		// table 0, packets coming from pods headed externally. Commit connections
		// so that reverse direction goes back to the pods.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
				"actions=ct(commit, zone=%d), output:%s",
				defaultOpenFlowCookie, ofPortPatch, config.Default.ConntrackZone, ofPortPhys))

		// table 0, packets coming from external. Send it through conntrack and
		// resubmit to table 1 to know the state of the connection.
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

	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=1, dl_dst=%s, actions=output:LOCAL",
			defaultOpenFlowCookie, bridgeMacAddress))

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
				defaultOpenFlowCookie, ofPortPhys, ofPortPatch))
	}

	if config.IPv4Mode {
		// We send BFD traffic both on the host and in ovn

		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:LOCAL",
				defaultOpenFlowCookie, ofPortPhys, ofPortPatch))

	}

	// New dispatch table 11
	// packets larger than known acceptable MTU need to go to kernel to create ICMP frag needed
	if !config.Gateway.DisablePacketMTUCheck {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
				"actions=LOCAL", defaultOpenFlowCookie))
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
	return nil
}

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string, nodeAnnotator kube.Annotator, cfg *managementPortConfig, watchFactory factory.ObjectCacheInterface) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, subnets, gwNextHops, nodeAnnotator)
	if err != nil {
		return nil, err
	}
	// add masquerade subnet route to avoid zeroconf routes
	err = addMasqueradeRoute(gwBridge.bridgeName, gwNextHops)
	if err != nil {
		return nil, err
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

		gw.nodeIPManager = newAddressManager(nodeAnnotator, cfg)

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

func newNodePortWatcher(patchPort, gwBridge, gwIntf string, ips []*net.IPNet, ofm *openflowManager, nodeIPManager *addressManager, watchFactory factory.ObjectCacheInterface) (*nodePortWatcher, error) {
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

	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(ips)

	npw := &nodePortWatcher{
		gatewayIPv4:   gatewayIPv4,
		gatewayIPv6:   gatewayIPv6,
		ofportPhys:    ofportPhys,
		ofportPatch:   ofportPatch,
		gwBridge:      gwBridge,
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

func addMasqueradeRoute(bridgeName string, nextHops []net.IP) error {
	// only apply when ipv4mode is enabled
	if !config.IPv4Mode {
		return nil
	}
	bridgeLink, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to find shared gw bridge interface: %s", bridgeName)
	}
	v4nextHops, err := util.MatchIPFamily(false, nextHops)
	if err != nil {
		return fmt.Errorf("no valid ipv4 next hop exists: %v", err)
	}
	_, masqIPNet, _ := net.ParseCIDR(types.V4MasqueradeSubnet)
	exists, err := util.LinkRouteExists(bridgeLink, v4nextHops[0], masqIPNet)
	if err != nil {
		return fmt.Errorf("failed to check if route exists for masquerade subnet, error: %v", err)
	}
	if exists {
		return nil
	}
	err = util.LinkRoutesAdd(bridgeLink, v4nextHops[0], []*net.IPNet{masqIPNet}, 0)
	if os.IsExist(err) {
		klog.V(5).Infof("Ignoring error %s from 'route add %s via %s'",
			err.Error(), masqIPNet, v4nextHops[0])
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to add OVN masquerade route to host, error: %v", err)
	}
	return nil
}
