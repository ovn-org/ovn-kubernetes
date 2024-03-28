package port_group

import (
	"fmt"
	"regexp"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"k8s.io/klog/v2"
)

const (
	// port groups suffixes
	// ingressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	ingressDefaultDenySuffix = "ingressDefaultDeny"
	// egressDefaultDenySuffix is the suffix used when creating the ingress port group for a namespace
	egressDefaultDenySuffix      = "egressDefaultDeny"
	defaultNetworkControllerName = "default-network-controller"
	// values for libovsdbops.PolicyDirectionKey
	// We don't reuse any external constants in this package, as it should update db entries from a
	// pre-defined format. If in the future values for some external constants change, this package shouldn't be affected.
	policyDirectionIngress = "Ingress"
	policyDirectionEgress  = "Egress"
)

type updatePortGroupInfo struct {
	acls  []*nbdb.ACL
	oldPG *nbdb.PortGroup
	newPG *nbdb.PortGroup
}

type PortGroupSyncer struct {
	nbClient    libovsdbclient.Client
	getPGWeight func(acls, ports int) float64
}

// getPGWeight returns weight of a port group based on the number of linked acls and ports.
// port group syncer updates both port groups and related acls, transaction time depends on the number of acls related to every port group.
// The concept of "weight" reflects that port group with 1 ACL and port group with 100 ACLs will have different
// transaction time. Based on local testing, the results are
// Update for 30`000 port groups with 1 ACL each takes 7,14 sec
// here almost linear dependency begins
// Update for 5`000 port groups with 10 ACL each takes 6 sec
// Update for 500 port groups with 100 ACL each takes 5,95 sec
// Update for 50 port groups with 1000 ACL each takes 5,84 sec
// Update for 5 port groups with 10000 ACL each takes 5,42 sec
// Considering given times safe within 10 second timeout, 5 port groups with 10000 ACLs may be updated in one batch.
// That makes a weight of port group with 10000 ACLs = 1/5.
// The number of ports in a port group also affects the weight. By adding a given number of ports in the local testing
// we got the following extra transaction time (o ports = 0 extra time):
// 50 pg + 1000 ACL:
//
//	 1K ports => 0.69 s
//	 5K ports => 2.89 s
//	10K ports => 6.85 s
//
// 500 pg + 100 ACL:
//
//	 1K ports => 5.27 s
//	 5K ports => 30.39 s
//	10K ports => 60.5 s
//
// 5000 pg + 10 ACL:
//
//	100 ports => 5.9 s
//	200 ports => 16.2 s
//	 1K ports => 58.5 s
//
// Considering 7 seconds is a safe transaction time, the following approximation may be applied:
// 1000 ports * 500 pg <= 7 seconds extra (weight = 1)
// => 1 port extra weight = 1/(500 * 1000)
func getPGWeight(acls, ports int) float64 {
	portsWeight := float64(ports) / 500000

	if acls < 10 {
		// extra coefficient for a small number of acls
		if acls == 0 {
			// updating a port group without acls may be estimated with a port group with 1 acl update time
			acls = 1
		}
		return portsWeight + float64(acls)/30000
	}
	// for acls > 10, use linear dependency
	return portsWeight + float64(acls)/50000
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
		nbClient:    nbClient,
		getPGWeight: getPGWeight,
	}
}

func getPortGroupNamespaceDbIDs(namespace, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, controllerName, map[libovsdbops.ExternalIDKey]string{
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
			libovsdbops.ObjectNameKey: fmt.Sprintf("%s:%s", policyNamespace, policyName),
		})
}

