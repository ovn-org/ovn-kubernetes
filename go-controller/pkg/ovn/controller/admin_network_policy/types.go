package adminnetworkpolicy

import (
	"fmt"
	"net"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"

	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// NOTE: Iteration v1 of ANP will only support upto 100 ANPs
// We will use priority range from 30000 (0) to 20000 (99) ACLs (both inclusive, note that these ACLs will be in tier1)
// In order to support more in the future, we will need to fix priority range in OVS
// See https://bugzilla.redhat.com/show_bug.cgi?id=2175752 for more details.
// NOTE: A cluster can have only BANP at a given time as defined by upstream KEP.
const (
	ANPFlowStartPriority            = 30000
	ANPMaxRulesPerObject            = 100
	ovnkSupportedPriorityUpperBound = 99   // corresponds to 20100 ACL priority
	BANPFlowPriority                = 1750 // down to 1651 (both inclusive, note that these ACLs will be in tier3)
)

type adminNetworkPolicySubject struct {
	namespaceSelector labels.Selector
	podSelector       labels.Selector
	// map of namespaces matching the provided namespaceSelector
	// {K: namespace name; V: {set of pods matching the provided podSelector}}
	namespaces map[string]sets.Set[string]
	// all the LSP port UUIDs of the subject pods selected by this ANP
	// Since nbdb.PortGroup.LSP stores the UUIDs of the LSP's storing it
	// in the cache makes it easier to compute the difference between
	// current set of UUIDs and desired set of UUIDs and do one set of
	// transact ops calculation. If not, for every pod/namespace update
	// we would need to do a lookup in the libovsdb cache for the ns_name
	// LSP index.
	// {K: networkName; V: {set of podLSPs matching the provided podSelector}}
	podPorts map[string]sets.Set[string]
}

type adminNetworkPolicyPeer struct {
	namespaceSelector labels.Selector
	podSelector       labels.Selector
	nodeSelector      labels.Selector
	// NOTE: We store the pods and nodes info since on a namespace, pod, node
	// delete event we don't get the object/labels and only have the key
	// map of namespaces matching the provided namespaceSelector
	// {K: namespace name; V: {set of pods matching the provided podSelector}}
	namespaces map[string]sets.Set[string]
	// set of nodes matching the provided nodeSelector
	nodes    sets.Set[string]
	networks sets.Set[string]
}

type gressRule struct {
	name string
	// priority is determined based on order in the list and calculated using adminNetworkPolicyState.priority
	priority int32
	// gressIndex tracks the index of this rule
	gressIndex  int32
	gressPrefix string
	// NOTE: Action here is the corresponding OVN action for
	// anpapi.AdminNetworkPolicyRuleAction
	action string
	peers  []*adminNetworkPolicyPeer
	ports  []*libovsdbutil.NetworkPolicyPort
	// all the peerAddresses of the peer entities (podIPs, nodeIPs, CIDR ranges) selected by this ANP Rule
	// for all networks
	// {K: networkName; V: {set of IPs}}
	peerAddresses map[string]sets.Set[string]
	// saves NamedPort representation;
	// key is the name of the Port
	// value is an array of possible representations of this port (relevance wrt to rule, peers)
	namedPorts map[string][]libovsdbutil.NamedNetworkPolicyPort
}

// adminNetworkPolicyState is the cache that keeps the state of a single
// admin network policy in the cluster with name being unique
type adminNetworkPolicyState struct {
	// name of the admin network policy (unique across cluster)
	name string
	// priority is anp.Spec.Priority (since BANP does not have priority, we hardcode to 0)
	anpPriority int32
	// priority is the OVN priority equivalent of anp.Spec.Priority
	// (since BANP does not have priority, we hardcode to BANPFlowPriority)
	ovnPriority int32
	// subject stores the objects needed to track .Spec.Subject changes
	subject *adminNetworkPolicySubject
	// ingressRules stores the objects needed to track .Spec.Ingress changes
	ingressRules []*gressRule
	// egressRules stores the objects needed to track .Spec.Egress changes
	egressRules []*gressRule
	// aclLoggingParams stores the log levels for the ACLs created for this ANP
	// this is based off the "k8s.ovn.org/acl-logging" annotation set on the ANP's
	aclLoggingParams *libovsdbutil.ACLLoggingLevels
	// set of networkNames selected either as part of ANP's
	// subject OR peers in any one of the rules
	managedNetworks sets.Set[string]
}

// newAdminNetworkPolicyState takes the provided ANP API object and creates a new corresponding
// adminNetworkPolicyState cache object for that API object.
func newAdminNetworkPolicyState(raw *anpapi.AdminNetworkPolicy) (*adminNetworkPolicyState, error) {
	anp := &adminNetworkPolicyState{
		name:            raw.Name,
		anpPriority:     raw.Spec.Priority,
		ovnPriority:     (ANPFlowStartPriority - raw.Spec.Priority*ANPMaxRulesPerObject),
		ingressRules:    make([]*gressRule, 0),
		egressRules:     make([]*gressRule, 0),
		managedNetworks: make(sets.Set[string]),
	}
	var err error
	anp.subject, err = newAdminNetworkPolicySubject(raw.Spec.Subject)
	if err != nil {
		return nil, err
	}

	var errs []error
	for i, rule := range raw.Spec.Ingress {
		anpRule, err := newAdminNetworkPolicyIngressRule(rule, int32(i), anp.ovnPriority-int32(i))
		if err != nil {
			err = fmt.Errorf("cannot create anp ingress Rule %d in ANP %s: %w", i, raw.Name, err)
			errs = append(errs, err)
			continue
		}
		anp.ingressRules = append(anp.ingressRules, anpRule)
	}
	for i, rule := range raw.Spec.Egress {
		anpRule, err := newAdminNetworkPolicyEgressRule(rule, int32(i), anp.ovnPriority-int32(i))
		if err != nil {
			err = fmt.Errorf("cannot create anp egress Rule %d in ANP %s: %w", i, raw.Name, err)
			errs = append(errs, err)
			continue
		}
		anp.egressRules = append(anp.egressRules, anpRule)
	}
	anp.aclLoggingParams, err = getACLLoggingLevelsForANP(raw.Annotations)
	if err != nil {
		err = fmt.Errorf("cannot parse ANP ACL logging annotation, disabling it for ANP %s: %w", raw.Name, err)
		errs = append(errs, err)
	}
	klog.V(5).Infof("Logging parameters for ANP %s are Allow=%s/Deny=%s/Pass=%s", raw.Name,
		anp.aclLoggingParams.Allow, anp.aclLoggingParams.Deny, anp.aclLoggingParams.Pass)
	return anp, utilerrors.Join(errs...)
}

// newAdminNetworkPolicySubject takes the provided ANP API Subject and creates a new corresponding
// adminNetworkPolicySubject cache object for that Subject.
func newAdminNetworkPolicySubject(raw anpapi.AdminNetworkPolicySubject) (*adminNetworkPolicySubject, error) {
	var subject *adminNetworkPolicySubject

	if raw.Namespaces != nil {
		subjectNamespaceSelector, err := metav1.LabelSelectorAsSelector(raw.Namespaces)
		if err != nil {
			return nil, err
		}
		if !subjectNamespaceSelector.Empty() {
			subject = &adminNetworkPolicySubject{
				namespaceSelector: subjectNamespaceSelector,
				podSelector:       labels.Everything(), // it means match all pods within the provided namespaces
			}
		} else {
			subject = &adminNetworkPolicySubject{
				namespaceSelector: labels.Everything(), // it means match all namespaces in the cluster
				podSelector:       labels.Everything(), // it means match all pods within the provided namespaces
			}
		}
	} else if raw.Pods != nil {
		// anp.Spec.Subject.Namespaces is not set; anp.Spec.Subject.Pods is set instead
		subjectNamespaceSelector, err := metav1.LabelSelectorAsSelector(&raw.Pods.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		subjectPodSelector, err := metav1.LabelSelectorAsSelector(&raw.Pods.PodSelector)
		if err != nil {
			return nil, err
		}
		subject = &adminNetworkPolicySubject{
			namespaceSelector: subjectNamespaceSelector,
			podSelector:       subjectPodSelector,
		}
	}
	return subject, nil
}

// newAdminNetworkPolicyPort takes the provided ANP API Port and creates a new corresponding
// adminNetworkPolicyPort cache object for that Port.
func newAdminNetworkPolicyPort(raw anpapi.AdminNetworkPolicyPort) *libovsdbutil.NetworkPolicyPort {
	var anpPort *libovsdbutil.NetworkPolicyPort
	if raw.PortNumber != nil {
		anpPort = libovsdbutil.GetNetworkPolicyPort(raw.PortNumber.Protocol, raw.PortNumber.Port, 0)
	} else {
		anpPort = libovsdbutil.GetNetworkPolicyPort(raw.PortRange.Protocol, raw.PortRange.Start, raw.PortRange.End)
	}
	return anpPort
}

// newAdminNetworkPolicyPeer takes the provided ANP API Peer and creates a new corresponding
// adminNetworkPolicyPeer cache object for that Peer.
func newAdminNetworkPolicyPeer(rawNamespaces *metav1.LabelSelector, rawPods *anpapi.NamespacedPod) (*adminNetworkPolicyPeer, error) {
	var anpPeer *adminNetworkPolicyPeer
	if rawNamespaces != nil {
		peerNamespaceSelector, err := metav1.LabelSelectorAsSelector(rawNamespaces)
		if err != nil {
			return nil, err
		}
		if !peerNamespaceSelector.Empty() {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: peerNamespaceSelector,
				podSelector:       labels.Everything(), // it means match all pods within the provided namespaces
			}
		} else {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: labels.Everything(), // it means match all namespaces in the cluster
				podSelector:       labels.Everything(), // it means match all pods within the provided namespaces
			}
		}
	} else if rawPods != nil {
		peerNamespaceSelector, err := metav1.LabelSelectorAsSelector(&rawPods.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if peerNamespaceSelector.Empty() {
			peerNamespaceSelector = labels.Everything()
		}
		peerPodSelector, err := metav1.LabelSelectorAsSelector(&rawPods.PodSelector)
		if err != nil {
			return nil, err
		}
		if peerPodSelector.Empty() {
			peerPodSelector = labels.Everything()
		}
		anpPeer = &adminNetworkPolicyPeer{
			namespaceSelector: peerNamespaceSelector,
			podSelector:       peerPodSelector,
		}
	}
	anpPeer.nodeSelector = labels.Nothing() // Nodes are not supported as ingress peers and for egress peers this will get overwritten
	return anpPeer, nil
}

// newAdminNetworkPolicyIngressPeer takes the provided ANP API Peer and creates a new corresponding
// adminNetworkPolicyPeer cache object for that Peer.
func newAdminNetworkPolicyIngressPeer(raw anpapi.AdminNetworkPolicyIngressPeer) (*adminNetworkPolicyPeer, error) {
	return newAdminNetworkPolicyPeer(raw.Namespaces, raw.Pods)
}

// newAdminNetworkPolicyEgressPeer takes the provided ANP API Peer and creates a new corresponding
// adminNetworkPolicyPeer cache object for that Peer.
func newAdminNetworkPolicyEgressPeer(raw anpapi.AdminNetworkPolicyEgressPeer) (*adminNetworkPolicyPeer, error) {
	var anpPeer *adminNetworkPolicyPeer
	if raw.Namespaces != nil || raw.Pods != nil {
		return newAdminNetworkPolicyPeer(raw.Namespaces, raw.Pods)
	} else if raw.Nodes != nil {
		peerNodeSelector, err := metav1.LabelSelectorAsSelector(raw.Nodes)
		if err != nil {
			return nil, err
		}
		if !peerNodeSelector.Empty() {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: labels.Nothing(), // doesn't match any namespaces
				podSelector:       labels.Nothing(), // doesn't match any pods
				nodeSelector:      peerNodeSelector,
			}
		} else {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: labels.Nothing(),    // doesn't match any namespaces
				podSelector:       labels.Nothing(),    // doesn't match any pods
				nodeSelector:      labels.Everything(), // matches all nodes
			}
		}
	} else if len(raw.Networks) > 0 {
		anpPeer = &adminNetworkPolicyPeer{
			namespaceSelector: labels.Nothing(), // doesn't match any namespaces
			podSelector:       labels.Nothing(), // doesn't match any pods
			nodeSelector:      labels.Nothing(), // doesn't match any nodes
			networks:          make(sets.Set[string]),
		}
		for _, cidr := range raw.Networks {
			_, ipNet, err := net.ParseCIDR(string(cidr))
			if err != nil {
				return nil, err
			}
			anpPeer.networks.Insert(ipNet.String())
		}
	}
	return anpPeer, nil
}

