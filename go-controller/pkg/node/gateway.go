package node

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	"github.com/safchain/ethtool"
	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init() error
	Start()
	GetGatewayBridgeIface() string
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
	return apierrors.NewAggregate(errors)
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
	return apierrors.NewAggregate(errors)
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
	return apierrors.NewAggregate(errors)
}

func (g *gateway) SyncServices(objs []interface{}) error {
	var err error
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
	return apierrors.NewAggregate(errors)

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
	return apierrors.NewAggregate(errors)

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
	return apierrors.NewAggregate(errors)

}

func (g *gateway) Init() error {
	var err error
	if err = g.initFunc(); err != nil {
		return err
	}
	servicesRetryFramework := g.newRetryFrameworkNode(factory.ServiceForGatewayType)
	if _, err = servicesRetryFramework.WatchResource(); err != nil {
		return fmt.Errorf("gateway init failed to start watching services: %v", err)
	}

	endpointSlicesRetryFramework := g.newRetryFrameworkNode(factory.EndpointSliceForGatewayType)
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

func gatewayInitInternal(nc *DefaultNodeNetworkController, nodeAnnotator kube.Annotator) (*gateway,
	*bridgeConfiguration, *bridgeConfiguration, error) {
	gwNextHops, gwIface, err := getGatewayNextHops()
	if err != nil {
		return nil, nil, nil, err
	}

	egressGatewayIntf := ""
	if config.Gateway.EgressGWInterface != "" {
		egressGatewayIntf = interfaceForEXGW(config.Gateway.EgressGWInterface)
	}

	var kubeNodeIP net.IP
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		kubeNodeIP, err = getNodePrimaryIP(nc)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	gatewayBridge, err := bridgeForInterface(gwIface, nc.name, types.PhysicalNetworkName, kubeNodeIP)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", gwIface)
	}
	var egressGWBridge *bridgeConfiguration
	if egressGatewayIntf != "" {
		egressGWBridge, err = bridgeForInterface(egressGatewayIntf, nc.name, types.PhysicalNetworkExGwName, nil)
		if err != nil {
			return nil, nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", egressGatewayIntf)
		}
	}

	gw := &gateway{
		stopChan:     nc.stopChan,
		wg:           nc.wg,
		watchFactory: nc.watchFactory.(*factory.WatchFactory),
	}

	if err := util.SetNodePrimaryIfAddrs(nodeAnnotator, gatewayBridge.ips); err != nil {
		klog.Errorf("Unable to set primary IP net label on node, err: %v", err)
	}

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, nil, nil, err
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
			return nil, nil, nil, err
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
				return nil, nil, nil, err
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
		return nil, nil, nil, err
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
	return gw, gatewayBridge, egressGWBridge, err
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
	return g.openflowManager.defaultBridge.bridgeName
}

type bridgeConfiguration struct {
	sync.Mutex
	bridgeName  string
	uplinkName  string
	ips         []*net.IPNet
	interfaceID string
	macAddress  net.HardwareAddr
	patchPort   string
	ofPortPatch string
	ofPortPhys  string
	ofPortHost  string
}

// updateInterfaceIPAddresses sets and returns the bridge's current ips
func (b *bridgeConfiguration) updateInterfaceIPAddresses() ([]*net.IPNet, error) {
	b.Lock()
	defer b.Unlock()
	ifAddrs, err := getNetworkInterfaceIPAddresses(b.bridgeName)
	if err == nil {
		b.ips = ifAddrs
	}
	return ifAddrs, err
}

func bridgeForInterface(intfName, nodeName, physicalNetworkName string, kubeNodeIP net.IP) (*bridgeConfiguration, error) {
	res := bridgeConfiguration{}
	gwIntf := intfName

	if bridgeName, _, err := util.RunOVSVsctl("port-to-br", intfName); err == nil {
		// This is an OVS bridge's internal port
		uplinkName, err := util.GetNicName(bridgeName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to find nic name for bridge %s", bridgeName)
		}
		res.bridgeName = bridgeName
		res.uplinkName = uplinkName
	} else if _, _, err := util.RunOVSVsctl("br-exists", intfName); err != nil {
		// This is not a OVS bridge. We need to create a OVS bridge
		// and add cluster.GatewayIntf as a port of that bridge.
		bridgeName, err := util.NicToBridge(intfName)
		if err != nil {
			return nil, errors.Wrapf(err, "NicToBridge failed for %s", intfName)
		}
		res.bridgeName = bridgeName
		res.uplinkName = intfName
		gwIntf = bridgeName
	} else {
		// gateway interface is an OVS bridge
		uplinkName, err := getIntfName(intfName)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to find intfName for %s", intfName)
		}
		res.bridgeName = intfName
		res.uplinkName = uplinkName
	}
	var err error
	// Now, we get IP addresses for the bridge
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		res.ips, err = getNetworkInterfaceIPAddresses(gwIntf)
	} else {
		res.ips, err = getDPUHostPrimaryIPAddresses(gwIntf, kubeNodeIP)
	}
	if err != nil {
		return nil, err
	}

	res.macAddress, err = util.GetOVSPortMACAddress(gwIntf)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get MAC address for ovs port %s", gwIntf)
	}

	res.interfaceID, err = bridgedGatewayNodeSetup(nodeName, res.bridgeName, physicalNetworkName)
	if err != nil {
		return nil, fmt.Errorf("failed to set up shared interface gateway: %v", err)
	}

	// the name of the patch port created by ovn-controller is of the form
	// patch-<logical_port_name_of_localnet_port>-to-br-int
	res.patchPort = "patch-" + res.bridgeName + "_" + nodeName + "-to-br-int"

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
