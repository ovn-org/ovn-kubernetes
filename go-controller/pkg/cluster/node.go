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
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

type postReadyFn func() error

func isOVNControllerReady(name string) (bool, error) {
	const runDir string = "/var/run/openvswitch/"

	pid, err := ioutil.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		return false, fmt.Errorf("unknown pid for ovn-controller process: %v", err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSuffix(string(pid), "\n"))
		ret, _, err := util.RunOVSAppctl("-t", ctlFile, "connection-status")
		if err == nil {
			logrus.Infof("node %s connection status = %s", name, ret)
			return ret == "connected", nil
		}
		return false, err
	})
	if err != nil {
		return false, fmt.Errorf("timed out waiting sbdb for node %s: %v", name, err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		_, _, err := util.RunOVSVsctl("--", "br-exists", "br-int")
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("timed out checking whether br-int exists or not on node %s: %v", name, err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		stdout, _, err := util.RunOVSOfctl("dump-aggregate", "br-int")
		if err != nil {
			return false, fmt.Errorf("failed to get aggregate flow statistics: %v", err)
		}
		return strings.Index(stdout, "flow_count=0") == -1, nil
	})
	if err != nil {
		return false, fmt.Errorf("timed out dumping br-int flow entries for node %s: %v", name, err)
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
	var wg sync.WaitGroup

	if config.MasterHA.ManageDBServers {
		var readyChan = make(chan bool, 1)

		err = cluster.watchConfigEndpoints(readyChan)
		if err != nil {
			return err
		}
		// Hold until we are certain that the endpoint has been setup.
		// We risk polling an inactive master if we don't wait while a new leader election is on-going
		<-readyChan
	} else {
		for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
			if err := auth.SetDBAuth(); err != nil {
				return err
			}
		}
	}

	err = setupOVNNode(name)
	if err != nil {
		return err
	}

	for _, clusterSubnet := range config.Default.ClusterSubnets {
		clusterSubnets = append(clusterSubnets, clusterSubnet.CIDR.String())
	}

	messages := make(chan error)

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if node, err = cluster.Kube.GetNode(name); err != nil {
			return false, fmt.Errorf("error retrieving node %s: %v", name, err)
		}
		if cidr, _, err = util.RunOVNNbctl("get", "logical_switch", node.Name, "other-config:subnet"); err != nil {
			return false, fmt.Errorf("error retrieving logical switch: %v", err)
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for node's: %q logical switch: %v", name, err)
	}

	_, subnet, err = net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid hostsubnet found for node %s: %v", node.Name, err)
	}

	logrus.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	if _, err = isOVNControllerReady(name); err != nil {
		return err
	}

	type readyFunc func(string, string) (bool, error)
	var readyFuncs []readyFunc
	var nodeAnnotations map[string]string
	var postReady postReadyFn

	// If gateway is enabled, get gateway annotations
	if config.Gateway.Mode != config.GatewayModeDisabled {
		nodeAnnotations, postReady, err = cluster.initGateway(node.Name, subnet.String())
		if err != nil {
			return err
		}
		readyFuncs = append(readyFuncs, GatewayReady)
	}

	// Get management port annotaitons
	mgmtPortAnnotations, err := CreateManagementPort(node.Name, subnet, clusterSubnets)
	if err != nil {
		return err
	}

	readyFuncs = append(readyFuncs, ManagementPortReady)

	// Combine mgmtPortAnnotations with any existing gwyAnnotations
	for k, v := range mgmtPortAnnotations {
		nodeAnnotations[k] = v
	}

	wg.Add(len(readyFuncs))

	// Set node annotations
	err = cluster.Kube.SetAnnotationsOnNode(node, nodeAnnotations)
	if err != nil {
		return fmt.Errorf("Failed to set node %s annotation: %v", node.Name, nodeAnnotations)
	}

	portName := "k8s-" + node.Name

	// Wait for the portMac to be created
	for _, f := range readyFuncs {
		go func(rf readyFunc) {
			defer wg.Done()
			err := wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
				return rf(node.Name, portName)
			})
			messages <- err
		}(f)
	}
	go func() {
		wg.Wait()
		close(messages)
	}()

	for i := range messages {
		if i != nil {
			return fmt.Errorf("Timeout error while obtaining addresses for %s (%v)", portName, i)
		}
	}

	if postReady != nil {
		err = postReady()
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

func updateOVNConfig(ep *kapi.Endpoints, readyChan chan bool) error {
	masterIPList, southboundDBPort, northboundDBPort, err := util.ExtractDbRemotesFromEndpoint(ep)
	if err != nil {
		return err
	}

	config.UpdateOVNNodeAuth(masterIPList, strconv.Itoa(int(southboundDBPort)), strconv.Itoa(int(northboundDBPort)))

	for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	logrus.Infof("OVN databases reconfigured, masterIPs %v, northbound-db %v, southbound-db %v", masterIPList, northboundDBPort, southboundDBPort)

	readyChan <- true
	return nil
}

//watchConfigEndpoints starts the watching of Endpoint resource and calls back to the appropriate handler logic
func (cluster *OvnClusterController) watchConfigEndpoints(readyChan chan bool) error {
	_, err := cluster.watchFactory.AddFilteredEndpointsHandler(config.Kubernetes.OVNConfigNamespace,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ep := obj.(*kapi.Endpoints)
				if ep.Name == "ovnkube-db" {
					if err := updateOVNConfig(ep, readyChan); err != nil {
						logrus.Errorf(err.Error())
					}
				}
			},
			UpdateFunc: func(old, new interface{}) {
				epNew := new.(*kapi.Endpoints)
				epOld := old.(*kapi.Endpoints)
				if !reflect.DeepEqual(epNew.Subsets, epOld.Subsets) && epNew.Name == "ovnkube-db" {
					if err := updateOVNConfig(epNew, readyChan); err != nil {
						logrus.Errorf(err.Error())
					}
				}
			},
		}, nil)
	return err
}
