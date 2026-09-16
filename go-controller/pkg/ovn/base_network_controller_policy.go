// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type netpolDefaultDenyACLType string

const (
	// netpolDefaultDenyACLType is used to distinguish default deny and arp allow acls create for the same port group
	defaultDenyACL netpolDefaultDenyACLType = "defaultDeny"
	arpAllowACL    netpolDefaultDenyACLType = "arpAllow"
	icmpAllowACL   netpolDefaultDenyACLType = "icmpAllow"

	// icmpAllowPolicyMatch is the match used when creating default allow ICMP and ICMPv6 ACLs for a namespace
	icmpAllowPolicyMatch = "(icmp || icmp6)"
	// arpAllowPolicyMatch is the match used when creating default allow ARP ACLs for a namespace
	arpAllowPolicyMatch   = "(arp || nd)"
	allowHairpinningACLID = "allow-hairpinning"
	// ovnStatelessNetPolAnnotationName is an annotation on K8s Network Policy resource to specify that all
	// the resulting OVN ACLs must be created as stateless
	ovnStatelessNetPolAnnotationName = "k8s.ovn.org/acl-stateless"
)

// defaultDenyPortGroups is a shared object and should be used by only 1 thread at a time
type defaultDenyPortGroups struct {
	// portName: map[portName]sets.String(policyNames)
	// store policies that are using every port in the map
	// these maps should be atomically updated with db operations
	// if adding a port to db for a policy fails, map shouldn't be changed
	ingressPortToPolicies map[string]sets.Set[string]
	egressPortToPolicies  map[string]sets.Set[string]
	// Last LSP UUID asserted into each default-deny port group.
	// Used when per-policy records lag a recreated port.
	ingressPortToAssertedUUID map[string]string
	egressPortToAssertedUUID  map[string]string
	// policies is a map of policies that use this port group
	// policy keys must be unique, and it can be retrieved with (np *networkPolicy) getKey()
	policies map[string]bool
}

// snapshotPortState returns a closure that restores the exact in-memory state
// for the affected ports. The caller must hold the shared port-group lock while
// taking and restoring the snapshot.
func (sharedPGs *defaultDenyPortGroups) snapshotPortState(np *networkPolicy, portNamesToUUIDs map[string]string) func() {
	previousIngressPolicies := map[string]sets.Set[string]{}
	previousIngressPoliciesOK := map[string]bool{}
	previousEgressPolicies := map[string]sets.Set[string]{}
	previousEgressPoliciesOK := map[string]bool{}
	previousIngressAsserted := map[string]string{}
	previousIngressAssertedOK := map[string]bool{}
	previousEgressAsserted := map[string]string{}
	previousEgressAssertedOK := map[string]bool{}
	for portName := range portNamesToUUIDs {
		if np.isIngress {
			policies, ok := sharedPGs.ingressPortToPolicies[portName]
			previousIngressPoliciesOK[portName] = ok
			if ok {
				previousIngressPolicies[portName] = policies.Clone()
			}
			previousIngressAsserted[portName], previousIngressAssertedOK[portName] = sharedPGs.ingressPortToAssertedUUID[portName]
		}
		if np.isEgress {
			policies, ok := sharedPGs.egressPortToPolicies[portName]
			previousEgressPoliciesOK[portName] = ok
			if ok {
				previousEgressPolicies[portName] = policies.Clone()
			}
			previousEgressAsserted[portName], previousEgressAssertedOK[portName] = sharedPGs.egressPortToAssertedUUID[portName]
		}
	}
	return func() {
		for portName := range portNamesToUUIDs {
			if np.isIngress {
				if previousIngressPoliciesOK[portName] {
					sharedPGs.ingressPortToPolicies[portName] = previousIngressPolicies[portName].Clone()
				} else {
					delete(sharedPGs.ingressPortToPolicies, portName)
				}
				if previousIngressAssertedOK[portName] {
					sharedPGs.ingressPortToAssertedUUID[portName] = previousIngressAsserted[portName]
				} else {
					delete(sharedPGs.ingressPortToAssertedUUID, portName)
				}
			}
			if np.isEgress {
				if previousEgressPoliciesOK[portName] {
					sharedPGs.egressPortToPolicies[portName] = previousEgressPolicies[portName].Clone()
				} else {
					delete(sharedPGs.egressPortToPolicies, portName)
				}
				if previousEgressAssertedOK[portName] {
					sharedPGs.egressPortToAssertedUUID[portName] = previousEgressAsserted[portName]
				} else {
					delete(sharedPGs.egressPortToAssertedUUID, portName)
				}
			}
		}
	}
}

// addPortsForPolicy adds port-policy association for default deny port groups and
// returns lists of new ports to add to the default deny port groups. The restore
// closure resets both reference sets and asserted UUIDs to their exact prior state.
// If port should be added to ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) addPortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string, restoreState func()) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}
	restoreState = sharedPGs.snapshotPortState(np, portNamesToUUIDs)

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// Reassert changed UUIDs; refcounts are by port name.
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 || sharedPGs.ingressPortToAssertedUUID[portName] != portUUID {
				ingressDenyPorts = append(ingressDenyPorts, portUUID)
				sharedPGs.ingressPortToAssertedUUID[portName] = portUUID
			}
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				sharedPGs.ingressPortToPolicies[portName] = sets.Set[string]{}
			}
			// increment the reference count.
			sharedPGs.ingressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 || sharedPGs.egressPortToAssertedUUID[portName] != portUUID {
				egressDenyPorts = append(egressDenyPorts, portUUID)
				sharedPGs.egressPortToAssertedUUID[portName] = portUUID
			}
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				sharedPGs.egressPortToPolicies[portName] = sets.Set[string]{}
			}
			// bump reference count
			sharedPGs.egressPortToPolicies[portName].Insert(np.getKey())
		}
	}
	return
}

// deletePortsForPolicy deletes port-policy association for default deny port groups,
// and returns lists of port UUIDs to delete from the default deny port groups.
// The returned closure restores the exact policy-reference and asserted-UUID
// state that preceded this call and must be invoked while holding the shared
// port-group lock if the corresponding OVN operation fails.
// If port should be deleted from ingress and/or egress default deny port group depends on policy spec.
func (sharedPGs *defaultDenyPortGroups) deletePortsForPolicy(np *networkPolicy,
	portNamesToUUIDs map[string]string) (ingressDenyPorts, egressDenyPorts []string, restoreState func()) {
	ingressDenyPorts = []string{}
	egressDenyPorts = []string{}
	restoreState = sharedPGs.snapshotPortState(np, portNamesToUUIDs)

	if np.isIngress {
		for portName, portUUID := range portNamesToUUIDs {
			// Delete and Len can be used for zero-value nil set
			sharedPGs.ingressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.ingressPortToPolicies[portName].Len() == 0 {
				// Delete the asserted UUID; recorded policy UUIDs may be stale.
				deleteUUID := portUUID
				if assertedUUID, ok := sharedPGs.ingressPortToAssertedUUID[portName]; ok {
					deleteUUID = assertedUUID
				}
				ingressDenyPorts = append(ingressDenyPorts, deleteUUID)
				delete(sharedPGs.ingressPortToPolicies, portName)
				delete(sharedPGs.ingressPortToAssertedUUID, portName)
			}
		}
	}
	if np.isEgress {
		for portName, portUUID := range portNamesToUUIDs {
			sharedPGs.egressPortToPolicies[portName].Delete(np.getKey())
			if sharedPGs.egressPortToPolicies[portName].Len() == 0 {
				deleteUUID := portUUID
				if assertedUUID, ok := sharedPGs.egressPortToAssertedUUID[portName]; ok {
					deleteUUID = assertedUUID
				}
				egressDenyPorts = append(egressDenyPorts, deleteUUID)
				delete(sharedPGs.egressPortToPolicies, portName)
				delete(sharedPGs.egressPortToAssertedUUID, portName)
			}
		}
	}
	return
}