// newAdminNetworkPolicyIngressRule takes the provided ANP API Ingress Rule and creates a new corresponding
// gressRule cache object for that Rule.
func newAdminNetworkPolicyIngressRule(raw anpapi.AdminNetworkPolicyIngressRule, index, priority int32) (*gressRule, error) {
	anpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		gressIndex:  index,
		action:      GetACLActionForANPRule(raw.Action),
		gressPrefix: string(libovsdbutil.ACLIngress),
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*libovsdbutil.NetworkPolicyPort, 0),
		namedPorts:  make(map[string][]libovsdbutil.NamedNetworkPolicyPort, 0),
	}
	for _, peer := range raw.From {
		anpPeer, err := newAdminNetworkPolicyIngressPeer(peer)
		if err != nil {
			return nil, err
		}
		anpRule.peers = append(anpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			if port.NamedPort != nil {
				anpRule.namedPorts[*port.NamedPort] = []libovsdbutil.NamedNetworkPolicyPort{}
			} else {
				anpPort := newAdminNetworkPolicyPort(port)
				anpRule.ports = append(anpRule.ports, anpPort)
			}
		}
	}

	return anpRule, nil
}

// newAdminNetworkPolicyEgressRule takes the provided ANP API Egress Rule and creates a new corresponding
// gressRule cache object for that Rule.
func newAdminNetworkPolicyEgressRule(raw anpapi.AdminNetworkPolicyEgressRule, index, priority int32) (*gressRule, error) {
	anpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		gressIndex:  index,
		action:      GetACLActionForANPRule(raw.Action),
		gressPrefix: string(libovsdbutil.ACLEgress),
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*libovsdbutil.NetworkPolicyPort, 0),
		namedPorts:  make(map[string][]libovsdbutil.NamedNetworkPolicyPort, 0),
	}
	for _, peer := range raw.To {
		anpPeer, err := newAdminNetworkPolicyEgressPeer(peer)
		if err != nil {
			return nil, err
		}
		anpRule.peers = append(anpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			if port.NamedPort != nil {
				anpRule.namedPorts[*port.NamedPort] = []libovsdbutil.NamedNetworkPolicyPort{}
			} else {
				anpPort := newAdminNetworkPolicyPort(port)
				anpRule.ports = append(anpRule.ports, anpPort)
			}
		}
	}
	return anpRule, nil
}

