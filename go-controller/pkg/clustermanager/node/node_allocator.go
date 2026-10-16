// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"fmt"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	sharednode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// NodeAllocator acts on node events handed off by the cluster network
// controller and does the following:
//   - allocates subnet from the cluster subnet pool. It also allocates subnets
//     from the hybrid overlay subnet pool if hybrid overlay is enabled.
//     It stores these allocated subnets in the node annotation.
//     Only for the default or layer3 networks.
//   - stores the network id in each node's annotation.
type NodeAllocator struct {
	kube kube.Interface
	// nodeClient is used only for the rare live apiserver read needed to reconcile
	// against current server state before releasing subnets; all normal node reads
	// go through nodeLister.
	nodeClient kubernetes.Interface
	nodeLister listers.NodeLister
	// idAllocator of IDs within the network
	idAllocator                  id.Allocator
	clusterSubnetAllocator       SubnetAllocator
	hybridOverlaySubnetAllocator SubnetAllocator

	// unique id of the network
	networkID int

	netInfo util.NetInfo

	// nodeSubnets is a list of node subnets that are managed by the cluster subnet allocator
	nodeSubnets []*net.IPNet

	// batcher batches annotation updates across all networks for better API server efficiency
	batcher *NodeAnnotationBatcher
}

func NewNodeAllocator(networkID int, netInfo util.NetInfo, nodeLister listers.NodeLister, kube kube.Interface, nodeClient kubernetes.Interface, tunnelIDAllocator id.Allocator, batcher *NodeAnnotationBatcher) *NodeAllocator {
	na := &NodeAllocator{
		kube:                         kube,
		nodeClient:                   nodeClient,
		nodeLister:                   nodeLister,
		networkID:                    networkID,
		netInfo:                      netInfo,
		clusterSubnetAllocator:       NewSubnetAllocator(),
		hybridOverlaySubnetAllocator: NewSubnetAllocator(),
		idAllocator:                  tunnelIDAllocator,
		batcher:                      batcher,
	}

	if na.hasNodeSubnetAllocation() {
		na.clusterSubnetAllocator = NewSubnetAllocator()
	}

	if na.hasHybridOverlayAllocation() {
		na.hybridOverlaySubnetAllocator = NewSubnetAllocator()
	}

	return na
}

func (na *NodeAllocator) Init() error {
	if !na.hasNodeSubnetAllocation() {
		return nil
	}
	na.CleanupStaleAnnotation()

	clusterSubnets := na.netInfo.Subnets()

	for _, clusterSubnet := range clusterSubnets {
		if err := na.clusterSubnetAllocator.AddNetworkRange(clusterSubnet.CIDR, clusterSubnet.HostSubnetLength); err != nil {
			return err
		}
		klog.V(5).Infof("Added network range %s to cluster subnet allocator", clusterSubnet.CIDR)
	}

	if na.hasHybridOverlayAllocation() {
		for _, hoSubnet := range config.HybridOverlay.ClusterSubnets {
			if err := na.hybridOverlaySubnetAllocator.AddNetworkRange(hoSubnet.CIDR, hoSubnet.HostSubnetLength); err != nil {
				return err
			}
			klog.V(5).Infof("Added network range %s to hybrid overlay subnet allocator", hoSubnet.CIDR)
		}
	}

	// update metrics for cluster subnets
	na.recordSubnetCount()

	return nil
}

// CleanupStaleAnnotation cleans up the stale annotations on all nodes.
// If an error occurs, it logs the error and continues to the next node.
func (na *NodeAllocator) CleanupStaleAnnotation() {
	// only cleanup once with the default network NodeAllocator
	if !na.netInfo.IsDefault() {
		return
	}
	existingNodes, err := na.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Error in retrieving the nodes: %v", err)
		return
	}

	for _, node := range existingNodes {
		if _, ok := node.Annotations[util.OVNNodeGRLRPAddrs]; !ok {
			continue
		}
		// to cleanup an annotation, set it to nil
		if err = na.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{util.OVNNodeGRLRPAddrs: nil}); err != nil {
			klog.Warningf("Failed to clear node %s annotation %s: %v",
				node.Name, util.OVNNodeGRLRPAddrs, err)
		}
	}
}

