package app

import (
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
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

	ip, physicalIPNet, _ := net.ParseCIDR(physicalIP)
	n, _ := physicalIPNet.Mask.Size()
	physicalIPMask := fmt.Sprintf("%s/%d", ip.String(), n)
	physicalIP = ip.String()

	if defaultGW != "" {
		defaultgwByte := net.ParseIP(defaultGW)
		defaultGW = defaultgwByte.String()
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	err := fetchOVNNB(context)
	if err != nil {
		return err
	}

	k8sClusterRouter, err := getK8sClusterRouter()
	if err != nil {
		return err
	}

	systemID, err := getLocalSystemID()
	if err != nil {
		return err
	}

	// Find if gateway routers have been created before.
	firstGW := "no"
	physicalGW, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "logical_router", "options:chassis!=null")
	if err != nil {
		logrus.Errorf("Failed to get physical gateway, stderr: %q, error: %v", stderr, err)
		return err
	}
	if physicalGW == "" {
		firstGW = "yes"
	}

	// Create a gateway router.
	gatewayRouter := "GR_" + nodeName
	stdout, stderr, err := util.RunOVNNbctl("--", "--may-exist", "lr-add", gatewayRouter, "--", "set", "logical_router", gatewayRouter, "options:chassis="+systemID, "external_ids:physical_ip="+physicalIP, "external_ids:first_gateway="+firstGW)
	if err != nil {
		logrus.Errorf("Failed to create logical router %v, stdout: %q, stderr: %q, error: %v", gatewayRouter, stdout, stderr, err)
		return err
	}

	// Connect gateway router to switch "join".
	routerMac, stderr, err := util.RunOVNNbctl("--if-exist", "get", "logical_router_port", "rtoj-"+gatewayRouter, "mac")
	if err != nil {
		logrus.Errorf("Failed to get logical router port, stderr: %q, error: %v", stderr, err)
		return err
	}

	var routerIP string
	if routerMac == "" {
		routerMac = util.GenerateMac()
		routerIP, err = generateGatewayIP()
		if err != nil {
			return err
		}

		stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", gatewayRouter, "rtoj-"+gatewayRouter, routerMac, routerIP, "--", "set", "logical_router_port", "rtoj-"+gatewayRouter, "external_ids:connect_to_join=yes")
		if err != nil {
			logrus.Errorf("Failed to add logical port to router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Connect the switch "join" to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", "join", "jtor-"+gatewayRouter, "--", "set", "logical_switch_port", "jtor-"+gatewayRouter, "type=router", "options:router-port=rtoj-"+gatewayRouter, "addresses="+"\""+routerMac+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add a static route in GR with distributed router as the nexthop.
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "lr-route-add", gatewayRouter, clusterIPSubnet, "100.64.1.1")
	if err != nil {
		logrus.Errorf("Failed to add a static route in GR with distributed router as the nexthop, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Add a static route in GR with physical gateway as the default next hop.
	if defaultGW != "" {
		stdout, stderr, err = util.RunOVNNbctl("--may-exist", "lr-route-add", gatewayRouter, "0.0.0.0/0", defaultGW)
		if err != nil {
			logrus.Errorf("Failed to add a static route in GR with physical gateway as the default next hop, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Add a default route in distributed router with first GR as the nexthop.
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "lr-route-add", k8sClusterRouter, "0.0.0.0/0", "100.64.1.2")
	if err != nil {
		logrus.Errorf("Failed to add a default route in distributed router with first GR as the nexthop, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create 2 load-balancers for north-south traffic for each gateway router.  One handles UDP and another handles TCP.
	k8sNSLbTCP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:TCP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		logrus.Errorf("Failed to get k8sNSLbTCP, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sNSLbTCP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:TCP_lb_gateway_router="+gatewayRouter)
		if err != nil {
			logrus.Errorf("Failed to create load balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	k8sNSLbUDP, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "load_balancer", "external_ids:UDP_lb_gateway_router="+gatewayRouter)
	if err != nil {
		logrus.Errorf("Failed to get k8sNSLbUDP, stderr: %q, error: %v", stderr, err)
		return err
	}
	if k8sNSLbUDP == "" {
		stdout, stderr, err = util.RunOVNNbctl("--", "create", "load_balancer", "external_ids:UDP_lb_gateway_router="+gatewayRouter, "protocol=udp")
		if err != nil {
			logrus.Errorf("Failed to create load balancer, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}

	// Add north-south load-balancers to the gateway router.
	stdout, stderr, err = util.RunOVNNbctl("set", "logical_router", gatewayRouter, "load_balancer="+k8sNSLbTCP)
	if err != nil {
		logrus.Errorf("Failed to set north-south load-balancers to the gateway router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}
	stdout, stderr, err = util.RunOVNNbctl("add", "logical_router", gatewayRouter, "load_balancer", k8sNSLbUDP)
	if err != nil {
		logrus.Errorf("Failed to add north-south load-balancers to the gateway router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Create the external switch for the physical interface to connect to.
	externalSwitch := "ext_" + nodeName
	stdout, stderr, err = util.RunOVNNbctl("--may-exist", "ls-add", externalSwitch)
	if err != nil {
		logrus.Errorf("Failed to create logical switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	var ifaceID, macAddress string
	if physicalInterface != "" {
		// Connect physical interface to br-int. Get its mac address.
		ifaceID = physicalInterface + "_" + nodeName
		stdout, stderr, err = util.RunOVSVsctl("--", "--may-exist", "add-port", "br-int", physicalInterface, "--", "set", "interface", physicalInterface, "external-ids:iface-id="+ifaceID)
		if err != nil {
			logrus.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
		macAddress, stderr, err = util.RunOVSVsctl("--if-exists", "get", "interface", physicalInterface, "mac_in_use")
		if err != nil {
			logrus.Errorf("Failed to get macAddress, stderr: %q, error: %v", stderr, err)
			return err
		}

		// Flush the IP address of the physical interface.
		_, err = exec.Command("ip", "addr", "flush", "dev", physicalInterface).CombinedOutput()
		if err != nil {
			return err
		}
	} else {
		// A OVS bridge's mac address can change when ports are added to it.
		// We cannot let that happen, so make the bridge mac address permanent.
		macAddress, stderr, err = util.RunOVSVsctl("--if-exists", "get", "interface", bridgeInterface, "mac_in_use")
		if err != nil {
			logrus.Errorf("Failed to get macAddress, stderr: %q, error: %v", stderr, err)
			return err
		}
		if macAddress == "" {
			return fmt.Errorf("No mac_address found for the bridge-interface")
		}
		stdout, stderr, err = util.RunOVSVsctl("set", "bridge", bridgeInterface, "other-config:hwaddr="+macAddress)
		if err != nil {
			logrus.Errorf("Failed to set bridge, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
		ifaceID = bridgeInterface + "_" + nodeName

		// Connect bridge interface to br-int via patch ports.
		patch1 := "k8s-patch-br-int-" + bridgeInterface
		patch2 := "k8s-patch-" + bridgeInterface + "-br-int"

		stdout, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", bridgeInterface, patch2, "--", "set", "interface", patch2, "type=patch", "options:peer="+patch1)
		if err != nil {
			logrus.Errorf("Failed to add port, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}

		stdout, stderr, err = util.RunOVSVsctl("--may-exist", "add-port", "br-int", patch1, "--", "set", "interface", patch1, "type=patch", "options:peer="+patch2, "external-ids:iface-id="+ifaceID)
		if err != nil {
			logrus.Errorf("Failed to add port, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
	}
	// Add external interface as a logical port to external_switch. This is a learning switch
	// port with "unknown" address. The external world is accessed via this port.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", externalSwitch, ifaceID, "--", "lsp-set-addresses", ifaceID, "unknown")
	if err != nil {
		logrus.Errorf("Failed to add logical port to switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect GR to external_switch with mac address of external interface and that IP address.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lrp-add", gatewayRouter, "rtoe-"+gatewayRouter, macAddress, physicalIPMask, "--", "set", "logical_router_port", "rtoe-"+gatewayRouter, "external-ids:gateway-physical-ip=yes")
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Connect the external_switch to the router.
	stdout, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", externalSwitch, "etor-"+gatewayRouter, "--", "set", "logical_switch_port", "etor-"+gatewayRouter, "type=router", "options:router-port=rtoe-"+gatewayRouter, "addresses="+"\""+macAddress+"\"")
	if err != nil {
		logrus.Errorf("Failed to add logical port to router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// Default SNAT rules.
	stdout, stderr, err = util.RunOVNNbctl("--", "--id=@nat", "create", "nat", "type=snat", "logical_ip="+clusterIPSubnet, "external_ip="+physicalIP, "--", "add", "logical_router", gatewayRouter, "nat", "@nat")
	if err != nil {
		logrus.Errorf("Failed to create default SNAT rules, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return err
	}

	// When there are multiple gateway routers (which would be the likely default for any sane deployment), we need to SNAT traffic
	// heading to the logical space with the Gateway router's IP so that return traffic comes back to the same gateway router.
	if routerIP != "" {
		routerIPByte, _, err := net.ParseCIDR(routerIP)
		if err != nil {
			return err
		}
		stdout, stderr, err = util.RunOVNNbctl("set", "logical_router", gatewayRouter, "options:lb_force_snat_ip="+routerIPByte.String())
		if err != nil {
			logrus.Errorf("Failed to set logical router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
			return err
		}
		if rampoutIPSubnet != "" {
			rampoutIPSubnets := strings.Split(rampoutIPSubnet, ",")
			for _, rampoutIPSubnet = range rampoutIPSubnets {
				_, _, err = net.ParseCIDR(rampoutIPSubnet)
				if err != nil {
					continue
				}

				// Add source IP address based routes in distributed router for this gateway router.
				stdout, stderr, err = util.RunOVSVsctl("--may-exist", "--policy=src-ip", "lr-route-add", k8sClusterRouter, rampoutIPSubnet, routerIPByte.String())
				if err != nil {
					logrus.Errorf("Failed to add source IP address based routes in distributed router, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
					return err
				}
			}
		}
	}
	return nil
}

func generateGatewayIP() (string, error) {
	// All the routers connected to "join" switch are in 100.64.1.0/24
	// network and they have their external_ids:connect_to_join set.
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=network", "find", "logical_router_port", "external_ids:connect_to_join=yes")
	if err != nil {
		logrus.Errorf("Failed to get logical router ports which connect to \"join\" switch, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return "", err
	}

	ipStart, ipStartNet, _ := net.ParseCIDR("100.64.1.0/24")
	ipMax, _, _ := net.ParseCIDR("100.64.1.255/24")
	n, _ := ipStartNet.Mask.Size()
	for !ipStart.Equal(ipMax) {
		ipStart = util.NextIP(ipStart)
		used := 0
		ips := strings.Split(strings.TrimSpace(stdout), "\n")
		for _, v := range ips {
			if ipStart.String() == v {
				used = 1
				break
			}
		}
		if used == 1 {
			continue
		} else {
			break
		}
	}
	ipMask := fmt.Sprintf("%s/%d", ipStart.String(), n)
	return ipMask, nil
}

func getLocalSystemID() (string, error) {
	localSystemID, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Open_vSwitch", ".", "external_ids:system-id")
	if err != nil {
		logrus.Errorf("No system-id configured in the local host, stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if localSystemID == "" {
		return "", fmt.Errorf("No system-id configured in the local host")
	}

	return localSystemID, nil
}

func getK8sClusterRouter() (string, error) {
	k8sClusterRouter, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "logical_router", "external_ids:k8s-cluster-router=yes")
	if err != nil {
		logrus.Errorf("Failed to get k8s cluster router, stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if k8sClusterRouter == "" {
		return "", fmt.Errorf("Failed to get k8s cluster router")
	}

	return k8sClusterRouter, nil
}
