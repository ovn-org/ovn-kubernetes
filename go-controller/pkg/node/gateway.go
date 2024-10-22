package node

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/safchain/ethtool"
	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init(<-chan struct{}, *sync.WaitGroup) error
	Start()
	GetGatewayBridgeIface() string
	SetDefaultGatewayBridgeMAC(addr net.HardwareAddr)
	Reconcile() error
}

type gateway struct {
	// loadBalancerHealthChecker is a health check server for load-balancer type services
	loadBalancerHealthChecker informer.ServiceAndEndpointsEventHandler
	// portClaimWatcher is for reserving ports for virtual IPs allocated by the cluster on the host
	portClaimWatcher informer.ServiceEventHandler
	// nodePortWatcherIptables is used in Shared GW mode to handle nodePort IPTable rules
	nodePortWatcherIptables informer.ServiceEventHandler
	// nodePortWatcher is used in Local+Shared GW modes to handle nodePort flows in shared OVS bridge
	nodePortWatcher informer.ServiceAndEndpointsEventHandler
	openflowManager *openflowManager
	nodeIPManager   *addressManager
	initFunc        func() error
	readyFunc       func() (bool, error)

	servicesRetryFramework *retry.RetryFramework

	watchFactory *factory.WatchFactory // used for retry
	stopChan     <-chan struct{}
	wg           *sync.WaitGroup
}

func (g *gateway) AddService(svc *kapi.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.AddService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) UpdateService(old, new *kapi.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.UpdateService(old, new); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) DeleteService(svc *kapi.Service) error {
	var err error
	var errors []error

	if g.portClaimWatcher != nil {
		if err = g.portClaimWatcher.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcherIptables != nil {
		if err = g.nodePortWatcherIptables.DeleteService(svc); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)
}

func (g *gateway) SyncServices(objs []interface{}) error {
	var err error
	klog.Infof("Starting gateway service sync")
	start := time.Now()
	if g.portClaimWatcher != nil {
		err = g.portClaimWatcher.SyncServices(objs)
	}
	if err == nil && g.loadBalancerHealthChecker != nil {
		err = g.loadBalancerHealthChecker.SyncServices(objs)
	}
	if err == nil && g.nodePortWatcher != nil {
		err = g.nodePortWatcher.SyncServices(objs)
	}
	if err == nil && g.nodePortWatcherIptables != nil {
		err = g.nodePortWatcherIptables.SyncServices(objs)
	}
	if err != nil {
		return fmt.Errorf("gateway sync services failed: %v", err)
	}
	klog.Infof("Gateway service sync done. Time taken: %s", time.Since(start))
	return nil
}

func (g *gateway) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.AddEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.AddEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)

}

func (g *gateway) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.UpdateEndpointSlice(oldEpSlice, newEpSlice); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.UpdateEndpointSlice(oldEpSlice, newEpSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)

}

