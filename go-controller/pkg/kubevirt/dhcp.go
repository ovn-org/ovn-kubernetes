// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	dhcpLeaseTime = 3500
)

// DHCPConfigsOpt mutates generated DHCP options before they are written to OVN.
type DHCPConfigsOpt = func(*dhcpConfigs)

type dhcpConfigs struct {
	V4 *nbdb.DHCPOptions
	V6 *nbdb.DHCPOptions
}

// WithIPv4Router configures the IPv4 router DHCP option.
func WithIPv4Router(router string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["router"] = router
	}
}

// WithIPv4MTU configures the IPv4 MTU DHCP option.
func WithIPv4MTU(mtu int) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["mtu"] = fmt.Sprintf("%d", mtu)
	}
}

// WithIPv4DNSServer configures the IPv4 DNS server DHCP option.
func WithIPv4DNSServer(dnsServer string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["dns_server"] = dnsServer
	}
}

// WithIPv6DNSServer configures the IPv6 DNS server DHCP option.
func WithIPv6DNSServer(dnsServer string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		// If there is no IPv6 DNS server, don't configure the option. This is
		// common in dual-stack environments since an IPv4 DNS server can serve
		// IPv6 AAAA records.
		if dnsServer == "" {
			return
		}
		if configs.V6 == nil {
			return
		}
		configs.V6.Options["dns_server"] = dnsServer
	}
}

// EnsureDHCPOptionsForLSP creates or updates DHCP options for a VM logical switch port.
func EnsureDHCPOptionsForLSP(controllerName string, nbClient libovsdbclient.Client, pod *corev1.Pod, ips []*net.IPNet, lsp *nbdb.LogicalSwitchPort, opts ...DHCPConfigsOpt) error {
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return fmt.Errorf("failed discovering vm description at pod %s/%s:%w", pod.Namespace, pod.Name, err)
	}
	if vmDescription == nil {
		return fmt.Errorf("missing vm label at pod %s/%s", pod.Namespace, pod.Name)
	}
	dhcpConfigs, err := composeDHCPConfigs(controllerName, vmDescription.Key(), ips, opts...)
	if err != nil {
		return fmt.Errorf("failed composing DHCP options: %v", err)
	}
	err = libovsdbops.CreateOrUpdateDhcpOptions(nbClient, lsp, dhcpConfigs.V4, dhcpConfigs.V6)
	if err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
}

func composeDHCPConfigs(controllerName string, vmKey ktypes.NamespacedName, podIPs []*net.IPNet, opts ...DHCPConfigsOpt) (*dhcpConfigs, error) {
	if len(podIPs) == 0 {
		return nil, fmt.Errorf("missing podIPs to compose dhcp options")
	}
	if vmKey.Name == "" {
		return nil, fmt.Errorf("missing vmName to compose dhcp options")
	}

	dhcpConfigs := &dhcpConfigs{}
	for _, ip := range podIPs {
		_, cidr, err := net.ParseCIDR(ip.String())
		if err != nil {
			return nil, fmt.Errorf("failed converting podIPs to cidr to configure dhcp: %v", err)
		}
		if utilnet.IsIPv4CIDR(cidr) {
			dhcpConfigs.V4 = ComposeDHCPv4Options(cidr.String(), controllerName, vmKey)
		} else if utilnet.IsIPv6CIDR(cidr) {
			dhcpConfigs.V6 = ComposeDHCPv6Options(cidr.String(), controllerName, vmKey)
		}
	}
	for _, opt := range opts {
		opt(dhcpConfigs)
	}
	return dhcpConfigs, nil
}

// RetrieveDNSServiceClusterIPs returns IPv4 and IPv6 cluster IPs for the configured DNS service.
func RetrieveDNSServiceClusterIPs(k8scli *factory.WatchFactory) (string, string, error) {
	dnsServer, err := k8scli.GetService(config.Kubernetes.DNSServiceNamespace, config.Kubernetes.DNSServiceName)
	if err != nil {
		return "", "", err
	}
	clusterIPv4 := ""
	clusterIPv6 := ""
	for _, clusterIP := range dnsServer.Spec.ClusterIPs {
		if utilnet.IsIPv4String(clusterIP) {
			clusterIPv4 = clusterIP
		} else if utilnet.IsIPv6String(clusterIP) {
			clusterIPv6 = clusterIP
		}
	}
	return clusterIPv4, clusterIPv6, nil
}

// ComposeDHCPv4Options builds OVN DHCPv4 options for a VM pod CIDR.
func ComposeDHCPv4Options(cidr, controllerName string, vmKey ktypes.NamespacedName) *nbdb.DHCPOptions {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv4)).String()
	dhcpOptions := &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"lease_time": fmt.Sprintf("%d", dhcpLeaseTime),
			"server_id":  ARPProxyIPv4,
			"server_mac": serverMAC,
			"hostname":   fmt.Sprintf("%q", vmKey.Name),
		},
	}
	return composeDHCPOptions(controllerName, vmKey, dhcpOptions)
}

// ComposeDHCPv6Options builds OVN DHCPv6 options for a VM pod CIDR.
func ComposeDHCPv6Options(cidr, controllerName string, vmKey ktypes.NamespacedName) *nbdb.DHCPOptions {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv6)).String()
	dhcpOptions := &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"server_id": serverMAC,
			"fqdn":      fmt.Sprintf("%q", vmKey.Name), // equivalent to ipv4 "hostname" option
		},
	}
	return composeDHCPOptions(controllerName, vmKey, dhcpOptions)
}

func composeDHCPOptions(controllerName string, vmKey ktypes.NamespacedName, dhcpOptions *nbdb.DHCPOptions) *nbdb.DHCPOptions {
	dhcpvOptionsDbObjectID := libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: vmKey.String(),
			libovsdbops.CIDRKey:       libovsdbops.BuildCIDRKey(dhcpOptions.Cidr),
		})
	dhcpOptions.ExternalIDs = dhcpvOptionsDbObjectID.GetExternalIDs()
	dhcpOptions.ExternalIDs[OvnZoneExternalIDKey] = OvnLocalZone
	return dhcpOptions
}

// DeleteDHCPOptions deletes OVN DHCP options owned by the VM associated with pod.
func DeleteDHCPOptions(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	return DeleteDHCPOptionsWithZone(nbClient, pod, "")
}

// DeleteDHCPOptionsWithZone deletes OVN DHCP options owned by the VM associated
// with pod for the specified OVN zone. If zone is empty, all zones are deleted.
func DeleteDHCPOptionsWithZone(nbClient libovsdbclient.Client, pod *corev1.Pod, zone string) error {
	vmDescription, err := NewVMDescriptionFromPod(pod)
	if err != nil {
		return fmt.Errorf("failed discovering vm description at pod %s/%s:%w", pod.Namespace, pod.Name, err)
	}
	if vmDescription == nil {
		return nil
	}
	if err := libovsdbops.DeleteDHCPOptionsWithPredicate(nbClient, func(item *nbdb.DHCPOptions) bool {
		if item.ExternalIDs[string(libovsdbops.ObjectNameKey)] != vmDescription.Key().String() {
			return false
		}
		return zone == "" || item.ExternalIDs[OvnZoneExternalIDKey] == zone
	}); err != nil {
		return err
	}
	return nil
}
