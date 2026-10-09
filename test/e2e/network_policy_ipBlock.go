// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

// These tests complement the upstream Kubernetes NetworkPolicy conformance suite
// (test/e2e/network/netpol/network_policy.go), which only covers ipBlock on egress
// and only for a single address family at a time (it builds one CIDR from
// pod.Status.PodIP). The cases below add the coverage that suite lacks:
//   - ingress ipBlock (upstream has none), single and multiple CIDRs, all IP families
//   - dual-stack egress ipBlock and ipBlock-with-except (both families in one policy)
var _ = ginkgo.Describe("Network Policy: ipBlock", feature.NetworkPolicy, func() {
	const (
		httpPort    = 8000
		pollTimeout = 1 * time.Minute
		pollCheck   = 6 * time.Second
		stayTimeout = 15 * time.Second
		stayCheck   = 5 * time.Second
	)
	netexecCmd := []string{"/bin/bash", "-c", fmt.Sprintf("/agnhost netexec --http-port %d", httpPort)}

	f := wrappedTestFramework("network-policy-ipblock")

	// twoNodes returns the names of two schedulable nodes, skipping the test if
	// fewer than two are available.
	twoNodes := func() (string, string) {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err, "failed to list schedulable nodes")
		if len(nodes.Items) < 2 {
			ginkgo.Skip("requires at least 2 schedulable nodes")
		}
		return nodes.Items[0].Name, nodes.Items[1].Name
	}

	createPolicy := func(policy *knet.NetworkPolicy) {
		_, err := f.ClientSet.NetworkingV1().NetworkPolicies(f.Namespace.Name).Create(
			context.TODO(), policy, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create network policy %s", policy.Name)
	}

	expectReachable := func(srcPodName string, dstPod *v1.Pod) {
		gomega.Eventually(func() error {
			return pokeAllPodIPs(f, srcPodName, dstPod)
		}, pollTimeout, pollCheck).Should(gomega.Succeed(),
			"%s should be able to reach %s", srcPodName, dstPod.Name)
	}

	// allPodIPsBlocked returns nil only when every one of dstPod's IPs is
	// unreachable. pokeAllPodIPs aggregates results and fails if *any* IP is
	// unreachable, which is too weak for a negative check: on a dual-stack
	// cluster it would still pass if one family were blocked while the other
	// leaked through. Probing each address independently keeps per-family
	// coverage for the blocked assertions.
	allPodIPsBlocked := func(srcPodName string, dstPod *v1.Pod) error {
		ips := dstPod.Status.PodIPs
		if len(ips) == 0 {
			ips = []v1.PodIP{{IP: dstPod.Status.PodIP}}
		}
		for _, podIP := range ips {
			if err := pokePod(f, srcPodName, podIP.IP); err == nil {
				return fmt.Errorf("%s unexpectedly reached %s at %s", srcPodName, dstPod.Name, podIP.IP)
			}
		}
		return nil
	}

	expectBlocked := func(srcPodName string, dstPod *v1.Pod) {
		gomega.Eventually(func() error {
			return allPodIPsBlocked(srcPodName, dstPod)
		}, pollTimeout, pollCheck).Should(gomega.Succeed(),
			"%s should not be able to reach %s on any IP", srcPodName, dstPod.Name)
		gomega.Consistently(func() error {
			return allPodIPsBlocked(srcPodName, dstPod)
		}, stayTimeout, stayCheck).Should(gomega.Succeed(),
			"%s should stay blocked from %s on all IPs", srcPodName, dstPod.Name)
	}

	// Upstream covers ipBlock only on egress, so this adds the ingress direction
	// across all supported IP families.
	ginkgo.It("enforces an ingress ipBlock policy allowing traffic only from the specified CIDR", func() {
		ns := f.Namespace.Name
		node0, node1 := twoNodes()

		serverLabels := map[string]string{"role": "ipblock-ingress-server"}
		server, err := createGenericPodWithLabel(f, "server", node0, ns, netexecCmd, serverLabels)
		framework.ExpectNoError(err, "failed to create server pod")
		allowed, err := createGenericPod(f, "allowed-client", node1, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create allowed client pod")
		blocked, err := createGenericPod(f, "blocked-client", node1, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create blocked client pod")

		ginkgo.By("allowing ingress to the server only from the allowed client's CIDRs")
		peers := ipBlockPeersFromCIDRs(hostCIDRsForPod(allowed))
		createPolicy(ingressIPBlockPolicy("allow-ingress-from-cidr", serverLabels, peers))

		ginkgo.By("verifying the allowed client can reach the server")
		expectReachable(allowed.Name, server)
		ginkgo.By("verifying the blocked client cannot reach the server")
		expectBlocked(blocked.Name, server)
	})

	// Ingress ipBlock with multiple CIDRs, ensuring every CIDR in the ipBlock list is
	// honored, not just the last one. Runs on all families.
	ginkgo.It("enforces an ingress ipBlock policy honoring all CIDRs in the list", func() {
		ns := f.Namespace.Name
		node0, node1 := twoNodes()

		serverLabels := map[string]string{"role": "ipblock-ingress-multi-server"}
		server, err := createGenericPodWithLabel(f, "server", node0, ns, netexecCmd, serverLabels)
		framework.ExpectNoError(err, "failed to create server pod")

		var allowedClients []*v1.Pod
		var cidrs []string
		for i := 1; i <= 3; i++ {
			node := node0
			if i%2 == 0 {
				node = node1
			}
			c, err := createGenericPod(f, fmt.Sprintf("allowed-client-%d", i), node, ns, netexecCmd)
			framework.ExpectNoError(err, "failed to create allowed client pod %d", i)
			allowedClients = append(allowedClients, c)
			cidrs = append(cidrs, hostCIDRsForPod(c)...)
		}
		blocked, err := createGenericPod(f, "blocked-client", node1, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create blocked client pod")

		ginkgo.By("allowing ingress to the server from all three client CIDRs")
		createPolicy(ingressIPBlockPolicy("allow-ingress-multi-cidr", serverLabels, ipBlockPeersFromCIDRs(cidrs)))

		ginkgo.By("verifying every allowed client can reach the server")
		for _, c := range allowedClients {
			expectReachable(c.Name, server)
		}
		ginkgo.By("verifying the non-listed client cannot reach the server")
		expectBlocked(blocked.Name, server)
	})

	// Dual-stack egress ipBlock. Upstream only ever puts a single family CIDR in the
	// policy; this validates both IPv4 and IPv6 CIDRs in one egress policy.
	ginkgo.It("enforces a dual-stack egress ipBlock policy allowing traffic only to the specified CIDRs", func() {
		if !isIPv4Supported(f.ClientSet) || !isIPv6Supported(f.ClientSet) {
			ginkgo.Skip("requires a dual-stack cluster")
		}
		ns := f.Namespace.Name
		node0, node1 := twoNodes()

		clientLabels := map[string]string{"role": "ipblock-egress-client"}
		client, err := createGenericPodWithLabel(f, "client", node0, ns, netexecCmd, clientLabels)
		framework.ExpectNoError(err, "failed to create client pod")
		allowedServer, err := createGenericPod(f, "allowed-server", node1, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create allowed server pod")
		otherServer, err := createGenericPod(f, "other-server", node1, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create other server pod")

		cidrs := hostCIDRsForPod(allowedServer)
		gomega.Expect(cidrs).To(gomega.HaveLen(2), "dual-stack allowed server should have one IPv4 and one IPv6 CIDR")

		ginkgo.By("allowing egress from the client only to the allowed server's IPv4 and IPv6 CIDRs")
		createPolicy(egressIPBlockPolicy("allow-egress-dual-cidr", clientLabels, ipBlockPeersFromCIDRs(cidrs)))

		ginkgo.By("verifying the client can reach the allowed server on both families")
		expectReachable(client.Name, allowedServer)
		ginkgo.By("verifying the client cannot reach the other server")
		expectBlocked(client.Name, otherServer)
	})

	// Dual-stack egress ipBlock with except. Validates that the except clause is honored
	// per-family when both IPv4 and IPv6 blocks are present in one egress policy.
	ginkgo.It("enforces a dual-stack egress ipBlock policy with an except clause", func() {
		if !isIPv4Supported(f.ClientSet) || !isIPv6Supported(f.ClientSet) {
			ginkgo.Skip("requires a dual-stack cluster")
		}
		ns := f.Namespace.Name
		node0, node1 := twoNodes()

		clientLabels := map[string]string{"role": "ipblock-egress-except-client"}
		client, err := createGenericPodWithLabel(f, "client", node1, ns, netexecCmd, clientLabels)
		framework.ExpectNoError(err, "failed to create client pod")
		// allowed and excepted servers share a node so they fall in the same enclosing CIDR.
		allowedServer, err := createGenericPod(f, "allowed-server", node0, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create allowed server pod")
		exceptServer, err := createGenericPod(f, "except-server", node0, ns, netexecCmd)
		framework.ExpectNoError(err, "failed to create excepted server pod")

		allowedByFamily := podIPsByFamily(allowedServer)
		exceptByFamily := podIPsByFamily(exceptServer)
		gomega.Expect(allowedByFamily).To(gomega.HaveLen(2), "allowed server should be dual-stack")
		gomega.Expect(exceptByFamily).To(gomega.HaveLen(2), "excepted server should be dual-stack")

		var peers []knet.NetworkPolicyPeer
		for isIPv6, allowedIP := range allowedByFamily {
			exceptIP, ok := exceptByFamily[isIPv6]
			gomega.Expect(ok).To(gomega.BeTrue(), "excepted server missing an address family present on allowed server")
			peers = append(peers, knet.NetworkPolicyPeer{
				IPBlock: &knet.IPBlock{
					CIDR:   enclosingCIDR(allowedIP),
					Except: []string{hostCIDR(exceptIP)},
				},
			})
		}

		ginkgo.By("allowing egress to a broad CIDR per family while excepting the excepted server")
		createPolicy(egressIPBlockPolicy("allow-egress-dual-cidr-except", clientLabels, peers))

		ginkgo.By("verifying the client can reach the allowed (non-excepted) server")
		expectReachable(client.Name, allowedServer)
		ginkgo.By("verifying the client cannot reach the excepted server")
		expectBlocked(client.Name, exceptServer)
	})
})

// hostCIDR returns the host (/32 or /128) CIDR for a single IP string.
func hostCIDR(ipStr string) string {
	if utilnet.IsIPv6String(ipStr) {
		return ipStr + "/128"
	}
	return ipStr + "/32"
}

// hostCIDRsForPod returns a host CIDR for every address family the pod has.
func hostCIDRsForPod(pod *v1.Pod) []string {
	cidrs := make([]string, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		cidrs = append(cidrs, hostCIDR(podIP.IP))
	}
	return cidrs
}

// podIPsByFamily maps isIPv6 -> pod IP for each address family the pod has.
func podIPsByFamily(pod *v1.Pod) map[bool]string {
	byFamily := make(map[bool]string, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		byFamily[utilnet.IsIPv6String(podIP.IP)] = podIP.IP
	}
	return byFamily
}

// enclosingCIDR returns a broad CIDR (/16 for IPv4, /64 for IPv6) that encloses the
// given IP, suitable as the allow block in an ipBlock-with-except rule.
func enclosingCIDR(ipStr string) string {
	ip := net.ParseIP(ipStr)
	var mask net.IPMask
	if utilnet.IsIPv6String(ipStr) {
		mask = net.CIDRMask(64, 128)
	} else {
		ip = ip.To4()
		mask = net.CIDRMask(16, 32)
	}
	return (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String()
}

// ipBlockPeersFromCIDRs builds one NetworkPolicyPeer per CIDR, each an ipBlock allow.
func ipBlockPeersFromCIDRs(cidrs []string) []knet.NetworkPolicyPeer {
	peers := make([]knet.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, knet.NetworkPolicyPeer{IPBlock: &knet.IPBlock{CIDR: cidr}})
	}
	return peers
}

func ingressIPBlockPolicy(name string, podSelector map[string]string, peers []knet.NetworkPolicyPeer) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			Ingress:     []knet.NetworkPolicyIngressRule{{From: peers}},
		},
	}
}

func egressIPBlockPolicy(name string, podSelector map[string]string, peers []knet.NetworkPolicyPeer) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: knet.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
			Egress:      []knet.NetworkPolicyEgressRule{{To: peers}},
		},
	}
}