func (g *gateway) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	var err error
	var errors []error

	if g.loadBalancerHealthChecker != nil {
		if err = g.loadBalancerHealthChecker.DeleteEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	if g.nodePortWatcher != nil {
		if err = g.nodePortWatcher.DeleteEndpointSlice(epSlice); err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.Join(errors...)

}

func (g *gateway) Init(stopChan <-chan struct{}, wg *sync.WaitGroup) error {
	g.stopChan = stopChan
	g.wg = wg

	var err error

	g.servicesRetryFramework = g.newRetryFrameworkNode(factory.ServiceForGatewayType)
	if _, err = g.servicesRetryFramework.WatchResource(); err != nil {
		return fmt.Errorf("gateway init failed to start watching services: %v", err)
	}

	endpointSlicesRetryFramework := g.newRetryFrameworkNode(factory.EndpointSliceForGatewayType)

	if util.IsNetworkSegmentationSupportEnabled() {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		req, err := labels.NewRequirement(discovery.LabelServiceName, selection.Exists, nil)
		if err != nil {
			return err
		}
		if _, err = endpointSlicesRetryFramework.WatchResourceFiltered("", labels.NewSelector().Add(*req)); err != nil {
			return fmt.Errorf("gateway init failed to start watching endpointslices: %v", err)
		}
		return nil
	}
	if _, err = endpointSlicesRetryFramework.WatchResource(); err != nil {
		return fmt.Errorf("gateway init failed to start watching endpointslices: %v", err)
	}
	return nil
}

func (g *gateway) Start() {
	if g.nodeIPManager != nil {
		g.nodeIPManager.Run(g.stopChan, g.wg)
	}

	if g.openflowManager != nil {
		klog.Info("Spawning Conntrack Rule Check Thread")
		g.openflowManager.Run(g.stopChan, g.wg)
	}
}

// sets up an uplink interface for UDP Generic Receive Offload forwarding as part of
// the EnableUDPAggregation feature.
func setupUDPAggregationUplink(ifname string) error {
	e, err := ethtool.NewEthtool()
	if err != nil {
		return fmt.Errorf("failed to initialize ethtool: %v", err)
	}
	defer e.Close()

	err = e.Change(ifname, map[string]bool{
		"rx-udp-gro-forwarding": true,
	})
	if err != nil {
		return fmt.Errorf("could not enable UDP offload features on %q: %v", ifname, err)
	}

	return nil
}

func gatewayInitInternal(nodeName, gwIntf, egressGatewayIntf string, gwNextHops []net.IP, gwIPs []*net.IPNet, nodeAnnotator kube.Annotator) (
	*bridgeConfiguration, *bridgeConfiguration, error) {
	gatewayBridge, err := bridgeForInterface(gwIntf, nodeName, types.PhysicalNetworkName, gwIPs)
	if err != nil {
		return nil, nil, fmt.Errorf("bridge for interface failed for %s: %w", gwIntf, err)
	}
	var egressGWBridge *bridgeConfiguration
	if egressGatewayIntf != "" {
		egressGWBridge, err = bridgeForInterface(egressGatewayIntf, nodeName, types.PhysicalNetworkExGwName, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("bridge for interface failed for %s: %w", egressGatewayIntf, err)
		}
	}

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, nil, err
	}

	// Set annotation that determines if options:gateway_mtu shall be set for this node.
	enableGatewayMTU := true
	if config.Gateway.DisablePacketMTUCheck {
		klog.Warningf("Config option disable-pkt-mtu-check is set to true. " +
			"options:gateway_mtu will be disabled on gateway routers. " +
			"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
		enableGatewayMTU = false
	} else {
		chkPktLengthSupported, err := util.DetectCheckPktLengthSupport(gatewayBridge.bridgeName)
		if err != nil {
			return nil, nil, err
		}
		if !chkPktLengthSupported {
			klog.Warningf("OVS does not support check_packet_length action. " +
				"options:gateway_mtu will be disabled on gateway routers. " +
				"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
			enableGatewayMTU = false
		} else {
			/* This is a work around. In order to have the most optimal performance, the packet MTU check should be
			 * disabled when OVS HW Offload is enabled on the node. The reason is that OVS HW Offload does not support
			 * packet MTU checks properly without the offload support for sFlow.
			 * The patches for sFlow in OvS: https://patchwork.ozlabs.org/project/openvswitch/list/?series=290804
			 * As of writing these offload support patches for sFlow are in review.
			 * TODO: This workaround should be removed once the offload support for sFlow patches are merged upstream OvS.
			 */
			ovsHardwareOffloadEnabled, err := util.IsOvsHwOffloadEnabled()
			if err != nil {
				return nil, nil, err
			}
			if ovsHardwareOffloadEnabled {
				klog.Warningf("OVS hardware offloading is enabled. " +
					"options:gateway_mtu will be disabled on gateway routers for performance reasons. " +
					"IP fragmentation or large TCP/UDP payloads may not be forwarded correctly.")
				enableGatewayMTU = false
			}
		}
	}
	if err := util.SetGatewayMTUSupport(nodeAnnotator, enableGatewayMTU); err != nil {
		return nil, nil, err
	}

	if config.Default.EnableUDPAggregation {
		err = setupUDPAggregationUplink(gatewayBridge.uplinkName)
		if err == nil && egressGWBridge != nil {
			err = setupUDPAggregationUplink(egressGWBridge.uplinkName)
		}
		if err != nil {
			klog.Warningf("Could not enable UDP packet aggregation on uplink interface (aggregation will be disabled): %v", err)
			config.Default.EnableUDPAggregation = false
		}
	}

	l3GwConfig := util.L3GatewayConfig{
		Mode:           config.Gateway.Mode,
		ChassisID:      chassisID,
		BridgeID:       gatewayBridge.bridgeName,
		InterfaceID:    gatewayBridge.interfaceID,
		MACAddress:     gatewayBridge.macAddress,
		IPAddresses:    gatewayBridge.ips,
		NextHops:       gwNextHops,
		NodePortEnable: config.Gateway.NodeportEnable,
		VLANID:         &config.Gateway.VLANID,
	}
	if egressGWBridge != nil {
		l3GwConfig.EgressGWInterfaceID = egressGWBridge.interfaceID
		l3GwConfig.EgressGWMACAddress = egressGWBridge.macAddress
		l3GwConfig.EgressGWIPAddresses = egressGWBridge.ips
	}

	err = util.SetL3GatewayConfig(nodeAnnotator, &l3GwConfig)
	return gatewayBridge, egressGWBridge, err
}

func gatewayReady(patchPort string) (bool, error) {
	// Get ofport of patchPort
	ofport, _, err := util.GetOVSOfPort("--if-exists", "get", "interface", patchPort, "ofport")
	if err != nil || len(ofport) == 0 {
		return false, nil
	}
	klog.Info("Gateway is ready")
	return true, nil
}

func (g *gateway) GetGatewayBridgeIface() string {
	return g.openflowManager.getDefaultBridgeName()
}

// getMaxFrameLength returns the maximum frame size (ignoring VLAN header) that a gateway can handle
func getMaxFrameLength() int {
	return config.Default.MTU + 14
}

// SetDefaultGatewayBridgeMAC updates the mac address for the OFM used to render flows with
func (g *gateway) SetDefaultGatewayBridgeMAC(macAddr net.HardwareAddr) {
	g.openflowManager.setDefaultBridgeMAC(macAddr)
	klog.Infof("Default gateway bridge MAC address updated to %s", macAddr)
}

// Reconcile handles triggering updates to different components of a gateway, like OFM, Services
func (g *gateway) Reconcile() error {
	klog.Info("Reconciling gateway with updates")
	node, err := g.watchFactory.GetNode(g.nodeIPManager.nodeName)
	if err != nil {
		return err
	}
	subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed to get subnets for node: %s for OpenFlow cache update; err: %w", node.Name, err)
	}
	if err := g.openflowManager.updateBridgeFlowCache(subnets, g.nodeIPManager.ListAddresses()); err != nil {
		return err
	}
	// Services create OpenFlow flows as well, need to update them all
	if g.servicesRetryFramework != nil {
		if errs := g.addAllServices(); errs != nil {
			err := utilerrors.Join(errs...)
			return err
		}
	}
	return nil
}

func (g *gateway) addAllServices() []error {
	errs := []error{}
	svcs, err := g.watchFactory.GetServices()
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, svc := range svcs {
			svc := *svc
			klog.V(5).Infof("Adding service %s/%s to retryServices", svc.Namespace, svc.Name)
			err = g.servicesRetryFramework.AddRetryObjWithAddNoBackoff(&svc)
			if err != nil {
				err = fmt.Errorf("failed to add service %s/%s to retry framework: %w", svc.Namespace, svc.Name, err)
				errs = append(errs, err)
			}
		}
	}
	g.servicesRetryFramework.RequestRetryObjs()
	return errs
}

