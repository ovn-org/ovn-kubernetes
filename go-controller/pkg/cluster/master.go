package cluster

import (
	"fmt"
	"net"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/sirupsen/logrus"
)

// RebuildOVNDatabase rebuilds db if HA option is enabled and OVN DB
// doesn't exist. First It updates k8s nodes by creating a logical
// switch for each node. Then it reads all resources from k8s and
// creates needed resources in OVN DB. Last, it updates master node
// ip in default namespace to trigger event on node.
func (cluster *OvnClusterController) RebuildOVNDatabase(
	masterNodeName string, oc *ovn.Controller) error {
	logrus.Debugf("Rebuild OVN database for cluster nodes")
	var err error
	ipChange, err := cluster.checkMasterIPChange(masterNodeName)
	if err != nil {
		logrus.Errorf("Error when checking Master Node IP Change: %v", err)
		return err
	}

	// If OvnHA options is enabled, make sure default namespace has the
	// annotation of current cluster master node's overlay IP address
	logrus.Debugf("cluster.OvnHA: %t", cluster.OvnHA)
	if cluster.OvnHA && ipChange {
		logrus.Debugf("HA is enabled and DB doesn't exist!")
		// Rebuild OVN DB for the k8s nodes
		err = cluster.UpdateDBForKubeNodes(masterNodeName)
		if err != nil {
			return err
		}
		// Rebuild OVN DB for the k8s namespaces and all the resource
		// objects inside the namespace including pods and network
		// policies
		err = cluster.UpdateKubeNsObjects(oc)
		if err != nil {
			return err
		}
		// Update master node IP annotation on default namespace
		err = cluster.UpdateMasterNodeIP(masterNodeName)
		if err != nil {
			return err
		}
	}

	return nil
}

// UpdateDBForKubeNodes rebuilds ovn db for k8s nodes
func (cluster *OvnClusterController) UpdateDBForKubeNodes(
	masterNodeName string) error {
	nodes, err := cluster.Kube.GetNodes()
	if err != nil {
		logrus.Errorf("Failed to obtain k8s nodes: %v", err)
		return err
	}
	for _, node := range nodes.Items {
		subnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			// Create a logical switch for the node
			logrus.Debugf("ovn_host_subnet: %s", subnet)
			ip, localNet, err := net.ParseCIDR(subnet)
			if err != nil {
				return fmt.Errorf("Failed to parse subnet %v: %v", subnet,
					err)
			}
			ip = util.NextIP(ip)
			n, _ := localNet.Mask.Size()
			routerIPMask := fmt.Sprintf("%s/%d", ip.String(), n)
			stdout, stderr, err := util.RunOVNNbctl("--may-exist",
				"ls-add", node.Name, "--", "set", "logical_switch",
				node.Name, fmt.Sprintf("other-config:subnet=%s", subnet),
				fmt.Sprintf("external-ids:gateway_ip=%s", routerIPMask))
			if err != nil {
				logrus.Errorf("Failed to create logical switch for "+
					"node %s, stdout: %q, stderr: %q, error: %v",
					node.Name, stdout, stderr, err)
				return err
			}
		}
	}
	return nil
}

// UpdateKubeNsObjects rebuilds ovn db for k8s namespaces
// and pods/networkpolicies inside the namespaces.
func (cluster *OvnClusterController) UpdateKubeNsObjects(
	oc *ovn.Controller) error {
	namespaces, err := cluster.Kube.GetNamespaces()
	if err != nil {
		logrus.Errorf("Failed to get k8s namespaces: %v", err)
		return err
	}
	for _, ns := range namespaces.Items {
		oc.AddNamespace(&ns)
		pods, err := cluster.Kube.GetPods(ns.Name)
		if err != nil {
			logrus.Errorf("Failed to get k8s pods: %v", err)
			return err
		}
		for _, pod := range pods.Items {
			oc.AddLogicalPortWithIP(&pod)
		}
		endpoints, err := cluster.Kube.GetEndpoints(ns.Name)
		if err != nil {
			logrus.Errorf("Failed to get k8s endpoints: %v", err)
			return err
		}
		for _, ep := range endpoints.Items {
			er := oc.AddEndpoints(&ep)
			if er != nil {
				logrus.Errorf("Error adding endpoints: %v", er)
				return er
			}
		}
		policies, err := cluster.Kube.GetNetworkPolicies(ns.Name)
		if err != nil {
			logrus.Errorf("Failed to get k8s network policies: %v", err)
			return err
		}
		for _, policy := range policies.Items {
			oc.AddNetworkPolicy(&policy)
		}
	}
	return nil
}

