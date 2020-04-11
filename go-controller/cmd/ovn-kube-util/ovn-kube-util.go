package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ovn-org/ovn-kubernetes/go-controller/cmd/ovn-kube-util/app"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/urfave/cli"
	"k8s.io/klog"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovn-kube-util"
	c.Usage = "Utils for kubernetes ovn"
	c.Version = config.Version
	c.Flags = []cli.Flag{
		cli.IntFlag{
			Name: "loglevel",
			Usage: "klog verbosity level (default: 4). Info, warn, fatal, error are always printed. " +
				"For debug messages, use 5. ",
			Value: 0,
		},
	}
	c.Commands = []cli.Command{
		app.NicsToBridgeCommand,
		app.BridgesToNicCommand,
		app.ReadinessProbeCommand,
		app.OvnDBExporterCommand,
	}

	c.Before = func(ctx *cli.Context) error {
		var level klog.Level

		klog.SetOutput(os.Stderr)
		if err := level.Set(strconv.Itoa(ctx.Int("loglevel"))); err != nil {
			return fmt.Errorf("failed to set klog log level %v", err)
		}
		return nil
	}

	if err := c.Run(os.Args); err != nil {
		klog.Exit(err)
	}
}
