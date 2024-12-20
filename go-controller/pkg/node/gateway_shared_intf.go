package node

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"
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
	// ovnKubeNodeSNATMark is used to mark packets that need to be SNAT-ed to nodeIP for
	// traffic originating from egressIP and egressService controlled pods towards other nodes in the cluster.
	ovnKubeNodeSNATMark = "0x3f0"

	// nftablesUDNServicePreroutingChain is a base chain registered into the prerouting hook,
	// and it contains one rule that jumps to nftablesUDNServiceMarkChain.
	// Traffic from the default network's management interface is bypassed
	// to prevent enabling the default network access to the local node's UDN NodePort.
	nftablesUDNServicePreroutingChain = "udn-service-prerouting"

	// nftablesUDNServiceOutputChain is a base chain registered into the output hook
	// it contains one rule that jumps to nftablesUDNServiceMarkChain
	nftablesUDNServiceOutputChain = "udn-service-output"

	// nftablesUDNServiceMarkChain is a regular chain trying to match the incoming traffic
	// against the following UDN service verdict maps: nftablesUDNMarkNodePortsMap,
	// nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map
	nftablesUDNServiceMarkChain = "udn-service-mark"

	// nftablesUDNMarkNodePortsMap is a verdict maps containing
	// localNodeIP / protocol / port keys indicating traffic that
	// should be marked with a UDN specific value, which is used to direct the traffic
	// to the appropriate network.
	nftablesUDNMarkNodePortsMap = "udn-mark-nodeports"

	// nftablesUDNMarkExternalIPsV4Map and nftablesUDNMarkExternalIPsV6Map are verdict
	// maps containing loadBalancerIP / protocol / port keys indicating traffic that
	// should be marked with a UDN specific value, which is used to direct the traffic
	// to the appropriate network.
	nftablesUDNMarkExternalIPsV4Map = "udn-mark-external-ips-v4"
	nftablesUDNMarkExternalIPsV6Map = "udn-mark-external-ips-v6"
)

// configureUDNServicesNFTables configures the nftables chains, rules, and verdict maps
// that are used to set packet marks on externally exposed UDN services
func configureUDNServicesNFTables() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return err
	}
	tx := nft.NewTransaction()

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServiceMarkChain,
		Comment: knftables.PtrTo("UDN services packet mark"),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServiceMarkChain})

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServicePreroutingChain,
		Comment: knftables.PtrTo("UDN services packet mark - Prerouting"),

		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.PreroutingHook),
		Priority: knftables.PtrTo(knftables.ManglePriority),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServicePreroutingChain})

	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServicePreroutingChain,
		Rule: knftables.Concat(
			"iifname", "!=", fmt.Sprintf("%q", types.K8sMgmtIntfName),
			"jump", nftablesUDNServiceMarkChain,
		),
	})

	tx.Add(&knftables.Chain{
		Name:    nftablesUDNServiceOutputChain,
		Comment: knftables.PtrTo("UDN services packet mark - Output"),

		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.ManglePriority),
	})
	tx.Flush(&knftables.Chain{Name: nftablesUDNServiceOutputChain})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceOutputChain,
		Rule: knftables.Concat(
			"jump", nftablesUDNServiceMarkChain,
		),
	})

	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkNodePortsMap,
		Comment: knftables.PtrTo("UDN services NodePorts mark"),
		Type:    "inet_proto . inet_service : verdict",
	})
	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkExternalIPsV4Map,
		Comment: knftables.PtrTo("UDN services External IPs mark (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service : verdict",
	})
	tx.Add(&knftables.Map{
		Name:    nftablesUDNMarkExternalIPsV6Map,
		Comment: knftables.PtrTo("UDN services External IPs mark (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service : verdict",
	})

	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"fib daddr type local meta l4proto . th dport vmap", "@", nftablesUDNMarkNodePortsMap,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"ip daddr . meta l4proto . th dport vmap", "@", nftablesUDNMarkExternalIPsV4Map,
		),
	})
	tx.Add(&knftables.Rule{
		Chain: nftablesUDNServiceMarkChain,
		Rule: knftables.Concat(
			"ip6 daddr . meta l4proto . th dport vmap", "@", nftablesUDNMarkExternalIPsV6Map,
		),
	})

	return nft.Run(context.TODO(), tx)
}

// nodePortWatcherIptables manages iptables rules for shared gateway
// to ensure that services using NodePorts are accessible.
type nodePortWatcherIptables struct {
	networkManager networkmanager.Interface
}

func newNodePortWatcherIptables(networkManager networkmanager.Interface) *nodePortWatcherIptables {
	return &nodePortWatcherIptables{
		networkManager: networkManager,
	}
}

// nodePortWatcher manages OpenFlow and iptables rules
// to ensure that services using NodePorts are accessible
type nodePortWatcher struct {
	dpuMode       bool
	gatewayIPv4   string
	gatewayIPv6   string
	gatewayIPLock sync.Mutex
	ofportPhys    string
	gwBridge      string
	// Map of service name to programmed iptables/OF rules
	serviceInfo     map[ktypes.NamespacedName]*serviceConfig
	serviceInfoLock sync.Mutex
	ofm             *openflowManager
	nodeIPManager   *addressManager
	networkManager  networkmanager.Interface
	watchFactory    factory.NodeWatchFactory
}

type serviceConfig struct {
	// Contains the current service
	service *kapi.Service
	// hasLocalHostNetworkEp will be true for a service if it has at least one endpoint which is "hostnetworked&local-to-this-node".
	hasLocalHostNetworkEp bool
	// localEndpoints stores all the local non-host-networked endpoints for this service
	localEndpoints sets.Set[string]
}

type cidrAndFlags struct {
	ipNet             *net.IPNet
	flags             int
	preferredLifetime int
	validLifetime     int
}

func (npw *nodePortWatcher) updateGatewayIPs(addressManager *addressManager) {
	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	addressManager.gatewayBridge.Lock()
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(addressManager.gatewayBridge.ips)
	addressManager.gatewayBridge.Unlock()

	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	npw.gatewayIPv4 = gatewayIPv4
	npw.gatewayIPv6 = gatewayIPv6
}

// updateServiceFlowCache handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (nodeport, external, ingress). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
func (npw *nodePortWatcher) updateServiceFlowCache(service *kapi.Service, netInfo util.NetInfo, add, hasLocalHostNetworkEp bool) error {
	if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink && npw.ofportPhys == "" {
		// if LGW mode and no uplink gateway bridge, ingress traffic enters host from node physical interface instead of the breth0. Skip adding these service flows to br-ex.
		return nil
	}

	var netConfig *bridgeUDNConfiguration
	var actions string

	if add {
		netConfig = npw.ofm.getActiveNetwork(netInfo)
		if netConfig == nil {
			return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
		}
		actions = fmt.Sprintf("output:%s", netConfig.ofPortPatch)
	}

	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	npw.gatewayIPLock.Lock()
	defer npw.gatewayIPLock.Unlock()
	var cookie, key string
	var err error
	var errors []error

	isServiceTypeETPLocal := util.ServiceExternalTrafficPolicyLocal(service)

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
				key = strings.Join([]string{"NodePort", service.Namespace, service.Name, flowProtocol, fmt.Sprintf("%d", svcPort.NodePort)}, "_")
				// Delete if needed and skip to next protocol
				if !add {
					npw.ofm.deleteFlowsByKey(key)
					continue
				}
				cookie, err = svcToCookie(service.Namespace, service.Name, flowProtocol, svcPort.NodePort)
				if err != nil {
					klog.Warningf("Unable to generate cookie for nodePort svc: %s, %s, %s, %d, error: %v",
						service.Namespace, service.Name, flowProtocol, svcPort.Port, err)
					cookie = "0"
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
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, config.Default.HostNodePortConntrackZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
					} else {
						nodeportFlows = append(nodeportFlows,
							fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
								cookie, npw.ofportPhys, flowProtocol, svcPort.NodePort, config.Default.HostNodePortConntrackZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
					}
					nodeportFlows = append(nodeportFlows,
						// table 6, Sends the packet to the host. Note that the constant etp svc cookie is used since this flow would be
						// same for all such services.
						fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
							etpSvcOpenFlowCookie),
						// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
						fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(zone=%d nat,table=7)",
							cookie, flowProtocol, svcPort.TargetPort.String(), config.Default.HostNodePortConntrackZone),
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
						fmt.Sprintf("cookie=%s, priority=110, in_port=%s, dl_src=%s, %s, tp_src=%d, "+
							"actions=output:%s",
							cookie, netConfig.ofPortPatch, npw.ofm.getDefaultBridgeMAC(), flowProtocol, svcPort.NodePort, npw.ofportPhys)})
				}
			}
		}

		// Flows for cloud load balancers on Azure/GCP
		// Established traffic is handled by default conntrack rules
		// NodePort/Ingress access in the OVS bridge will only ever come from outside of the host
		ingParsedIPs := make([]string, 0, len(service.Status.LoadBalancer.Ingress))
		for _, ing := range service.Status.LoadBalancer.Ingress {
			if len(ing.IP) > 0 {
				ip := utilnet.ParseIPSloppy(ing.IP)
				if ip == nil {
					errors = append(errors, fmt.Errorf("failed to parse Ingress IP: %q", ing.IP))
				} else {
					ingParsedIPs = append(ingParsedIPs, ip.String())
				}
			}
		}

		// flows for externalIPs
		extParsedIPs := make([]string, 0, len(service.Spec.ExternalIPs))
		for _, externalIP := range service.Spec.ExternalIPs {
			ip := utilnet.ParseIPSloppy(externalIP)
			if ip == nil {
				errors = append(errors, fmt.Errorf("failed to parse External IP: %q", externalIP))
			} else {
				extParsedIPs = append(extParsedIPs, ip.String())
			}
		}
		var ofPorts []string
		// don't get the ports unless we need to as it is a costly operation
		if (len(extParsedIPs) > 0 || len(ingParsedIPs) > 0) && add {
			ofPorts, err = util.GetOpenFlowPorts(npw.gwBridge, false)
			if err != nil {
				// in the odd case that getting all ports from the bridge should not work,
				// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
				klog.Warningf("Unable to get port list from bridge. Using ovsLocalPort as output only: error: %v",
					err)
			}
		}
		if err = npw.createLbAndExternalSvcFlows(service, netConfig, &svcPort, add, hasLocalHostNetworkEp, protocol, actions,
			ingParsedIPs, "Ingress", ofPorts); err != nil {
			errors = append(errors, err)
		}

		if err = npw.createLbAndExternalSvcFlows(service, netConfig, &svcPort, add, hasLocalHostNetworkEp, protocol, actions,
			extParsedIPs, "External", ofPorts); err != nil {
			errors = append(errors, err)
		}
	}

	// Add flows for default network services that are accessible from UDN networks
	if util.IsNetworkSegmentationSupportEnabled() {
		// The flow added below has a higher priority than the per UDN service flow:
		//   priority=200, table=2, ip, ip_src=169.254.0.<UDN>, actions=set_field:<bridge-mac>->eth_dst,output:<UDN-patch-port>
		// This ordering ensures that traffic to UDN allowed default services goes to the the default patch port.

		if util.IsUDNEnabledService(ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}.String()) {
			key = strings.Join([]string{"UDNAllowedSVC", service.Namespace, service.Name}, "_")
			if !add {
				npw.ofm.deleteFlowsByKey(key)
				return utilerrors.Join(errors...)
			}

			ipPrefix := "ip"
			masqueradeSubnet := config.Gateway.V4MasqueradeSubnet
			if !utilnet.IsIPv4String(service.Spec.ClusterIP) {
				ipPrefix = "ipv6"
				masqueradeSubnet = config.Gateway.V6MasqueradeSubnet
			}
			// table 2, user-defined network host -> OVN towards default cluster network services
			defaultNetConfig := npw.ofm.defaultBridge.getActiveNetworkBridgeConfig(types.DefaultNetworkName)

			npw.ofm.updateFlowCacheEntry(key, []string{fmt.Sprintf("cookie=%s, priority=300, table=2, %s, %s_src=%s, %s_dst=%s, "+
				"actions=set_field:%s->eth_dst,output:%s",
				defaultOpenFlowCookie, ipPrefix, ipPrefix, masqueradeSubnet, ipPrefix, service.Spec.ClusterIP,
				npw.ofm.getDefaultBridgeMAC().String(), defaultNetConfig.ofPortPatch)})
		}
	}
	return utilerrors.Join(errors...)
}