type networkPolicy struct {
	// Mutable state is protected by the corresponding bnc.networkPolicies key lock.
	name            string
	namespace       string
	ingressPolicies []*gressPolicy
	egressPolicies  []*gressPolicy
	isIngress       bool
	isEgress        bool

	// Selector used by pod-driven membership reconciliation.
	localPodSelector labels.Selector
	// peerAddressSets stores PodSelectorAddressSet keys for peers that this network policy was successfully added to.
	// Required for cleanup.
	peerAddressSets []string

	// localPods is a map of pods affected by this policy.
	// It is used to update defaultDeny port group port counters, when deleting network policy.
	// Port should only be added here if it was successfully added to default deny port group,
	// and local port group in db.
	// map of portName(string): portUUID(string)
	localPods sync.Map
	// localPodPorts is the pod-identity index for localPods. Values are immutable
	// snapshots of map[portName]portUUID and are replaced only after the matching
	// OVN transaction succeeds.
	// map of pod namespace/name(string): map[portName(string)]portUUID(string)
	localPodPorts sync.Map

	portGroupName string
	// Prevent membership updates while failed policy cleanup is pending.
	deleted bool
}

func NewNetworkPolicy(policy *knet.NetworkPolicy) *networkPolicy {
	policyTypeIngress, policyTypeEgress := getPolicyType(policy)
	np := &networkPolicy{
		name:            policy.Name,
		namespace:       policy.Namespace,
		ingressPolicies: make([]*gressPolicy, 0),
		egressPolicies:  make([]*gressPolicy, 0),
		isIngress:       policyTypeIngress,
		isEgress:        policyTypeEgress,
		localPods:       sync.Map{},
		localPodPorts:   sync.Map{},
	}
	return np
}

func localPolicyPodKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

func clonePolicyPorts(ports map[string]string) map[string]string {
	cloned := make(map[string]string, len(ports))
	for portName, portUUID := range ports {
		cloned[portName] = portUUID
	}
	return cloned
}

func (np *networkPolicy) getLocalPortsForPod(pod *corev1.Pod) map[string]string {
	ports, ok := np.localPodPorts.Load(localPolicyPodKey(pod))
	if !ok {
		return map[string]string{}
	}
	return clonePolicyPorts(ports.(map[string]string))
}

// setLocalPortsForPod updates both membership indexes after the corresponding
// OVN transaction succeeds. Callers pass the complete recorded state for pod.
func (np *networkPolicy) setLocalPortsForPod(pod *corev1.Pod, ports map[string]string) {
	podKey := localPolicyPodKey(pod)
	previousPorts := np.getLocalPortsForPod(pod)
	for portName := range previousPorts {
		if _, ok := ports[portName]; !ok {
			np.localPods.Delete(portName)
		}
	}
	for portName, portUUID := range ports {
		np.localPods.Store(portName, portUUID)
	}
	if len(ports) == 0 {
		np.localPodPorts.Delete(podKey)
		return
	}
	np.localPodPorts.Store(podKey, clonePolicyPorts(ports))
}

func (bnc *BaseNetworkController) syncNetworkPolicies(networkPolicies []interface{}) error {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range networkPolicies {
		policy, ok := npInterface.(*knet.NetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncNetworkPolicies: %v", npInterface)
		}
		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}
	err := bnc.syncNetworkPoliciesCommon(expectedPolicies)
	if err != nil {
		return err
	}

	// add default hairpin allow acl
	err = bnc.addHairpinAllowACL()
	if err != nil {
		return fmt.Errorf("failed to create allow hairpin acl: %w", err)
	}

	return nil
}

func (bnc *BaseNetworkController) addHairpinAllowACL() error {
	var v4Match, v6Match, match string

	if config.IPv4Mode {
		v4Match = fmt.Sprintf("%s.src == %s", "ip4", config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP.String())
		match = v4Match
	}
	if config.IPv6Mode {
		v6Match = fmt.Sprintf("%s.src == %s", "ip6", config.Gateway.MasqueradeIPs.V6OVNServiceHairpinMasqueradeIP.String())
		match = v6Match
	}
	if config.IPv4Mode && config.IPv6Mode {
		match = fmt.Sprintf("(%s || %s)", v4Match, v6Match)
	}

	ingressACLIDs := bnc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeIngress))
	ingressACL := libovsdbutil.BuildACLWithDefaultTier(ingressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, libovsdbutil.LportIngress)

	egressACLIDs := bnc.getNetpolDefaultACLDbIDs(string(knet.PolicyTypeEgress))
	egressACL := libovsdbutil.BuildACLWithDefaultTier(egressACLIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, libovsdbutil.LportEgressAfterLB)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, nil, ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to create or update hairpin allow ACL %v", err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(bnc.nbClient, ops, bnc.getClusterPortGroupName(types.ClusterPortGroupNameBase),
		ingressACL, egressACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL hairpin allow acl to port group: %v", err)
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// syncNetworkPoliciesCommon syncs logical entities associated with existing network policies.
// It serves both networkpolicies (for default network) and multi-networkpolicies (for secondary networks)
func (bnc *BaseNetworkController) syncNetworkPoliciesCommon(expectedPolicies map[string]map[string]bool) error {
	// find network policies that don't exist in k8s anymore, but still present in the dbs, and cleanup.
	// Peer address sets and network policy's port groups (together with acls) will be cleaned up.
	// Delete port groups with acls first, since address sets may be referenced in these acls, and
	// cause SyntaxError in ovn-controller, if address sets deleted first, but acls still reference them.

	// cleanup port groups
	// netpol-owned port groups first
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, bnc.controllerName, nil)
	p := libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespace, policyName, err := libovsdbops.ParseNamespaceNameKey(item.ExternalIDs[libovsdbops.ObjectNameKey.String()])
		if err != nil {
			klog.Errorf("Failed to sync stale network policy %v: port group IDs parsing failed: %v",
				item.ExternalIDs[libovsdbops.ObjectNameKey.String()], err)
			return false
		}
		// delete if policy is not present in expectedPolicies
		return !expectedPolicies[namespace][policyName]
	})
	if err := libovsdbops.DeletePortGroupsWithPredicate(bnc.nbClient, p); err != nil {
		return fmt.Errorf("cannot delete namespace NetworkPolicy port groups: %v", err)
	}

	// netpol-namespace-owned default deny port groups
	predicateIDs = libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetpolNamespace, bnc.controllerName, nil)
	p = libovsdbops.GetPredicate[*nbdb.PortGroup](predicateIDs, func(item *nbdb.PortGroup) bool {
		namespace := item.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		_, ok := expectedPolicies[namespace]
		// delete default deny port group if no policies in that namespace are found
		return !ok
	})
	if err := libovsdbops.DeletePortGroupsWithPredicate(bnc.nbClient, p); err != nil {
		return fmt.Errorf("cannot find default deny NetworkPolicy port groups: %v", err)
	}
	return nil
}

func getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNode, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.IpKey:         mgmtPortIP,
		})
}

// There is no delete function for this ACL type, because the ACL is applied on a node switch.
// When the node is deleted, switch will be deleted by the node sync, and the dependent ACLs will be
// garbage-collected.
func (bnc *BaseNetworkController) addAllowACLFromNode(switchName string, mgmtPortIP net.IP) error {
	ipFamily := "ip4"
	if utilnet.IsIPv6(mgmtPortIP) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtPortIP.String())
	dbIDs := getAllowFromNodeACLDbIDs(switchName, mgmtPortIP.String(), bnc.controllerName)
	nodeACL := libovsdbutil.BuildACLWithDefaultTier(dbIDs, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, nil, libovsdbutil.LportIngress)

	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig(), nodeACL)
	if err != nil {
		return fmt.Errorf("failed to create or update ACL %v: %v", nodeACL, err)
	}

	ops, err = libovsdbops.AddACLsToLogicalSwitchOps(bnc.nbClient, ops, switchName, nodeACL)
	if err != nil {
		return fmt.Errorf("failed to add ACL %v to switch %s: %v", nodeACL, switchName, err)
	}

	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func (bnc *BaseNetworkController) getDefaultDenyPolicyACLIDs(ns string, aclDir libovsdbutil.ACLDirection,
	defaultACLType netpolDefaultDenyACLType) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: string(aclDir),
			libovsdbops.TypeKey:            string(defaultACLType),
		})
}

