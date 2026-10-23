// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

// This file holds NetworkPolicy e2e tests that verify how ovn-kubernetes
// translates NetworkPolicy objects into OVN NB-DB constructs (ACLs and
// address sets) and that those constructs survive an ovnkube-node restart.
// They are the native-framework rewrites of the following cases:
//
//	55287 - default NP ACLs use "arp || nd" (not bare "arp") for ARPAllowPolicies
//	62524 - address_set referenced by an ACL is not missing when the NP name has a dot
//	70009 - a pod's IP lands in the OVN address_set for an allow-same-namespace NP
//	65901 - an unrelated pod label update does not trigger duplicate libovsdb txns
//	64786 - NP in a long (62-char) namespace is recreated after ovnkube-node restart
//	64787 - NP with duplicate egress ipBlock CIDRs is recreated after restart
//	64788 - a manually-injected duplicate ACL is removed after ovnkube-node restart
//
// Runtime risks flagged for Kind validation (these compile/vet clean but the
// exact OVN behavior must be confirmed on a cluster):
//   - runOVNNBCTL targets whatever nb-ovsdb pod findOVNNBDBPod returns. On an
//     OVN interconnect (IC) cluster the NB DB is per-node, so queries that must
//     be node-local (62524/70009/65901/64788) may need to target the specific
//     node's ovnkube-node nb-ovsdb container instead of the central pod.
//   - 65901 reads "transacting operations" lines from the ovnkube-controller
//     container log; these only appear at a sufficiently high libovsdb log
//     level. The OpenShift original read /var/log/ovnkube/libovsdb.log.
//   - address_set scoping (62524/70009) is derived by regex from ACL match
//     strings; precise per-policy scoping may need tuning.

import (
	"context"
	"encoding/csv"
	"fmt"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
)

const (
	npaclPollTimeout  = 60 * time.Second
	npaclPollInterval = 2 * time.Second
	npaclServerPort   = 8000
)

// addrSetRefRe matches an OVN address-set reference inside an ACL match string,
// e.g. "ip4.src == {$a1234}" -> captures "a1234".
var addrSetRefRe = regexp.MustCompile(`\{\$(\w+)\}`)

// npaclNetexecCmd is the command agnhost pods run so pokePod can curl them.
func npaclNetexecCmd() []string {
	return []string{"/agnhost", "netexec", "--http-port", fmt.Sprintf("%d", npaclServerPort)}
}

// npaclServerPod creates a running agnhost netexec pod in the framework
// namespace, optionally pinned to nodeName and carrying labels.
func npaclServerPod(fr *framework.Framework, name, nodeName string, labels map[string]string) *v1.Pod {
	var pod *v1.Pod
	if nodeName != "" {
		pod = newAgnhostPodOnNode(name, nodeName, labels, npaclNetexecCmd()...)
	} else {
		pod = newAgnhostPod(fr.Namespace.Name, name, npaclNetexecCmd()...)
		pod.Labels = labels
	}
	return e2epod.NewPodClient(fr).CreateSync(context.TODO(), pod)
}

// npaclCreatePolicy creates a NetworkPolicy in ns.
func npaclCreatePolicy(cs clientset.Interface, ns string, policy *knet.NetworkPolicy) {
	_, err := cs.NetworkingV1().NetworkPolicies(ns).Create(context.TODO(), policy, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating network policy %s/%s", ns, policy.Name)
}

// npaclACLNameMatches returns (name, match) pairs for all ACLs in the NB DB.
func npaclACLNameMatches(fr *framework.Framework, cs clientset.Interface) [][]string {
	out, err := runOVNNBCTL(fr, cs, "--format=csv", "--no-heading", "--columns=name,match", "find", "ACL")
	Expect(err).NotTo(HaveOccurred(), "listing ACLs")
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	Expect(err).NotTo(HaveOccurred(), "parsing ACL csv")
	return records
}

// npaclAddressSetsForNS returns the set of OVN address-set names referenced by
// ACLs whose name mentions the given namespace. Best-effort scoping; see the
// file-level note about per-policy scoping.
func npaclAddressSetsForNS(fr *framework.Framework, cs clientset.Interface, ns string) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, rec := range npaclACLNameMatches(fr, cs) {
		if len(rec) < 2 {
			continue
		}
		name, match := rec[0], rec[1]
		if !strings.Contains(name, ns) {
			continue
		}
		for _, m := range addrSetRefRe.FindAllStringSubmatch(match, -1) {
			if _, ok := seen[m[1]]; ok {
				continue
			}
			seen[m[1]] = struct{}{}
			names = append(names, m[1])
		}
	}
	return names
}

