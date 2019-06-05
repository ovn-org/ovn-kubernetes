package main

import (
	"os"

	"github.com/ovn-org/ovn-kubernetes/go-controller/cmd/ovn-kube-util/app"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovn-kube-util"
	c.Usage = "Utils for kubernetes ovn"
	c.Version = config.Version

	c.Commands = []cli.Command{
		app.NicsToBridgeCommand,
		app.BridgesToNicCommand,
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
