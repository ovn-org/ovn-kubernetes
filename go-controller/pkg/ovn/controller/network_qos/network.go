// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"

	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var cudnKind = udnv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork")

type networkContext struct {
	netInfo util.NetInfo
	// Use the same owner controller as the old per-network controllers so existing QoS rows
	// and shared address sets are found.
	controllerName string
}

func (c *Controller) newNetworkContext(netInfo util.NetInfo) *networkContext {
	return &networkContext{
		netInfo:        netInfo,
		controllerName: networkControllerName(netInfo.GetNetworkName()),
	}
}

func networkControllerName(networkName string) string {
	return networkName + "-network-controller"
}

// Returns an empty name for unsupported topologies.
func (nctx *networkContext) logicalSwitchName(nodeName string) string {
	netInfo := nctx.netInfo
	switch {
	case netInfo.TopologyType() == types.Layer2Topology:
		return netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
	case netInfo.TopologyType() == types.LocalnetTopology:
		return netInfo.GetNetworkScopedSwitchName(types.OVNLocalnetSwitch)
	case !netInfo.IsUserDefinedNetwork() || netInfo.TopologyType() == types.Layer3Topology:
		return netInfo.GetNetworkScopedSwitchName(nodeName)
	default:
		return ""
	}
}

// Rules should apply on the node the packet comes from, before it crosses the network.
//   - Layer2: one switch spans all nodes, and a packet to a pod on another node leaves the
//     switch on that node, so rules apply when packets enter the switch (from-lport).
//   - Other topologies: packets leave the node switch, so rules apply when packets leave
//     the switch (to-lport), after a Service IP is replaced by the pod IP.
func (nctx *networkContext) qosDirection() string {
	if nctx.netInfo.TopologyType() == types.Layer2Topology {
		return nbdb.QoSDirectionFromLport
	}
	return nbdb.QoSDirectionToLport
}

func validateNetworkSelectors(networkSelectors crdtypes.NetworkSelectors) error {
	for index, networkSelector := range networkSelectors {
		var selectors []*metav1.LabelSelector
		switch networkSelector.NetworkSelectionType {
		case crdtypes.ClusterUserDefinedNetworks:
			if networkSelector.ClusterUserDefinedNetworkSelector == nil {
				return fmt.Errorf("network selector %d: empty cluster user defined network selector", index)
			}
			selectors = append(selectors, &networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
		case crdtypes.NetworkAttachmentDefinitions:
			if networkSelector.NetworkAttachmentDefinitionSelector == nil {
				return fmt.Errorf("network selector %d: empty network attachment definition selector", index)
			}
			selectors = append(selectors, &networkSelector.NetworkAttachmentDefinitionSelector.NamespaceSelector,
				&networkSelector.NetworkAttachmentDefinitionSelector.NetworkSelector)
		default:
			return fmt.Errorf("network selector %d: unsupported network selection type %s", index, networkSelector.NetworkSelectionType)
		}
		for _, selector := range selectors {
			if _, err := metav1.LabelSelectorAsSelector(selector); err != nil {
				return fmt.Errorf("network selector %d: invalid label selector %s: %w", index, selector.String(), err)
			}
		}
	}
	return nil
}

func (c *Controller) resolveNetworks(networkSelectors crdtypes.NetworkSelectors) (map[string]util.NetInfo, error) {
	networks := map[string]util.NetInfo{}
	if len(networkSelectors) == 0 {
		networks[types.DefaultNetworkName] = &util.DefaultNetInfo{}
		return networks, nil
	}
	// Without multiple networks there are no NADs to select.
	if c.nadLister == nil {
		return networks, nil
	}

	for _, networkSelector := range networkSelectors {
		var nads []*nadv1.NetworkAttachmentDefinition
		var err error
		switch networkSelector.NetworkSelectionType {
		case crdtypes.ClusterUserDefinedNetworks:
			nads, err = c.nadsBySelectors(nil, &networkSelector.ClusterUserDefinedNetworkSelector.NetworkSelector)
			cudnNADs := make([]*nadv1.NetworkAttachmentDefinition, 0, len(nads))
			for _, nad := range nads {
				if isOwnedByCUDN(nad) {
					cudnNADs = append(cudnNADs, nad)
				}
			}
			nads = cudnNADs
		case crdtypes.NetworkAttachmentDefinitions:
			nads, err = c.nadsBySelectors(&networkSelector.NetworkAttachmentDefinitionSelector.NamespaceSelector,
				&networkSelector.NetworkAttachmentDefinitionSelector.NetworkSelector)
		default:
			err = fmt.Errorf("unsupported network selection type %s", networkSelector.NetworkSelectionType)
		}
		if err != nil {
			return nil, err
		}
		c.addNetworksOfNADs(networks, nads)
	}

	return networks, nil
}

// A NAD change can affect any network selector.
// Requeue policies with selectors and let the workqueue merge duplicates.
// Policies using only the default network are unaffected.
func (c *Controller) reconcileNAD(key string) error {
	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list NetworkQoS: %w", err)
	}
	requeued := 0
	for _, nqos := range nqoses {
		if len(nqos.Spec.NetworkSelectors) == 0 {
			continue
		}
		c.nqosController.Reconcile(nqosKey(nqos.Namespace, nqos.Name))
		requeued++
	}
	klog.V(5).Infof("Requeued %d NetworkQoS for NAD %s", requeued, key)
	return nil
}

