package admin_network_policy

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// NOTE: Iteration v1 of ANP will only support upto 100 ANPs
// We will use priority range from 30000 to 20000 ACLs (both inclusive, note that these ACLs will be in tier1)
// In order to support more in the future, we will need to fix priority range in OVS
// See https://bugzilla.redhat.com/show_bug.cgi?id=2175752 for more details.
const (
	ANPFlowStartPriority = 30000
	ANPMaxRulesPerObject = 100
	ANPIngressPrefix     = "ANPIngress"
	ANPEgressPrefix      = "ANPEgress"
	ANPExternalIDKey     = "AdminNetworkPolicy" // key set on port-groups to identify which ANP it belongs to
)

// TODO: Double check how empty selector means all labels match works
type adminNetworkPolicySubject struct {
	namespaceSelector labels.Selector
	podSelector       labels.Selector
	// map of pods matching the provided namespaceSelector and podSelector
	pods *sync.Map // {K:namespace name V:syncMap{K:pod's name V:-> LSP UUID}}
	// map of namedPorts; used only for namedPort logic. NOTE: This is expensive
	namedPorts *sync.Map // {K:namedPortName V:syncMap{K:podIP: V:adminNetworkPolicyPort}}
}

// TODO: Implement sameLabels & notSameLabels
type adminNetworkPolicyPeer struct {
	namespaceSelector labels.Selector
	podSelector       labels.Selector
	// map of pods matching the provided namespaceSelector and podSelector
	pods *sync.Map // {K:namespace name V:syncMap{K:pod's name V:-> ips in the addrSet}}
	// map of namedPorts; used only for namedPort logic. NOTE: This is expensive
	namedPorts *sync.Map // {K:namedPortName V:syncMap{K:podIP: V:adminNetworkPolicyPort}}
}

type adminNetworkPolicyPort struct {
	protocol string
	port     int32  // will store startPort if its a range; if this is set then name will be empty
	endPort  int32  // if this is set then name will be empty
	name     string // will store namedPort value; if this is set then port and endPort will be 0
}

type gressRule struct {
	name        string
	priority    int32 // determined based on order in the list
	gressPrefix string
	action      anpapi.AdminNetworkPolicyRuleAction
	peers       []*adminNetworkPolicyPeer
	ports       []*adminNetworkPolicyPort
	// address-set that contains the IPs of all peers in this rule
	addrSet addressset.AddressSet
}

type adminNetworkPolicy struct {
	sync.RWMutex
	name         string
	priority     int32
	subject      *adminNetworkPolicySubject
	ingressRules []*gressRule
	egressRules  []*gressRule
	stale        bool
}

// shallow copies the ANP object provided.
func cloneANP(raw *anpapi.AdminNetworkPolicy) (*adminNetworkPolicy, error) {
	anp := &adminNetworkPolicy{
		name:         raw.Name,
		priority:     (ANPFlowStartPriority - raw.Spec.Priority*ANPMaxRulesPerObject),
		ingressRules: make([]*gressRule, 0),
		egressRules:  make([]*gressRule, 0),
	}
	var err error
	anp.subject, err = cloneANPSubject(raw.Spec.Subject)
	if err != nil {
		return nil, err
	}

	addErrors := errors.New("")
	for i, rule := range raw.Spec.Ingress {
		anpRule, err := cloneANPIngressRule(rule, anp.priority-int32(i))
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create anp ingress Rule %d in ANP %s - %v",
				i, raw.Name, err)
			continue
		}
		anp.ingressRules = append(anp.ingressRules, anpRule)
	}
	for i, rule := range raw.Spec.Egress {
		anpRule, err := cloneANPEgressRule(rule, anp.priority-int32(i))
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create anp egress Rule %d in ANP %s - %v",
				i, raw.Name, err)
			continue
		}
		anp.egressRules = append(anp.egressRules, anpRule)
	}

	if addErrors.Error() == "" {
		addErrors = nil
	}
	return anp, addErrors
}