// AddSubnets adds new subnet ranges to the node allocator
// It is used when a new subnet is added to a secondary layer3 network by updating UDN or NAD
func (na *NodeAllocator) AddSubnets(subnets []config.CIDRNetworkEntry) error {
	if !na.hasNodeSubnetAllocation() {
		return nil
	}

	for _, subnet := range subnets {
		if err := na.clusterSubnetAllocator.AddNetworkRange(subnet.CIDR, subnet.HostSubnetLength); err != nil {
			return fmt.Errorf("failed to add network range %s/%d: %w", subnet.CIDR.String(), subnet.HostSubnetLength, err)
		}
		klog.V(5).Infof("Added new network range %s/%d to cluster subnet allocator for network %s",
			subnet.CIDR.String(), subnet.HostSubnetLength, na.netInfo.GetNetworkName())
	}
	na.recordSubnetCount()

	return nil
}

func (na *NodeAllocator) hasHybridOverlayAllocation() bool {
	// When config.HybridOverlay.ClusterSubnets is empty, assume the subnet allocation will be managed by an external component.
	return config.HybridOverlay.Enabled && !na.netInfo.IsUserDefinedNetwork() && len(config.HybridOverlay.ClusterSubnets) > 0
}

func (na *NodeAllocator) hasHybridOverlayAllocationUnmanaged() bool {
	return config.HybridOverlay.Enabled && !na.netInfo.IsUserDefinedNetwork() && len(config.HybridOverlay.ClusterSubnets) == 0
}

func (na *NodeAllocator) recordSubnetCount() {
	// only for L3 networks
	if na.hasNodeSubnetAllocation() {
		v4count, v6count := na.clusterSubnetAllocator.Count()
		metrics.RecordSubnetCount(float64(v4count), float64(v6count), na.netInfo.GetNetworkName())
	}
}

func (na *NodeAllocator) recordSubnetUsage() {
	// only for L3 networks
	if na.hasNodeSubnetAllocation() {
		v4used, v6used := na.clusterSubnetAllocator.Usage()
		metrics.RecordSubnetUsage(float64(v4used), float64(v6used), na.netInfo.GetNetworkName())
	}
}

