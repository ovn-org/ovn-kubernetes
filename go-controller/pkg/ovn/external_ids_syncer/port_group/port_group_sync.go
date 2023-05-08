package port_group

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"k8s.io/klog/v2"
)

const (
	// port groups suffixes
	// ingressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	ingressDefaultDenySuffix = "ingressDefaultDeny"
	// egressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	egressDefaultDenySuffix      = "egressDefaultDeny"
	defaultNetworkControllerName = "default-network-controller"
)

type updatePortGroupInfo struct {
	acls  []*nbdb.ACL
	oldPG *nbdb.PortGroup
	newPG *nbdb.PortGroup
}

type PortGroupSyncer struct {
	nbClient libovsdbclient.Client
	// txnBatchSize is used to control how many port groups will be updated with 1 db transaction.
	// Set to 0 to disable batching
	txnBatchSize int
}

func getControllerName(networkExternalID string) string {
	if networkExternalID == "" {
		return defaultNetworkControllerName
	}
	return networkExternalID + "-network-controller"
}

// NewPortGroupSyncer creates a PortGroupSyncer that will sync port groups without the new ExternalIDs for all
// controllers. Controller name will be defined based on getControllerName() function.
func NewPortGroupSyncer(nbClient libovsdbclient.Client) *PortGroupSyncer {
	return &PortGroupSyncer{
		nbClient:     nbClient,
		txnBatchSize: 50,
	}
}

func getPortGroupMulticastDbIDs(namespace, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupMulticast, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: namespace,
	})
}

func getPortGroupNetpolNamespaceDbIDs(namespace, direction, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetpolNamespace, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey:      namespace,
		libovsdbops.PolicyDirectionKey: direction,
	})
}

func getPortGroupNetworkPolicyDbIDs(policyNamespace, policyName, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNetworkPolicy, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s", policyNamespace, policyName),
		})
}

func getPortGroupClusterDbIDs(baseName, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupCluster, controllerName, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: baseName,
	})
}

// getReferencingObjsAndNewDbIDs finds all object that reference stale port group and tries to create a new dbIDs
// based on referencing objects
func (syncer *PortGroupSyncer) getReferencingObjsAndNewDbIDs(oldHash, oldName, networkExternalID string) (acls []*nbdb.ACL,
	dbIDs *libovsdbops.DbObjectIDs, err error) {
	// find all referencing objects
	aclPred := func(acl *nbdb.ACL) bool {
		return strings.Contains(acl.Match, "@"+oldHash)
	}
	acls, err = libovsdbops.FindACLsWithPredicate(syncer.nbClient, aclPred)
	if err != nil {
		err = fmt.Errorf("failed to find acls for port group %s: %v", oldHash, err)
		return
	}
	// build dbIDs
	switch {
	// Filter port groups with pre-defined names
	case oldName == types.ClusterPortGroupNameBase || oldName == types.ClusterRtrPortGroupNameBase:
		dbIDs = getPortGroupClusterDbIDs(oldName, networkExternalID)
	case strings.Contains(oldName, "_"):
		// network policy owned namespace
		s := strings.SplitN(oldName, "_", 2)
		if s[1] == ingressDefaultDenySuffix || s[1] == egressDefaultDenySuffix {
			// default deny port group, name format = hash(namespace)_gressSuffix
			// need to find unhashed namespace name, use referencing ACLs
			if len(acls) == 0 {
				// default deny port group should always have acls
				err = fmt.Errorf("defaultDeny port group doesn't have any referencing ACLs, can't extract namespace")
				return
			}
			// all default deny acls will have the same namespace as ExternalID
			acl := acls[0]
			namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
			var direction string
			if s[1] == ingressDefaultDenySuffix {
				direction = "Ingress"
			} else {
				direction = "Egress"
			}
			dbIDs = getPortGroupNetpolNamespaceDbIDs(namespace, direction, networkExternalID)
		} else {
			// s[0]=policyNamespace, s[1]=policyName
			dbIDs = getPortGroupNetworkPolicyDbIDs(s[0], s[1], networkExternalID)
		}
	default:
		// multicast port group name is just namespace
		dbIDs = getPortGroupMulticastDbIDs(oldName, networkExternalID)
	}
	// dbIDs is set
	return
}

