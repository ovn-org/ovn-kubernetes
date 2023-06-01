package kubevirt

import (
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type DHCPConfig struct {
	V4Options *nbdb.DHCPOptions
	V6Options *nbdb.DHCPOptions
}

func ComposeDHCPConfig(k8scli *factory.WatchFactory, hostname string, ips []*net.IPNet) (*DHCPConfig, error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("missing ips to compose dchp options")
	}
	if hostname == "" {
		return nil, fmt.Errorf("missing hostname to compose dchp options")
	}

	dnsServerIPv4, dnsServerIPv6, err := retrieveDNSServiceClusterIPs(k8scli)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving dns service cluster ip: %v", err)
	}

	dhcpConfig := &DHCPConfig{}
	for _, ip := range ips {
		_, cidr, err := net.ParseCIDR(ip.String())
		if err != nil {
			return nil, fmt.Errorf("failed converting ips to cidr to configure dhcp: %v", err)
		}
		if utilnet.IsIPv4CIDR(cidr) {
			dhcpConfig.V4Options = ComposeDHCPv4Options(cidr.String(), ARPProxyIPv4, dnsServerIPv4, hostname)
		} else if utilnet.IsIPv6CIDR(cidr) {
			dhcpConfig.V6Options = ComposeDHCPv6Options(cidr.String(), ARPProxyIPv6, dnsServerIPv6)
		}
	}
	return dhcpConfig, nil
}

func retrieveDNSServiceClusterIPs(k8scli *factory.WatchFactory) (string, string, error) {
	dnsServer, err := k8scli.GetService("kube-system", "kube-dns")
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", "", err
		}
		dnsServer, err = k8scli.GetService("openshift-dns", "dns-default")
		if err != nil {
			return "", "", err
		}
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

func ComposeDHCPv4Options(cidr string, arpProxyIP, dnsServer, hostname string) *nbdb.DHCPOptions {
	dhcpLeaseTime := 3500
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(arpProxyIP)).String()
	return &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"lease_time": fmt.Sprintf("%d", dhcpLeaseTime),
			"router":     arpProxyIP,
			"dns_server": dnsServer,
			"server_id":  arpProxyIP,
			"server_mac": serverMAC,
			"hostname":   fmt.Sprintf("%q", hostname),
		},
	}
}

func ComposeDHCPv6Options(cidr, arpProxyIP, dnsServer string) *nbdb.DHCPOptions {
	serverMAC := util.IPAddrToHWAddr(net.ParseIP(arpProxyIP)).String()
	dhcpOptions := &nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"server_id": serverMAC,
		},
	}
	if dnsServer != "" {
		dhcpOptions.Options["dns_server"] = dnsServer
	}
	return dhcpOptions
}
