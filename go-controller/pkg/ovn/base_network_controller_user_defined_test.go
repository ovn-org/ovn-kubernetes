// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	gotesting "testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type failSecondPodPortDeleteClient struct {
	libovsdbclient.Client
	portDeletes int
}

// Transact fails the second port deletion, regardless of NAD iteration order.
func (c *failSecondPodPortDeleteClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	deletesPort := false
	for _, op := range ops {
		if op.Op == ovsdb.OperationDelete && op.Table == nbdb.LogicalSwitchPortTable {
			deletesPort = true
			if c.portDeletes > 0 {
				return nil, fmt.Errorf("injected second NAD cleanup failure")
			}
		}
	}
	results, err := c.Client.Transact(ctx, ops...)
	if err == nil && deletesPort {
		c.portDeletes++
	}
	return results, err
}

var _ = Describe("BaseUserDefinedNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	DescribeTable("preserves reused IPs after partial multi-NAD pod cleanup", func(reattach bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode, config.IPv6Mode = true, true
		const networkName, namespace, nodeName = "bluenet", "greenamespace", "node-a"
		nadKeys := []string{namespace + "/rednad", namespace + "/orangenad"}
		nads := &nettypes.NetworkAttachmentDefinitionList{}
		for _, name := range []string{"rednad", "orangenad"} {
			nad := ovntest.GenerateNAD(networkName, name, namespace,
				types.Layer3Topology, "100.128.0.0/16,fd00::/48", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			nads.Items = append(nads.Items, *nad)
		}
		podA := ovntest.NewPod(namespace, "pod-a", nodeName, "10.128.0.3")
		podA.UID = "pod-a-uid"
		podA.Status.Phase = corev1.PodRunning
		podA.Annotations = map[string]string{nettypes.NetworkAttachmentAnnot: nadKeys[0] + "," + nadKeys[1]}
		var err error
		for i, nadKey := range nadKeys {
			ips := ovntest.MustParseIPNets(fmt.Sprintf("100.128.0.%d/24", i+3), fmt.Sprintf("fd00::%d/64", i+3))
			podA.Annotations, err = util.MarshalPodAnnotation(podA.Annotations, &util.PodAnnotation{
				MAC: util.IPAddrToHWAddr(ips[0].IP), IPs: ips, Role: types.NetworkRoleSecondary,
			}, nadKey)
			Expect(err).NotTo(HaveOccurred())
		}

		switchName := util.GetUserDefinedNetworkPrefix(networkName) + nodeName
		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			podA, ovntest.NewNamespace(namespace), newNode(nodeName, "192.0.2.10/24"), nads,
		)
		DeferCleanup(fakeOVN.shutdown)
		bnc := fakeOVN.userDefinedNetworkControllers[networkName].bnc
		for _, nadKey := range nadKeys {
			Eventually(func() string {
				return bnc.networkManager.GetNetworkNameForNADKey(nadKey)
			}).Should(Equal(networkName))
		}
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/24", "fd00::/64"), nil, nil)).To(Succeed())
		podPorts := func(pod *corev1.Pod) ([]*nbdb.LogicalSwitchPort, error) {
			return libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
				return lsp.Options["iface-id-ver"] == string(pod.UID)
			})
		}

		By("programming both of pod A's attachments through the controller")
		Expect(bnc.ensurePodForUserDefinedNetwork(podA, true)).To(Succeed())
		podA, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), podA.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		portInfoMap, err := bnc.logicalPortCache.getAll(podA)
		Expect(err).NotTo(HaveOccurred())
		Expect(portInfoMap).To(HaveLen(2))
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(2))

		By("releasing one attachment before cleanup of the second attachment fails")
		bnc.nbClient = &failSecondPodPortDeleteClient{Client: fakeOVN.nbClient}
		err = bnc.removePodForUserDefinedNetwork(podA, portInfoMap)
		Expect(err).To(MatchError(ContainSubstring("injected second NAD cleanup failure")))
		bnc.nbClient = fakeOVN.nbClient
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(1))
		remainingPorts, err := podPorts(podA)
		Expect(err).NotTo(HaveOccurred())
		var releasedNAD string
		for nadKey, info := range portInfoMap {
			if info.uuid != remainingPorts[0].UUID {
				releasedNAD = nadKey
			}
		}
		Expect(releasedNAD).NotTo(BeEmpty())
		annotationA, err := util.UnmarshalPodAnnotation(podA.Annotations, releasedNAD)
		Expect(err).NotTo(HaveOccurred())

		By("allocating the freed IPs and programming a logical port for pod B")
		podB := ovntest.NewPod(namespace, "pod-b", nodeName, "10.128.0.4")
		podB.UID = "pod-b-uid"
		podB.Status.Phase = corev1.PodRunning
		podB.Annotations = map[string]string{nettypes.NetworkAttachmentAnnot: releasedNAD}
		podB, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Create(context.Background(), podB, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (*corev1.Pod, error) { return fakeOVN.watcher.GetPod(namespace, podB.Name) }).ShouldNot(BeNil())
		Expect(bnc.ensurePodForUserDefinedNetwork(podB, true)).To(Succeed())
		podB, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), podB.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		annotationB, err := util.UnmarshalPodAnnotation(podB.Annotations, releasedNAD)
		Expect(err).NotTo(HaveOccurred())
		Expect(annotationB.IPs).To(Equal(annotationA.IPs))
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podB) }).Should(HaveLen(1))
		ownerPorts, err := podPorts(podB)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerPorts[0].Addresses).To(ConsistOf(annotationB.MAC.String() + " " + util.JoinIPNetIPs(annotationB.IPs, " ")))

		By("rejecting pod A's stale reattachment without creating a duplicate-IP logical port")
		err = bnc.ensurePodForUserDefinedNetwork(podA, true)
		Expect(err).To(HaveOccurred())
		Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
		Expect(podPorts(podA)).To(HaveLen(1))

		if reattach {
			By("keeping pod B's reservation until its cleanup completes")
			Expect(bnc.removePodForUserDefinedNetwork(podB, nil)).To(Succeed())
			By("reattaching pod A only after it can reacquire the IPs")
			Expect(bnc.ensurePodForUserDefinedNetwork(podA, true)).To(Succeed())
			Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(2))
			portInfoMap, err = bnc.logicalPortCache.getAll(podA)
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.removePodForUserDefinedNetwork(podA, portInfoMap)).To(Succeed())
		} else {
			By("retrying pod A's cleanup without releasing pod B's reservation")
			Expect(bnc.removePodForUserDefinedNetwork(podA, portInfoMap)).To(Succeed())
			for _, ip := range annotationB.IPs {
				Expect(ipallocator.IsErrAllocated(bnc.lsManager.AllocateIPs(switchName, "test-owner", []*net.IPNet{ip}))).To(
					BeTrue(), "pod A's cleanup retry must preserve pod B's reservation for %s", ip)
			}
			Expect(podPorts(podB)).To(Equal(ownerPorts))
			Expect(bnc.removePodForUserDefinedNetwork(podB, nil)).To(Succeed())
		}

		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(BeEmpty())
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podB) }).Should(BeEmpty())
		for _, info := range portInfoMap {
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", info.ips)).To(Succeed(), "final cleanup must release every attachment's IPs")
		}
	},
		Entry("cleanup retries preserve the new owner's IPs", false),
		Entry("reattachment requires successful IP reacquisition", true),
	)

	DescribeTable("restores ownership after a completed pod's IPs are reused", func(defaultNetwork, completedOwner, ownerFirst bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode, config.IPv6Mode = true, true
		const namespace, nodeName, networkName = "greenamespace", "node-a", "bluenet"
		nadKey, switchName := namespace+"/rednad", util.GetUserDefinedNetworkPrefix(networkName)+nodeName
		role := types.NetworkRoleSecondary
		ips := ovntest.MustParseIPNets("100.128.0.3/24", "fd00::3/64")
		subnets := ovntest.MustParseIPNets("100.128.0.0/24", "fd00::/64")
		if defaultNetwork {
			nadKey, switchName, role = types.DefaultNetworkName, nodeName, types.NetworkRolePrimary
			ips = ovntest.MustParseIPNets("10.128.1.3/24", "fd00::3/64")
			subnets = ovntest.MustParseIPNets("10.128.1.0/24", "fd00::/64")
		}
		annotation := &util.PodAnnotation{IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP), Role: role}
		stale := ovntest.NewPod(namespace, "old-pod", nodeName, ips[0].IP.String())
		stale.Status.Phase = corev1.PodRunning
		stale.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(100, 0))}}
		stale.Annotations = map[string]string{}
		if !defaultNetwork {
			stale.Annotations[nettypes.NetworkAttachmentAnnot] = nadKey
		}
		var err error
		stale.Annotations, err = util.MarshalPodAnnotation(stale.Annotations, annotation, nadKey)
		Expect(err).NotTo(HaveOccurred())
		nad := ovntest.GenerateNAD(networkName, "rednad", namespace, types.Layer3Topology,
			"100.128.0.0/16,fd00::/48", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		fakeOVN := NewFakeOVN(true, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{Name: switchName},
		}}, stale, ovntest.NewNamespace(namespace), newNode(nodeName, "192.0.2.10/24"),
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nad}})
		DeferCleanup(fakeOVN.shutdown)
		var bnc *BaseNetworkController
		var syncPods func([]interface{}) error
		var removePod func(*corev1.Pod) error
		var addPod func(*corev1.Pod) error
		if defaultNetwork {
			bnc = &fakeOVN.controller.BaseNetworkController
			syncPods = fakeOVN.controller.syncPods
			removePod = func(p *corev1.Pod) error { return fakeOVN.controller.removePod(p, nil) }
			addPod = fakeOVN.controller.addLogicalPort
			Expect(fakeOVN.controller.WatchNamespaces()).To(Succeed())
		} else {
			controller := fakeOVN.userDefinedNetworkControllers[networkName].bnc
			bnc = &controller.BaseNetworkController
			syncPods = controller.syncPodsForUserDefinedNetwork
			removePod = func(p *corev1.Pod) error { return controller.removePodForUserDefinedNetwork(p, nil) }
			addPod = func(p *corev1.Pod) error { return controller.ensurePodForUserDefinedNetwork(p, true) }
			Eventually(func() string { return bnc.networkManager.GetNetworkNameForNADKey(nadKey) }).Should(Equal(networkName))
		}
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, subnets, nil, nil)).To(Succeed())
		By("completing the first pod and releasing its allocation without removing its annotation")
		Expect(addPod(stale)).To(Succeed())
		stale.Status.Phase = corev1.PodSucceeded
		stale, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).UpdateStatus(context.Background(), stale, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(removePod(stale)).To(Succeed())
		By("allocating the freed addresses to a different pod")
		owner := ovntest.NewPod(namespace, "owner", nodeName, "")
		owner.Status.Phase = corev1.PodRunning
		owner.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(200, 0))}}
		if !defaultNetwork {
			owner.Annotations = map[string]string{nettypes.NetworkAttachmentAnnot: nadKey}
		}
		owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Create(context.Background(), owner, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (*corev1.Pod, error) { return bnc.watchFactory.GetPod(namespace, owner.Name) }).ShouldNot(BeNil())
		Expect(addPod(owner)).To(Succeed())
		owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), owner.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		ownerAnnotation, err := util.UnmarshalPodAnnotation(owner.Annotations, nadKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerAnnotation.IPs).To(Equal(ips))
		if completedOwner {
			owner.Status.Phase = corev1.PodSucceeded
			owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).UpdateStatus(context.Background(), owner, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("restarting IPAM before the second pod is cleaned up")
		// No pod handlers are running in this test. Keep the API and OVN
		// state from the lifecycle above, but rebuild the in-memory allocator.
		bnc.lsManager = logicalswitchmanager.NewLogicalSwitchManager()
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, subnets, nil, nil)).To(Succeed())
		Expect(syncPods([]interface{}{stale, owner})).To(Succeed())
		Expect(syncPods([]interface{}{owner, stale})).To(Succeed(), "startup retries must preserve the selected owner")
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(owner, nadKey), ips)).To(BeTrue())
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(stale, nadKey), ips)).To(BeFalse())
		first, last := stale, owner
		if ownerFirst {
			first, last = owner, stale
		}
		By("cleaning up both pods without releasing the current owner early or leaking its allocation")
		Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Delete(context.Background(), owner.Name, metav1.DeleteOptions{})).To(Succeed())
		Expect(removePod(first)).To(Succeed())
		if !ownerFirst {
			Expect(ipallocator.IsErrAllocated(bnc.lsManager.AllocateIPs(switchName, "test-owner", ips))).To(BeTrue())
		}
		Expect(removePod(last)).To(Succeed())
		Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", ips)).To(Succeed(), "final cleanup must not leak the reservation")
	},
		Entry("default, running owner, stale first", true, false, false),
		Entry("default, running owner, owner first", true, false, true),
		Entry("default, completed owner, stale first", true, true, false),
		Entry("default, completed owner, owner first", true, true, true),
		Entry("UDN, running owner, stale first", false, false, false),
		Entry("UDN, running owner, owner first", false, false, true),
		Entry("UDN, completed owner, stale first", false, true, false),
		Entry("UDN, completed owner, owner first", false, true, true),
	)

	type dhcpTest struct {
		vmName                string
		ips                   []string
		dns                   []string
		expectedDHCPv4Options *nbdb.DHCPOptions
		expectedDHCPv6Options *nbdb.DHCPOptions
	}
	DescribeTable("with layer2 primary UDN when configuring DHCP", func(t dhcpTest) {
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fakeOVN := NewFakeOVN(true, "worker1")
		lsp := &nbdb.LogicalSwitchPort{
			Name: "vm-port",
			UUID: "vm-port-UUID",
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  "layer2-switch-UUID",
			Name:  "layer2-switch",
			Ports: []string{lsp.UUID},
		}

		initialDB := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
			},
		}
		fakeOVN.startWithDBSetup(
			initialDB,
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "kube-system",
					Name:      "kube-dns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: t.dns,
				},
			},
		)
		defer fakeOVN.shutdown()

		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "dummy",
				Labels: map[string]string{
					kubevirtv1.AppLabel:                "virt-launcher",
					kubevirtv1.VirtualMachineNameLabel: t.vmName,
				},
				Annotations: map[string]string{
					kubevirtv1.DomainAnnotation: t.vmName,
				},
			},
		}
		ips, err := util.ParseIPNets(t.ips)
		Expect(err).ToNot(HaveOccurred())
		podAnnotation := &util.PodAnnotation{
			IPs: ips,
		}
		Expect(controller.bnc.ensureDHCP(pod, podAnnotation, lsp)).To(Succeed())
		expectedDB := []libovsdbtest.TestData{}

		By("asserting the OVN entities provisioned in the NBDB are the expected ones")
		expectedLSP := lsp.DeepCopy()
		if t.expectedDHCPv4Options != nil {
			t.expectedDHCPv4Options.UUID = "vm1-dhcpv4-UUID"
			expectedLSP.Dhcpv4Options = &t.expectedDHCPv4Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv4Options)
		}
		if t.expectedDHCPv6Options != nil {
			t.expectedDHCPv6Options.UUID = "vm1-dhcpv6-UUID"
			expectedLSP.Dhcpv6Options = &t.expectedDHCPv6Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv6Options)
		}
		// Refresh logical switch to have the propert ports uuid
		obtainedLogicalSwitches := []*nbdb.LogicalSwitch{}
		Expect(fakeOVN.nbClient.List(context.Background(), &obtainedLogicalSwitches)).To(Succeed())
		expectedDB = append(expectedDB,
			obtainedLogicalSwitches[0],
			expectedLSP,
		)
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDB))

	},
		Entry("for ipv4 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100"},
			ips:    []string{"192.168.100.4/24"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
		}),
		Entry("for ipv6 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"2015:100:200::10"},
			ips:    []string{"2010:100:200::2/60"},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
		Entry("for dualstack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100", "2015:100:200::10"},
			ips:    []string{"192.168.100.4/24", "2010:100:200::2/60"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
	)
	Context("enableSourceLSPFailedLiveMigration", func() {
		const (
			vmName        = "test-vm"
			nadKey        = "awips/mgmt"
			localNodeName = "node-local"
		)

		newVirtLauncherPod := func(name, nodeName string, phase corev1.PodPhase, annotations map[string]string) *corev1.Pod {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[kubevirtv1.DomainAnnotation] = vmName
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "awips",
					Labels: map[string]string{
						kubevirtv1.AppLabel:                "virt-launcher",
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					CreationTimestamp: metav1.Time{Time: time.Now()},
					Annotations:       annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "kubevirt.io/v1",
						Kind:       "VirtualMachineInstance",
						Name:       vmName,
					}},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
				Status: corev1.PodStatus{
					Phase: phase,
				},
			}
			return pod
		}

		setupControllerWithDBSetup := func(dbSetup *libovsdbtest.TestSetup, pods ...*corev1.Pod) (*BaseUserDefinedNetworkController, *FakeOVN) {
			localnetNAD := ovntest.GenerateNAD("mgmt", "mgmt", "awips",
				types.LocalnetTopology, "", types.NetworkRoleSecondary)

			fakeOVN := NewFakeOVN(false, localNodeName)
			objs := []runtime.Object{}
			for _, p := range pods {
				objs = append(objs, p)
			}
			if dbSetup != nil {
				fakeOVN.startWithDBSetup(*dbSetup, objs...)
			} else {
				fakeOVN.start(objs...)
			}
			DeferCleanup(fakeOVN.shutdown)

			Expect(fakeOVN.NewUserDefinedNetworkController(localnetNAD)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["mgmt"]
			Expect(ok).To(BeTrue())

			controller.bnc.nodeName = localNodeName

			return controller.bnc, fakeOVN
		}

		setupController := func(pods ...*corev1.Pod) *BaseUserDefinedNetworkController {
			bnc, _ := setupControllerWithDBSetup(nil, pods...)
			return bnc
		}

		It("should skip source LSP re-enable when source pod is on a remote node", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			// Target pod is local, failed (completed) — triggers LiveMigrationFailed detection
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			// Make target created after source so DiscoverLiveMigrationStatus picks it as target
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			// Call with empty IPs (IPAM-less localnet) — this would fail without the locality guard
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod is on a remote node")
		})

		It("should skip source LSP re-enable when source pod LSP is not local", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodSucceeded, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod LSP is not local")
		})

		It("should re-enable source LSP when source pod is local and migration failed", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePodName := "virt-launcher-" + vmName + "-source"
			sourcePod := newVirtLauncherPod(sourcePodName, localNodeName, corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			// Build LSP and switch names matching what the controller will compute:
			//   LSP name: GetUserDefinedNetworkLogicalPortName(namespace, podName, nadKey)
			//   Switch name: GetNetworkScopedSwitchName(OVNLocalnetSwitch)
			sourceLSPName := util.GetUserDefinedNetworkLogicalPortName(sourcePod.Namespace, sourcePodName, nadKey)
			sourceLSP := &nbdb.LogicalSwitchPort{
				UUID:    sourceLSPName + "-UUID",
				Name:    sourceLSPName,
				Enabled: ptr.To(false),
			}
			switchName := util.GetUserDefinedNetworkPrefix("mgmt") + types.OVNLocalnetSwitch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: []string{sourceLSP.UUID},
			}

			dbSetup := &libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					sourceLSP,
				},
			}

			bnc, fakeOVN := setupControllerWithDBSetup(dbSetup, sourcePod, targetPod)

			mac := "0a:58:0a:80:00:05"
			ips := []string{"10.128.0.5/24"}
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, mac, ips)
			Expect(err).NotTo(HaveOccurred(), "should re-enable source LSP without error")

			// Verify the LSP was updated: Enabled=true and addresses set
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      sourceLSP.UUID,
				Name:      sourceLSPName,
				Enabled:   ptr.To(true),
				Addresses: []string{mac + " 10.128.0.5"},
			}
			expectedSwitch := logicalSwitch.DeepCopy()
			Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedSwitch, expectedLSP))
		})
	})

	It("should not fail to sync pods if namespace is gone", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false, "worker1")
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager instead of a fake one, so getActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForNode("worker1", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// simulate that we listed the pod, but namespace was deleted after
		podWithNoNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "doesnotexist",
				Name:      "dummy",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podWithNoNamespace)

		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not fail to sync pods if namespace has primary UDN label but NAD not ready", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false, "worker1")
		// Create namespace with primary UDN label but no NAD
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Labels: map[string]string{
					types.RequiredUDNNamespaceLabel: "",
				},
			},
		}
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
			namespace,
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager so GetActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForNode("worker1", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// Pod in namespace with primary UDN label but no NAD causes InvalidPrimaryNetworkError
		podInLabeledNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-pod",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podInLabeledNamespace)

		// Should skip pod without error when GetActiveNetworkForNamespace returns InvalidPrimaryNetworkError
		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("picks up an already-running remote pod when its namespace newly joins a running primary UDN", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		const (
			localNode    = "node-local"
			newNamespace = "greenamespace"
		)
		// bluenet is already running, serving a different namespace
		initialNad := ovntest.GenerateNAD("bluenet", "initialnad", "bluenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)

		remotePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: newNamespace, Name: "remote-running"},
			Spec:       corev1.PodSpec{NodeName: "node-remote"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		namespaceObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: newNamespace}}

		fakeOVN := NewFakeOVN(false, localNode)
		fakeOVN.start(namespaceObj, remotePod)
		DeferCleanup(fakeOVN.shutdown)

		Expect(fakeOVN.NewUserDefinedNetworkController(initialNad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		bnc := controller.bnc

		// bluenet now also picks up greenamespace, where remotePod is already Running
		afterInfo := util.NewMutableNetInfo(bnc.GetNetInfo())
		afterInfo.AddNADs(util.GetNADName(newNamespace, "rednad"))
		Expect(bnc.reconcile(afterInfo, func(string) {})).To(Succeed())

		key, err := retry.GetResourceKey(remotePod)
		Expect(err).NotTo(HaveOccurred())
		retry.CheckRetryObjectEventually(key, true, bnc.retryPods)
	})

	Context("when shouldFilterNamespace does not filter a pod because the namespace is missing from the informer", func() {
		const (
			missingNamespace = "greenamespace"
			podName          = "dummy"
			localNode        = "worker1"
		)

		var (
			bnc     *BaseUserDefinedNetworkController
			fakeOVN *FakeOVN
			pod     *corev1.Pod
		)

		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.IPv4Mode = true
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "dummy-pod-uid",
					Namespace: missingNamespace,
					Name:      podName,
				},
				Spec: corev1.PodSpec{
					NodeName: localNode,
				},
			}
			fakeOVN = NewFakeOVN(false, localNode)
			fakeOVN.start(
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: localNode,
						Annotations: map[string]string{
							"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
							util.OvnNodeChassisID:     chassisIDForNode(localNode),
						},
					},
				},
				pod,
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
			Expect(ok).To(BeTrue())
			bnc = controller.bnc
			Expect(bnc.shouldFilterNamespace(missingNamespace)).To(BeFalse())
		})

		DescribeTable("initializes and cleans up a namespace when its UDN pod arrives first", func(namespaceReadyDuringRetry bool) {
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			err := bnc.ensurePodForUserDefinedNetwork(pod, true)
			Expect(err).To(MatchError(ContainSubstring("failed to get primary network namespace NAD")))
			Expect(err).To(MatchError(apierrors.IsNotFound, "IsNotFound"))

			namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Create(
				context.Background(),
				newUDNNamespace(missingNamespace),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			nadToCreate := nad.DeepCopy()
			ovntest.AnnotateNADWithNetworkID("3", nadToCreate)
			_, err = fakeOVN.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadToCreate.Namespace).Create(
				context.Background(), nadToCreate, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nadKey := util.GetNADName(nad.Namespace, nad.Name)
			nodeSubnet := ovntest.MustParseIPNet("100.128.0.0/24")
			switchName := bnc.GetNetworkScopedSwitchName(localNode)
			Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, []*net.IPNet{nodeSubnet}, nil, nil)).To(Succeed())
			Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, &nbdb.LogicalSwitch{Name: switchName})).To(Succeed())

			Eventually(func() (string, error) {
				return bnc.networkManager.GetPrimaryNADForNamespace(missingNamespace)
			}).WithTimeout(5 * time.Second).Should(Equal(nadKey))
			pgName := bnc.getNamespacePortGroupName(missingNamespace)
			Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace), "pod setup must not initialize namespace state")
			lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
			initializeNamespace := func() {
				Expect(bnc.AddNamespaceForUserDefinedNetwork(namespace)).To(Succeed())
				Eventually(func() error {
					_, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
					return err
				}).WithTimeout(5 * time.Second).Should(Succeed())
			}
			if namespaceReadyDuringRetry {
				// Run the namespace handler only after the pod's first lookup has
				// observed the missing group. The same pod setup call must recover.
				bnc.nbClient = &namespacePortGroupLookupClient{
					Client: fakeOVN.nbClient, afterFirstLookup: initializeNamespace,
				}
			} else {
				Expect(bnc.ensurePodForUserDefinedNetwork(pod, true)).To(MatchError(ContainSubstring("failed to add pod port")))
				_, err = libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				Expect(err).To(MatchError(libovsdbclient.ErrNotFound))
				Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace))
				_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
				Expect(err).To(MatchError(libovsdbclient.ErrNotFound), "failed membership must not commit the pod port")
				Eventually(func() bool {
					currentPod, err := fakeOVN.watcher.GetPod(pod.Namespace, pod.Name)
					if err != nil {
						return false
					}
					annotation, err := util.UnmarshalPodAnnotation(currentPod.Annotations, nadKey)
					return err == nil && util.IsValidPodAnnotation(annotation)
				}).WithTimeout(5 * time.Second).Should(BeTrue())
				initializeNamespace()
			}

			Expect(bnc.ensurePodForUserDefinedNetwork(pod, true)).To(Succeed())
			bnc.nbClient = fakeOVN.nbClient
			nsInfo, unlock := bnc.getNamespaceLocked(missingNamespace, true)
			Expect(nsInfo).NotTo(BeNil())
			initializedPGName := nsInfo.portGroupName
			unlock()
			Expect(initializedPGName).To(Equal(pgName))

			updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Get(
				context.Background(), pod.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			podAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(podAnnotation.IPs).To(HaveLen(1))
			Expect(nodeSubnet.Contains(podAnnotation.IPs[0].IP)).To(BeTrue())
			Expect(podAnnotation.MAC).NotTo(BeEmpty())
			Expect(podAnnotation.Role).To(Equal(types.NetworkRolePrimary))

			lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(HaveLen(1))
			Expect(lsps[0].Addresses).To(ConsistOf(fmt.Sprintf("%s %s", podAnnotation.MAC, podAnnotation.IPs[0].IP)))
			Eventually(func() (*nbdb.PortGroup, error) {
				return libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
			}).WithTimeout(5 * time.Second).Should(HaveField("Ports", ConsistOf(lsps[0].UUID)))

			By("letting namespace teardown remove the group before pod cleanup")
			Expect(bnc.deleteNamespaceForUserDefinedNetwork(namespace)).To(Succeed())
			Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace))
			Eventually(func() error {
				_, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				return err
			}).WithTimeout(5 * time.Second).Should(MatchError(libovsdbclient.ErrNotFound))
			Expect(bnc.removePodForUserDefinedNetwork(updatedPod, nil)).To(Succeed())
			Eventually(fakeOVN.nbClient).WithTimeout(5 * time.Second).Should(libovsdbtest.HaveDataIgnoringUUIDs(
				&nbdb.LogicalSwitch{Name: switchName},
			))
		},
			Entry("namespace becomes ready within the local retry", true),
			Entry("missing namespace handler falls back to a later pod retry", false),
		)

		It("removePod deletes this network's logical port while the namespace is missing", func() {
			nadKey := util.GetNADName(nad.Namespace, nad.Name)
			bnc.networkManager = &nadKeyNameOverlay{
				Interface:    bnc.networkManager,
				nadToNetwork: map[string]string{nadKey: bnc.GetNetworkName()},
			}

			annotations, err := util.MarshalPodAnnotation(nil, &util.PodAnnotation{
				MAC:  net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0x00, 0x05},
				IPs:  ovntest.MustParseIPNets("100.128.0.5/24"),
				Role: types.NetworkRolePrimary,
			}, nadKey)
			Expect(err).NotTo(HaveOccurred())
			pod.Annotations = annotations

			switchName := bnc.GetNetworkScopedSwitchName(localNode)
			lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
			ls := &nbdb.LogicalSwitch{Name: switchName}
			lsp := &nbdb.LogicalSwitchPort{Name: lspName}
			Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, ls)).To(Succeed())
			Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(fakeOVN.nbClient, ls, lsp)).To(Succeed())

			lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(HaveLen(1))
			Expect(lsps[0]).To(Equal(lsp))

			Expect(bnc.removePodForUserDefinedNetwork(pod, map[string]*lpInfo{
				nadKey: {
					name:          lspName,
					uuid:          lsp.UUID,
					logicalSwitch: switchName,
				},
			})).To(Succeed())
			lsps, err = libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(BeEmpty())
		})
	})

	It("removes a cached pod port after its NAD mapping is deleted", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		netInfo, err := util.ParseNADInfo(layer2NAD)
		Expect(err).NotTo(HaveOccurred())

		const (
			nodeName      = "worker1"
			podName       = "pod1"
			nadKey        = "greenamespace/rednad"
			foreignNADKey = "greenamespace/orangenad"
			foreignSwitch = "orangenet_ovn-layer2-switch"
		)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "greenamespace",
				Name:      podName,
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
		podIPs, err := util.ParseIPNets([]string{"100.128.0.2/16"})
		Expect(err).NotTo(HaveOccurred())
		podMAC, err := net.ParseMAC("0a:58:64:80:00:02")
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, nadKey)
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, foreignNADKey)
		Expect(err).NotTo(HaveOccurred())

		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		lspName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, nadKey)
		foreignLSPName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, foreignNADKey)
		lsp := &nbdb.LogicalSwitchPort{UUID: lspName + "-UUID", Name: lspName}
		foreignLSP := &nbdb.LogicalSwitchPort{
			UUID: foreignLSPName + "-UUID",
			Name: foreignLSPName,
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  switchName + "-UUID",
			Name:  switchName,
			Ports: []string{lsp.UUID},
		}
		foreignLogicalSwitch := &nbdb.LogicalSwitch{
			UUID:  foreignSwitch + "-UUID",
			Name:  foreignSwitch,
			Ports: []string{foreignLSP.UUID},
		}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
				foreignLogicalSwitch,
				foreignLSP,
			},
		}, pod)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		controller.bnc.networkManager = (&networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{},
			NADNetworks:     map[string]util.NetInfo{},
		}).Interface()

		portInfoMap := map[string]*lpInfo{
			nadKey: fakeOVN.portCache.add(
				pod, switchName, nadKey, lsp.UUID, podMAC, podIPs),
			foreignNADKey: fakeOVN.portCache.add(
				pod, foreignSwitch, foreignNADKey,
				foreignLSP.UUID, podMAC, podIPs),
		}
		Expect(controller.bnc.removePodForUserDefinedNetwork(
			pod, portInfoMap)).To(Succeed())

		expectedSwitch := logicalSwitch.DeepCopy()
		expectedSwitch.Ports = nil
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(
			expectedSwitch,
			foreignLogicalSwitch,
			foreignLSP,
		))
	})

	It("processes a primary UDN pod delete while its namespace exists after the NAD mapping is deleted", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(layer2NAD)
		Expect(err).NotTo(HaveOccurred())

		const (
			nodeName = "worker1"
			podName  = "pod1"
			nadKey   = "greenamespace/rednad"
		)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "greenamespace",
				Name:      podName,
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
		podIPs, err := util.ParseIPNets([]string{"100.128.0.2/16"})
		Expect(err).NotTo(HaveOccurred())
		podMAC, err := net.ParseMAC("0a:58:64:80:00:02")
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, nadKey)
		Expect(err).NotTo(HaveOccurred())

		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID",
			Name: switchName,
		}
		lspName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, nadKey)
		lsp := &nbdb.LogicalSwitchPort{UUID: lspName + "-UUID", Name: lspName}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{logicalSwitch},
		})
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		fakeNetworkManager := &networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{pod.Namespace: netInfo},
			NADNetworks:     map[string]util.NetInfo{},
		}
		controller.bnc.networkManager = fakeNetworkManager.Interface()
		Expect(controller.bnc.WatchPods()).To(Succeed())

		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).
			Create(context.Background(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := fakeOVN.watcher.GetPod(pod.Namespace, pod.Name)
			return err
		}).Should(Succeed())

		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(
			fakeOVN.nbClient, &nbdb.LogicalSwitch{Name: switchName}, lsp)).To(Succeed())
		fakeOVN.portCache.add(pod, switchName, nadKey, lsp.UUID, podMAC, podIPs)
		_, err = fakeNetworkManager.GetPrimaryNADForNamespace(pod.Namespace)
		Expect(err).To(MatchError(util.IsInvalidPrimaryNetworkError, "IsInvalidPrimaryNetworkError"))
		Expect(controller.bnc.FilterOutResource(factory.PodType, pod)).To(BeTrue())

		Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).
			Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(logicalSwitch))
	})

})

