// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkconnect

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapply "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	cncapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/applyconfiguration/clusternetworkconnect/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const fieldManager = "clustermanager-networkconnect-controller"

var (
	errConfig = errors.New("configuration error")
)

// getPrimaryNADForNamespace returns the primary NAD key and network info for a namespace.
// This is used when processing namespaces that may have a primary UDN.
// Returns:
//   - nadKey: the primary NAD key in "namespace/name" format (empty if namespace uses default network)
//   - network: the network info for the primary network (nil if namespace uses default network)
//   - err: error if failed to get/validate the network
//
// If the namespace uses the default network (no primary UDN), returns ("", nil, nil).
// Callers should check for empty nadKey to determine if namespace has a primary UDN.
func getPrimaryNADForNamespace(networkMgr networkmanager.Interface, namespaceName string) (nadKey string, network util.NetInfo, err error) {
	namespacePrimaryNetwork, err := networkMgr.GetActiveNetworkForNamespace(namespaceName)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
			// We intentionally ignore transient namespace/network state because
			// UDN Controller hasn't created the NAD yet, OR NAD doesn't exist in a
			// namespace that has the required UDN label. It could also be that the
			// UDN was deleted and the NAD is also gone. Namespace/NAD handlers will
			// trigger reconciliation when state becomes available.
			return "", nil, nil
		}
		return "", nil, err
	}
	if namespacePrimaryNetwork == nil || namespacePrimaryNetwork.IsDefault() {
		// No primary UDN in this namespace
		return "", nil, nil
	}
	primaryNADKey, err := networkMgr.GetPrimaryNADForNamespace(namespaceName)
	if err != nil {
		if util.IsInvalidPrimaryNetworkError(err) || apierrors.IsNotFound(err) {
			return "", nil, nil
		}
		return "", nil, err
	}
	if primaryNADKey == ovntypes.DefaultNetworkName {
		return "", nil, nil
	}
	return primaryNADKey, namespacePrimaryNetwork, nil
}

func (c *Controller) updateStatus(ctx context.Context, cnc *networkconnectv1.ClusterNetworkConnect, e error) {
	condition := metaapply.Condition().
		WithType("Accepted").
		WithStatus("True").
		WithReason("ResourceAllocationSucceeded").
		WithMessage("All validation checks passed successfully")
	if e != nil {
		msg := e.Error()
		if len(msg) >= 32767 { // max length of message can be 32768
			msg = msg[:32766]
		}
		condition = condition.WithStatus("False").
			WithReason("ResourceAllocationFailed").
			WithMessage(msg)
	}
	// check if condition actually changed before updating status
	existingCondition := meta.FindStatusCondition(cnc.Status.Conditions, "Accepted")
	if existingCondition != nil &&
		existingCondition.Status == *condition.Status &&
		existingCondition.Reason == *condition.Reason &&
		existingCondition.Message == *condition.Message {
		return
	}
	if existingCondition == nil || existingCondition.Status != *condition.Status {
		// LastTransitionTime should only be updated in the Status (True/False)	changes
		condition = condition.WithLastTransitionTime(metav1.NewTime(time.Now()))
	}

	_, err := c.cncClient.K8sV1().ClusterNetworkConnects().ApplyStatus(
		ctx,
		cncapply.ClusterNetworkConnect(cnc.Name).WithStatus(
			cncapply.ClusterNetworkConnectStatus().WithConditions(condition),
		),
		metav1.ApplyOptions{
			FieldManager: fieldManager,
			Force:        true,
		},
	)
	if err != nil {
		klog.Errorf("Failed to update status for cluster network connect %s: %v", cnc.Name, err)
	}
}

func (c *Controller) reconcileClusterNetworkConnect(ctx context.Context, key string) error {
	startTime := time.Now()
	_, cncName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split CNC key %s: %w", key, err)
	}
	klog.V(5).Infof("reconcileClusterNetworkConnect %s", cncName)
	defer func() {
		klog.Infof("reconcileClusterNetworkConnect %s took %v", cncName, time.Since(startTime))
	}()
	cnc, err := c.cncLister.Get(cncName)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get CNC %s: %w", cncName, err)
	}
	err = c.syncClusterNetworkConnect(cncName, cnc)
	if cnc != nil {
		c.updateStatus(ctx, cnc, err)
	}
	return err
}