// newBaselineAdminNetworkPolicyState takes the provided BANP API object and creates a new corresponding
// adminNetworkPolicyState cache object for that API object.
func newBaselineAdminNetworkPolicyState(raw *anpapi.BaselineAdminNetworkPolicy) (*adminNetworkPolicyState, error) {
	banp := &adminNetworkPolicyState{
		name:            raw.Name,
		anpPriority:     0, // since BANP does not have priority, we hardcode to 0
		ovnPriority:     BANPFlowPriority,
		ingressRules:    make([]*gressRule, 0),
		egressRules:     make([]*gressRule, 0),
		managedNetworks: make(sets.Set[string]),
	}
	var err error
	banp.subject, err = newAdminNetworkPolicySubject(raw.Spec.Subject)
	if err != nil {
		return nil, err
	}
	var errs []error
	for i, rule := range raw.Spec.Ingress {
		banpRule, err := newBaselineAdminNetworkPolicyIngressRule(rule, int32(i), BANPFlowPriority-int32(i))
		if err != nil {
			err = fmt.Errorf("cannot create banp ingress Rule %d in BANP %s: %w", i, raw.Name, err)
			errs = append(errs, err)
			continue
		}
		banp.ingressRules = append(banp.ingressRules, banpRule)
	}
	for i, rule := range raw.Spec.Egress {
		banpRule, err := newBaselineAdminNetworkPolicyEgressRule(rule, int32(i), BANPFlowPriority-int32(i))
		if err != nil {
			err = fmt.Errorf("cannot create banp egress Rule %d in BANP %s: %w", i, raw.Name, err)
			errs = append(errs, err)
			continue
		}
		banp.egressRules = append(banp.egressRules, banpRule)
	}
	banp.aclLoggingParams, err = getACLLoggingLevelsForANP(raw.Annotations)
	if err != nil {
		err = fmt.Errorf("cannot parse BANP ACL logging annotation, disabling it for BANP %s: %w", raw.Name, err)
		errs = append(errs, err)
	}
	klog.V(5).Infof("Logging parameters for BANP %s are Allow=%s/Deny=%s", raw.Name,
		banp.aclLoggingParams.Allow, banp.aclLoggingParams.Deny)
	return banp, utilerrors.Join(errs...)
}