type bridgeConfiguration struct {
	sync.Mutex
	nodeName    string
	bridgeName  string
	uplinkName  string
	ips         []*net.IPNet
	interfaceID string
	macAddress  net.HardwareAddr
	ofPortPhys  string
	ofPortHost  string
	netConfig   map[string]*bridgeUDNConfiguration
}

// updateInterfaceIPAddresses sets and returns the bridge's current ips
func (b *bridgeConfiguration) updateInterfaceIPAddresses(node *kapi.Node) ([]*net.IPNet, error) {
	b.Lock()
	defer b.Unlock()
	ifAddrs, err := getNetworkInterfaceIPAddresses(b.bridgeName)
	if err != nil {
		return nil, err
	}

	// For DPU, here we need to use the DPU host's IP address which is the tenant cluster's
	// host internal IP address instead of the DPU's external bridge IP address.
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		nodeAddrStr, err := util.GetNodePrimaryIP(node)
		if err != nil {
			return nil, err
		}
		nodeAddr := net.ParseIP(nodeAddrStr)
		if nodeAddr == nil {
			return nil, fmt.Errorf("failed to parse node IP address. %v", nodeAddrStr)
		}
		ifAddrs, err = getDPUHostPrimaryIPAddresses(nodeAddr, ifAddrs)
		if err != nil {
			return nil, err
		}
	}

	b.ips = ifAddrs
	return ifAddrs, nil
}

