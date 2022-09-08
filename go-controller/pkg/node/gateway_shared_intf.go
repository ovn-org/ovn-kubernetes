package node

import (
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// defaultOpenFlowCookie identifies default open flow rules added to the host OVS bridge.
	// The hex number 0xdeff105, aka defflos, is meant to sound like default flows.
	defaultOpenFlowCookie = "0xdeff105"
	// etpSvcOpenFlowCookie identifies constant open flow rules added to the host OVS
	// bridge to move packets between host and external for etp=local traffic.
	// The hex number 0xe745ecf105, represents etp(e74)-service(5ec)-flows which makes it easier for debugging.
	etpSvcOpenFlowCookie = "0xe745ecf105"
	// ovsLocalPort is the name of the OVS bridge local port
	ovsLocalPort = "LOCAL"
	// ctMarkOVN is the conntrack mark value for OVN traffic
	ctMarkOVN = "0x1"
	// ctMarkHost is the conntrack mark value for host traffic
	ctMarkHost = "0x2"
	// ovnkubeITPMark is the fwmark used for host->ITP=local svc traffic. Note that the fwmark is not a part
	// of the packet, but just stored by kernel in its memory to track/filter packet. Hence fwmark is lost as
	// soon as packet exits the host.
	ovnkubeITPMark = "0x1745ec" // constant itp(174)-service(5ec)
	// ovnkubeSvcViaMgmPortRT is the number of the custom routing table used to steer host->service
	// traffic packets into OVN via ovn-k8s-mp0. Currently only used for ITP=local traffic.
	ovnkubeSvcViaMgmPortRT = "7"
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
	nodeName    string
	// Map of service name to programmed iptables/OF rules
	serviceInfo           map[ktypes.NamespacedName]*serviceConfig
	serviceInfoLock       sync.Mutex
	egressServiceInfo     map[ktypes.NamespacedName]*serviceEps
	egressServiceInfoLock sync.Mutex
	ofm                   *openflowManager
	nodeIPManager         *addressManager
	watchFactory          factory.NodeWatchFactory
}

type serviceConfig struct {
	// Contains the current service
	service *kapi.Service
	// hasLocalHostNetworkEp will be true for a service if it has at least one endpoint which is "hostnetworked&local-to-this-node".
	hasLocalHostNetworkEp bool
}

type serviceEps struct {
	v4 sets.String
	v6 sets.String
}

// updateServiceFlowCache handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (nodeport, external, ingress). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//        case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//        case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, add, hasLocalHostNetworkEp bool) {
	var cookie, key string
	var err error

	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()
	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)

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
				if isServiceTypeETPLocal && hasLocalHostNetworkEp {
					// case1 (see function description for details)
					var nodeportFlows []string
					klog.V(5).Infof("Adding flows on breth0 for Nodeport Service %s in Namespace: %s since ExternalTrafficPolicy=local", service.Name, service.Namespace)
					// table 0, This rule matches on all traffic with dst port == NodePort, DNAT's the nodePort to the svc targetPort
					// If ipv6 make sure to choose the ipv6 node address for rule
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
						// table 6, Sends the packet to the host. Note that the constant etp svc cookie is used since this flow would be
						// same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
							etpSvcOpenFlowCookie),
						// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(zone=%d nat,table=7)",
							cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
						// table 7, Sends the packet back out eth0 to the external client. Note that the constant etp svc
						// cookie is used since this would be same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=7, "+
							"actions=output:%s", etpSvcOpenFlowCookie, npw.ofportPhys))
					npw.ofm.updateFlowCacheEntry(key, nodeportFlows)
				} else if config.Gateway.Mode == config.GatewayModeShared {
					// case2 (see function description for details)
					npw.ofm.updateFlowCacheEntry(key, []string{
						// table=0, matches on service traffic towards nodePort and sends it to OVN pipeline
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, "+
							"actions=%s",
							cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, actions),
						// table=0, matches on return traffic from service nodePort and sends it out to primary node interface (br-ex)
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
				err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, hasLocalHostNetworkEp, protocol, actions, ing.IP, "Ingress")
				if err != nil {
					klog.Errorf(err.Error())
				}
			}
		}
		// flows for externalIPs
		for _, externalIP := range service.Spec.ExternalIPs {
			err = npw.createLbAndExternalSvcFlows(service, &svcPort, add, hasLocalHostNetworkEp, protocol, actions, externalIP, "External")
			if err != nil {
				klog.Errorf(err.Error())
			}
		}
	}
}