func (bnc *BaseNetworkController) getDefaultDenyPolicyPortGroupIDs(ns string, aclDir libovsdbutil.ACLDirection) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetpolNamespace, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
			// in the same namespace there can be 2 default deny port groups, egress and ingress,
			// every port group has default deny and arp allow acl.
			libovsdbops.PolicyDirectionKey: string(aclDir),
		})
}

func (bnc *BaseNetworkController) defaultDenyPortGroupName(namespace string, aclDir libovsdbutil.ACLDirection) string {
	return libovsdbutil.GetPortGroupName(bnc.getDefaultDenyPolicyPortGroupIDs(namespace, aclDir))
}

func (bnc *BaseNetworkController) buildDenyACLs(namespace, pgName string, aclLogging *libovsdbutil.ACLLoggingLevels,
	aclDir libovsdbutil.ACLDirection) []*nbdb.ACL {
	denyMatch := libovsdbutil.GetACLMatch(pgName, "", aclDir)
	allowARPMatch := libovsdbutil.GetACLMatch(pgName, arpAllowPolicyMatch, aclDir)
	aclPipeline := libovsdbutil.ACLDirectionToACLPipeline(aclDir)

	acls := make([]*nbdb.ACL, 0, 3)
	acls = append(acls, libovsdbutil.BuildACLWithDefaultTier(bnc.getDefaultDenyPolicyACLIDs(namespace, aclDir, defaultDenyACL),
		types.DefaultDenyPriority, denyMatch, nbdb.ACLActionDrop, aclLogging, aclPipeline))
	acls = append(acls, libovsdbutil.BuildACLWithDefaultTier(bnc.getDefaultDenyPolicyACLIDs(namespace, aclDir, arpAllowACL),
		types.DefaultAllowPriority, allowARPMatch, nbdb.ACLActionAllow, nil, aclPipeline))
	if config.OVNKubernetesFeature.AllowICMPNetworkPolicy {
		allowICMPMatch := libovsdbutil.GetACLMatch(pgName, icmpAllowPolicyMatch, aclDir)
		acls = append(acls, libovsdbutil.BuildACLWithDefaultTier(bnc.getDefaultDenyPolicyACLIDs(namespace, aclDir, icmpAllowACL),
			types.DefaultAllowPriority, allowICMPMatch, nbdb.ACLActionAllow, nil, aclPipeline))
	}
	return acls
}

func (bnc *BaseNetworkController) addPolicyToDefaultPortGroups(np *networkPolicy, aclLogging *libovsdbutil.ACLLoggingLevels) error {
	return bnc.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, loaded := bnc.sharedNetpolPortGroups.LoadOrStore(pgKey, &defaultDenyPortGroups{
			ingressPortToPolicies:     map[string]sets.Set[string]{},
			egressPortToPolicies:      map[string]sets.Set[string]{},
			ingressPortToAssertedUUID: map[string]string{},
			egressPortToAssertedUUID:  map[string]string{},
			policies:                  map[string]bool{},
		})
		if !loaded {
			// create port groups with acls
			err := bnc.createDefaultDenyPGAndACLs(np.namespace, np.name, aclLogging)
			if err != nil {
				bnc.sharedNetpolPortGroups.Delete(pgKey)
				return fmt.Errorf("failed to create default deny port groups: %v", err)
			}
		}
		sharedPGs.policies[np.getKey()] = true
		return nil
	})
}

func (bnc *BaseNetworkController) delPolicyFromDefaultPortGroups(np *networkPolicy) error {
	return bnc.sharedNetpolPortGroups.DoWithLock(np.namespace, func(pgKey string) error {
		sharedPGs, found := bnc.sharedNetpolPortGroups.Load(pgKey)
		if !found {
			return nil
		}
		delete(sharedPGs.policies, np.getKey())
		if len(sharedPGs.policies) == 0 {
			// last policy was deleted, delete port group
			err := bnc.deleteDefaultDenyPGAndACLs(np.namespace)
			if err != nil {
				return fmt.Errorf("failed to delete defaul deny port group: %v", err)
			}
			bnc.sharedNetpolPortGroups.Delete(pgKey)
		}
		return nil
	})
}

// createDefaultDenyPGAndACLs creates the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (bnc *BaseNetworkController) createDefaultDenyPGAndACLs(namespace, policy string, aclLogging *libovsdbutil.ACLLoggingLevels) error {
	ingressPGIDs := bnc.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLIngress)
	ingressPGName := libovsdbutil.GetPortGroupName(ingressPGIDs)
	ingressACLs := bnc.buildDenyACLs(namespace, ingressPGName, aclLogging, libovsdbutil.ACLIngress)
	egressPGIDs := bnc.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLEgress)
	egressPGName := libovsdbutil.GetPortGroupName(egressPGIDs)
	egressACLs := bnc.buildDenyACLs(namespace, egressPGName, aclLogging, libovsdbutil.ACLEgress)
	allACLs := append(ingressACLs, egressACLs...)
	ops, err := libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, nil, bnc.GetSamplingConfig(), allACLs...)
	if err != nil {
		return err
	}

	ingressPG := libovsdbutil.BuildPortGroup(ingressPGIDs, nil, ingressACLs)
	egressPG := libovsdbutil.BuildPortGroup(egressPGIDs, nil, egressACLs)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(bnc.nbClient, ops, ingressPG, egressPG)
	if err != nil {
		return err
	}

	recordOps, txOkCallBack, _, err := bnc.AddConfigDurationRecord("networkpolicy", namespace, policy)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)
	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return err
	}
	txOkCallBack()

	return nil
}

// deleteDefaultDenyPGAndACLs deletes the default port groups and acls for a namespace
// must be called with defaultDenyPortGroups lock
func (bnc *BaseNetworkController) deleteDefaultDenyPGAndACLs(namespace string) error {
	ingressPGName := bnc.defaultDenyPortGroupName(namespace, libovsdbutil.ACLIngress)
	egressPGName := bnc.defaultDenyPortGroupName(namespace, libovsdbutil.ACLEgress)

	ops, err := libovsdbops.DeletePortGroupsOps(bnc.nbClient, nil, ingressPGName, egressPGName)
	if err != nil {
		return err
	}
	// No need to delete ACLs, since they will be garbage collected with deleted port groups
	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact deleteDefaultDenyPGAndACLs: %v", err)
	}

	return nil
}

// Must be called with the namespace and networkPolicies key locks held.
func (bnc *BaseNetworkController) updateACLLoggingForPolicy(np *networkPolicy, aclLogging *libovsdbutil.ACLLoggingLevels) error {
	if np.deleted {
		return nil
	}

	// Predicate for given network policy ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, bnc.controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: libovsdbops.BuildNamespaceNameKey(np.namespace, np.name),
	})
	p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	return libovsdbutil.UpdateACLLoggingWithPredicate(bnc.nbClient, p, aclLogging)
}

func (bnc *BaseNetworkController) updateACLLoggingForDefaultACLs(ns string, nsInfo *namespaceInfo) error {
	return bnc.sharedNetpolPortGroups.DoWithLock(ns, func(pgKey string) error {
		_, loaded := bnc.sharedNetpolPortGroups.Load(pgKey)
		if !loaded {
			// shared port group doesn't exist, nothing to update
			return nil
		}
		predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNamespace, bnc.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: ns,
				libovsdbops.TypeKey:       string(defaultDenyACL),
			})
		p := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
		defaultDenyACLs, err := libovsdbops.FindACLsWithPredicate(bnc.nbClient, p)
		if err != nil {
			return fmt.Errorf("failed to find netpol default deny acls for namespace %s: %v", ns, err)
		}
		if err := libovsdbutil.UpdateACLLogging(bnc.nbClient, defaultDenyACLs, &nsInfo.aclLogging); err != nil {
			return fmt.Errorf("unable to update ACL logging for namespace %s: %w", ns, err)
		}
		return nil
	})
}

