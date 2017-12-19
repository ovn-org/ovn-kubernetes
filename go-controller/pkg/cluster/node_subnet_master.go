package cluster

import (
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"
)

func (cluster *nodeSubnetController) MasterStart(masterNodeName string) error {
	subrange := make([]string, 0)

	existingNodes, err := cluster.Kube.GetNodes()
	if err != nil {
		return fmt.Errorf("error fetching subnets: %v", err)
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			subrange = append(subrange, hostsubnet)
		}
	}

	// Add the masterSwitchNetwork to subrange so that it is counted as one already taken
	masterSwitchNetwork, err := calculateMasterSwitchNetwork(cluster.ClusterIPNet.String(), cluster.PodSubnetLength)
	if err != nil {
		return err
	}
	subrange = append(subrange, masterSwitchNetwork)

	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'podSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	cluster.podSubnetAllocator, err = netutils.NewSubnetAllocator(cluster.ClusterIPNet.String(), cluster.PodSubnetLength, subrange)
	if err != nil {
		return err
	}

	// now go over the 'existing' list again and create annotations for those who do not have it
	for _, node := range existingNodes.Items {
		_, ok := node.Annotations[OvnHostSubnet]
		if !ok {
			err := cluster.addNode(&node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		}
	}

	if err := setupMasterModeNode(masterNodeName, masterSwitchNetwork, cluster.ClusterIPNet, cluster.ClusterServicesSubnet); err != nil {
		return err
	}

	// go routine to watch all node events. On creation, addNode will be called that will create a subnet for the switch belonging to that node.
	// On a delete call, the subnet will be returned to the allocator as the switch is deleted from ovn
	cluster.watchNodes()

	return nil
}

func calculateMasterSwitchNetwork(clusterNetwork string, podSubnetLength uint32) (string, error) {
	subAllocator, err := netutils.NewSubnetAllocator(clusterNetwork, podSubnetLength, make([]string, 0))
	if err != nil {
		return "", err
	}
	sn, err := subAllocator.GetNetwork()
	return sn.String(), err
}

func setupMasterModeNode(masterNodeName string, masterSwitchNetwork string, clusterIPNet *net.IPNet, clusterServicesSubnet string) error {
	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", masterNodeName, "--", "set", "logical_router", masterNodeName, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create 2 load-balancers for east-west traffic.  One handles UDP and another handles TCP.
	k8sClusterLbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
	if err != nil {
		logrus.Errorf("Failed to get tcp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}

	if k8sClusterLbTCP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
		if err != nil {
			logrus.Errorf("Failed to create tcp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	k8sClusterLbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes")
	if err != nil {
		logrus.Errorf("Failed to get udp load-balancer, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sClusterLbUDP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-udp=yes", "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create udp load-balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Create a logical switch called "join" that will be used to connect gateway routers to the distributed router.
	// The "join" will be allocated IP addresses in the range 100.64.1.0/24.
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", "join")
	if err != nil {
		logrus.Errorf("Failed to create logical switch called \"join\", stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the distributed router to "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+masterNodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", masterNodeName, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", masterNodeName, "rtoj-"+masterNodeName, routerMac, "100.64.1.1/24", "--", "set", "logical_router_port", "rtoj-"+masterNodeName, "external_ids:connect_to_join=yes")
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v", masterNodeName, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+masterNodeName, "--", "set", "logical_switch_port", "jtor-"+masterNodeName, "type=router", "options:router-port=rtoj-"+masterNodeName, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical switch port to logical router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	err = ovn.CreateManagementPort(masterNodeName, masterSwitchNetwork,
		clusterIPNet.String(), clusterServicesSubnet)
	if err != nil {
		return fmt.Errorf("Failed create management port: %v", err)
	}

	// Create a lock for gateway-init to co-ordinate.
	stdout, stderr, err = util.RunOVNNbctl("--", "set", "nb_global", ".",
		"external-ids:gateway-lock=\"\"")
	if err != nil {
		logrus.Errorf("Failed to create lock for gateways, "+
			"stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	return nil
}

func (cluster *nodeSubnetController) addNode(node *kapi.Node) error {
	// Do not create a subnet if the node already has a subnet
	hostsubnet, ok := node.Annotations[OvnHostSubnet]
	if ok {
		// double check if the hostsubnet looks valid
		_, _, err := net.ParseCIDR(hostsubnet)
		if err == nil {
			return nil
		}
	}
	// Create new subnet
	sn, err := cluster.podSubnetAllocator.GetNetwork()
	if err != nil {
		return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
	}

	err = cluster.Kube.SetAnnotationOnNode(node, OvnHostSubnet, sn.String())
	if err != nil {
		_ = cluster.podSubnetAllocator.ReleaseNetwork(sn)
		return fmt.Errorf("Error creating subnet %s for node %s: %v", sn.String(), node.Name, err)
	}
	logrus.Infof("Created HostSubnet %s", sn.String())
	return nil
}

func (cluster *nodeSubnetController) deleteNode(node *kapi.Node) error {
	sub, ok := node.Annotations[OvnHostSubnet]
	if !ok {
		return fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}
	err = cluster.podSubnetAllocator.ReleaseNetwork(subnet)
	if err != nil {
		return fmt.Errorf("Error deleting subnet %v for node %q: %v", sub, node.Name, err)
	}

	logrus.Infof("Deleted HostSubnet %s for node %s", sub, node.Name)
	return nil
}

func (cluster *nodeSubnetController) watchNodes() {
	cluster.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			err := cluster.addNode(node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*kapi.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				node, ok = tombstone.Obj.(*kapi.Node)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a node %#v", obj)
					return
				}
			}
			logrus.Debugf("Delete event for Node %q", node.Name)
			err := cluster.deleteNode(node)
			if err != nil {
				logrus.Errorf("Error deleting node %s: %v", node.Name, err)
			}
		},
	}, nil)
}
