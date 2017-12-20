package app

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
)

const (
	argMasterSwitchSubnet string = "master-switch-subnet"
)

// InitMasterCmd initializes k8s master node.
var InitMasterCmd = cli.Command{
	Name:  "master-init",
	Usage: "Initialize k8s master node",
	Flags: newFlags(
		cli.StringFlag{
			Name:  argMasterSwitchSubnet,
			Usage: "The smaller subnet just for master.",
		},
	),
	Action: func(context *cli.Context) error {
		if err := initMaster(context); err != nil {
			return fmt.Errorf("Failed init master: %v", err)
		}
		return nil
	},
}

func initMaster(context *cli.Context) error {
	masterSwitchSubnet, err := util.StringArg(context, argMasterSwitchSubnet)
	if err != nil {
		return err
	}

	clusterIPSubnet, err := util.StringArg(context, argClusterIPSubnet)
	if err != nil {
		return err
	}

	nodeName, err := util.StringArg(context, argNodeName)
	if err != nil {
		return err
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	if err := fetchOVNNB(context); err != nil {
		return err
	}

	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", nodeName, "--", "set", "logical_router", nodeName, "external_ids:k8s-cluster-router=yes")
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
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:k8s-cluster-lb-tcp=yes")
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
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+nodeName, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port rtoj-%v, stderr: %q, error: %v", nodeName, stderr, err)
		return err
	}
	if routerMac == "" {
		routerMac = util.GenerateMac()
		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", nodeName, "rtoj-"+nodeName, routerMac, "100.64.1.1/24", "--", "set", "logical_router_port", "rtoj-"+nodeName, "external_ids:connect_to_join=yes")
		if err != nil {
			logrus.Errorf("Failed to add logical router port rtoj-%v, stdout: %q, stderr: %q, error: %v", nodeName, stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+nodeName, "--", "set", "logical_switch_port", "jtor-"+nodeName, "type=router", "options:router-port=rtoj-"+nodeName, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical switch port to logical router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	err = ovn.CreateManagementPort(nodeName, masterSwitchSubnet, clusterIPSubnet)
	if err != nil {
		return fmt.Errorf("Failed create management port: %v", err)
	}
	return nil
}
