package acl

import (
	"fmt"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/pkg/errors"

	"k8s.io/klog/v2"
)

// RemoveACLFromNodeSwitches removes the ACL uuid entry from Logical Switch acl's list.
func RemoveACLFromNodeSwitches(switches []string, aclUUID string) error {
	if len(switches) == 0 {
		return nil
	}
	args := []string{}
	for _, ls := range switches {
		args = append(args, "--", "--if-exists", "remove", "logical_switch", ls, "acl", aclUUID)
	}
	_, _, err := util.RunOVNNbctl(args...)
	if err != nil {
		return errors.Wrapf(err, "Error while removing ACL: %s, from switches", aclUUID)
	}
	klog.Infof("ACL: %s, removed from switches: %s", aclUUID, switches)
	return nil
}

func PurgeRejectRules(nbClient libovsdbclient.Client) error {
	// TODO
	// - Removing blindly reject ACLs might be problematic if in the future we add this types of ACLs for other purposes
	// - Do ACLs need to be removed or only references to them, which is what we seem to be doing elsewhere?
	acls := []nbdb.ACL{}
	err := nbClient.WhereCache(func(acl *nbdb.ACL) bool {
		return acl.Action == nbdb.ACLActionReject
	}).List(&acls)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return errors.Wrapf(err, "Error while querying ACLs with reject action: %v", err)
	}

	if len(acls) == 0 {
		klog.Info("No reject ACLs to remove")
		return nil
	}

	aclPtrs := []*nbdb.ACL{}
	for _, acl := range acls {
		aclPtrs = append(aclPtrs, &acl)
		data, stderr, err := util.RunOVNNbctl("--format=csv", "--data=bare", "--no-headings", "--columns=_uuid", "find", "logical_switch", fmt.Sprintf("acls{>=}%s", acl.UUID))
		if err != nil {
			return errors.Wrapf(err, "Error while querying ACLs uuid:%s with reject action: %s", acl.UUID, stderr)
		}
		ls := strings.Split(data, "\n")
		err = RemoveACLFromNodeSwitches(ls, acl.UUID)
		if err != nil {
			return errors.Wrapf(err, "Failed to remove reject acl from logical switches")
		}
	}

	err = libovsdbops.DeleteACLsFromPortGroup(nbClient, types.ClusterPortGroupName, aclPtrs...)
	if err != nil {
		klog.Errorf("Error trying to remove ACLs %+v from port group %s: %v", acls, types.ClusterPortGroupName, err)
	}

	return nil
}
