package cluster

import (
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openshift/origin/pkg/util/netutils"
)

type namespaceSubnetController struct {
	ClusterConfig

	podSubnetAllocator *netutils.SubnetAllocator
	watchFactory       *factory.WatchFactory
}

// Generic master/node object
func NewNamespaceSubnetController(clusterConfig *ClusterConfig, factory *factory.WatchFactory) OvnClusterController {
	return &nodeSubnetController{
		ClusterConfig: *clusterConfig,
		watchFactory: factory,
	}
}