// createLbAndExternalSvcFlows handles managing breth0 gateway flows for ingress traffic towards kubernetes services
// (externalIP and LoadBalancer types). By default incoming traffic into the node is steered directly into OVN (case3 below).
//
// case1: If a service has externalTrafficPolicy=local, and has host-networked endpoints local to the node (hasLocalHostNetworkEp),
// traffic instead will be steered directly into the host and DNAT-ed to the targetPort on the host.
//
// case2: All other types of services in SGW mode i.e:
//
//	case2a: if externalTrafficPolicy=cluster + SGW mode, traffic will be steered into OVN via GR.
//	case2b: if externalTrafficPolicy=local + !hasLocalHostNetworkEp + SGW mode, traffic will be steered into OVN via GR.
//
// NOTE: If LGW mode, the default flow will take care of sending traffic to host irrespective of service flow type.
//
// `add` parameter indicates if the flows should exist or be removed from the cache
// `hasLocalHostNetworkEp` indicates if at least one host networked endpoint exists for this service which is local to this node.
// `protocol` is TCP/UDP/SCTP as set in the svc.Port
// `actions`: "send to patchport"
// `externalIPOrLBIngressIP` is either externalIP.IP or LB.status.ingress.IP
// `ipType` is either "External" or "Ingress"
func (npw *nodePortWatcher) createLbAndExternalSvcFlows(service *kapi.Service, netConfig *bridgeUDNConfiguration, svcPort *kapi.ServicePort, add bool,
	hasLocalHostNetworkEp bool, protocol string, actions string, externalIPOrLBIngressIPs []string, ipType string, ofPorts []string) error {

	for _, externalIPOrLBIngressIP := range externalIPOrLBIngressIPs {
		// each path has per IP generates about 4-5 flows. So we preallocate a slice with capacity.
		externalIPFlows := make([]string, 0, 5)

		// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
		// that dl_src is included in match criteria!

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
			continue
		}
		// add the ARP bypass flow regardless of service type or gateway modes since its applicable in all scenarios.
		arpFlow := npw.generateARPBypassFlow(ofPorts, netConfig.ofPortPatch, externalIPOrLBIngressIP, cookie)
		externalIPFlows = append(externalIPFlows, arpFlow)
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
						cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, config.Default.HostNodePortConntrackZone, npw.gatewayIPv6, svcPort.TargetPort.String()))
			} else {
				externalIPFlows = append(externalIPFlows,
					fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, actions=ct(commit,zone=%d,nat(dst=%s:%s),table=6)",
						cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, config.Default.HostNodePortConntrackZone, npw.gatewayIPv4, svcPort.TargetPort.String()))
			}
			externalIPFlows = append(externalIPFlows,
				// table 6, Sends the packet to Host. Note that the constant etp svc cookie is used since this flow would be
				// same for all such services.
				fmt.Sprintf("cookie=%s, priority=110, table=6, actions=output:LOCAL",
					etpSvcOpenFlowCookie),
				// table 0, Matches on return traffic, i.e traffic coming from the host networked pod's port, and unDNATs
				fmt.Sprintf("cookie=%s, priority=110, in_port=LOCAL, %s, tp_src=%s, actions=ct(commit,zone=%d nat,table=7)",
					cookie, flowProtocol, svcPort.TargetPort.String(), config.Default.HostNodePortConntrackZone),
				// table 7, Sends the reply packet back out eth0 to the external client. Note that the constant etp svc
				// cookie is used since this would be same for all such services.
				fmt.Sprintf("cookie=%s, priority=110, table=7, actions=output:%s",
					etpSvcOpenFlowCookie, npw.ofportPhys))
		} else if config.Gateway.Mode == config.GatewayModeShared {
			// add the ICMP Fragmentation flow for shared gateway mode.
			icmpFlow := npw.generateICMPFragmentationFlow(nwDst, externalIPOrLBIngressIP, netConfig.ofPortPatch, cookie)
			externalIPFlows = append(externalIPFlows, icmpFlow)
			// case2 (see function description for details)
			externalIPFlows = append(externalIPFlows,
				// table=0, matches on service traffic towards externalIP or LB ingress and sends it to OVN pipeline
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, tp_dst=%d, "+
					"actions=%s",
					cookie, npw.ofportPhys, flowProtocol, nwDst, externalIPOrLBIngressIP, svcPort.Port, actions),
				// table=0, matches on return traffic from service externalIP or LB ingress and sends it out to primary node interface (br-ex)
				fmt.Sprintf("cookie=%s, priority=110, in_port=%s, dl_src=%s, %s, %s=%s, tp_src=%d, "+
					"actions=output:%s",
					cookie, netConfig.ofPortPatch, npw.ofm.getDefaultBridgeMAC(), flowProtocol, nwSrc, externalIPOrLBIngressIP, svcPort.Port, npw.ofportPhys))
		}
		npw.ofm.updateFlowCacheEntry(key, externalIPFlows)
	}

	return nil
}