func TestAdvertisedSharedGatewaySNATUsesLiveAllowedExtIPSets(t *gotesting.T) {
	for _, outboundSNAT := range []string{types.NoOverlaySNATDisabled, types.NoOverlaySNATEnabled} {
		t.Run(outboundSNAT, func(t *gotesting.T) {
			bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, outboundSNAT, config.GatewayModeShared)
			seedAdvertisedSNATAddressSets(t, asf)

			expectAdvertisedSNATUsesLiveAllowedExtIPs(t, bsnc, asf, localPodSubnets)
		})
	}
}

func TestAdvertisedSharedGatewaySNATFailsWithoutAllowedExtIPsForFamily(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeShared)
	config.IPv6Mode = false
	seedAdvertisedSNATAddressSets(t, asf)

	g := NewWithT(t)
	_, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).To(MatchError(ContainSubstring(
		"failed to build allowed_ext_ips SNAT for advertised network bluenet, subnet ae70::/64: no address set UUID for IPv6",
	)))
}

func TestAdvertisedLocalGatewaySNATUsesDestinationMatch(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeLocal)
	seedAdvertisedSNATAddressSets(t, asf)

	expectAdvertisedSNATUsesDestinationMatch(t, bsnc, asf, localPodSubnets)
}

func newAdvertisedSNATTestController(
	t *gotesting.T,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	return newAdvertisedSNATTestControllerForTopology(
		t,
		types.Layer3Topology,
		"100.128.0.0/16/24,ae70::/60/64",
		outboundSNAT,
		gatewayMode,
		ovntest.MustParseIPNets("100.128.0.0/24", "ae70::/64"),
	)
}

