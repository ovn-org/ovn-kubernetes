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
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/openflowmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

type openflowManager struct {
	defaultBridge         *bridgeConfiguration
	externalGatewayBridge *bridgeConfiguration
	ofManager             *openflowmanager.Controller
}

func (c *openflowManager) getDefaultBridgePorts() (string, string, string, string) {
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()
	return c.defaultBridge.patchPort, c.defaultBridge.ofPortPatch,
		c.defaultBridge.uplinkName, c.defaultBridge.ofPortPhys
}

func (c *openflowManager) getExGwBridgePorts() (string, string, string, string) {
	c.externalGatewayBridge.Lock()
	defer c.externalGatewayBridge.Unlock()
	return c.externalGatewayBridge.patchPort, c.externalGatewayBridge.ofPortPatch,
		c.externalGatewayBridge.uplinkName, c.externalGatewayBridge.ofPortPhys
}

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

func (c *openflowManager) requestFlowSync() {
	c.ofManager.RequestFlowSync(c.defaultBridge.bridgeName)
	if c.externalGatewayBridge != nil {
		c.ofManager.RequestFlowSync(c.externalGatewayBridge.bridgeName)
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
func newGatewayOpenFlowManager(gwBridge, exGWBridge *bridgeConfiguration, subnets []*net.IPNet, extraIPs []net.IP, ofManager *openflowmanager.Controller) (*openflowManager, error) {
	// add health check function to check default OpenFlow flows are on the shared gateway bridge
	ofm := &openflowManager{
		defaultBridge:         gwBridge,
		externalGatewayBridge: exGWBridge,
		ofManager:             ofManager,
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

				if err := checkPorts(c.getDefaultBridgePorts()); err != nil {
					klog.Errorf("Checkports failed %v", err)
					continue
				}

				if c.externalGatewayBridge != nil {
					if err := checkPorts(c.getExGwBridgePorts()); err != nil {
						klog.Errorf("Checkports failed %v", err)
						continue
					}
				}
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

	c.ofManager.UpdateFlowCacheEntry(c.defaultBridge.bridgeName, "NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
	c.ofManager.UpdateFlowCacheEntry(c.defaultBridge.bridgeName, "DEFAULT", dftFlows)

	// we consume ex gw bridge flows only if that is enabled
	if c.externalGatewayBridge != nil {
		c.externalGatewayBridge.Lock()
		defer c.externalGatewayBridge.Unlock()
		c.ofManager.UpdateFlowCacheEntry(c.externalGatewayBridge.bridgeName, "NORMAL", []string{fmt.Sprintf("table=0,priority=0,actions=%s\n", util.NormalAction)})
		exGWBridgeDftFlows, err := commonFlows(subnets, c.externalGatewayBridge)
		if err != nil {
			return err
		}
		c.ofManager.UpdateFlowCacheEntry(c.externalGatewayBridge.bridgeName, "DEFAULT", exGWBridgeDftFlows)
	}
	return nil
}

func checkPorts(patchIntf, ofPortPatch, physIntf, ofPortPhys string) error {
	// it could be that the ovn-controller recreated the patch between the host OVS bridge and
	// the integration bridge, as a result the ofport number changed for that patch interface
	curOfportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get", "Interface", patchIntf, "ofport")
	if err != nil {
		return fmt.Errorf("failed to get ofport of %s, stderr: %q: %w", patchIntf, stderr, err)

	}
	if ofPortPatch != curOfportPatch {
		klog.Errorf("Fatal error: patch port %s ofport changed from %s to %s",
			patchIntf, ofPortPatch, curOfportPatch)
		os.Exit(1)
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
func bootstrapOVSFlows() error {
	// see if patch port exists already
	var portsOutput string
	var stderr string
	var err error
	if portsOutput, stderr, err = util.RunOVSVsctl("--no-heading", "--data=bare", "--format=csv", "--columns",
		"name", "list", "interface"); err != nil {
		// bridge exists, but could not list ports
		return fmt.Errorf("failed to list ports on existing bridge br-int: %s, %w", stderr, err)
	}

	var bridge string
	var patchPort string
	// patch-br-int-to-<bridge name>_<node>
	r := regexp.MustCompile("^patch-(.*)_.*?-to-br-int$")
	for _, line := range strings.Split(portsOutput, "\n") {
		matches := r.FindStringSubmatch(line)
		if len(matches) == 2 {
			patchPort = matches[0]
			bridge = matches[1]
			break
		}
	}

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
