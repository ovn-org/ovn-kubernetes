package app

import (
	"fmt"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
)

// InitGatewayCmd initializes k8s gateway node.
var InitGatewayCmd = cli.Command{
	Name:  "gateway-init",
	Usage: "Initialize k8s gateway node",
	Flags: append([]cli.Flag{
		cli.StringFlag{
			Name:  "cluster-ip-subnet",
			Usage: "The cluster wide larger subnet of private ip addresses.",
		},
		cli.StringFlag{
			Name:  "physical-interface",
			Usage: "The physical interface via which external connectivity is provided.",
		},
		cli.StringFlag{
			Name:  "bridge-interface",
			Usage: "The OVS bridge interface via which external connectivity is provided.",
		},
		cli.StringFlag{
			Name:  "physical-ip",
			Usage: "The ip address of the physical interface or bridge interface via which external connectivity is provided. This should be of the form IP/MASK.",
		},
		cli.StringFlag{
			Name:  "node-name",
			Usage: "A unique node name.",
		},
		cli.StringFlag{
			Name:  "default-gw",
			Usage: "The next hop IP address for your physical interface.",
		},
		cli.StringFlag{
			Name:  "rampout-ip-subnets",
			Usage: "Uses this gateway to rampout traffic originating from the specified comma separated ip subnets.  Used to distribute outgoing traffic via multiple gateways.",
		},
	}, config.Flags...),
	Action: func(context *cli.Context) error {
		if err := initGateway(context); err != nil {
			return fmt.Errorf("failed init gateway: %v", err)
		}
		return nil
	},
}

func initGateway(context *cli.Context) error {
	if _, err := config.InitConfig(context, &config.Defaults{OvnNorthAddress: true}); err != nil {
		return err
	}

	clusterIPSubnet := context.String("cluster-ip-subnet")
	if clusterIPSubnet == "" {
		return fmt.Errorf("argument --cluster-ip-subnet should be non-null")
	}

	nodeName := context.String("node-name")
	if nodeName == "" {
		return fmt.Errorf("argument --node-name should be non-null")
	}

	physicalIP := context.String("physical-ip")
	if physicalIP == "" {
		return fmt.Errorf("argument --physical-ip should be non-null")
	}

	physicalInterface := context.String("physical-interface")
	bridgeInterface := context.String("bridge-interface")
	defaultGW := context.String("default-gw")
	rampoutIPSubnet := context.String("rampout-ip-subnet")

	// We want either of args.physical_interface or args.bridge_interface provided. But not both. (XOR)
	if (len(physicalInterface) == 0 && len(bridgeInterface) == 0) || (len(physicalInterface) != 0 && len(bridgeInterface) != 0) {
		return fmt.Errorf("One of physical-interface or bridge-interface has to be specified")
	}

	return util.GatewayInit(clusterIPSubnet, nodeName, physicalIP,
		physicalInterface, bridgeInterface, defaultGW, rampoutIPSubnet)
}