func (c *Controller) syncClusterNetworkConnect(cncName string, cnc *networkconnectv1.ClusterNetworkConnect) error {
	c.Lock()
	defer c.Unlock()
	cncState, cncExists := c.cncCache[cncName]
	if cnc == nil {
		// CNC is being deleted, clean up resources
		// Clean up the cache
		// Note: allocator cleanup is not needed - it will be garbage collected
		// when the cache entry is deleted below since it's self-contained per-CNC
		// Annotations also don't need to be removed since object is already deleted.
		if cncExists {
			// Release tunnel key
			c.tunnelKeysAllocator.ReleaseKeys(cncName)
			klog.V(4).Infof("Released tunnel key for deleted CNC %s", cncName)
		}

		// Clean up the cache
		delete(c.cncCache, cncName)
		klog.V(4).Infof("Cleaned up cache for deleted CNC %s", cncName)
		return nil
	}
	// If CNC state doesn't exist yet (means its a CNC creation), create entry in the cache
	if !cncExists {
		cncState = &clusterNetworkConnectState{
			name:             cnc.Name,
			selectedNADs:     sets.New[string](),
			selectedNetworks: sets.New[string](),
		}
		connectSubnetAllocator, err := NewHybridConnectSubnetAllocator(cnc.Spec.ConnectSubnets, cncName)
		if err != nil {
			return fmt.Errorf("failed to initialize subnet allocator for CNC %s: %w", cncName, err)
		}
		connectSubnets := []*net.IPNet{}
		for _, cs := range cnc.Spec.ConnectSubnets {
			// ignore error, this was already parsed for NewHybridConnectSubnetAllocator
			_, cidr, _ := net.ParseCIDR(string(cs.CIDR))
			connectSubnets = append(connectSubnets, cidr)
		}
		cncState.connectSubnets = connectSubnets
		cncState.allocator = connectSubnetAllocator
		klog.V(5).Infof("Initialized subnet allocator for CNC %s", cncName)
		c.cncCache[cnc.Name] = cncState
	}

	// STEP1: Discover the selected UDNs and CUDNs
	// Discovery, allocation, and release continue on per-network errors, so healthy networks
	// make progress. Errors are aggregated and returned at the end.
	var errs []error
	discoveredNetworks, allMatchingNADKeys, err := c.discoverSelectedNetworks(cnc)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to discover selected networks for CNC %s: %w", cncName, err))
	}
	// STEP2: Validate the CNC
	if err = validateCNC(cncState, discoveredNetworks); err != nil {
		return fmt.Errorf("validation failed for CNC %s: %w", cncName, err)
	}
	if err = c.crossValidateCNCs(cncState, discoveredNetworks); err != nil {
		return fmt.Errorf("cross-validation failed for CNC %s: %w", cncName, err)
	}

	// STEP3: Generate a tunnelID for the connect router corresponding to this CNC
	// passing a value greater than 4096 as networkID - actually we don't need this value,
	// but it's required by the allocator to ensure that the prederministic tunnel keys
	// that are derived from the networkID are not reused for backwards compatibility reasons.
	// So we want to skip that range and use the next available tunnel key.
	// do this only if the CNC is being created - its a one time allocation.
	if cncState.tunnelID == 0 { // cncState will exist as its created above
		tunnelID, err := c.tunnelKeysAllocator.AllocateKeys(cnc.Name, 4096+1, 1)
		if err != nil {
			return fmt.Errorf("failed to allocate tunnel key for CNC %s: %w", cncName, err)
		}
		err = util.UpdateNetworkConnectRouterTunnelKeyAnnotation(cnc.Name, c.cncClient, tunnelID[0])
		if err != nil {
			return fmt.Errorf("failed to update network connect router tunnel key annotation for CNC %s: %w", cncName, err)
		}
		cncState.tunnelID = tunnelID[0]
	}

	// STEP4: Generate or release subnets of size CNC.Spec.ConnectSubnets.NetworkPrefix for each layer3 network
	//  and /31 or /127 subnets for each layer2 network
	// We intentionally don't compute or use the networksNeedingAllocation set here because we want to return all
	// currently allocated subnets for each owner back to the annotation update step.
	allocatedSubnets, allMatchingNetworkKeys, err := c.allocateSubnets(discoveredNetworks, cncState.allocator)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to allocate subnets for CNC %s: %w", cncName, err))
	}
	// This step will handle the release of subnets for networks that are no longer matched or are deleted.
	// NOTE: Since allMatchingNetworkKeys might not have the network or nad which had a transient error,
	// (a rare event like informer list of get or parse nad going wrong for a nad update event), its possible
	// we end up releasing and re-allocating subnets for networks that had a transient error. But that risk is
	// acceptable and we can live with it in favor of the gain we get by not blocking the setup of other healthy networks.
	networksNeedingRelease := cncState.selectedNetworks.Difference(allMatchingNetworkKeys)
	if len(networksNeedingRelease) > 0 {
		err = c.releaseSubnets(networksNeedingRelease, cncState.allocator)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to release subnets for CNC %s: %w", cncName, err))
		}
	}
	networksNeedingAllocation := allMatchingNetworkKeys.Difference(cncState.selectedNetworks)
	klog.V(5).Infof("CNC %s: selectedNetworks=%v, allMatchingNetworkKeys=%v, networksNeedingAllocation=%v, networksNeedingRelease=%v",
		cncName,
		cncState.selectedNetworks.UnsortedList(), allMatchingNetworkKeys.UnsortedList(), networksNeedingAllocation.UnsortedList(),
		networksNeedingRelease.UnsortedList())
	// we need to update the annotation only if there are networks that are newly matched or newly released
	if len(networksNeedingAllocation) > 0 || len(networksNeedingRelease) > 0 {
		err = util.UpdateNetworkConnectSubnetAnnotation(cnc, c.cncClient, allocatedSubnets)
		if err != nil {
			return fmt.Errorf("failed to update network connect subnet annotation for CNC %s: %w", cncName, err)
		}
	}
	// plumbing is now done, update the cache with latest
	cncState.selectedNADs = allMatchingNADKeys
	klog.V(5).Infof("Updated selectedNADs cache for CNC %s with %d NADs", cncName, allMatchingNADKeys.Len())
	cncState.selectedNetworks = allMatchingNetworkKeys
	klog.V(5).Infof("Updated selectedNetworks cache for CNC %s with %d networks", cncName, allMatchingNetworkKeys.Len())
	return kerrors.NewAggregate(errs)
}