func newAdvertisedSNATTestControllerForTopology(
	t *gotesting.T,
	topology string,
	cidrs string,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
	localPodSubnets []*net.IPNet,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	RegisterTestingT(t)
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = gatewayMode
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"

	const (
		networkName = "bluenet"
		nadName     = "rednad"
		namespace   = "greenamespace"
	)
	nad := ovntest.GenerateNADWithConfig(nadName, namespace, fmt.Sprintf(`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "transport": %q,
        "outboundSNAT": %q
}
`,
		networkName,
		topology,
		cidrs,
		fmt.Sprintf("%s/%s", namespace, nadName),
		types.NetworkRolePrimary,
		types.NetworkTransportNoOverlay,
		outboundSNAT,
	))
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	controllerName := getNetworkControllerName(netInfo.GetNetworkName())
	asf := addressset.NewFakeAddressSetFactory(controllerName)
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker1",
			Annotations: map[string]string{
				util.OVNNodeHostCIDRs: `["192.168.126.11/24","fd00::11/64"]`,
			},
		},
	}
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{node}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}
	if err := watchFactory.Start(); err != nil {
		t.Fatalf("failed to start watch factory: %v", err)
	}
	t.Cleanup(watchFactory.Shutdown)

	nbClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
	if err != nil {
		t.Fatalf("failed to create libovsdb test harness: %v", err)
	}
	t.Cleanup(libovsdbCleanup.Cleanup)
	addressSetManager := addresssetmanager.NewAddressSetManager(
		watchFactory.PodCoreInformer(),
		watchFactory.NamespaceInformer(),
		watchFactory.NodeCoreInformer(),
		nbClient,
		networkmanager.Default().Interface().GetNetworkNameForNADKey,
	)
	return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				controllerName:      controllerName,
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				addressSetFactory:   asf,
				addressSetManager:   addressSetManager,
			},
		},
		asf,
		localPodSubnets
}

