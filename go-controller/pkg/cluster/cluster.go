package cluster

import (
	"fmt"
	"net"
	"net/url"
	"os/exec"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/client-go/kubernetes"
)

// OvnClusterController is the object holder for utilities meant for cluster management
type OvnClusterController struct {
	Kube                  kube.Interface
	watchFactory          *factory.WatchFactory
	masterSubnetAllocator *netutils.SubnetAllocator

	ClusterIPNet          *net.IPNet
	ClusterServicesSubnet string
	HostSubnetLength      uint32

	GatewayInit      bool
	GatewayIntf      string
	GatewayBridge    string
	GatewayNextHop   string
	GatewaySpareIntf bool
	NodePortEnable   bool
}

const (
	// OvnHostSubnet is the constant string representing the annotation key
	OvnHostSubnet = "ovn_host_subnet"
)

// NewClusterController creates a new controller for IP subnet allocation to
// a given resource type (either Namespace or Node)
func NewClusterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory) *OvnClusterController {
	return &OvnClusterController{
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
	}
}

func setupOVN(nodeName, kubeServer, kubeToken string) error {
	if _, err := url.Parse(kubeServer); err != nil {
		return fmt.Errorf("error parsing k8s server %q: %v", kubeServer, err)
	}

	nodeIP, err := netutils.GetNodeIP(nodeName)
	if err != nil {
		return fmt.Errorf("Failed to obtain local IP: %v", err)
	}

	args := []string{
		"set",
		"Open_vSwitch",
		".",
		"external_ids:ovn-encap-type=geneve",
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		fmt.Sprintf("external_ids:k8s-api-server=\"%s\"", kubeServer),
		fmt.Sprintf("external_ids:k8s-api-token=\"%s\"", kubeToken),
	}
	if config.OvnNorth.ClientAuth.Scheme != config.OvnDBSchemeUnix {
		args = append(args, fmt.Sprintf("external_ids:ovn-nb=\"%s\"", config.OvnNorth.ClientAuth.GetURL()))
	}
	if config.OvnSouth.ClientAuth.Scheme != config.OvnDBSchemeUnix {
		args = append(args, fmt.Sprintf("external_ids:ovn-remote=\"%s\"", config.OvnSouth.ClientAuth.GetURL()))
	}

	out, err := exec.Command("ovs-vsctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("Error setting OVS external IDs: %v\n  %q", err, string(out))
	}

	return nil
}