// handleNetPolNamespaceUpdate should update all network policies related to given namespace.
// Must be called with namespace Lock, should be retriable
func (bnc *BaseNetworkController) handleNetPolNamespaceUpdate(namespace string, nsInfo *namespaceInfo) error {
	// update shared port group ACLs
	if err := bnc.updateACLLoggingForDefaultACLs(namespace, nsInfo); err != nil {
		return fmt.Errorf("failed to update default deny ACLs for namespace %s: %v", namespace, err)
	}
	// now update network policy specific ACLs
	klog.V(5).Infof("Setting network policy ACLs for ns: %s", namespace)
	for npKey := range nsInfo.relatedNetworkPolicies {
		err := bnc.networkPolicies.DoWithLock(npKey, func(_ string) error {
			np, found := bnc.networkPolicies.Load(npKey)
			if !found {
				klog.Errorf("Netpol was deleted from cache, but not from namespace related objects")
				return nil
			}
			return bnc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging)
		})
		if err != nil {
			return fmt.Errorf("unable to update ACL for network policy %s: %v", npKey, err)
		}
		klog.Infof("ACL for network policy: %s, updated to new log level: %s", npKey, nsInfo.aclLogging.Allow)
	}
	return nil
}

// getPolicyType returns whether the policy is of type ingress and/or egress
func getPolicyType(policy *knet.NetworkPolicy) (bool, bool) {
	var policyTypeIngress bool
	var policyTypeEgress bool

	for _, policyType := range policy.Spec.PolicyTypes {
		if policyType == knet.PolicyTypeIngress {
			policyTypeIngress = true
		} else if policyType == knet.PolicyTypeEgress {
			policyTypeEgress = true
		}
	}

	return policyTypeIngress, policyTypeEgress
}

// localPolicyPortChanges is the desired-vs-recorded membership diff for one
// pod and policy. Resolution errors are returned alongside safe changes so NAD
// removals can be cleaned up without dropping membership whose current LSP is
// only temporarily absent from logicalPortCache.
type localPolicyPortChanges struct {
	portsToAdd       map[string]string
	portsToDelete    map[string]string
	resolutionErrors []error
}

func portGroupPortUUIDs(portNamesToUUIDs map[string]string) []string {
	uuids := make([]string, 0, len(portNamesToUUIDs))
	for _, uuid := range portNamesToUUIDs {
		uuids = append(uuids, uuid)
	}
	return uuids
}

// getLocalPolicyPortChanges compares the policy's recorded membership for pod
// with the ports that should currently be members. An unresolved expected LSP
// is retried and its same-name recorded membership is retained. Ports that are
// no longer desired because the selector, zone, or NAD set changed are safe to
// remove without consulting logicalPortCache.
func (bnc *BaseNetworkController) getLocalPolicyPortChanges(np *networkPolicy, pod *corev1.Pod, selected bool) localPolicyPortChanges {
	changes := localPolicyPortChanges{
		portsToAdd:    map[string]string{},
		portsToDelete: map[string]string{},
	}
	recordedPorts := np.getLocalPortsForPod(pod)
	deleteAllRecorded := func() {
		for portName, portUUID := range recordedPorts {
			changes.portsToDelete[portName] = portUUID
		}
	}

	if !selected {
		deleteAllRecorded()
		return changes
	}
	if !util.PodScheduled(pod) {
		// A Pod UID cannot transition from scheduled back to unscheduled. Any
		// recorded membership under this namespace/name therefore belongs to an
		// older incarnation and must not be inherited by a pending replacement.
		deleteAllRecorded()
		return changes
	}
	if !bnc.isPodScheduledOnLocalNode(pod) || !bnc.podExpectedInLogicalCache(pod) {
		deleteAllRecorded()
		return changes
	}

	nadKeys, err := bnc.getPodNADKeys(pod)
	if err != nil {
		// The desired NAD set is unknown, so none of the recorded ports can be
		// safely classified as stale. Pod reconciliation reports the malformed
		// attachment state separately.
		klog.Warningf("Failed to get NAD keys for pod %s/%s for networkPolicy %s, preserving recorded membership: %v",
			pod.Namespace, pod.Name, np.name, err)
		return changes
	}

	expectedPortNames := sets.New[string]()
	for _, nadKey := range nadKeys {
		logicalPortName := bnc.GetLogicalPortName(pod, nadKey)
		expectedPortNames.Insert(logicalPortName)
		recordedPortUUID, recorded := recordedPorts[logicalPortName]

		portInfo, err := bnc.logicalPortCache.get(pod, nadKey)
		if err != nil {
			changes.resolutionErrors = append(changes.resolutionErrors,
				fmt.Errorf("unable to get port info for pod %s/%s NAD key %s: %w", pod.Namespace, pod.Name, nadKey, err))
			continue
		}
		if !portInfo.expires.IsZero() {
			changes.resolutionErrors = append(changes.resolutionErrors,
				fmt.Errorf("port info for pod %s/%s NAD key %s is scheduled for removal", pod.Namespace, pod.Name, nadKey))
			continue
		}
		if bnc.IsUserDefinedNetwork() && portInfo.appliedNetworkName != bnc.GetNetworkName() {
			changes.resolutionErrors = append(changes.resolutionErrors,
				fmt.Errorf("port info for pod %s/%s NAD key %s belongs to network %s, expected %s",
					pod.Namespace, pod.Name, nadKey, portInfo.appliedNetworkName, bnc.GetNetworkName()))
			continue
		}

		if recorded && recordedPortUUID == portInfo.uuid {
			continue
		}
		if recorded {
			// Replace recorded membership for a previous LSP incarnation.
			changes.portsToDelete[logicalPortName] = recordedPortUUID
		}
		changes.portsToAdd[portInfo.name] = portInfo.uuid
	}

	for portName, portUUID := range recordedPorts {
		if !expectedPortNames.Has(portName) {
			changes.portsToDelete[portName] = portUUID
		}
	}
	return changes
}

// denyPGAddPorts adds ports to default deny port groups.
// It also can take existing ops e.g. to add port to network policy port group and transact it.
// It only adds new ports that do not already exist in the deny port groups.
func (bnc *BaseNetworkController) denyPGAddPorts(np *networkPolicy, portNamesToUUIDs map[string]string, ops []ovsdb.Operation) error {
	var err error
	ingressDenyPGName := bnc.defaultDenyPortGroupName(np.namespace, libovsdbutil.ACLIngress)
	egressDenyPGName := bnc.defaultDenyPortGroupName(np.namespace, libovsdbutil.ACLEgress)

	pgKey := np.namespace
	// this lock guarantees that sharedPortGroup counters will be updated atomically
	// with adding port to port group in db.
	bnc.sharedNetpolPortGroups.LockKey(pgKey)
	defer bnc.sharedNetpolPortGroups.UnlockKey(pgKey)
	sharedPGs, ok := bnc.sharedNetpolPortGroups.Load(pgKey)
	if !ok {
		// Port group doesn't exist
		return fmt.Errorf("port groups for ns %s don't exist", np.namespace)
	}

	ingressDenyPorts, egressDenyPorts, restoreState := sharedPGs.addPortsForPolicy(np, portNamesToUUIDs)
	// Restore the exact reference and asserted-UUID snapshot on error while the
	// shared port-group key is still locked.
	defer func() {
		if err != nil {
			restoreState()
		}
	}()

	if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
		// db changes required
		ops, err = libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", ingressDenyPGName, err)
		}

		ops, err = libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, ops, egressDenyPGName, egressDenyPorts...)
		if err != nil {
			return fmt.Errorf("unable to get add ports to %s port group ops: %v", egressDenyPGName, err)
		}
	}
	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact add ports to default deny port groups: %v", err)
	}
	return nil
}

