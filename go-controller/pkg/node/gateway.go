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
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Gateway responds to Service and Endpoint K8s events
// and programs OVN gateway functionality.
// It may also spawn threads to ensure the flow tables
// are kept in sync
type Gateway interface {
	informer.ServiceAndEndpointsEventHandler
	Init(factory.NodeWatchFactory) error
	Start(<-chan struct{}, *sync.WaitGroup)
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
}

func (g *gateway) AddService(svc *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.AddService(svc)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.AddService(svc)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.AddService(svc)
	}
	if g.nodePortWatcherIptables != nil {
		g.nodePortWatcherIptables.AddService(svc)
	}
}

func (g *gateway) UpdateService(old, new *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.UpdateService(old, new)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.UpdateService(old, new)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.UpdateService(old, new)
	}
	if g.nodePortWatcherIptables != nil {
		g.nodePortWatcherIptables.UpdateService(old, new)
	}
}

func (g *gateway) DeleteService(svc *kapi.Service) {
	if g.portClaimWatcher != nil {
		g.portClaimWatcher.DeleteService(svc)
	}
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.DeleteService(svc)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.DeleteService(svc)
	}
	if g.nodePortWatcherIptables != nil {
		g.nodePortWatcherIptables.DeleteService(svc)
	}
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

func (g *gateway) AddEndpointSlice(epSlice *discovery.EndpointSlice) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.AddEndpointSlice(epSlice)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.AddEndpointSlice(epSlice)
	}
}

func (g *gateway) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.UpdateEndpointSlice(oldEpSlice, newEpSlice)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.UpdateEndpointSlice(oldEpSlice, newEpSlice)
	}
}

func (g *gateway) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) {
	if g.loadBalancerHealthChecker != nil {
		g.loadBalancerHealthChecker.DeleteEndpointSlice(epSlice)
	}
	if g.nodePortWatcher != nil {
		g.nodePortWatcher.DeleteEndpointSlice(epSlice)
	}
}

func (g *gateway) Init(wf factory.NodeWatchFactory) error {
	err := g.initFunc()
	if err != nil {
		return err
	}
	_, err = wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			g.AddService(svc)
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			g.UpdateService(oldSvc, newSvc)
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			g.DeleteService(svc)
		},
	}, g.SyncServices)
	if err != nil {
		klog.Errorf("Gateway init failed to add service handler: %v", err)
		return err
	}

	_, err = wf.AddEndpointSliceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			epSlice := obj.(*discovery.EndpointSlice)
			g.AddEndpointSlice(epSlice)
		},
		UpdateFunc: func(old, new interface{}) {
			oldEpSlice := old.(*discovery.EndpointSlice)
			newEpSlice := new.(*discovery.EndpointSlice)
			g.UpdateEndpointSlice(oldEpSlice, newEpSlice)
		},
		DeleteFunc: func(obj interface{}) {
			epSlice := obj.(*discovery.EndpointSlice)
			g.DeleteEndpointSlice(epSlice)
		},
	}, nil)
	if err != nil {
		klog.Errorf("Gateway init failed to add endpoints handler: %v", err)
		return err
	}
	return nil
}

func (g *gateway) Start(stopChan <-chan struct{}, wg *sync.WaitGroup) {
	if g.nodeIPManager != nil {
		g.nodeIPManager.Run(stopChan, wg)
	}

	if g.openflowManager != nil {
		klog.Info("Spawning Conntrack Rule Check Thread")
		g.openflowManager.Run(stopChan, wg)
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
		return nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", gwIntf)
	}
	var egressGWBridge *bridgeConfiguration
	if egressGatewayIntf != "" {
		egressGWBridge, err = bridgeForInterface(egressGatewayIntf, nodeName, types.PhysicalNetworkExGwName, nil)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "Bridge for interface failed for %s", egressGatewayIntf)
		}
	}

	chassisID, err := util.GetNodeChassisID()
	if err != nil {
		return nil, nil, err
	}

	if !config.Gateway.DisablePacketMTUCheck {
		chkPktLengthSupported, err := util.DetectCheckPktLengthSupport(gatewayBridge.bridgeName)
		if err != nil {
			return nil, nil, err
		}

		if !chkPktLengthSupported {
			klog.Warningf("OVS on this node does not support check packet length action in kernel datapath. This "+
				"will cause incoming packets destined to OVN and larger than pod MTU: %d to the node, being dropped "+
				"without sending fragmentation needed", config.Default.MTU)
			config.Gateway.DisablePacketMTUCheck = true
		}
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
	return g.openflowManager.defaultBridge.bridgeName
}

// getMaxFrameLength returns the maximum frame size (ignoring VLAN header) that a gateway can handle
func getMaxFrameLength() int {
	return config.Default.MTU + 14
}

type bridgeConfiguration struct {
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

func bridgeForInterface(intfName, nodeName, physicalNetworkName string, gwIPs []*net.IPNet) (*bridgeConfiguration, error) {
	res := bridgeConfiguration{}
	gwIntf := intfName
	bridgeCreated := false

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
		bridgeCreated = true
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
	if len(gwIPs) > 0 {
		// use gwIPs if provided
		res.ips = gwIPs
	} else {
		// get IP addresses from OVS bridge. If IP does not exist,
		// error out.
		res.ips, err = getNetworkInterfaceIPAddresses(gwIntf)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get interface details for %s", gwIntf)
		}
	}

	res.interfaceID, res.macAddress, err = bridgedGatewayNodeSetup(nodeName, res.bridgeName, gwIntf,
		physicalNetworkName, bridgeCreated)
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