// validateCNC performs validation checks for the given CNC connectNetworks and discovered networks.
func validateCNC(cncState *clusterNetworkConnectState, discoveredNetworks []util.NetInfo) error {
	// Check for overlap with cluster networks.
	// This includes service CIDR and masquerade CIDR
	if err := util.CheckSubnetOverlapWithClusterSubnets(cncState.connectSubnets, "ClusterNetworkConnect.connectSubnets"); err != nil {
		return fmt.Errorf("cluster subnets overlap with cluster network connect subnets: %w", err)
	}
	// Check for subnet overlaps between connectSubnets and discovered networks.
	// That includes overlaps with pod subnet, transit and join subnets.
	if err := util.CheckSubnetOverlapWithNetworks(cncState.connectSubnets, "ClusterNetworkConnect.connectSubnets", discoveredNetworks); err != nil {
		return fmt.Errorf("selected networks overlap with cluster network connect subnets: %w", err)
	}
	// Now check for overlaps among the discovered networks themselves.
	if err := util.CheckNetworksOverlap(discoveredNetworks); err != nil {
		return fmt.Errorf("can not connect selected networks: selected networks have overlapping subnets: %w", err)
	}
	// Also check ipFamily compatibility between discovered networks and connectSubnets
	// The only incompatible case is when discovered networks contain both ipv4-only and ipv6-only networks.
	var ipv4Only, ipv6Only string
	for _, network := range discoveredNetworks {
		networkIPv4, networkIPv6 := network.IPMode()
		if networkIPv4 && !networkIPv6 {
			// ipv4 only network
			if ipv6Only != "" {
				// conflict
				return fmt.Errorf("IP family conflict: connected network %s has only ipv4 subnets, but another connected network %s has only ipv6 subnets",
					network.GetNetworkName(), ipv6Only)
			}
			ipv4Only = network.GetNetworkName()
		} else if !networkIPv4 && networkIPv6 {
			// ipv6 only network
			if ipv4Only != "" {
				// conflict
				return fmt.Errorf("IP family conflict: connected network %s has only ipv6 subnets, but another connected network %s has only ipv4 subnets",
					network.GetNetworkName(), ipv4Only)
			}
			ipv6Only = network.GetNetworkName()
		}
	}
	// Check that connectSubnets can support the discovered networks' ipFamilies
	var csIPv4, csIPv6 bool
	for _, cs := range cncState.connectSubnets {
		if cs.IP.To4() != nil {
			csIPv4 = true
		} else {
			csIPv6 = true
		}
	}
	if ipv4Only != "" && !csIPv4 {
		return fmt.Errorf("IP family conflict: connected network %s is ipv4-only but connectSubnets do not contain an ipv4 subnet",
			ipv4Only)
	}
	if ipv6Only != "" && !csIPv6 {
		return fmt.Errorf("IP family conflict: connected network %s is ipv6-only but connectSubnets do not contain an ipv6 subnet",
			ipv6Only)
	}
	return nil
}

