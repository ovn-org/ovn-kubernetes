package node

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

type openflowManager struct {
	defaultBridge         *bridgeConfiguration
	externalGatewayBridge *bridgeConfiguration
	// flow cache, use map instead of array for readability when debugging
	flowCache     map[string][]string
	flowMutex     sync.Mutex
	exGWFlowCache map[string][]string
	exGWFlowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan chan struct{}
}

// UTILs Needed for UDN (also leveraged for default netInfo) in openflowmanager

func (c *openflowManager) getDefaultBridgePortConfigurations() ([]bridgeUDNConfiguration, string, string) {
	return c.defaultBridge.getBridgePortConfigurations()
}

func (c *openflowManager) getExGwBridgePortConfigurations() ([]bridgeUDNConfiguration, string, string) {
	return c.externalGatewayBridge.getBridgePortConfigurations()
}

func (c *openflowManager) addNetwork(nInfo util.NetInfo, masqCTMark uint, v4MasqIPs, v6MasqIPs *udn.MasqueradeIPs) {
	c.defaultBridge.addNetworkBridgeConfig(nInfo, masqCTMark, v4MasqIPs, v6MasqIPs)
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.addNetworkBridgeConfig(nInfo, masqCTMark, v4MasqIPs, v6MasqIPs)
	}
}

func (c *openflowManager) delNetwork(nInfo util.NetInfo) {
	c.defaultBridge.delNetworkBridgeConfig(nInfo)
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.delNetworkBridgeConfig(nInfo)
	}
}

func (c *openflowManager) getActiveNetwork(nInfo util.NetInfo) *bridgeUDNConfiguration {
	return c.defaultBridge.getActiveNetworkBridgeConfig(nInfo)
}

// END UDN UTILs

func (c *openflowManager) getDefaultBridgeName() string {
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()
	return c.defaultBridge.bridgeName
}

func (c *openflowManager) getDefaultBridgeMAC() net.HardwareAddr {
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()
	return c.defaultBridge.macAddress
}

func (c *openflowManager) setDefaultBridgeMAC(macAddr net.HardwareAddr) {
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()
	c.defaultBridge.macAddress = macAddr
}

func (c *openflowManager) updateFlowCacheEntry(key string, flows []string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	c.flowCache[key] = flows
}

func (c *openflowManager) deleteFlowsByKey(key string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	delete(c.flowCache, key)
}

func (c *openflowManager) updateExBridgeFlowCacheEntry(key string, flows []string) {
	c.exGWFlowMutex.Lock()
	defer c.exGWFlowMutex.Unlock()
	c.exGWFlowCache[key] = flows
}

func (c *openflowManager) requestFlowSync() {
	select {
	case c.flowChan <- struct{}{}:
		klog.V(5).Infof("Gateway OpenFlow sync requested")
	default:
		klog.V(5).Infof("Gateway OpenFlow sync already requested")
	}
}

func (c *openflowManager) syncFlows() {
	// protect gwBridge config from being updated by gw.nodeIPManager
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()

	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()

	flows := []string{}
	for _, entry := range c.flowCache {
		flows = append(flows, entry...)
	}

	_, stderr, err := util.ReplaceOFFlows(c.defaultBridge.bridgeName, flows)
	if err != nil {
		klog.Errorf("Failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, c.flowCache)
	}

	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.Lock()
		defer c.externalGatewayBridge.Unlock()

		c.exGWFlowMutex.Lock()
		defer c.exGWFlowMutex.Unlock()

		flows := []string{}
		for _, entry := range c.exGWFlowCache {
			flows = append(flows, entry...)
		}

		_, stderr, err := util.ReplaceOFFlows(c.externalGatewayBridge.bridgeName, flows)
		if err != nil {
			klog.Errorf("Failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, c.exGWFlowCache)
		}
	}
}

// since we share the host's k8s node IP, add OpenFlow flows
// -- to steer the NodePort traffic arriving on the host to the OVN logical topology and
// -- to also connection track the outbound north-south traffic through l3 gateway so that
//
//	the return traffic can be steered back to OVN logical topology
//
// -- to handle host -> service access, via masquerading from the host to OVN GR
// -- to handle external -> service(ExternalTrafficPolicy: Local) -> host access without SNAT
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration, subnets []*net.IPNet, extraIPs []net.IP) (*openflowManager, error) {
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

	if err := ofm.updateBridgeFlowCache(subnets, extraIPs); err != nil {
		return nil, err
	}

	// defer flowSync until syncService() to prevent the existing service OpenFlows being deleted
	return ofm, nil
}

