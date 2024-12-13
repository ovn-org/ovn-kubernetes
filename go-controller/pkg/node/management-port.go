package node

import (
	"fmt"
	"net"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	listers "k8s.io/client-go/listers/core/v1"

	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ManagementPort is an interface that provides creation and healthcheck for ovn-k8s management port
type ManagementPort interface {
	// Create Management port, use annotator to update node annotation with management port details
	// and waiter to set up condition to wait on for management port creation
	Create(routeManager *routemanager.Controller, node *v1.Node, nodeLister listers.NodeLister, kubeInterface kube.Interface, waiter *startupWaiter) (*managementPortConfig, error)
	// CheckManagementPortHealth checks periodically for management port health until stopChan is posted
	// or closed and reports any warnings/errors to log
	CheckManagementPortHealth(routeManager *routemanager.Controller, cfg *managementPortConfig, stopChan chan struct{})
	// Currently, the management port(s) that doesn't have an assignable IP address are the following cases:
	//   - Full mode with HW backed device (e.g. Virtual Function Representor).
	//   - DPU mode with Virtual Function Representor.
	// It is up to the implementation of the ManagementPort to report whether an IP address can be assigned for the
	// type of ManagementPort.
	HasIpAddr() bool
}

// NewManagementPorts creates a new ManagementPorts
func NewManagementPorts(nodeName string, hostSubnets []*net.IPNet, netdevName, rep string) []ManagementPort {
	// Kubernetes emits events when pods are created. The event will contain
	// only lowercase letters of the hostname even though the kubelet is
	// started with a hostname that contains lowercase and uppercase letters.
	// When the kubelet is started with a hostname containing lowercase and
	// uppercase letters, this causes a mismatch between what the watcher
	// will try to fetch and what kubernetes provides, thus failing to
	// create the port on the logical switch.
	// Until the above is changed, switch to a lowercase hostname
	nodeName = strings.ToLower(nodeName)

	switch config.OvnKubeNode.Mode {
	case types.NodeModeDPU:
		return []ManagementPort{newManagementPortRepresentor(nodeName, hostSubnets, rep)}
	case types.NodeModeDPUHost:
		return []ManagementPort{newManagementPortNetdev(hostSubnets, netdevName)}
	default:
		// create OVS internal port or configure netdevice and its representor
		if config.OvnKubeNode.MgmtPortNetdev == "" {
			return []ManagementPort{newManagementPort(nodeName, hostSubnets)}
		} else {
			return []ManagementPort{
				newManagementPortNetdev(hostSubnets, netdevName),
				newManagementPortRepresentor(nodeName, hostSubnets, rep),
			}
		}
	}
}

type managementPort struct {
	nodeName    string
	hostSubnets []*net.IPNet
}

// newManagementPort creates a new newManagementPort
func newManagementPort(nodeName string, hostSubnets []*net.IPNet) ManagementPort {
	return &managementPort{
		nodeName:    nodeName,
		hostSubnets: hostSubnets,
	}
}

func (mp *managementPort) Create(routeManager *routemanager.Controller, node *v1.Node,
	nodeLister listers.NodeLister, kubeInterface kube.Interface, waiter *startupWaiter) (*managementPortConfig, error) {
	for _, mgmtPortName := range []string{types.K8sMgmtIntfName, types.K8sMgmtIntfName + "_0"} {
		if err := syncMgmtPortInterface(mp.hostSubnets, mgmtPortName, true); err != nil {
			return nil, fmt.Errorf("failed to sync management port: %v", err)
		}
	}

	var err error
	var macAddr net.HardwareAddr
	// find suitable MAC address
	// check node annotation first, to ensure we are not picking a new MAC when one was already configured
	if macAddr, err = util.ParseNodeManagementPortMACAddresses(node, types.DefaultNetworkName); err != nil && !util.IsAnnotationNotSetError(err) {
		return nil, err
	}
	if len(macAddr) == 0 {
		// calculate mac from subnets
		if len(mp.hostSubnets) == 0 {
			return nil, fmt.Errorf("cannot determine subnets while configuring management port for network: %s", types.DefaultNetworkName)
		}
		macAddr = util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(mp.hostSubnets[0]).IP)
	}

	// Create a OVS internal interface.
	legacyMgmtIntfName := util.GetLegacyK8sMgmtIntfName(mp.nodeName)
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", legacyMgmtIntfName,
		"--", "--may-exist", "add-port", "br-int", types.K8sMgmtIntfName,
		"--", "set", "interface", types.K8sMgmtIntfName, fmt.Sprintf("mac=\"%s\"", macAddr.String()),
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id="+types.K8sPrefix+mp.nodeName)
	if err != nil {
		klog.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return nil, err
	}

	cfg, err := createPlatformManagementPort(routeManager, types.K8sMgmtIntfName, mp.hostSubnets)
	if err != nil {
		return nil, err
	}

	waiter.AddWait(managementPortReady, nil)
	return cfg, nil
}

func (mp *managementPort) CheckManagementPortHealth(routeManager *routemanager.Controller, cfg *managementPortConfig, stopChan chan struct{}) {
	go wait.Until(
		func() {
			checkManagementPortHealth(routeManager, cfg)
		},
		30*time.Second,
		stopChan)
}

// OVS Internal Port Netdev should have IP addresses assignable to them.
func (mp *managementPort) HasIpAddr() bool {
	return true
}

func managementPortReady() (bool, error) {
	k8sMgmtIntfName := types.K8sMgmtIntfName
	if config.OvnKubeNode.MgmtPortNetdev != "" {
		k8sMgmtIntfName += "_0"
	}
	// Get the OVS interface name for the Management Port
	ofport, _, err := util.RunOVSVsctl("--if-exists", "get", "interface", k8sMgmtIntfName, "ofport")
	if err != nil {
		return false, nil
	}

	// OpenFlow table 65 performs logical-to-physical translation. It matches the packet’s logical
	// egress  port. Its actions output the packet to the port attached to the OVN integration bridge
	// that represents that logical  port.
	stdout, _, err := util.RunOVSOfctl("--no-stats", "--no-names", "dump-flows", "br-int",
		"table=65,out_port="+ofport)
	if err != nil {
		return false, nil
	}
	if !strings.Contains(stdout, "actions=output:"+ofport) {
		return false, nil
	}
	klog.Infof("Management port %s is ready", k8sMgmtIntfName)
	return true, nil
}
