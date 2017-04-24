package cluster

import (
	"net"
	"os/exec"
	"time"

	"github.com/golang/glog"
	kapi "k8s.io/client-go/pkg/api/v1"

	"github.com/openshift/origin/pkg/util/netutils"
)

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (cluster *OvnClusterController) StartClusterNode(name string) error {
	count := 30
	var err error
	var node *kapi.Node
	var subnet *net.IPNet

	for count > 0 {
		if count != 30 {
			time.Sleep(time.Second)
		}
		count--

		// setup the node, create the logical switch
		node, err = cluster.Kube.GetNode(name)
		if err != nil {
			glog.Errorf("Error starting node %s, no node found - %v", name, err)
			continue
		}

		sub, ok := node.Annotations[OvnHostSubnet]
		if !ok {
			glog.Errorf("Error starting node %s, no annotation found on node for subnet - %v", name, err)
			continue
		}
		_, subnet, err = net.ParseCIDR(sub)
		if err != nil {
			glog.Errorf("Invalid hostsubnet found for node %s - %v", node.Name, err)
			return err
		}
		break
	}

	if count == 0 {
		glog.Errorf("Failed to get node/node-annotation for %s - %v", name, err)
		return err
	}

	nodeIP, err := netutils.GetNodeIP(node.Name)
	if err != nil {
		glog.Errorf("Failed to obtain node's IP: %v", err)
		return err
	}

	glog.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	out, err := exec.Command("ovnkube-setup-node", cluster.Token, nodeIP, cluster.KubeServer, subnet.String(), cluster.ClusterIPNet.String(), name).CombinedOutput()
	if err != nil {
		glog.Errorf("Error in setting up node - %s (%v)", string(out), err)
	}

	return err
}