// npaclAddressSetAddresses returns the trimmed "addresses" column of an OVN
// address set, or "" if empty.
func npaclAddressSetAddresses(fr *framework.Framework, cs clientset.Interface, asName string) string {
	out, err := runOVNNBCTL(fr, cs, "--bare", "--no-heading", "--columns=addresses", "list", "Address_Set", asName)
	Expect(err).NotTo(HaveOccurred(), "listing address_set %s", asName)
	return strings.Trim(strings.TrimSpace(out), "[]")
}

// npaclNBCTLOnNode runs ovn-nbctl in the nb-ovsdb container of the ovnkube-node
// (NB DB) pod on a specific node. runOVNNBCTL targets whichever NB DB pod
// findOVNNBDBPod returns first, but on an interconnect cluster the NB DB is
// per-node, so a test that writes to the NB DB and then restarts ovnkube-node
// must direct both at the same node's zone.
func npaclNBCTLOnNode(fr *framework.Framework, cs clientset.Interface, ovnNs, node string, args ...string) (string, error) {
	pods, err := cs.CoreV1().Pods(ovnNs).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "ovn-db-pod=true",
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no ovn NB DB pod found on node %s", node)
	}
	cmd := append([]string{"ovn-nbctl"}, args...)
	stdout, stderr, err := ExecCommandInContainerWithFullOutput(fr, ovnNs, pods.Items[0].Name, "nb-ovsdb", cmd...)
	if err != nil {
		return stdout, fmt.Errorf("ovn-nbctl %v on node %s: %w, stderr: %s", args, node, err, stderr)
	}
	return strings.TrimSpace(stdout), nil
}

var _ = Describe("Network Policy: default ARP-allow ACL", feature.NetworkPolicy, func() {
	fr := wrappedTestFramework("netpol-acl-arp")

	// 55287: the default-deny ARPAllowPolicy ACLs must match "(arp || nd)" so
	// that IPv6 neighbor discovery is permitted, not just IPv4 ARP.
	It("should use '(arp || nd)' and not a bare 'arp' match", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet

		npaclServerPod(fr, "pod1", "", nil)

		By("creating a default deny-ingress network policy")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "default-deny-ingress"},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
			},
		})

		By("verifying every ARP-allow ACL uses '(arp || nd)'")
		Eventually(func(g Gomega) {
			matches, err := runOVNNBCTL(fr, cs, "--bare", "--no-heading", "--columns=match", "find", "ACL")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(matches).To(ContainSubstring("(arp || nd)"),
				"expected an ARPAllowPolicy ACL matching '(arp || nd)'")
			// A correct match is "... && (arp || nd)"; a regressed one would be
			// "... && arp" with no "nd". The leading paren after "&& " keeps the
			// correct form from matching this regex.
			g.Expect(matches).NotTo(MatchRegexp(`&& arp\b`),
				"found an ARP-allow ACL using bare 'arp' instead of '(arp || nd)'")
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())
	})
})