func (c *Controller) crossValidateCNCs(cncState *clusterNetworkConnectState, discoveredNetworks []util.NetInfo) error {
	// If more than 1 ClusterNetworkConnect API selects the same network, then they must have non-overlapping
	// connectSubnets configured. If there is a conflict, we must emit an event and fail.
	// OVN does not handle blocking the creation of two ports on the same router with same subnets.
	// So if that happens, it will cause wrong routing and hence OVN-Kubernetes must detect this conflict.
	// Step 1: find CNCs that have overlapping selected networks with this CNC
	// Step 2: check for connectSubnets overlap with those CNCs

	// Build a set of selected networks for this CNC.
	// We can't use cncState.selectedNetworks because it is only updated at the end of the sync,
	// and validation should happen before that.
	currentCNCSelectedNetworks := sets.New[string]()
	for _, network := range discoveredNetworks {
		networkID := network.GetNetworkID()
		if networkID == ovntypes.NoNetworkID {
			continue
		}
		topologyType := network.TopologyType()
		owner := util.ComputeNetworkOwner(topologyType, networkID)
		currentCNCSelectedNetworks.Insert(owner)
	}
	for otherCNCName, otherCNCState := range c.cncCache {
		if otherCNCName == cncState.name {
			continue
		}
		overlappingNetworks := currentCNCSelectedNetworks.Intersection(otherCNCState.selectedNetworks)
		if len(overlappingNetworks) != 0 {
			// There are overlapping selected networks, check for connectSubnets overlap
			if util.NetworksOverlap(cncState.connectSubnets, otherCNCState.connectSubnets) {
				return fmt.Errorf("connectSubnets overlap detected between CNC %s and CNC %s which both select networks %v",
					cncState.name, otherCNCName, overlappingNetworks.UnsortedList())
			}
		}
	}
	return nil
}

func (c *Controller) discoverSelectedNetworks(cnc *networkconnectv1.ClusterNetworkConnect) ([]util.NetInfo, sets.Set[string], error) {
	discoveredNetworks := map[string]util.NetInfo{}
	allMatchingNADKeys := sets.New[string]()
	var errs []error

	for _, selector := range cnc.Spec.NetworkSelectors {
		switch selector.NetworkSelectionType {
		case apitypes.ClusterUserDefinedNetworks:
			networkSelector, err := metav1.LabelSelectorAsSelector(&selector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to parse CUDN network selector: %w", err))
				continue
			}
			nads, err := c.nadLister.List(networkSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list NADs for CUDN selector: %w", err))
				continue
			}
			for _, nad := range nads {
				// check this NAD is controlled by a CUDN
				controller := metav1.GetControllerOfNoCopy(nad)
				isCUDN := controller != nil && controller.Kind == cudnGVK.Kind && controller.APIVersion == cudnGVK.GroupVersion().String()
				if !isCUDN {
					continue
				}
				network := c.networkManager.GetNetInfoForNADKey(types.NamespacedName{Namespace: nad.Namespace, Name: nad.Name}.String())
				if network == nil {
					// if network doesn't exist in networkmanager, we will get an update later
					klog.V(5).Infof("Failed to find NetInfo for NAD %s/%s", nad.Namespace, nad.Name)
					continue
				}
				if !network.IsPrimaryNetwork() {
					continue
				}
				if network.Transport() == ovntypes.NetworkTransportNoOverlay || network.Transport() == ovntypes.NetworkTransportEVPN {
					errs = append(errs, fmt.Errorf("network %s has transport type %s that is not supported for networkConnect",
						network.GetNetworkName(), network.Transport()))
					continue
				}
				// This NAD passed all validation checks, so it's selected by this CNC
				nadKey := nad.Namespace + "/" + nad.Name
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks[network.GetNetworkName()] = network
			}
		case apitypes.PrimaryUserDefinedNetworks:
			namespaceSelector, err := metav1.LabelSelectorAsSelector(&selector.PrimaryUserDefinedNetworkSelector.NamespaceSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to parse PUDN namespace selector: %w", err))
				continue
			}
			namespaces, err := c.namespaceLister.List(namespaceSelector)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to list namespaces for PUDN selector: %w", err))
				continue
			}
			for _, ns := range namespaces {
				nadKey, namespacePrimaryNetwork, err := getPrimaryNADForNamespace(c.networkManager, ns.Name)
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to get active network for namespace %s: %w", ns.Name, err))
					continue
				}
				if nadKey == "" {
					// Namespace uses default network (no primary UDN) or UDN was deleted
					continue
				}
				allMatchingNADKeys.Insert(nadKey)
				discoveredNetworks[namespacePrimaryNetwork.GetNetworkName()] = namespacePrimaryNetwork
			}
		default:
			errs = append(errs, fmt.Errorf("%w: unsupported network selection type %s", errConfig, selector.NetworkSelectionType))
		}
	}

	return slices.Collect(maps.Values(discoveredNetworks)), allMatchingNADKeys, kerrors.NewAggregate(errs)
}

