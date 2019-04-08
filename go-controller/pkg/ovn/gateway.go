package ovn

import (
	"fmt"
	"github.com/sirupsen/logrus"
)

func (ovn *Controller) getOvnGateways() []string {
	// Return all created gateways.
	var gateways []string
	gatewayRecords := ovn.ovnNbCache.GetMap("Logical_Router", "uuid")
	for _, gatewayRecord := range gatewayRecords {
		options := gatewayRecord.(map[string]interface{})["options"]
		if options == nil {
			continue
		}

		chassis := options.(map[string]interface{})["chassis"]
		if chassis == nil {
			continue
		}

		gateways = append(gateways, gatewayRecord.(map[string]interface{})["name"].(string))
	}

	return gateways
}

func (ovn *Controller) getGatewayPhysicalIP(
	physicalGateway string) (string, error) {
	gatewayRecord := ovn.ovnNbCache.GetMap("Logical_Router", "name",
		physicalGateway)
	if len(gatewayRecord) == 0 {
		return "", fmt.Errorf("failed to get gateway record for gateway %s",
			physicalGateway)
	}

	externalIds := gatewayRecord["external_ids"]
	if externalIds == nil {
		return "", fmt.Errorf("failed to get external_ids for gateway %s",
			physicalGateway)
	}

	physicalIP := externalIds.(map[string]interface{})["physical_ip"]
	if physicalIP == nil {
		return "", fmt.Errorf("failed to get physical_ip for gateway %s",
			physicalGateway)
	}

	return physicalIP.(string), nil
}

func (ovn *Controller) getGatewayLoadBalancer(physicalGateway,
	protocol string) string {
	externalIDKey := protocol + "_lb_gateway_router"

	lbRecords := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid")
	for _, lbRecord := range lbRecords {
		externalIds := lbRecord.(map[string]interface{})["external_ids"]
		if externalIds == nil {
			continue
		}

		externalIDValue := externalIds.(map[string]interface{})[externalIDKey]
		if externalIDValue == nil {
			continue
		}

		if externalIDValue.(string) == physicalGateway {
			return lbRecord.(map[string]interface{})["uuid"].(string)
		}
	}

	return ""
}

func (ovn *Controller) createGatewaysVIP(protocol string, port, targetPort int32, ips []string) error {

	logrus.Debugf("Creating Gateway VIP - %s, %s, %d, %v", protocol, port, targetPort, ips)

	// Each gateway has a separate load-balancer for N/S traffic

	physicalGateways := ovn.getOvnGateways()

	for _, physicalGateway := range physicalGateways {
		physicalIP, err := ovn.getGatewayPhysicalIP(physicalGateway)
		if err != nil {
			logrus.Errorf("physical gateway %s does not have physical ip (%v)",
				physicalGateway, err)
			continue
		}

		loadBalancer := ovn.getGatewayLoadBalancer(physicalGateway,
			protocol)
		if loadBalancer == "" {
			continue
		}

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