func (syncer *PortGroupSyncer) getUpdatePortGroupOps(portGroupInfos []*updatePortGroupInfo) (ops []libovsdb.Operation, err error) {
	// one referencing object may contain multiple references that need to be updated
	// these maps are used to track referenced that need to be replaced for every object type
	aclsToUpdate := map[string]*nbdb.ACL{}

	for _, portGroupInfo := range portGroupInfos {
		oldName := portGroupInfo.oldPG.ExternalIDs["name"]
		// create updated port group
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(syncer.nbClient, ops, portGroupInfo.newPG)
		if err != nil {
			return nil, fmt.Errorf("failed to get update port group ops for port group %s: %v", oldName, err)
		}
		// delete old port group
		ops, err = libovsdbops.DeletePortGroupsOps(syncer.nbClient, ops, portGroupInfo.oldPG.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get update port group ops for port group %s: %v", oldName, err)
		}
		oldHash := "@" + portGroupInfo.oldPG.Name
		newHash := "@" + portGroupInfo.newPG.Name

		for _, acl := range portGroupInfo.acls {
			if _, ok := aclsToUpdate[acl.UUID]; !ok {
				aclsToUpdate[acl.UUID] = acl
			}
			aclsToUpdate[acl.UUID].Match = strings.ReplaceAll(aclsToUpdate[acl.UUID].Match, oldHash, newHash)
		}
	}

	for _, acl := range aclsToUpdate {
		ops, err = libovsdbops.UpdateACLsOps(syncer.nbClient, ops, acl)
		if err != nil {
			return nil, fmt.Errorf("failed to get update acl ops: %v", err)
		}
	}

	return
}

// getPortGroupUpdateInfo adds db ops to update port group and objects that reference it
func (syncer *PortGroupSyncer) getPortGroupUpdateInfo(pg *nbdb.PortGroup) (*updatePortGroupInfo, error) {
	pgName := pg.ExternalIDs["name"]
	networkExternalID := pg.ExternalIDs[types.NetworkExternalID]
	if pgName == "" {
		return nil, fmt.Errorf("port group doesn't have expecter ExternalID[\"name\"]")
	}

	acls, dbIDs, err := syncer.getReferencingObjsAndNewDbIDs(pg.Name, pgName, networkExternalID)
	if err != nil {
		return nil, fmt.Errorf("failed to get new dbIDs for port group %s: %v", pg.ExternalIDs["name"], err)
	}

	// since we need to update portGroup.Name, which is an index and not listed in getAllUpdatableFields,
	// we copy existing portGroup, update the required fields, and replace (delete and create) existing port group with the updated
	newPG := pg.DeepCopy()
	// reset UUID
	newPG.UUID = ""
	// update port group Name and ExternalIDs
	externalIDs := dbIDs.GetExternalIDs()
	newPG.Name = externalIDs[libovsdbops.PrimaryIDKey.String()]
	newPG.ExternalIDs = externalIDs
	return &updatePortGroupInfo{acls, pg, newPG}, nil
}

// SyncPortGroups must be run after ACLs sync, since it uses new ACL.ExternalIDs
func (syncer *PortGroupSyncer) SyncPortGroups() error {
	// stale port groups don't have controller ID
	portGroupList, err := libovsdbops.FindPortGroupsWithPredicate(syncer.nbClient, libovsdbops.GetNoOwnerPredicate[*nbdb.PortGroup]())
	if err != nil {
		return fmt.Errorf("failed to find stale port groups: %v", err)
	}

	updatedCount := 0
	defer func() {
		klog.Infof("SyncPortGroups handled %d of %d stale port groups", updatedCount, len(portGroupList))
	}()
	i := 0
	for i < len(portGroupList) {
		pgUpdateInfos := []*updatePortGroupInfo{}
		for j := 0; (j < syncer.txnBatchSize || syncer.txnBatchSize == 0) && i < len(portGroupList); i, j = i+1, j+1 {
			updateInfo, err := syncer.getPortGroupUpdateInfo(portGroupList[i])
			if err != nil {
				return err
			}
			pgUpdateInfos = append(pgUpdateInfos, updateInfo)
		}
		// generate update ops
		ops, err := syncer.getUpdatePortGroupOps(pgUpdateInfos)
		if err != nil {
			return fmt.Errorf("failed to get update port groups ops: %w", err)
		}
		_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to transact port group sync ops: %v", err)
		}
		updatedCount = i
	}
	return nil
}
