package node

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"
)

type userNetManagementPortConfig struct {
	ifName string
	link   netlink.Link
}

func newUserNetManagementPortConfig(interfaceName string, ips []*net.IPNet) (*userNetManagementPortConfig, error) {
	var err error

	mpcfg := &userNetManagementPortConfig{
		ifName: interfaceName,
	}

	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", mpcfg.ifName,
		"--", "--may-exist", "add-port", "br-int", mpcfg.ifName,
		"--", "set", "interface", mpcfg.ifName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id="+types.K8sPrefix+mpcfg.ifName)
	if err != nil {
		klog.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return nil, err
	}
	macAddress, err := util.GetOVSPortMACAddress(types.K8sMgmtIntfName)
	if err != nil {
		klog.Errorf("Failed to get management port MAC address: %v", err)
		return nil, err
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", types.K8sMgmtIntfName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress.String(), ":", "\\:")))
	if err != nil {
		klog.Errorf("Failed to persist MAC address %q for %q: stderr:%s (%v)", macAddress.String(),
			types.K8sMgmtIntfName, stderr, err)
		return nil, err
	}

	if mpcfg.link, err = util.LinkSetUp(mpcfg.ifName); err != nil {
		return nil, err
	}

	for _, ip := range ips {
		var err error
		var exists bool
		if exists, err = util.LinkAddrExist(mpcfg.link, ip); err == nil && !exists {
			err = util.LinkAddrAdd(mpcfg.link, ip, 0, 0, 0)
		}
		if err != nil {
			return nil, err
		}
	}

	return mpcfg, nil
}