// createLbAndExternalSvcFlows handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (externalIP and LoadBalancer types). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//        case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//        case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
// `protocol` is TCP/UDP/SCTP as set in the svc.Port
// `actions`: based on config.Gateway.DisablePacketMTUCheck, this will either be "send to patchport" or "send to table11 for checking if it needs to go to kernel for ICMP needs frag"
// `externalIPOrLBIngressIP` is either externalIP.IP or LB.status.ingress.IP
// `ipType` is either "External" or "Ingress"
func (npw *nodePortWatcher) createLbAndExternalSvcFlows(service *kapi.Service, svcPort *kapi.ServicePort, add bool, hasLocalHostNetworkEp bool, protocol string, actions string, externalIPOrLBIngressIP string, ipType string) error {
	if net.ParseIP(externalIPOrLBIngressIP) == nil {
		return fmt.Errorf("failed to parse %s IP: %q", ipType, externalIPOrLBIngressIP)
	}
	flowProtocol := protocol
	nwDst := "nw_dst"
	nwSrc := "nw_src"
	if utilnet.IsIPv6String(externalIPOrLBIngressIP) {
		flowProtocol = protocol + "6"
		nwDst = "ipv6_dst"
		nwSrc = "ipv6_src"
	}
	cookie, err := svcToCookie(service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port)
	if err != nil {
		klog.Warningf("Unable to generate cookie for %s svc: %s, %s, %s, %d, error: %v",
			ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, svcPort.Port, err)
		cookie = "0"
	}
	key := strings.Join([]string{ipType, service.Namespace, service.Name, externalIPOrLBIngressIP, fmt.Sprintf("%d", svcPort.Port)}, "_")
	// Delete if needed and skip to next protocol
	if !add {
		npw.ofm.deleteFlowsByKey(key)
		return nil
	}
	// add the ARP bypass flow regarless of service type or gateway modes since its applicable in all scenarios.
	arpFlow := npw.generateArpBypassFlow(protocol, externalIPOrLBIngressIP, cookie)
	externalIPFlows := []string{arpFlow}
	// This allows external traffic ingress when the svc's ExternalTrafficPolicy is
	// set to Local, and the backend pod is HostNetworked. We need to add
	// Flows that will DNAT all external traffic destined for the lb/externalIP service
	// to the nodeIP / nodeIP:port of the host networked backend.
	// And then ensure that return traffic is UnDNATed correctly back
	// to the ingress / external IP
	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)
	if isServiceTypeETPLocal && hasLocalHostNetworkEp {
		// case1 (see function description for details)
		klog.V(5).Infof("Adding flows on breth0 for %s Service %s in Namespace: %s since ExternalTrafficPolicy=local", ipType, service.Name, service.Namespace)
		// table 0, This rule matches on all traffic with dst ip == LoadbalancerIP / externalIP, DNAT's the nodePort to the svc targetPort
		// If ipv6 make sure to choose the ipv6 node address for rule
		if strings.Contains(flowProtocol, "6") {
			externalIPFlows = append(externalIPFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=[%s]:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
		} else {
			externalIPFlows = append(externalIPFlows,
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, HostNodePortCTZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
		}
		externalIPFlows = append(externalIPFlows,
			// table 6, Sends the packet to Host. Note that the constant etp svc cookie is used since this flow would be
			// same for all such services.
			fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
				etpSvcOpenFlowCookie),
			// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
			fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
				cookie, flowProtocol, svcPort.TargetPort.String(), HostNodePortCTZone),
			// table 7, Sends the reply packet back out eth0 to the external client. Note that the constant etp svc
			// cookie is used since this would be same for all such services.
			fmt.Sprintf("cookie=%s, priority=110, table=7, actions=output:%s",
				etpSvcOpenFlowCookie, npw.ofportPhys))
	} else if config.Gateway.Mode == config.GatewayModeShared {
		// case2 (see function description for details)
		externalIPFlows = append(externalIPFlows,
			// table=0, matches on service traffic towards externalIP or LB ingress and sends it to OVN pipeline
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
				"actions=%s",
				cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, actions),
			// table=0, matches on return traffic from service externalIP or LB ingress and sends it out to primary node interface (br-ex)
			fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_src=%d, "+
				"actions=output:%s",
				cookie, npw.ofportPatch, flowProtocol, nwSrc, externalIPOrLBIngressIP, svcPort.Port, npw.ofportPhys))
	}
	npw.ofm.updateFlowCacheEntry(key, externalIPFlows)

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
func (npw *nodePortWatcher) getAndSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool) (old *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	old, exists = npw.serviceInfo[index]
	npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp}
	return old, exists
}

