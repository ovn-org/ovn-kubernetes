package ovn

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

const (
	noneMatch = "None"
	// IPv6 multicast traffic destined to dynamic groups must have the "T" bit
	// set to 1: https://tools.ietf.org/html/rfc3307#section-4.3
	ipv6DynamicMulticastMatch = "(ip6.dst[120..127] == 0xff && ip6.dst[116] == 1)"
	// Legacy multicastDefaultDeny port group removed by commit 40a90f0
	legacyMulticastDefaultDenyPortGroup = "mcastPortGroupDeny"
)

func getACLMatchAF(ipv4Match, ipv6Match string) string {
	if config.IPv4Mode && config.IPv6Mode {
		return "(" + ipv4Match + " || " + ipv6Match + ")"
	} else if config.IPv4Mode {
		return ipv4Match
	} else {
		return ipv6Match
	}
}

// Creates the match string used for ACLs matching on multicast traffic.
func getMulticastACLMatch() string {
	return "(ip4.mcast || mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")"
}

// Allow IGMP traffic (e.g., IGMP queries) and namespace multicast traffic
// towards pods.
func getMulticastACLIgrMatchV4(addrSetName string) string {
	return "(igmp || (ip4.src == $" + addrSetName + " && ip4.mcast))"
}

// Allow MLD traffic (e.g., MLD queries) and namespace multicast traffic
// towards pods.
func getMulticastACLIgrMatchV6(addrSetName string) string {
	return "(mldv1 || mldv2 || (ip6.src == $" + addrSetName + " && " + ipv6DynamicMulticastMatch + "))"
}

// Creates the match string used for ACLs allowing incoming multicast into a
// namespace, that is, from IPs that are in the namespace's address set.
func getMulticastACLIgrMatch(nsInfo *namespaceInfo) string {
	var ipv4Match, ipv6Match string
	addrSetNameV4, addrSetNameV6 := nsInfo.addressSet.GetASHashNames()
	if config.IPv4Mode {
		ipv4Match = getMulticastACLIgrMatchV4(addrSetNameV4)
	}
	if config.IPv6Mode {
		ipv6Match = getMulticastACLIgrMatchV6(addrSetNameV6)
	}
	return getACLMatchAF(ipv4Match, ipv6Match)
}

// Creates the match string used for ACLs allowing outgoing multicast from a
// namespace.
func getMulticastACLEgrMatch() string {
	var ipv4Match, ipv6Match string
	if config.IPv4Mode {
		ipv4Match = "ip4.mcast"
	}
	if config.IPv6Mode {
		ipv6Match = "(mldv1 || mldv2 || " + ipv6DynamicMulticastMatch + ")"
	}
	return getACLMatchAF(ipv4Match, ipv6Match)
}

func getMcastACLName(nsORpg, mcastSuffix string) string {
	return joinACLName(nsORpg, mcastSuffix)
}

// Creates a policy to allow multicast traffic within 'ns':
// - a port group containing all logical ports associated with 'ns'
// - one "from-lport" ACL allowing egress multicast traffic from the pods
//   in 'ns'
// - one "to-lport" ACL allowing ingress multicast traffic to pods in 'ns'.
//   This matches only traffic originated by pods in 'ns' (based on the
//   namespace address set).
func (oc *DefaultNetworkController) createMulticastAllowPolicy(ns string, nsInfo *namespaceInfo) error {
	portGroupName := hashedPortGroup(ns)

	aclT := lportEgressAfterLB
	egressMatch := getACLMatch(portGroupName, getMulticastACLEgrMatch(), aclT)
	egressACL := BuildACL(getMcastACLName(ns, "MulticastAllowEgress"),
		types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, nil, aclT,
		getDefaultDenyPolicyExternalIDs(aclT))

	aclT = lportIngress
	ingressMatch := getACLMatch(portGroupName, getMulticastACLIgrMatch(nsInfo), aclT)
	ingressACL := BuildACL(getMcastACLName(ns, "MulticastAllowIngress"),
		types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, nil, aclT,
		getDefaultDenyPolicyExternalIDs(aclT))

	acls := []*nbdb.ACL{egressACL, ingressACL}
	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, acls...)
	if err != nil {
		return err
	}

	// Add all ports from this namespace to the multicast allow group.
	ports := []*nbdb.LogicalSwitchPort{}
	pods, err := oc.watchFactory.GetPods(ns)
	if err != nil {
		klog.Warningf("Failed to get pods for namespace %q: %v", ns, err)
	}
	for _, pod := range pods {
		if util.PodCompleted(pod) {
			continue
		}
		if portInfo, err := oc.logicalPortCache.get(pod, types.DefaultNetworkName); err != nil {
			klog.Errorf(err.Error())
		} else {
			ports = append(ports, &nbdb.LogicalSwitchPort{UUID: portInfo.uuid})
		}
	}

	pg := libovsdbops.BuildPortGroup(portGroupName, ns, ports, acls)
	ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(oc.nbClient, ops, pg)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