// Run starts OpenFlow Manager which will constantly sync flows for managed OVS bridges
func (c *openflowManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		syncPeriod := 15 * time.Second
		timer := time.NewTicker(syncPeriod)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:

				if err := checkPorts(c.getDefaultBridgePortConfigurations()); err != nil {
					klog.Errorf("Checkports failed %v", err)
					continue
				}

				if c.externalGatewayBridge != nil {
					if err := checkPorts(c.getExGwBridgePortConfigurations()); err != nil {
						klog.Errorf("Checkports failed %v", err)
						continue
					}
				}
				c.syncFlows()
			case <-c.flowChan:
				c.syncFlows()
				timer.Reset(syncPeriod)
			case <-stopChan:
				return
			}
		}
	}()
}

// updateBridgeFlowCache generates the "static" per-bridge flows
// note: this is shared between shared and local gateway modes
func (c *openflowManager) updateBridgeFlowCache(subnets []*net.IPNet, extraIPs []net.IP) error {
	// protect defaultBridge config from being updated by gw.nodeIPManager
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()

	// CAUTION: when adding new flows where the in_port is ofPortPatch and the out_port is ofPortPhys, ensure
	// that dl_src is included in match criteria!

	dftFlows, err := flowsForDefaultBridge(c.defaultBridge, extraIPs)
	if err != nil {
		return err
	}
	dftCommonFlows, err := commonFlows(subnets, c.defaultBridge)
	if err != nil {
		return err
	}
	dftFlows = append(dftFlows, dftCommonFlows...)

	c.updateFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	c.updateFlowCacheEntry("DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.Lock()
		defer c.externalGatewayBridge.Unlock()
		c.updateExBridgeFlowCacheEntry("NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		exGWBridgeDftFlows, err := commonFlows(subnets, c.externalGatewayBridge)
		if err != nil {
			return err
		}
		c.updateExBridgeFlowCacheEntry("DEFAULT", exGWBridgeDftFlows)
	}
	return nil
}

func checkPorts(netConfigs []bridgeUDNConfiguration, physIntf, ofPortPhys string) error {
	// it could be that the ovn-controller recreated the patch between the host OVS bridge and
	// the integration bridge, as a result the ofport number changed for that patch interface
	for _, netConfig := range netConfigs {
		if netConfig.ofPortPatch == "" {
			continue
		}
		curOfportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get", "Interface", netConfig.patchPort, "ofport")
		if err != nil {
			return fmt.Errorf("failed to get ofport of %s, stderr: %q: %w", netConfig.patchPort, stderr, err)

		}
		if netConfig.ofPortPatch != curOfportPatch {
			if netConfig.isDefaultNetwork() || curOfportPatch != "" {
				klog.Errorf("Fatal error: patch port %s ofport changed from %s to %s",
					netConfig.patchPort, netConfig.ofPortPatch, curOfportPatch)
				os.Exit(1)
			} else {
				klog.Warningf("Patch port %s removed for existing network", netConfig.patchPort)
			}
		}
	}

	// it could be that someone removed the physical interface and added it back on the OVS host
	// bridge, as a result the ofport number changed for that physical interface
	curOfportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get", "interface", physIntf, "ofport")
	if err != nil {
		return fmt.Errorf("failed to get ofport of %s, stderr: %q: %w", physIntf, stderr, err)
	}
	if ofPortPhys != curOfportPhys {
		klog.Errorf("Fatal error: phys port %s ofport changed from %s to %s",
			physIntf, ofPortPhys, curOfportPhys)
		os.Exit(1)
	}
	return nil
}

// bootstrapOVSFlows handles ensuring basic, required flows are in place. This is done before OpenFlow manager has
// been created/started, and only done when there is just a NORMAL flow programmed and OVN/OVS is already setup
func bootstrapOVSFlows(nodeName string) error {
	// see if patch port exists already
	var portsOutput string
	var stderr string
	var err error
	if portsOutput, stderr, err = util.RunOVSVsctl("--no-heading", "--data=bare", "--format=csv", "--columns",
		"name", "list", "interface"); err != nil {
		// bridge exists, but could not list ports
		return fmt.Errorf("failed to list ports on existing bridge br-int: %s, %w", stderr, err)
	}

	bridge, patchPort := localnetPortInfo(nodeName, portsOutput)

	if len(bridge) == 0 {
		// bridge exists but no patch port was found
		return nil
	}

	// get the current flows and if there is more than just default flow, we dont need to bootstrap as we already
	// have flows
	flows, err := util.GetOFFlows(bridge)
	if err != nil {
		return err
	}
	if len(flows) > 1 {
		// more than 1 flow, assume the OVS has retained previous flows from previous running OVNK instance
		return nil
	}

	// only have 1 flow, need to install required flows
	klog.Infof("Default NORMAL flow installed on OVS bridge: %s, will bootstrap with required port security flows", bridge)

	// Get ofport of patchPort
	ofportPatch, stderr, err := util.GetOVSOfPort("get", "Interface", patchPort, "ofport")
	if err != nil {
		return fmt.Errorf("failed while waiting on patch port %q to be created by ovn-controller and "+
			"while getting ofport. stderr: %q, error: %v", patchPort, stderr, err)
	}

	var bridgeMACAddress net.HardwareAddr
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		hostRep, err := util.GetDPUHostInterface(bridge)
		if err != nil {
			return err
		}
		bridgeMACAddress, err = util.GetSriovnetOps().GetRepresentorPeerMacAddress(hostRep)
		if err != nil {
			return err
		}
	} else {
		bridgeMACAddress, err = util.GetOVSPortMACAddress(bridge)
		if err != nil {
			return fmt.Errorf("failed to get MAC address for ovs port %s: %w", bridge, err)
		}
	}

	var dftFlows []string
	// table 0, check packets coming from OVN have the correct mac address. Low priority flows that are a catch all
	// for non-IP packets that would normally be forwarded with NORMAL action (table 0, priority 0 flow).
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=10, table=0, in_port=%s, dl_src=%s, actions=output:NORMAL",
			defaultOpenFlowCookie, ofportPatch, bridgeMACAddress))
	dftFlows = append(dftFlows,
		fmt.Sprintf("cookie=%s, priority=9, table=0, in_port=%s, actions=drop",
			defaultOpenFlowCookie, ofportPatch))
	dftFlows = append(dftFlows, "priority=0, table=0, actions=output:NORMAL")

	_, stderr, err = util.ReplaceOFFlows(bridge, dftFlows)
	if err != nil {
		return fmt.Errorf("failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, dftFlows)
	}

	return nil
}

// localnetPortInfo returns the name of the bridge and the patch port name for the default cluster network
func localnetPortInfo(nodeName string, portsOutput string) (string, string) {
	// This needs to work with:
	// - default network: patch-<bridge name>_<node>-to-br-int
	// but not with:
	// - user defined primary network: patch-<bridge name>_<network-name>_<node>-to-br-int
	// - user defined secondary localnet network: patch-<bridge name>_<network-name>_ovn_localnet_port-to-br-int
	// TODO: going forward, maybe it would preferable to just read the bridge name from the config.
	r := regexp.MustCompile(fmt.Sprintf("^patch-([^_]*)_%s-to-br-int$", nodeName))
	for _, line := range strings.Split(portsOutput, "\n") {
		matches := r.FindStringSubmatch(line)
		if len(matches) == 2 {
			return matches[1], matches[0]
		}
	}
	return "", ""
}