// UpdateMasterNodeIP add annotations of master node IP on
// default namespace
func (cluster *OvnClusterController) UpdateMasterNodeIP(
	masterNodeName string) error {
	masterNodeIP, err := netutils.GetNodeIP(masterNodeName)
	if err != nil {
		return fmt.Errorf("Failed to obtain local IP from master node "+
			"%q: %v", masterNodeName, err)
	}

	defaultNs, err := cluster.Kube.GetNamespace(DefaultNamespace)
	if err != nil {
		return fmt.Errorf("Failed to get default namespace: %v", err)
	}

	// Get overlay ip on OVN master node from default namespace. If it
	// doesn't have it or the IP address is different than the current one,
	// set it with current master overlay IP.
	masterIP, ok := defaultNs.Annotations[MasterOverlayIP]
	if !ok || masterIP != masterNodeIP {
		err := cluster.Kube.SetAnnotationOnNamespace(defaultNs, MasterOverlayIP,
			masterNodeIP)
		if err != nil {
			return fmt.Errorf("Failed to set %s=%s on namespace %s: %v",
				MasterOverlayIP, masterNodeIP, defaultNs.Name, err)
		}
	}

	return nil
}

func (cluster *OvnClusterController) checkMasterIPChange(
	masterNodeName string) (bool, error) {
	// Check DB existence by checking if the default namespace annotated
	// IP address is the same as the master node IP. If annotated IP
	// address is different, we assume that the ovn db is crashed on the
	// old node and needs to be rebuilt on the new master node.
	masterNodeIP, err := netutils.GetNodeIP(masterNodeName)
	if err != nil {
		return false, fmt.Errorf("Failed to obtain local IP from master "+
			"node %q: %v", masterNodeName, err)
	}

	defaultNs, err := cluster.Kube.GetNamespace(DefaultNamespace)
	if err != nil {
		return false, fmt.Errorf("Failed to get default namespace: %v", err)
	}

	// Get overlay ip on OVN master node from default namespace. If the IP
	// address is different than master node IP, return true. Else, return
	// false.
	masterIP := defaultNs.Annotations[MasterOverlayIP]
	logrus.Debugf("Master IP: %s, Annotated IP: %s", masterNodeIP, masterIP)
	if masterIP != masterNodeIP {
		logrus.Debugf("Detected Master node IP is different than default " +
			"namespae annotated IP.")
		return true, nil
	}
	return false, nil
}