func bridgeForInterface(intfName, nodeName, physicalNetworkName string, gwIPs []*net.IPNet) (*bridgeConfiguration, error) {
	defaultNetConfig := &bridgeUDNConfiguration{
		masqCTMark: ctMarkOVN,
	}
	res := bridgeConfiguration{
		nodeName: nodeName,
		netConfig: map[string]*bridgeUDNConfiguration{
			types.DefaultNetworkName: defaultNetConfig,
		},
	}
	gwIntf := intfName

	if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to find nic name for bridge %s: %w", bridgeName, err)
		}
		res.bridgeName = bridgeName
		res.uplinkName = uplinkName
		gwIntf = bridgeName
	} else if _, _, err := util.RunOVSVsctl("br-exists", intfName); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err := util.NicToBridge(intfName)
		if err != nil {
			return nil, fmt.Errorf("nicToBridge failed for %s: %w", intfName, err)
		}
		res.bridgeName = bridgeName
		res.uplinkName = intfName
		gwIntf = bridgeName
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err := getIntfName(intfName)
		if err != nil {
			if config.Gateway.Mode == config.GatewayModeLocal && config.Gateway.AllowNoUplink {
				klog.Infof("Could not find uplink for %s, setup gateway bridge with no uplink port, egress IP and egress GW will not work", intfName)
			} else {
				return nil, fmt.Errorf("failed to find intfName for %s: %w", intfName, err)
			}
		} else {
			res.uplinkName = uplinkName
		}
		res.bridgeName = intfName
	}
	var err error
	// Now, we get IP addresses for the bridge
	if len(gwIPs) > 0 {
		// use gwIPs if provided
		res.ips = gwIPs
	} else {
		// get IP addresses from OVS bridge. If IP does not exist,
		// error out.
		res.ips, err = getNetworkInterfaceIPAddresses(gwIntf)
		if err != nil {
			return nil, fmt.Errorf("failed to get interface details for %s: %w", gwIntf, err)
		}
	}

	res.macAddress, err = util.GetOVSPortMACAddress(gwIntf)
	if err != nil {
		return nil, fmt.Errorf("failed to get MAC address for ovs port %s: %w", gwIntf, err)
	}

	res.interfaceID, err = bridgedGatewayNodeSetup(nodeName, res.bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	defaultNetConfig.patchPort = (&util.DefaultNetInfo{}).GetNetworkScopedPatchPortName(res.bridgeName, nodeName)

	// for DPU we use the host MAC address for the Gateway configuration
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		hostRep, err := util.GetDPUHostInterface(res.bridgeName)
		if err != nil {
			return nil, err
		}
		res.macAddress, err = util.GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
		if err != nil {
			return nil, err
		}
	}

	return &res, nil
}