func cloneANPSubject(raw anpapi.AdminNetworkPolicySubject) (*adminNetworkPolicySubject, error) {
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

func getPortProtocol(proto v1.Protocol) string {
	var protocol string
	switch proto {
	case v1.ProtocolTCP:
		protocol = "tcp"
	case v1.ProtocolSCTP:
		protocol = "sctp"
	case v1.ProtocolUDP:
		protocol = "udp"
	}
	return protocol
}

func cloneANPPort(raw anpapi.AdminNetworkPolicyPort) *adminNetworkPolicyPort {
	anpPort := adminNetworkPolicyPort{}
	if raw.PortNumber != nil {
		anpPort.protocol = getPortProtocol(raw.PortNumber.Protocol)
		anpPort.port = raw.PortNumber.Port
	} else if raw.NamedPort != nil {
		anpPort.name = *raw.NamedPort
	} else {
		anpPort.protocol = getPortProtocol(raw.PortRange.Protocol)
		anpPort.port = raw.PortRange.Start
		anpPort.port = raw.PortRange.End
	}
	return &anpPort
}

func cloneANPPeer(raw anpapi.AdminNetworkPolicyPeer) (*adminNetworkPolicyPeer, error) {
	var anpPeer *adminNetworkPolicyPeer
	if raw.Namespaces != nil {
		peerNamespaceSelector, err := metav1.LabelSelectorAsSelector(raw.Namespaces.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if !peerNamespaceSelector.Empty() {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: peerNamespaceSelector,
				// TODO: See if it makes sense to just use the namespace address-sets we have in case the podselector is empty meaning all pods.
				podSelector: labels.Everything(), // it means match all pods within the provided namespaces
			}
		} else {
			anpPeer = &adminNetworkPolicyPeer{
				namespaceSelector: labels.Everything(), // it means match all namespaces in the cluster
				// TODO: See if it makes sense to just use the namespace address-sets we have in case the podselector is empty meaning all pods.
				podSelector: labels.Everything(), // it means match all pods within the provided namespaces
			}
		}
	} else if raw.Pods != nil {
		peerNamespaceSelector, err := metav1.LabelSelectorAsSelector(raw.Pods.Namespaces.NamespaceSelector)
		if err != nil {
			return nil, err
		}
		if peerNamespaceSelector.Empty() {
			peerNamespaceSelector = labels.Everything()
		}
		peerPodSelector, err := metav1.LabelSelectorAsSelector(&raw.Pods.PodSelector)
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
	return anpPeer, nil
}

// shallow copies the ANPRule objects provided.
func cloneANPIngressRule(raw anpapi.AdminNetworkPolicyIngressRule, priority int32) (*gressRule, error) {
	anpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		action:      raw.Action,
		gressPrefix: ANPIngressPrefix,
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*adminNetworkPolicyPort, 0),
	}
	for _, peer := range raw.From {
		anpPeer, err := cloneANPPeer(peer)
		if err != nil {
			return nil, err
		}
		anpRule.peers = append(anpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			anpPort := cloneANPPort(port)
			anpRule.ports = append(anpRule.ports, anpPort)
		}
	}

	return anpRule, nil
}

// shallow copies the ANPRule objects provided.
func cloneANPEgressRule(raw anpapi.AdminNetworkPolicyEgressRule, priority int32) (*gressRule, error) {
	anpRule := &gressRule{
		name:        raw.Name,
		priority:    priority,
		action:      raw.Action,
		gressPrefix: ANPEgressPrefix,
		peers:       make([]*adminNetworkPolicyPeer, 0),
		ports:       make([]*adminNetworkPolicyPort, 0),
	}
	for _, peer := range raw.To {
		anpPeer, err := cloneANPPeer(peer)
		if err != nil {
			return nil, err
		}
		anpRule.peers = append(anpRule.peers, anpPeer)
	}
	if raw.Ports != nil {
		for _, port := range *raw.Ports {
			anpPort := cloneANPPort(port)
			anpRule.ports = append(anpRule.ports, anpPort)
		}
	}
	return anpRule, nil
}