func seedAdvertisedSNATAddressSets(t *gotesting.T, asf addressset.AddressSetFactory) {
	t.Helper()
	nodeIPsASIDs := getClusterNodeIPsAddrSetDbIDsForTest()
	if _, err := asf.NewAddressSet(nodeIPsASIDs, []string{"192.168.126.11", "fd00::11"}); err != nil {
		t.Fatalf("failed to create node IP address set: %v", err)
	}

	svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
	if _, err := asf.NewAddressSet(svcIPsASIDs, []string{"10.96.0.10", "fd02::10"}); err != nil {
		t.Fatalf("failed to create UDN-enabled service address set: %v", err)
	}
}

func expectAdvertisedSNATUsesLiveAllowedExtIPs(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(4))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	nodeIPv4ASUUID, nodeIPv6ASUUID := nodeIPsAS.GetASUUID()
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPv4ASUUID, svcIPv6ASUUID := svcIPsAS.GetASUUID()

	actualAllowedExtIPsByLogicalIP := map[string][]string{}
	for _, snat := range snats {
		g.Expect(snat.Match).To(Equal(""))
		g.Expect(snat.AllowedExtIPs).NotTo(BeNil())
		g.Expect(snat.ExemptedExtIPs).To(BeNil())
		actualAllowedExtIPsByLogicalIP[snat.LogicalIP] = append(
			actualAllowedExtIPsByLogicalIP[snat.LogicalIP],
			*snat.AllowedExtIPs,
		)
	}
	g.Expect(actualAllowedExtIPsByLogicalIP["100.128.0.0/24"]).To(ConsistOf(nodeIPv4ASUUID, svcIPv4ASUUID))
	g.Expect(actualAllowedExtIPsByLogicalIP["ae70::/64"]).To(ConsistOf(nodeIPv6ASUUID, svcIPv6ASUUID))
}

