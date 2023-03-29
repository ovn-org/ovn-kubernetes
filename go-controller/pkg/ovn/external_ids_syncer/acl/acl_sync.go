package acl

import (
	"fmt"
	"strconv"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/batching"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	defaultDenyPolicyTypeACLExtIdKey = "default-deny-policy-type"
	mcastDefaultDenyID               = "DefaultDeny"
	mcastAllowInterNodeID            = "AllowInterNode"
	l4MatchACLExtIdKey               = "l4Match"
	ipBlockCIDRACLExtIdKey           = "ipblock_cidr"
	namespaceACLExtIdKey             = "namespace"
	policyACLExtIdKey                = "policy"
	policyTypeACLExtIdKey            = "policy_type"
	policyTypeNumACLExtIdKey         = "%s_num"
	noneMatch                        = "None"
	emptyIdx                         = -1
)

type aclSyncer struct {
	nbClient       libovsdbclient.Client
	controllerName string
	// txnBatchSize is used to control how many acls will be updated with 1 db transaction.
	txnBatchSize int
}

// controllerName is the name of the new controller that should own all acls without controller
func NewACLSyncer(nbClient libovsdbclient.Client, controllerName string) *aclSyncer {
	return &aclSyncer{
		nbClient:       nbClient,
		controllerName: controllerName,
		// create time (which is the upper bound of how much time an update can take) for 20K ACLs
		// (gress ACL were used for testing as the ones that have the biggest number of ExternalIDs)
		// is ~4 sec, which is safe enough to not exceed 10 sec transaction timeout.
		txnBatchSize: 20000,
	}
}

func (syncer *aclSyncer) SyncACLs(existingNodes *v1.NodeList) error {
	// stale acls don't have controller ID
	legacyAclPred := libovsdbops.GetNoOwnerPredicate[*nbdb.ACL]()
	legacyACLs, err := libovsdbops.FindACLsWithPredicate(syncer.nbClient, legacyAclPred)
	if err != nil {
		return fmt.Errorf("unable to find stale ACLs, cannot update stale data: %v", err)
	}
	if len(legacyACLs) == 0 {
		return nil
	}

	var updatedACLs []*nbdb.ACL
	multicastACLs := syncer.updateStaleMulticastACLsDbIDs(legacyACLs)
	klog.Infof("Found %d stale multicast ACLs", len(multicastACLs))
	updatedACLs = append(updatedACLs, multicastACLs...)

	allowFromNodeACLs := syncer.updateStaleNetpolNodeACLs(legacyACLs, existingNodes.Items)
	klog.Infof("Found %d stale allow from node ACLs", len(allowFromNodeACLs))
	updatedACLs = append(updatedACLs, allowFromNodeACLs...)

	gressPolicyACLs, err := syncer.updateStaleGressPolicies(legacyACLs)
	if err != nil {
		return fmt.Errorf("failed to update gress policy ACLs: %w", err)
	}
	klog.Infof("Found %d stale gress ACLs", len(gressPolicyACLs))
	updatedACLs = append(updatedACLs, gressPolicyACLs...)

	err = batching.Batch[*nbdb.ACL](syncer.txnBatchSize, updatedACLs, func(batchACLs []*nbdb.ACL) error {
		return libovsdbops.CreateOrUpdateACLs(syncer.nbClient, batchACLs...)
	})
	if err != nil {
		return fmt.Errorf("cannot update stale ACLs: %v", err)
	}
	return nil
}

func (syncer *aclSyncer) getDefaultMcastACLDbIDs(mcastType, policyDirection string) *libovsdbops.DbObjectIDs {
	// there are 2 types of default multicast ACLs in every direction (Ingress/Egress)
	// DefaultDeny = deny multicast by default
	// AllowInterNode = allow inter-node multicast
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastCluster, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.TypeKey:            mcastType,
			libovsdbops.PolicyDirectionKey: policyDirection,
		})

}

func (syncer *aclSyncer) getNamespaceMcastACLDbIDs(ns, policyDirection string) *libovsdbops.DbObjectIDs {
	// namespaces ACL
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLMulticastNamespace, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:      ns,
			libovsdbops.PolicyDirectionKey: policyDirection,
		})
}

