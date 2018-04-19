package cluster

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"

	kapi "k8s.io/api/core/v1"
)

const (
	windowsOS = "windows"
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
		cluster.ClusterIPNet.String(),
		cluster.ClusterServicesSubnet)
	if err != nil {
		return err
	}

	if cluster.GatewayInit {
		if runtime.GOOS == windowsOS {
			panic("Windows is not supported as a gateway node")
		}
		err = cluster.initGateway(node.Name, cluster.ClusterIPNet.String(),
			subnet.String())
		if err != nil {
			return err
		}
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	err = os.MkdirAll(config.CNI.ConfDir, os.ModeDir)
	if err != nil {
		return err
	}

	// Always create the CNI config for consistency.
	cniConf := config.CNI.ConfDir + "/10-ovn-kubernetes.conf"

	var f *os.File
	f, err = os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	confJSON := fmt.Sprintf("{\"name\":\"ovn-kubernetes\", \"type\":\"%s\"}", config.CNI.Plugin)
	_, err = f.Write([]byte(confJSON))

	return err
}