// hybridOverlayNodeEnsureSubnet allocates a subnet and sets the
// hybrid overlay subnet annotation. It returns any newly allocated subnet
// or an error. If an error occurs, the newly allocated subnet will be released.
func (na *NodeAllocator) hybridOverlayNodeEnsureSubnet(node *corev1.Node, annotator kube.Annotator) (*net.IPNet, error) {
	var existingSubnets []*net.IPNet
	// Do not allocate a subnet if the node already has one
	subnet, err := houtil.ParseHybridOverlayHostSubnet(node)
	if err != nil {
		// Log the error and try to allocate new subnets
		klog.Warningf("Failed to get node %s hybrid overlay subnet annotation: %v", node.Name, err)
	} else if subnet != nil {
		existingSubnets = []*net.IPNet{subnet}
	}

	// Allocate a new host subnet for this node
	// FIXME: hybrid overlay is only IPv4 for now due to limitations on the Windows side
	hostSubnets, allocatedSubnets, err := na.allocateNodeSubnets(na.hybridOverlaySubnetAllocator, node.Name, existingSubnets, true, false)
	if err != nil {
		return nil, fmt.Errorf("error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}

	if err := annotator.Set(hotypes.HybridOverlayNodeSubnet, hostSubnets[0].String()); err != nil {
		if e := na.hybridOverlaySubnetAllocator.ReleaseNetworks(node.Name, allocatedSubnets...); e != nil {
			klog.Warningf("Failed to release hybrid over subnet for the node %s from the allocator: %v", node.Name, e)
		}
		return nil, fmt.Errorf("error setting hybrid overlay host subnet: %w", err)
	}

	return hostSubnets[0], nil
}

func (na *NodeAllocator) releaseHybridOverlayNodeSubnet(nodeName string) {
	na.hybridOverlaySubnetAllocator.ReleaseAllNetworks(nodeName)
	klog.Infof("Deleted hybrid overlay HostSubnets for node %s", nodeName)
}

// NeedsNodeAllocationWithState determines if the annotations assigned by
// NodeAllocator are missing on a node using pre-parsed node annotation state.
func (na *NodeAllocator) NeedsNodeAllocationWithState(node *corev1.Node, state *sharednode.NodeAnnotationState) bool {
	if util.NoHostSubnet(node) {
		if na.hasHybridOverlayAllocation() {
			if _, ok := node.Annotations[hotypes.HybridOverlayNodeSubnet]; !ok {
				return true
			}
		}
		return false
	}

	if na.hasNodeSubnetAllocation() {
		hostSubnets, err := state.Subnets(na.netInfo.GetNetworkName())
		if err != nil {
			return true
		}
		if !na.hasExpectedHostSubnets(hostSubnets) {
			return true
		}
	}

	if na.HasNodeTunnelIDAllocation() {
		if _, err := state.TunnelID(na.netInfo.GetNetworkName()); err != nil {
			return true
		}
	}

	return false
}

// hasExpectedHostSubnets returns true when the node-subnets annotation matches
// the current IP-family mode for this network. During single-stack/dual-stack
// conversion we may have a stale annotation that still exists, but no longer
// satisfies the configured families, so allocation must run again.
func (na *NodeAllocator) hasExpectedHostSubnets(hostSubnets []*net.IPNet) bool {
	ipv4Mode, ipv6Mode := na.netInfo.IPMode()
	foundIPv4 := false
	foundIPv6 := false

	for _, subnet := range hostSubnets {
		switch {
		case utilnet.IsIPv4CIDR(subnet):
			if !ipv4Mode || foundIPv4 {
				return false
			}
			foundIPv4 = true
		case utilnet.IsIPv6CIDR(subnet):
			if !ipv6Mode || foundIPv6 {
				return false
			}
			foundIPv6 = true
		default:
			return false
		}
	}

	return foundIPv4 == ipv4Mode && foundIPv6 == ipv6Mode
}

// NeedsNodeCleanup determines if node annotations or allocator state exist for this network.
func (na *NodeAllocator) NeedsNodeCleanup(node *corev1.Node) (bool, error) {
	if util.NoHostSubnet(node) {
		return false, nil
	}

	networkName := na.netInfo.GetNetworkName()
	if util.HasNodeHostSubnetAnnotation(node, networkName) {
		return true, nil
	}

	if _, err := util.ParseNetworkIDAnnotation(node, networkName); err == nil {
		return true, nil
	} else if !util.IsAnnotationNotSetError(err) {
		return false, fmt.Errorf("failed to parse node %q network id annotation for network %s: %w",
			node.Name, networkName, err)
	}

	if util.IsNetworkSegmentationSupportEnabled() && na.netInfo.IsPrimaryNetwork() && util.DoesNetworkRequireTunnelIDs(na.netInfo) {
		if _, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, networkName); err == nil {
			return true, nil
		} else if !util.IsAnnotationNotSetError(err) {
			return false, fmt.Errorf("failed to parse node %q tunnel id annotation for network %s: %w",
				node.Name, networkName, err)
		}
		if na.idAllocator != nil && na.idAllocator.GetID(networkName+"_"+node.Name) != types.InvalidID {
			return true, nil
		}
	}

	return false, nil
}

// HandleAddUpdateNodeEvent handles the add or update node event
func (na *NodeAllocator) HandleAddUpdateNodeEvent(node *corev1.Node) error {
	defer na.recordSubnetUsage()

	if util.NoHostSubnet(node) {
		if na.hasHybridOverlayAllocation() {
			annotator := kube.NewNodeAnnotator(na.kube, node.Name)
			allocatedSubnet, err := na.hybridOverlayNodeEnsureSubnet(node, annotator)
			if err != nil {
				return fmt.Errorf("failed to update node %s hybrid overlay subnet annotation: %v", node.Name, err)
			}
			if err := annotator.Run(); err != nil {
				// Release allocated subnet if any errors occurred
				if allocatedSubnet != nil {
					na.releaseHybridOverlayNodeSubnet(node.Name)
				}
				return fmt.Errorf("failed to set hybrid overlay annotations for node %s: %v", node.Name, err)
			}
		} else if na.hasHybridOverlayAllocationUnmanaged() {
			// this is a hybrid overlay node but not managed by the hybrid overlay subnet allocator
			// Hybrid overlay is only available for IPv4 for now, so we only need to check for IPv4 subnets
			if err := na.markAllocatedNetworksForUnmanagedHONode(node); err != nil {
				return fmt.Errorf("failed to mark the subnet %v as allocated in the cluster subnet allocator for node %s: %v", na.nodeSubnets, node.Name, err)
			}
		}
		return nil
	}

	return na.syncNodeNetworkAnnotations(node)
}

// syncNodeNetworkAnnotations does 2 things
//   - syncs the node's allocated subnets in the node subnet annotation
//   - syncs the join subnet annotation
//   - syncs the layer 2 tunnel id annotation
//   - syncs the network id in the node network id annotation (legacy)
func (na *NodeAllocator) syncNodeNetworkAnnotations(node *corev1.Node) error {
	networkName := na.netInfo.GetNetworkName()

	networkID, err := util.ParseNetworkIDAnnotation(node, networkName)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			// Log the error and try to allocate new subnets
			klog.Warningf("Failed to get node %s network id annotations for network %s : %v", node.Name, networkName, err)
		}
		// if there is an error we are going to set networkID here to NoNetworkID to prevent
		// the annotation being updated
		networkID = types.NoNetworkID
	}

	updatedSubnetsMap := map[string][]*net.IPNet{}
	var validExistingSubnets, allocatedSubnets []*net.IPNet
	if na.hasNodeSubnetAllocation() {
		existingSubnets, err := util.ParseNodeHostSubnetAnnotation(node, networkName)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			// Log the error and try to allocate new subnets
			klog.Warningf("Failed to get node %s host subnets annotations for network %s : %v", node.Name, networkName, err)
		}

		// On return validExistingSubnets will contain any valid subnets that
		// were already assigned to the node. allocatedSubnets will contain
		// any newly allocated subnets required to ensure that the node has one subnet
		// from each enabled IP family.
		ipv4Mode, ipv6Mode := na.netInfo.IPMode()
		validExistingSubnets, allocatedSubnets, err = na.allocateNodeSubnets(na.clusterSubnetAllocator, node.Name, existingSubnets, ipv4Mode, ipv6Mode)
		if err != nil {
			return err
		}

		// If the existing subnets weren't OK, or new ones were allocated, update the node annotation.
		// This happens in a couple cases:
		// 1) new node: no existing subnets and one or more new subnets were allocated
		// 2) dual-stack to single-stack conversion: two existing subnets but only one will be valid, and no allocated subnets
		// 3) bad subnet annotation: one more existing subnets will be invalid and might have allocated a correct one
		if len(existingSubnets) != len(validExistingSubnets) || len(allocatedSubnets) > 0 {
			updatedSubnetsMap[networkName] = validExistingSubnets
		}
	}
	// TunnelID annotation is set in node only if Layer2UsesTransitRouter is disabled.
	// Given that tunnelID only is used by `createGWRouterPeerSwitchPort`.
	newTunnelID := types.NoTunnelID
	if na.HasNodeTunnelIDAllocation() {
		existingTunnelID, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, networkName)
		if err != nil && !util.IsAnnotationNotSetError(err) {
			return fmt.Errorf("failed to fetch tunnelID annotation from the node %s for network %s, err: %v",
				node.Name, networkName, err)
		}
		if existingTunnelID == types.InvalidID {
			if newTunnelID, err = na.idAllocator.AllocateID(networkName + "_" + node.Name); err != nil {
				return fmt.Errorf("failed to assign node %s tunnel id for network %s: %w", node.Name, networkName, err)
			}
			// This log should be printed only once at start up per network; per node
			klog.V(4).Infof("Allocating node %s tunnelID %d for network %s", node.Name, newTunnelID, networkName)
		} else {
			// calling reserve on already reserved id for the same key is a no-op; so we are fine here
			if err = na.idAllocator.ReserveID(networkName+"_"+node.Name, existingTunnelID); err != nil {
				return fmt.Errorf("failed to reserve node %s tunnel id for network %s: %w", node.Name, networkName, err)
			}
		}
	}

	// only update node annotation with ID if it had it before (networkID is not NoNetworkID)
	// and does not match.
	// NoNetworkID means do not update the annotation
	if networkID != types.NoNetworkID && networkID != na.networkID {
		networkID = na.networkID
	} else if networkID == na.networkID {
		// don't need to update if there was no change to the ID
		networkID = types.NoNetworkID
	}

	// Also update the node annotation if the networkID doesn't match
	if len(updatedSubnetsMap) > 0 || networkID != types.NoNetworkID || newTunnelID != types.NoTunnelID {
		updateStart := time.Now()
		err = na.updateNodeNetworkAnnotationsWithRetry(node.Name, updatedSubnetsMap, networkID, newTunnelID)

		if err != nil {
			// Before releasing, read the node from the apiserver and keep
			// any subnets already recorded in its host-subnet annotation.
			// Lagging informer cache may not have latest annotations (e.g. from the previous run of the same handler)
			releaseSubnets := allocatedSubnets
			// Bound the live read so an unresponsive apiserver cannot block cleanup indefinitely.
			getCtx, getCancel := context.WithTimeout(context.Background(), getNodeAPITimeout)
			latestNode, getErr := na.nodeClient.CoreV1().Nodes().Get(getCtx, node.Name, metav1.GetOptions{})
			getCancel()
			if getErr == nil {
				if persisted, parseErr := util.ParseNodeHostSubnetAnnotation(latestNode, networkName); parseErr == nil {
					releaseSubnets = subnetsNotPersisted(allocatedSubnets, persisted)
				}
			} else {
				klog.Warningf("Failed to read node %s before releasing subnets on annotation update failure: %v", node.Name, getErr)
			}
			if len(releaseSubnets) > 0 {
				if errR := na.clusterSubnetAllocator.ReleaseNetworks(node.Name, releaseSubnets...); errR != nil {
					klog.Warningf("Error releasing node %s subnets: %v", node.Name, errR)
				}
			}
			if newTunnelID != types.NoTunnelID {
				na.idAllocator.ReleaseID(networkName + "_" + node.Name)
				klog.Infof("Releasing node %s tunnelID for network %s since annotation update failed", node.Name, networkName)
			}
			return err
		}

		if na.netInfo.IsUserDefinedNetwork() && config.Metrics.EnableScaleMetrics {
			updateDuration := time.Since(updateStart)
			metrics.RecordUDNUpdateNodeAnnotationDuration(networkName, updateDuration.Seconds())
		}
	}

	return nil
}

