//go:build linux
// +build linux

package node

import (
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

func newRoutingViaOVNGateway(nodeName string, subnets []*net.IPNet, gwNextHops []net.IP, gwIntf, egressGWIntf string,
	gwIPs []*net.IPNet, nodeAnnotator kube.Annotator, kube kube.Interface, cfg *managementPortConfig, watchFactory factory.NodeWatchFactory) (*gateway, error) {
	klog.Info("Creating new gateway that will perform routing directly via OVN")
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

	gw.initFunc = func() error {
		// Program cluster.GatewayIntf to let non-pod traffic to go to host
		// stack
		klog.Info("Creating Routing Via OVN Gateway Openflow Manager")
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
		gw.openflowManager, err = newRoutingViaOVNGatewayOpenFlowManager(gwBridge, exGwBridge)
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

	klog.Info("Routing Via OVN Gateway Creation Complete")
	return gw, nil
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//    the return traffic can be steered back to OVN logical topology
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newRoutingViaOVNGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration) (*openflowManager, error) {
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
