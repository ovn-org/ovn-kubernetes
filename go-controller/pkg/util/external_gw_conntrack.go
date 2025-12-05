// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package util

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/mdlayher/ndp"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// inspired by arping timeout
var msgTimeout = 500 * time.Millisecond

func findInterfaceForDstIP(dstIP string) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	netIP := net.ParseIP(dstIP)
	if netIP == nil {
		return nil, fmt.Errorf("failed to parse ip: %w", err)
	}

	isDown := func(iface net.Interface) bool {
		return iface.Flags&net.FlagUp == 0
	}
	hasAddressInNetwork := func(iface net.Interface) bool {
		// ignore loopback interfaces
		if iface.Flags&net.FlagLoopback != 0 {
			return false
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return false
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ipnet.Contains(netIP) {
					return true
				}
			}
		}
		return false
	}

	for _, iface := range ifaces {
		if isDown(iface) {
			continue
		}
		if !hasAddressInNetwork(iface) {
			continue
		}
		return &iface, nil
	}
	return nil, errors.New("no usable interface found")
}

func readNDPMsg(msg ndp.Message) (targetIP netip.Addr, mac net.HardwareAddr, err error) {
	// Expect a neighbor advertisement message with a target link-layer
	// address option.
	na, ok := msg.(*ndp.NeighborAdvertisement)
	if !ok {
		err = fmt.Errorf("message is not a neighbor advertisement: %T", msg)
		return
	}
	if len(na.Options) != 1 {
		err = fmt.Errorf("expected one option in neighbor advertisement")
		return
	}
	lla, ok := na.Options[0].(*ndp.LinkLayerAddress)
	if !ok {
		err = fmt.Errorf("option is not a link-layer address: %T", msg)
		return
	}
	// target ip doesn't have a zone set, return ip without a zone to compare
	return na.TargetAddress.WithZone(""), lla.Addr, nil
}

// getIPv6MacOnIface tries to resolve as many ips as possible.
// Errors that prevent only 1 ip from being resolved are logged and not returned.
// When an error is returned, some MACs may still be resolved and returned too.
func getIPv6MacOnIface(info *ifaceWithTargetIPs) ([]net.HardwareAddr, error) {
	// Set up an *ndp.Conn, bound to this interface's link-local IPv6 address.
	c, _, err := ndp.Listen(info.iface, ndp.LinkLocal)
	if err != nil {
		return nil, fmt.Errorf("failed to dial NDP connection: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			klog.Errorf("Failed to close NDP connection on interface %s: %v", info.iface.Name, err)
		}
	}()
	for _, resolveIP := range info.ips {
		target, err := netip.ParseAddr(resolveIP)
		if err != nil {
			klog.Errorf("Failed to ParseAddr %v: %v", resolveIP, err)
			continue
		}
		// Use target's solicited-node multicast address to request that the target
		// respond with a neighbor advertisement.
		snm, err := ndp.SolicitedNodeMulticast(target)
		if err != nil {
			klog.Errorf("Failed to determine solicited-node multicast address: %v", err)
			continue
		}

		// Build a neighbor solicitation message, indicate the target's link-local
		// address, and also specify our source link-layer address.
		m := &ndp.NeighborSolicitation{
			TargetAddress: target,
			Options: []ndp.Option{
				&ndp.LinkLayerAddress{
					Direction: ndp.Source,
					Addr:      info.iface.HardwareAddr,
				},
			},
		}

		// Send the multicast message and wait for a response.
		if err = c.WriteTo(m, nil, snm); err != nil {
			klog.Errorf("Failed to send neighbor solicitation: %v", err)
			continue
		}
	}
	ipsToFind := sets.New[string](info.ips...)
	macs := []net.HardwareAddr{}

	maxDuration := time.Duration(len(info.ips)) * msgTimeout
	for start := time.Now(); time.Since(start) < maxDuration; {
		if err = c.SetReadDeadline(time.Now().Add(msgTimeout)); err != nil {
			return macs, fmt.Errorf("failed to set read deadline: %w", err)
		}
		msg, _, _, err := c.ReadFrom()
		if err != nil {
			// if some ips are resolved and others are not available anymore, return macs
			// when no more messages are received
			return macs, fmt.Errorf("failed to read NDP message: %v", err)
		}
		// target ip doesn't have a zone set, return ip without a zone to compare
		ip, mac, err := readNDPMsg(msg)
		if err != nil {
			// wrong message, doesn't mean error
			continue
		}
		if ipsToFind.Has(ip.String()) {
			macs = append(macs, mac)
			ipsToFind.Delete(ip.String())
			if len(ipsToFind) == 0 {
				// all ips are resolved
				return macs, nil
			}
		}
	}
	klog.Errorf("Failed to receive NA for ips %v after %s", ipsToFind, maxDuration)
	return macs, nil
}