// CleanupNode removes this network's per-node annotations when a node object is available
// and always releases allocator state for the provided node name.
func (na *NodeAllocator) CleanupNode(nodeName string, node *corev1.Node) error {
	networkName := na.netInfo.GetNetworkName()
	if node != nil {
		needsUpdate := util.HasNodeHostSubnetAnnotation(node, networkName)
		if !needsUpdate {
			if _, err := util.ParseNetworkIDAnnotation(node, networkName); err == nil {
				needsUpdate = true
			} else if !util.IsAnnotationNotSetError(err) {
				return fmt.Errorf("failed to parse node %q network id annotation for network %s: %w",
					node.Name, networkName, err)
			}
		}
		if !needsUpdate && util.IsNetworkSegmentationSupportEnabled() && na.netInfo.IsPrimaryNetwork() && util.DoesNetworkRequireTunnelIDs(na.netInfo) {
			if _, err := util.ParseUDNLayer2NodeGRLRPTunnelIDs(node, networkName); err == nil {
				needsUpdate = true
			} else if !util.IsAnnotationNotSetError(err) {
				return fmt.Errorf("failed to parse node %q tunnel id annotation for network %s: %w",
					node.Name, networkName, err)
			}
		}
		if needsUpdate {
			hostSubnetsMap := map[string][]*net.IPNet{networkName: nil}
			// passing util.InvalidID deletes the network/tunnel id annotation for the network.
			if err := na.updateNodeNetworkAnnotationsWithRetry(nodeName, hostSubnetsMap, types.InvalidID, types.InvalidID); err != nil {
				return fmt.Errorf("failed to clear node %q subnet annotation for network %s: %w",
					nodeName, networkName, err)
			}
		}
	}

	if na.hasHybridOverlayAllocation() {
		na.releaseHybridOverlayNodeSubnet(nodeName)
	}
	if na.hasNodeSubnetAllocation() || na.hasHybridOverlayAllocationUnmanaged() {
		na.clusterSubnetAllocator.ReleaseAllNetworks(nodeName)
		na.recordSubnetUsage()
	}
	if util.IsNetworkSegmentationSupportEnabled() && na.netInfo.IsPrimaryNetwork() && util.DoesNetworkRequireTunnelIDs(na.netInfo) {
		na.idAllocator.ReleaseID(networkName + "_" + nodeName)
	}

	return nil
}