// denyPGDeletePorts deletes ports from default deny port groups.
// Set useLocalPods = true, when deleting networkPolicy to remove all its ports from defaultDeny port groups.
// It also can take existing ops e.g. to delete ports from network policy port group and transact it.
func (bnc *BaseNetworkController) denyPGDeletePorts(np *networkPolicy, portNamesToUUIDs map[string]string, useLocalPods bool,
	ops []ovsdb.Operation) error {
	var err error
	if useLocalPods {
		portNamesToUUIDs = map[string]string{}
		np.localPods.Range(func(key, value interface{}) bool {
			portNamesToUUIDs[key.(string)] = value.(string)
			return true
		})
	}
	if len(portNamesToUUIDs) != 0 {
		ingressDenyPGName := bnc.defaultDenyPortGroupName(np.namespace, libovsdbutil.ACLIngress)
		egressDenyPGName := bnc.defaultDenyPortGroupName(np.namespace, libovsdbutil.ACLEgress)

		pgKey := np.namespace
		// this lock guarantees that sharedPortGroup counters will be updated atomically
		// with adding port to port group in db.
		bnc.sharedNetpolPortGroups.LockKey(pgKey)
		defer bnc.sharedNetpolPortGroups.UnlockKey(pgKey)
		sharedPGs, ok := bnc.sharedNetpolPortGroups.Load(pgKey)
		if !ok {
			// Port group doesn't exist, nothing to clean up
			klog.Infof("Skip delete ports from default deny port group: port group doesn't exist")
		} else {
			ingressDenyPorts, egressDenyPorts, restoreState := sharedPGs.deletePortsForPolicy(np, portNamesToUUIDs)
			// Restore the exact reference and asserted-UUID snapshot on error.
			// The shared port-group key remains locked through transaction and
			// rollback, including when no default-deny DB mutation was needed.
			defer func() {
				if err != nil {
					restoreState()
				}
			}()

			if len(ingressDenyPorts) != 0 || len(egressDenyPorts) != 0 {
				// db changes required
				ops, err = libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, ops, ingressDenyPGName, ingressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", ingressDenyPGName, err)
				}

				ops, err = libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, ops, egressDenyPGName, egressDenyPorts...)
				if err != nil {
					return fmt.Errorf("unable to get del ports from %s port group ops: %v", egressDenyPGName, err)
				}
			}
		}
	}
	_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("unable to transact del ports from default deny port groups: %v", err)
	}

	return nil
}

// removeRecordedPolicyPorts removes policy/default-deny ports from OVN. The
// caller updates the in-memory recorded state only after this succeeds.
func (bnc *BaseNetworkController) removeRecordedPolicyPorts(np *networkPolicy, portNamesToUUIDs map[string]string) error {
	ops, err := libovsdbops.DeletePortsFromPortGroupOps(bnc.nbClient, nil, np.portGroupName, portGroupPortUUIDs(portNamesToUUIDs)...)
	if err != nil {
		return fmt.Errorf("unable to get ops to delete ports from policy port group %s: %w", np.portGroupName, err)
	}
	if err = bnc.denyPGDeletePorts(np, portNamesToUUIDs, false, ops); err != nil {
		return fmt.Errorf("unable to delete ports from default deny port groups: %w", err)
	}
	return nil
}

// reconcileLocalPodForNetworkPolicy makes one pod's policy membership match the
// latest selector, zone, NAD, and logical-port state. The recorded snapshot is
// advanced after each successful OVN transaction so a partial failure is safe
// to retry. The caller must hold the networkPolicies key lock.
func (bnc *BaseNetworkController) reconcileLocalPodForNetworkPolicy(np *networkPolicy, pod *corev1.Pod, selected, requirePorts bool) error {
	changes := bnc.getLocalPolicyPortChanges(np, pod, selected)
	recordedPorts := np.getLocalPortsForPod(pod)
	if len(changes.portsToDelete) > 0 {
		start := time.Now()
		klog.Infof("Processing NetworkPolicy %s/%s to delete %d ports for pod %s/%s...",
			np.namespace, np.name, len(changes.portsToDelete), pod.Namespace, pod.Name)
		if err := bnc.removeRecordedPolicyPorts(np, changes.portsToDelete); err != nil {
			return fmt.Errorf("unable to remove stale policy ports for pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		for portName := range changes.portsToDelete {
			delete(recordedPorts, portName)
		}
		np.setLocalPortsForPod(pod, recordedPorts)
		if !bnc.IsUserDefinedNetwork() && config.Metrics.EnableScaleMetrics {
			metrics.RecordNetpolLocalPodEvent("delete", time.Since(start))
		}
	}

	if len(changes.portsToAdd) > 0 {
		start := time.Now()
		var err error
		var ops []ovsdb.Operation
		policyPortUUIDs := portGroupPortUUIDs(changes.portsToAdd)
		if !PortGroupHasPorts(bnc.nbClient, np.portGroupName, policyPortUUIDs) {
			ops, err = libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, nil, np.portGroupName, policyPortUUIDs...)
			if err != nil {
				return fmt.Errorf("unable to get ops to add pod %s/%s to policy port group %s: %w",
					pod.Namespace, pod.Name, np.portGroupName, err)
			}
		}
		if err = bnc.denyPGAddPorts(np, changes.portsToAdd, ops); err != nil {
			return fmt.Errorf("unable to add pod %s/%s to default deny port group: %w", pod.Namespace, pod.Name, err)
		}
		for portName, portUUID := range changes.portsToAdd {
			recordedPorts[portName] = portUUID
		}
		np.setLocalPortsForPod(pod, recordedPorts)
		if !bnc.IsUserDefinedNetwork() && config.Metrics.EnableScaleMetrics {
			metrics.RecordNetpolLocalPodEvent("add", time.Since(start))
		}
	}

	if requirePorts {
		return utilerrors.Join(changes.resolutionErrors...)
	}
	return nil
}

func (bnc *BaseNetworkController) addNetworkPolicyToNamespaceIndex(np *networkPolicy) {
	_ = bnc.networkPolicyKeysByNamespace.DoWithLock(np.namespace, func(namespace string) error {
		policyKeys, _ := bnc.networkPolicyKeysByNamespace.Load(namespace)
		indexedPolicyKeys := policyKeys.Clone()
		indexedPolicyKeys.Insert(np.getKey())
		bnc.networkPolicyKeysByNamespace.Store(namespace, indexedPolicyKeys)
		return nil
	})
}

func (bnc *BaseNetworkController) deleteNetworkPolicyFromNamespaceIndex(np *networkPolicy) {
	_ = bnc.networkPolicyKeysByNamespace.DoWithLock(np.namespace, func(namespace string) error {
		policyKeys, ok := bnc.networkPolicyKeysByNamespace.Load(namespace)
		if !ok {
			return nil
		}
		indexedPolicyKeys := policyKeys.Clone()
		indexedPolicyKeys.Delete(np.getKey())
		if indexedPolicyKeys.Len() == 0 {
			bnc.networkPolicyKeysByNamespace.Delete(namespace)
			return nil
		}
		bnc.networkPolicyKeysByNamespace.Store(namespace, indexedPolicyKeys)
		return nil
	})
}

func (bnc *BaseNetworkController) getNetworkPolicyKeysForNamespace(namespace string) []string {
	policyKeys, _ := bnc.networkPolicyKeysByNamespace.Load(namespace)
	return sets.List(policyKeys)
}