// updateStaleMulticastACLsDbIDs updates multicast ACLs that don't have new ExternalIDs set.
// Must be run before WatchNamespace, since namespaceSync function uses syncNsMulticast, which relies on the new IDs.
func (syncer *aclSyncer) updateStaleMulticastACLsDbIDs(legacyACLs []*nbdb.ACL) []*nbdb.ACL {
	updatedACLs := []*nbdb.ACL{}
	for _, acl := range legacyACLs {
		var dbIDs *libovsdbops.DbObjectIDs
		if acl.Priority == types.DefaultMcastDenyPriority {
			// there is only 1 type acl type with this priority: default deny
			dbIDs = syncer.getDefaultMcastACLDbIDs(mcastDefaultDenyID, acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey])
		} else if acl.Priority == types.DefaultMcastAllowPriority {
			// there are 4 multicast allow types
			if acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == string(knet.PolicyTypeIngress) {
				// ingress allow acl
				// either default of namespaced
				if strings.Contains(acl.Match, types.ClusterRtrPortGroupName) {
					// default allow ingress
					dbIDs = syncer.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, string(knet.PolicyTypeIngress))
				} else {
					// namespace allow ingress
					// acl Name can be truncated (max length 64), but k8s namespace is limited to 63 symbols,
					// therefore it is safe to extract it from the name
					ns := strings.Split(libovsdbops.GetACLName(acl), "_")[0]
					dbIDs = syncer.getNamespaceMcastACLDbIDs(ns, string(knet.PolicyTypeIngress))
				}
			} else if acl.ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] == string(knet.PolicyTypeEgress) {
				// egress allow acl
				// either default of namespaced
				if strings.Contains(acl.Match, types.ClusterRtrPortGroupName) {
					// default allow egress
					dbIDs = syncer.getDefaultMcastACLDbIDs(mcastAllowInterNodeID, string(knet.PolicyTypeEgress))
				} else {
					// namespace allow egress
					// acl Name can be truncated (max length 64), but k8s namespace is limited to 63 symbols,
					// therefore it is safe to extract it from the name
					ns := strings.Split(libovsdbops.GetACLName(acl), "_")[0]
					dbIDs = syncer.getNamespaceMcastACLDbIDs(ns, string(knet.PolicyTypeEgress))
				}
			} else {
				// unexpected, acl with multicast priority should have ExternalIDs[defaultDenyPolicyTypeACLExtIdKey] set
				klog.Warningf("Found stale ACL with multicast priority %d, but without expected ExternalID[%s]: %+v",
					acl.Priority, defaultDenyPolicyTypeACLExtIdKey, acl)
				continue
			}
		} else {
			//non-multicast acl
			continue
		}
		// update externalIDs
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return updatedACLs
}

func (syncer *aclSyncer) getAllowFromNodeACLDbIDs(nodeName, mgmtPortIP string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetpolNode, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: nodeName,
			libovsdbops.IpKey:         mgmtPortIP,
		})
}

// updateStaleNetpolNodeACLs updates allow from node ACLs, that don't have new ExternalIDs based
// on DbObjectIDs set. Allow from node acls are applied on the node switch, therefore the cleanup for deleted is not needed,
// since acl will be deleted toegther with the node switch.
func (syncer *aclSyncer) updateStaleNetpolNodeACLs(legacyACLs []*nbdb.ACL, existingNodes []v1.Node) []*nbdb.ACL {
	// ACL to allow traffic from host via management port has no name or ExternalIDs
	// The only way to find it is by exact match
	type aclInfo struct {
		nodeName string
		ip       string
	}
	matchToNode := map[string]aclInfo{}
	for _, node := range existingNodes {
		hostSubnets, err := util.ParseNodeHostSubnetAnnotation(&node, types.DefaultNetworkName)
		if err != nil {
			klog.Warningf("Couldn't parse hostSubnet annotation for node %s: %v", node.Name, err)
			continue
		}
		for _, hostSubnet := range hostSubnets {
			mgmtIfAddr := util.GetNodeManagementIfAddr(hostSubnet)
			ipFamily := "ip4"
			if utilnet.IsIPv6(mgmtIfAddr.IP) {
				ipFamily = "ip6"
			}
			match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIfAddr.IP.String())
			matchToNode[match] = aclInfo{
				nodeName: node.Name,
				ip:       mgmtIfAddr.IP.String(),
			}
		}
	}
	updatedACLs := []*nbdb.ACL{}
	for _, acl := range legacyACLs {
		if _, ok := matchToNode[acl.Match]; ok {
			aclInfo := matchToNode[acl.Match]
			dbIDs := syncer.getAllowFromNodeACLDbIDs(aclInfo.nodeName, aclInfo.ip)
			// Update ExternalIDs and Name based on new dbIndex
			acl.ExternalIDs = dbIDs.GetExternalIDs()
			updatedACLs = append(updatedACLs, acl)
		}
	}
	return updatedACLs
}