type ifaceWithTargetIPs struct {
	iface *net.Interface
	ips   []string
}

// getIPv6Macs is best-effort resolution.
// It logs errors instead of returning them to resolve as many IPs as possible
func getIPv6Macs(resolveIPs ...string) ([]net.HardwareAddr, error) {
	if len(resolveIPs) == 0 {
		return nil, nil
	}
	// map[interfaceName][ifaceWithTargetIPs, ...]
	infos := map[string]*ifaceWithTargetIPs{}
	for _, resolveIP := range resolveIPs {
		if !utilnet.IsIPv6String(resolveIP) {
			klog.Warningf("Non-ipv6 address %s was passed for MAC resolution, ignore", resolveIP)
			continue
		}
		iface, err := findInterfaceForDstIP(resolveIP)
		if err != nil {
			klog.Errorf("Failed to find interface for ip %v: %v", resolveIP, err)
			continue
		}
		info, ok := infos[iface.Name]
		if !ok {
			info = &ifaceWithTargetIPs{
				iface: iface,
			}
			infos[iface.Name] = info
		}
		info.ips = append(info.ips, resolveIP)
	}
	allMacs := []net.HardwareAddr{}
	for _, info := range infos {
		macs, err := getIPv6MacOnIface(info)
		if err != nil {
			klog.Errorf("Failed to resolve ips on iface %s: %v", info.iface.Name, err)
			// don't continue, some macs may still be returned
		}
		if len(macs) > 0 {
			allMacs = append(allMacs, macs...)
		}
	}
	return allMacs, nil
}

func getIPv4Macs(resolveIPs ...string) ([]net.HardwareAddr, error) {
	if len(resolveIPs) == 0 {
		return nil, nil
	}
	validMACs := sync.Map{}
	var wg sync.WaitGroup
	wg.Add(len(resolveIPs))
	for _, gwIP := range resolveIPs {
		go func(gwIP string) {
			defer wg.Done()
			if len(gwIP) > 0 {
				if hwAddr, err := GetMACAddressFromARP(net.ParseIP(gwIP)); err != nil {
					klog.Errorf("Failed to lookup hardware address for gatewayIP %s: %v", gwIP, err)
				} else if len(hwAddr) > 0 {
					validMACs.Store(gwIP, hwAddr)
				}
			}
		}(gwIP)
	}
	wg.Wait()
	validNextHopMACs := []net.HardwareAddr{}
	validMACs.Range(func(_ interface{}, value interface{}) bool {
		validNextHopMACs = append(validNextHopMACs, value.(net.HardwareAddr))
		return true
	})
	return validNextHopMACs, nil
}

func convertMacToLabel(hwAddr net.HardwareAddr) []byte {
	// we need to reverse the mac before passing it to the conntrack filter since OVN saves the MAC in the following format
	// +------------------------------------------------------------ +
	// | 128 ...  112 ... 96 ... 80 ... 64 ... 48 ... 32 ... 16 ... 0|
	// +------------------+-------+--------------------+-------------|
	// |                  | UNUSED|    MAC ADDRESS     |   UNUSED    |
	// +------------------+-------+--------------------+-------------+
	for i, j := 0, len(hwAddr)-1; i < j; i, j = i+1, j-1 {
		hwAddr[i], hwAddr[j] = hwAddr[j], hwAddr[i]
	}
	return hwAddr
}