// addOrSetServiceInfo creates and sets the serviceConfig if it doesn't exist
func (npw *nodePortWatcher) addOrSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool) (exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if _, exists := npw.serviceInfo[index]; !exists {
		// Only set this if it doesn't exist
		npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp}
		return false
	}
	return true

}

// updateServiceInfo sets the serviceConfig for a service and returns the existing serviceConfig, if inputs are nil
// do not update those fields, if it does not exist return nil.
func (npw *nodePortWatcher) updateServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp *bool) (old *serviceConfig, exists bool) {

	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if old, exists = npw.serviceInfo[index]; !exists {
		klog.V(5).Infof("No serviceConfig found for service %s in namespace %s", index.Name, index.Namespace)
		return nil, exists
	}

	if service != nil {
		npw.serviceInfo[index].service = service
	}

	if hasLocalHostNetworkEp != nil {
		npw.serviceInfo[index].hasLocalHostNetworkEp = *hasLocalHostNetworkEp
	}

	return old, exists
}

// addServiceRules ensures the correct iptables rules and OpenFlow physical
// flows are programmed for a given service and endpoint configuration
func addServiceRules(service *kapi.Service, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) {
	// For dpu or Full mode
	if npw != nil {
		npw.updateServiceFlowCache(service, true, svcHasLocalHostNetEndPnt)
		npw.ofm.requestFlowSync()
		if !npw.dpuMode {
			// add iptable rules only in full mode
			addGatewayIptRules(service, svcHasLocalHostNetEndPnt)
			updateEgressSVCIptRules(service, npw)
		}
		return
	}
	// For Host Only Mode
	addGatewayIptRules(service, svcHasLocalHostNetEndPnt)
}

// delServiceRules deletes all possible iptables rules and OpenFlow physical
// flows for a service
func delServiceRules(service *kapi.Service, npw *nodePortWatcher) {
	// full mode || dpu mode
	if npw != nil {
		npw.updateServiceFlowCache(service, false, false)
		npw.ofm.requestFlowSync()
		if !npw.dpuMode {
			// Always try and delete all rules here in full mode & in host only mode. We don't touch iptables in dpu mode.
			// +--------------------------+-----------------------+-----------------------+--------------------------------+
			// | svcHasLocalHostNetEndPnt | ExternalTrafficPolicy | InternalTrafficPolicy |     Scenario for deletion      |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes the MARK          |
			// |         false            |         cluster       |          local        |      rules for itp=local       |
			// |                          |                       |                       |       called from mangle       |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes the REDIRECT      |
			// |         true             |         cluster       |          local        |      rules towards target      |
			// |                          |                       |                       |       port for itp=local       |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       | deletes the DNAT rules for     |
			// |         false            |          local        |          cluster      |    non-local-host-net          |
			// |                          |                       |                       | eps towards masqueradeIP +     |
			// |                          |                       |                       | DNAT rules towards clusterIP   |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |    deletes the DNAT rules      |
			// |       false||true        |          cluster      |          cluster      |   	towards clusterIP          |
			// |                          |                       |                       |       for the default case     |
			// |--------------------------|-----------------------|-----------------------|--------------------------------|
			// |                          |                       |                       |      deletes all the rules     |
			// |       false||true        |          local        |          local        |   for etp=local + itp=local    |
			// |                          |                       |                       |   + default dnat towards CIP   |
			// +--------------------------+-----------------------+-----------------------+--------------------------------+

			delGatewayIptRules(service, true)
			delGatewayIptRules(service, false)
			delAllEgressSVCIptRules(service, npw)
		}
		return
	}

	delGatewayIptRules(service, true)
	delGatewayIptRules(service, false)
}

