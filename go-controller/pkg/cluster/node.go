package cluster

import (
	"net"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	windowsOS = "windows"
)

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (cluster *OvnClusterController) StartClusterNode(name string) error {
	count := 300
	var err error
	var node *kapi.Node
	var subnet *net.IPNet
	var clusterSubnets []string

	for _, clusterSubnet := range cluster.ClusterIPNet {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	for count > 0 {
		if count != 300 {
			time.Sleep(time.Second)
		}
		count--

		// setup the node, create the logical switch
		node, err = cluster.Kube.GetNode(name)
		if err != nil {
			logrus.Errorf("Error starting node %s, no node found - %v", name, err)
			continue
		}

		sub, ok := node.Annotations[OvnHostSubnet]
		if !ok {
			logrus.Errorf("Error starting node %s, no annotation found on node for subnet - %v", name, err)
			continue
		}
		_, subnet, err = net.ParseCIDR(sub)
		if err != nil {
			logrus.Errorf("Invalid hostsubnet found for node %s - %v", node.Name, err)
			return err
		}
		break
	}

	if count == 0 {
		logrus.Errorf("Failed to get node/node-annotation for %s - %v", name, err)
		return err
	}

	logrus.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	err = setupOVNNode(name, config.Kubernetes.APIServer, config.Kubernetes.Token,
		config.Kubernetes.CACert)
	if err != nil {
		return err
	}

	err = ovn.CreateManagementPort(node.Name, subnet.String(),
		cluster.ClusterServicesSubnet,
		clusterSubnets)
	if err != nil {
		return err
	}

	if cluster.GatewayInit {
		if runtime.GOOS == windowsOS {
			panic("Windows is not supported as a gateway node")
		}
		err = cluster.initGateway(node.Name, clusterSubnets,
			subnet.String())
		if err != nil {
			return err
		}
	}

	if err = config.WriteCNIConfig(); err != nil {
		return err
	}

	if cluster.OvnHA {
		err = cluster.watchNamespaceUpdate(node, subnet.String())
		return err
	}

	// start the cni server
	cniServer := cni.NewCNIServer("")
	err = cniServer.Start(cni.HandleCNIRequest)

	return err
}

// If default namespace MasterOverlayIP annotation has been chaged, update
// config.OvnNorth and config.OvnSouth auth with new ovn-nb and ovn-remote
// IP address
func (cluster *OvnClusterController) updateOvnNode(masterIP string,
	node *kapi.Node, subnet string) error {
	err := config.UpdateOvnNodeAuth(masterIP)
	if err != nil {
		return err
	}
	err = setupOVNNode(node.Name, config.Kubernetes.APIServer,
		config.Kubernetes.Token, config.Kubernetes.CACert)
	if err != nil {
		logrus.Errorf("Failed to setup OVN node (%v)", err)
		return err
	}

	var clusterSubnets []string

	for _, clusterSubnet := range cluster.ClusterIPNet {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	// Recreate logical switch and management port for this node
	err = ovn.CreateManagementPort(node.Name, subnet,
		cluster.ClusterServicesSubnet,
		clusterSubnets)
	if err != nil {
		return err
	}

	// Reinit Gateway for this node if the --init-gateways flag is set
	if cluster.GatewayInit {
		if runtime.GOOS == windowsOS {
			panic("Windows is not supported as a gateway node")
		}
		err = cluster.initGateway(node.Name, clusterSubnets,
			subnet)
		if err != nil {
			return err
		}
	}

	return nil
}

// watchNamespaceUpdate starts watching namespace resources and calls back
// the update handler logic if there is any namspace update event
func (cluster *OvnClusterController) watchNamespaceUpdate(node *kapi.Node,
	subnet string) error {
	_, err := cluster.watchFactory.AddNamespaceHandler(
		cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, newer interface{}) {
				oldNs := old.(*kapi.Namespace)
				oldMasterIP := oldNs.Annotations[MasterOverlayIP]
				newNs := newer.(*kapi.Namespace)
				newMasterIP := newNs.Annotations[MasterOverlayIP]
				if newMasterIP != oldMasterIP {
					err := cluster.updateOvnNode(newMasterIP, node, subnet)
					if err != nil {
						logrus.Errorf("Failed to update OVN node with new ",
							"masterIP %s: %v", newMasterIP, err)
					}
				}
			},
		}, nil)
	return err
}
