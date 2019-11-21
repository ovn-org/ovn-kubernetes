package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kexec "k8s.io/utils/exec"
)

func main() {
	c := cli.NewApp()
	c.Name = "hybrid-overlay"
	c.Usage = "a controller to integrate disparate networks within kubernetes with VXLAN routes"
	c.Version = config.Version
	c.Flags = append(c.Flags, controller.GetHybridOverlayCLIFlags([]cli.Flag{
		cli.BoolFlag{
			Name:  "master",
			Usage: "Run in master mode, where subnets are assigned to incoming non-native nodes",
		},
		cli.StringFlag{
			Name: "node",
			Usage: "Run in node mode, where actual actions are performed " +
				"to integrate with rest of the network. Requires the " +
				"name that node is registered with in kubernetes cluster.",
		}})...)
	c.Flags = append(c.Flags, config.K8sFlags...)
	c.Action = func(c *cli.Context) error {
		if err := runHybridOverlay(c); err != nil {
			panic(err.Error())
		}
		return nil
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func runHybridOverlay(ctx *cli.Context) error {
	exec := kexec.New()
	if _, err := config.InitConfig(ctx, exec, nil); err != nil {
		return err
	}

	if err := util.SetExecWithoutOVS(exec); err != nil {
		return err
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		return err
	}

	stopChan := make(chan struct{})
	defer close(stopChan)

	factory, err := factory.NewWatchFactory(clientset, stopChan)
	if err != nil {
		return err
	}

	master := ctx.Bool("master")
	nodeName := ctx.String("node")

	if err := controller.StartHybridOverlay(ctx, master, nodeName, clientset, factory); err != nil {
		return err
	}

	if master || nodeName != "" {
		// run forever
		select {}
	}

	return nil
}
