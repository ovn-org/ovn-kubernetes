package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	ovncluster "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	kexec "k8s.io/utils/exec"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = config.Version
	c.Flags = append([]cli.Flag{
		// Kubernetes-related options
		cli.StringFlag{
			Name:  "cluster-subnet",
			Value: "11.11.0.0/16",
			Usage: "Cluster wide IP subnet to use",
		},
		cli.StringFlag{
			Name: "service-cluster-ip-range",
			Usage: "A CIDR notation IP range from which k8s assigns " +
				"service cluster IPs. This should be the same as the one " +
				"provided for kube-apiserver \"-service-cluster-ip-range\" " +
				"option.",
		},

		// Mode flags
		cli.BoolFlag{
			Name:  "net-controller",
			Usage: "Flag to start the central controller that watches pods/services/policies",
		},
		cli.StringFlag{
			Name:  "init-master",
			Usage: "initialize master, requires the hostname as argument",
		},
		cli.StringFlag{
			Name:  "init-node",
			Usage: "initialize node, requires the name that node is registered with in kubernetes cluster",
		},
		cli.StringFlag{
			Name: "remove-node",
			Usage: "Remove a node from the OVN cluster, requires the name " +
				"that the node is registered with in the kubernetes cluster",
		},

		// Daemon file
		cli.StringFlag{
			Name:  "pidfile",
			Usage: "Name of file that will hold the ovnkube pid (optional)",
		},

		// Gateway flags
		cli.BoolFlag{
			Name:  "init-gateways",
			Usage: "initialize a gateway in the minion. Only useful with \"init-node\"",
		},
		cli.StringFlag{
			Name: "gateway-interface",
			Usage: "The interface in minions that will be the gateway interface. " +
				"If none specified, then the node's interface on which the " +
				"default gateway is configured will be used as the gateway " +
				"interface. Only useful with \"init-gateways\"",
		},
		cli.StringFlag{
			Name: "gateway-nexthop",
			Usage: "The external default gateway which is used as a next hop by " +
				"OVN gateway.  This is many times just the default gateway " +
				"of the node in question. If not specified, the default gateway" +
				"configured in the node is used. Only useful with " +
				"\"init-gateways\"",
		},
		cli.BoolFlag{
			Name: "gateway-spare-interface",
			Usage: "If true, assumes that \"gateway-interface\" provided can be " +
				"exclusively used for the OVN gateway.  When true, only OVN" +
				"related traffic can flow through this interface",
		},
		cli.BoolFlag{
			Name: "gateway-localnet",
			Usage: "If true, creates a localnet gateway to let traffic reach " +
				"host network and also exit host with iptables NAT",
		},
		cli.BoolFlag{
			Name:  "nodeport",
			Usage: "Setup nodeport based ingress on gateways.",
		},

		cli.BoolFlag{
			Name:  "ha",
			Usage: "HA option to reconstruct OVN database after failover",
		},
	}, config.Flags...)
	c.Action = func(c *cli.Context) error {
		return runOvnKube(c)
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func delPidfile(pidfile string) {
	if pidfile != "" {
		if _, err := os.Stat(pidfile); err == nil {
			if err := os.Remove(pidfile); err != nil {
				logrus.Errorf("%s delete failed: %v", pidfile, err)
			}
		}
	}
}

func runOvnKube(ctx *cli.Context) error {
	exec := kexec.New()
	_, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}
	pidfile := ctx.String("pidfile")

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		delPidfile(pidfile)
		os.Exit(1)
	}()

	defer delPidfile(pidfile)

	if pidfile != "" {
		// need to test if already there
		_, err := os.Stat(pidfile)

		// Create if it doesn't exist, else exit with error
		if os.IsNotExist(err) {
			if err := ioutil.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
				logrus.Errorf("failed to write pidfile %s (%v). Ignoring..", pidfile, err)
			}
		} else {
			// get the pid and see if it exists
			pid, err := ioutil.ReadFile(pidfile)
			if err != nil {
				logrus.Errorf("pidfile %s exists but can't be read", pidfile)
				return err
			}
			_, err1 := os.Stat("/proc/" + string(pid[:]) + "/cmdline")
			if os.IsNotExist(err1) {
				// Left over pid from dead process
				if err := ioutil.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
					logrus.Errorf("failed to write pidfile %s (%v). Ignoring..", pidfile, err)
				}
			} else {
				logrus.Errorf("pidfile %s exists and ovnkube is running", pidfile)
				os.Exit(1)
			}
		}
	}

	if err = util.SetExec(exec); err != nil {
		logrus.Errorf("Failed to initialize exec helper: %v", err)
		return err
	}

	nodeToRemove := ctx.String("remove-node")
	if nodeToRemove != "" {
		err = util.RemoveNode(nodeToRemove)
		if err != nil {
			logrus.Errorf("Failed to remove node %v", err)
		}
		return nil
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		panic(err.Error())
	}

	// create factory and start the controllers asked for
	stopChan := make(chan struct{})
	factory, err := factory.NewWatchFactory(clientset, stopChan)
	if err != nil {
		panic(err.Error)
	}

	netController := ctx.Bool("net-controller")
	master := ctx.String("init-master")
	node := ctx.String("init-node")
	nodePortEnable := ctx.Bool("nodeport")
	clusterController := ovncluster.NewClusterController(clientset, factory)

	if master != "" || node != "" {
		clusterController.HostSubnetLength = 8
		clusterController.GatewayInit = ctx.Bool("init-gateways")
		clusterController.GatewayIntf = ctx.String("gateway-interface")
		clusterController.GatewayNextHop = ctx.String("gateway-nexthop")
		clusterController.GatewaySpareIntf = ctx.Bool("gateway-spare-interface")
		clusterController.LocalnetGateway = ctx.Bool("gateway-localnet")
		clusterController.OvnHA = ctx.Bool("ha")
		_, clusterController.ClusterIPNet, err = net.ParseCIDR(ctx.String("cluster-subnet"))
		if err != nil {
			panic(err.Error)
		}

		clusterServicesSubnet := ctx.String("service-cluster-ip-range")
		if clusterServicesSubnet != "" {
			var servicesSubnet *net.IPNet
			_, servicesSubnet, err = net.ParseCIDR(
				clusterServicesSubnet)
			if err != nil {
				panic(err.Error)
			}
			clusterController.ClusterServicesSubnet = servicesSubnet.String()
		}
		clusterController.NodePortEnable = nodePortEnable

		if master != "" {
			if runtime.GOOS == "windows" {
				panic("Windows is not supported as master node")
			}
			// run the cluster controller to init the master
			err := clusterController.StartClusterMaster(master)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}

		if node != "" {
			if config.Kubernetes.Token == "" {
				panic("Cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
			}

			err := clusterController.StartClusterNode(node)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}
	}
	if netController {
		ovnController := ovn.NewOvnController(clientset, factory, nodePortEnable)
		if clusterController.OvnHA {
			err := clusterController.RebuildOVNDatabase(master, ovnController)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}
		if err := ovnController.Run(); err != nil {
			logrus.Errorf(err.Error())
			panic(err.Error())
		}
	}
	if master != "" || netController {
		// run forever
		select {}
	}
	if node != "" {
		// run forever
		select {}
	}

	return nil
}
