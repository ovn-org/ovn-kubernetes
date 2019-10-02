package cluster

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

func isOVNControllerReady(name string) (bool, error) {
	const runDir string = "/var/run/openvswitch/"

	pid, err := ioutil.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		logrus.Errorf("unknown pid for ovn-controller process: %v", err)
		return false, err
	}

	if err := wait.PollImmediate(500*time.Millisecond,
		300*time.Second,
		func() (bool, error) {
			ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl",
				strings.TrimSuffix(string(pid), "\n"))
			ret, _, err := util.RunOVSAppctl("-t", ctlFile,
				"connection-status")
			if err == nil {
				logrus.Infof("node %s connection status = %s",
					name, ret)
				return ret == "connected", nil
			}
			return false, err
		}); err != nil {
		logrus.Errorf("timed out waiting sbdb for node %s: %v", name, err)
		return false, err
	}

	if err := wait.PollImmediate(500*time.Millisecond,
		300*time.Second,
		func() (bool, error) {
			flows, _, err := util.RunOVSOfctl("dump-flows", "br-int")
			if err == nil {
				return len(flows) > 0, nil
			}
			return false, err
		}); err != nil {
		logrus.Errorf("timed out dumping br-int flow entries for node %s: %v",
			name, err)
		return false, err
	}
	return true, nil
}

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (cluster *OvnClusterController) StartClusterNode(name string) error {
	var err error
	var node *kapi.Node
	var subnet *net.IPNet
	var clusterSubnets []string
	var cidr string
	var connected bool

	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	if err := wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		node, err = cluster.Kube.GetNode(name)
		if err != nil {
			logrus.Errorf("Error starting node %s, no node found - %v", name, err)
			return false, nil
		}
		if cidr, _, err = util.RunOVNNbctl("get", "logical_switch", node.Name, "other-config:subnet"); err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		logrus.Errorf("timed out waiting for node %q logical switch: %v", name, err)
		return err
	}

	_, subnet, err = net.ParseCIDR(cidr)
	if err != nil {
		logrus.Errorf("Invalid hostsubnet found for node %s - %v", node.Name, err)
		return err
	}

	logrus.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	err = cluster.watchConfigEndpoints()
	if err != nil {
		return err
	}

	err = setupOVNNode(name)
	if err != nil {
		return err
	}

	connected, err = isOVNControllerReady(name)
	if err != nil {
		return err
	}
	if !connected {
		return nil
	}

	err = ovn.CreateManagementPort(node.Name, subnet.String(), clusterSubnets)
	if err != nil {
		return err
	}

	if config.Gateway.Mode != config.GatewayModeDisabled {
		err = cluster.initGateway(node.Name, clusterSubnets, subnet.String())
		if err != nil {
			return err
		}
	}

	confFile := filepath.Join(config.CNI.ConfDir, config.CNIConfFileName)
	_, err = os.Stat(confFile)
	if os.IsNotExist(err) {
		err = config.WriteCNIConfig(config.CNI.ConfDir, config.CNIConfFileName)
		if err != nil {
			return err
		}
	}

	// start the cni server
	cniServer := cni.NewCNIServer("")
	err = cniServer.Start(cni.HandleCNIRequest)

	return err
}

func validateOVNConfigEndpoint(ep *kapi.Endpoints) bool {
	if len(ep.Subsets) == 1 && len(ep.Subsets[0].Ports) == 2 {
		return true
	}

	return false

}

func updateOVNConfig(ep *kapi.Endpoints) {
	if !validateOVNConfigEndpoint(ep) {
		logrus.Errorf("endpoint %s is not in the right format to configure OVN", ep.Name)
		return
	}
	var southboundDBPort string
	var northboundDBPort string
	var masterIPList []string
	for _, ovnDB := range ep.Subsets[0].Ports {
		if ovnDB.Name == "south" {
			southboundDBPort = strconv.Itoa(int(ovnDB.Port))
		}
		if ovnDB.Name == "north" {
			northboundDBPort = strconv.Itoa(int(ovnDB.Port))
		}
	}
	for _, address := range ep.Subsets[0].Addresses {
		masterIPList = append(masterIPList, address.IP)
	}
	err := config.UpdateOVNNodeAuth(masterIPList, southboundDBPort, northboundDBPort)
	if err != nil {
		logrus.Errorf(err.Error())
		return
	}
	for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
		if err := auth.SetDBAuth(); err != nil {
			logrus.Errorf(err.Error())
			return
		}
		logrus.Infof("OVN databases reconfigured, masterIP %s, northbound-db %s, southbound-db %s", ep.Subsets[0].Addresses[0].IP, northboundDBPort, southboundDBPort)
	}

}

//watchConfigEndpoints starts the watching of Endpoint resource and calls back to the appropriate handler logic
func (cluster *OvnClusterController) watchConfigEndpoints() error {
	_, err := cluster.watchFactory.AddFilteredEndpointsHandler(config.Kubernetes.OVNConfigNamespace,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ep := obj.(*kapi.Endpoints)
				if ep.Name == "ovnkube-db" {
					updateOVNConfig(ep)
					return
				}
			},
			UpdateFunc: func(old, new interface{}) {
				epNew := new.(*kapi.Endpoints)
				epOld := old.(*kapi.Endpoints)
				if !reflect.DeepEqual(epNew.Subsets, epOld.Subsets) && epNew.Name == "ovnkube-db" {
					updateOVNConfig(epNew)
				}

			},
		}, nil)
	return err
}