func (syncer *aclSyncer) getNetpolGressACLDbIDs(policyNamespace, policyName, policyType string,
	gressIdx, portPolicyIdx, ipBlockIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLNetworkPolicy, syncer.controllerName,
		map[libovsdbops.ExternalIDKey]string{
			// policy namespace+name
			libovsdbops.ObjectNameKey: policyNamespace + ":" + policyName,
			// egress or ingress
			libovsdbops.PolicyDirectionKey: policyType,
			// gress rule index
			libovsdbops.GressIdxKey: strconv.Itoa(gressIdx),
			// acls are created for every gp.portPolicies:
			// - for empty policy (no selectors and no ip blocks) - empty ACL
			// OR
			// - all selector-based peers ACL
			// - for every IPBlock +1 ACL
			// Therefore unique id for given gressPolicy is portPolicy idx + IPBlock idx
			// (empty policy and all selector-based peers ACLs will have idx=-1)
			libovsdbops.PortPolicyIndexKey: strconv.Itoa(portPolicyIdx),
			libovsdbops.IpBlockIndexKey:    strconv.Itoa(ipBlockIdx),
		})
}

func (syncer *aclSyncer) updateStaleGressPolicies(legacyACLs []*nbdb.ACL) (updatedACLs []*nbdb.ACL, err error) {
	if len(legacyACLs) == 0 {
		return
	}
	// for every gress policy build mapping to count port policies.
	// l4MatchACLExtIdKey was previously assigned based on port policy,
	// we can just assign idx to every port Policy and make sure there are no equal ACLs.
	gressPolicyPortCount := map[string]map[string]int{}
	for _, acl := range legacyACLs {
		if acl.ExternalIDs[policyTypeACLExtIdKey] == "" {
			// not gress ACL
			continue
		}
		policyNamespace := acl.ExternalIDs[namespaceACLExtIdKey]
		policyName := acl.ExternalIDs[policyACLExtIdKey]
		policyType := acl.ExternalIDs[policyTypeACLExtIdKey]
		idxKey := fmt.Sprintf(policyTypeNumACLExtIdKey, policyType)
		idx, err := strconv.Atoi(acl.ExternalIDs[idxKey])
		if err != nil {
			return nil, fmt.Errorf("unable to parse gress policy idx %s: %v",
				acl.ExternalIDs[idxKey], err)
		}
		var ipBlockIdx int
		// ipBlockCIDRACLExtIdKey is "false" for non-ipBlock ACLs.
		// Then for the first ipBlock in a given gress policy it is "true",
		// and for the rest of them is idx+1
		if acl.ExternalIDs[ipBlockCIDRACLExtIdKey] == "true" {
			ipBlockIdx = 0
		} else if acl.ExternalIDs[ipBlockCIDRACLExtIdKey] == "false" {
			ipBlockIdx = emptyIdx
		} else {
			ipBlockIdx, err = strconv.Atoi(acl.ExternalIDs[ipBlockCIDRACLExtIdKey])
			if err != nil {
				return nil, fmt.Errorf("unable to parse gress policy ipBlockCIDRACLExtIdKey %s: %v",
					acl.ExternalIDs[ipBlockCIDRACLExtIdKey], err)
			}
			ipBlockIdx -= 1
		}
		gressACLID := strings.Join([]string{policyNamespace, policyName, policyType, fmt.Sprintf("%d", idx)}, "_")
		if gressPolicyPortCount[gressACLID] == nil {
			gressPolicyPortCount[gressACLID] = map[string]int{}
		}
		var portIdx int
		l4Match := acl.ExternalIDs[l4MatchACLExtIdKey]
		if l4Match == noneMatch {
			portIdx = emptyIdx
		} else {
			if _, ok := gressPolicyPortCount[gressACLID][l4Match]; !ok {
				// this l4MatchACLExtIdKey is new for given gressPolicy, assign the next idx to it
				gressPolicyPortCount[gressACLID][l4Match] = len(gressPolicyPortCount[gressACLID])
			}
			portIdx = gressPolicyPortCount[gressACLID][l4Match]
		}
		dbIDs := syncer.getNetpolGressACLDbIDs(policyNamespace, policyName,
			policyType, idx, portIdx, ipBlockIdx)
		acl.ExternalIDs = dbIDs.GetExternalIDs()
		updatedACLs = append(updatedACLs, acl)
	}
	return
}
