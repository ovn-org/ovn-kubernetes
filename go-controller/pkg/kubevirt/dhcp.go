package kubevirt

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	dhcpLeaseTime = 3500
)

type DHCPOption struct {
	V4 *nbdb.DHCPOptions
	V6 *nbdb.DHCPOptions
}

type Option func(option *DHCPOption)

func NewDHCPv4Option(cidr string, serverIP string, dbObjIDs *libovsdbops.DbObjectIDs, opts ...Option) DHCPOption {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(serverIP)).String()
	dhcp4Option := &DHCPOption{V4: &nbdb.DHCPOptions{
		Cidr:        cidr,
		ExternalIDs: dbObjIDs.GetExternalIDs(),
		Options: map[string]string{
			"server_id":  serverIP,
			"server_mac": serverMAC,
			"lease_time": fmt.Sprintf("%d", dhcpLeaseTime),
		},
	}}
	for _, opt := range opts {
		opt(dhcp4Option)
	}
	return *dhcp4Option
}

func SecondaryNetworkDHCPKey(cidr string, networkName string, controllerName string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(
		libovsdbops.SecondaryNetworkDHCPOptions,
		controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:       cidr,
			libovsdbops.SecondaryNetworkKey: networkName,
		})
}

func WithDNSServer(serverIP string) Option {
	return func(option *DHCPOption) {
		option.V4.Options["dns_server"] = serverIP
	}
}

func WithGateway(gwIP string) Option {
	return func(option *DHCPOption) {
		option.V4.Options["router"] = gwIP
	}
}

func WithHostname(hostname string) Option {
	return func(option *DHCPOption) {
		option.V4.Options["hostname"] = hostname
	}
}

func WithMTU(mtu int) Option {
	return func(option *DHCPOption) {
		option.V4.Options["mtu"] = fmt.Sprintf("%d", mtu)
	}
}

func EnsureDHCPOptions(dhcpOptions *nbdb.DHCPOptions, nbClient libovsdbclient.Client, lsp *nbdb.LogicalSwitchPort) error {
	if err := libovsdbops.CreateOrUpdateDhcpOptions(nbClient, lsp, dhcpOptions, nil); err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
}

func EnsureDHCPOptionsForVM(controllerName string, nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod, ovnPodAnnotation *util.PodAnnotation, lsp *nbdb.LogicalSwitchPort) error {
	if !IsPodLiveMigratable(pod) {
		return nil
	}

	// Fake router to delegate on proxy arp mechanism
	vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if !ok {
		return fmt.Errorf("missing %s label at pod %s/%s when configuaring DHCP", kubevirtv1.VirtualMachineNameLabel, pod.Namespace, pod.Name)
	}
	dhcpConfigs, err := composeDHCPConfigs(watchFactory, controllerName, pod.Namespace, vmName, ovnPodAnnotation.IPs)
	if err != nil {
		return fmt.Errorf("failed composing DHCP options: %v", err)
	}
	err = libovsdbops.CreateOrUpdateDhcpOptions(nbClient, lsp, dhcpConfigs.V4, dhcpConfigs.V6)
	if err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
}

func composeDHCPConfigs(k8scli *factory.WatchFactory, controllerName, namespace, vmName string, podIPs []*net.IPNet) (*DHCPOption, error) {
	if len(podIPs) == 0 {
		return nil, fmt.Errorf("missing podIPs to compose dhcp options")
	}
	if vmName == "" {
		return nil, fmt.Errorf("missing vmName to compose dhcp options")
	}

	dnsServerIPv4, dnsServerIPv6, err := retrieveDNSServiceClusterIPs(k8scli)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving dns service cluster ip: %v", err)
	}

	dhcpConfigs := &DHCPOption{}
	for _, ip := range podIPs {
		_, cidr, err := net.ParseCIDR(ip.String())
		if err != nil {
			return nil, fmt.Errorf("failed converting podIPs to cidr to configure dhcp: %v", err)
		}
		if utilnet.IsIPv4CIDR(cidr) {
			var opts []Option
			opts = append(
				opts,
				WithHostname(vmName),
				WithGateway(ARPProxyIPv4),
				WithDNSServer(dnsServerIPv4),
			)
			dhcpConfigs.V4 = NewDHCPv4Option(
				cidr.String(),
				ARPProxyIPv4,
				VMDHCPOptionsKey(controllerName, namespace, vmName, cidr.String()),
				opts...,
			).V4
		} else if utilnet.IsIPv6CIDR(cidr) {
			dhcpConfigs.V6 = ComposeDHCPv6Options(cidr.String(), dnsServerIPv6, controllerName, namespace, vmName)
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

func ComposeDHCPv6Options(cidr, dnsServer, controllerName, namespace, vmName string) *nbdb.DHCPOptions {
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
	return composeDHCPOptions(controllerName, namespace, vmName, dhcpOptions)
}

func composeDHCPOptions(controllerName, namespace, vmName string, dhcpOptions *nbdb.DHCPOptions) *nbdb.DHCPOptions {
	dhcpvOptionsDbObjectID := VMDHCPOptionsKey(controllerName, namespace, vmName, dhcpOptions.Cidr)
	dhcpOptions.ExternalIDs = dhcpvOptionsDbObjectID.GetExternalIDs()
	dhcpOptions.ExternalIDs[OvnZoneExternalIDKey] = OvnLocalZone
	return dhcpOptions
}

func VMDHCPOptionsKey(controllerName string, namespace string, vmName string, cidr string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, controllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey:     cidr,
			libovsdbops.VirtualMachineKey: vmName,
			libovsdbops.NamespaceKey:      namespace,
		})
}

func DeleteDHCPOptions(controllerName string, nbClient libovsdbclient.Client, pod *corev1.Pod, nadName string) error {
	vmKey := ExtractVMNameFromPod(pod)
	if vmKey == nil {
		return nil
	}
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadName)
	if err != nil {
		return err
	}
	for _, ipNet := range podAnnotation.IPs {
		cidr := net.IPNet{
			IP:   ipNet.IP.Mask(ipNet.Mask),
			Mask: ipNet.Mask,
		}
		dhcpOptions := composeDHCPOptions(controllerName, vmKey.Namespace, vmKey.Name, &nbdb.DHCPOptions{
			Cidr: cidr.String(),
		})
		if err := libovsdbops.DeleteDHCPOptions(nbClient, dhcpOptions); err != nil {
			return err
		}
	}
	return nil
}