func expectAdvertisedSNATUsesDestinationMatch(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(2))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())

	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())
	v4Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv4, nodeIPsAS, svcIPsAS)
	v6Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv6, nodeIPsAS, svcIPsAS)

	g.Expect(snats[0].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v4Match)))
	g.Expect(snats[0].AllowedExtIPs).To(BeNil())
	g.Expect(snats[0].ExemptedExtIPs).To(BeNil())

	g.Expect(snats[1].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v6Match)))
	g.Expect(snats[1].AllowedExtIPs).To(BeNil())
	g.Expect(snats[1].ExemptedExtIPs).To(BeNil())
}

var _ = Describe("dhcpPodNetworkUpdated", func() {
	newController := func(ipamType string) *BaseUserDefinedNetworkController {
		netconf := &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: "localnet-net",
				Type: "ovn-k8s-cni-overlay",
				IPAM: cnitypes.IPAM{Type: ipamType},
			},
			NADName:  "foo-ns/localnet-nad",
			Topology: types.LocalnetTopology,
		}
		netInfo, err := util.NewNetInfo(netconf)
		Expect(err).NotTo(HaveOccurred())
		return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				networkManager: &networkmanager.FakeNetworkManager{
					NADNetworks: map[string]util.NetInfo{"foo-ns/localnet-nad": netInfo},
				},
			},
		}
	}

	// podNetworks maps NAD keys to their pod-networks annotation entries;
	// a nil map stands for "no pod at all"
	type podNetworks map[string]*util.PodAnnotation

	podWithNetworks := func(networks podNetworks) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bar-pod",
				Namespace: "foo-ns",
			},
		}
		for nadKey, entry := range networks {
			var err error
			pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, entry, nadKey)
			Expect(err).NotTo(HaveOccurred())
		}
		return pod
	}

	const nadKey = "foo-ns/localnet-nad"
	var (
		// this network's entry as the CNI first writes it, before the DHCP
		// exchange, and after patching the learned lease into it
		dhcpMACOnly = &util.PodAnnotation{
			MAC:      ovntest.MustParseMAC("0a:58:fd:98:00:01"),
			Role:     types.NetworkRoleSecondary,
			IPAMMode: types.IPAMTypeDHCP,
		}
		dhcpLeased = &util.PodAnnotation{
			IPs:      ovntest.MustParseIPNets("10.1.192.102/24"),
			MAC:      ovntest.MustParseMAC("0a:58:fd:98:00:01"),
			Gateways: ovntest.MustParseIPs("10.1.192.1"),
			Role:     types.NetworkRoleSecondary,
			IPAMMode: types.IPAMTypeDHCP,
		}
		// the default network's entry every multi-homed pod carries alongside
		defaultNetEntry = &util.PodAnnotation{
			IPs:  ovntest.MustParseIPNets("10.244.1.5/24"),
			MAC:  ovntest.MustParseMAC("0a:58:0a:f4:01:05"),
			Role: types.NetworkRolePrimary,
		}
	)

	dhcpPodNetworkUpdated := func(ipamType string, oldNetworks, newNetworks podNetworks) bool {
		bsnc := newController(ipamType)
		var oldPod *corev1.Pod
		if oldNetworks != nil {
			oldPod = podWithNetworks(oldNetworks)
		}
		return bsnc.dhcpPodNetworkUpdated(oldPod, podWithNetworks(newNetworks))
	}

	DescribeTable("re-triggers port processing when this network's entry is added or updated",
		func(ipamType string, oldNetworks, newNetworks podNetworks) {
			Expect(dhcpPodNetworkUpdated(ipamType, oldNetworks, newNetworks)).To(BeTrue())
		},
		Entry("when the annotation gains the DHCP-learned IPs",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpMACOnly}, podNetworks{nadKey: dhcpLeased}),
		// the CNI clears the stale lease at the start of a repeat ADD so the
		// port security is relaxed to MAC-only for the new DHCP exchange
		Entry("when the CNI clears the previous lease on a repeat ADD",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, podNetworks{nadKey: dhcpMACOnly}),
		Entry("on the CNI lease patch of this network's entry in a multi-homed annotation",
			types.IPAMTypeDHCP,
			podNetworks{nadKey: dhcpMACOnly, "default": defaultNetEntry},
			podNetworks{nadKey: dhcpLeased, "default": defaultNetEntry}),
	)

	DescribeTable("does not re-trigger port processing",
		func(ipamType string, oldNetworks, newNetworks podNetworks) {
			Expect(dhcpPodNetworkUpdated(ipamType, oldNetworks, newNetworks)).To(BeFalse())
		},
		Entry("when the annotation is unchanged",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, podNetworks{nadKey: dhcpLeased}),
		Entry("on non-DHCP networks",
			"", podNetworks{nadKey: dhcpMACOnly}, podNetworks{nadKey: dhcpLeased}),
		Entry("without an old pod",
			types.IPAMTypeDHCP, nil, podNetworks{nadKey: dhcpLeased}),
		Entry("when only another network's entry changes",
			types.IPAMTypeDHCP,
			podNetworks{nadKey: dhcpMACOnly},
			podNetworks{nadKey: dhcpMACOnly, "default": defaultNetEntry}),
		// nothing legitimately removes a DHCP entry from a live pod, and
		// reprocessing the port without its annotation would only churn
		Entry("when this network's entry is removed",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, podNetworks{}),
	)
})