// generate ARP/NS bypass flow which will send the ARP/NS request everywhere *but* to OVN
// OpenFlow will not do hairpin switching, so we can safely add the origin port to the list of ports, too
func (npw *nodePortWatcher) generateARPBypassFlow(ofPorts []string, ofPortPatch, ipAddr string, cookie string) string {
	addrResDst := "arp_tpa"
	addrResProto := "arp, arp_op=1"
	if utilnet.IsIPv6String(ipAddr) {
		addrResDst = "nd_target"
		addrResProto = "icmp6, icmp_type=135, icmp_code=0"
	}

	var arpFlow string
	var arpPortsFiltered []string
	if len(ofPorts) == 0 {
		// in the odd case that getting all ports from the bridge should not work,
		// simply output to LOCAL (this should work well in the vast majority of cases, anyway)
		arpFlow = fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, "+
			"actions=output:%s",
			cookie, npw.ofportPhys, addrResProto, addrResDst, ipAddr, ovsLocalPort)
	} else {
		// cover the case where breth0 has more than 3 ports, e.g. if an admin adds a 4th port
		// and the ExternalIP would be on that port
		// Use all ports except for ofPortPhys and the ofportPatch
		// Filtering ofPortPhys is for consistency / readability only, OpenFlow will not send
		// out the in_port normally (see man 7 ovs-actions)
		for _, port := range ofPorts {
			if port == ofPortPatch || port == npw.ofportPhys {
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

func (npw *nodePortWatcher) generateICMPFragmentationFlow(nwDst, ipAddr string, ofPortPatch, cookie string) string {
	// we send any ICMP destination unreachable, fragmentation needed to the OVN pipeline too so that
	// path MTU discovery continues to work.
	icmpMatch := "icmp"
	icmpType := 3
	icmpCode := 4
	if utilnet.IsIPv6String(ipAddr) {
		icmpMatch = "icmp6"
		icmpType = 2
		icmpCode = 0
	}
	icmpFragmentationFlow := fmt.Sprintf("cookie=%s, priority=110, in_port=%s, %s, %s=%s, icmp_type=%d, "+
		"icmp_code=%d, actions=output:%s",
		cookie, npw.ofportPhys, icmpMatch, nwDst, ipAddr, icmpType, icmpCode, ofPortPatch)
	return icmpFragmentationFlow
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
func (npw *nodePortWatcher) getAndSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool, localEndpoints sets.Set[string]) (old *serviceConfig, exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	old, exists = npw.serviceInfo[index]
	var ptrCopy serviceConfig
	if exists {
		ptrCopy = *old
	}
	npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
	return &ptrCopy, exists
}

// addOrSetServiceInfo creates and sets the serviceConfig if it doesn't exist
func (npw *nodePortWatcher) addOrSetServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp bool, localEndpoints sets.Set[string]) (exists bool) {
	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if _, exists := npw.serviceInfo[index]; !exists {
		// Only set this if it doesn't exist
		npw.serviceInfo[index] = &serviceConfig{service: service, hasLocalHostNetworkEp: hasLocalHostNetworkEp, localEndpoints: localEndpoints}
		return false
	}
	return true

}

// updateServiceInfo sets the serviceConfig for a service and returns the existing serviceConfig, if inputs are nil
// do not update those fields, if it does not exist return nil.
func (npw *nodePortWatcher) updateServiceInfo(index ktypes.NamespacedName, service *kapi.Service, hasLocalHostNetworkEp *bool, localEndpoints sets.Set[string]) (old *serviceConfig, exists bool) {

	npw.serviceInfoLock.Lock()
	defer npw.serviceInfoLock.Unlock()

	if old, exists = npw.serviceInfo[index]; !exists {
		klog.V(5).Infof("No serviceConfig found for service %s in namespace %s", index.Name, index.Namespace)
		return nil, exists
	}
	ptrCopy := *old
	if service != nil {
		npw.serviceInfo[index].service = service
	}

	if hasLocalHostNetworkEp != nil {
		npw.serviceInfo[index].hasLocalHostNetworkEp = *hasLocalHostNetworkEp
	}

	if localEndpoints != nil {
		npw.serviceInfo[index].localEndpoints = localEndpoints
	}

	return &ptrCopy, exists
}

// addServiceRules ensures the correct iptables rules and OpenFlow physical
// flows are programmed for a given service and endpoint configuration
func addServiceRules(service *kapi.Service, netInfo util.NetInfo, localEndpoints []string, svcHasLocalHostNetEndPnt bool, npw *nodePortWatcher) error {
	// For dpu or Full mode
	var err error
	var errors []error
	var activeNetwork *bridgeUDNConfiguration
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, netInfo, true, svcHasLocalHostNetEndPnt); err != nil {
			errors = append(errors, err)
		}
		npw.ofm.requestFlowSync()
		activeNetwork = npw.ofm.getActiveNetwork(netInfo)
		if activeNetwork == nil {
			return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
		}
	}

	if npw == nil || !npw.dpuMode {
		// add iptables/nftables rules only in full mode
		iptRules := getGatewayIPTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)
		if len(iptRules) > 0 {
			if err := insertIptRules(iptRules); err != nil {
				err = fmt.Errorf("failed to add iptables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
		nftElems := getGatewayNFTRules(service, localEndpoints, svcHasLocalHostNetEndPnt)
		if netInfo.IsPrimaryNetwork() && activeNetwork != nil {
			nftElems = append(nftElems, getUDNNFTRules(service, activeNetwork)...)
		}
		if len(nftElems) > 0 {
			if err := nodenft.UpdateNFTElements(nftElems); err != nil {
				err = fmt.Errorf("failed to update nftables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
	}

	return utilerrors.Join(errors...)
}

// delServiceRules deletes all possible iptables rules and OpenFlow physical
// flows for a service
func delServiceRules(service *kapi.Service, localEndpoints []string, npw *nodePortWatcher) error {
	var err error
	var errors []error
	// full mode || dpu mode
	if npw != nil {
		if err = npw.updateServiceFlowCache(service, nil, false, false); err != nil {
			errors = append(errors, fmt.Errorf("error updating service flow cache: %v", err))
		}
		npw.ofm.requestFlowSync()
	}

	if npw == nil || !npw.dpuMode {
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
		// |       false||true        |          cluster      |          cluster      |     towards clusterIP          |
		// |                          |                       |                       |       for the default case     |
		// |--------------------------|-----------------------|-----------------------|--------------------------------|
		// |                          |                       |                       |      deletes all the rules     |
		// |       false||true        |          local        |          local        |   for etp=local + itp=local    |
		// |                          |                       |                       |   + default dnat towards CIP   |
		// +--------------------------+-----------------------+-----------------------+--------------------------------+

		iptRules := getGatewayIPTRules(service, localEndpoints, true)
		iptRules = append(iptRules, getGatewayIPTRules(service, localEndpoints, false)...)
		if len(iptRules) > 0 {
			if err := nodeipt.DelRules(iptRules); err != nil {
				err := fmt.Errorf("failed to delete iptables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}
		nftElems := getGatewayNFTRules(service, localEndpoints, true)
		nftElems = append(nftElems, getGatewayNFTRules(service, localEndpoints, false)...)
		if len(nftElems) > 0 {
			if err := nodenft.DeleteNFTElements(nftElems); err != nil {
				err = fmt.Errorf("failed to delete nftables rules for service %s/%s: %v",
					service.Namespace, service.Name, err)
				errors = append(errors, err)
			}
		}

		if util.IsNetworkSegmentationSupportEnabled() {
			// NOTE: The code below is not using nodenft.DeleteNFTElements because it first adds elements
			// before removing them, which fails for UDN NFT rules. These rules only have map keys,
			// not key-value pairs, making it impossible to add.
			// Attempt to delete the elements directly and handle the IsNotFound error.
			//
			// TODO: Switch to `nft destroy` when supported.
			nftElems = getUDNNFTRules(service, nil)
			if len(nftElems) > 0 {
				nft, err := nodenft.GetNFTablesHelper()
				if err != nil {
					return utilerrors.Join(append(errors, err)...)
				}

				tx := nft.NewTransaction()
				for _, elem := range nftElems {
					tx.Delete(elem)
				}

				if err := nft.Run(context.TODO(), tx); err != nil && !knftables.IsNotFound(err) {
					err = fmt.Errorf("failed to delete nftables rules for UDN service %s/%s: %v",
						service.Namespace, service.Name, err)
					errors = append(errors, err)
				}
			}
		}
	}

	return utilerrors.Join(errors...)
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
		(new.Spec.AllocateLoadBalancerNodePorts != nil && old.Spec.AllocateLoadBalancerNodePorts != nil &&
			reflect.DeepEqual(*new.Spec.AllocateLoadBalancerNodePorts, *old.Spec.AllocateLoadBalancerNodePorts))
}

// AddService handles configuring shared gateway bridge flows to steer External IP, Node Port, Ingress LB traffic into OVN
func (npw *nodePortWatcher) AddService(service *kapi.Service) error {
	var localEndpoints sets.Set[string]
	var hasLocalHostNetworkEp bool
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Adding service %s in namespace %s", service.Name, service.Namespace)

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(service.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for service %s in namespace %s: %w", service.Name, service.Namespace, err)
	}

	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(service.Namespace, service.Name, netInfo.GetNetworkName())
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during service add: %w",
				service.Namespace, service.Name, err)
		}
		klog.V(5).Infof("No endpointslice found for service %s in namespace %s during service Add",
			service.Name, service.Namespace)
		// No endpoint object exists yet so default to false
		hasLocalHostNetworkEp = false
	} else {
		nodeIPs := npw.nodeIPManager.ListAddresses()
		localEndpoints = npw.GetLocalEligibleEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp = util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
	}
	// If something didn't already do it add correct Service rules
	if exists := npw.addOrSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints); !exists {
		klog.V(5).Infof("Service Add %s event in namespace %s came before endpoint event setting svcConfig",
			service.Name, service.Namespace)
		if err := addServiceRules(service, netInfo, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			npw.getAndDeleteServiceInfo(name)
			return fmt.Errorf("AddService failed for nodePortWatcher: %w, trying delete: %v", err, delServiceRules(service, sets.List(localEndpoints), npw))
		}
	} else {
		// Need to update flows here in case an attribute of the gateway has changed, such as MAC address
		klog.V(5).Infof("Updating already programmed rules for %s in namespace %s", service.Name, service.Namespace)
		if err = npw.updateServiceFlowCache(service, netInfo, true, hasLocalHostNetworkEp); err != nil {
			return fmt.Errorf("failed to update flows for service %s/%s: %w", service.Namespace, service.Name, err)
		}
		npw.ofm.requestFlowSync()
	}
	return nil
}

func (npw *nodePortWatcher) UpdateService(old, new *kapi.Service) error {
	var err error
	var errors []error
	name := ktypes.NamespacedName{Namespace: old.Namespace, Name: old.Name}

	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to any of .Spec.Ports, "+
			".Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs, .Spec.Type, .Status.LoadBalancer.Ingress, "+
			".Spec.ExternalTrafficPolicy, .Spec.InternalTrafficPolicy", new.Name)
		return nil
	}
	// Update the service in svcConfig if we need to so that other handler
	// threads do the correct thing, leave hasLocalHostNetworkEp and localEndpoints alone in the cache
	svcConfig, exists := npw.updateServiceInfo(name, new, nil, nil)
	if !exists {
		klog.V(5).Infof("Service %s in namespace %s was deleted during service Update", old.Name, old.Namespace)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		// Delete old rules if needed, but don't delete svcConfig
		// so that we don't miss any endpoint update events here
		klog.V(5).Infof("Deleting old service rules for: %v", old)

		if err = delServiceRules(old, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		klog.V(5).Infof("Adding new service rules for: %v", new)

		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(new.Namespace)
		if err != nil {
			return fmt.Errorf("error getting active network for service %s in namespace %s: %w", new.Name, new.Namespace, err)
		}

		if err = addServiceRules(new, netInfo, sets.List(svcConfig.localEndpoints), svcConfig.hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
	}
	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcher: %v", err)
	}
	return nil

}

// deleteConntrackForServiceVIP deletes the conntrack entries for the provided svcVIP:svcPort by comparing them to ConntrackOrigDstIP:ConntrackOrigDstPort
func deleteConntrackForServiceVIP(svcVIPs []string, svcPorts []kapi.ServicePort, ns, name string) error {
	for _, svcVIP := range svcVIPs {
		for _, svcPort := range svcPorts {
			if err := util.DeleteConntrackServicePort(svcVIP, svcPort.Port, svcPort.Protocol,
				netlink.ConntrackOrigDstIP, nil); err != nil {
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
				if err := util.DeleteConntrackServicePort(nodeIP.String(), svcPort.NodePort, svcPort.Protocol,
					netlink.ConntrackOrigDstIP, nil); err != nil {
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

func (npw *nodePortWatcher) DeleteService(service *kapi.Service) error {
	var err error
	var errors []error
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	klog.V(5).Infof("Deleting service %s in namespace %s", service.Name, service.Namespace)
	name := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}
	if svcConfig, exists := npw.getAndDeleteServiceInfo(name); exists {
		if err = delServiceRules(svcConfig.service, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
	} else {
		klog.Warningf("Delete service: no service found in cache for endpoint %s in namespace %s", service.Name, service.Namespace)
	}
	// Remove all conntrack entries for the serviceVIPs of this service irrespective of protocol stack
	// since service deletion is considered as unplugging the network cable and hence graceful termination
	// is not guaranteed. See https://github.com/kubernetes/kubernetes/issues/108523#issuecomment-1074044415.
	if err = npw.deleteConntrackForService(service); err != nil {
		errors = append(errors, fmt.Errorf("failed to delete conntrack entry for service %v: %v", name, err))
	}

	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcher: %v", err)
	}
	return nil

}

func (npw *nodePortWatcher) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	var keepIPTRules []nodeipt.Rule
	var keepNFTSetElems, keepNFTMapElems []*knftables.Element
	for _, serviceInterface := range services {
		name := ktypes.NamespacedName{Namespace: serviceInterface.(*kapi.Service).Namespace, Name: serviceInterface.(*kapi.Service).Name}

		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// don't process headless service
		if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
			continue
		}

		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(service.Namespace)
		if err != nil {
			errors = append(errors, err)
			continue
		}

		epSlices, err := npw.watchFactory.GetServiceEndpointSlices(service.Namespace, service.Name, netInfo.GetNetworkName())
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("error retrieving all endpointslices for service %s/%s during SyncServices: %w",
					service.Namespace, service.Name, err)
			}
			klog.V(5).Infof("No endpointslice found for service %s in namespace %s during sync", service.Name, service.Namespace)
			continue
		}
		nodeIPs := npw.nodeIPManager.ListAddresses()
		localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, service)
		hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)
		npw.getAndSetServiceInfo(name, service, hasLocalHostNetworkEp, localEndpoints)

		// Delete OF rules for service if they exist
		if err = npw.updateServiceFlowCache(service, netInfo, false, hasLocalHostNetworkEp); err != nil {
			errors = append(errors, err)
		}
		if err = npw.updateServiceFlowCache(service, netInfo, true, hasLocalHostNetworkEp); err != nil {
			errors = append(errors, err)
		}
		// Add correct netfilter rules only for Full mode
		if !npw.dpuMode {
			localEndpointsArray := sets.List(localEndpoints)
			keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, localEndpointsArray, hasLocalHostNetworkEp)...)
			keepNFTSetElems = append(keepNFTSetElems, getGatewayNFTRules(service, localEndpointsArray, hasLocalHostNetworkEp)...)
			if util.IsNetworkSegmentationSupportEnabled() && netInfo.IsPrimaryNetwork() {
				netConfig := npw.ofm.getActiveNetwork(netInfo)
				if netConfig == nil {
					return fmt.Errorf("failed to get active network config for network %s", netInfo.GetNetworkName())
				}
				keepNFTMapElems = append(keepNFTMapElems, getUDNNFTRules(service, netConfig)...)
			}
		}
	}

	// sync OF rules once
	npw.ofm.requestFlowSync()
	// sync netfilter rules once only for Full mode
	if !npw.dpuMode {
		// (NOTE: Order is important, add jump to iptableETPChain before jump to NP/EIP chains)
		for _, chain := range []string{iptableITPChain, egressservice.Chain, iptableNodePortChain, iptableExternalIPChain, iptableETPChain} {
			if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
				errors = append(errors, err)
			}
		}
		if err = recreateIPTRules("mangle", iptableITPChain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}

		for _, set := range []string{nftablesMgmtPortNoSNATNodePorts, nftablesMgmtPortNoSNATServicesV4, nftablesMgmtPortNoSNATServicesV6} {
			if err = recreateNFTSet(set, keepNFTSetElems); err != nil {
				errors = append(errors, err)
			}
		}
		if util.IsNetworkSegmentationSupportEnabled() {
			for _, nftMap := range []string{nftablesUDNMarkNodePortsMap, nftablesUDNMarkExternalIPsV4Map, nftablesUDNMarkExternalIPsV6Map} {
				if err = recreateNFTMap(nftMap, keepNFTMapElems); err != nil {
					errors = append(errors, err)
				}
			}
		}
	}
	return utilerrors.Join(errors...)
}