var _ = Describe("Network Policy: ACL address sets", feature.NetworkPolicy, func() {
	fr := wrappedTestFramework("netpol-acl-addrset")

	// 62524: a NetworkPolicy whose name contains a dot must still have its
	// address sets created; the ACLs must not reference a missing address_set.
	It("should not reference a missing address_set when the policy name contains a dot", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet

		server := npaclServerPod(fr, "web", "", map[string]string{"role": "web"})
		npaclServerPod(fr, "allowed", "", map[string]string{"role": "allowed"})
		npaclServerPod(fr, "denied", "", map[string]string{"role": "denied"})

		By("creating an ingress+egress network policy whose name contains a dot")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "egress-ingress-62524.test"},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "web"}},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress, knet.PolicyTypeEgress},
				Ingress: []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "allowed"}},
					}},
				}},
				Egress: []knet.NetworkPolicyEgressRule{{
					To: []knet.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				}},
			},
		})

		By("verifying the dotted-name policy's ACLs reference existing address sets")
		Eventually(func(g Gomega) {
			asNames := npaclAddressSetsForNS(fr, cs, ns)
			g.Expect(asNames).NotTo(BeEmpty(), "expected ACLs referencing address sets for ns %s", ns)
			for _, as := range asNames {
				out, err := runOVNNBCTL(fr, cs, "--bare", "--no-heading", "--columns=_uuid", "list", "Address_Set", as)
				g.Expect(err).NotTo(HaveOccurred(), "address_set %s referenced by an ACL must exist", as)
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(), "address_set %s is missing", as)
			}
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())

		By("verifying connectivity matches the policy")
		Eventually(func() error {
			return pokePod(fr, "allowed", server.Status.PodIP)
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed(), "allowed peer should reach the web pod")
		Consistently(func() error {
			return pokePod(fr, "denied", server.Status.PodIP)
		}, 5*time.Second, 1*time.Second).Should(HaveOccurred(), "denied peer should not reach the web pod")
	})

	// 70009: the address_set backing an allow-same-namespace policy must gain a
	// pod's IP once that pod is running.
	It("should include a pod's IP in the allow-same-namespace address_set", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet

		By("creating an allow-same-namespace ingress policy before any pods exist")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-same-namespace"},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
				Ingress: []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				}},
			},
		})

		var asNames []string
		By("locating the policy's address set(s), initially empty")
		Eventually(func(g Gomega) {
			asNames = npaclAddressSetsForNS(fr, cs, ns)
			g.Expect(asNames).NotTo(BeEmpty())
			for _, as := range asNames {
				g.Expect(npaclAddressSetAddresses(fr, cs, as)).To(BeEmpty(),
					"address_set %s should be empty before pods exist", as)
			}
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())

		By("creating a pod and verifying its IP is added to the address set")
		pod := npaclServerPod(fr, "pod1", "", nil)
		Expect(pod.Status.PodIP).NotTo(BeEmpty())
		Eventually(func(g Gomega) {
			found := false
			for _, as := range asNames {
				if strings.Contains(npaclAddressSetAddresses(fr, cs, as), pod.Status.PodIP) {
					found = true
				}
			}
			g.Expect(found).To(BeTrue(), "pod IP %s should appear in an allow-same-namespace address_set", pod.Status.PodIP)
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())
	})
})

var _ = Describe("Network Policy: libovsdb transactions", feature.NetworkPolicy, Serial, func() {
	fr := wrappedTestFramework("netpol-acl-txn")

	// 65901: updating a pod with a label unrelated to any policy peer selector
	// must not generate additional libovsdb transactions for the policy's
	// address set.
	// TODO(validation): this relies on "transacting operations" lines being
	// present in the ovnkube-controller container log, which depends on the
	// libovsdb log level. The OpenShift original read
	// /var/log/ovnkube/libovsdb.log. Confirm/adjust the source on Kind.
	It("should not generate duplicate transactions on an unrelated pod label update", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet

		pod := npaclServerPod(fr, "pod1", "", map[string]string{"type": "red"})
		Expect(pod.Status.PodIP).NotTo(BeEmpty())
		node := pod.Spec.NodeName
		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()

		By("creating an allow-ingress-from-red policy")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-ingress-to-red"},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"type": "red"}},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
				Ingress: []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"type": "red"}},
					}},
				}},
			},
		})

		By("waiting for the PodSelector address set to contain the pod IP")
		var asUUID string
		Eventually(func(g Gomega) {
			for _, as := range npaclAddressSetsForNS(fr, cs, ns) {
				if strings.Contains(npaclAddressSetAddresses(fr, cs, as), pod.Status.PodIP) {
					out, err := runOVNNBCTL(fr, cs, "--bare", "--no-heading", "--columns=_uuid", "list", "Address_Set", as)
					g.Expect(err).NotTo(HaveOccurred())
					asUUID = strings.TrimSpace(out)
				}
			}
			g.Expect(asUUID).NotTo(BeEmpty())
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())

		txnRegex := fmt.Sprintf("transacting operations.*%s.*%s", asUUID, pod.Status.PodIP)
		countTxns := func() int {
			logs, err := getOVNKubePodLogsFiltered(cs, ovnNs, node, txnRegex)
			if err != nil {
				framework.Logf("could not read ovnkube-node logs: %v", err)
				return -1
			}
			return strings.Count(logs, "\n")
		}

		By("recording the transaction count, then applying an unrelated label")
		before := countTxns()
		_, err := cs.CoreV1().Pods(ns).Patch(context.TODO(), pod.Name, "application/merge-patch+json",
			[]byte(`{"metadata":{"labels":{"unrelated-label":"test-update"}}}`), metav1.PatchOptions{})
		Expect(err).NotTo(HaveOccurred())
		time.Sleep(5 * time.Second)

		By("verifying no new transactions reference the address set")
		Expect(countTxns()).To(Equal(before),
			"an unrelated pod label update should not generate new address-set transactions")
	})
})