var _ = Describe("localnet DHCP IPAM pod lifecycle", func() {
	const (
		namespaceName = "foo-ns"
		nadName       = "localnet-nad"
		networkName   = "localnet-net"
		nodeName      = "node1"
		chassisID     = "chassis-node1"
		podName       = "bar-pod"
		nadKey        = namespaceName + "/" + nadName
		podMAC        = "0a:58:fd:98:00:01"
		podIP         = "10.1.192.102"

		macOnlyAnnotation = `{"foo-ns/localnet-nad":{"mac_address":"0a:58:fd:98:00:01","role":"secondary","ipam_mode":"dhcp"}}`
		leasedAnnotation  = `{"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.102/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
	})

	It("creates the LSP with the CNI-minted MAC and tightens it after the DHCP lease patch", func() {
		nad := ovntest.GenerateNADWithConfig(nadName, namespaceName, fmt.Sprintf(`
{
        "cniVersion": "1.0.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": "localnet",
        "netAttachDefName": %q,
        "ipam": {"type": "dhcp"}
}
`, networkName, nadKey))
		ovntest.AnnotateNADWithNetworkID("3", nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		switchName := netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch)

		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID",
			Name: switchName,
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespaceName,
				Name:      podName,
				UID:       podName,
				Annotations: map[string]string{
					nettypes.NetworkAttachmentAnnot: nadKey,
					types.OvnPodAnnotationName:      macOnlyAnnotation,
				},
			},
			Spec:   corev1.PodSpec{NodeName: nodeName},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": chassisID},
				},
			},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}},
			pod,
			&nettypes.NetworkAttachmentDefinitionList{Items: []nettypes.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)

		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers[networkName]
		Expect(ok).To(BeTrue())
		bnc := controller.bnc

		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, nil, nil, nil)).To(Succeed())

		Expect(bnc.WatchPods()).To(Succeed())

		portName := util.GetUserDefinedNetworkLogicalPortName(namespaceName, podName, nadKey)
		newExpectedLSP := func(addresses string) *nbdb.LogicalSwitchPort {
			return &nbdb.LogicalSwitchPort{
				UUID:         portName + "-UUID",
				Name:         portName,
				Addresses:    []string{addresses},
				PortSecurity: []string{addresses},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              namespaceName,
					types.NetworkExternalID:  networkName,
					types.NADExternalID:      nadKey,
					types.TopologyExternalID: types.LocalnetTopology,
				},
				Options: map[string]string{
					"requested-chassis": chassisID,
					"iface-id-ver":      podName,
				},
			}
		}
		expectedSwitch := &nbdb.LogicalSwitch{
			UUID:  switchName + "-UUID",
			Name:  switchName,
			Ports: []string{portName + "-UUID"},
		}

		By("the pod add creates the LSP with just the MAC Address")
		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedSwitch, newExpectedLSP(podMAC)))

		By("the CNI patches the DHCP-learned IP into the pod-networks annotation and the pod-update event tightens the LSP to MAC+IP")
		updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespaceName).
			Get(context.Background(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		updatedPod.Annotations[types.OvnPodAnnotationName] = leasedAnnotation
		// the fake clientset doesn't bump ResourceVersion; informers need it
		// to change to deliver the update event
		updatedPod.ResourceVersion = "42"
		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespaceName).
			Update(context.Background(), updatedPod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedSwitch, newExpectedLSP(podMAC+" "+podIP)))
	})
})

// nadKeyNameOverlay keeps GetPrimaryNADForNamespace (and thus namespace-informer
// NotFound) on the wrapped manager, while allowing tests to claim a NAD key.
type nadKeyNameOverlay struct {
	networkmanager.Interface
	nadToNetwork map[string]string
}

func (m *nadKeyNameOverlay) GetNetworkNameForNADKey(nadKey string) string {
	if name, ok := m.nadToNetwork[nadKey]; ok {
		return name
	}
	return m.Interface.GetNetworkNameForNADKey(nadKey)
}