func (na *NodeAllocator) Sync(nodes []interface{}) error {
	if !na.hasNodeSubnetAllocation() {
		return nil
	}

	defer na.recordSubnetUsage()

	networkName := na.netInfo.GetNetworkName()

	for _, tmp := range nodes {
		node, ok := tmp.(*corev1.Node)
		if !ok {
			return fmt.Errorf("spurious object in syncNodes: %v", tmp)
		}

		if util.NoHostSubnet(node) {
			if na.hasHybridOverlayAllocation() {
				// this is a hybrid overlay node so mark as allocated from the hybrid overlay subnet allocator
				hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
				if err != nil {
					klog.Errorf("Failed to parse hybrid overlay for node %s: %v", node.Name, err)
				} else if hostSubnet != nil {
					klog.V(5).Infof("Node %s contains subnets: %v", node.Name, hostSubnet)
					if err := na.hybridOverlaySubnetAllocator.MarkAllocatedNetworks(node.Name, hostSubnet); err != nil {
						klog.Errorf("Failed to mark the subnet %v as allocated in the hybrid subnet allocator for node %s: %v", hostSubnet, node.Name, err)
					}
				}
			} else if na.hasHybridOverlayAllocationUnmanaged() {
				// this is a hybrid overlay node but not managed by the hybrid overlay subnet allocator
				// Hybrid overlay is only available for IPv4 for now, so we only need to check for IPv4 subnets
				if err := na.markAllocatedNetworksForUnmanagedHONode(node); err != nil {
					klog.Errorf("Failed to mark the subnet as allocated in the cluster subnet allocator for hybrid overlay node %s: %v", node.Name, err)
				}

			}
		} else {
			hostSubnets, _ := util.ParseNodeHostSubnetAnnotation(node, networkName)
			if len(hostSubnets) > 0 {
				klog.V(5).Infof("Node %s contains subnets: %v for network : %s", node.Name, hostSubnets, networkName)
				if err := na.clusterSubnetAllocator.MarkAllocatedNetworks(node.Name, hostSubnets...); err != nil {
					klog.Errorf("Failed to mark the subnet %v as allocated in the cluster subnet allocator for node %s: %v", hostSubnets, node.Name, err)
				}
			} else {
				klog.V(5).Infof("Node %s contains no subnets for network : %s", node.Name, networkName)
			}
		}
	}

	return nil
}

