package acl

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
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

// GetRejectACLs returns a map with the ACLs with a reject action
// the map uses the name of the ACL as key and the uuid as value
func GetRejectACLs() (map[string]string, error) {
	// Get OVN's current reject ACLs. Note, currently only services use reject ACLs.
	result := make(map[string]string)
	type ovnACLData struct {
		Data [][]interface{}
	}
	data, stderr, err := util.RunOVNNbctl("--columns=name,_uuid", "--format=json", "find", "acl", "action=reject")
	if err != nil {
		return result, errors.Wrapf(err, "Error while querying ACLs with reject action: %s", stderr)
	}
	// Process the output
	x := ovnACLData{}
	if err := json.Unmarshal([]byte(data), &x); err != nil {
		return result, errors.Wrapf(err, "Unable to get current OVN reject ACLs. Unable to sync reject ACLs")
	}
	for _, entry := range x.Data {
		// ACL entry format is a slice: [<aclName>, ["_uuid", <uuid>]]
		if len(entry) != 2 {
			continue
		}
		name, ok := entry[0].(string)
		if !ok {
			continue
		}
		uuidData, ok := entry[1].([]interface{})
		if !ok || len(uuidData) != 2 {
			continue
		}
		uuid, ok := uuidData[1].(string)
		if !ok {
			continue
		}
		result[name] = uuid
	}
	return result, nil
}

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

// RemoveACLFromPortGroup removes the ACL from the port-group
func RemoveACLFromPortGroup(aclUUID, clusterPortGroupUUID string) error {
	_, stderr, err := util.RunOVNNbctl("--", "--if-exists", "remove", "port_group", clusterPortGroupUUID, "acls", aclUUID)
	if err != nil {
		return errors.Wrapf(err, "Failed to remove reject ACL %s from port group %s: stderr: %q", aclUUID, clusterPortGroupUUID, stderr)
	}
	klog.Infof("ACL: %s, removed from the port group : %s", aclUUID, clusterPortGroupUUID)
	return nil
}

// AddRejectACLToPortGroup adds a reject ACL to a PortGroup
func AddRejectACLToPortGroup(clusterPortGroupUUID, aclName, sourceIP string, sourcePort int, proto v1.Protocol) (string, error) {
	l3Prefix := "ip4"
	if utilnet.IsIPv6String(sourceIP) {
		l3Prefix = "ip6"
	}

	aclMatch := fmt.Sprintf("match=\"%s.dst==%s && %s && %s.dst==%d\"", l3Prefix, sourceIP,
		strings.ToLower(string(proto)), strings.ToLower(string(proto)), sourcePort)
	cmd := []string{"--id=@reject-acl", "create", "acl", "direction=from-lport", "priority=1000", aclMatch, "action=reject",
		fmt.Sprintf("name=%s", aclName), "--", "add", "port_group", clusterPortGroupUUID, "acls", "@reject-acl"}
	aclUUID, stderr, err := util.RunOVNNbctl(cmd...)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to add ACL: %s, %q, to cluster port group %s, stderr: %q", aclUUID, aclName, clusterPortGroupUUID, stderr)
	}
	return aclUUID, nil
}

// AddRejectACLToLogicalSwitch adds a reject ACL to a logical switch
func AddRejectACLToLogicalSwitch(logicalSwitch, aclName, sourceIP string, sourcePort int, proto v1.Protocol) (string, error) {
	l3Prefix := "ip4"
	if utilnet.IsIPv6String(sourceIP) {
		l3Prefix = "ip6"
	}

	aclMatch := fmt.Sprintf("match=\"%s.dst==%s && %s && %s.dst==%d\"", l3Prefix, sourceIP,
		strings.ToLower(string(proto)), strings.ToLower(string(proto)), sourcePort)
	cmd := []string{"--id=@reject-acl", "create", "acl", "direction=from-lport", "priority=1000", aclMatch, "action=reject",
		fmt.Sprintf("name=%s", aclName), "--", "add", "logical_switch", logicalSwitch, "acls", "@reject-acl"}

	aclUUID, stderr, err := util.RunOVNNbctl(cmd...)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to add ACL: %s, %q, to cluster switch %s, stderr: %q", aclUUID, aclName, logicalSwitch, stderr)
	}
	return aclUUID, nil
}