func (c *Controller) syncAdminNetworkPolicy(key string) error {
	startTime := time.Now()
	_, anpName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for Admin Network Policy %s", anpName)

	defer func() {
		klog.V(4).Infof("Finished syncing Admin Network Policy %s : %v", anpName, time.Since(startTime))
	}()

	anp, err := c.anpLister.Get(anpName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.Infof("SURYA %v", anp)
	// TODO1 (tssurya): Determine if its possible to make it more efficient by comparing with cache
	// if we do need to change anything or not instead of clearing and setting always for any update.
	// TODO2 (tssurya): Determine if it is more efficient by deleting and creation objects in same transaction??
	// Downside of that would be if there are lots of ANPs in cluster the transaction would be huge.
	// delete existing setup if any and recreate
	err = c.clearAdminNetworkPolicy(anpName)
	if err != nil {
		return err
	}

	if anp == nil { // it was deleted no need to process further
		return nil
	}

	return c.setAdminNetworkPolicy(anp)
}

func (c *Controller) setAdminNetworkPolicy(anpObj *anpapi.AdminNetworkPolicy) error {
	anp, err := cloneANP(anpObj)
	if err != nil {
		return err
	}

	anp.Lock()
	defer anp.Unlock()
	anp.stale = true // until we finish processing successfully

	if existingName, loaded := c.anpPriorityMap.Load(anp.priority); loaded {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, ANPWithSamePriorityExists)
		return fmt.Errorf("error attempting to add ANP %s with priority %d when, "+
			"it already has an ANP %s with the same priority", anpObj.Name, anpObj.Spec.Priority, existingName)
	}
	c.anpPriorityMap.Store(anp.priority, anpObj.Name)
	// there should not be an item in the cache for the given name
	// as we first attempt to delete before create.
	if _, loaded := c.anpCache.LoadOrStore(anpObj.Name, anp); loaded {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyAlreadyExistsInCache)
		return fmt.Errorf("error attempting to add ANP %s with priority %d when, "+
			"cache already has an ANP with the same name present", anpObj.Name, anpObj.Spec.Priority)
	}

	portGroupName, readableGroupName := getAdminNetworkPolicyPGName(anp.name)
	lsps, err := c.getPortsOfSubject(anp.subject)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyBuildPortGroupFailed)
		return fmt.Errorf("unable to fetch ports for anp %s: %w", anp.name, err)
	}
	ops := []ovsdb.Operation{}
	acls, err := c.buildANPACLs(anp, portGroupName)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyBuildACLFailed)
		return fmt.Errorf("unable to build acls for anp %s: %w", anp.name, err)
	}
	ops, err = libovsdbops.CreateOrUpdateACLsOps(c.nbClient, ops, acls...)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyCreateUpdateACLFailed)
		return fmt.Errorf("failed to create ACL ops: %v", err)
	}
	pgExternalIDs := map[string]string{ANPExternalIDKey: readableGroupName}
	pg := libovsdbops.BuildPortGroup(portGroupName, lsps, acls, pgExternalIDs)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(c.nbClient, ops, pg)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyCreateUpdatePortGroupFailed)
		return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(c.nbClient, ops)
	if err != nil {
		// we can ignore the error if status update doesn't succeed; best effort
		_ = c.updateANPStatusToNotReady(anpObj, PolicyTransactFailed)
		return fmt.Errorf("failed to run ovsdb txn to add ports and acls to port group: %v", err)
	}
	// we can ignore the error if status update doesn't succeed; best effort
	_ = c.updateANPStatusToReady(anpObj)
	anp.stale = false // we can mark it as "ready" now
	return nil
}

func getAdminNetworkPolicyPGName(name string) (hashedPGName, readablePGName string) {
	return util.HashForOVN(name), name
}

func (c *Controller) buildANPACLs(anp *adminNetworkPolicy, pgName string) ([]*nbdb.ACL, error) {
	acls := []*nbdb.ACL{}
	for _, ingressRule := range anp.ingressRules {
		acl, err := c.convertANPRuleToACL(anp, ingressRule, pgName)
		if err != nil {
			return nil, err
		}
		acls = append(acls, acl...)
	}
	for _, egressRule := range anp.egressRules {
		acl, err := c.convertANPRuleToACL(anp, egressRule, pgName)
		if err != nil {
			return nil, err
		}
		acls = append(acls, acl...)
	}

	return acls, nil
}