func getPortGroupAdminNetworkPolicyDbIDs(anpName string, isBanp bool, networkExternalID string) *libovsdbops.DbObjectIDs {
	controllerName := getControllerName(networkExternalID)
	idsType := libovsdbops.PortGroupAdminNetworkPolicy
	if isBanp {
		idsType = libovsdbops.PortGroupBaselineAdminNetworkPolicy
	}
	return libovsdbops.NewDbObjectIDs(idsType, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: anpName,
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
func (syncer *PortGroupSyncer) getReferencingObjsAndNewDbIDs(oldHash, oldName, networkExternalID string, referencingACLUUIDs []string) (acls []*nbdb.ACL,
	dbIDs *libovsdbops.DbObjectIDs, err error) {
	// get all referencing objects
	refACLs := []*nbdb.ACL{}
	for _, aclUUID := range referencingACLUUIDs {
		refACLs = append(refACLs, &nbdb.ACL{UUID: aclUUID})
	}
	acls, err = libovsdbops.FindACLs(syncer.nbClient, refACLs)
	if err != nil {
		err = fmt.Errorf("failed to find acls for port group %s: %v", oldHash, err)
		return
	}
	// build dbIDs
	switch {
	// Filter port groups with pre-defined names
	case oldName == types.ClusterPortGroupNameBase || oldName == types.ClusterRtrPortGroupNameBase:
		dbIDs = getPortGroupClusterDbIDs(oldName, networkExternalID)
	case strings.HasPrefix(oldName, "ANP:"):
		// ANP owned port group
		dbIDs = getPortGroupAdminNetworkPolicyDbIDs(strings.TrimPrefix(oldName, "ANP:"), false, networkExternalID)
	case strings.HasPrefix(oldName, "BANP:"):
		// ANP owned port group
		dbIDs = getPortGroupAdminNetworkPolicyDbIDs(strings.TrimPrefix(oldName, "BANP:"), true, networkExternalID)
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
				direction = policyDirectionIngress
			} else {
				direction = policyDirectionEgress
			}
			dbIDs = getPortGroupNetpolNamespaceDbIDs(namespace, direction, networkExternalID)
		} else {
			// s[0]=policyNamespace, s[1]=policyName
			dbIDs = getPortGroupNetworkPolicyDbIDs(s[0], s[1], networkExternalID)
		}
	default:
		// namespace port group name is just namespace
		dbIDs = getPortGroupNamespaceDbIDs(oldName, networkExternalID)
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
func (syncer *PortGroupSyncer) getPortGroupUpdateInfo(pg *nbdb.PortGroup, pgToACLs map[string][]string) (*updatePortGroupInfo, error) {
	pgName := pg.ExternalIDs["name"]
	networkExternalID := pg.ExternalIDs[types.NetworkExternalID]
	if pgName == "" {
		return nil, fmt.Errorf("port group doesn't have expected ExternalID[\"name\"]")
	}

	acls, dbIDs, err := syncer.getReferencingObjsAndNewDbIDs(pg.Name, pgName, networkExternalID, pgToACLs[pg.Name])
	if err != nil {
		return nil, fmt.Errorf("failed to get new dbIDs for port group %s: %v", pg.ExternalIDs["name"], err)
	}

	// since we need to update portGroup.Name, which is an index and not listed in getAllUpdatableFields,
	// we copy existing portGroup, update the required fields, and replace (delete and create) existing port group with the updated
	newPG := pg.DeepCopy()
	// reset UUID
	newPG.UUID = ""
	// update port group Name and ExternalIDs exactly the same way as in libovsdbops.BuildPortGroup
	newPG.Name = libovsdbutil.GetPortGroupName(dbIDs)
	newPG.ExternalIDs = dbIDs.GetExternalIDs()
	return &updatePortGroupInfo{acls, pg, newPG}, nil
}

func getPGNamesFromMatch(match string) []string {
	pgs := []string{}
	pgreg := regexp.MustCompile("@([a-zA-Z_.][a-zA-Z_.0-9]*)")
	for res := pgreg.FindStringIndex(match); res != nil; res = pgreg.FindStringIndex(match) {
		pgName := match[res[0]+1 : res[1]]
		pgs = append(pgs, pgName)
		match = match[res[1]:]
	}
	return pgs
}

func (syncer *PortGroupSyncer) getReferencingACLs() (map[string][]string, error) {
	pgToACLUUIDs := map[string][]string{}
	_, err := libovsdbops.FindACLsWithPredicate(syncer.nbClient, func(acl *nbdb.ACL) bool {
		aclPGs := getPGNamesFromMatch(acl.Match)
		for _, pgName := range aclPGs {
			pgToACLUUIDs[pgName] = append(pgToACLUUIDs[pgName], acl.UUID)
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	return pgToACLUUIDs, nil
}

// SyncPortGroups must be run after ACLs sync, since it uses new ACL.ExternalIDs
func (syncer *PortGroupSyncer) SyncPortGroups() error {
	// stale port groups don't have controller ID
	portGroupList, err := libovsdbops.FindPortGroupsWithPredicate(syncer.nbClient, libovsdbops.GetNoOwnerPredicate[*nbdb.PortGroup]())
	if err != nil {
		return fmt.Errorf("failed to find stale port groups: %v", err)
	}

	pgToACLs, err := syncer.getReferencingACLs()
	if err != nil {
		return fmt.Errorf("failed to find referencing acls: %v", err)
	}

	klog.Infof("SyncPortGroups found %d stale port groups", len(portGroupList))

	opsWeight := 0.0
	pgUpdateInfos := []*updatePortGroupInfo{}
	i := 0

	transact := func() error {
		ops, err := syncer.getUpdatePortGroupOps(pgUpdateInfos)
		if err != nil {
			return fmt.Errorf("failed to get update port groups ops: %w", err)
		}
		_, err = libovsdbops.TransactAndCheck(syncer.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to transact port group sync ops: %v", err)
		}
		opsWeight = 0.0
		pgUpdateInfos = []*updatePortGroupInfo{}
		return nil
	}

	for i < len(portGroupList) {
		pgUpdateWeight := syncer.getPGWeight(len(portGroupList[i].ACLs), len(portGroupList[i].Ports))
		// This port group would overcome the maximum operation weight of 1
		// so transact all the previously accumulated ops first if any.
		if opsWeight+pgUpdateWeight > 1 {
			// time to transact
			if err = transact(); err != nil {
				return err
			}
		}

		// since the same acl may be affected by multiple port group updates, it is important to call
		// getPortGroupUpdateInfo, which captures acls, after transact.
		// Otherwise, transact may change acl that was captured by getPortGroupUpdateInfo and the following
		// update will override the changes.
		updateInfo, err := syncer.getPortGroupUpdateInfo(portGroupList[i], pgToACLs)
		if err != nil {
			return err
		}

		opsWeight += pgUpdateWeight
		pgUpdateInfos = append(pgUpdateInfos, updateInfo)
		i++
	}
	if len(pgUpdateInfos) > 0 {
		// end of iteration, transact what is left
		if err = transact(); err != nil {
			return err
		}
	}
	return nil
}
