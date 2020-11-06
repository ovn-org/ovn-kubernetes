package acl

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
)

// GetACLByName returns the ACL UUID
func GetACLByName(aclName string) (string, error) {
	aclUUID, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "acl",
		fmt.Sprintf("name=%s", aclName))
	if err != nil {
		return "", errors.Wrapf(err, "Error while querying ACLs by name: %s", stderr)
	}
	return aclUUID, nil
}

// RemoveACLFromNodeSwitches removes the ACL uuid entry from Logical Switch acl's list.
func RemoveACLFromNodeSwitches(switches []string, aclUUID string) error {
	if len(switches) > 0 {
		args := []string{}
		for _, ls := range switches {
			args = append(args, "--", "--if-exists", "remove", "logical_switch", ls, "acl", aclUUID)
		}
		_, _, err := util.RunOVNNbctl(args...)
		if err != nil {
			return errors.Wrapf(err, "Error while removing ACL: %s, from switches", aclUUID)
		} else {
			klog.Infof("ACL: %s, removed from switches: %s", aclUUID, switches)
		}
	}
	return nil
}

// RemoveACLFromPortGroup removes the ACL from the port-group
func RemoveACLFromPortGroup(aclUUID, clusterPortGroupUUID string) error {
	_, stderr, err := util.RunOVNNbctl("--", "--if-exists", "remove", "port_group", clusterPortGroupUUID, "acls", aclUUID)
	if err != nil {
		return errors.Wrapf(err, "Failed to remove reject ACL %s from port group %s: stderr: %q", aclUUID, clusterPortGroupUUID, stderr)
	}
	klog.Infof("ACL: %s, removed from the port group : %s", aclUUID, clusterPortGroupUUID)
	return nil
}