// StartClusterMaster runs a subnet IPAM and a controller that watches arrival/departure
// of nodes in the cluster
// On an addition to the cluster (node create), a new subnet is created for it that will translate
// to creation of a logical switch (done by the node, but could be created here at the master process too)
// Upon deletion of a node, the switch will be deleted
//
// TODO: Verify that the cluster was not already called with a different global subnet
//  If true, then either quit or perform a complete reconfiguration of the cluster (recreate switches/routers with new subnet values)
func (cluster *OvnClusterController) StartClusterMaster(masterNodeName string) error {

	alreadyAllocated := make([]string, 0)
	existingNodes, err := cluster.Kube.GetNodes()
	if err != nil {
		logrus.Errorf("Error in initializing/fetching subnets: %v", err)
		return err
	}
	for _, node := range existingNodes.Items {
		hostsubnet, ok := node.Annotations[OvnHostSubnet]
		if ok {
			alreadyAllocated = append(alreadyAllocated, hostsubnet)
		}
	}
	masterSubnetAllocatorList := make([]*netutils.SubnetAllocator, 0)
	// NewSubnetAllocator is a subnet IPAM, which takes a CIDR (first argument)
	// and gives out subnets of length 'hostSubnetLength' (second argument)
	// but omitting any that exist in 'subrange' (third argument)
	for _, clusterEntry := range cluster.ClusterIPNet {
		subrange := make([]string, 0)
		for _, entry := range alreadyAllocated {
			if clusterEntry.CIDR.Contains(net.ParseIP(entry)) {
				subrange = append(subrange, entry)
			}
		}
		subnetAllocator, err := netutils.NewSubnetAllocator(clusterEntry.CIDR.String(), 32-clusterEntry.HostSubnetLength, subrange)
		if err != nil {
			return err
		}
		masterSubnetAllocatorList = append(masterSubnetAllocatorList, subnetAllocator)
	}
	cluster.masterSubnetAllocatorList = masterSubnetAllocatorList

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

	if err := cluster.SetupMaster(masterNodeName); err != nil {
		logrus.Errorf("Failed to setup master (%v)", err)
		return err
	}

	// Watch all node events.  On creation, addNode will be called that will
	// create a subnet for the switch belonging to that node. On a delete
	// call, the subnet will be returned to the allocator as the switch is
	// deleted from ovn
	return cluster.watchNodes()
}

// SetupMaster creates the central router and load-balancers for the network
func (cluster *OvnClusterController) SetupMaster(masterNodeName string) error {
	if err := setupOVNMaster(masterNodeName); err != nil {
		return err
	}

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
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes", "protocol=tcp")
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

func (cluster *OvnClusterController) addNode(node *kapi.Node) error {
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
	for _, possibleSubnet := range cluster.masterSubnetAllocatorList {
		sn, err := possibleSubnet.GetNetwork()
		if err == netutils.ErrSubnetAllocatorFull {
			// Current subnet exhausted, check next possible subnet
			continue
		} else if err != nil {
			return fmt.Errorf("Error allocating network for node %s: %v", node.Name, err)
		} else {
			err = cluster.Kube.SetAnnotationOnNode(node, OvnHostSubnet, sn.String())
			if err != nil {
				_ = possibleSubnet.ReleaseNetwork(sn)
				return fmt.Errorf("Error creating subnet %s for node %s: %v", sn.String(), node.Name, err)
			}
			logrus.Infof("Created HostSubnet %s", sn.String())
			return nil
		}
	}
	return fmt.Errorf("error allocating netork for node %s: No more allocatable ranges", node.Name)
}

func (cluster *OvnClusterController) deleteNode(node *kapi.Node) error {
	sub, ok := node.Annotations[OvnHostSubnet]
	if !ok {
		return fmt.Errorf("Error in obtaining host subnet for node %q for deletion", node.Name)
	}

	_, subnet, err := net.ParseCIDR(sub)
	if err != nil {
		return fmt.Errorf("Error in parsing hostsubnet - %v", err)
	}
	for _, possibleSubnet := range cluster.masterSubnetAllocatorList {
		err = possibleSubnet.ReleaseNetwork(subnet)
		if err == nil {
			logrus.Infof("Deleted HostSubnet %s for node %s", sub, node.Name)
			return nil
		}
	}
	// SubnetAllocator.network is an unexported field so the only way to figure out if a subnet is in a network is to try and delete it
	// if deletion succeeds then stop iterating, if the list is exhausted the node subnet wasn't deleteted return err
	return fmt.Errorf("Error deleting subnet %v for node %q: subnet not found in any CIDR range or already available", sub, node.Name)

}

func (cluster *OvnClusterController) watchNodes() error {
	_, err := cluster.watchFactory.AddNodeHandler(cache.ResourceEventHandlerFuncs{
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
			node := obj.(*kapi.Node)
			logrus.Debugf("Delete event for Node %q", node.Name)
			err := cluster.deleteNode(node)
			if err != nil {
				logrus.Errorf("Error deleting node %s: %v", node.Name, err)
			}
			err = util.RemoveNode(node.Name)
			if err != nil {
				logrus.Errorf("Failed to remove node %s (%v)", node.Name, err)
			}
		},
	}, nil)
	return err
}