func (c *Controller) convertANPRuleToACL(anp *adminNetworkPolicy, rule *gressRule, pgName string) ([]*nbdb.ACL, error) {
	// create address-set
	// TODO (tssurya): Revisit this logic to see if its better to do one address-set per peer
	// and join them with OR if that is more perf efficient. Had briefly discussed this OVN team
	// We are not yet clear which is better since both have advantages and disadvantages.
	// Decide this after doing some scale runs.
	var err error
	rule.addrSet, err = c.createASForPeers(rule.peers, rule.priority, rule.gressPrefix, anp.name, libovsdbops.AddressSetAdminNetworkPolicy)
	if err != nil {
		return nil, fmt.Errorf("unable to create address set for "+
			" rule %s with priority %d: %w", rule.name, rule.priority, err)
	}
	// create match based on direction and address-set
	l3Match := constructMatchFromAddressSet(rule.gressPrefix, rule.addrSet)
	// create match based on rule type (ingress/egress) and port-group
	var lportMatch, match, direction string
	var options, extIDs map[string]string
	if rule.gressPrefix == ANPIngressPrefix {
		lportMatch = fmt.Sprintf("(outport == @%s)", pgName)
		direction = nbdb.ACLDirectionToLport
	} else {
		lportMatch = fmt.Sprintf("(inport == @%s)", pgName)
		direction = nbdb.ACLDirectionFromLport
		options = map[string]string{
			"apply-after-lb": "true",
		}
	}
	acls := []*nbdb.ACL{}
	if len(rule.ports) == 0 {
		extIDs = getANPRuleACLDbIDs(anp.name, rule.gressPrefix, fmt.Sprintf("%d", rule.priority), controllerName, libovsdbops.ACLAdminNetworkPolicy).GetExternalIDs()
		match = fmt.Sprintf("%s && %s", l3Match, lportMatch)
		acl := libovsdbops.BuildACL(
			getANPGressPolicyACLName(anp.name, rule.gressPrefix, fmt.Sprintf("%d", rule.priority)),
			direction,
			int(rule.priority),
			match,
			getACLActionForANP(rule.action),
			types.OvnACLLoggingMeter, // TODO: FIX THIS LATER
			nbdb.ACLSeverityDebug,    // TODO: FIX THIS LATER
			false,                    // TODO: FIX THIS LATER
			extIDs,
			options,
			types.DefaultANPACLTier,
		)
		acls = append(acls, acl)
		return acls, nil
	}
	for i, port := range rule.ports {
		extIDs = getANPRuleACLDbIDs(anp.name, rule.gressPrefix, fmt.Sprintf("%d.%d", rule.priority, i), controllerName, libovsdbops.ACLAdminNetworkPolicy).GetExternalIDs()
		if port.name == "" {
			l4Match := constructMatchFromPorts(port)
			match = fmt.Sprintf("%s && %s && %s", l3Match, lportMatch, l4Match)
		} else {
			// named Ports; this will not be scale efficient
			l4Matches, err := constructMatchFromNamedPorts(port.name, rule.peers, anp.subject, rule.gressPrefix)
			if err != nil {
				// it means either namespaces don't exist yet or pods with those namedPorts don't exist yet
				// in that case, warn the user and don't create any rules for the namedPort and simply continue
				klog.Warningf("Unable to create ACL for namedPort: %v", err)
				continue
			}
			match = fmt.Sprintf("%s && (%s)", lportMatch, l4Matches)
		}
		acl := libovsdbops.BuildACL(
			getANPGressPolicyACLName(anp.name, rule.gressPrefix, fmt.Sprintf("%d.%d", rule.priority, i)),
			direction,
			int(rule.priority),
			match,
			getACLActionForANP(rule.action),
			types.OvnACLLoggingMeter, // TODO: FIX THIS LATER
			nbdb.ACLSeverityDebug,    // TODO: FIX THIS LATER
			false,                    // TODO: FIX THIS LATER
			extIDs,
			options,
			types.DefaultANPACLTier,
		)
		acls = append(acls, acl)
	}
	return acls, nil
}

