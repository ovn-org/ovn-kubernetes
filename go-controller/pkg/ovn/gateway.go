package ovn

import (
	"os/exec"
	"strings"
	"unicode"

	"github.com/Sirupsen/logrus"
)

func (ovn *Controller) getOvnGateways() ([]string, error) {
	// Return all created gateways.
	out, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=name", "find",
		"logical_router",
		"options:chassis!=null").CombinedOutput()
	return strings.Fields(string(out)), err
}

func (ovn *Controller) createGatewaysVIP(protocol string, port, targetPort int32, ips []string) error {

	logrus.Debugf("Creating Gateway VIP - %s, %s, %d, %v", protocol, port, targetPort, ips)

	// Each gateway has a separate load-balancer for N/S traffic

	physicalGateways, err := ovn.getOvnGateways()
	if err != nil {
		return err
	}

	for _, physicalGateway := range physicalGateways {
		// Go through each gateway to get its physical_ip and load-balancer.
		out, err := exec.Command(OvnNbctl, "get", "logical_router", physicalGateway,
			"external_ids:physical_ip").CombinedOutput()
		if err != nil {
			logrus.Errorf("Error in getting gateway physical IP from %s: %v(%v)", physicalGateway, string(out), err)
			continue
		}

		physicalIP := strings.Trim(string(out), "\" \n")

		if physicalIP == "" {
			logrus.Errorf("physical gateway %s does not have physical ip", physicalGateway)
			continue
		}

		externalIDKey := protocol + "_lb_gateway_router"
		out, err = exec.Command(OvnNbctl, "--data=bare", "--no-heading",
			"--columns=_uuid", "find", "load_balancer",
			"external_ids:"+externalIDKey+"="+
				physicalGateway).CombinedOutput()
		if err != nil {
			logrus.Errorf("Error in creating Gateway VIP. Find failed for %s on protocol %s: (%v)",
				physicalGateway, protocol, err)
			continue
		}

		if string(out) == "" {
			logrus.Errorf("physical gateway %s does not have load_balancer",
				(physicalGateway))
			continue
		}
		loadBalancer := strings.TrimFunc(string(out), unicode.IsSpace)

		// With the physical_ip:port as the VIP, add an entry in
		// 'load_balancer'.
		err = ovn.createLoadBalancerVIP(loadBalancer,
			physicalIP, port, ips, targetPort)
		if err != nil {
			logrus.Errorf("Failed to create VIP in load balancer %s - %v", loadBalancer, err)
			continue
		}
	}
	return nil
}