// SyncConntrackForExternalGateways removes stale conntrack entries for pods returned by podsGetter.
// To do so, it resolves all given gwIPsToKeep MAC addresses that are used as labels by ecmp conntrack flows.
// Conntrack flows with MAC labels that do not belong to any of gwIPsToKeep are removed.
func SyncConntrackForExternalGateways(gwIPsToKeep sets.Set[string], isPodInLocalZone func(pod *corev1.Pod) (bool, error),
	podsGetter func() ([]*corev1.Pod, error)) error {
	ipv6IPs := []string{}
	ipv4IPs := []string{}
	for gwIP := range gwIPsToKeep {
		if len(gwIP) > 0 {
			if utilnet.IsIPv6String(gwIP) {
				ipv6IPs = append(ipv6IPs, gwIP)
			} else {
				ipv4IPs = append(ipv4IPs, gwIP)
			}
		}
	}

	ipv4Macs, err := getIPv4Macs(ipv4IPs...)
	if err != nil {
		klog.Errorf("Failed to lookup hardware address for gatewayIPs %+v: %v", ipv4IPs, err)
	}

	ipv6Macs, err := getIPv6Macs(ipv6IPs...)
	if err != nil {
		klog.Errorf("Failed to lookup hardware address for gatewayIPs %+v: %v", ipv6IPs, err)
	}

	validNextHopMACs := [][]byte{}
	for _, mac := range append(ipv4Macs, ipv6Macs...) {
		validNextHopMACs = append(validNextHopMACs, convertMacToLabel(mac))
	}

	// Handle corner case where there are 0 IPs on the annotations OR none of the ARPs were successful; i.e allowMACList={empty}.
	// This means we *need to* pass a label > 128 bits that will not match on any conntrack entry labels for these pods.
	// That way any remaining entries with labels having MACs set will get purged.
	if len(validNextHopMACs) == 0 {
		validNextHopMACs = append(validNextHopMACs, []byte("does-not-contain-anything"))
	}
	pods, err := podsGetter()
	if err != nil {
		return fmt.Errorf("unable to get pods from informer: %v", err)
	}

	// matchedFlows are the labeled conntrack entries whose MAC labels do not
	// match any remaining gateway. ConntrackListUnmatchLabelEntries dumps IPv4 and
	// IPv6 tables once and returns only those matches. Per-pod work below
	// filters this set by pod IP.
	matchedFlows, err := ConntrackListUnmatchLabelEntries(validNextHopMACs)
	if err != nil {
		return fmt.Errorf("failed to list conntrack entries: %v", err)
	}

	var errs []error
	for _, pod := range pods {
		pod := pod

		if isPodInLocalZone != nil {
			// Since it's executed in ovnkube-controller only for multi-zone-ic the following hack of filtering
			// local pods will work. Error will be treated as best-effort and ignored
			if localPod, _ := isPodInLocalZone(pod); !localPod {
				continue
			}
		}

		podIPs, err := GetPodIPsOfNetwork(pod, &DefaultNetInfo{}, nil)
		if err != nil && !errors.Is(err, ErrNoPodIPFound) {
			errs = append(errs, fmt.Errorf("unable to fetch IP for pod %s/%s: %v", pod.Namespace, pod.Name, err))
		}
		for _, podIP := range podIPs {
			// OrigDstIP: pod as server (ingress from next hop). OrigSrcIP: pod as client (egress).
			errs = append(errs, deletePodConntrackEntriesForGW(podIP.String(), validNextHopMACs, matchedFlows, netlink.ConntrackOrigDstIP)...)
			errs = append(errs, deletePodConntrackEntriesForGW(podIP.String(), validNextHopMACs, matchedFlows, netlink.ConntrackOrigSrcIP)...)
		}
	}

	return utilerrors.Join(errs...)
}