func deleteMulticastAllowPolicy(nbClient libovsdbclient.Client, ns string) error {
	portGroupName := hashedPortGroup(ns)
	// ACLs referenced by the port group wil be deleted by db if there are no other references
	err := libovsdbops.DeletePortGroups(nbClient, portGroupName)
	if err != nil {
		return fmt.Errorf("failed deleting port group %s: %v", portGroupName, err)
	}

	return nil
}

// Creates a global default deny multicast policy:
// - one ACL dropping egress multicast traffic from all pods: this is to
//   protect OVN controller from processing IP multicast reports from nodes
//   that are not allowed to receive multicast traffic.
// - one ACL dropping ingress multicast traffic to all pods.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *DefaultNetworkController) createDefaultDenyMulticastPolicy() error {
	match := getMulticastACLMatch()

	// By default deny any egress multicast traffic from any pod. This drops
	// IP multicast membership reports therefore denying any multicast traffic
	// to be forwarded to pods.
	aclT := lportEgressAfterLB
	egressACL := BuildACL(getMcastACLName(types.ClusterPortGroupName, "DefaultDenyMulticastEgress"),
		types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, nil,
		aclT, getDefaultDenyPolicyExternalIDs(aclT))

	// By default deny any ingress multicast traffic to any pod.
	aclT = lportIngress
	ingressACL := BuildACL(getMcastACLName(types.ClusterPortGroupName, "DefaultDenyMulticastIngress"),
		types.DefaultMcastDenyPriority, match, nbdb.ACLActionDrop, nil,
		aclT, getDefaultDenyPolicyExternalIDs(aclT))

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, egressACL, ingressACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, types.ClusterPortGroupName, egressACL, ingressACL)
	if err != nil {
		return err
	}

	// Remove old multicastDefaultDeny port group now that all ports
	// have been added to the clusterPortGroup by WatchPods()
	ops, err = libovsdbops.DeletePortGroupsOps(oc.nbClient, ops, legacyMulticastDefaultDenyPortGroup)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// Creates a global default allow multicast policy:
// - one ACL allowing multicast traffic from cluster router ports
// - one ACL allowing multicast traffic to cluster router ports.
// Caller must hold the namespace's namespaceInfo object lock.
func (oc *DefaultNetworkController) createDefaultAllowMulticastPolicy() error {
	mcastMatch := getMulticastACLMatch()

	aclT := lportEgressAfterLB
	egressMatch := getACLMatch(types.ClusterRtrPortGroupName, mcastMatch, aclT)
	egressACL := BuildACL(getMcastACLName(types.ClusterRtrPortGroupName, "DefaultAllowMulticastEgress"),
		types.DefaultMcastAllowPriority, egressMatch, nbdb.ACLActionAllow, nil,
		aclT, getDefaultDenyPolicyExternalIDs(aclT))

	aclT = lportIngress
	ingressMatch := getACLMatch(types.ClusterRtrPortGroupName, mcastMatch, aclT)
	ingressACL := BuildACL(getMcastACLName(types.ClusterRtrPortGroupName, "DefaultAllowMulticastIngress"),
		types.DefaultMcastAllowPriority, ingressMatch, nbdb.ACLActionAllow, nil,
		aclT, getDefaultDenyPolicyExternalIDs(aclT))

	ops, err := libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, nil, egressACL, ingressACL)
	if err != nil {
		return err
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, types.ClusterRtrPortGroupName, egressACL, ingressACL)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return err
	}

	return nil
}

// podAddAllowMulticastPolicy adds the pod's logical switch port to the namespace's
// multicast port group. Caller must hold the namespace's namespaceInfo object
// lock.
func podAddAllowMulticastPolicy(nbClient libovsdbclient.Client, ns string, portInfo *lpInfo) error {
	return libovsdbops.AddPortsToPortGroup(nbClient, hashedPortGroup(ns), portInfo.uuid)
}

// podDeleteAllowMulticastPolicy removes the pod's logical switch port from the
// namespace's multicast port group. Caller must hold the namespace's
// namespaceInfo object lock.
func podDeleteAllowMulticastPolicy(nbClient libovsdbclient.Client, ns string, portUUID string) error {
	return libovsdbops.DeletePortsFromPortGroup(nbClient, hashedPortGroup(ns), portUUID)
}
