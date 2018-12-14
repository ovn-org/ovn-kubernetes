package cluster

import (
	"fmt"
	"net"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/kubernetes"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                      kube.Interface
	watchFactory              *factory.WatchFactory
	masterSubnetAllocatorList []*netutils.SubnetAllocator

	ClusterServicesSubnet string
	ClusterIPNet          []CIDRNetworkEntry

	GatewayInit      bool
	GatewayIntf      string
	GatewayBridge    string
	GatewayNextHop   string
	GatewaySpareIntf bool
	NodePortEnable   bool
	OvnHA            bool
	LocalnetGateway  bool
}

// CIDRNetworkEntry is the object that holds the definition for a single network CIDR range
type CIDRNetworkEntry struct {
	CIDR             *net.IPNet
	HostSubnetLength uint32
}

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"
	// DefaultNamespace is the name of the default namespace
	DefaultNamespace = "default"
	// MasterOverlayIP is the overlay IP address on master node
	MasterOverlayIP = "master_overlay_ip"
	// OvnClusterRouter is the name of the distributed router
	OvnClusterRouter = "ovn_cluster_router"
)

// NewClusterController creates a new controller for IP subnet allocation to
// a given resource type (either Namespace or Node)
func NewClusterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *OvnClusterController {
	return &OvnClusterController{
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
	}
}

func setupOVNNode(nodeName string) error {
	// Tell ovn-*bctl how to talk to the database
	for _, auth := range []*config.OvnDBAuth{
		config.OvnNorth.ClientAuth,
		config.OvnSouth.ClientAuth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	var err error

	nodeIP := config.Default.EncapIP
	if nodeIP == "" {
		nodeIP, err = netutils.GetNodeIP(nodeName)
		if err != nil {
			return fmt.Errorf("failed to obtain local IP from hostname %q: %v", nodeName, err)
		}
	} else {
		if ip := net.ParseIP(nodeIP); ip == nil {
			return fmt.Errorf("invalid encapsulation IP provided %q", nodeIP)
		}
	}

	_, stderr, err := util.RunOVSVsctl("set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
	)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}
	return nil
}

func setupOVNMaster(nodeName string) error {
	// Configure both server and client of OVN databases, since master uses both
	for _, auth := range []*config.OvnDBAuth{
		config.OvnNorth.ServerAuth,
		config.OvnNorth.ClientAuth,
		config.OvnSouth.ServerAuth,
		config.OvnSouth.ClientAuth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}
	return nil
}