func (npw *nodePortWatcher) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var svc *kapi.Service

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(epSlice.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for endpointslice %s in namespace %s: %w", epSlice.Name, epSlice.Namespace, err)
	}

	if util.IsNetworkSegmentationSupportEnabled() && !util.IsEndpointSliceForNetwork(epSlice, netInfo) {
		return nil
	}

	svcNamespacedName, err := util.ServiceFromEndpointSlice(epSlice, netInfo)
	if err != nil || svcNamespacedName == nil {
		return err
	}

	svc, err = npw.watchFactory.GetService(svcNamespacedName.Namespace, svcNamespacedName.Name)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving service %s/%s during endpointslice add: %w",
				svcNamespacedName.Namespace, svcNamespacedName.Name, err)
		}
		// This is not necessarily an error. For e.g when there are endpoints
		// without a corresponding service.
		klog.V(5).Infof("No service found for endpointslice %s in namespace %s during endpointslice add",
			epSlice.Name, epSlice.Namespace)
		return nil
	}

	if !util.ServiceTypeHasClusterIP(svc) || !util.IsClusterIPSet(svc) {
		return nil
	}

	klog.V(5).Infof("Adding endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	nodeIPs := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(svc.Namespace, svc.Name, netInfo.GetNetworkName())
	if err != nil {
		// No need to continue adding the new endpoint slice, if we can't retrieve all slices for this service
		return fmt.Errorf("error retrieving endpointslices for service %s/%s during endpointslice add: %w", svc.Namespace, svc.Name, err)
	}
	localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEp := util.HasLocalHostNetworkEndpoints(localEndpoints, nodeIPs)

	// Here we make sure the correct rules are programmed whenever an AddEndpointSlice event is
	// received, only alter flows if we need to, i.e if cache wasn't set or if it was and
	// hasLocalHostNetworkEp or localEndpoints state (for LB svc where NPs=0) changed, to prevent flow churn
	out, exists := npw.getServiceInfo(*svcNamespacedName)
	if !exists {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is creating rules", epSlice.Name, epSlice.Namespace)
		if err = addServiceRules(svc, netInfo, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			return err
		}
		npw.addOrSetServiceInfo(*svcNamespacedName, svc, hasLocalHostNetworkEp, localEndpoints)
		return nil
	}

	if out.hasLocalHostNetworkEp != hasLocalHostNetworkEp ||
		(!util.LoadBalancerServiceHasNodePortAllocation(svc) && !reflect.DeepEqual(out.localEndpoints, localEndpoints)) {
		klog.V(5).Infof("Endpointslice %s ADD event in namespace %s is updating rules", epSlice.Name, epSlice.Namespace)
		if err = delServiceRules(svc, sets.List(out.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
		if err = addServiceRules(svc, netInfo, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		} else {
			npw.updateServiceInfo(*svcNamespacedName, svc, &hasLocalHostNetworkEp, localEndpoints)
		}
		return utilerrors.Join(errors...)
	}
	return nil

}

func (npw *nodePortWatcher) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error
	var hasLocalHostNetworkEp = false

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(epSlice.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for endpointslice %s in namespace %s: %w", epSlice.Name, epSlice.Namespace, err)
	}
	if util.IsNetworkSegmentationSupportEnabled() && !util.IsEndpointSliceForNetwork(epSlice, netInfo) {
		return nil
	}

	klog.V(5).Infof("Deleting endpointslice %s in namespace %s", epSlice.Name, epSlice.Namespace)
	// remove rules for endpoints and add back normal ones
	namespacedName, err := util.ServiceFromEndpointSlice(epSlice, netInfo)
	if err != nil || namespacedName == nil {
		return err
	}
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(namespacedName.Namespace, namespacedName.Name, netInfo.GetNetworkName())
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice delete on %s: %w",
				namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
		}
		// an endpoint slice that we retry to delete will be gone from the api server, so don't return here
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice delete on %s (did we previously fail to delete it?)",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name)
		epSlices = []*discovery.EndpointSlice{epSlice}
	}

	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice delete: %v",
			namespacedName.Namespace, namespacedName.Name, epSlice.Name, err)
	}
	localEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	if svcConfig, exists := npw.updateServiceInfo(*namespacedName, nil, &hasLocalHostNetworkEp, localEndpoints); exists {
		netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(namespacedName.Namespace)
		if err != nil {
			return fmt.Errorf("error getting active network for service %s/%s: %w", namespacedName.Namespace, namespacedName.Name, err)
		}

		// Lock the cache mutex here so we don't miss a service delete during an endpoint delete
		// we have to do this because deleting and adding iptables rules is slow.
		npw.serviceInfoLock.Lock()
		defer npw.serviceInfoLock.Unlock()

		if err = delServiceRules(svcConfig.service, sets.List(svcConfig.localEndpoints), npw); err != nil {
			errors = append(errors, err)
		}
		if err = addServiceRules(svcConfig.service, netInfo, sets.List(localEndpoints), hasLocalHostNetworkEp, npw); err != nil {
			errors = append(errors, err)
		}
		return utilerrors.Join(errors...)
	}
	return nil
}

// GetLocalEndpointAddresses returns a list of eligible endpoints that are local to the node
func (npw *nodePortWatcher) GetLocalEligibleEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) sets.Set[string] {
	return util.GetLocalEligibleEndpointAddressesFromSlices(endpointSlices, service, npw.nodeIPManager.nodeName)
}

func (npw *nodePortWatcher) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	// TODO (tssurya): refactor bits in this function to ensure add and delete endpoint slices are not called repeatedly
	// Context: Both add and delete endpointslice are calling delServiceRules followed by addServiceRules which makes double
	// the number of calls than needed for an update endpoint slice
	var err error
	var errors []error

	netInfo, err := npw.networkManager.GetActiveNetworkForNamespace(newEpSlice.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for endpointslice %s in namespace %s: %w", newEpSlice.Name, newEpSlice.Namespace, err)
	}

	if util.IsNetworkSegmentationSupportEnabled() && !util.IsEndpointSliceForNetwork(newEpSlice, netInfo) {
		return nil
	}

	namespacedName, err := util.ServiceFromEndpointSlice(newEpSlice, netInfo)
	if err != nil || namespacedName == nil {
		return err
	}
	svc, err := npw.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("error retrieving service %s/%s for endpointslice %s during endpointslice update: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	oldEndpointAddresses := util.GetEligibleEndpointAddressesFromSlices([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newEndpointAddresses := util.GetEligibleEndpointAddressesFromSlices([]*discovery.EndpointSlice{newEpSlice}, svc)
	if reflect.DeepEqual(oldEndpointAddresses, newEndpointAddresses) {
		return nil
	}

	klog.V(5).Infof("Updating endpointslice %s in namespace %s", oldEpSlice.Name, oldEpSlice.Namespace)

	var serviceInfo *serviceConfig
	var exists bool
	if serviceInfo, exists = npw.getServiceInfo(*namespacedName); !exists {
		// When a service is updated from externalName to nodeport type, it won't be
		// in nodePortWatcher cache (npw): in this case, have the new nodeport IPtable rules
		// installed.
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
	} else if len(newEndpointAddresses) == 0 {
		// With no endpoint addresses in new endpointslice, delete old endpoint rules
		// and add normal ones back
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
	}

	// Update rules and service cache if hasHostNetworkEndpoints status changed or localEndpoints changed
	nodeIPs := npw.nodeIPManager.ListAddresses()
	epSlices, err := npw.watchFactory.GetServiceEndpointSlices(newEpSlice.Namespace, namespacedName.Name, netInfo.GetNetworkName())
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving all endpointslices for service %s/%s during endpointslice update on %s: %w",
				namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
		}
		klog.V(5).Infof("No endpointslices found for service %s/%s during endpointslice update on %s: %v",
			namespacedName.Namespace, namespacedName.Name, newEpSlice.Name, err)
	}

	// Delete old endpoint slice and add new one when local endpoints have changed or the presence of local host-network
	// endpoints has changed. For this second comparison, check first between the old endpoint slice and all current
	// endpointslices for this service. This is a partial comparison, in case serviceInfo is not set. When it is set, compare
	// between /all/ old endpoint slices and all new ones.
	oldLocalEndpoints := npw.GetLocalEligibleEndpointAddresses([]*discovery.EndpointSlice{oldEpSlice}, svc)
	newLocalEndpoints := npw.GetLocalEligibleEndpointAddresses(epSlices, svc)
	hasLocalHostNetworkEpOld := util.HasLocalHostNetworkEndpoints(oldLocalEndpoints, nodeIPs)
	hasLocalHostNetworkEpNew := util.HasLocalHostNetworkEndpoints(newLocalEndpoints, nodeIPs)

	localEndpointsHaveChanged := serviceInfo != nil && !reflect.DeepEqual(serviceInfo.localEndpoints, newLocalEndpoints)
	localHostNetworkEndpointsPresenceHasChanged := hasLocalHostNetworkEpOld != hasLocalHostNetworkEpNew ||
		serviceInfo != nil && serviceInfo.hasLocalHostNetworkEp != hasLocalHostNetworkEpNew

	if localEndpointsHaveChanged || localHostNetworkEndpointsPresenceHasChanged {
		if err = npw.DeleteEndpointSlice(oldEpSlice); err != nil {
			errors = append(errors, err)
		}
		if err = npw.AddEndpointSlice(newEpSlice); err != nil {
			errors = append(errors, err)
		}
		return utilerrors.Join(errors...)
	}

	return utilerrors.Join(errors...)
}

func (npwipt *nodePortWatcherIptables) AddService(service *kapi.Service) error {
	// don't process headless service or services that doesn't have NodePorts or ExternalIPs
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	netInfo, err := npwipt.networkManager.GetActiveNetworkForNamespace(service.Namespace)
	if err != nil {
		return fmt.Errorf("error getting active network for service %s in namespace %s: %w", service.Name, service.Namespace, err)
	}

	if err := addServiceRules(service, netInfo, nil, false, nil); err != nil {
		return fmt.Errorf("AddService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) UpdateService(old, new *kapi.Service) error {
	var err error
	var errors []error
	if serviceUpdateNotNeeded(old, new) {
		klog.V(5).Infof("Skipping service update for: %s as change does not apply to "+
			"any of .Spec.Ports, .Spec.ExternalIP, .Spec.ClusterIP, .Spec.ClusterIPs,"+
			" .Spec.Type, .Status.LoadBalancer.Ingress", new.Name)
		return nil
	}

	if util.ServiceTypeHasClusterIP(old) && util.IsClusterIPSet(old) {
		if err = delServiceRules(old, nil, nil); err != nil {
			errors = append(errors, err)
		}
	}

	if util.ServiceTypeHasClusterIP(new) && util.IsClusterIPSet(new) {
		netInfo, err := npwipt.networkManager.GetActiveNetworkForNamespace(new.Namespace)
		if err != nil {
			return fmt.Errorf("error getting active network for service %s in namespace %s: %w", new.Name, new.Namespace, err)
		}

		if err = addServiceRules(new, netInfo, nil, false, nil); err != nil {
			errors = append(errors, err)
		}
	}
	if err = utilerrors.Join(errors...); err != nil {
		return fmt.Errorf("UpdateService failed for nodePortWatcherIptables: %v", err)
	}
	return nil

}

func (npwipt *nodePortWatcherIptables) DeleteService(service *kapi.Service) error {
	// don't process headless service
	if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
		return nil
	}

	if err := delServiceRules(service, nil, nil); err != nil {
		return fmt.Errorf("DeleteService failed for nodePortWatcherIptables: %v", err)
	}
	return nil
}

func (npwipt *nodePortWatcherIptables) SyncServices(services []interface{}) error {
	var err error
	var errors []error
	keepIPTRules := []nodeipt.Rule{}
	keepNFTElems := []*knftables.Element{}
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(*kapi.Service)
		if !ok {
			klog.Errorf("Spurious object in syncServices: %v",
				serviceInterface)
			continue
		}
		// don't process headless service
		if !util.ServiceTypeHasClusterIP(service) || !util.IsClusterIPSet(service) {
			continue
		}
		// Add correct iptables rules.
		// TODO: ETP and ITP is not implemented for smart NIC mode.
		keepIPTRules = append(keepIPTRules, getGatewayIPTRules(service, nil, false)...)
		keepNFTElems = append(keepNFTElems, getGatewayNFTRules(service, nil, false)...)
	}

	// sync rules once
	for _, chain := range []string{iptableNodePortChain, iptableExternalIPChain} {
		if err = recreateIPTRules("nat", chain, keepIPTRules); err != nil {
			errors = append(errors, err)
		}
	}

	for _, set := range []string{nftablesMgmtPortNoSNATNodePorts, nftablesMgmtPortNoSNATServicesV4, nftablesMgmtPortNoSNATServicesV6} {
		if err = recreateNFTSet(set, keepNFTElems); err != nil {
			errors = append(errors, err)
		}
	}

	return utilerrors.Join(errors...)
}

