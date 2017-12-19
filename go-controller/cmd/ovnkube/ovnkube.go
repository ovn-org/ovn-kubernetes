package main

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = "0.0.1"
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
			Name:  "nodeport",
			Usage: "Setup nodeport based ingress on gateways.",
		},
		cli.IntFlag{
			Name:  "pod-subnet-prefix",
			Value: 24,
			Usage: "Subnet prefix allocated from --cluster-subnet which pods will be allocated IPs from",
		},
		cli.StringFlag{
			Name:  "pod-subnet-mode",
			Value: "node",
			Usage: "Pod subnet-per-node ('node')",
		},
	}, config.Flags...)
	c.Action = func(c *cli.Context) error {
		return runOvnKube(c)
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func runOvnKube(ctx *cli.Context) error {
	if err := config.InitConfig(ctx, nil); err != nil {
		return err
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
	psMode, err := ovn.ValidatePodSubnetMode(ctx.String("pod-subnet-mode"))
	if err != nil {
		panic(err.Error())
	}
	podSubnetPrefix := ctx.Int("pod-subnet-prefix")

	if master != "" || node != "" {
		_, ipNet, err := net.ParseCIDR(ctx.String("cluster-subnet"))
		if err != nil {
			panic(err.Error)
		}
		clusterSubnetSize, clusterSubnetBits := ipNet.Mask.Size()
		clusterSubnetPrefix := clusterSubnetBits - clusterSubnetSize
		if podSubnetPrefix > clusterSubnetBits || podSubnetPrefix < clusterSubnetPrefix+1 {
			panic(fmt.Sprintf("--pod-subnet-prefix must be less than %d and greater than %d", clusterSubnetBits, clusterSubnetPrefix+1))
		}
		psLength := uint32(32 - podSubnetPrefix)

		clusterConfig := &cluster.ClusterConfig{
			Kube: &kube.Kube{KClient: clientset},
			ClusterIPNet: ipNet,
			PodSubnetLength: psLength,
		}
		serviceCIDR := ctx.String("service-cluster-ip-range")
		if serviceCIDR != "" {
			var servicesSubnet *net.IPNet
			_, servicesSubnet, err = net.ParseCIDR(serviceCIDR)
			if err != nil {
				panic(err.Error)
			}
			clusterConfig.ClusterServicesSubnet = servicesSubnet.String()
		}

		gatewayConfig := &cluster.GatewayConfig{
			GatewayInit: ctx.Bool("init-gateways"),
			GatewayIntf: ctx.String("gateway-interface"),
			GatewayNextHop: ctx.String("gateway-nexthop"),
			GatewaySpareIntf: ctx.Bool("gateway-spare-interface"),
			NodePortEnable: nodePortEnable,
		}

		var clusterController cluster.OvnClusterController
		switch psMode {
		case ovn.PodSubnetModeNode:
			clusterController = cluster.NewNodeSubnetController(clusterConfig, gatewayConfig, factory)
		}

		if master != "" {
			if runtime.GOOS == "windows" {
				panic("Windows is not supported as master node")
			}
			// run the cluster controller to init the master
			err := cluster.StartMaster(clusterController, master)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}

		if node != "" {
			if config.Kubernetes.Token == "" {
				panic("Cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
			}

			err := cluster.StartNode(clusterController, node)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}
	}
	if netController {
		ovnController := ovn.NewOvnController(clientset, factory, psMode)
		ovnController.NodePortEnable = nodePortEnable
		ovnController.Run()
	}

	if master != "" || netController {
		// run forever
		select {}
	}

	return nil
}