func serviceUpdateNotNeeded(old, new *kapi.Service) bool {
	return reflect.DeepEqual(new.Spec.Ports, old.Spec.Ports) &&
		reflect.DeepEqual(new.Spec.ExternalIPs, old.Spec.ExternalIPs) &&
		reflect.DeepEqual(new.Spec.ClusterIP, old.Spec.ClusterIP) &&
		reflect.DeepEqual(new.Spec.ClusterIPs, old.Spec.ClusterIPs) &&
		reflect.DeepEqual(new.Spec.Type, old.Spec.Type) &&
		reflect.DeepEqual(new.Status.LoadBalancer.Ingress, old.Status.LoadBalancer.Ingress) &&
		reflect.DeepEqual(new.Spec.ExternalTrafficPolicy, old.Spec.ExternalTrafficPolicy) &&
		(new.Spec.InternalTrafficPolicy != nil && old.Spec.InternalTrafficPolicy != nil &&
			reflect.DeepEqual(*new.Spec.InternalTrafficPolicy, *old.Spec.InternalTrafficPolicy)) &&
		!util.EgressSVCHostChanged(old, new)
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) {
	var hasLocalHostNetworkEp bool
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return
	}

	klog.V(5).Infof("Adding service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	epSlices, err := npw.watchFactory.GetEndpointSlices(service.Namespace, service.Name)
	if err != nil {
		klog.V(5).Infof("No endpointslice found for service %s in namespace %s during service Add", service.Name, service.Namespace)
		// No endpoint object exists yet so default to false
		hasLocalHostNetworkEp = false
	} else {
		nodeIPs := npw.nodeIPManager.ListAddresses()
		hasLocalHostNetworkEp = hasLocalHostNetworkEndpoints(epSlices, nodeIPs)
	}

	// If something didn't already do it add correct Service rules
	if exists := npw.addOrSetServiceInfo(name, service, hasLocalHostNetworkEp); !exists {
		klog.V(5).Infof("Service Add %s event in namespace %s came before endpoint event setting svcConfig", service.Name, service.Namespace)
		addServiceRules(service, hasLocalHostNetworkEp, npw)
	} else {
		klog.V(5).Infof("Rules already programmed for %s in namespace %s", service.Name, service.Namespace)
	}
}

func (npw *nodePortWatcher) UpdateService(old, new *kapi.Service) {
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs, .Spec.Type, .Status.LoadBalancer.Ingress, "+
			".Spec.ExternalTrafficPolicy, .Spec.InternalTrafficPolicy, Egress service host", new.Name)
		return
	}
	// Update the service in svcConfig if we need to so that other handler
	// threads do the correct thing, leave hasLocalHostNetworkEp alone in the cache
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
		addServiceRules(new, svcConfig.hasLocalHostNetworkEp, npw)
	}
}

// deleteConntrackForServiceVIP deletes the conntrack entries for the provided svcVIP:svcPort by comparing them to ConntrackOrigDstIP:ConntrackOrigDstPort
func deleteConntrackForServiceVIP(svcVIPs []string, svcPorts []kapi.ServicePort, ns, name string) error {
	for _, svcVIP := range svcVIPs {
		for _, svcPort := range svcPorts {
			err := util.DeleteConntrack(svcVIP, svcPort.Port, svcPort.Protocol, netlink.ConntrackOrigDstIP, nil)
			if err != nil {
				return fmt.Errorf("failed to delete conntrack entry for service %s/%s with svcVIP %s, svcPort %d, protocol %s: %v",
					ns, name, svcVIP, svcPort.Port, svcPort.Protocol, err)
			}
		}
	}
	return nil
}