// One CUDN may have multiple NADs, fetch NetInfo once per network, and skip networks
// that are not served on this node or that have no IP addresses.
func (c *Controller) addNetworksOfNADs(networks map[string]util.NetInfo, nads []*nadv1.NetworkAttachmentDefinition) {
	nadKeyOfNetwork := map[string]string{}
	for _, nad := range nads {
		nadKey := util.GetNADName(nad.Namespace, nad.Name)
		networkName := c.networkManager.GetNetworkNameForNADKey(nadKey)
		if networkName == "" {
			continue
		}
		if _, resolved := networks[networkName]; resolved {
			continue
		}
		if _, seen := nadKeyOfNetwork[networkName]; !seen {
			nadKeyOfNetwork[networkName] = nadKey
		}
	}
	for networkName, nadKey := range nadKeyOfNetwork {
		netInfo := c.networkManager.GetNetInfoForNADKey(nadKey)
		if netInfo == nil {
			continue
		}
		if ipv4Mode, ipv6Mode := netInfo.IPMode(); !ipv4Mode && !ipv6Mode {
			klog.V(5).Infof("Network %s has no IP addresses, NetworkQoS does not apply to it", networkName)
			continue
		}
		networks[networkName] = netInfo
	}
}

// Select NADs by namespace and NAD selectors:
// - A nil or empty namespace selector matches all namespaces
// - An empty NAD selector matches all NADs in those namespaces
func (c *Controller) nadsBySelectors(namespaceSelector, nadSelector *metav1.LabelSelector) ([]*nadv1.NetworkAttachmentDefinition, error) {
	nadSel, err := metav1.LabelSelectorAsSelector(nadSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid network selector %v: %w", nadSelector.String(), err)
	}
	if namespaceSelector == nil || namespaceSelector.Size() == 0 {
		return c.nadLister.List(nadSel)
	}
	namespaces, err := c.namespacesBySelector(namespaceSelector)
	if err != nil {
		return nil, err
	}
	var selectedNADs []*nadv1.NetworkAttachmentDefinition
	for _, namespace := range namespaces {
		nads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(nadSel)
		if err != nil {
			return nil, fmt.Errorf("failed to list NADs in namespace %s: %w", namespace, err)
		}
		selectedNADs = append(selectedNADs, nads...)
	}
	return selectedNADs, nil
}

func (c *Controller) namespacesBySelector(namespaceSelector *metav1.LabelSelector) ([]string, error) {
	nsSelector, err := metav1.LabelSelectorAsSelector(namespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid namespace selector %v: %w", namespaceSelector.String(), err)
	}
	namespaces, err := c.namespaceLister.List(nsSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	names := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		names = append(names, namespace.Name)
	}
	return names, nil
}

func isOwnedByCUDN(nad *nadv1.NetworkAttachmentDefinition) bool {
	owner := metav1.GetControllerOfNoCopy(nad)
	return owner != nil && owner.Kind == cudnKind.Kind && owner.APIVersion == cudnKind.GroupVersion().String()
}

func (c *Controller) namespaceHoldsNADs(namespace string) (bool, error) {
	if c.nadLister == nil {
		return false, nil
	}
	nads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
	if err != nil {
		return false, fmt.Errorf("failed to list NADs in namespace %s: %w", namespace, err)
	}
	return len(nads) > 0, nil
}
