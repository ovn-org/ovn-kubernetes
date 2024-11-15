package egressip

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/gaissmai/cidrtree"
	ocpconfigapi "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerscorev1 "k8s.io/client-go/informers/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipinformerv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/informers/externalversions/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func PlatformTypeIsEgressIPCloudProvider() bool {
	return config.Kubernetes.PlatformType == string(ocpconfigapi.AWSPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.GCPPlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.AzurePlatformType) ||
		config.Kubernetes.PlatformType == string(ocpconfigapi.OpenStackPlatformType)
}

// GetNodeEIPConfig attempts to generate EIP configuration from a nodes
// annotations. If the platform is running in the cloud, retrieve config info
// from node obj annotation added by Cloud Network Config Controller (CNCC). If
// not on a cloud platform (i.e. baremetal), retrieve from the node obj primary
// interface annotation.
func GetNodeEIPConfig(node *corev1.Node) (*util.ParsedNodeEgressIPConfiguration, error) {
	var parsedEgressIPConfig *util.ParsedNodeEgressIPConfiguration
	var err error
	if PlatformTypeIsEgressIPCloudProvider() {
		parsedEgressIPConfig, err = util.ParseCloudEgressIPConfig(node)
	} else {
		parsedEgressIPConfig, err = util.ParseNodePrimaryIfAddr(node)
	}
	if err != nil {
		return nil, fmt.Errorf("unable to generate egress IP config for node %s: %w", node.Name, err)
	}
	return parsedEgressIPConfig, nil
}

type watchFactory interface {
	NamespaceInformer() informerscorev1.NamespaceInformer
	EgressIPInformer() egressipinformerv1.EgressIPInformer
}

// IsEgressIPLocal checks if the EgressIP is hosted on a local network either
// through a primary or secondary interface.
func IsEgressIPLocal(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := false
	primary, secondary, _, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return primary || secondary, err
}

// IsEgressIPLocalOrAdvertised checks if the EgressIP can be hosted on the
// given node: either because it is local to a network directly connected to the
// node through a primary or secondary interface, or because it is configured to
// be advertised for any of its selected namespaces on the given node.
func IsEgressIPLocalOrAdvertised(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := true
	primary, secondary, advertised, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return primary || secondary || advertised, err
}

// IsEgressIPPrimaryOrAdvertised checks if the EgressIP can be hosted on a network
// managed by OVN on the given node: either because it can be hosted on the
// network directly connected on the primary interface or because it is
// configured to be advertised for any of its selected namespaces.
func IsEgressIPPrimaryOrAdvertised(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
) (bool, error) {
	checkAdvertisements := true
	primary, _, advertised, _, err := getEgressIPPrimarySecondaryAdvertised(wf, nm, eipCponfig, node, eip, ip, checkAdvertisements)
	return primary || advertised, err
}

// GetEgressIPPrimaryOrSecondaryNetwork attempts to retrieve a network directly
// connected to the given node that contains EgressIP. Check the OVN network
// first as represented by parameter eIPConfig, and if no match is found, and if
// not in a cloud environment, check secondary host networks.
func GetEgressIPPrimaryOrSecondaryNetwork(node *corev1.Node, eIPConfig *util.ParsedNodeEgressIPConfiguration, eIP net.IP) (string, error) {
	network := getEgressIPPrimaryNetwork(eIPConfig, eIP)
	if network != "" {
		return network, nil
	}
	return getEgressIPSecondaryNetwork(node, eIP)
}

// GetEgressIPAdvertisedNodes return the common nodes on which an EgressIP is
// advertised for all the selected namespaces. If the EgressIP is not advertised
// at all it return nil. If it is not advertised in at least one common node for
// all the selected namespaces it return an empty set.
func GetEgressIPAdvertisedNodes(wf watchFactory, nm networkmanager.Interface, name string) (sets.Set[string], error) {
	if !AdvertisementsEnabled() {
		return nil, nil
	}

	var advertisedOnNodes sets.Set[string]
	eip, err := wf.EgressIPInformer().Lister().Get(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get EgressIP %s: %v", name, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&eip.Spec.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse EgressIP %s namespace selector: %v", name, err)
	}
	selectedNs, err := wf.NamespaceInformer().Lister().List(selector)
	if err != nil {
		return nil, fmt.Errorf("failed to list selected namespaces for EgressIP %s: %v", name, err)
	}
	for _, ns := range selectedNs {
		network := nm.GetActiveNetworkForNamespaceFast(ns.Name)
		nodes := network.GetEgressIPAdvertisedNodes()
		switch {
		case len(nodes) == 0:
			continue
		case advertisedOnNodes == nil:
			advertisedOnNodes = sets.New(nodes...)
		default:
			advertisedOnNodes = advertisedOnNodes.Intersection(sets.New(nodes...))
		}
	}

	return advertisedOnNodes, nil
}

func ReconcileEgressIPNetworkChangeAnyNode(old, new util.NetInfo) bool {
	return reconcileEgressIPNetworkChange(nil, old, new)
}

func ReconcileEgressIPNetworkChangeOnNodes(nodes []string, old, new util.NetInfo) bool {
	return reconcileEgressIPNetworkChange(nodes, old, new)
}

func AdvertisementsEnabled() bool {
	return util.IsRouteAdvertisementsEnabled() && config.OVNKubernetesFeature.EnableEgressIP
}

