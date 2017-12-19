package cluster

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

// StartMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func StartMaster(controller OvnClusterController, masterNodeName string) error {
	err := util.StartOVS()
	if err != nil {
		return err
	}

	err = util.StartOvnNorthd()
	if err != nil {
		return err
	}

	if err := setupOVNMaster(masterNodeName); err != nil {
		return err
	}

	return controller.MasterStart(masterNodeName)
}
