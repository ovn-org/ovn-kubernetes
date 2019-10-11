package controller

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"

	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes"
)

const (
	hybridOverlayClusterSubnetsFlag string = "hybrid-overlay-cluster-subnets"
)

// StartHybridOverlay starts one or both of the master and node controllers for
// hybrid overlay
func StartHybridOverlay(ctx *cli.Context, master bool, nodeName string,
	clientset kubernetes.Interface, wf *factory.WatchFactory) error {

	if master {
		hybridOverlayClusterSubnets, err := GetHybridOverlayClusterSubnets(ctx)
		if err != nil {
			return err
		}
		masterController, err := NewMaster(clientset, hybridOverlayClusterSubnets)
		if err != nil {
			return err
		}
		if err := masterController.Start(wf); err != nil {
			return err
		}
	}

	if nodeName != "" {
		nodeController, err := NewNode(clientset, nodeName)
		if err != nil {
			return err
		}
		if err := nodeController.Start(wf); err != nil {
			return err
		}
	}

	return nil
}

// GetHybridOverlayClusterSubnets returns any configured hybrid overlay cluster subnets
func GetHybridOverlayClusterSubnets(ctx *cli.Context) ([]config.CIDRNetworkEntry, error) {
	return config.ParseClusterSubnetEntries(ctx.String(hybridOverlayClusterSubnetsFlag))
}

// GetHybridOverlayCLIFlags returns standard CLI flags for hybrid overlay functionality,
// and appends any passed-in flags to the list for convenience
func GetHybridOverlayCLIFlags(flags []cli.Flag) []cli.Flag {
	return append(flags, []cli.Flag{
		cli.StringFlag{
			Name: hybridOverlayClusterSubnetsFlag,
			Usage: "A comma separated set of IP subnets and the associated" +
				"hostsubnetlengths (eg, \"10.128.0.0/14/23,10.0.0.0/14/23\"). " +
				"to use with the extended hybrid network. Each entry is given " +
				"in the form IP address/subnet mask/hostsubnetlength, " +
				"the hostsubnetlength is optional and if unspecified defaults to 24. The " +
				"hostsubnetlength defines how many IP addresses are dedicated to each node.",
		},
	}...)
}
