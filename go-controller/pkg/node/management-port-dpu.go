package node

import (
	"fmt"
	"net"
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

type managementPortRepresentor struct {
	nodeName    string
	hostSubnets []*net.IPNet
	repName     string
}

// newManagementPortRepresentor creates a new managementPortRepresentor
func newManagementPortRepresentor(nodeName string, hostSubnets []*net.IPNet, rep string) ManagementPort {
	return &managementPortRepresentor{
		nodeName:    nodeName,
		hostSubnets: hostSubnets,
		repName:     rep,
	}
}

func (mp *managementPortRepresentor) Create(_ *routemanager.Controller, node *v1.Node,
	nodeLister listers.NodeLister, kubeInterface kube.Interface, waiter *startupWaiter) (*managementPortConfig, error) {
	k8sMgmtIntfName := types.K8sMgmtIntfName
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		k8sMgmtIntfName += "_0"
	}

	br_type, err := util.GetDatapathType("br-int")
	if err != nil {
		return nil, fmt.Errorf("failed to get datapath type for bridge br-int : %v", err)
	}

	klog.Infof("Lookup representor link and existing management port for '%v'", mp.repName)
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repName)
	if err != nil {
		return nil, err
	}

	if link.Attrs().Name != k8sMgmtIntfName {
		if err := syncMgmtPortInterface(mp.hostSubnets, k8sMgmtIntfName, false); err != nil {
			return nil, fmt.Errorf("failed to check existing management port: %v", err)
		}
	}

	// configure management port: rename, set MTU and set link up and connect representor port to br-int
	klog.Infof("Setup representor management port: %s", link.Attrs().Name)

	setName := link.Attrs().Name != k8sMgmtIntfName
	setMTU := link.Attrs().MTU != config.Default.MTU

	if setName || setMTU {
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			return nil, fmt.Errorf("failed to set link down for device %s. %v", mp.repName, err)
		}

		if setName {
			if err = util.GetNetLinkOps().LinkSetName(link, k8sMgmtIntfName); err != nil {
				return nil, fmt.Errorf("failed to set link name for device %s. %v", mp.repName, err)
			}
		}

		if setMTU {
			if err = util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
				return nil, fmt.Errorf("failed to set link MTU for device %s. %v", link.Attrs().Name, err)
			}
		}
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to set link up for device %s. %v", link.Attrs().Name, err)
	}

	ovsArgs := []string{
		"--", "--may-exist", "add-port", "br-int", k8sMgmtIntfName,
		"--", "set", "interface", k8sMgmtIntfName,
		"external-ids:iface-id=" + types.K8sPrefix + mp.nodeName,
	}
	if mp.repName != k8sMgmtIntfName {
		ovsArgs = append(ovsArgs, "external-ids:ovn-orig-mgmt-port-rep-name="+mp.repName)
	}

	if br_type == types.DatapathUserspace {
		dpdkArgs := []string{"type=dpdk"}
		ovsArgs = append(ovsArgs, dpdkArgs...)
		ovsArgs = append(ovsArgs, fmt.Sprintf("mtu_request=%v", config.Default.MTU))
	}

	// Plug management port representor to OVS.
	stdout, stderr, err := util.RunOVSVsctl(ovsArgs...)
	if err != nil {
		klog.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v",
			k8sMgmtIntfName, stdout, stderr, err)
		return nil, err
	}

	mpcfg := &managementPortConfig{
		ifName: k8sMgmtIntfName,
		link:   link,
	}

	waiter.AddWait(managementPortReady, nil)
	return mpcfg, nil
}

func (mp *managementPortRepresentor) checkRepresentorPortHealth(cfg *managementPortConfig) {
	// After host reboot, management port link name changes back to default name.
	link, err := util.GetNetLinkOps().LinkByName(cfg.ifName)
	if err != nil {
		klog.Errorf("Failed to get link device %s, error: %v", cfg.ifName, err)
		// Get management port representor by name
		link, err := util.GetNetLinkOps().LinkByName(mp.repName)
		if err != nil {
			klog.Errorf("Failed to get link device %s, error: %v", mp.repName, err)
			return
		}
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			klog.Errorf("Failed to set link down for device %s. %v", mp.repName, err)
			return
		}
		if err = util.GetNetLinkOps().LinkSetName(link, cfg.ifName); err != nil {
			klog.Errorf("Rename link from %s to %s failed: %v", mp.repName, cfg.ifName, err)
			return
		}
		if link.Attrs().MTU != config.Default.MTU {
			if err = util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU); err != nil {
				klog.Errorf("Failed to set link MTU for device %s. %v", cfg.ifName, err)
			}
		}
		if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
			klog.Errorf("Failed to set link up for device %s. %v", cfg.ifName, err)
		}
		cfg.link = link
	} else if (link.Attrs().Flags & net.FlagUp) != net.FlagUp {
		if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
			klog.Errorf("Failed to set link up for device %s. %v", cfg.ifName, err)
		}
	}
}

