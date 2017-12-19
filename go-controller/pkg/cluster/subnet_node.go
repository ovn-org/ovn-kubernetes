package cluster

import (
	"net"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openshift/origin/pkg/util/netutils"
)

type GatewayConfig struct {
	GatewayInit      bool
	GatewayIntf      string
	GatewayBridge    string
	GatewayNextHop   string
	GatewaySpareIntf bool
	NodePortEnable   bool
}

type nodeSubnetController struct {
	ClusterConfig
	GatewayConfig

	podSubnetAllocator *netutils.SubnetAllocator
	nodeSubnet         *net.IPNet
	gatewayConfig      GatewayConfig
	watchFactory       *factory.WatchFactory
}

// Generic master/node object
func NewNodeSubnetController(clusterConfig *ClusterConfig, gatewayConfig *GatewayConfig, factory *factory.WatchFactory) OvnClusterController {
	return &nodeSubnetController{
		ClusterConfig: *clusterConfig,
		GatewayConfig: *gatewayConfig,
		watchFactory: factory,
	}
}