// deleteConntrackForService deletes the conntrack entries corresponding to the service VIPs of the provided service
func (npw *nodePortWatcher) deleteConntrackForService(service *kapi.Service) error {
	// remove conntrack entries for LB VIPs and External IPs
	externalIPs := util.GetExternalAndLBIPs(service)
	if err := deleteConntrackForServiceVIP(externalIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	if util.ServiceTypeHasNodePort(service) {
		// remove conntrack entries for NodePorts
		nodeIPs := npw.nodeIPManager.ListAddresses()
		for _, nodeIP := range nodeIPs {
			for _, svcPort := range service.Spec.Ports {
				err := util.DeleteConntrack(nodeIP.String(), svcPort.NodePort, svcPort.Protocol, netlink.ConntrackOrigDstIP, nil)
				if err != nil {
					return fmt.Errorf("failed to delete conntrack entry for service %s/%s with nodeIP %s, nodePort %d, protocol %s: %v",
						service.Namespace, service.Name, nodeIP, svcPort.Port, svcPort.Protocol, err)
				}
			}
		}
	}
	// remove conntrack entries for ClusterIPs
	clusterIPs := util.GetClusterIPs(service)
	if err := deleteConntrackForServiceVIP(clusterIPs, service.Spec.Ports, service.Namespace, service.Name); err != nil {
		return err
	}
	return nil
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
	// Remove all conntrack entries for the serviceVIPs of this service irrespective of protocol stack
	// since service deletion is considered as unplugging the network cable and hence graceful termination
	// is not guaranteed. See https://github.com/kubernetes/kubernetes/issues/108523#issuecomment-1074044415.
	err := npw.deleteConntrackForService(service)
	if err != nil {
		klog.Errorf("Failed to delete conntrack entry for service %v: %v", name, err)
	}

}

func (npw *nodePortWatcher) SyncServices(services []interface{}) error {
	keepIPTRules := []iptRule{}
	for _, serviceInterface := range services {
		name := ktypes.NamespacedName{Namespace: serviceInterface.(*kapi.Service).Namespace, Name: serviceInterface.(*kapi.Service).Name}

		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}

		epSlices, err := npw.watchFactory.GetEndpointSlices(service.Namespace, service.Name)
		if err != nil {
			klog.V(5).Infof("No endpointslice found for service %s in namespace %s during sync", service.Name, service.Namespace)
			continue
		}
		nodeIPs := npw.nodeIPManager.ListAddresses()
		hasLocalHostNetworkEp := hasLocalHostNetworkEndpoints(epSlices, nodeIPs)
		npw.getAndSetServiceInfo(name, service, hasLocalHostNetworkEp)
		// Delete OF rules for service if they exist
		npw.updateServiceFlowCache(service, false, hasLocalHostNetworkEp)
		npw.updateServiceFlowCache(service, true, hasLocalHostNetworkEp)
		// Add correct iptables rules only for Full mode
		if !npw.dpuMode {
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, hasLocalHostNetworkEp)...)
		}

		if !npw.dpuMode && shouldConfigureEgressSVC(service, npw) {
			v4Eps := sets.NewString()
			v6Eps := sets.NewString()

			for _, epSlice := range epSlices {
				if epSlice.AddressType == discovery.AddressTypeFQDN {
					continue
				}
				epsToInsert := v4Eps
				if epSlice.AddressType == discovery.AddressTypeIPv6 {
					epsToInsert = v6Eps
				}

				for _, ep := range epSlice.Endpoints {
					for _, ip := range ep.Addresses {
						if !isHostEndpoint(ip) {
							epsToInsert.Insert(ip)
						}
					}
				}
			}

			keepIPTRules = append(keepIPTRules, egressSVCIPTRulesForEndpoints(service, v4Eps.UnsortedList(), v6Eps.UnsortedList())...)

			npw.egressServiceInfoLock.Lock()
			npw.egressServiceInfo[name] = &serviceEps{v4: v4Eps, v6: v6Eps}
			npw.egressServiceInfoLock.Unlock()
		}
	}
	// sync OF rules once
	npw.ofm.requestFlowSync()
	// sync IPtables rules once only for Full mode
	if !npw.dpuMode {
		// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
		for _, chain := range []string{iptableITPChain, iptableESVCChain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain, iptableMgmPortChain} {
			recreateIPTRules("nat", chain, keepIPTRules)
		}
		recreateIPTRules("mangle", iptableITPChain, keepIPTRules)
	}
	// FIXME(FF): This function must propagate errors back to caller.
	// https://bugzilla.redhat.com/show_bug.cgi?id=2081857
	return nil
}

