package acl

import (
	"fmt"
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