// newBaselineAdminNetworkPolicyIngressRule takes the provided BANP API Ingress Rule and creates a new corresponding
// gressRule cache object for that Rule.
func newBaselineAdminNetworkPolicyIngressRule(raw anpapi.BaselineAdminNetworkPolicyIngressRule, index, priority int32) (*gressRule, error) {
	banpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		gressIndex:  index,
		action:      GetACLActionForBANPRule(raw.Action),
		gressPrefix: string(libovsdbutil.ACLIngress),
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*libovsdbutil.NetworkPolicyPort, 0),
		namedPorts:  make(map[string][]libovsdbutil.NamedNetworkPolicyPort, 0),
	}
	for _, peer := range raw.From {
		anpPeer, err := newAdminNetworkPolicyIngressPeer(peer)
		if err != nil {
			return nil, err
		}
		banpRule.peers = append(banpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			if port.NamedPort != nil {
				banpRule.namedPorts[*port.NamedPort] = []libovsdbutil.NamedNetworkPolicyPort{}
			} else {
				anpPort := newAdminNetworkPolicyPort(port)
				banpRule.ports = append(banpRule.ports, anpPort)
			}
		}
	}

	return banpRule, nil
}

// newBaselineAdminNetworkPolicyEgressRule takes the provided BANP API Egress Rule and creates a new corresponding
// gressRule cache object for that Rule.
func newBaselineAdminNetworkPolicyEgressRule(raw anpapi.BaselineAdminNetworkPolicyEgressRule, index, priority int32) (*gressRule, error) {
	banpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		gressIndex:  index,
		action:      GetACLActionForBANPRule(raw.Action),
		gressPrefix: string(libovsdbutil.ACLEgress),
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*libovsdbutil.NetworkPolicyPort, 0),
		namedPorts:  make(map[string][]libovsdbutil.NamedNetworkPolicyPort, 0),
	}
	for _, peer := range raw.To {
		banpPeer, err := newAdminNetworkPolicyEgressPeer(peer)
		if err != nil {
			return nil, err
		}
		banpRule.peers = append(banpRule.peers, banpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			if port.NamedPort != nil {
				banpRule.namedPorts[*port.NamedPort] = []libovsdbutil.NamedNetworkPolicyPort{}
			} else {
				anpPort := newAdminNetworkPolicyPort(port)
				banpRule.ports = append(banpRule.ports, anpPort)
			}
		}
	}
	return banpRule, nil
}