func getANPRuleACLDbIDs(name, gressPrefix, priority, controller string, idType *libovsdbops.ObjectIDsType) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(idType, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey:      name,
		libovsdbops.PolicyDirectionKey: gressPrefix,
		// priority is the unique id for address set within given gressPrefix
		libovsdbops.PriorityKey: priority,
	})
}

func getACLActionForANP(action anpapi.AdminNetworkPolicyRuleAction) string {
	var ovnACLAction string
	switch action {
	case anpapi.AdminNetworkPolicyRuleActionAllow:
		ovnACLAction = nbdb.ACLActionAllowRelated
	case anpapi.AdminNetworkPolicyRuleActionDeny:
		ovnACLAction = nbdb.ACLActionDrop
	case anpapi.AdminNetworkPolicyRuleActionPass:
		ovnACLAction = nbdb.ACLActionPass
	default:
		panic(fmt.Sprintf("Failed to build ANP ACL: unknown acl action %s", action))
	}
	return ovnACLAction
}

func (c *Controller) createASForPeers(peers []*adminNetworkPolicyPeer, priority int32, gressPrefix, anpName string, idType *libovsdbops.ObjectIDsType) (addressset.AddressSet, error) {
	var addrSet addressset.AddressSet
	var err error
	podsIps := []net.IP{}
	for _, peer := range peers {
		podsCache := sync.Map{}
		namedPortsCache := sync.Map{}
		// TODO: Double check how empty selector means all labels match works
		namespaces, err := c.anpNamespaceLister.List(peer.namespaceSelector)
		if err != nil {
			return nil, err
		}
		// TODO: Remove duplicate IPs from being added from multiple peers
		for _, namespace := range namespaces {
			namespaceCache, _ := podsCache.LoadOrStore(namespace.Name, &sync.Map{})
			podNamespaceLister := c.anpPodLister.Pods(namespace.Name)
			pods, err := podNamespaceLister.List(peer.podSelector)
			if err != nil {
				return nil, err
			}
			for _, pod := range pods {
				// we don't handle HostNetworked or completed pods; unscheduled pods shall be handled via pod update path
				if !util.PodWantsHostNetwork(pod) && !util.PodCompleted(pod) || !util.PodScheduled(pod) {
					if err := populateNamedPortsFromPod(&namedPortsCache, pod, true); err != nil {
						return nil, err
					}
					podIPs, err := util.GetPodIPsOfNetwork(pod, &util.DefaultNetInfo{})
					if err != nil {
						return nil, err
					}
					namespaceCache.(*sync.Map).Store(pod.Name, podIPs)
					podsIps = append(podsIps, podIPs...)
				}
			}
			podsCache.Store(namespace.Name, namespaceCache)
		}
		peer.pods = &podsCache
		peer.namedPorts = &namedPortsCache
	}
	asIndex := GetANPPeerAddrSetDbIDs(anpName, gressPrefix, fmt.Sprintf("%d", priority), controllerName, idType)
	addrSet, err = c.addressSetFactory.EnsureAddressSet(asIndex)
	if err != nil {
		return nil, err
	}
	err = addrSet.SetIPs(podsIps)
	if err != nil {
		return nil, err
	}
	return addrSet, nil
}

func GetANPPeerAddrSetDbIDs(name, gressPrefix, priority, controller string, idType *libovsdbops.ObjectIDsType) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(idType, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey:      name,
		libovsdbops.PolicyDirectionKey: gressPrefix,
		// priority is the unique id for address set within given gressPrefix
		libovsdbops.PriorityKey: priority,
	})
}

