package app

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
)

// InitMasterCmd initializes k8s master node.
var InitMasterCmd = cli.Command{
	Name:  "master-init",
	Usage: "Initialize k8s master node",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "cluster-ip-subnet",
			Usage: "The cluster wide larger subnet of private ip addresses.",
		},
		cli.StringFlag{
			Name:  "master-switch-subnet",
			Usage: "The smaller subnet just for master.",
		},
		cli.StringFlag{
			Name:  "node-name",
			Usage: "A unique node name.",
		},
		cli.StringFlag{
			Name:  "nb-privkey",
			Usage: "The private key used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cert",
			Usage: "The certificate used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cacert",
			Usage: "The CA certificate used for northbound API SSL connections.",
		},
	},
	Action: func(context *cli.Context) error {
		if err := initMaster(context); err != nil {
			return fmt.Errorf("Failed init master: %v", err)
		}
		return nil
	},
}

func initMaster(context *cli.Context) error {
	masterSwitchSubnet := context.String("master-switch-subnet")
	if masterSwitchSubnet == "" {
		return fmt.Errorf("argument --master-switch-subnet should be non-null")
	}

	clusterIPSubnet := context.String("cluster-ip-subnet")
	if clusterIPSubnet == "" {
		return fmt.Errorf("argument --cluster-ip-subnet should be non-null")
	}

	nodeName := context.String("node-name")
	if nodeName == "" {
		return fmt.Errorf("argument --cluster-ip-subnet should be non-null")
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	err := fetchOVNNB(context)
	if err != nil {
		return err
	}

	// Create a single common distributed router for the cluster.
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", nodeName, "--", "set", "logical_router", nodeName, "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to create a single common distributed router for the cluster, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Check whether this version of OVN supports DNS.
	_, _, err = util.RunOVNNbctl("list", "dns")
	if err == nil {
		var dns string
		dns, stderr, err = util.RunOVNNbctl("--data=bare",
			"--no-heading", "--columns=_uuid", "find", "dns",
			"external_ids:k8s-dns=yes")
		if err != nil {
			logrus.Errorf("Failed to get dns, stderr: %q, error: %v", stderr,
				err)
			return err
		}

		if dns == "" {
			stdout, stderr, err = util.RunOVNNbctl("create", "dns",
				"external_ids:k8s-dns=yes")
			if err != nil {
				logrus.Errorf("Failed to create dns, stdout: %q, stderr: %q, "+
					"error: %v", stdout, stderr, err)
				return err
			}
		}
	} else {
		// The schema does not support DNS.
		logrus.Debugf("The schema does not support DNS. Ignoring..")
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

	err = createManagementPort(nodeName, masterSwitchSubnet, clusterIPSubnet)
	if err != nil {
		return fmt.Errorf("Failed create management port: %v", err)
	}
	return nil
}