func deletePodConntrackEntriesForGW(podIP string, validNextHopMACs [][]byte, flows []*netlink.ConntrackFlow, filterType netlink.ConntrackFilterType) []error {
	var errs []error
	podIPFilter := map[string]netlink.ConntrackFilterType{podIP: filterType}
	// For each stale flow that involves this pod in the given orig direction,
	// ConntrackDeleteFilters (raw dump replay) is used twice: once with
	// UnmatchLabels to drop labeled unmatched entries, then with no labels so
	// unlabeled copies of the same orig 5-tuple in other zones are also removed
	// and traffic can hand off to a remaining gateway.
	//
	// Orig src+dst ports are included so a shared destination behind ECMP next
	// hops (same podIP↔dstIP, different sports / next-hop MACs) does not cause
	// peers hashed to remaining gateways to be wiped.
	for _, flow := range flows {
		match, err := ConntrackFlowMatchesFilter(flow, podIPFilter, 0, "", nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to match conntrack entry for pod %s: %v", podIP, err))
			continue
		}
		if !match {
			continue
		}
		// Labeled delete can include reply IPs from the listed flow; UnmatchLabels
		// is the hop discriminator.
		labeledIPFilter := map[string]netlink.ConntrackFilterType{
			flow.Forward.SrcIP.String(): netlink.ConntrackOrigSrcIP,
			flow.Forward.DstIP.String(): netlink.ConntrackOrigDstIP,
			flow.Reverse.SrcIP.String(): netlink.ConntrackReplySrcIP,
			flow.Reverse.DstIP.String(): netlink.ConntrackReplyDstIP,
		}
		// Unlabeled zone / host SNAT twins often keep the same orig 5-tuple but a
		// different reply (e.g. node IP when MEG SNAT is enabled). Matching reply
		// IPs from the labeled flow would miss those copies whether SNAT is on or
		// off, so the second delete keys on orig IPs + ports only.
		unlabeledIPFilter := map[string]netlink.ConntrackFilterType{
			flow.Forward.SrcIP.String(): netlink.ConntrackOrigSrcIP,
			flow.Forward.DstIP.String(): netlink.ConntrackOrigDstIP,
		}
		// MEG conntrack cleanup only supports TCP/UDP/SCTP (corev1.Protocol).
		// Other protocols have no mapped L4 type in DeleteConntrackTuple; an
		// IP-only filter can wipe remaining-gateway peers to the same dest.
		protocol := protocolFromConntrack(flow.Forward.Protocol)
		if protocol == "" {
			continue
		}
		// Delete the conntrack entry not matching any of the remaining gateway MACs
		_, err = DeleteConntrackTuple(labeledIPFilter, flow.Forward.SrcPort, flow.Forward.DstPort, protocol, validNextHopMACs)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete conntrack entry for pod %s: %v", podIP, err))
		}
		// Unlabeled delete has no MAC labels to distinguish ECMP next hops. Skip
		// when L4 ports are absent — IP+protocol alone can wipe remaining-gateway
		// peers to the same destination.
		if flow.Forward.SrcPort == 0 && flow.Forward.DstPort == 0 {
			continue
		}
		// Delete unlabeled copies of the same orig 5-tuple (any reply / zone).
		_, err = DeleteConntrackTuple(unlabeledIPFilter, flow.Forward.SrcPort, flow.Forward.DstPort, protocol, nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete conntrack entry for pod %s: %v", podIP, err))
		}
	}
	return errs
}

func protocolFromConntrack(proto uint8) corev1.Protocol {
	switch proto {
	case 17:
		return corev1.ProtocolUDP
	case 132:
		return corev1.ProtocolSCTP
	case 6:
		return corev1.ProtocolTCP
	default:
		return ""
	}
}