func (bnc *BaseNetworkController) reconcileNetworkPolicyPortGroups(pod *corev1.Pod) error {
	return bnc.reconcileNetworkPolicyPortGroupsWithPortCheck(pod, true)
}

// During old-pod teardown the replacement's ports may not exist yet. Apply
// safe membership changes, but leave unresolved ports for replacement setup;
// making them a teardown error would prevent that setup from ever running.
func (bnc *BaseNetworkController) reconcileNetworkPolicyPortGroupsWithPortCheck(pod *corev1.Pod, requirePorts bool) error {
	var errs []error
	for _, npKey := range bnc.getNetworkPolicyKeysForNamespace(pod.Namespace) {
		err := bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
			np, ok := bnc.networkPolicies.Load(npKey)
			if !ok || np.deleted || np.localPodSelector == nil {
				return nil
			}
			selected := np.localPodSelector.Matches(labels.Set(pod.Labels))
			return bnc.reconcileLocalPodForNetworkPolicy(np, pod, selected, requirePorts)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to reconcile pod %s/%s with network policy %s: %w", pod.Namespace, pod.Name, npKey, err))
		}
	}
	return utilerrors.Join(errs...)
}

func (bnc *BaseNetworkController) deletePodNetworkPolicyMembership(pod *corev1.Pod) error {
	var errs []error
	// Clear any recorded membership, even if the pod no longer matches.
	for _, npKey := range bnc.getNetworkPolicyKeysForNamespace(pod.Namespace) {
		err := bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
			np, ok := bnc.networkPolicies.Load(npKey)
			if !ok || np.deleted {
				return nil
			}
			return bnc.reconcileLocalPodForNetworkPolicy(np, pod, false, false)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete pod %s/%s from network policy %s: %w", pod.Namespace, pod.Name, npKey, err))
		}
	}
	return utilerrors.Join(errs...)
}

type bootstrapLocalPolicyPodPorts struct {
	pod   *corev1.Pod
	ports map[string]string
}

// bootstrapLocalPodsForNetworkPolicy adds a new policy's initial membership in
// one transaction. The caller holds the networkPolicies key lock from index
// publication through the pod snapshot and recorded-state updates.
func (bnc *BaseNetworkController) bootstrapLocalPodsForNetworkPolicy(np *networkPolicy, pods []*corev1.Pod) error {
	var resolutionErrors []error
	portsToAdd := map[string]string{}
	podPortsToRecord := make([]bootstrapLocalPolicyPodPorts, 0, len(pods))

	for _, pod := range pods {
		changes := bnc.getLocalPolicyPortChanges(np, pod, true)
		if len(changes.portsToDelete) != 0 {
			return fmt.Errorf("network policy %s bootstrap found recorded membership for new policy pod %s/%s",
				np.getKey(), pod.Namespace, pod.Name)
		}
		if len(changes.resolutionErrors) != 0 {
			for _, resolveErr := range changes.resolutionErrors {
				resolutionErrors = append(resolutionErrors,
					fmt.Errorf("failed to resolve pod %s/%s for network policy %s: %w",
						pod.Namespace, pod.Name, np.getKey(), resolveErr))
			}
		}
		if len(changes.portsToAdd) == 0 {
			continue
		}

		podPorts := clonePolicyPorts(changes.portsToAdd)
		podPortsToRecord = append(podPortsToRecord, bootstrapLocalPolicyPodPorts{
			pod:   pod,
			ports: podPorts,
		})
		for portName, portUUID := range podPorts {
			portsToAdd[portName] = portUUID
		}
	}

	if len(portsToAdd) == 0 {
		return utilerrors.Join(resolutionErrors...)
	}

	start := time.Now()
	klog.Infof("Processing NetworkPolicy %s/%s to bootstrap %d ports for %d local pods...",
		np.namespace, np.name, len(portsToAdd), len(podPortsToRecord))
	ops, err := libovsdbops.AddPortsToPortGroupOps(bnc.nbClient, nil, np.portGroupName,
		portGroupPortUUIDs(portsToAdd)...)
	if err != nil {
		return fmt.Errorf("unable to get ops to bootstrap policy port group %s: %w", np.portGroupName, err)
	}
	if err = bnc.denyPGAddPorts(np, portsToAdd, ops); err != nil {
		return fmt.Errorf("unable to bootstrap default deny port groups for network policy %s: %w", np.getKey(), err)
	}

	// Advance both recorded indexes only after the combined policy/default-deny
	// transaction succeeds.
	for _, podPorts := range podPortsToRecord {
		np.setLocalPortsForPod(podPorts.pod, podPorts.ports)
	}
	if !bnc.IsUserDefinedNetwork() && config.Metrics.EnableScaleMetrics {
		metrics.RecordNetpolLocalPodEvent("add", time.Since(start))
	}

	return utilerrors.Join(resolutionErrors...)
}

func (bnc *BaseNetworkController) getNetworkPolicyPortGroupDbIDs(namespace, name string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: libovsdbops.BuildNamespaceNameKey(namespace, name),
		})
}

func (bnc *BaseNetworkController) getNetworkPolicyPGName(namespace, name string) string {
	return libovsdbutil.GetPortGroupName(bnc.getNetworkPolicyPortGroupDbIDs(namespace, name))
}