var _ = Describe("Network Policy: ACL recreation after ovnkube-node restart", feature.NetworkPolicy, Serial, func() {
	fr := wrappedTestFramework("netpol-acl-recreate")

	// assertNoPolicyFailureAfterRestart restarts the ovnkube-node pod on the
	// given node and asserts the ovnkube-controller log for the policy contains
	// no "failed" lines (i.e. the ACLs were recreated cleanly).
	assertNoPolicyFailureAfterRestart := func(cs clientset.Interface, ovnNs, node, ns, policyName string) {
		By(fmt.Sprintf("restarting ovnkube-node on node %s", node))
		Expect(restartOVNKubeNodePod(cs, ovnNs, node)).To(Succeed())

		By("verifying the policy was recreated without failures")
		filter := fmt.Sprintf(" %s/%s ", ns, policyName)
		Eventually(func(g Gomega) {
			logs, err := getOVNKubePodLogsFiltered(cs, ovnNs, node, regexp.QuoteMeta(filter))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).NotTo(ContainSubstring("failed"),
				"ovnkube-controller reported a failure recreating policy %s/%s", ns, policyName)
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())
	}

	// 64786: an allow-to-same-namespace policy in a 62-char namespace must be
	// recreated after an ovnkube-node restart (the long name must not cause the
	// ACLs to be treated as duplicates).
	It("should recreate a policy in a long-named namespace", func() {
		cs := fr.ClientSet
		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()
		longNs := "test-64786networkpolicy-with-a-62chars-62chars-long-namespace62"

		By("creating a namespace with a 62-character name")
		_, err := cs.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:   longNs,
				Labels: map[string]string{"kubernetes.io/metadata.name": longNs},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = cs.CoreV1().Namespaces().Delete(context.TODO(), longNs, metav1.DeleteOptions{})
		})

		By("creating a pod in the long-named namespace")
		pod := newAgnhostPod(longNs, "hello-pod", npaclNetexecCmd()...)
		_, err = cs.CoreV1().Pods(longNs).Create(context.TODO(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.TODO(), cs, "hello-pod", longNs)).To(Succeed())
		got, err := cs.CoreV1().Pods(longNs).Get(context.TODO(), "hello-pod", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("creating an allow-to-same-namespace policy")
		npaclCreatePolicy(cs, longNs, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-to-same-namespace"},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
				Egress: []knet.NetworkPolicyEgressRule{{
					To: []knet.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				}},
			},
		})

		assertNoPolicyFailureAfterRestart(cs, ovnNs, got.Spec.NodeName, longNs, "allow-to-same-namespace")
	})

	// 64787: a policy with duplicate egress ipBlock CIDRs must be recreated
	// after an ovnkube-node restart.
	It("should recreate a policy with duplicate egress ipBlock CIDRs", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet
		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()

		pod := npaclServerPod(fr, "hello-pod", "", nil)
		dupCIDR := "10.0.0.0/8"
		policyName := "ipblock-egress-dup"

		By("creating an egress policy with duplicate ipBlock CIDRs")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeEgress},
				Egress: []knet.NetworkPolicyEgressRule{{
					To: []knet.NetworkPolicyPeer{
						{IPBlock: &knet.IPBlock{CIDR: dupCIDR}},
						{IPBlock: &knet.IPBlock{CIDR: dupCIDR}},
						{IPBlock: &knet.IPBlock{CIDR: dupCIDR}},
					},
				}},
			},
		})

		assertNoPolicyFailureAfterRestart(cs, ovnNs, pod.Spec.NodeName, ns, policyName)
	})

	// 64788: a manually-injected duplicate ACL must be cleaned up by
	// ovnkube-controller after an ovnkube-node restart. All NB-DB operations are
	// pinned to the test pod's node (its interconnect zone) so the injection and
	// the restart act on the same NB DB. ACL names contain colons, which must be
	// escaped (\:) for ovn-nbctl's parser.
	It("should remove a manually-created duplicate ACL", func() {
		ns := fr.Namespace.Name
		cs := fr.ClientSet
		ovnNs := deploymentconfig.Get().OVNKubernetesNamespace()
		policyName := "ipblock-64788"

		pod := npaclServerPod(fr, "test-pod", "", nil)
		node := pod.Spec.NodeName
		nbctl := func(args ...string) (string, error) {
			return npaclNBCTLOnNode(fr, cs, ovnNs, node, args...)
		}

		By("creating an ingress ipBlock policy")
		npaclCreatePolicy(cs, ns, &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: knet.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []knet.PolicyType{knet.PolicyTypeIngress},
				Ingress: []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{IPBlock: &knet.IPBlock{CIDR: "10.1.0.0/16"}}},
				}},
			},
		})

		aclNamePrefix := fmt.Sprintf("NP:%s:%s", ns, policyName)
		var aclUUID, pgUUID, aclMatch, aclPrio, aclMeter string

		By("locating the policy's ACL and its port group")
		Eventually(func(g Gomega) {
			out, err := nbctl("--format=csv", "--no-heading",
				"--columns=_uuid,name,match,priority,meter", "find", "ACL")
			g.Expect(err).NotTo(HaveOccurred())
			reader := csv.NewReader(strings.NewReader(out))
			reader.FieldsPerRecord = -1
			records, err := reader.ReadAll()
			g.Expect(err).NotTo(HaveOccurred())
			aclUUID = ""
			for _, rec := range records {
				if len(rec) >= 5 && strings.Contains(rec[1], aclNamePrefix) {
					aclUUID, aclMatch, aclPrio, aclMeter = rec[0], rec[2], rec[3], rec[4]
					break
				}
			}
			g.Expect(aclUUID).NotTo(BeEmpty(), "could not find ACL for policy %s", policyName)

			pg, err := nbctl("--bare", "--no-heading", "--columns=_uuid",
				"find", "Port_Group", fmt.Sprintf("acls{>=}%s", aclUUID))
			g.Expect(err).NotTo(HaveOccurred())
			pgUUID = strings.TrimSpace(strings.Split(pg, "\n")[0])
			g.Expect(pgUUID).NotTo(BeEmpty(), "could not find port group for ACL %s", aclUUID)
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())

		// Use a colon-free name for the injected ACL: ovn-nbctl's column parser
		// chokes on ':' in a bare value, and ovnkube-controller removes foreign
		// ACLs from a port group it owns by reconciliation regardless of name.
		dupNameArg := fmt.Sprintf("dupacl-%s", policyName)

		By("injecting a duplicate ACL into the port group")
		_, err := nbctl(
			"--id=@copyacl", "create", "acl",
			"name="+dupNameArg,
			fmt.Sprintf("priority=%s", aclPrio),
			"direction=to-lport", "action=allow-related",
			fmt.Sprintf("match=%s", aclMatch),
			fmt.Sprintf("meter=%s", aclMeter),
			"--", "add", "Port_Group", pgUUID, "acls", "@copyacl")
		Expect(err).NotTo(HaveOccurred())

		By("confirming the duplicate ACL exists")
		dup, err := nbctl("--bare", "--no-heading", "--columns=_uuid",
			"find", "ACL", "name="+dupNameArg)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(dup)).NotTo(BeEmpty())

		By("restarting ovnkube-node and verifying the duplicate ACL is removed")
		Expect(restartOVNKubeNodePod(cs, ovnNs, node)).To(Succeed())
		Eventually(func(g Gomega) {
			out, err := nbctl("--bare", "--no-heading", "--columns=_uuid",
				"find", "ACL", "name="+dupNameArg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "duplicate ACL should have been removed")
		}, npaclPollTimeout, npaclPollInterval).Should(Succeed())
	})
})
