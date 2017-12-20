package cluster

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/openshift/origin/pkg/util/netutils"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"

	kapi "k8s.io/client-go/pkg/api/v1"
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

	nodeIP, err := netutils.GetNodeIP(node.Name)
	if err != nil {
		logrus.Errorf("Failed to obtain node's IP: %v", err)
		return err
	}

	logrus.Infof("Node %s ready for ovn initialization with subnet %s", node.Name, subnet.String())

	out, err := exec.Command("systemctl", "start", "openvswitch").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error starting openvswitch service: %v\n  %q", err, string(out))
	}

	args := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-nb=\"%s\"", cluster.NorthDBClientAuth.GetURL()),
		fmt.Sprintf("external_ids:ovn-remote=\"%s\"", cluster.SouthDBClientAuth.GetURL()),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		"external_ids:ovn-encap-type=\"geneve\"",
		fmt.Sprintf("external_ids:k8s-api-server=\"%s\"", cluster.KubeServer),
		fmt.Sprintf("external_ids:k8s-api-token=\"%s\"", cluster.Token),
	}
	out, err = exec.Command("ovs-vsctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting OVS external IDs: %v\n  %q", err, string(out))
	}

	out, err = exec.Command("systemctl", "restart", "ovn-controller").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error starting ovn-controller service: %v\n  %q", err, string(out))
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Update config globals that OVN exec utils use
	cluster.NorthDBClientAuth.SetConfig()

	if runtime.GOOS != "win32" {
		cniPluginPath, err := exec.LookPath(config.CniPlugin)
		if err != nil {
			return fmt.Errorf("No cni plugin %v found", config.CniPlugin)
		}

		_, err = os.Stat(config.CniLinkPath)
		if err != nil && !os.IsExist(err) {
			err = os.MkdirAll(config.CniLinkPath, os.ModeDir)
			if err != nil {
				return err
			}
		}
		cniFile := config.CniLinkPath + "/ovn_cni"
		_, err = os.Stat(cniFile)
		if err != nil && !os.IsExist(err) {
			_, err = exec.Command("ln", "-s", cniPluginPath, cniFile).CombinedOutput()
			if err != nil {
				return err
			}
		}

		_, err = os.Stat(config.CniConfPath)
		if err != nil && !os.IsExist(err) {
			err = os.MkdirAll(config.CniConfPath, os.ModeDir)
			if err != nil {
				return err
			}
		}

		// Always create the CNI config for consistency.
		cniConf := config.CniConfPath + "/10-net.conf"
		f, err := os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write([]byte("{\"name\":\"ovn-cni\", \"type\":\"ovn_cni\"}"))
		if err != nil {
			return err
		}
	}

	return ovn.CreateManagementPort(node.Name, subnet.String(), cluster.ClusterIPNet.String())
}
