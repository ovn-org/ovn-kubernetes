package kubevirt

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	dhcpLeaseTime = 3500
)

type dhcpConfigs struct {
	V4 *nbdb.DHCPOptions
	V6 *nbdb.DHCPOptions
}

func EnsureDHCPOptionsForMigratablePod(controllerName string, nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod, ips []*net.IPNet, lsp *nbdb.LogicalSwitchPort) error {
	vmKey := ExtractVMNameFromPod(pod)
	if vmKey == nil {
		return fmt.Errorf("missing vm label at pod %s/%s", pod.Namespace, pod.Name)
	}
	dhcpConfigs, err := composeDHCPConfigs(watchFactory, controllerName, *vmKey, ips)
	if err != nil {
		return fmt.Errorf("failed composing DHCP options: %v", err)
	}
	err = libovsdbops.CreateOrUpdateDhcpOptions(nbClient, lsp, dhcpConfigs.V4, dhcpConfigs.V6)
	if err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
}

func composeDHCPConfigs(k8scli *factory.WatchFactory, controllerName string, vmKey ktypes.NamespacedName, podIPs []*net.IPNet) (*dhcpConfigs, error) {
	if len(podIPs) == 0 {
		return nil, fmt.Errorf("missing podIPs to compose dhcp options")
	}
	if vmKey.Name == "" {
		return nil, fmt.Errorf("missing vmName to compose dhcp options")
	}

	dnsServerIPv4, dnsServerIPv6, err := retrieveDNSServiceClusterIPs(k8scli)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving dns service cluster ip: %v", err)
	}

	dhcpConfigs := &dhcpConfigs{}
	for _, ip := range podIPs {
		_, cidr, err := net.ParseCIDR(ip.String())
		if err != nil {
			return nil, fmt.Errorf("failed converting podIPs to cidr to configure dhcp: %v", err)
		}
		if utilnet.IsIPv4CIDR(cidr) {
			dhcpConfigs.V4 = ComposeDHCPv4Options(cidr.String(), dnsServerIPv4, controllerName, vmKey)
		} else if utilnet.IsIPv6CIDR(cidr) {
			dhcpConfigs.V6 = ComposeDHCPv6Options(cidr.String(), dnsServerIPv6, controllerName, vmKey)
		}
	}
	return dhcpConfigs, nil
}

func retrieveDNSServiceClusterIPs(k8scli *factory.WatchFactory) (string, string, error) {
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

func ComposeDHCPv4Options(cidr, dnsServer, controllerName string, vmKey ktypes.NamespacedName) *nbdb.DHCPOptions {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv4)).String()
	dhcpOptions := &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"lease_time": fmt.Sprintf("%d", dhcpLeaseTime),
			"router":     ARPProxyIPv4,
			"dns_server": dnsServer,
			"server_id":  ARPProxyIPv4,
			"server_mac": serverMAC,
			"hostname":   fmt.Sprintf("%q", vmKey.Name),
		},
	}
	return composeDHCPOptions(controllerName, vmKey, dhcpOptions)
}

func ComposeDHCPv6Options(cidr, dnsServer, controllerName string, vmKey ktypes.NamespacedName) *nbdb.DHCPOptions {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv6)).String()
	dhcpOptions := &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"server_id": serverMAC,
		},
	}
	if dnsServer != "" {
		dhcpOptions.Options["dns_server"] = dnsServer
	}
	return composeDHCPOptions(controllerName, vmKey, dhcpOptions)
}

func composeDHCPOptions(controllerName string, vmKey ktypes.NamespacedName, dhcpOptions *nbdb.DHCPOptions) *nbdb.DHCPOptions {
	dhcpvOptionsDbObjectID := libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: vmKey.String(),
			libovsdbops.CIDRKey:       strings.ReplaceAll(dhcpOptions.Cidr, ":", "."),
		})
	dhcpOptions.ExternalIDs = dhcpvOptionsDbObjectID.GetExternalIDs()
	dhcpOptions.ExternalIDs[OvnZoneExternalIDKey] = OvnLocalZone
	return dhcpOptions
}

func DeleteDHCPOptions(nbClient libovsdbclient.Client, pod *corev1.Pod) error {
	vmKey := ExtractVMNameFromPod(pod)
	if vmKey == nil {
		return nil
	}
	if err := libovsdbops.DeleteDHCPOptionsWithPredicate(nbClient, func(item *nbdb.DHCPOptions) bool {
		return item.ExternalIDs[string(libovsdbops.ObjectNameKey)] == vmKey.String()
	}); err != nil {
		return err
	}
	return nil
}
