package cluster

import (
	"fmt"
	"net"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"

	kapi "k8s.io/api/core/v1"
)

func (cluster *nodeSubnetController) NodeInit(name string) error {
	var err error
	cluster.nodeSubnet, err = waitForNodeSubnet(cluster.Kube, name)
	if err != nil {
		return err
	}
	logrus.Infof("Node %s ready for ovn initialization with subnet %s", name, cluster.nodeSubnet.String())
	return nil
}

func (cluster *nodeSubnetController) NodeStart(name string) error {
	err := ovn.CreateManagementPort(name, cluster.nodeSubnet.String(),
		cluster.ClusterIPNet.String(),
		cluster.ClusterServicesSubnet)
	if err == nil {
		return err
	}

	if cluster.GatewayInit {
		if runtime.GOOS == windowsOS {
			panic("Windows is not supported as a gateway node")
		}
		err = cluster.initGateway(name, cluster.ClusterIPNet.String(),
			cluster.nodeSubnet.String())
		if err != nil {
			return err
		}
	}

	return err
}

func waitForNodeSubnet(kube kube.Interface, name string) (*net.IPNet, error) {
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
		node, err = kube.GetNode(name)
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
			return nil, fmt.Errorf("invalid hostsubnet found for node %s - %v", name, err)
		}
		break
	}

	if count == 0 {
		return nil, fmt.Errorf("failed to get node/node-annotation for %s - %v", name, err)
	}

	return subnet, nil
}