func flowsForDefaultBridge(bridge *bridgeConfiguration, extraIPs []net.IP) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortHost := bridge.ofPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string
	// 14 bytes of overhead for ethernet header (does not include VLAN)
	maxPktLength := getMaxFrameLength()

	if config.IPv4Mode {
		// table0, Geneve packets coming from external. Skip conntrack and go directly to host
		// if dest mac is the shared mac send directly to host.
		if ofPortPhys != "" {
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
		}
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		for _, netConfig := range bridge.patchedNetConfigs() {
			// table 0, SVC Hairpin from OVN destined to local host, DNAT and go to table 4
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					defaultOpenFlowCookie, netConfig.ofPortPatch, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String(), physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() == nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V4HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range bridge.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s, ip_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, ip.String(), physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ip, ip_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	}
	if config.IPv6Mode {
		if ofPortPhys != "" {
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
		}

		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		// table 0, SVC Hairpin from OVN destined to local host, DNAT to host, send to table 4
		for _, netConfig := range bridge.patchedNetConfigs() {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
					"actions=ct(commit,zone=%d,nat(dst=%s),table=4)",
					defaultOpenFlowCookie, netConfig.ofPortPatch, config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String(), physicalIP.IP,
					config.Default.HostMasqConntrackZone, physicalIP.IP))
		}

		// table 0, hairpin from OVN destined to local host (but an additional node IP), send to table 4
		for _, ip := range extraIPs {
			if ip.To4() != nil {
				continue
			}
			// not needed for the physical IP
			if ip.Equal(physicalIP.IP) {
				continue
			}

			// not needed for special masquerade IP
			if ip.Equal(config.Gateway.MasqueradeIPs.V6HostMasqueradeIP) {
				continue
			}

			for _, netConfig := range bridge.patchedNetConfigs() {
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s, ipv6_src=%s,"+
						"actions=ct(commit,zone=%d,table=4)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, ip.String(), physicalIP.IP,
						config.Default.HostMasqConntrackZone))
			}
		}

		// table 0, Reply SVC traffic from Host -> OVN, unSNAT and goto table 5
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, ipv6, ipv6_dst=%s,"+
				"actions=ct(zone=%d,nat,table=5)",
				defaultOpenFlowCookie, ofPortHost, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Default.OVNMasqConntrackZone))
	}

	var protoPrefix, masqIP, masqSubnet string

	// table 0, packets coming from Host -> Service
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if utilnet.IsIPv4CIDR(svcCIDR) {
			protoPrefix = "ip"
			masqIP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V4MasqueradeSubnet
		} else {
			protoPrefix = "ipv6"
			masqIP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP.String()
			masqSubnet = config.Gateway.V6MasqueradeSubnet
		}

		// table 0, Host (default network) -> OVN towards SVC, SNAT to special IP.
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_dst=%s, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=2)",
				defaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
				svcCIDR, config.Default.HostMasqConntrackZone, masqIP))

		if util.IsNetworkSegmentationSupportEnabled() {
			// table 0, Host (UDNs) -> OVN towards SVC, SNAT to special IP.
			// For packets originating from UDN, commit without NATing, those
			// have already been SNATed to the masq IP of the UDN.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=550, in_port=%s, %s, %s_src=%s, %s_dst=%s, "+
					"actions=ct(commit,zone=%d,table=2)",
					defaultOpenFlowCookie, ofPortHost, protoPrefix, protoPrefix,
					masqSubnet, protoPrefix, svcCIDR, config.Default.HostMasqConntrackZone))
		}

		masqDst := masqIP
		if util.IsNetworkSegmentationSupportEnabled() {
			// In UDN match on the whole masquerade subnet to handle replies from UDN enabled services
			masqDst = masqSubnet
		}
		for _, netConfig := range bridge.patchedNetConfigs() {
			// table 0, Reply hairpin traffic to host, coming from OVN, unSNAT
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=500, in_port=%s, %s, %s_src=%s, %s_dst=%s,"+
					"actions=ct(zone=%d,nat,table=3)",
					defaultOpenFlowCookie, netConfig.ofPortPatch, protoPrefix, protoPrefix, svcCIDR,
					protoPrefix, masqDst, config.Default.HostMasqConntrackZone))
			// table 0, Reply traffic coming from OVN to outside, drop it if the DNAT wasn't done either
			// at the GR load balancer or switch load balancer. It means the correct port wasn't provided.
			// nodeCIDR->serviceCIDR traffic flow is internal and it shouldn't be carried to outside the cluster
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=105, in_port=%s, %s, %s_dst=%s,"+
					"actions=drop", defaultOpenFlowCookie, netConfig.ofPortPatch, protoPrefix, protoPrefix, svcCIDR))
		}
	}

	// table 0, add IP fragment reassembly flows, only needed in SGW mode with
	// physical interface attached to bridge
	if config.Gateway.Mode == config.GatewayModeShared && ofPortPhys != "" {
		reassemblyFlows := generateIPFragmentReassemblyFlow(ofPortPhys)
		dftFlows = append(dftFlows, reassemblyFlows...)
	}
	if ofPortPhys != "" {
		for _, netConfig := range bridge.patchedNetConfigs() {
			var actions string
			if config.Gateway.Mode != config.GatewayModeLocal || config.Gateway.DisablePacketMTUCheck {
				actions = fmt.Sprintf("output:%s", netConfig.ofPortPatch)
			} else {
				// packets larger than known acceptable MTU need to go to kernel for
				// potential fragmentation
				// introduced specifically for replies to egress traffic not routed
				// through the host
				actions = fmt.Sprintf("check_pkt_larger(%d)->reg0[0],resubmit(,11)", maxPktLength)
			}

			if config.IPv4Mode {
				// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", defaultOpenFlowCookie, netConfig.masqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ip, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", defaultOpenFlowCookie, netConfig.masqCTMark, actions))

			}

			if config.IPv6Mode {
				// table 1, established and related connections in zone 64000 with ct_mark ctMarkOVN go to OVN
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+est, ct_mark=%s, "+
						"actions=%s", defaultOpenFlowCookie, netConfig.masqCTMark, actions))

				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=100, table=1, ipv6, ct_state=+trk+rel, ct_mark=%s, "+
						"actions=%s", defaultOpenFlowCookie, netConfig.masqCTMark, actions))
			}
		}
		if config.IPv4Mode {
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
	}

	defaultNetConfig := bridge.netConfig[types.DefaultNetworkName]

	// table 2, dispatch from Host -> OVN
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=100, table=2, "+
			"actions=set_field:%s->eth_dst,output:%s", defaultOpenFlowCookie,
			bridgeMacAddress, defaultNetConfig.ofPortPatch))

	// table 2, priority 200, dispatch from UDN -> Host -> OVN. These packets have
	// already been SNATed to the UDN's masq IP or have been marked with the UDN's packet mark.
	if config.IPv4Mode {
		for _, netConfig := range bridge.patchedNetConfigs() {
			if netConfig.isDefaultNetwork() {
				continue
			}
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip, ip_src=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					defaultOpenFlowCookie, netConfig.v4MasqIPs.ManagementPort.IP,
					bridgeMacAddress, netConfig.ofPortPatch))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					defaultOpenFlowCookie, netConfig.pktMark,
					bridgeMacAddress, netConfig.ofPortPatch))
		}
	}

	if config.IPv6Mode {
		for _, netConfig := range bridge.patchedNetConfigs() {
			if netConfig.isDefaultNetwork() {
				continue
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip6, ipv6_src=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					defaultOpenFlowCookie, netConfig.v6MasqIPs.ManagementPort.IP,
					bridgeMacAddress, netConfig.ofPortPatch))
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=200, table=2, ip6, pkt_mark=%s, "+
					"actions=set_field:%s->eth_dst,output:%s",
					defaultOpenFlowCookie, netConfig.pktMark,
					bridgeMacAddress, netConfig.ofPortPatch))
		}
	}

	// table 3, dispatch from OVN -> Host
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, table=3, "+
			"actions=move:NXM_OF_ETH_DST[]->NXM_OF_ETH_SRC[],set_field:%s->eth_dst,output:%s",
			defaultOpenFlowCookie, bridgeMacAddress, ofPortHost))

	// table 4, hairpinned pkts that need to go from OVN -> Host
	// We need to SNAT and masquerade OVN GR IP, send to table 3 for dispatch to Host
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ip,"+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=4,ipv6, "+
				"actions=ct(commit,zone=%d,nat(src=%s),table=3)",
				defaultOpenFlowCookie, config.Default.OVNMasqConntrackZone, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()))
	}
	// table 5, Host Reply traffic to hairpinned svc, need to unDNAT, send to table 2
	if config.IPv4Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ip, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, config.Default.HostMasqConntrackZone))
	}
	if config.IPv6Mode {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, table=5, ipv6, "+
				"actions=ct(commit,zone=%d,nat,table=2)",
				defaultOpenFlowCookie, config.Default.HostMasqConntrackZone))
	}
	return dftFlows, nil
}

