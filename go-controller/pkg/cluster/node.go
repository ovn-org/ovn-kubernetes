package cluster

import (
	"fmt"
	"net"
	"os/exec"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/openshift/origin/pkg/util/netutils"

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

	overlayArgs := []string{
		"minion-init",
		fmt.Sprintf("--cluster-ip-subnet=%s", cluster.ClusterIPNet.String()),
		fmt.Sprintf("--minion-switch-subnet=%s", subnet.String()),
		fmt.Sprintf("--node-name=%s", name),
	}
	if cluster.NorthDBClientAuth.scheme == OvnDBSchemeSSL {
		overlayArgs = append(overlayArgs, fmt.Sprintf("--nb-privkey=%s", cluster.NorthDBClientAuth.PrivKey))
		overlayArgs = append(overlayArgs, fmt.Sprintf("--nb-cert=%s", cluster.NorthDBClientAuth.Cert))
		overlayArgs = append(overlayArgs, fmt.Sprintf("--nb-cacert=%s", cluster.NorthDBClientAuth.CACert))
	}
	out, err = exec.Command("ovn-k8s-overlay", overlayArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error setting up OVN k8s overlay: %v\n  %q", err, string(out))
	}

	return nil
}