// getNodeAPITimeout bounds the live apiserver read used before releasing
// subnets on an annotation-update failure.
const getNodeAPITimeout = 30 * time.Second

// subnetsNotPersisted returns the subnets in allocated that are not present in
// persisted (compared by CIDR string). Used to avoid releasing a subnet that a
// prior reconcile already wrote to the node's host-subnet annotation.
func subnetsNotPersisted(allocated, persisted []*net.IPNet) []*net.IPNet {
	persistedSet := sets.New[string]()
	for _, s := range persisted {
		persistedSet.Insert(s.String())
	}
	out := make([]*net.IPNet, 0, len(allocated))
	for _, s := range allocated {
		if !persistedSet.Has(s.String()) {
			out = append(out, s)
		}
	}
	return out
}

// updateNodeNetworkAnnotationsWithRetry will update the node's subnet annotation and network id annotation
func (na *NodeAllocator) updateNodeNetworkAnnotationsWithRetry(nodeName string, hostSubnetsMap map[string][]*net.IPNet, networkId, tunnelID int) error {
	if na.batcher == nil {
		return fmt.Errorf("node annotation batcher is required but was not configured")
	}

	networkName := na.netInfo.GetNetworkName()
	hostSubnets, hasSubnetUpdate := hostSubnetsMap[networkName]
	if hasSubnetUpdate || networkId != types.NoNetworkID || tunnelID != types.NoTunnelID {
		na.batcher.EnqueueUpdate(nodeName, networkName, hostSubnets, hasSubnetUpdate, networkId, tunnelID)
		klog.V(5).Infof("Enqueued annotation update for node %s network %s to batcher", nodeName, networkName)
	}
	return nil
}