// allocateSubnets allocates subnets for the given discovered networks
// It returns a map of owner to subnets
// NOTE: If owner already had its subnets allocated, it will simply return those existing subnets
func (c *Controller) allocateSubnets(discoveredNetworks []util.NetInfo, allocator HybridConnectSubnetAllocator) (map[string][]*net.IPNet, sets.Set[string], error) {
	var owner string
	var subnets []*net.IPNet
	var errs []error
	allMatchingNetworkKeys := sets.New[string]()
	allocatedSubnets := make(map[string][]*net.IPNet)
	for _, network := range discoveredNetworks {
		networkID := network.GetNetworkID()
		if networkID == ovntypes.NoNetworkID {
			errs = append(errs, fmt.Errorf("network id is invalid for network %s", network.GetNetworkName()))
			continue
		}
		var err error
		if network.TopologyType() == ovntypes.Layer3Topology {
			owner = util.ComputeNetworkOwner(ovntypes.Layer3Topology, networkID)
			subnets, err = allocator.AllocateLayer3Subnet(owner)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to allocate Layer3 subnet for network %s: %w", network.GetNetworkName(), err))
				continue
			}
		} else if network.TopologyType() == ovntypes.Layer2Topology {
			owner = util.ComputeNetworkOwner(ovntypes.Layer2Topology, networkID)
			subnets, err = allocator.AllocateLayer2Subnet(owner)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to allocate Layer2 subnet for network %s: %w", network.GetNetworkName(), err))
				continue
			}
		} else {
			errs = append(errs, fmt.Errorf("unsupported network topology type %s for network %s", network.TopologyType(), network.GetNetworkName()))
			continue
		}
		allocatedSubnets[owner] = subnets
		allMatchingNetworkKeys.Insert(owner)
		klog.V(5).Infof("Allocated subnets %v for %s (network: %s)", subnets, owner, network.GetNetworkName())
	}
	return allocatedSubnets, allMatchingNetworkKeys, kerrors.NewAggregate(errs)
}

// releaseSubnets releases subnets for the given network keys.
// Network keys encode topology type and network ID (e.g., "layer3_1", "layer2_2"),
// allowing subnet release without needing to re-discover network info.
func (c *Controller) releaseSubnets(networksNeedingRelease sets.Set[string],
	allocator HybridConnectSubnetAllocator) error {
	var errs []error
	for networkKey := range networksNeedingRelease {
		topologyType, _, err := util.ParseNetworkOwner(networkKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid network key format: %s", networkKey))
			continue
		}
		switch topologyType {
		case ovntypes.Layer3Topology:
			allocator.ReleaseLayer3Subnet(networkKey)
		case ovntypes.Layer2Topology:
			allocator.ReleaseLayer2Subnet(networkKey)
		default:
			errs = append(errs, fmt.Errorf("unsupported network topology type %s for network %s", topologyType, networkKey))
			continue
		}
		klog.V(5).Infof("Released subnets for network %s", networkKey)
	}
	return kerrors.NewAggregate(errs)
}
