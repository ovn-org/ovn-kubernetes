package main

import (
	"os"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/cmd/ovn-kube-util/app"
	"github.com/urfave/cli"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovn-kube-util"
	c.Usage = "Utils for kubernetes ovn"
	c.Version = "0.0.1"

	c.Commands = []cli.Command{
		app.NicsToBridgeCommand,
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