func (npw *nodePortWatcher) AddEndpointSlice(epSlice *discovery.EndpointSlice) {
	svcName := epSlice.Labels[discovery.LabelServiceName]
	svc, err := npw.watchFactory.GetService(epSlice.Namespace, svcName)
	if err != nil {
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		klog.V(5).Infof("No service found for endpointslice %s in namespace %s during add", epSlice.Name, epSlice.Namespace)
		return
	}

	if !util.ServiceTypeHasClusterIP(svc) || !util.IsClusterIPSet(svc) {
		return
	}

	klog.V(5).Infof("Adding endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	nodeIPs := npw.nodeIPManager.ListAddresses()
	hasLocalHostNetworkEp := hasLocalHostNetworkEndpoints([]*discovery.EndpointSlice{epSlice}, nodeIPs)

	// Here we make sure the correct rules are programmed whenever an AddEndpointSlice
	// event is received, only alter flows if we need to, i.e if cache wasn't
	// set or if it was and hasLocalHostNetworkEp state changed, to prevent flow churn
	namespacedName := namespacedNameFromEPSlice(epSlice)
	out, exists := npw.getAndSetServiceInfo(namespacedName, svc, hasLocalHostNetworkEp)
	if !exists {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is creating rules", epSlice.Name, epSlice.Namespace)
		addServiceRules(svc, hasLocalHostNetworkEp, npw)
		return
	}

	if out.hasLocalHostNetworkEp != hasLocalHostNetworkEp {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is updating rules", epSlice.Name, epSlice.Namespace)
		delServiceRules(svc, npw)
		addServiceRules(svc, hasLocalHostNetworkEp, npw)
		return
	}

	// Call this in case it wasn't already called by addServiceRules
	npw.egressServiceInfoLock.Lock()
	_, found := npw.egressServiceInfo[namespacedName]
	npw.egressServiceInfoLock.Unlock()
	if found && !npw.dpuMode {
		updateEgressSVCIptRules(svc, npw)
	}

}

func (npw *nodePortWatcher) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) {
	var hasLocalHostNetworkEp = false

	klog.V(5).Infof("Deleting endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	// remove rules for endpoints and add back normal ones
	namespacedName := namespacedNameFromEPSlice(epSlice)
	if svcConfig, exists := npw.updateServiceInfo(namespacedName, nil, &hasLocalHostNetworkEp); exists {
		// Lock the cache mutex here so we don't miss a service delete during an endpoint delete
		// we have to do this because deleting and adding iptables rules is slow.
		npw.serviceInfoLock.Lock()
		defer npw.serviceInfoLock.Unlock()

		delServiceRules(svcConfig.service, npw)
		addServiceRules(svcConfig.service, hasLocalHostNetworkEp, npw)
	}
}

func getEndpointAddresses(endpointSlice *discovery.EndpointSlice) []string {
	endpointsAddress := make([]string, 0)
	for _, endpoint := range endpointSlice.Endpoints {
		endpointsAddress = append(endpointsAddress, endpoint.Addresses...)
	}
	return endpointsAddress
}

func (npw *nodePortWatcher) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) {
	oldEpAddr := getEndpointAddresses(oldEpSlice)
	newEpAddr := getEndpointAddresses(newEpSlice)
	if reflect.DeepEqual(oldEpAddr, newEpAddr) {
		return
	}

	namespacedName := namespacedNameFromEPSlice(oldEpSlice)

	if _, exists := npw.getServiceInfo(namespacedName); !exists {
		// When a service is updated from externalName to nodeport type, it won't be
		// in nodePortWatcher cache (npw): in this case, have the new nodeport IPtable rules
		// installed.
		npw.AddEndpointSlice(newEpSlice)
	} else if len(newEpAddr) == 0 {
		// With no endpoint addresses in new endpointslice, delete old endpoint rules
		// and add normal ones back
		npw.DeleteEndpointSlice(oldEpSlice)
	}

	klog.V(5).Infof("Updating endpointslice %s in namespace %s", oldEpSlice.Name, oldEpSlice.Namespace)

	// Update rules if hasHostNetworkEndpoints status changed
	nodeIPs := npw.nodeIPManager.ListAddresses()
	hasLocalHostNetworkEpOld := hasLocalHostNetworkEndpoints([]*discovery.EndpointSlice{oldEpSlice}, nodeIPs)
	hasLocalHostNetworkEpNew := hasLocalHostNetworkEndpoints([]*discovery.EndpointSlice{newEpSlice}, nodeIPs)
	if hasLocalHostNetworkEpOld != hasLocalHostNetworkEpNew {
		npw.DeleteEndpointSlice(oldEpSlice)
		npw.AddEndpointSlice(newEpSlice)
		return
	}

	// Call this in case it wasn't already called by addServiceRules
	npw.egressServiceInfoLock.Lock()
	_, found := npw.egressServiceInfo[namespacedName]
	npw.egressServiceInfoLock.Unlock()
	if found && !npw.dpuMode {
		svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
		if err != nil {
			return
		}
		updateEgressSVCIptRules(svc, npw)
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
	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs, .Spec.Type, .Status.LoadBalancer.Ingress, Egress service annotations", new.Name)
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

func (npwipt *nodePortWatcherIptables) SyncServices(services []interface{}) error {
	keepIPTRules := []iptRule{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// Add correct iptables rules.
		// TODO: ETP and ITP is not implemented for smart NIC mode.
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, false)...)
	}

	// sync IPtables rules once
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		recreateIPTRules("nat", chain, keepIPTRules)
	}
	// FIXME(FF): This function must propagate errors back to caller.
	// https://bugzilla.redhat.com/show_bug.cgi?id=2081858
	return nil
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration, extraIPs []net.IP) (*openflowManager, error) {
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

	if err := ofm.updateBridgeFlowCache(extraIPs); err != nil {
		return nil, err
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

// updateBridgeFlowCache generates the "static" per-bridge flows
// note: this is shared between shared and local gateway modes
func (ofm *openflowManager) updateBridgeFlowCache(extraIPs []net.IP) error {
	dftFlows, err := flowsForDefaultBridge(ofm.defaultBridge, extraIPs)
	if err != nil {
		return err
	}
	dftCommonFlows := commonFlows(ofm.defaultBridge)
	dftFlows = append(dftFlows, dftCommonFlows...)

	ofm.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	ofm.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if ofm.externalGatewayBridge != nil {
		ofm.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		exGWBridgeDftFlows := commonFlows(ofm.externalGatewayBridge)
		ofm.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}
	return nil
}

func flowsForDefaultBridge(bridge *bridgeConfiguration, extraIPs []net.IP) ([]string, error) {
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortPatch := bridge.ofPortPatch
	ofPortHost := bridge.ofPortHost
	bridgeIPs := bridge.ips

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

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() == nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
					"actions=ct(commit,zone=%d,table=4)",
					defaultOpenFlowCookie, ofPortPatch, ip.String(), physicalIP.IP,
					HostMasqCTZone))
		}

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

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() != nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
					"actions=ct(commit,zone=%d,table=4)",
					defaultOpenFlowCookie, ofPortPatch, ip.String(), physicalIP.IP,
					HostMasqCTZone))
		}

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

