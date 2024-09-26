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
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	dhcpLeaseTime = 3500
)

type DHCPConfigsOpt = func(*dhcpConfigs)

type dhcpConfigs struct {
	V4 *nbdb.DHCPOptions
	V6 *nbdb.DHCPOptions
}

func WithIPv4Router(router string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["router"] = router
	}
}

func WithIPv4MTU(mtu int) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["mtu"] = fmt.Sprintf("%d", mtu)
	}
}

func WithIPv4DNSServer(dnsServer string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		if configs.V4 == nil {
			return
		}
		configs.V4.Options["dns_server"] = dnsServer
	}
}

func WithIPv6DNSServer(dnsServer string) func(*dhcpConfigs) {
	return func(configs *dhcpConfigs) {
		// If there is no ipv6 dns server don't configure the option, this is
		// quite common at dual stack envs since a ipv4 dns server can serve
		// ipv6 AAAA records.
		if dnsServer == "" {
			return
		}
		if configs.V6 == nil {
			return
		}
		configs.V6.Options["dns_server"] = dnsServer
	}
}

func EnsureDHCPOptionsForMigratablePod(controllerName string, nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod, ips []*net.IPNet, lsp *nbdb.LogicalSwitchPort) error {
	dnsServerIPv4, dnsServerIPv6, err := RetrieveDNSServiceClusterIPs(watchFactory)
	if err != nil {
		return fmt.Errorf("failed retrieving dns service cluster ip: %v", err)
	}

	return EnsureDHCPOptionsForLSP(controllerName, nbClient, pod, ips, lsp,
		WithIPv4Router(ARPProxyIPv4),
		WithIPv4DNSServer(dnsServerIPv4),
		WithIPv6DNSServer(dnsServerIPv6),
	)
}

func EnsureDHCPOptionsForLSP(controllerName string, nbClient libovsdbclient.Client, pod *corev1.Pod, ips []*net.IPNet, lsp *nbdb.LogicalSwitchPort, opts ...DHCPConfigsOpt) error {
	vmKey := ExtractVMNameFromPod(pod)
	if vmKey == nil {
		return fmt.Errorf("missing vm label at pod %s/%s", pod.Namespace, pod.Name)
	}
	dhcpConfigs, err := composeDHCPConfigs(controllerName, *vmKey, ips, opts...)
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