func (mp *managementPortRepresentor) CheckManagementPortHealth(_ *routemanager.Controller, cfg *managementPortConfig, stopChan chan struct{}) {
	go wait.Until(
		func() {
			mp.checkRepresentorPortHealth(cfg)
		},
		5*time.Second,
		stopChan)
}

// Port representors should not have any IP address assignable to them, thus always return false.
func (mp *managementPortRepresentor) HasIpAddr() bool {
	return false
}

type managementPortNetdev struct {
	hostSubnets []*net.IPNet
	netdevName  string
}

// newManagementPortNetdev creates a new managementPortNetdev
func newManagementPortNetdev(hostSubnets []*net.IPNet, netdevName string) ManagementPort {
	return &managementPortNetdev{
		hostSubnets: hostSubnets,
		netdevName:  netdevName,
	}
}

func (mp *managementPortNetdev) Create(routeManager *routemanager.Controller, node *v1.Node,
	nodeLister listers.NodeLister, kubeInterface kube.Interface, waiter *startupWaiter) (*managementPortConfig, error) {
	klog.Infof("Lookup netdevice link and existing management port using '%v'", mp.netdevName)
	link, err := util.GetNetLinkOps().LinkByName(mp.netdevName)
	if err != nil {
		return nil, err
	}

	if link.Attrs().Name != types.K8sMgmtIntfName {
		err = syncMgmtPortInterface(mp.hostSubnets, types.K8sMgmtIntfName, false)
		if err != nil {
			return nil, fmt.Errorf("failed to sync management port: %v", err)
		}
	}

	// configure management port: name, mac, MTU, iptables
	// mac addr, derived from the first entry in host subnets using the .2 address as mac with a fixed prefix.
	klog.Infof("Setup netdevice management port: %s", link.Attrs().Name)
	mgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(mp.hostSubnets[0]).IP)
	setMac := link.Attrs().HardwareAddr.String() != mgmtPortMac.String()
	setName := link.Attrs().Name != types.K8sMgmtIntfName
	setMTU := link.Attrs().MTU != config.Default.MTU

	if setMac || setName || setMTU {
		err := util.GetNetLinkOps().LinkSetDown(link)
		if err != nil {
			return nil, fmt.Errorf("failed to set link down for %s. %v", mp.netdevName, err)
		}

		if setMac {
			err := util.GetNetLinkOps().LinkSetHardwareAddr(link, mgmtPortMac)
			if err != nil {
				return nil, fmt.Errorf("failed to set management port MAC address. %v", err)
			}
		}

		if setName {
			err := util.GetNetLinkOps().LinkSetName(link, types.K8sMgmtIntfName)
			if err != nil {
				return nil, fmt.Errorf("failed to set management port name. %v", err)
			}
		}

		if setMTU {
			err := util.GetNetLinkOps().LinkSetMTU(link, config.Default.MTU)
			if err != nil {
				return nil, fmt.Errorf("failed to set management port MTU. %v", err)
			}
		}
	}

	if mp.netdevName != types.K8sMgmtIntfName && config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// Store original interface name for later use
		if _, stderr, err := util.RunOVSVsctl("set", "Open_vSwitch", ".",
			"external-ids:ovn-orig-mgmt-port-netdev-name="+mp.netdevName); err != nil {
			return nil, fmt.Errorf("failed to store original mgmt port interface name: %s", stderr)
		}
	}

	// Set link up
	err = util.GetNetLinkOps().LinkSetUp(link)
	if err != nil {
		return nil, fmt.Errorf("failed to set link up for %s. %v", types.K8sMgmtIntfName, err)
	}

	// Setup Iptable and routes
	cfg, err := createPlatformManagementPort(routeManager, types.K8sMgmtIntfName, mp.hostSubnets)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (mp *managementPortNetdev) CheckManagementPortHealth(routeManager *routemanager.Controller, cfg *managementPortConfig, stopChan chan struct{}) {
	go wait.Until(
		func() {
			checkManagementPortHealth(routeManager, cfg)
		},
		30*time.Second,
		stopChan)
}

// Management port Netdev should have IP addresses assignable to them.
func (mp *managementPortNetdev) HasIpAddr() bool {
	return true
}