func commonFlows(subnets []*net.IPNet, bridge *bridgeConfiguration, isPodNetworkAdvertised bool) ([]string, error) {
	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!
	ofPortPhys := bridge.ofPortPhys
	bridgeMacAddress := bridge.macAddress.String()
	ofPortHost := bridge.ofPortHost
	bridgeIPs := bridge.ips

	var dftFlows []string

	if ofPortPhys != "" {
		// table 0, we check to see if this dest mac is the shared mac, if so flood to all ports
		actions := "output:" + ofPortHost
		for _, netConfig := range bridge.patchedNetConfigs() {
			actions += ",output:" + netConfig.ofPortPatch
		}
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_dst=%s, actions=%s",
				defaultOpenFlowCookie, ofPortPhys, bridgeMacAddress, actions))
	}

	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	for _, netConfig := range bridge.patchedNetConfigs() {
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
				defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress))
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
				defaultOpenFlowCookie, netConfig.ofPortPatch))
	}

	if config.IPv4Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(false, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv4 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be SNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// SNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ip, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, ovnKubeNodeSNATMark,
						config.Default.ConntrackZone, physicalIP.IP, netConfig.masqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect &&
					config.Gateway.Mode != config.GatewayModeDisabled && bridge.eipMarkIPs != nil {
					if netConfig.masqCTMark != ctMarkOVN {
						for mark, eip := range bridge.eipMarkIPs.GetIPv4() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ip, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, mark,
									config.Default.ConntrackZone, eip, netConfig.masqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.isDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ip, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, config.Default.ConntrackZone,
							netConfig.masqCTMark, ofPortPhys))
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ip, ip_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, netConfig.v4MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.masqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ip, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp, nw_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				// We send BFD traffic coming from OVN to outside directly using a higher priority flow
				if ofPortPhys != "" {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp, tp_dst=3784, actions=output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}

		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ip, "+
					"actions=ct(zone=%d, nat, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}

	if config.IPv6Mode {
		physicalIP, err := util.MatchFirstIPNetFamily(true, bridgeIPs)
		if err != nil {
			return nil, fmt.Errorf("unable to determine IPv6 physical IP of host: %v", err)
		}
		if ofPortPhys != "" {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table0, packets coming from egressIP pods that have mark 1008 on them
				// will be DNAT-ed a final time into nodeIP to maintain consistency in traffic even if the GR
				// DNATs these into egressIP prior to reaching external bridge.
				// egressService pods will also undergo this SNAT to nodeIP since these features are tied
				// together at the OVN policy level on the distributed router.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ipv6, pkt_mark=%s "+
						"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)),output:%s",
						defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, ovnKubeNodeSNATMark,
						config.Default.ConntrackZone, physicalIP.IP, netConfig.masqCTMark, ofPortPhys))

				// table 0, packets coming from egressIP pods only from user defined networks. If an egressIP is assigned to
				// this node, then all networks get a flow even if no pods on that network were selected for by this egressIP.
				if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect &&
					config.Gateway.Mode != config.GatewayModeDisabled && bridge.eipMarkIPs != nil {
					if netConfig.masqCTMark != ctMarkOVN {
						for mark, eip := range bridge.eipMarkIPs.GetIPv6() {
							dftFlows = append(dftFlows,
								fmt.Sprintf("cookie=%s, priority=105, in_port=%s, dl_src=%s, ipv6, pkt_mark=%d, "+
									"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
									defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, mark,
									config.Default.ConntrackZone, eip, netConfig.masqCTMark, ofPortPhys))
						}
					}
				}

				// table 0, packets coming from pods headed externally. Commit connections with ct_mark ctMarkOVN
				// so that reverse direction goes back to the pods.
				if netConfig.isDefaultNetwork() {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ipv6, "+
							"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, config.Default.ConntrackZone, netConfig.masqCTMark, ofPortPhys))
				} else {
					//  for UDN we additionally SNAT the packet from masquerade IP -> node IP
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=100, in_port=%s, dl_src=%s, ipv6, ipv6_src=%s, "+
							"actions=ct(commit, zone=%d, nat(src=%s), exec(set_field:%s->ct_mark)), output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, netConfig.v6MasqIPs.GatewayRouter.IP, config.Default.ConntrackZone,
							physicalIP.IP, netConfig.masqCTMark, ofPortPhys))
				}
			}

			// table 0, packets coming from host. Commit connections with ct_mark ctMarkHost
			// so that reverse direction goes back to the host.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=100, in_port=%s, ipv6, "+
					"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
					defaultOpenFlowCookie, ofPortHost, config.Default.ConntrackZone, ctMarkHost, ofPortPhys))
		}
		if config.Gateway.Mode == config.GatewayModeLocal {
			for _, netConfig := range bridge.patchedNetConfigs() {
				// table 0, any packet coming from OVN send to host in LGW mode, host will take care of sending it outside if needed.
				// exceptions are traffic for egressIP and egressGW features and ICMP related traffic which will hit the priority 100 flow instead of this.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, tcp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, udp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=175, in_port=%s, sctp6, ipv6_src=%s, "+
						"actions=ct(table=4,zone=%d)",
						defaultOpenFlowCookie, netConfig.ofPortPatch, physicalIP.IP, config.Default.HostMasqConntrackZone))
				if ofPortPhys != "" {
					// We send BFD traffic coming from OVN to outside directly using a higher priority flow
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=650, table=0, in_port=%s, dl_src=%s, udp6, tp_dst=3784, actions=output:%s",
							defaultOpenFlowCookie, netConfig.ofPortPatch, bridgeMacAddress, ofPortPhys))
				}
			}
		}
		if ofPortPhys != "" {
			// table 0, packets coming from external. Send it through conntrack and
			// resubmit to table 1 to know the state and mark of the connection.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=50, in_port=%s, ipv6, "+
					"actions=ct(zone=%d, nat, table=1)", defaultOpenFlowCookie, ofPortPhys, config.Default.ConntrackZone))
		}
	}
	// Egress IP is often configured on a node different from the one hosting the affected pod.
	// Due to the fact that ovn-controllers on different nodes apply the changes independently,
	// there is a chance that the pod traffic will reach the egress node before it configures the SNAT flows.
	// Drop pod traffic that is not SNATed, excluding local pods(required for ICNIv2)
	defaultNetConfig := bridge.netConfig[types.DefaultNetworkName]
	if config.OVNKubernetesFeature.EnableEgressIP {
		for _, clusterEntry := range config.Default.ClusterSubnets {
			cidr := clusterEntry.CIDR
			ipv := getIPv(cidr)
			// table 0, drop packets coming from pods headed externally that were not SNATed.
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=104, in_port=%s, %s, %s_src=%s, actions=drop",
					defaultOpenFlowCookie, defaultNetConfig.ofPortPatch, ipv, ipv, cidr))
		}
		for _, subnet := range subnets {
			ipv := getIPv(subnet)
			if ofPortPhys != "" {
				// table 0, commit connections from local pods.
				// ICNIv2 requires that local pod traffic can leave the node without SNAT.
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=109, in_port=%s, dl_src=%s, %s, %s_src=%s"+
						"actions=ct(commit, zone=%d, exec(set_field:%s->ct_mark)), output:%s",
						defaultOpenFlowCookie, defaultNetConfig.ofPortPatch, bridgeMacAddress, ipv, ipv, subnet,
						config.Default.ConntrackZone, ctMarkOVN, ofPortPhys))
			}
		}
	}

	if ofPortPhys != "" {
		if config.Gateway.DisableSNATMultipleGWs || isPodNetworkAdvertised {
			// table 1, traffic to pod subnet go directly to OVN
			output := defaultNetConfig.ofPortPatch
			if isPodNetworkAdvertised && config.Gateway.Mode == config.GatewayModeLocal {
				// except if advertised through BGP, go to kernel
				// TODO: MEG enabled pods should still go through the patch port
				// but holding this until
				// https://issues.redhat.com/browse/FDP-646 is fixed, for now we
				// are assuming MEG & BGP are not used together
				output = ovsLocalPort
			}
			for _, clusterEntry := range config.Default.ClusterSubnets {
				cidr := clusterEntry.CIDR
				ipv := getIPv(cidr)
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=15, table=1, %s, %s_dst=%s, "+
						"actions=output:%s",
						defaultOpenFlowCookie, ipv, ipv, cidr, output))
			}
			if output == defaultNetConfig.ofPortPatch {
				// except node management traffic
				for _, subnet := range subnets {
					mgmtIP := util.GetNodeManagementIfAddr(subnet)
					ipv := getIPv(mgmtIP)
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=16, table=1, %s, %s_dst=%s, "+
							"actions=output:%s",
							defaultOpenFlowCookie, ipv, ipv, mgmtIP.IP, ovsLocalPort),
					)
				}
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
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp6, tp_dst=3784, actions=output:%s,output:%s",
						defaultOpenFlowCookie, ofPortPhys, defaultNetConfig.ofPortPatch, ofPortHost))
			}
		}

		if config.IPv4Mode {
			if ofPortPhys != "" {
				// We send BFD traffic both on the host and in ovn
				dftFlows = append(dftFlows,
					fmt.Sprintf("cookie=%s, priority=13, table=1, in_port=%s, udp, tp_dst=3784, actions=output:%s,output:%s",
						defaultOpenFlowCookie, ofPortPhys, defaultNetConfig.ofPortPatch, ofPortHost))
			}
		}

		// packets larger than known acceptable MTU need to go to kernel for
		// potential fragmentation
		// introduced specifically for replies to egress traffic not routed
		// through the host
		if config.Gateway.Mode == config.GatewayModeLocal && !config.Gateway.DisablePacketMTUCheck {
			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=10, table=11, reg0=0x1, "+
					"actions=output:%s", defaultOpenFlowCookie, ofPortHost))

			// Send UDN destined traffic to right patch port
			for _, netConfig := range bridge.patchedNetConfigs() {
				if netConfig.masqCTMark != ctMarkOVN {
					dftFlows = append(dftFlows,
						fmt.Sprintf("cookie=%s, priority=5, table=11, ct_mark=%s, "+
							"actions=output:%s", defaultOpenFlowCookie, netConfig.masqCTMark, netConfig.ofPortPatch))
				}
			}

			dftFlows = append(dftFlows,
				fmt.Sprintf("cookie=%s, priority=1, table=11, "+
					"actions=output:%s", defaultOpenFlowCookie, defaultNetConfig.ofPortPatch))
		}

		// table 1, all other connections do normal processing
		dftFlows = append(dftFlows,
			fmt.Sprintf("cookie=%s, priority=0, table=1, actions=output:NORMAL", defaultOpenFlowCookie))
	}

	return dftFlows, nil
}

func setBridgeOfPorts(bridge *bridgeConfiguration) error {
	bridge.Lock()
	defer bridge.Unlock()
	// Get ofport of patchPort
	for _, netConfig := range bridge.netConfig {
		if err := netConfig.setBridgeNetworkOfPortsInternal(); err != nil {
			return fmt.Errorf("error setting bridge openflow ports for network with patchport %v: err: %v", netConfig.patchPort, err)
		}
	}

	if bridge.uplinkName != "" {
		// Get ofport of physical interface
		ofportPhys, stderr, err := util.GetOVSOfPort("get", "interface", bridge.uplinkName, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
				bridge.uplinkName, stderr, err)
		}
		bridge.ofPortPhys = ofportPhys
	}

	// Get ofport representing the host. That is, host representor port in case of DPUs, ovsLocalPort otherwise.
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

