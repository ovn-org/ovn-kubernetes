package cluster

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
)

func isOVNControllerReady(name string) (bool, error) {
	runDir := util.GetOvnRunDir()

	pid, err := ioutil.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		return false, fmt.Errorf("unknown pid for ovn-controller process: %v", err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSuffix(string(pid), "\n"))
		ret, _, err := util.RunOVSAppctl("-t", ctlFile, "connection-status")
		if err == nil {
			klog.Infof("node %s connection status = %s", name, ret)
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
			return false, nil
		}
		return !strings.Contains(stdout, "flow_count=0"), nil
	})
	if err != nil {
		return false, fmt.Errorf("timed out dumping br-int flow entries for node %s: %v", name, err)
	}

	return true, nil
}

func getNodeHostSubnetAnnotation(node *kapi.Node) (string, error) {
	subnet, ok := node.Annotations[ovn.OvnNodeSubnets]
	if ok {
		nodeSubnets := make(map[string]string)
		if err := json.Unmarshal([]byte(subnet), &nodeSubnets); err != nil {
			return "", fmt.Errorf("error parsing node-subnets annotation: %v", err)
		}
		subnet, ok = nodeSubnets["default"]
	}
	if !ok {
		return "", fmt.Errorf("node %q has no subnet annotation", node.Name)
	}
	return subnet, nil
}

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (cluster *OvnClusterController) StartClusterNode(name string) error {
	var err error
	var node *kapi.Node
	var subnet *net.IPNet
	var cidr string

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	lastLevel := fmt.Sprintf("%v", level.Get())

	if err := level.Set("5"); err != nil {
		klog.Errorf("setting klog \"loglevel\" to 5 failed, err: %v", err)
	}

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

	if node, err = cluster.Kube.GetNode(name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", name, err)
	}
	err = setupOVNNode(node)
	if err != nil {
		return err
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if node, err = cluster.Kube.GetNode(name); err != nil {
			klog.Infof("waiting to retrieve node %s: %v", name, err)
			return false, nil
		}
		cidr, err = getNodeHostSubnetAnnotation(node)
		if err != nil {
			klog.Infof("waiting for node %s to start, no annotation found on node for subnet - %v", name, err)
			return false, nil
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

	klog.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	if _, err = isOVNControllerReady(name); err != nil {
		return err
	}

	nodeAnnotator := kube.NewNodeAnnotator(cluster.Kube, node)
	waiter := newStartupWaiter(node.Name)

	// Initialize gateway resources on the node
	if err := cluster.initGateway(node.Name, subnet.String(), nodeAnnotator, waiter); err != nil {
		return err
	}

	// Initialize management port resources on the node
	if err := createManagementPort(node.Name, subnet, nodeAnnotator, waiter); err != nil {
		return err
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("Failed to set node %s annotations: %v", node.Name, err)
	}

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	if err := level.Set(lastLevel); err != nil {
		klog.Errorf("reset of initial klog \"loglevel\" failed, err: %v", err)
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

	klog.Infof("OVN databases reconfigured, masterIPs %v, northbound-db %v, southbound-db %v", masterIPList, northboundDBPort, southboundDBPort)

	readyChan <- true
	return nil
}

//watchConfigEndpoints starts the watching of Endpoint resource and calls back to the appropriate handler logic
func (cluster *OvnClusterController) watchConfigEndpoints(readyChan chan bool) error {
	_, err := cluster.watchFactory.AddFilteredEndpointsHandler(config.Kubernetes.OVNConfigNamespace, nil,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ep := obj.(*kapi.Endpoints)
				if ep.Name == "ovnkube-db" {
					if err := updateOVNConfig(ep, readyChan); err != nil {
						klog.Errorf(err.Error())
					}
				}
			},
			UpdateFunc: func(old, new interface{}) {
				epNew := new.(*kapi.Endpoints)
				epOld := old.(*kapi.Endpoints)
				if !reflect.DeepEqual(epNew.Subsets, epOld.Subsets) && epNew.Name == "ovnkube-db" {
					if err := updateOVNConfig(epNew, readyChan); err != nil {
						klog.Errorf(err.Error())
					}
				}
			},
		}, nil)
	return err
}
