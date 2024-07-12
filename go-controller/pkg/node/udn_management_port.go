package node

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

type udnManagementPortConfig struct {
	managementPortConfig
}

func (nc *SecondaryNodeNetworkController) newUDNManagementPortConfig(nodeAnnotator kube.Annotator) (*udnManagementPortConfig, error) {
	var err error
	interfaceName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(nc.networkID))
	//TODO; handle returned config, configure ip address
	node, err := nc.watchFactory.GetNode(nc.name)
	if err != nil {
		return nil, err
	}
	networkLocalSubnets, err := util.ParseNodeHostSubnetAnnotation(node, nc.GetNetworkName())
	if err != nil {
		klog.Infof("Waiting for node %s to start, no annotation found on node for subnet: %v", nc.name, err)
		return nil, err
	}

	mpcfg := &udnManagementPortConfig{
		managementPortConfig{
			ifName: interfaceName,
		},
	}

	stdout, stderr, err := util.RunOVSVsctl(
		"--", "--if-exists", "del-port", "br-int", mpcfg.ifName,
		"--", "--may-exist", "add-port", "br-int", mpcfg.ifName,
		"--", "set", "interface", mpcfg.ifName,
		"type=internal", "mtu_request="+fmt.Sprintf("%d", config.Default.MTU),
		"external-ids:iface-id="+nc.GetNetworkScopedK8sMgmtIntfName(nc.name))
	if err != nil {
		klog.Errorf("Failed to add port to br-int, stdout: %q, stderr: %q, error: %v", stdout, stderr, err)
		return nil, err
	}
	macAddress, err := util.GetOVSPortMACAddress(interfaceName)
	if err != nil {
		klog.Errorf("Failed to get management port MAC address: %v", err)
		return nil, err
	}
	// persist the MAC address so that upon node reboot we get back the same mac address.
	_, stderr, err = util.RunOVSVsctl("set", "interface", interfaceName,
		fmt.Sprintf("mac=%s", strings.ReplaceAll(macAddress.String(), ":", "\\:")))
	if err != nil {
		klog.Errorf("Failed to persist MAC address %q for %q: stderr:%s (%v)", macAddress.String(),
			interfaceName, stderr, err)
		return nil, err
	}
	if mpcfg.link, err = util.LinkSetUp(mpcfg.ifName); err != nil {
		return nil, err
	}
	for _, subnet := range networkLocalSubnets {
		ip := util.GetNodeManagementIfAddr(subnet)
		var err error
		var exists bool
		if exists, err = util.LinkAddrExist(mpcfg.link, ip); err == nil && !exists {
			err = util.LinkAddrAdd(mpcfg.link, ip, 0, 0, 0)
		}
		if err != nil {
			return nil, err
		}
	}
	klog.Infof("SURYA %v/%v/%v", node.Name, macAddress, nc.GetNetworkName())
	if err := util.UpdateNodeManagementPortMACAddresses(node, nodeAnnotator, macAddress, nc.GetNetworkName()); err != nil {
		klog.Infof("SURYA %v", err)
		return nil, err
	}
	klog.Infof("SURYA %v", err)
	return mpcfg, nil
}
