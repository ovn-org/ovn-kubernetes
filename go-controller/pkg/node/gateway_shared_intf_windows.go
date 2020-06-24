package node

import (
	"net"

	kapi "k8s.io/api/core/v1"
)

func initSharedGatewayIPTables() error {
	return nil
}

func cleanupSharedGatewayIPTChains() {
}

func addSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func delSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func syncSharedGatewayIptRules(services []interface{}, nodeIP *net.IPNet) {
}

func setupLocalNodeAccessBridge(nodeName string, subnets []*net.IPNet) error {
	return nil
}