func commonFlows(bridge *bridgeConfiguration) []string {
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortPatch := bridge.ofPortPatch
	ofPortHost := bridge.ofPortHost

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

// initSvcViaMgmPortRoutingRules creates the svc2managementport routing table, routes and rules
// that let's us forward service traffic to ovn-k8s-mp0 as opposed to the default route towards breth0
func initSvcViaMgmPortRoutingRules(hostSubnets []*net.IPNet) error {
	// create ovnkubeSvcViaMgmPortRT and service route towards ovn-k8s-mp0
	for _, hostSubnet := range hostSubnets {
		isIPv6 := utilnet.IsIPv6CIDR(hostSubnet)
		gatewayIP := util.GetNodeGatewayIfAddr(hostSubnet).IP.String()
		for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
			if isIPv6 == utilnet.IsIPv6CIDR(svcCIDR) {
				if stdout, stderr, err := util.RunIP("route", "replace", "table", ovnkubeSvcViaMgmPortRT, svcCIDR.String(), "via", gatewayIP, "dev", types.K8sMgmtIntfName); err != nil {
					return fmt.Errorf("error adding routing table entry into custom routing table: %s: stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
				}
				klog.V(5).Infof("Successfully added route into custom routing table: %s", ovnkubeSvcViaMgmPortRT)
			}
		}
	}

	createRule := func(family string) error {
		stdout, stderr, err := util.RunIP(family, "rule")
		if err != nil {
			return fmt.Errorf("error listing routing rules, stdout: %s, stderr: %s, err: %v", stdout, stderr, err)
		}
		if !strings.Contains(stdout, fmt.Sprintf("from all fwmark %s lookup %s", ovnkubeITPMark, ovnkubeSvcViaMgmPortRT)) {
			if stdout, stderr, err := util.RunIP(family, "rule", "add", "fwmark", ovnkubeITPMark, "lookup", ovnkubeSvcViaMgmPortRT, "prio", "30"); err != nil {
				return fmt.Errorf("error adding routing rule for service via management table (%s): stdout: %s, stderr: %s, err: %v", ovnkubeSvcViaMgmPortRT, stdout, stderr, err)
			}
		}
		return nil
	}

	// create ip rule that will forward ovnkubeITPMark marked packets to ovnkubeITPRoutingTable
	if config.IPv4Mode {
		if err := createRule("-4"); err != nil {
			return fmt.Errorf("could not add IPv4 rule: %v", err)
		}
	}
	if config.IPv6Mode {
		if err := createRule("-6"); err != nil {
			return fmt.Errorf("could not add IPv6 rule: %v", err)
		}
	}

	// lastly update the reverse path filtering options for ovn-k8s-mp0 interface to avoid dropping return packets
	// NOTE: v6 doesn't have rp_filter strict mode block
	rpFilterLooseMode := "2"
	// TODO: Convert testing framework to mock golang module utilities. Example:
	// result, err := sysctl.Sysctl(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", types.K8sMgmtIntfName), rpFilterLooseMode)
	stdout, stderr, err := util.RunSysctl("-w", fmt.Sprintf("net.ipv4.conf.%s.rp_filter=%s", types.K8sMgmtIntfName, rpFilterLooseMode))
	if err != nil || stdout != fmt.Sprintf("net.ipv4.conf.%s.rp_filter = %s", types.K8sMgmtIntfName, rpFilterLooseMode) {
		return fmt.Errorf("could not set the correct rp_filter value for interface %s: stdout: %v, stderr: %v, err: %v",
			types.K8sMgmtIntfName, stdout, stderr, err)
	}

	return nil
}

func newSharedGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet, nodeAnnotator kube.Annotator, kube kube.Interface, cfg *managementPortConfig, watchFactory factory.NodeWatchFactory) (*gateway, error) {
	klog.Info("Creating new shared gateway")
	gw := &gateway{}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, gwNextHops, gwIPs, nodeAnnotator)
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
		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory)
		nodeIPs := gw.nodeIPManager.ListAddresses()

		gw.openflowManager, err = newGatewayOpenFlowManager(gwBridge, exGwBridge, nodeIPs)
		if err != nil {
			return err
		}

		// resync flows on IP change
		gw.nodeIPManager.OnChanged = func() {
			klog.V(5).Info("Node addresses changed, re-syncing bridge flows")
			if err := gw.openflowManager.updateBridgeFlowCache(gw.nodeIPManager.ListAddresses()); err != nil {
				// very unlikely - somehow node has lost its IP address
				klog.Errorf("Failed to re-generate gateway flows after address change: %v", err)
			}
			gw.openflowManager.requestFlowSync()
		}

		if config.Gateway.NodeportEnable {
			if config.OvnKubeNode.Mode == types.NodeModeFull {
				// (TODO): Internal Traffic Policy is not supported in DPU mode
				if err := initSvcViaMgmPortRoutingRules(subnets); err != nil {
					return err
				}
			}
			klog.Info("Creating Shared Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge.patchPort, gwBridge.bridgeName, gwBridge.uplinkName, nodeName, gwBridge.ips, gw.openflowManager, gw.nodeIPManager, watchFactory)
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

func newNodePortWatcher(patchPort, gwBridge, gwIntf, nodeName string, ips []*net.IPNet, ofm *openflowManager,
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
		dpuMode:           dpuMode,
		gatewayIPv4:       gatewayIPv4,
		gatewayIPv6:       gatewayIPv6,
		ofportPhys:        ofportPhys,
		ofportPatch:       ofportPatch,
		gwBridge:          gwBridge,
		nodeName:          nodeName,
		serviceInfo:       make(map[ktypes.NamespacedName]*serviceConfig),
		egressServiceInfo: make(map[ktypes.NamespacedName]*serviceEps),
		nodeIPManager:     nodeIPManager,
		ofm:               ofm,
		watchFactory:      watchFactory,
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
