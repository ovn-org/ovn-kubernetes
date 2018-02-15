package app

import (
	"fmt"
	"net/url"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

// InitGatewayCmd initializes k8s gateway node.
var InitGatewayCmd = cli.Command{
	Name:  "gateway-init",
	Usage: "Initialize k8s gateway node",
	Flags: []cli.Flag{
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
		cli.StringFlag{
			Name:  "nb-privkey",
			Usage: "The private key used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cert",
			Usage: "The certificate used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cacert",
			Usage: "The CA certificate used for northbound API SSL connections.",
		},
	},
	Action: func(context *cli.Context) error {
		if err := initGateway(context); err != nil {
			return fmt.Errorf("failed init gateway: %v", err)
		}
		return nil
	},
}

func fetchOVNNB(context *cli.Context) error {
	ovnNB, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".", "external_ids:ovn-nb")
	if err != nil {
		logrus.Errorf("Failed to get OVN central database's ip address, stderr: %q, error: %v", stderr, err)
		return err
	}
	if ovnNB == "" {
		return fmt.Errorf("OVN central database's ip address not set")
	}
	logrus.Infof("Successfully get OVN central database's ip address %q", ovnNB)

	// verify the OVN central database's ip address.
	url, err := url.Parse(ovnNB)
	if err != nil {
		return fmt.Errorf("Failed to parse OVN northbound URL %q: %v", ovnNB, err)
	}

	config.Scheme = url.Scheme
	if url.Scheme == "ssl" {
		privkey, _ := util.StringArg(context, "nb-privkey")
		if privkey != "" {
			config.NbctlPrivateKey = privkey
		}
		cert, _ := util.StringArg(context, "nb-cert")
		if cert != "" {
			config.NbctlCertificate = cert
		}
		cacert, _ := util.StringArg(context, "nb-cacert")
		if cacert != "" {
			config.NbctlCACert = cacert
		}

		if config.NbctlPrivateKey == "" || config.NbctlCertificate == "" || config.NbctlCACert == "" {
			return fmt.Errorf("Must specify private key, certificate, and CA certificate for 'ssl' scheme")
		}

		if !util.PathExist(config.NbctlPrivateKey) {
			return fmt.Errorf("No private key %s found", config.NbctlPrivateKey)
		}
		if !util.PathExist(config.NbctlCertificate) {
			return fmt.Errorf("No certificate %s found", config.NbctlCertificate)
		}
		if !util.PathExist(config.NbctlCACert) {
			return fmt.Errorf("No CA certificate %s found", config.NbctlCACert)
		}
	}
	config.OvnNB = ovnNB
	return nil
}

func initGateway(context *cli.Context) error {
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

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	err := fetchOVNNB(context)
	if err != nil {
		return err
	}

	err = util.GatewayInit(clusterIPSubnet, nodeName, physicalIP,
		physicalInterface, bridgeInterface, defaultGW, rampoutIPSubnet)

	return err
}