func getEgressIPPrimarySecondaryAdvertised(
	wf watchFactory,
	nm networkmanager.Interface,
	eipCponfig *util.ParsedNodeEgressIPConfiguration,
	node *corev1.Node,
	eip string,
	ip net.IP,
	checkAdvertisements bool,
) (primary, secondary, advertised bool, network string, err error) {
	network = getEgressIPPrimaryNetwork(eipCponfig, ip)
	if network != "" {
		primary = true
		return
	}
	network, err = getEgressIPSecondaryNetwork(node, ip)
	if err != nil {
		return
	}
	if network != "" {
		secondary = true
		return
	}
	if !AdvertisementsEnabled() || !checkAdvertisements {
		return
	}
	advertised, err = isAdvertisedOnNode(wf, nm, eip, node.Name)
	return
}

func reconcileEgressIPNetworkChange(nodes []string, old, new util.NetInfo) bool {
	getNamespacesAndEgressIPNodes := func(net util.NetInfo) (sets.Set[string], sets.Set[string]) {
		ns, nodes := sets.New[string](), sets.New[string]()
		if net != nil && (net.IsPrimaryNetwork() || net.IsDefault()) {
			ns.Insert(net.GetNamespaces()...)
			nodes.Insert(net.GetEgressIPAdvertisedNodes()...)
		}
		return ns, nodes
	}

	oldNs, oldNodes := getNamespacesAndEgressIPNodes(old)
	newNs, newNodes := getNamespacesAndEgressIPNodes(new)
	hadNsChanges := !oldNs.Equal(newNs)
	var hadNodeChanges bool
	switch {
	case len(nodes) == 0:
		hadNodeChanges = !oldNodes.Equal(newNodes)
	case len(nodes) > 0:
		hadNodeChanges = oldNodes.HasAny(nodes...) != newNodes.HasAny(nodes...)

	}

	if !hadNsChanges && !hadNodeChanges {
		return false
	}
	return true
}

func getEgressIPPrimaryNetwork(eIPConfig *util.ParsedNodeEgressIPConfiguration, ip net.IP) string {
	if eIPConfig.V4.Net != nil && eIPConfig.V4.Net.Contains(ip) {
		return eIPConfig.V4.Net.String()
	}
	if eIPConfig.V6.Net != nil && eIPConfig.V6.Net.Contains(ip) {
		return eIPConfig.V6.Net.String()
	}
	return ""
}

// getEgressIPSecondaryNetwork attempts to find a secondary host network
// to host the argument IP and includes only global unicast addresses.
func getEgressIPSecondaryNetwork(node *corev1.Node, ip net.IP) (string, error) {
	// Do not attempt to check if a secondary host network may host an EIP if we
	// are in a cloud environment
	if util.PlatformTypeIsEgressIPCloudProvider() {
		return "", nil
	}
	networks, err := util.ParseNodeHostCIDRsExcludeOVNNetworks(node)
	if err != nil {
		return "", fmt.Errorf("failed to get host-cidrs annotation excluding OVN networks for node %s: %v",
			node.Name, err)
	}
	cidrs, err := makeCIDRs(networks...)
	if err != nil {
		return "", err
	}
	if len(cidrs) == 0 {
		return "", nil
	}
	isIPv6 := ip.To4() == nil
	cidrs = filterIPVersion(cidrs, isIPv6)
	lpmTree := cidrtree.New(cidrs...)
	for _, prefix := range cidrs {
		if !prefix.Addr().IsGlobalUnicast() {
			lpmTree.Delete(prefix)
		}
	}
	addr, err := netip.ParseAddr(ip.String())
	if err != nil {
		return "", fmt.Errorf("failed to convert IP %s to netip address: %v", ip.String(), err)
	}
	match, found := lpmTree.Lookup(addr)
	if !found {
		return "", nil
	}
	return match.String(), nil
}

func isAdvertisedOnNode(wf watchFactory, nm networkmanager.Interface, eip, node string) (bool, error) {
	if !AdvertisementsEnabled() {
		return false, nil
	}
	egressip, err := wf.EgressIPInformer().Lister().Get(eip)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get Egress IP %s: %v", eip, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(&egressip.Spec.NamespaceSelector)
	if err != nil {
		return false, fmt.Errorf("failed to parse Egress IP %s namespace selector: %v", metav1.NamespaceAll, err)
	}
	selectedNs, err := wf.NamespaceInformer().Lister().List(selector)
	if err != nil {
		return false, fmt.Errorf("failed to list selected namespaces of Egress IP %s: %v", eip, err)
	}
	for _, ns := range selectedNs {
		network := nm.GetActiveNetworkForNamespaceFast(ns.Name)
		advertisements := network.GetEgressIPAdvertisedVRFs()
		advertised := len(advertisements[node]) > 0
		if advertised {
			return true, nil
		}
	}
	return false, nil
}

func makeCIDRs(s ...string) (cidrs []netip.Prefix, err error) {
	for _, cidrString := range s {
		prefix, err := netip.ParsePrefix(cidrString)
		if err != nil {
			return nil, err
		}
		cidrs = append(cidrs, prefix)
	}
	return cidrs, nil
}

func filterIPVersion(cidrs []netip.Prefix, v6 bool) []netip.Prefix {
	validCIDRs := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		if cidr.Addr().Is4() && v6 {
			continue
		}
		if cidr.Addr().Is6() && !v6 {
			continue
		}
		validCIDRs = append(validCIDRs, cidr)
	}
	return validCIDRs
}