// Cleanup the subnet annotations from the node
func (na *NodeAllocator) Cleanup() error {
	networkName := na.netInfo.GetNetworkName()

	// remove hostsubnet annotation for this network
	klog.Infof("Remove node-subnets annotation for network %s on all nodes", networkName)
	existingNodes, err := na.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error in retrieving the nodes: %v", err)
	}

	for _, node := range existingNodes {
		if util.NoHostSubnet(node) {
			// Secondary network subnet is not allocated for a nohost subnet node
			klog.V(5).Infof("Node %s is not managed by OVN", node.Name)
			continue
		}

		hostSubnetsMap := map[string][]*net.IPNet{networkName: {}}
		// passing util.InvalidID deletes the network/tunnel id annotation for the network.
		err = na.updateNodeNetworkAnnotationsWithRetry(node.Name, hostSubnetsMap, types.InvalidID, types.InvalidID)
		if err != nil {
			return fmt.Errorf("failed to clear node %q subnet annotation for network %s",
				node.Name, networkName)
		}

		na.clusterSubnetAllocator.ReleaseAllNetworks(node.Name)
	}

	return nil
}

// allocateNodeSubnets either validates existing node subnets against the allocators
// ranges, or allocates new subnets if the node doesn't have any yet, or returns an error
func (na *NodeAllocator) allocateNodeSubnets(allocator SubnetAllocator, nodeName string, existingSubnets []*net.IPNet, ipv4Mode, ipv6Mode bool) ([]*net.IPNet, []*net.IPNet, error) {
	allocatedSubnets := []*net.IPNet{}

	// OVN can work in single-stack or dual-stack only.
	expectedHostSubnets := 1
	// if dual-stack mode we expect one subnet per each IP family
	if ipv4Mode && ipv6Mode {
		expectedHostSubnets = 2
	}

	// If any existing subnets the node has are valid, mark them as reserved.
	// The node might have invalid or already-reserved subnets, or it might
	// have more subnets than configured in OVN (like for dual-stack to/from
	// single-stack conversion).
	// filter in place slice
	// https://github.com/golang/go/wiki/SliceTricks#filter-in-place
	foundIPv4 := false
	foundIPv6 := false
	n := 0
	for _, subnet := range existingSubnets {
		if (ipv4Mode && utilnet.IsIPv4CIDR(subnet) && !foundIPv4) || (ipv6Mode && utilnet.IsIPv6CIDR(subnet) && !foundIPv6) {
			if err := allocator.MarkAllocatedNetworks(nodeName, subnet); err == nil {
				existingSubnets[n] = subnet
				n++
				if utilnet.IsIPv4CIDR(subnet) {
					foundIPv4 = true
				} else if utilnet.IsIPv6CIDR(subnet) {
					foundIPv6 = true
				}
				continue
			}
		}
		// this subnet is no longer needed; release it
		klog.Infof("Releasing unused or invalid subnet %v on node %s, network %s",
			subnet, na.netInfo.GetNetworkName(), nodeName)
		if err := allocator.ReleaseNetworks(nodeName, subnet); err != nil {
			klog.Warningf("Failed to release subnet %v on node %s, network %s: %v",
				subnet, nodeName, na.netInfo.GetNetworkName(), err)
		}
	}
	// recreate existingSubnets with the valid subnets
	existingSubnets = existingSubnets[:n]

	// Node has enough valid subnets already allocated
	if len(existingSubnets) == expectedHostSubnets {
		return existingSubnets, allocatedSubnets, nil
	}

	// Release allocated subnets on error
	releaseAllocatedSubnets := true
	defer func() {
		if releaseAllocatedSubnets {
			for _, subnet := range allocatedSubnets {
				klog.Warningf("Releasing subnet %v on node %s, network: %s", subnet, nodeName, na.netInfo.GetNetworkName())
				if errR := allocator.ReleaseNetworks(nodeName, subnet); errR != nil {
					klog.Warningf("Error releasing subnet %v on node %s, network %s: %v",
						subnet, na.netInfo.GetNetworkName(), nodeName, errR)
				}
			}
		}
	}()

	// allocateOneSubnet is a helper to process the result of a subnet allocation
	allocateOneSubnet := func(allocatedHostSubnet *net.IPNet, allocErr error) error {
		if allocErr != nil {
			return fmt.Errorf("error allocating network for node %s, network name %s: %v",
				nodeName, na.netInfo.GetNetworkName(), allocErr)
		}
		// the allocator returns nil if it can't provide a subnet
		// we should filter them out or they will be appended to the slice
		if allocatedHostSubnet != nil {
			klog.V(5).Infof("Allocating subnet %v on node %s for network: %s",
				allocatedHostSubnet, nodeName, na.netInfo.GetNetworkName())
			allocatedSubnets = append(allocatedSubnets, allocatedHostSubnet)
		}
		return nil
	}

	// allocate new subnets if needed
	if ipv4Mode && !foundIPv4 {
		if err := allocateOneSubnet(allocator.AllocateIPv4Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}
	if ipv6Mode && !foundIPv6 {
		if err := allocateOneSubnet(allocator.AllocateIPv6Network(nodeName)); err != nil {
			return nil, nil, err
		}
	}

	// check if we were able to allocate the new subnets require
	// this can only happen if OVN is not configured correctly
	// so it will require a reconfiguration and restart.
	wantedSubnets := expectedHostSubnets - len(existingSubnets)
	if wantedSubnets > 0 && len(allocatedSubnets) != wantedSubnets {
		return nil, nil, fmt.Errorf("error allocating networks for network: %s, node %s: %d subnets expected only new %d subnets allocated",
			na.netInfo.GetNetworkName(), nodeName, expectedHostSubnets, len(allocatedSubnets))
	}

	hostSubnets := append(existingSubnets, allocatedSubnets...)
	klog.Infof("Allocated Subnets %v on Node %s for Network: %s", hostSubnets, nodeName, na.netInfo.GetNetworkName())

	// Success; prevent the release-on-error from triggering and return all node subnets
	releaseAllocatedSubnets = false
	return hostSubnets, allocatedSubnets, nil
}

func (na *NodeAllocator) hasNodeSubnetAllocation() bool {
	// we only allocate subnets for L3 user-defined network or default network
	return na.netInfo.TopologyType() == types.Layer3Topology || !na.netInfo.IsUserDefinedNetwork()
}

func (na *NodeAllocator) HasNodeSubnetAllocation() bool {
	return na.hasNodeSubnetAllocation()
}

func (na *NodeAllocator) HasNodeTunnelIDAllocation() bool {
	return util.IsNetworkSegmentationSupportEnabled() &&
		na.netInfo.IsPrimaryNetwork() &&
		util.DoesNetworkRequireTunnelIDs(na.netInfo) &&
		!config.Layer2UsesTransitRouter
}

func (na *NodeAllocator) markAllocatedNetworksForUnmanagedHONode(node *corev1.Node) error {
	hostSubnet, err := houtil.ParseHybridOverlayHostSubnet(node)
	if err != nil {
		return fmt.Errorf("failed to parse hybrid overlay for node %s: %v", node.Name, err)
	}

	var overlaps []*net.IPNet
	for _, clusterSubnet := range na.netInfo.Subnets() {
		if overlaps = util.IPNetOverlaps(hostSubnet, clusterSubnet.CIDR); len(overlaps) != 0 {
			break
		}
	}
	if len(overlaps) == 0 {
		// if the host subnet does not overlap with any cluster subnet, return
		return nil
	}

	if hostSubnet != nil {
		if na.nodeSubnets == nil {
			// initialize the nodeSubnets variable at the first called
			na.nodeSubnets = na.clusterSubnetAllocator.ListAllIPv4Networks()
		}
		// check if the host subnet overlaps with any node subnet that is managed by the cluster subnet allocator
		// if it does, mark it as allocated in the cluster subnet allocator
		if overlaps := util.IPNetOverlaps(hostSubnet, na.nodeSubnets...); overlaps != nil {
			klog.Infof("Hybrid overlay node %s overlaps with subnets: %v", node.Name, overlaps)
			if err := na.clusterSubnetAllocator.MarkAllocatedNetworks(node.Name, overlaps...); err != nil {
				return fmt.Errorf("failed to mark the subnet %v as allocated: %v", hostSubnet, err)
			}
		}
	}
	return nil
}