// createNetworkPolicy creates a network policy, should be retriable.
// If network policy with given key exists, it will try to clean it up first, and return an error if it fails.
// No need to log network policy key here, because caller of createNetworkPolicy should prepend error message with
// that information.
func (bnc *BaseNetworkController) createNetworkPolicy(policy *knet.NetworkPolicy, aclLogging *libovsdbutil.ACLLoggingLevels) (*networkPolicy, error) {
	// To avoid existing connections disruption, make sure to apply allow ACLs before applying deny ACLs.
	// 1. Cleanup old policy if it failed to be created
	// 2. Build gress policies, create addressSets for peers
	// 3. Add policy to default deny port group.
	// 4. Build policy ACLs and port group.
	// Pods are not added to default deny port groups yet, this is just a preparation step
	// 5. Publish the selector index while holding the networkPolicies key lock.
	// 6. Sync existing local pods while pod handlers wait on the policy lock.

	npKey := getPolicyKey(policy)
	var np *networkPolicy

	// network policy will be annotated with this
	// annotation -- [ "k8s.ovn.org/acl-stateless": "true"] for the ingress/egress
	// policies to be added as stateless OVN ACL's.
	// if the above annotation is not present or set to false in network policy,
	// then corresponding egress/ingress policies will be added as stateful OVN ACL's.
	var statelessNetPol bool
	if config.OVNKubernetesFeature.EnableStatelessNetPol {
		// look for stateless annotation if the statelessNetPol feature flag is enabled
		val, ok := policy.Annotations[ovnStatelessNetPolAnnotationName]
		if ok && val == "true" {
			statelessNetPol = true
		}
	}

	err := bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		oldNP, found := bnc.networkPolicies.Load(npKey)
		if found {
			// 1. Cleanup old policy if it failed to be created
			if cleanupErr := bnc.cleanupNetworkPolicy(oldNP); cleanupErr != nil {
				return fmt.Errorf("cleanup for retrying network policy create failed: %v", cleanupErr)
			}
		}
		np, found = bnc.networkPolicies.LoadOrStore(npKey, NewNetworkPolicy(policy))
		if found {
			// that should never happen, because successful cleanup will delete np from bnc.networkPolicies
			return fmt.Errorf("network policy is found in the system, "+
				"while it should've been cleaned up, obj: %+v", np)
		}
		// no need to check np.deleted, since the object has just been created
		// now we have a new np stored in bnc.networkPolicies
		var err error

		if aclLogging.Deny != "" || aclLogging.Allow != "" {
			klog.Infof("ACL logging for network policy %s in namespace %s set to deny=%s, allow=%s",
				policy.Name, policy.Namespace, aclLogging.Deny, aclLogging.Allow)
		}

		// 2. Build gress policies, create addressSets for peers

		// Consider both ingress and egress rules of the policy regardless of this
		// policy type. A pod is isolated as long as as it is selected by any
		// namespace policy. Since we don't process all namespace policies on a
		// given policy update that might change the isolation status of a selected
		// pod, we have created the allow ACLs derived from the policy rules in case
		// the selected pods become isolated in the future even if that is not their
		// current status.

		// Go through each ingress rule.  For each ingress rule, create an
		// addressSet for the peer pods.
		for i, ingressJSON := range policy.Spec.Ingress {
			klog.V(5).Infof("Network policy ingress is %+v", ingressJSON)

			ingress := newGressPolicy(knet.PolicyTypeIngress, i, policy.Namespace, policy.Name, bnc.controllerName, statelessNetPol, bnc.GetNetInfo())
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.ingressPolicies = append(np.ingressPolicies, ingress)

			// Each ingress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range ingressJSON.Ports {
				ingress.addPortPolicy(&portJSON)
			}

			for _, fromJSON := range ingressJSON.From {
				err := bnc.setupGressPolicy(np, ingress, fromJSON)
				if err != nil {
					return err
				}
			}
		}

		// Go through each egress rule.  For each egress rule, create an
		// addressSet for the peer pods.
		for i, egressJSON := range policy.Spec.Egress {
			klog.V(5).Infof("Network policy egress is %+v", egressJSON)

			egress := newGressPolicy(knet.PolicyTypeEgress, i, policy.Namespace, policy.Name, bnc.controllerName, statelessNetPol, bnc.GetNetInfo())
			// append ingress policy to be able to cleanup created address set
			// see cleanupNetworkPolicy for details
			np.egressPolicies = append(np.egressPolicies, egress)

			// Each egress rule can have multiple ports to which we allow traffic.
			for _, portJSON := range egressJSON.Ports {
				egress.addPortPolicy(&portJSON)
			}

			for _, toJSON := range egressJSON.To {
				err := bnc.setupGressPolicy(np, egress, toJSON)
				if err != nil {
					return err
				}
			}
		}
		klog.Infof("Policy %s added to peer address sets %v", npKey, np.peerAddressSets)

		// 3. Add policy to default deny port group
		// Pods are not added to default deny port groups yet, this is just a preparation step
		err = bnc.addPolicyToDefaultPortGroups(np, aclLogging)
		if err != nil {
			return err
		}

		// 4. Build policy ACLs and port group; pods are added after indexing.

		pgDbIDs := bnc.getNetworkPolicyPortGroupDbIDs(policy.Namespace, policy.Name)
		np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
		ops := []ovsdb.Operation{}

		acls := bnc.buildNetworkPolicyACLs(np, aclLogging)
		ops, err = libovsdbops.CreateOrUpdateACLsOps(bnc.nbClient, ops, bnc.GetSamplingConfig(), acls...)
		if err != nil {
			return fmt.Errorf("failed to create ACL ops: %v", err)
		}

		pg := libovsdbutil.BuildPortGroup(pgDbIDs, nil, acls)
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(bnc.nbClient, ops, pg)
		if err != nil {
			return fmt.Errorf("failed to create ops to add port to a port group: %v", err)
		}

		var recordOps []ovsdb.Operation
		var txOkCallBack func()
		recordOps, txOkCallBack, _, err = bnc.AddConfigDurationRecord("networkpolicy", policy.Namespace, policy.Name)
		if err != nil {
			klog.Errorf("Failed to record config duration: %v", err)
		}
		ops = append(ops, recordOps...)

		_, err = libovsdbops.TransactAndCheck(bnc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to run ovsdb txn to add ports to port group: %v", err)
		}
		txOkCallBack()

		sel, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("could not set up local pod selector for network policy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
		np.localPodSelector = sel
		bnc.addNetworkPolicyToNamespaceIndex(np)

		// Pod handlers observing the published index wait on the policy key lock
		// until the initial snapshot and its recorded state are committed.
		pods, err := bnc.watchFactory.GetPodsBySelector(policy.Namespace, policy.Spec.PodSelector)
		if err != nil {
			return fmt.Errorf("failed to list local pods for network policy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
		if err := bnc.bootstrapLocalPodsForNetworkPolicy(np, pods); err != nil {
			return fmt.Errorf("failed to bootstrap local pods for network policy %s/%s on network %s: %w",
				policy.Namespace, policy.Name, bnc.GetNetworkName(), err)
		}
		return nil
	})
	return np, err
}

// this method makes nil and empty PodSelectors behave differently (it shouldn't be the case,
// but this is a legacy behaviour that customers rely on).
// nil selector will use namespace address sets, a side-effect of this is hostNetwork pods won't be selected, and
// config.Kubernetes.HostNetworkNamespace will be included with empty NamespaceSelector.
func useNamespaceAddrSet(peer knet.NetworkPolicyPeer) bool {
	return peer.NamespaceSelector != nil && peer.PodSelector == nil
}

func (bnc *BaseNetworkController) setupGressPolicy(np *networkPolicy, gp *gressPolicy,
	peer knet.NetworkPolicyPeer) error {
	// Add IPBlock to ingress network policy
	if peer.IPBlock != nil {
		gp.addIPBlock(peer.IPBlock)
		return nil
	}
	if peer.PodSelector == nil && peer.NamespaceSelector == nil {
		// undefined behaviour
		klog.Errorf("setupGressPolicy failed: all fields unset")
		return nil
	}
	// use podSelector address set
	podSelector := peer.PodSelector
	if podSelector == nil {
		// nil pod selector is equivalent to empty pod selector, which selects all
		podSelector = &metav1.LabelSelector{}
	}
	// np.namespace will be used when fromJSON.NamespaceSelector = nil
	asKey, ipv4as, ipv6as, err := bnc.addressSetManager.EnsureAddressSet(
		podSelector, peer.NamespaceSelector, nil, np.namespace, np.getKeyWithKind(), bnc.controllerName, bnc.GetNetInfo(), useNamespaceAddrSet(peer))
	// even if GetPodSelectorAddressSet failed, add key for future cleanup or retry.
	np.peerAddressSets = append(np.peerAddressSets, asKey)
	if err != nil {
		return fmt.Errorf("failed to ensure pod selector address set %s: %v", asKey, err)
	}
	gp.addPeerAddressSets(ipv4as, ipv6as)

	return nil
}

