// +build linux

package cluster

import (
	"fmt"
	"syscall"

	kapi "k8s.io/api/core/v1"

	"github.com/coreos/go-iptables/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"

	"github.com/vishvananda/netlink"
)

// getDefaultGatewayInterfaceDetails returns the interface name on
// which the default gateway (for route to 0.0.0.0) is configured.
// It also returns the default gateway itself.
func getDefaultGatewayInterfaceDetails() (string, string, error) {
	routes, err := netlink.RouteList(nil, syscall.AF_INET)
	if err != nil {
		return "", "", fmt.Errorf("Failed to get routing table in node")
	}

	for i := range routes {
		route := routes[i]
		if route.Dst == nil && route.Gw != nil && route.LinkIndex > 0 {
			intfLink, err := netlink.LinkByIndex(route.LinkIndex)
			if err != nil {
				continue
			}
			intfName := intfLink.Attrs().Name
			if intfName != "" {
				return intfName, route.Gw.String(), nil
			}
		}
	}
	return "", "", fmt.Errorf("Failed to get default gateway interface")
}

func getIntfName(gatewayIntf string) (string, error) {
	// The given (or autodetected) interface is an OVS bridge and this could be
	// created by us using util.NicToBridge() or it was pre-created by the user.

	// Is intfName a port of gatewayIntf?
	intfName := util.GetNicName(gatewayIntf)
	_, stderr, err := util.RunOVSVsctl("--if-exists", "get",
		"interface", intfName, "ofport")
	if err != nil {
		return "", fmt.Errorf("failed to get ofport of %s, stderr: %q, error: %v",
			intfName, stderr, err)
	}
	return intfName, nil
}

func addWARToAccessAPIServer(kube kube.Interface) error {
	// first get the kubernetes service's cluster IP and port
	svc, err := kube.GetService(kapi.NamespaceDefault, "kubernetes")
	if err != nil {
		return fmt.Errorf("failed to get service 'kubernetes' in namespace %q",
			kapi.NamespaceDefault)
	}

	clusterIP := svc.Spec.ClusterIP
	clusterPort := fmt.Sprintf("%d", svc.Spec.Ports[0].Port)

	// next get the kubernetes service's endpoint IPs and port
	ep, err := kube.GetEndpoint(kapi.NamespaceDefault, "kubernetes")
	if err != nil {
		return fmt.Errorf("failed to get the endpoints and port for 'kubernetes' service")
	}

	epIPPorts := make([]string, 0)
	for _, subset := range ep.Subsets {
		for _, ip := range subset.Addresses {
			for _, port := range subset.Ports {
				epIPPorts = append(epIPPorts, fmt.Sprintf("%s:%d", ip.IP, port.Port))
			}
		}
	}

	ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4)
	if err != nil {
		return fmt.Errorf("failed to initialize iptables: %v", err)
	}
	// delete all the existing OVN-KUBE-APIACCESS rules
	_ = ipt.ClearChain("nat", "OVN-KUBE-APIACCESS")

	rules := make([]iptRule, 0)
	rules = append(rules, iptRule{
		table: "nat",
		chain: "OUTPUT",
		args:  []string{"-j", "OVN-KUBE-APIACCESS"},
	})
	for i, epIPPort := range epIPPorts {
		ruleArgs := []string{"-p", "tcp", "-d", clusterIP, "--dport", clusterPort, "-j", "DNAT",
			"--to-destination", epIPPort}
		// if there are more than one master, then use a round robin approach to distribute the connections
		// across various master
		if i != 0 {
			ruleArgs = append(ruleArgs, "-m", "statistic", "--mode", "nth", "--every", fmt.Sprintf("%d", i+1))
		}

		rules = append(rules, iptRule{
			table: "nat",
			chain: "OVN-KUBE-APIACCESS",
			args:  ruleArgs,
		})
	}

	logrus.Debugf("Add rules %v to access K8s API Server through K8s service cluster IP %s:%s",
		rules, clusterIP, clusterPort)
	return addIptRules(ipt, rules)
}
