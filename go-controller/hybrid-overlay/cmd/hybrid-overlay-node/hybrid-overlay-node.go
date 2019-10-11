package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog"
	kexec "k8s.io/utils/exec"
)

var nodeName string

func main() {
	c := cli.NewApp()
	c.Name = "hybrid-overlay-node"
	c.Usage = "a node controller to integrate disparate networks with VXLAN tunnels"
	c.Version = config.Version
	c.Flags = config.GetFlags([]cli.Flag{
		cli.StringFlag{
			Name:        "node",
			Usage:       "The name of this node in the Kubernetes cluster.",
			Destination: &nodeName,
		}})
	c.Action = func(c *cli.Context) error {
		if err := runHybridOverlay(c); err != nil {
			panic(err.Error())
		}
		return nil
	}

	if err := c.Run(os.Args); err != nil {
		klog.Fatal(err)
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

	if nodeName == "" {
		return fmt.Errorf("missing node name; use the 'node' flag to provide one")
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

	if err := controller.StartHybridOverlay(false, nodeName, clientset, factory); err != nil {
		return err
	}

	// run forever
	select {}
}