// addNetworkPolicy creates and applies OVN ACLs to pod logical switch
// ports from Kubernetes NetworkPolicy objects using OVN Port Groups
// if addNetworkPolicy fails, create or delete operation can be retried
func (bnc *BaseNetworkController) addNetworkPolicy(policy *knet.NetworkPolicy) error {
	klog.Infof("Adding network policy %s for network %s", getPolicyKey(policy), bnc.GetNetworkName())
	if bnc.networkPolicyMetricsEnabled() {
		start := time.Now()
		defer func() {
			duration := time.Since(start)
			metrics.RecordNetpolEvent("add", duration)
		}()
	}

	// To not hold nsLock for the whole process on network policy creation, we do the following:
	// 1. save required namespace information to use for netpol create
	// 2. create network policy without ns Lock
	// 3. lock namespace
	// 4. check if namespace information related to network policy has changed, run the same function as on namespace update
	// 5. subscribe to namespace update events
	// 6. unlock namespace

	// 1. save required namespace information to use for netpol create,
	npKey := getPolicyKey(policy)
	nsInfo, nsUnlock := bnc.getNamespaceLocked(policy.Namespace, true)
	if nsInfo == nil {
		return fmt.Errorf("unable to get namespace for network policy %s: namespace doesn't exist", npKey)
	}
	aclLogging := nsInfo.aclLogging
	nsUnlock()

	// 2. create network policy without ns Lock, cleanup on failure
	_, err := bnc.createNetworkPolicy(policy, &aclLogging)
	defer func() {
		if err != nil {
			klog.Infof("Create network policy %s failed, try to cleanup", npKey)
			// try to cleanup network policy straight away
			// it will be retried later with add/delete network policy handlers if it fails
			cleanupErr := bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
				np, ok := bnc.networkPolicies.Load(npKey)
				if !ok {
					klog.Infof("Deleting policy %s that is already deleted", npKey)
					return nil
				}
				return bnc.cleanupNetworkPolicy(np)
			})
			if cleanupErr != nil {
				klog.Infof("Cleanup for failed create network policy %s returned an error: %v",
					npKey, cleanupErr)
			}
		}
	}()
	if err != nil {
		return fmt.Errorf("failed to create Network Policy %s: %v", npKey, err)
	}
	klog.Infof("Create network policy %s resources completed, update namespace loglevel", npKey)

	// 3. lock namespace
	nsInfo, nsUnlock = bnc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo == nil {
		// namespace was deleted while we were adding network policy,
		// try to cleanup network policy
		// expect retry to be handled by delete event that should come
		err = fmt.Errorf("unable to get namespace at the end of network policy %s creation: %v", npKey, err)
		return err
	}
	// 6. defer unlock namespace
	defer nsUnlock()

	// 4. check if namespace information related to network policy has changed,
	// network policy only reacts to namespace update ACL log level.
	// Run handleNetPolNamespaceUpdate sequence, but only for 1 newly added policy.
	if nsInfo.aclLogging.Deny != aclLogging.Deny {
		if err = bnc.updateACLLoggingForDefaultACLs(policy.Namespace, nsInfo); err != nil {
			return fmt.Errorf("network policy %s failed to be created: update default deny ACLs failed: %v", npKey, err)
		} else {
			klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
				npKey, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}
	if nsInfo.aclLogging.Allow != aclLogging.Allow {
		err = bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
			np, ok := bnc.networkPolicies.Load(npKey)
			if !ok {
				return nil
			}
			return bnc.updateACLLoggingForPolicy(np, &nsInfo.aclLogging)
		})
		if err != nil {
			return fmt.Errorf("network policy %s failed to be created: update policy ACLs failed: %v", npKey, err)
		} else {
			klog.Infof("Policy %s: ACL logging setting updated to deny=%s allow=%s",
				npKey, nsInfo.aclLogging.Deny, nsInfo.aclLogging.Allow)
		}
	}

	// 5. subscribe to namespace update events
	nsInfo.relatedNetworkPolicies[npKey] = true
	return nil
}

// buildNetworkPolicyACLs builds the ACLS associated with the 'gress policies
// of the provided network policy.
func (bnc *BaseNetworkController) buildNetworkPolicyACLs(np *networkPolicy, aclLogging *libovsdbutil.ACLLoggingLevels) []*nbdb.ACL {
	acls := []*nbdb.ACL{}
	for _, gp := range np.ingressPolicies {
		acl, _ := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}
	for _, gp := range np.egressPolicies {
		acl, _ := gp.buildLocalPodACLs(np.portGroupName, aclLogging)
		acls = append(acls, acl...)
	}

	return acls
}

// NetworkPolicy metrics are recorded for the default and primary UDN controllers.
func (bnc *BaseNetworkController) networkPolicyMetricsEnabled() bool {
	return config.Metrics.EnableScaleMetrics && (bnc.IsDefault() || bnc.IsPrimaryNetwork())
}

// deleteNetworkPolicy removes a network policy
// It only uses Namespace and Name from given network policy
func (bnc *BaseNetworkController) deleteNetworkPolicy(policy *knet.NetworkPolicy) error {
	npKey := getPolicyKey(policy)
	var start time.Time
	if bnc.networkPolicyMetricsEnabled() {
		start = time.Now()
	}
	// First lock and update namespace
	nsInfo, nsUnlock := bnc.getNamespaceLocked(policy.Namespace, false)
	if nsInfo != nil {
		// unsubscribe from namespace events
		delete(nsInfo.relatedNetworkPolicies, npKey)
		nsUnlock()
	}
	// Next cleanup network policy
	err := bnc.networkPolicies.DoWithLock(npKey, func(npKey string) error {
		np, ok := bnc.networkPolicies.Load(npKey)
		if !ok {
			return nil
		}
		if bnc.networkPolicyMetricsEnabled() {
			defer func() {
				duration := time.Since(start)
				metrics.RecordNetpolEvent("delete", duration)
			}()
		}
		if err := bnc.cleanupNetworkPolicy(np); err != nil {
			return fmt.Errorf("deleting policy %s failed: %v", npKey, err)
		}
		return nil
	})
	return err
}

// cleanupNetworkPolicy should be retriable
// It updates bnc.networkPolicies on success, should be called with bnc.networkPolicies key locked.
// No need to log network policy key here, because caller of cleanupNetworkPolicy should prepend error message with
// that information.
func (bnc *BaseNetworkController) cleanupNetworkPolicy(np *networkPolicy) error {
	npKey := np.getKey()
	klog.Infof("Cleaning up network policy %s", npKey)
	// Prevent pod updates from using a policy whose cleanup is still pending.
	np.deleted = true
	bnc.deleteNetworkPolicyFromNamespaceIndex(np)
	var err error

	// Delete the port group, idempotent
	ops, err := libovsdbops.DeletePortGroupsOps(bnc.nbClient, nil, np.portGroupName)
	if err != nil {
		return fmt.Errorf("failed to get delete network policy port group %s ops: %v", np.portGroupName, err)
	}
	recordOps, txOkCallBack, _, err := bnc.AddConfigDurationRecord("networkpolicy", np.namespace, np.name)
	if err != nil {
		klog.Errorf("Failed to record config duration: %v", err)
	}
	ops = append(ops, recordOps...)

	err = bnc.denyPGDeletePorts(np, nil, true, ops)
	if err != nil {
		return fmt.Errorf("unable to delete ports from defaultDeny port group: %v", err)
	}
	// transaction was successful, exec callback
	txOkCallBack()
	// cleanup local pods, since they were deleted from port groups
	np.localPods = sync.Map{}
	np.localPodPorts = sync.Map{}

	err = bnc.delPolicyFromDefaultPortGroups(np)
	if err != nil {
		return fmt.Errorf("unable to delete policy from default deny port groups: %v", err)
	}

	// delete from peer address set, this may cause address set deletion, so we need to
	// do that after ACLs are deleted to avoid ovn-controller errors
	for i, asKey := range np.peerAddressSets {
		if err := bnc.addressSetManager.DeleteAddressSet(asKey, np.getKeyWithKind()); err != nil {
			// remove deleted address sets from the list
			np.peerAddressSets = np.peerAddressSets[i:]
			return fmt.Errorf("failed to delete network policy from peer address set %s: %v", asKey, err)
		}
	}
	np.peerAddressSets = nil

	// finally, delete netpol from existing networkPolicies
	// this is the signal that cleanup was successful
	bnc.networkPolicies.Delete(npKey)
	return nil
}

// The following 2 functions should return the same key for network policy based on k8s on internal networkPolicy object
func getPolicyKey(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v", policy.Namespace, policy.Name)
}

func (np *networkPolicy) getKey() string {
	return fmt.Sprintf("%v/%v", np.namespace, np.name)
}

func (np *networkPolicy) getKeyWithKind() string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", np.namespace, np.name)
}

// PortGroupHasPorts returns true if a port group contains all given ports
func PortGroupHasPorts(nbClient libovsdbclient.Client, pgName string, portUUIDs []string) bool {
	pg := &nbdb.PortGroup{
		Name: pgName,
	}
	pg, err := libovsdbops.GetPortGroup(nbClient, pg)
	if err != nil {
		return false
	}

	return sets.NewString(pg.Ports...).HasAll(portUUIDs...)
}

func (bnc *BaseNetworkController) getNetpolDefaultACLDbIDs(direction string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolDefault, bnc.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      allowHairpinningACLID,
			libovsdbops.PolicyDirectionKey: direction,
		})
}
