package node

import (
	"context"
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iprulemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler
	// stores the networkID of this network
	networkID *int
	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for the given secondary network. It supports layer3, layer2 and
// localnet topology types.
func NewSecondaryNodeNetworkController(
	cnnci *CommonNodeNetworkControllerInfo,
	netInfo util.NetInfo,
	vrfManager *vrfmanager.Controller,
	ruleManager *iprulemanager.Controller,
	defaultNetworkGateway Gateway,
) (*SecondaryNodeNetworkController, error) {

	snnc := &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             util.NewReconcilableNetInfo(netInfo),
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
		},
	}
	if util.IsNetworkSegmentationSupportEnabled() && snnc.IsPrimaryNetwork() {
		node, err := snnc.watchFactory.GetNode(snnc.name)
		if err != nil {
			return nil, fmt.Errorf("error retrieving node %s while creating node network controller for network %s: %v",
				snnc.name, netInfo.GetNetworkName(), err)
		}
		networkID, err := snnc.getNetworkID()
		if err != nil {
			return nil, fmt.Errorf("error retrieving network id for network %s: %v", netInfo.GetNetworkName(), err)
		}

		snnc.gateway, err = NewUserDefinedNetworkGateway(snnc.GetNetInfo(), networkID, node,
			snnc.watchFactory.NodeCoreInformer().Lister(), snnc.Kube, vrfManager, ruleManager, defaultNetworkGateway)
		if err != nil {
			return nil, fmt.Errorf("error creating UDN gateway for network %s: %v", netInfo.GetNetworkName(), err)
		}
	}
	return snnc, nil
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())

	// enable adding ovs ports for dpu pods in both primary and secondary user defined networks
	if (config.OVNKubernetesFeature.EnableMultiNetwork || util.IsNetworkSegmentationSupportEnabled()) && config.OvnKubeNode.Mode == types.NodeModeDPU {
		handler, err := nc.watchPodsDPU()
		if err != nil {
			return err
		}
		nc.podHandler = handler
	}
	if util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() {
		if err := nc.gateway.AddNetwork(); err != nil {
			return fmt.Errorf("failed to add network to node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}
	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	if nc.gateway != nil {
		return nc.gateway.DelNetwork()
	}
	return nil
}

func (oc *SecondaryNodeNetworkController) getNetworkID() (int, error) {
	if oc.networkID == nil || *oc.networkID == util.InvalidID {
		oc.networkID = ptr.To(util.InvalidID)
		nodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			return util.InvalidID, err
		}
		*oc.networkID, err = util.GetNetworkID(nodes, oc.GetNetInfo())
		if err != nil {
			return util.InvalidID, err
		}
	}
	return *oc.networkID, nil
}

// Reconcile function reconciles three entities based on whether UDN network is advertised
// and the gateway mode:
// 1. iptables NAT rules for OVN-KUBE-UDN-MASQUERADE chain while using local gateway mode
// 2. IP rules
// 3. IP routes
// 4. OpenFlows on br-ex bridge to forward traffic to correct ofports
func (oc *SecondaryNodeNetworkController) Reconcile(netInfo util.NetInfo) error {
	err := util.ReconcileNetInfo(oc.ReconcilableNetInfo, netInfo)
	if err != nil {
		klog.Errorf("Failed to reconcile network %s: %v", oc.GetNetworkName(), err)
	}

	oc.gateway.isUDNNetworkAdvertised = oc.isPodNetworkAdvertisedAtNode()

	if err := oc.updateUDNSubnetReturnRule(); err != nil {
		return fmt.Errorf("error while updating iptables return rule for UDN %s: %s", oc.gateway.GetNetworkName(), err)
	}

	if err := oc.updateUDNVRFIPRule(); err != nil {
		return fmt.Errorf("error while updating ip rule for UDN %s: %s", oc.gateway.GetNetworkName(), err)
	}

	if err := oc.updateUDNFlow(); err != nil {
		return fmt.Errorf("error while updating logical flow for UDN %s: %s", oc.gateway.GetNetworkName(), err)
	}

	return nil
}

func (oc *SecondaryNodeNetworkController) isPodNetworkAdvertisedAtNode() bool {
	return util.IsPodNetworkAdvertisedAtNode(oc.gateway.NetInfo, oc.name)
}

// updateUDNSubnetReturnRule inserts below iptables return rule when UDN network
// is advertised, gateway mode is local and UDN subnet is 10.132.0.0/14:
// -A OVN-KUBE-UDN-MASQUERADE -s 10.132.0.0/14 -c 0 0 -j RETURN
func (oc *SecondaryNodeNetworkController) updateUDNSubnetReturnRule() error {
	if oc.gateway.isUDNNetworkAdvertised && config.Gateway.Mode == config.GatewayModeLocal {
		for _, udnSubnet := range oc.Subnets() {
			if err := insertIptRules(getUDNSubnetReturnRule(udnSubnet.CIDR,
				getIPTablesProtocol(udnSubnet.CIDR.IP.String()))); err != nil {
				return err
			}
		}
	} else {
		for _, udnSubnet := range oc.Subnets() {
			if err := deleteIptRules(getUDNSubnetReturnRule(udnSubnet.CIDR,
				getIPTablesProtocol(udnSubnet.CIDR.IP.String()))); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateUDNVRFIPRule modifies existing IP rule for an UDN nework when that particular
// network(10.132.0.0/14) is advertised:
// Existing IP rule after creation of UDN network:
// 2000:	from all fwmark 0x1001 lookup 1009
// 2000:	from all to 169.254.0.12 lookup 1009
// IP rule after the network is advertised:
// 2000:	from all fwmark 0x1001 lookup 1009
// 2000:	from all to 10.132.0.0/14 lookup 1009
func (oc *SecondaryNodeNetworkController) updateUDNVRFIPRule() error {
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(oc.gateway.networkID))
	mplink, err := util.LinkByName(interfaceName)
	if err != nil {
		return fmt.Errorf("unable to get link for %s, error: %v", interfaceName, err)
	}
	vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)

	if err = oc.gateway.ruleManager.DeleteWithMetadata(oc.gateway.GetNetworkRuleMetadata()); err != nil {
		return fmt.Errorf("unable to delete iprule for network %s, err: %v", oc.gateway.GetNetworkName(), err)
	}
	udnReplyIPRules, err := oc.gateway.constructUDNVRFIPRules(vrfTableId)
	if err != nil {
		return fmt.Errorf("unable to get iprules for network %s, err: %v", oc.gateway.GetNetworkName(), err)
	}
	for _, rule := range udnReplyIPRules {
		if err = oc.gateway.ruleManager.AddWithMetadata(rule, oc.gateway.GetNetworkRuleMetadata()); err != nil {
			return fmt.Errorf("unable to create iprule %v for network %s, err: %v", rule, oc.gateway.GetNetworkName(), err)
		}
	}
	return nil
}

// updateUDNFlow adds below OpenFlows based on the gateway mode and whether the network
// is advertised or not:
// table=1, n_packets=0, n_bytes=0, priority=16,ip,nw_dst=128.192.0.2 actions=LOCAL (Both gateway modes)
// table=1, n_packets=0, n_bytes=0, priority=15,ip,nw_dst=128.192.0.0/14 actions=output:3 (shared gateway mode)
func (oc *SecondaryNodeNetworkController) updateUDNFlow() error {
	node, err := oc.gateway.watchFactory.GetNode(oc.gateway.nodeIPManager.nodeName)
	if err != nil {
		return fmt.Errorf("unable to get node %s: %s", oc.gateway.nodeIPManager.nodeName, err)
	}
	subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed to get subnets for node: %s for OpenFlow cache update; err: %w", node.Name, err)
	}
	if err := oc.gateway.openflowManager.updateBridgeFlowCache(subnets, oc.gateway.nodeIPManager.ListAddresses(),
		oc.gateway.isPodNetworkAdvertised, oc.gateway.isUDNNetworkAdvertised); err != nil {
		return err
	}
	return nil
}
