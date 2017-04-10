package cluster

import (
	"net"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/client-go/tools/cache"
)

type OvnClusterController struct {
	Kube                  kube.KubeInterface
	masterSubnetAllocator *netutils.SubnetAllocator

	KubeServer       string
	CACert           string
	Token            string
	ClusterIPNet     *net.IPNet
	HostSubnetLength uint32

	StartNodeWatch func(handler cache.ResourceEventHandler)
}

const (
	OVN_HOST_SUBNET = "ovn_host_subnet"
)
