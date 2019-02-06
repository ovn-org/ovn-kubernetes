package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/common"
	mastercontroller "github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/master"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	kexec "k8s.io/utils/exec"
)

func main() {
	c := cli.NewApp()
	c.Name = "hybrid-sdn"
	c.Usage = "a controller to integrate disparate networks within kubernetes with VxLAN routes"
	c.Version = config.Version
	c.Flags = append([]cli.Flag{
		// Kubernetes-related options
		cli.StringFlag{
			Name:  "cluster-subnet",
			Value: "10.128.0.0/16",
			Usage: "A comma separated set of IP subnets and the associated with the main kubernetes cluster",
		},
		cli.StringFlag{
			Name:  "hybrid-cluster-subnet",
			Value: "11.128.0.0/16",
			Usage: "A comma separated set of IP subnets and the associated with the extended hybrid network",
		},

		// Mode flags
		cli.BoolFlag{
			Name:  "master",
			Usage: "Run in master mode, where subnets are assigned to in-coming non-native nodes",
		},
		cli.BoolFlag{
			Name:  "node",
			Usage: "Run in node mode, where actual actions are performed to integrate with rest of the network",
		},
	}, config.Flags...)
	c.Action = func(c *cli.Context) error {
		return runHybridSDN(c)
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func runHybridSDN(ctx *cli.Context) error {
	exec := kexec.New()
	_, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		panic(err.Error())
	}

	// TODO: expose the stop channel to user? (trap etc)
	stopChan := make(chan struct{})

	master := ctx.Bool("master")
	node := ctx.Bool("node")

	if master {
		mastercontroller.HybridClusterSubnet = ctx.String("hybrid-cluster-subnet")
		err = common.RunMaster(clientset, stopChan)
	}
	if node {
		err = common.RunNode(clientset, stopChan)
	}

	return err
}
