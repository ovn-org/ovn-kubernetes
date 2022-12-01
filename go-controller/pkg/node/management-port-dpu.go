package node

import (
	"fmt"
	"net"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managementPortRepresentor struct {
	nodeName    string
	hostSubnets []*net.IPNet
	repName     string
}

// newManagementPortRepresentor creates a new managementPortRepresentor
func newManagementPortRepresentor(nodeName string, hostSubnets []*net.IPNet) ManagementPort {
	var repName string

	// In ovnkube-node mode DPU representor name stored in MgmtPortNetdev variable
	if config.OvnKubeNode.MgmtPortRepresentor == "" {
		repName = config.OvnKubeNode.MgmtPortNetdev
	} else {
		repName = config.OvnKubeNode.MgmtPortRepresentor
	}
	return &managementPortRepresentor{
		nodeName:    nodeName,
		hostSubnets: hostSubnets,
		repName:     repName,
	}
}

func (mp *managementPortRepresentor) Create(nodeAnnotator kube.Annotator, waiter *startupWaiter) (*managementPortConfig, error) {
	k8sMgmtIntfName := types.K8sMgmtIntfName
	if config.OvnKubeNode.MgmtPortRepresentor != "" {
		k8sMgmtIntfName += "_0"
	}
	// Get management port representor netdevice
	link, err := util.GetNetLinkOps().LinkByName(mp.repName)
	if err != nil {
		// It may fail in case this is not the first run after reboot and management port has already been renamed.
		link, err = util.GetNetLinkOps().LinkByName(k8sMgmtIntfName)
		if err != nil {
			return nil, fmt.Errorf("failed to get link device for %s. %v", mp.repName, err)
		}
	}

	// configure management port: rename, set MTU and set link up and connect representor port to br-int
	klog.Infof("Create representor management port: %s", link.Attrs().Name)
	setName := link.Attrs().Name != k8sMgmtIntfName
	setMTU := link.Attrs().MTU != config.Default.MTU

	if setName || setMTU {
		if err = util.GetNetLinkOps().LinkSetDown(link); err != nil {
			return nil, fmt.Errorf("failed to set link down for device %s. %v", mp.repName, err)
		}

		if setName {
			if err = util.GetNetLinkOps().LinkSetName(link, k8sMgmtIntfName); err != nil {
				// NOTE(adrianc): rename may fail with "file exists" in case an interface is already named
				// ovn-k8s-mp*, this may happen if mgmt-port-netdev changes during deployment. ATM we are
				// not handling it.
				// TODO: handle mgmt-port-netdev change.
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

	// Plug management port representor to OVS.
	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--may-exist", "add-port", "br-int", k8sMgmtIntfName,
		"--", "set", "interface", k8sMgmtIntfName,
		"external-ids:iface-id="+types.K8sPrefix+mp.nodeName)
	if err != nil {
		klog.Errorf("Failed to add port %q to br-int, stdout: %q, stderr: %q, error: %v",
			k8sMgmtIntfName, stdout, stderr, err)
		return nil, err
	}

	mpcfg := &managementPortConfig{
		ifName: k8sMgmtIntfName,
		link:   link,
	}

	mgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(mp.hostSubnets[0]).IP)
	if err := util.SetNodeManagementPortMACAddress(nodeAnnotator, mgmtPortMac); err != nil {
		return nil, err
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

func (mp *managementPortRepresentor) CheckManagementPortHealth(cfg *managementPortConfig, stopChan chan struct{}) {
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
func newManagementPortNetdev(hostSubnets []*net.IPNet) ManagementPort {
	return &managementPortNetdev{
		hostSubnets: hostSubnets,
		netdevName:  config.OvnKubeNode.MgmtPortNetdev,
	}
}

func (mp *managementPortNetdev) Create(nodeAnnotator kube.Annotator, waiter *startupWaiter) (*managementPortConfig, error) {
	// get Netdev that is used for management port.
	link, err := util.GetNetLinkOps().LinkByName(mp.netdevName)
	if err != nil {
		// this may not the first time invoked on the node after reboot
		// netdev may have already been renamed to ovn-k8s-mp0.
		link, err = util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return nil, fmt.Errorf("failed to get link device for %s. %v", mp.netdevName, err)
		}
	}

	// configure management port: name, mac, MTU, iptables
	// mac addr, derived from the first entry in host subnets using the .2 address as mac with a fixed prefix.
	klog.Infof("Setup management port dpu host: %s", link.Attrs().Name)
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
				// NOTE(adrianc): rename may fail with "file exists" in case an interface is already named
				// ovn-k8s-mp0, this may happen if mgmt-port-netdev changes during deployment. ATM we are
				// not handling it.
				// TODO: handle mgmt-port-netdev change.
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

	// Set link up
	err = util.GetNetLinkOps().LinkSetUp(link)
	if err != nil {
		return nil, fmt.Errorf("failed to set link up for %s. %v", types.K8sMgmtIntfName, err)
	}

	// Setup Iptable and routes
	cfg, err := createPlatformManagementPort(types.K8sMgmtIntfName, mp.hostSubnets)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (mp *managementPortNetdev) CheckManagementPortHealth(cfg *managementPortConfig, stopChan chan struct{}) {
	go wait.Until(
		func() {
			checkManagementPortHealth(cfg)
		},
		30*time.Second,
		stopChan)
}

// Management port Netdev should have IP addresses assignable to them.
func (mp *managementPortNetdev) HasIpAddr() bool {
	return true
}