func newGateway(
	nodeName string,
	subnets []*net.IPNet,
	gwNextHops []net.IP,
	gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet,
	nodeAnnotator kube.Annotator,
	cfg *managementPortConfig,
	kube kube.Interface,
	watchFactory factory.NodeWatchFactory,
	routeManager *routemanager.Controller,
	linkManager *linkmanager.Controller,
	networkManager networkmanager.Interface,
	gatewayMode config.GatewayMode,
) (*gateway, error) {
	klog.Info("Creating new gateway")
	gw := &gateway{}

	if gatewayMode == config.GatewayModeLocal {
		if err := initLocalGateway(subnets, cfg); err != nil {
			return nil, fmt.Errorf("failed to initialize new local gateway, err: %w", err)
		}
	}

	gwBridge, exGwBridge, err := gatewayInitInternal(
		nodeName, gwIntf, egressGWIntf, gwNextHops, gwIPs, nodeAnnotator)
	if err != nil {
		return nil, err
	}

	if exGwBridge != nil {
		gw.readyFunc = func() (bool, error) {
			gwBridge.Lock()
			for _, netConfig := range gwBridge.netConfig {
				ready, err := gatewayReady(netConfig.patchPort)
				if err != nil || !ready {
					gwBridge.Unlock()
					return false, err
				}
			}
			gwBridge.Unlock()
			exGwBridge.Lock()
			for _, netConfig := range exGwBridge.netConfig {
				exGWReady, err := gatewayReady(netConfig.patchPort)
				if err != nil || !exGWReady {
					exGwBridge.Unlock()
					return false, err
				}
			}
			exGwBridge.Unlock()
			return true, nil
		}
	} else {
		gw.readyFunc = func() (bool, error) {
			gwBridge.Lock()
			for _, netConfig := range gwBridge.netConfig {
				ready, err := gatewayReady(netConfig.patchPort)
				if err != nil || !ready {
					gwBridge.Unlock()
					return false, err
				}
			}
			gwBridge.Unlock()
			return true, nil
		}
	}

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Gateway Openflow Manager")
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
		if util.IsNetworkSegmentationSupportEnabled() && config.OVNKubernetesFeature.EnableInterconnect && config.Gateway.Mode != config.GatewayModeDisabled {
			gw.bridgeEIPAddrManager = newBridgeEIPAddrManager(nodeName, gwBridge.bridgeName, linkManager, kube, watchFactory.EgressIPInformer(), watchFactory.NodeCoreInformer())
			gwBridge.eipMarkIPs = gw.bridgeEIPAddrManager.GetCache()
		}
		gw.nodeIPManager = newAddressManager(nodeName, kube, cfg, watchFactory, gwBridge)
		nodeIPs := gw.nodeIPManager.ListAddresses()

		if config.OvnKubeNode.Mode == types.NodeModeFull {
			// Delete stale masquerade resources if there are any. This is to make sure that there
			// are no Linux resources with IP from old masquerade subnet when masquerade subnet
			// gets changed as part of day2 operation.
			if err := deleteStaleMasqueradeResources(gwBridge.bridgeName, nodeName, watchFactory); err != nil {
				return fmt.Errorf("failed to remove stale masquerade resources: %w", err)
			}

			if err := setNodeMasqueradeIPOnExtBridge(gwBridge.bridgeName); err != nil {
				return fmt.Errorf("failed to set the node masquerade IP on the ext bridge %s: %v", gwBridge.bridgeName, err)
			}

			if err := addMasqueradeRoute(routeManager, gwBridge.bridgeName, nodeName, gwIPs, watchFactory); err != nil {
				return fmt.Errorf("failed to set the node masquerade route to OVN: %v", err)
			}

			// Masquerade config mostly done on node, update annotation
			if err := updateMasqueradeAnnotation(nodeName, kube); err != nil {
				return fmt.Errorf("failed to update masquerade subnet annotation on node: %s, error: %v", nodeName, err)
			}
		}

		gw.openflowManager, err = newGatewayOpenFlowManager(gwBridge, exGwBridge, subnets, nodeIPs)
		if err != nil {
			return err
		}

		// resync flows on IP change
		gw.nodeIPManager.OnChanged = func() {
			klog.V(5).Info("Node addresses changed, re-syncing bridge flows")
			if err := gw.openflowManager.updateBridgeFlowCache(subnets, gw.nodeIPManager.ListAddresses(), gw.isPodNetworkAdvertised); err != nil {
				// very unlikely - somehow node has lost its IP address
				klog.Errorf("Failed to re-generate gateway flows after address change: %v", err)
			}
			npw, _ := gw.nodePortWatcher.(*nodePortWatcher)
			npw.updateGatewayIPs(gw.nodeIPManager)
			// Services create OpenFlow flows as well, need to update them all
			if gw.servicesRetryFramework != nil {
				if errs := gw.addAllServices(); len(errs) > 0 {
					err := utilerrors.Join(errs...)
					klog.Errorf("Failed to sync all services after node IP change: %v", err)
				}
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
			klog.Info("Creating Gateway Node Port Watcher")
			gw.nodePortWatcher, err = newNodePortWatcher(gwBridge, gw.openflowManager, gw.nodeIPManager, watchFactory, networkManager)
			if err != nil {
				return err
			}
		} else {
			// no service OpenFlows, request to sync flows now.
			gw.openflowManager.requestFlowSync()
		}

		if err := addHostMACBindings(gwBridge.bridgeName); err != nil {
			return fmt.Errorf("failed to add MAC bindings for service routing: %w", err)
		}

		return nil
	}
	gw.watchFactory = watchFactory.(*factory.WatchFactory)
	klog.Info("Gateway Creation Complete")
	return gw, nil
}

func newNodePortWatcher(
	gwBridge *bridgeConfiguration,
	ofm *openflowManager,
	nodeIPManager *addressManager,
	watchFactory factory.NodeWatchFactory,
	networkManager networkmanager.Interface,
) (*nodePortWatcher, error) {

	// Get ofport of physical interface
	ofportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get",
		"interface", gwBridge.uplinkName, "ofport")
	if err != nil {
		return nil, fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			gwBridge.uplinkName, stderr, err)
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
		if util.IsNetworkSegmentationSupportEnabled() {
			if err := configureUDNServicesNFTables(); err != nil {
				return nil, fmt.Errorf("unable to configure UDN nftables: %w", err)
			}
		}
	}

	var subnets []*net.IPNet
	for _, subnet := range config.Default.ClusterSubnets {
		subnets = append(subnets, subnet.CIDR)
	}
	subnets = append(subnets, config.Kubernetes.ServiceCIDRs...)
	if config.Gateway.DisableForwarding {
		if err := initExternalBridgeServiceForwardingRules(subnets); err != nil {
			return nil, fmt.Errorf("failed to add accept rules in forwarding table for bridge %s: err %v", gwBridge.bridgeName, err)
		}
	} else {
		if err := delExternalBridgeServiceForwardingRules(subnets); err != nil {
			return nil, fmt.Errorf("failed to delete accept rules in forwarding table for bridge %s: err %v", gwBridge.bridgeName, err)
		}
	}

	// used to tell addServiceRules which rules to add
	dpuMode := false
	if config.OvnKubeNode.Mode != types.NodeModeFull {
		dpuMode = true
	}

	// Get Physical IPs of Node, Can be IPV4 IPV6 or both
	gatewayIPv4, gatewayIPv6 := getGatewayFamilyAddrs(gwBridge.ips)

	npw := &nodePortWatcher{
		dpuMode:        dpuMode,
		gatewayIPv4:    gatewayIPv4,
		gatewayIPv6:    gatewayIPv6,
		ofportPhys:     ofportPhys,
		gwBridge:       gwBridge.bridgeName,
		serviceInfo:    make(map[ktypes.NamespacedName]*serviceConfig),
		nodeIPManager:  nodeIPManager,
		ofm:            ofm,
		watchFactory:   watchFactory,
		networkManager: networkManager,
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

func addMasqueradeRoute(routeManager *routemanager.Controller, netIfaceName, nodeName string, ifAddrs []*net.IPNet, watchFactory factory.NodeWatchFactory) error {
	var ipv4, ipv6 net.IP
	findIPs := func(ips []net.IP) error {
		var err error
		if config.IPv4Mode && ipv4 == nil {
			ipv4, err = util.MatchFirstIPFamily(false, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		if config.IPv6Mode && ipv6 == nil {
			ipv6, err = util.MatchFirstIPFamily(true, ips)
			if err != nil {
				return fmt.Errorf("missing IP among %+v: %v", ips, err)
			}
		}
		return nil
	}

	// Try first with the node status IPs and fallback to the interface IPs. The
	// fallback is a workaround for instances where the node status might not
	// have the minimum set of IPs we need (for example, when ovnkube is
	// restarted after enabling an IP family without actually restarting kubelet
	// with a new configuration including an IP address for that family). Node
	// status IPs are preferred though because a user might add arbitrary IP
	// addresses to the interface that we don't really want to use and might
	// cause problems.

	var nodeIPs []net.IP
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return err
	}
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type != kapi.NodeInternalIP {
			continue
		}
		nodeIP := utilnet.ParseIPSloppy(nodeAddr.Address)
		nodeIPs = append(nodeIPs, nodeIP)
	}

	err = findIPs(nodeIPs)
	if err != nil {
		klog.Warningf("Unable to add OVN masquerade route to host using source node status IPs: %v", err)
		// fallback to the interface IPs
		var ifIPs []net.IP
		for _, ifAddr := range ifAddrs {
			ifIPs = append(ifIPs, ifAddr.IP)
		}
		err := findIPs(ifIPs)
		if err != nil {
			return fmt.Errorf("unable to add OVN masquerade route to host using interface IPs: %v", err)
		}
	}

	netIfaceLink, err := util.LinkSetUp(netIfaceName)
	if err != nil {
		return fmt.Errorf("unable to find shared gw bridge interface: %s", netIfaceName)
	}
	mtu := 0
	if ipv4 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/32", config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv4)
		err = routeManager.Add(netlink.Route{LinkIndex: netIfaceLink.Attrs().Index, Dst: masqIPNet, MTU: mtu, Src: ipv4})
		if err != nil {
			return fmt.Errorf("failed to add OVN Masquerade route: %w", err)
		}
	}

	if ipv6 != nil {
		_, masqIPNet, _ := net.ParseCIDR(fmt.Sprintf("%s/128", config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()))
		klog.Infof("Setting OVN Masquerade route with source: %s", ipv6)
		err = routeManager.Add(netlink.Route{LinkIndex: netIfaceLink.Attrs().Index, Dst: masqIPNet, MTU: mtu, Src: ipv6})
		if err != nil {
			return fmt.Errorf("failed to add OVN Masquerade route: %w", err)
		}
	}
	return nil
}

func setNodeMasqueradeIPOnExtBridge(extBridgeName string) error {
	extBridge, err := util.LinkSetUp(extBridgeName)
	if err != nil {
		return err
	}

	var bridgeCIDRs []cidrAndFlags
	if config.IPv4Mode {
		_, masqIPNet, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
		masqIPNet.IP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: 0})
	}

	if config.IPv6Mode {
		_, masqIPNet, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)
		masqIPNet.IP = config.Gateway.MasqueradeIPs.V6HostMasqueradeIP
		// Deprecate the IPv6 host masquerade IP address to ensure its not used in source address selection except
		// if a route explicitly sets its src IP as this masquerade IP. See RFC 3484 for more details for linux src address selection.
		// Currently, we set a route with destination as the service CIDR with source IP as the host masquerade IP.
		// Also, ideally we would only set the preferredLifetime to 0, but because this is the default value of this type, the netlink lib
		// will only propagate preferred lifetime to netlink if either preferred lifetime or valid lifetime is set greater than 0.
		// Set valid lifetime to max will achieve our goal of setting preferred lifetime 0.
		bridgeCIDRs = append(bridgeCIDRs, cidrAndFlags{ipNet: masqIPNet, flags: unix.IFA_F_NODAD, preferredLifetime: 0,
			validLifetime: math.MaxUint32})
	}

	for _, bridgeCIDR := range bridgeCIDRs {
		if exists, err := util.LinkAddrExist(extBridge, bridgeCIDR.ipNet); err == nil && !exists {
			if err := util.LinkAddrAdd(extBridge, bridgeCIDR.ipNet, bridgeCIDR.flags, bridgeCIDR.preferredLifetime,
				bridgeCIDR.validLifetime); err != nil {
				return fmt.Errorf("failed to set node masq IP on bridge %s because unable to add address %s: %v",
					extBridgeName, bridgeCIDR.ipNet.String(), err)
			}
		} else if err == nil && exists && utilnet.IsIPv6(bridgeCIDR.ipNet.IP) {
			// FIXME(mk): remove this logic when it is no longer possible to upgrade from a version which doesn't have
			// a deprecated ipv6 host masq addr

			// Deprecate IPv6 address to prevent connections from using it as its source address. For connections towards
			// a service VIP, routes exist to explicitly add this address as source.
			isDeprecated, err := util.IsDeprecatedAddr(extBridge, bridgeCIDR.ipNet)
			if err != nil {
				return fmt.Errorf("failed to set node masq IP on bridge %s because unable to detect if address %s is deprecated: %v",
					extBridgeName, bridgeCIDR.ipNet.String(), err)
			}
			if !isDeprecated {
				if err = util.LinkAddrDel(extBridge, bridgeCIDR.ipNet); err != nil {
					klog.Warningf("Failed to delete stale masq IP %s on bridge %s because unable to delete it: %v",
						bridgeCIDR.ipNet.String(), extBridgeName, err)
				}
				if err = util.LinkAddrAdd(extBridge, bridgeCIDR.ipNet, bridgeCIDR.flags, bridgeCIDR.preferredLifetime,
					bridgeCIDR.validLifetime); err != nil {
					return err
				}
			}
		} else if err != nil {
			return fmt.Errorf(
				"failed to check existence of addr %s in bridge %s: %v", bridgeCIDR.ipNet, extBridgeName, err)
		}
	}

	return nil
}

