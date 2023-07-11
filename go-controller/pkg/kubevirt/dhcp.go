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

type Option func(option *nbdb.DHCPOptions)

type DHCPOptionRequest struct {
	Cidr    net.IPNet
	Options []Option
}

func newDHCPOptionsForVM(controllerName string, vmName ktypes.NamespacedName, cidr *net.IPNet, opts ...Option) *nbdb.DHCPOptions {
	serverID := ARPProxyIPv4
	if utilnet.IsIPv6CIDR(cidr) {
		serverID = ARPProxyIPv6
	}
	dhcpOption := initDHCPOptions(
		controllerName,
		vmName,
		&nbdb.DHCPOptions{
			Cidr:    cidr.String(),
			Options: mandatoryDHCPOptions(serverID),
		},
	)
	for _, opt := range opts {
		opt(dhcpOption)
	}
	return dhcpOption
}

func mandatoryDHCPOptions(serverIP string) map[string]string {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(ARPProxyIPv4)).String()
	return map[string]string{
		"server_id":  serverIP,
		"server_mac": serverMAC,
		"lease_time": fmt.Sprintf("%d", dhcpLeaseTime),
	}
}

func WithDNSServer(serverIP string) Option {
	return func(option *nbdb.DHCPOptions) {
		option.Options["dns_server"] = serverIP
	}
}

func WithGateway(gwIP string) Option {
	return func(option *nbdb.DHCPOptions) {
		option.Options["router"] = gwIP
	}
}

func WithMTU(mtu int) Option {
	return func(option *nbdb.DHCPOptions) {
		option.Options["mtu"] = fmt.Sprintf("%d", mtu)
	}
}

func WithHostname(hostname string) Option {
	return func(option *nbdb.DHCPOptions) {
		option.Options["hostname"] = fmt.Sprintf("%q", hostname)
	}
}

func EnsureDHCPOptions(nbClient libovsdbclient.Client, controllerName string, vmName ktypes.NamespacedName, v4Req *DHCPOptionRequest, v6Req *DHCPOptionRequest, lsp *nbdb.LogicalSwitchPort) error {
	v4DHCPConfig := newDHCPOptionsForVM(controllerName, vmName, &v4Req.Cidr, v4Req.Options...)
	var v6DHCPConfig *nbdb.DHCPOptions
	if v6Req != nil {
		v6DHCPConfig = newDHCPOptionsForVM(controllerName, vmName, &v6Req.Cidr, v6Req.Options...)
	}
	if err := libovsdbops.CreateOrUpdateDhcpOptions(nbClient, lsp, v4DHCPConfig, v6DHCPConfig); err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
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

func initDHCPOptions(controllerName string, vmKey ktypes.NamespacedName, dhcpOptions *nbdb.DHCPOptions) *nbdb.DHCPOptions {
	dhcpvOptionsDbObjectID := DHCPOptionsKeyForVMs(controllerName, vmKey, dhcpOptions.Cidr)
	dhcpOptions.ExternalIDs = dhcpvOptionsDbObjectID.GetExternalIDs()
	dhcpOptions.ExternalIDs[OvnZoneExternalIDKey] = OvnLocalZone
	return dhcpOptions
}

func DHCPOptionsKeyForVMs(controllerName string, vmKey ktypes.NamespacedName, cidr string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: vmKey.String(),
			libovsdbops.CIDRKey:       strings.ReplaceAll(cidr, ":", "."),
		})
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
