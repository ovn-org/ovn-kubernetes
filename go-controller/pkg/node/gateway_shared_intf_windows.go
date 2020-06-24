package node

import (
	kapi "k8s.io/api/core/v1"
	"net"
)

func createNodePortIptableChain() error {
	return nil
}

func deleteNodePortIptableChain() {
}

func addSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func delSharedGatewayIptRules(service *kapi.Service, nodeIP *net.IPNet) {
}

func setupLocalNodeAccessBridge(nodeName string, subnet *net.IPNet) error {
	return nil
}
