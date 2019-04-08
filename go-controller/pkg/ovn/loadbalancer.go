package ovn

import (
	"fmt"

	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbtransaction"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/helpers"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

func (ovn *Controller) getLoadBalancer(protocol kapi.Protocol) (string,
	error) {
	if protocol != kapi.ProtocolTCP && protocol != kapi.ProtocolUDP {
		return "", fmt.Errorf("unknown protocol")
	}

	if outStr, ok := ovn.loadbalancerClusterCache[string(protocol)]; ok {
		return outStr, nil
	}

	var key string
	if protocol == kapi.ProtocolTCP {
		key = "k8s-cluster-lb-tcp"
	} else {
		key = "k8s-cluster-lb-udp"
	}

	lbs := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid")
	for _, lb := range lbs {
		externalIds := lb.(map[string]interface{})["external_ids"]
		if externalIds == nil {
			continue
		}

		externalID := externalIds.(map[string]interface{})[key]
		if externalID != nil && externalID.(string) == "yes" {
			ovn.loadbalancerClusterCache[string(protocol)] = lb.(map[string]interface{})["uuid"].(string)
			return lb.(map[string]interface{})["uuid"].(string), nil
		}
	}

	ovn.loadbalancerClusterCache[string(protocol)] = ""
	return "", nil
}

func (ovn *Controller) getDefaultGatewayLoadBalancer(protocol kapi.Protocol) string {
	if outStr, ok := ovn.loadbalancerGWCache[string(protocol)]; ok {
		return outStr
	}

	var defaultGateway string

	gateways := ovn.ovnNbCache.GetMap("Logical_Router", "uuid")
	for _, gateway := range gateways {
		options := gateway.(map[string]interface{})["options"]
		if options == nil {
			continue
		}
		option := options.(map[string]interface{})["lb_force_snat_ip"]
		if option != nil && option.(string) == "100.64.1.2" {
			defaultGateway = gateway.(map[string]interface{})["name"].(string)
			break
		}
	}

	if defaultGateway == "" {
		logrus.Errorf("Error locating default gateway")
		return ""
	}

	externalIDKey := string(protocol) + "_lb_gateway_router"
	lbs := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid")
	for _, lb := range lbs {
		externalIds := lb.(map[string]interface{})["external_ids"]
		if externalIds == nil {
			continue
		}
		externalID := externalIds.(map[string]interface{})[externalIDKey]
		if externalID != nil && externalID.(string) == defaultGateway {
			ovn.loadbalancerGWCache[string(protocol)] = lb.(map[string]interface{})["uuid"].(string)
			return lb.(map[string]interface{})["uuid"].(string)
		}
	}

	return ""
}

func (ovn *Controller) getLoadBalancerVIPS(
	loadBalancer string) (map[string]interface{}, error) {
	vips := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid", loadBalancer)["vips"]

	if vips != nil {
		return vips.(map[string]interface{}), nil
	}
	return nil, nil
}

func (ovn *Controller) deleteLoadBalancerVIP(loadBalancer, vipString string) {
	lb := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid", loadBalancer)

	if lb["vips"] == nil {
		return
	}
	vips := lb["vips"].(map[string]interface{})

	for vip := range vips {
		if vip == vipString {
			delete(vips, vipString)
		}
	}

	var err error
	retry := true
	for retry {
		txn := ovn.ovnNBDB.Transaction("OVN_Northbound")
		txn.Update(dbtransaction.Update{
			Table: "Load_Balancer",
			Where: [][]interface{}{{"_uuid", "==", []string{"uuid", loadBalancer}}},
			Row: map[string]interface{}{
				"vips": helpers.MakeOVSDBMap(vips),
			},
		})
		_, err, retry = txn.Commit()
	}

	if err != nil {
		logrus.Errorf("Error in deleting load balancer vip %s for %s"+
			"error: %v", vipString, loadBalancer, err)
	}
}

func (ovn *Controller) createLoadBalancerVIP(lb string, serviceIP string, port int32, ips []string, targetPort int32) error {
	logrus.Debugf("Creating lb with %s, %s, %d, [%v], %d", lb, serviceIP, port, ips, targetPort)

	// With service_ip:port as a VIP, create an entry in 'load_balancer'
	key := fmt.Sprintf("%s:%d", serviceIP, port)

	if len(ips) == 0 {
		ovn.deleteLoadBalancerVIP(lb, key)
		return nil
	}

	var commaSeparatedEndpoints string
	for i, ep := range ips {
		comma := ","
		if i == 0 {
			comma = ""
		}
		commaSeparatedEndpoints += fmt.Sprintf("%s%s:%d", comma, ep, targetPort)
	}
	lbOvsdb := ovn.ovnNbCache.GetMap("Load_Balancer", "uuid", lb)
	if len(lbOvsdb) == 0 {
		return fmt.Errorf("Failed to get Load_Balancer record for %s", lb)
	}

	newVips := make(map[string]interface{})
	if lbOvsdb["vips"] != nil {
		newVips = lbOvsdb["vips"].(map[string]interface{})
	}

	newVips[key] = commaSeparatedEndpoints

	var err error
	retry := true
	for retry {
		txn := ovn.ovnNBDB.Transaction("OVN_Northbound")
		txn.Update(dbtransaction.Update{
			Table: "Load_Balancer",
			Where: [][]interface{}{{"_uuid", "==", []string{"uuid", lb}}},
			Row: map[string]interface{}{
				"vips": helpers.MakeOVSDBMap(newVips),
			},
		})
		_, err, retry = txn.Commit()
	}

	if err != nil {
		return fmt.Errorf("Error in creating load balancer: %s "+
			"error: %v", lb, err)
	}
	return nil
}