func addHostMACBindings(bridgeName string) error {
	// Add a neighbour entry on the K8s node to map dummy next-hop masquerade
	// addresses with MACs. This is required because these addresses do not
	// exist on the network and will not respond to an ARP/ND, so to route them
	// we need an entry.
	// Additionally, the OVN Masquerade IP is not assigned to its interface, so
	// we also need a fake entry for that.
	link, err := util.LinkSetUp(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", bridgeName, err)
	}

	var neighborIPs []string
	if config.IPv4Mode {
		neighborIPs = append(neighborIPs, config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String(), config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP.String())
	}
	if config.IPv6Mode {
		neighborIPs = append(neighborIPs, config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String(), config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP.String())
	}
	for _, ip := range neighborIPs {
		klog.Infof("Ensuring IP Neighbor entry for: %s", ip)
		dummyNextHopMAC := util.IPAddrToHWAddr(net.ParseIP(ip))
		if exists, err := util.LinkNeighExists(link, net.ParseIP(ip), dummyNextHopMAC); err == nil && !exists {
			// LinkNeighExists checks if the mac also matches, but it is possible there is a stale entry
			// still in the neighbor cache which would prevent add. Therefore execute a delete first.
			if err = util.LinkNeighDel(link, net.ParseIP(ip)); err != nil {
				klog.Warningf("Failed to remove IP neighbor entry for ip %s, on iface %s: %v",
					ip, bridgeName, err)
			}
			if err = util.LinkNeighAdd(link, net.ParseIP(ip), dummyNextHopMAC); err != nil {
				return fmt.Errorf("failed to configure neighbor: %s, on iface %s: %v",
					ip, bridgeName, err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to configure neighbor:%s, on iface %s: %v", ip, bridgeName, err)
		}
	}
	return nil
}

func updateMasqueradeAnnotation(nodeName string, kube kube.Interface) error {
	_, v4MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
	_, v6MasqueradeCIDR, _ := net.ParseCIDR(config.Gateway.V6MasqueradeSubnet)
	nodeAnnotation, err := util.CreateNodeMasqueradeSubnetAnnotation(nil, v4MasqueradeCIDR, v6MasqueradeCIDR)
	if err != nil {
		return fmt.Errorf("unable to generate masquerade subnet annotation update: %w", err)
	}
	if err := kube.SetAnnotationsOnNode(nodeName, nodeAnnotation); err != nil {
		return fmt.Errorf("unable to set node masquerade subnet annotation update: %w", err)
	}
	return nil
}

// generateIPFragmentReassemblyFlow adds flows in table 0 that send packets to a
// specific conntrack zone for reassembly with the same priority as node port
// flows that match on L4 fields. After reassembly packets are reinjected to
// table 0 again. This requires a conntrack immplementation that reassembles
// fragments. This reqreuiment is met for the kernel datapath with the netfilter
// module loaded. This reqreuiment is not met for the userspace datapath.
func generateIPFragmentReassemblyFlow(ofPortPhys string) []string {
	flows := make([]string, 0, 2)
	if config.IPv4Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, ip, nw_frag=yes, actions=ct(table=0,zone=%d)",
				defaultOpenFlowCookie,
				ofPortPhys,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}
	if config.IPv6Mode {
		flows = append(flows,
			fmt.Sprintf("cookie=%s, priority=110, table=0, in_port=%s, ipv6, nw_frag=yes, actions=ct(table=0,zone=%d)",
				defaultOpenFlowCookie,
				ofPortPhys,
				config.Default.ReassemblyConntrackZone,
			),
		)
	}

	return flows
}

// deleteStaleMasqueradeResources removes stale Linux resources when config.Gateway.V4MasqueradeSubnet
// or config.Gateway.V6MasqueradeSubnet gets changed at day 2.
func deleteStaleMasqueradeResources(bridgeName, nodeName string, wf factory.NodeWatchFactory) error {
	var staleMasqueradeIPs config.MasqueradeIPsConfig
	node, err := wf.GetNode(nodeName)
	if err != nil {
		return err
	}
	subnets, err := util.ParseNodeMasqueradeSubnet(node)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// no annotation set, must be initial bring up, nothing to clean
			return nil
		}
		return err
	}

	var v4ConfiguredMasqueradeNet, v6ConfiguredMasqueradeNet *net.IPNet

	for _, subnet := range subnets {
		if utilnet.IsIPv6CIDR(subnet) {
			v6ConfiguredMasqueradeNet = subnet
		} else if utilnet.IsIPv4CIDR(subnet) {
			v4ConfiguredMasqueradeNet = subnet
		} else {
			return fmt.Errorf("invalid subnet for masquerade annotation: %s", subnet)
		}
	}

	if v4ConfiguredMasqueradeNet != nil && config.Gateway.V4MasqueradeSubnet != v4ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV4MasqueradeIPs(v4ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V4MasqueradeIPs: %s", err)
		}
	}
	if v6ConfiguredMasqueradeNet != nil && config.Gateway.V6MasqueradeSubnet != v6ConfiguredMasqueradeNet.String() {
		if err := config.AllocateV6MasqueradeIPs(v6ConfiguredMasqueradeNet.IP, &staleMasqueradeIPs); err != nil {
			return fmt.Errorf("unable to determine stale V6MasqueradeIPs: %s", err)
		}
	}
	link, err := util.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", bridgeName, err)
	}

	if staleMasqueradeIPs.V4HostMasqueradeIP != nil || staleMasqueradeIPs.V6HostMasqueradeIP != nil {
		if err = deleteMasqueradeResources(link, &staleMasqueradeIPs); err != nil {
			klog.Errorf("Unable to delete masquerade resources! Some configuration for the masquerade subnet "+
				"may be left on the node and may cause issues! Errors: %v", err)
		}
	}

	return nil
}

// deleteMasqueradeResources removes following Linux resources given a config.MasqueradeIPsConfig
// struct and netlink.Link:
// - neighbour object for IPv4 and IPv6 OVNMasqueradeIP and DummyNextHopMasqueradeIP.
// - masquerade route added by addMasqueradeRoute function while starting up the gateway.
// - iptables rules created for masquerade subnet based on ipForwarding and Gateway mode.
// - stale HostMasqueradeIP address from gateway bridge
func deleteMasqueradeResources(link netlink.Link, staleMasqueradeIPs *config.MasqueradeIPsConfig) error {
	var subnets []*net.IPNet
	var neighborIPs []net.IP
	var aggregatedErrors []error
	klog.Infof("Stale masquerade resources detected, cleaning IPs: %s, %s, %s, %s",
		staleMasqueradeIPs.V4HostMasqueradeIP,
		staleMasqueradeIPs.V6HostMasqueradeIP,
		staleMasqueradeIPs.V4OVNMasqueradeIP,
		staleMasqueradeIPs.V6OVNMasqueradeIP)
	if config.IPv4Mode && staleMasqueradeIPs.V4HostMasqueradeIP != nil {
		// Delete any stale masquerade IP from external bridge.
		hostMasqIPNet, err := util.LinkAddrGetIPNet(link, staleMasqueradeIPs.V4HostMasqueradeIP)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("unable to get IPNet from link %s: %w", link, err))
		}
		if hostMasqIPNet != nil {
			if err := util.LinkAddrDel(link, hostMasqIPNet); err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove masquerade IP from bridge %s: %w", link, err))
			}
		}

		_, masqIPNet, err := net.ParseCIDR(fmt.Sprintf("%s/32", staleMasqueradeIPs.V4OVNMasqueradeIP.String()))
		if err != nil {
			aggregatedErrors = append(aggregatedErrors,
				fmt.Errorf("failed to parse V4OVNMasqueradeIP %s: %v", staleMasqueradeIPs.V4OVNMasqueradeIP.String(), err))
		}
		subnets = append(subnets, masqIPNet)
		neighborIPs = append(neighborIPs, staleMasqueradeIPs.V4OVNMasqueradeIP, staleMasqueradeIPs.V4DummyNextHopMasqueradeIP)
		if err := nodeipt.DelRules(getStaleMasqueradeIptablesRules(staleMasqueradeIPs.V4OVNMasqueradeIP)); err != nil {
			aggregatedErrors = append(aggregatedErrors,
				fmt.Errorf("failed to delete forwarding iptables rules for stale masquerade subnet %s: ", err))
		}
	}

	if config.IPv6Mode && staleMasqueradeIPs.V6HostMasqueradeIP != nil {
		// Delete any stale masquerade IP from external bridge.
		hostMasqIPNet, err := util.LinkAddrGetIPNet(link, staleMasqueradeIPs.V6HostMasqueradeIP)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("unable to get IPNet from link %s: %w", link, err))
		}
		if hostMasqIPNet != nil {
			if err := util.LinkAddrDel(link, hostMasqIPNet); err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove masquerade IP from bridge %s: %w", link, err))
			}
		}

		_, masqIPNet, err := net.ParseCIDR(fmt.Sprintf("%s/128", staleMasqueradeIPs.V6OVNMasqueradeIP.String()))
		if err != nil {
			return fmt.Errorf("failed to parse V6OVNMasqueradeIP %s: %v", staleMasqueradeIPs.V6OVNMasqueradeIP.String(), err)
		}
		subnets = append(subnets, masqIPNet)
		neighborIPs = append(neighborIPs, staleMasqueradeIPs.V6OVNMasqueradeIP, staleMasqueradeIPs.V6DummyNextHopMasqueradeIP)
		if err := nodeipt.DelRules(getStaleMasqueradeIptablesRules(staleMasqueradeIPs.V6OVNMasqueradeIP)); err != nil {
			return fmt.Errorf("failed to delete forwarding iptables rules for stale masquerade subnet %s: ", err)
		}
	}

	for _, ip := range neighborIPs {
		if err := util.LinkNeighDel(link, ip); err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to remove IP neighbour entry for ip %s, "+
				"on iface %s: %v", ip, link.Attrs().Name, err))
		}
	}

	if len(subnets) != 0 {
		if err := util.LinkRoutesDel(link, subnets); err != nil {
			aggregatedErrors = append(aggregatedErrors, fmt.Errorf("failed to list addresses for the link %s: %v", link.Attrs().Name, err))
		}
	}

	return utilerrors.Join(aggregatedErrors...)
}

func getIPv(ipnet *net.IPNet) string {
	prefix := "ip"
	if utilnet.IsIPv6CIDR(ipnet) {
		prefix = "ipv6"
	}
	return prefix
}
