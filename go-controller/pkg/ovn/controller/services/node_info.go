// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	globalconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type nodeInfo struct {
	// the node's Name
	name string
	// The list of physical IPs reported by the gatewayconf annotation
	l3gatewayAddresses []net.IP
	// The list of physical IPs and subnet masks the node has, as reported by the host-cidrs annotation
	hostAddresses []net.IP
	// The pod network subnet(s)
	podSubnets []net.IPNet
	// the name of the node's GatewayRouter, or "" of non-existent
	gatewayRouterName string
	// The name of the node's switch - never empty
	switchName string
	// The chassisID of the node (ovs.external-ids:system-id)
	chassisID string
	// if nodePort is disabled on this node?
	nodePortDisabled bool

	// The Kubernetes availability zone from the topology.kubernetes.io/zone label
	topologyZone string

	// The list of node's management IPs
	mgmtIPs []net.IP
}

func (ni *nodeInfo) hostAddressesStr() []string {
	out := make([]string, 0, len(ni.hostAddresses))
	for _, ip := range ni.hostAddresses {
		out = append(out, ip.String())
	}
	return out
}

func (ni *nodeInfo) l3gatewayAddressesStr() []string {
	out := make([]string, 0, len(ni.l3gatewayAddresses))
	for _, ip := range ni.l3gatewayAddresses {
		out = append(out, ip.String())
	}
	return out
}

func nodeInfoForNetwork(node *corev1.Node, netInfo util.NetInfo) (*nodeInfo, error) {
	var hsn []*net.IPNet
	var err error
	if netInfo.TopologyType() == types.Layer2Topology {
		for _, subnet := range netInfo.Subnets() {
			hsn = append(hsn, subnet.CIDR)
		}
	} else {
		hsn, err = util.ParseNodeHostSubnetAnnotation(node, netInfo.GetNetworkName())
	}
	if err != nil || hsn == nil || util.NoHostSubnet(node) {
		return nil, err
	}

	switchName := netInfo.GetNetworkScopedSwitchName(node.Name)
	grName := ""
	l3gatewayAddresses := []net.IP{}
	chassisID := ""
	nodePortEnabled := false

	// if the node has a gateway config, it will soon have a gateway router
	// so, set the router name
	gwConf, err := util.ParseNodeL3GatewayAnnotation(node)
	if err != nil || gwConf == nil {
		klog.Infof("Node %s has invalid / no gateway config: %v", node.Name, err)
	} else if gwConf.Mode != globalconfig.GatewayModeDisabled {
		grName = netInfo.GetNetworkScopedGWRouterName(node.Name)
		// L3 GW IP addresses are not network-specific, we can take them from the default L3 GW annotation
		for _, ip := range gwConf.IPAddresses {
			l3gatewayAddresses = append(l3gatewayAddresses, ip.IP)
		}
		nodePortEnabled = gwConf.NodePortEnable
		chassisID = gwConf.ChassisID
	}
	hostAddresses, err := util.GetNodeHostAddrs(node)
	if err != nil {
		klog.Warningf("Failed to get node host CIDRs for [%s]: %s", node.Name, err.Error())
	}

	hostAddressesIPs := make([]net.IP, 0, len(hostAddresses))
	for _, ipStr := range hostAddresses {
		ip := net.ParseIP(ipStr)
		hostAddressesIPs = append(hostAddressesIPs, ip)
	}

	mgmtIPs := make([]net.IP, 0, len(hsn))
	for _, hostSubnet := range hsn {
		mgmtIPs = append(mgmtIPs, netInfo.GetNodeManagementIP(hostSubnet).IP)
	}

	ni := &nodeInfo{
		name:               node.Name,
		l3gatewayAddresses: l3gatewayAddresses,
		hostAddresses:      hostAddressesIPs,
		podSubnets:         make([]net.IPNet, 0, len(hsn)),
		mgmtIPs:            mgmtIPs,
		gatewayRouterName:  grName,
		switchName:         switchName,
		chassisID:          chassisID,
		nodePortDisabled:   !nodePortEnabled,
		topologyZone:       node.Labels[corev1.LabelTopologyZone],
	}
	for i := range hsn {
		ni.podSubnets = append(ni.podSubnets, *hsn[i])
	}
	return ni, nil
}