func constructMatchFromNamedPorts(portName string, peers []*adminNetworkPolicyPeer, subject *adminNetworkPolicySubject, gressPrefix string) (string, error) {
	var match string
	if gressPrefix == ANPIngressPrefix || gressPrefix == BANPIngressPrefix {
		// since direction of rule is ingress, we need to search for the destination port in the policy subject
		// subject.namedPorts was populated inside getPortsOfSubject
		ipToPortCache, loaded := subject.namedPorts.Load(portName)
		if !loaded {
			return "", fmt.Errorf("unable to find a matching namedPort %s in the subject pods of the defined policy", portName)
		}
		ipToPortCache.(*sync.Map).Range(func(key, value any) bool {
			anpPort := value.(adminNetworkPolicyPort)
			podIP := key.(net.IP)
			isV4 := podIP.To4() != nil
			switch {
			case config.IPv4Mode && isV4:
				match = match + fmt.Sprintf("(ip4.src == %s && %s && %s.dst==%d)", podIP, anpPort.protocol, anpPort.protocol, anpPort.port)
			case config.IPv6Mode && !isV4:
				match = match + fmt.Sprintf("(ip6.src == %s && %s && %s.dst==%d)", podIP, anpPort.protocol, anpPort.protocol, anpPort.port)
			}
			match = match + " || "
			return true
		})
	} else {
		// since direction of rule is egress, we need to search for the destination port in the policy rule peers
		// peer.namedPorts was populated inside createASForPeers
		for _, peer := range peers {
			ipToPortCache, loaded := peer.namedPorts.Load(portName)
			if !loaded {
				continue // no-op, let's move to next peer
			}
			ipToPortCache.(*sync.Map).Range(func(key, value any) bool {
				anpPort := value.(adminNetworkPolicyPort)
				podIP := key.(net.IP)
				isV4 := podIP.To4() != nil
				switch {
				case config.IPv4Mode && isV4:
					match = match + fmt.Sprintf("(ip4.dst == %s && %s && %s.dst==%d)", podIP, anpPort.protocol, anpPort.protocol, anpPort.port)
				case config.IPv6Mode && !isV4:
					match = match + fmt.Sprintf("(ip6.dst == %s && %s && %s.dst==%d)", podIP, anpPort.protocol, anpPort.protocol, anpPort.port)
				}
				match = match + " || "
				return true
			})
		}
		if len(match) == 0 {
			klog.Infof("SURYA: No match found")
			return "", fmt.Errorf("unable to find a matching namedPort %s in the peer pods of the defined policy", portName)
		}
	}
	return match, nil
}

func constructMatchFromAddressSet(gressPrefix string, addrSet addressset.AddressSet) string {
	hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 := addrSet.GetASHashNames()
	var direction, match string
	if gressPrefix == ANPIngressPrefix || gressPrefix == BANPIngressPrefix {
		direction = "src"
	} else {
		direction = "dst"
	}

	switch {
	case config.IPv4Mode && config.IPv6Mode:
		match = fmt.Sprintf("(ip4.%s == $%s || ip6.%s == $%s)", direction, hashedAddressSetNameIPv4, direction, hashedAddressSetNameIPv6)
	case config.IPv4Mode:
		match = fmt.Sprintf("(ip4.%s == $%s)", direction, hashedAddressSetNameIPv4)
	case config.IPv6Mode:
		match = fmt.Sprintf("(ip6.%s == $%s)", direction, hashedAddressSetNameIPv6)
	}

	return fmt.Sprintf("(%s)", match)
}

func constructMatchFromPorts(port *adminNetworkPolicyPort) string {
	if port.endPort != 0 && port.endPort != port.port {
		// Port Range is set
		return fmt.Sprintf("(%s && %d<=%s.dst<=%d)", port.protocol, port.port, port.protocol, port.endPort)
	} else if port.port != 0 {
		// Port Number is set
		return fmt.Sprintf("(%s && %s.dst==%d)", port.protocol, port.protocol, port.port)
	}
	// no ports were set, select full protocol
	return fmt.Sprintf("(%s)", port.protocol)
}

func getANPGressPolicyACLName(anpName, gressPrefix, priority string) string {
	substrings := []string{anpName, gressPrefix, priority}
	return strings.Join(substrings, "_")
}

