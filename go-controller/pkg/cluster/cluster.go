package cluster

import (
	"net"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/client-go/tools/cache"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                  kube.Interface
	masterSubnetAllocator *netutils.SubnetAllocator

	KubeServer       string
	CACert           string
	Token            string
	ClusterIPNet     *net.IPNet
	HostSubnetLength uint32

	StartNodeWatch func(handler cache.ResourceEventHandler)
}

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"
)
