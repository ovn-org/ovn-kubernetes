package main

import (
	"os"

	"github.com/openvswitch/ovn-kubernetes/go-controller/cmd/ovn-k8s-overlay/app"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovn-k8s-overlay"
	c.Usage = "run ovn-k8s-overlay to init master, minion, gateway"
	c.Version = "0.0.1"

	c.Commands = []cli.Command{
		app.InitGatewayCmd,
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
