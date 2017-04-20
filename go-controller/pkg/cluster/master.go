package cluster

import (
	"fmt"
	"net"
	"os/exec"

	"github.com/golang/glog"

	utilwait "k8s.io/apimachinery/pkg/util/wait"
	kapi "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/origin/pkg/util/netutils"
)

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (cluster *OvnClusterController) StartClusterMaster(masterNodeName string) error {
	clusterNetwork := cluster.ClusterIPNet
	hostSubnetLength := cluster.HostSubnetLength

	subrange := make([]string, 0)
	existingNodes, err := cluster.Kube.GetNodes()
	if err != nil {
		glog.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OVN_HOST_SUBNET]
		if ok {
			subrange = append(subrange, hostsubnet)
		}
	}
	masterSwitchNetwork, err := calculateMasterSwitchNetwork(clusterNetwork.String(), hostSubnetLength)
	if err != nil {
		return err
	}
	// Add the masterSwitchNetwork to subrange so that it is counted as one already taken
	subrange = append(subrange, masterSwitchNetwork)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	cluster.masterSubnetAllocator, err = netutils.NewSubnetAllocator(clusterNetwork.String(), hostSubnetLength, subrange)
	if err != nil {
		return err
	}

	// now go over the 'existing' list again and create annotations for those who do not have it
	for _, node := range existingNodes.Items {
		_, ok := node.Annotations[OVN_HOST_SUBNET]
		if !ok {
			err := cluster.addNode(&node)
			if err != nil {
				glog.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		}
	}

	cluster.SetupMaster(masterNodeName, masterSwitchNetwork)

	// go routine to watch all node events. On creation, addNode will be called that will create a subnet for the switch belonging to that node.
	// On a delete call, the subnet will be returned to the allocator as the switch is deleted from ovn
	go utilwait.Forever(cluster.watchNodes, 0)
	return nil
}

func calculateMasterSwitchNetwork(clusterNetwork string, hostSubnetLength uint32) (string, error) {
	subAllocator, err := netutils.NewSubnetAllocator(clusterNetwork, hostSubnetLength, make([]string, 0))
	sn, err := subAllocator.GetNetwork()
	return sn.String(), err
}

func (cluster *OvnClusterController) SetupMaster(masterNodeName string, masterSwitchNetwork string) {
	out, err := exec.Command("ovnkube-setup-master", cluster.Token, cluster.KubeServer, masterSwitchNetwork, cluster.ClusterIPNet.String(), masterNodeName).CombinedOutput()
	if err != nil {
		glog.Errorf("Error setting up master node - %v(%v)", string(out), err)
	}
}

func (cluster *OvnClusterController) addNode(node *kapi.Node) error {
	// Create new subnet
	sn, err := cluster.masterSubnetAllocator.GetNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}

	err = cluster.Kube.SetAnnotationOnNode(node, OVN_HOST_SUBNET, sn.String())
	if err != nil {
		cluster.masterSubnetAllocator.ReleaseNetwork(sn)
		return fmt.Errorf("Error creating subnet %s for node %s: %v", sn.String(), node.Name, err)
	}
	glog.Infof("Created HostSubnet %s", sn.String())
	return nil
}

func (cluster *OvnClusterController) deleteNode(node *kapi.Node) error {
	sub, ok := node.Annotations[OVN_HOST_SUBNET]
	if !ok {
		return fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}
	err = cluster.masterSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %v", sub, node.Name, err)
	}

	glog.Infof("Deleted HostSubnet %s for node %s", sub, node.Name)
	return nil
}

func (cluster *OvnClusterController) watchNodes() {
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			glog.V(5).Infof("Added event for Node %q", node.Name)
			err := cluster.addNode(node)
			if err != nil {
				glog.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
			return
		},
		UpdateFunc: func(old, new interface{}) { return },
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*kapi.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				node, ok = tombstone.Obj.(*kapi.Node)
				if !ok {
					glog.Errorf("tombstone contained object that is not a node %#v", obj)
					return
				}
			}
			glog.V(5).Infof("Delete event for Node %q", node.Name)
			err := cluster.deleteNode(node)
			if err != nil {
				glog.Errorf("Error deleting node %s: %v", node.Name, err)
			}
			return
		},
	}
	cluster.StartNodeWatch(handler)
}