func (c *Controller) getPortsOfSubject(subject *adminNetworkPolicySubject) ([]*nbdb.LogicalSwitchPort, error) {
	ports := []*nbdb.LogicalSwitchPort{}
	namespaces, err := c.anpNamespaceLister.List(subject.namespaceSelector)
	if err != nil {
		return nil, err
	}
	podsCache := sync.Map{}
	namedPortsCache := sync.Map{}
	for _, namespace := range namespaces {
		namespaceCache, _ := podsCache.LoadOrStore(namespace.Name, &sync.Map{})
		podNamespaceLister := c.anpPodLister.Pods(namespace.Name)
		pods, err := podNamespaceLister.List(subject.podSelector)
		if err != nil {
			return nil, err
		}
		for _, pod := range pods {
			if util.PodWantsHostNetwork(pod) || util.PodCompleted(pod) || !util.PodScheduled(pod) {
				continue
			}
			if err := populateNamedPortsFromPod(&namedPortsCache, pod, true); err != nil {
				return nil, err
			}
			logicalPortName := util.GetLogicalPortName(pod.Namespace, pod.Name)
			lsp := &nbdb.LogicalSwitchPort{Name: logicalPortName}
			lsp, err = libovsdbops.GetLogicalSwitchPort(c.nbClient, lsp)
			if err != nil {
				if errors.Is(err, libovsdbclient.ErrNotFound) { // TODO: Double check this logic
					// reprocess it when LSP is created - whenever the next update for the pod comes
					continue
				}
				return nil, fmt.Errorf("error retrieving logical switch port with Name %s "+
					" from libovsdb cache: %w", logicalPortName, err)
			}
			ports = append(ports, lsp)
			namespaceCache.(*sync.Map).Store(pod.Name, lsp.UUID)
		}
		podsCache.LoadOrStore(namespace.Name, namespaceCache)
	}
	subject.pods = &podsCache
	subject.namedPorts = &namedPortsCache
	return ports, nil
}

func (c *Controller) clearAdminNetworkPolicy(anpName string) error {
	obj, loaded := c.anpCache.Load(anpName)
	if !loaded {
		// there is no existing ANP configured with this priority, nothing to clean
		klog.Infof("ANP %s not found in cache", anpName)
		return nil
	}

	anp := obj.(*adminNetworkPolicy)
	anp.Lock()
	defer anp.Unlock()
	klog.Infof("SURYA %v", anpName)
	// clear NBDB objects for the given ANP (PG, ACLs on that PG, AddrSets used by the ACLs)
	var err error
	// remove PG for Subject (ACLs will get cleaned up automatically)
	portGroupName, readableGroupName := getAdminNetworkPolicyPGName(anp.name)
	// no need to batch this with address-set deletes since this itself will contain a bunch of ACLs that need to be deleted which is heavy enough.
	err = libovsdbops.DeletePortGroups(c.nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("unable to delete PG %s for ANP %s: %w", readableGroupName, anp.name, err)
	}
	// remove address-sets that were created for the peers of each rule fpr the whole ANP
	err = c.clearASForPeers(anp.name, libovsdbops.AddressSetAdminNetworkPolicy)
	if err != nil {
		return fmt.Errorf("failed to delete address-sets for ANP %s/%d: %w", anp.name, anp.priority, err)
	}
	// we can delete the object from the cache now.
	// we also mark it as stale to prevent pod processing if RLock
	// acquired after removal from cache.
	klog.Infof("SURYA")
	c.anpPriorityMap.Delete(anp.priority)
	c.anpCache.Delete(anpName)
	anp.stale = true

	return nil
}

func (c *Controller) clearASForPeers(anpName string, idType *libovsdbops.ObjectIDsType) error {
	predicateIDs := libovsdbops.NewDbObjectIDs(idType, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: anpName,
		})
	klog.Infof("SURYA %v", predicateIDs)
	asPredicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(c.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to destroy address-set for ANP %s, err: %v", anpName, err)
	}
	return nil
}
