package app

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
)

const (
	argClusterIPSubnet string = "cluster-ip-subnet"
	argNodeName        string = "node-name"
	argNBPrivKey       string = "nb-privkey"
	argNBCert          string = "nb-cert"
	argNBCACert        string = "nb-cacert"
)

func newFlags(customFlags ...cli.Flag) []cli.Flag {
	flags := []cli.Flag{
		cli.StringFlag{
			Name:  argClusterIPSubnet,
			Usage: "The cluster wide larger subnet of private ip addresses.",
		},
		cli.StringFlag{
			Name:  argNodeName,
			Usage: "A unique node name.",
		},
		cli.StringFlag{
			Name:  argNBPrivKey,
			Usage: "The private key used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  argNBCert,
			Usage: "The certificate used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  argNBCACert,
			Usage: "The CA certificate used for northbound API SSL connections.",
		},
	}
	return append(flags, customFlags...)
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
		privkey, _ := util.StringArg(context, argNBPrivKey)
		if privkey != "" {
			config.NbctlPrivateKey = privkey
		}
		cert, _ := util.StringArg(context, argNBCert)
		if cert != "" {
			config.NbctlCertificate = cert
		}
		cacert, _ := util.StringArg(context, argNBCACert)
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
