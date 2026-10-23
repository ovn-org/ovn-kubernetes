// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package addresssetmanager

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const controllerName = "test-controller"

func getPolicyKeyWithKind(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v/%v", "NetworkPolicy", policy.Namespace, policy.Name)
}

func eventuallyExpectAddressSetsWithIP(nbClient libovsdbclient.Client, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip})
		gomega.Eventually(nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	}
}

func eventuallyExpectEmptyAddressSetsExist(nbClient libovsdbclient.Client, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{})
		gomega.Eventually(nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	}
}

// this is a test wrapper to count db transactions
type countingClient struct {
	libovsdbclient.Client
	transactCount atomic.Int64
}

func (c *countingClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	c.transactCount.Add(1)
	return c.Client.Transact(ctx, ops...)
}

func newNetInfo(networkName, topology, role, subnets string) util.NetInfo {
	netconf := ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: networkName,
			Type: "ovn-k8s-cni-overlay",
		},
		Role:     role,
		Topology: topology,
		Subnets:  subnets,
	}
	netInfo, err := util.NewNetInfo(&netconf)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return netInfo
}

var _ = ginkgo.Describe("OVN podSelectorAddressSet", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
		netPolicyName1 = "networkpolicy1"
		nodeName       = "node1"
		podLabelKey    = "podLabel"
		ip1            = "10.128.1.1"
		ip2            = "10.128.1.2"
		ip3            = "10.128.1.3"
		ip4            = "10.128.1.4"
		hostNetPodIP   = "10.0.1.1"
		node1Subnet    = "10.244.0.0/24"
		node1MgmtIP    = "10.244.0.2"
	)
	var (
		addressSetManager  *AddressSetManager
		wf                 *factory.WatchFactory
		clientSet          *util.OVNKubeControllerClientset
		initialDB          libovsdbtest.TestSetup
		libovsdbCleanup    *libovsdbtest.Context
		libovsdbNBClient   libovsdbclient.Client
		countingNBClient   *countingClient
		fakeNetworkManager *networkmanager.FakeNetworkManager
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}
		fakeNetworkManager = &networkmanager.FakeNetworkManager{
			NADNetworks: map[string]util.NetInfo{},
		}
	})

	ginkgo.AfterEach(func() {
		// stop controller
		if addressSetManager != nil {
			addressSetManager.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	startAddrSetManagerWithNodes := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, pods []corev1.Pod, nodes []corev1.Node) {
		clientSet = util.GetOVNClientset(
			&corev1.NamespaceList{Items: namespaces},
			&corev1.PodList{Items: pods},
			&corev1.NodeList{Items: nodes},
		).GetOVNKubeControllerClientset()
		var err error
		wf, err = factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(wf.Start()).To(gomega.Succeed())
		libovsdbNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// this is only used for "saves db transactions when IPs don't change" test
		countingNBClient = &countingClient{Client: libovsdbNBClient}
		addressSetManager = NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), wf.NodeCoreInformer(), countingNBClient,
			fakeNetworkManager.Interface().GetNetworkNameForNADKey)
		err = addressSetManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	startAddrSetManager := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, pods []corev1.Pod) {
		startAddrSetManagerWithNodes(dbSetup, namespaces, pods, nil)
	}

	ginkgo.It("validates selectors", func() {
		// start ovn without any objects
		startAddrSetManager(initialDB, nil, nil)
		namespace := *testing.NewNamespace(namespaceName1)
		networkPolicy := testing.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace.Name,
			"", "label1", true, true)
		// create peer with invalid Operator
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "key",
					Operator: "",
					Values:   []string{"value"},
				}},
			},
		}
		// try to add invalid peer
		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("is not a valid label selector operator"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))

		// add nil pod selector
		_, _, _, err = addressSetManager.EnsureAddressSet(
			nil, peer.NamespaceSelector, nil, networkPolicy.Namespace, getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("pod selector is nil"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))

		// namespace selector is nil and namespace is empty
		_, _, _, err = addressSetManager.EnsureAddressSet(
			peer.PodSelector, nil, nil, "", getPolicyKeyWithKind(networkPolicy), controllerName, &util.DefaultNetInfo{}, false)
		// error should happen on handler add
		gomega.Expect(err.Error()).To(gomega.ContainSubstring("namespace selector is nil and namespace is empty"))
		// address set will not be created
		gomega.Consistently(addressSetManager.nbClient, 100*time.Millisecond, 20*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{}))
	})

	ginkgo.It("populates address set synchronously on first ensure", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}
		pod := testing.NewPod(namespace1.Name, "pod1", nodeName, podIP)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{podIP})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})

	ginkgo.It("reconciles the registered cluster node IP address set on node events", func() {
		node1 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node1",
				Annotations: map[string]string{util.OVNNodeHostCIDRs: `["10.0.0.1/24"]`},
			},
		}
		node2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node2",
				Annotations: map[string]string{util.OVNNodeHostCIDRs: `["10.0.0.2/24"]`},
			},
		}
		startAddrSetManagerWithNodes(initialDB, nil, nil, []corev1.Node{node1, node2})

		addressSetFactory := addressset.NewOvnAddressSetFactory(libovsdbNBClient, config.IPv4Mode, config.IPv6Mode)
		expectNodeIPs := func(expected ...string) {
			gomega.Eventually(func() []string {
				as, err := addressSetFactory.GetAddressSet(getClusterNodeIPsAddrSetDbIDs())
				if err != nil {
					return nil
				}
				v4Addresses, v6Addresses := as.GetAddresses()
				return append(v4Addresses, v6Addresses...)
			}).Should(gomega.ConsistOf(expected))
		}

		_, err := addressSetManager.EnsureClusterNodeIPsAddressSet(ClusterNodeIPsRouteAdvertisementsBackRef)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.1", "10.0.0.2")

		node2Copy := node2.DeepCopy()
		node2Copy.Annotations[util.OVNNodeHostCIDRs] = `["10.0.0.2/24","10.0.0.20/24"]`
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2Copy, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.1", "10.0.0.2", "10.0.0.20")

		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node1.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectNodeIPs("10.0.0.2", "10.0.0.20")
	})

	ginkgo.It("creates one address set for multiple users with the same selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "label1",
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref1", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref2", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name, controllerName, false)
		peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
		// expect peer address set only
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
	})
	ginkgo.It("creates different address set for multiple users with the same selector depending on legacyNetpolMode", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": "label1",
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref1", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref2", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nil, namespace1.Name,
			"backref3", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name,
			controllerName, false)
		peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
		peerASLegacyIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name,
			controllerName, true)
		peerASLegacy, _ := addressset.GetTestDbAddrSets(peerASLegacyIDs, []string{})
		// expect 2 peer address sets only
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS, peerASLegacy}))
	})
	ginkgo.It("creates the address set and tracks pod IPs for a subnet-less DHCP localnet network", func() {
		// a DHCP-IPAM localnet network has no subnets, but its pods carry
		// IPv4 leases reported via the pod-networks annotation so the network
		// must report IPv4 mode or EnsureAddressSet fails with "neither IPv4
		// nor IPv6 mode is enabled" and selector peers can never resolve
		dhcpNetInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: "dhcpnet",
				Type: "ovn-k8s-cni-overlay",
				IPAM: cnitypes.IPAM{Type: "dhcp"},
			},
			Role:     types.NetworkRoleSecondary,
			Topology: types.LocalnetTopology,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		namespace1 := *testing.NewNamespace(namespaceName1)
		pod := testing.NewPodWithSecondaryNADIP(namespace1.Name, "ns1pod1", nodeName, "10.244.0.2",
			dhcpNetInfo.GetNetworkName(), "172.18.0.225")
		pod.Labels = map[string]string{podLabelKey: "ns1pod1"}
		nadKey := fmt.Sprintf("%s/%s", namespace1.Name, dhcpNetInfo.GetNetworkName())
		fakeNetworkManager.NADNetworks[nadKey] = dhcpNetInfo
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod})

		podSelector := &metav1.LabelSelector{}
		_, _, _, err = addressSetManager.EnsureAddressSet(
			podSelector, nil, nil, namespace1.Name, "backRef", controllerName, dhcpNetInfo, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nil, namespace1.Name, controllerName, false)
		peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{"172.18.0.225"})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
	})

	// this is run during ginkgo tree construction, so env is not set yet
	config.IPv4Mode = true
	config.IPv6Mode = false
	primaryNetInfo := newNetInfo("primarynet", types.Layer3Topology, types.NetworkRolePrimary, "10.128.1.0/24/27")
	secondaryNetInfo := newNetInfo("secondarynet", types.Layer2Topology, types.NetworkRoleSecondary, "10.128.1.0/24")

	newPod := func(namespace, name, node, podIP string, netInfo util.NetInfo) *corev1.Pod {
		nadKey := fmt.Sprintf("%s/%s", namespace, netInfo.GetNetworkName())
		fakeNetworkManager.NADNetworks[nadKey] = netInfo
		if netInfo.GetNetworkName() == types.DefaultNetworkName {
			return testing.NewPod(namespace, name, node, podIP)
		} else {
			return testing.NewPodWithSecondaryNADIP(namespace, name, node, "10.244.0.2", netInfo.GetNetworkName(), podIP)
		}
	}

	for _, netInfo := range []util.NetInfo{&util.DefaultNetInfo{}, primaryNetInfo, secondaryNetInfo} {
		netInfo := netInfo
		ginkgo.DescribeTable(fmt.Sprintf("[%s] adds selected pod ips to the address set and removes on delete", netInfo.GetNetworkName()),
			func(peer knet.NetworkPolicyPeer, staticNamespace string, addrSetIPsList []string, legacyMode bool, netInfo util.NetInfo) {
				addrSetIPs := sets.New(addrSetIPsList...)
				namespace1 := *testing.NewNamespace(namespaceName1)
				namespace2 := *testing.NewNamespace(namespaceName2)
				config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
				hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
				ns1pod1 := newPod(namespace1.Name, "ns1pod1", nodeName, ip1, netInfo)
				ns1pod2 := newPod(namespace1.Name, "ns1pod2", nodeName, ip2, netInfo)
				ns1pod3 := newPod(namespace1.Name, "ns1pod3", nodeName, hostNetPodIP, netInfo)
				ns1pod3.Spec.HostNetwork = true
				ns2pod1 := newPod(namespace2.Name, "ns2pod1", nodeName, ip3, netInfo)
				ns2pod2 := newPod(namespace2.Name, "ns2pod2", nodeName, ip4, netInfo)
				podsList := []corev1.Pod{}
				for _, pod := range []*corev1.Pod{ns1pod1, ns1pod2, ns1pod3, ns2pod1, ns2pod2} {
					pod.Labels = map[string]string{podLabelKey: pod.Name}
					podsList = append(podsList, *pod)
				}
				testNode := corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
						Annotations: map[string]string{
							"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
						},
					},
				}
				startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{namespace1, namespace2, hostNetNamespace}, podsList,
					[]corev1.Node{testNode})

				_, _, _, err := addressSetManager.EnsureAddressSet(
					peer.PodSelector, peer.NamespaceSelector, nil, staticNamespace, "backRef", controllerName, netInfo, legacyMode)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// address set should be created and pod ips added
				peerASIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, staticNamespace, controllerName, legacyMode)
				// non-default networks don't match hostNetwork namespace remove node1MgmtIP if present
				if !netInfo.IsDefault() {
					addrSetIPs.Delete(node1MgmtIP)
					// secondary networks don't match hostNetwork pods, remove hostNetPodIP if present
					if !netInfo.IsPrimaryNetwork() {
						addrSetIPs.Delete(hostNetPodIP)
					}
				}
				peerAS, _ := addressset.GetTestDbAddrSets(peerASIDs, addrSetIPs.UnsortedList())
				gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
				// now delete ns1pod1 and ns1pod3 to test both overlay and hostnet pod IP cleanup
				err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), ns1pod1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), ns1pod3.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// deleted pod IPs should be removed from address set
				addrSetIPs.Delete(ip1, hostNetPodIP)
				peerAS, _ = addressset.GetTestDbAddrSets(peerASIDs, addrSetIPs.UnsortedList())
				gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{peerAS}))
			},
			ginkgo.Entry("all pods from a static namespace", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1, ip2, hostNetPodIP}, false, netInfo),
			ginkgo.Entry("selected pods from a static namespace", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
				},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1}, false, netInfo),
			ginkgo.Entry("all pods from all namespaces", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip2, hostNetPodIP, ip3, ip4}, false, netInfo),
			ginkgo.Entry("selected pods from all namespaces", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      podLabelKey,
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"ns1pod1", "ns2pod1"},
						},
					},
				},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip3}, false, netInfo),
			ginkgo.Entry("all pods from selected namespaces", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3, ip4}, false, netInfo),
			ginkgo.Entry("selected pods from selected namespaces", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3}, false, netInfo),
			ginkgo.Entry("all pods from a static namespace, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1, ip2}, true, netInfo),
			ginkgo.Entry("selected pods from a static namespace, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns1pod1"},
				},
				NamespaceSelector: nil,
			}, namespaceName1, []string{ip1}, true, netInfo),
			ginkgo.Entry("all pods from all namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector:       &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip2, ip3, ip4, node1MgmtIP}, true, netInfo),
			ginkgo.Entry("selected pods from all namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      podLabelKey,
							Operator: metav1.LabelSelectorOpIn,
							Values:   []string{"ns1pod1", "ns2pod1"},
						},
					},
				},
				NamespaceSelector: &metav1.LabelSelector{},
			}, namespaceName1, []string{ip1, ip3}, true, netInfo),
			ginkgo.Entry("all pods from selected namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3, ip4}, true, netInfo),
			ginkgo.Entry("selected pods from selected namespaces, legacyNetpolMode", knet.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{podLabelKey: "ns2pod1"},
				},
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"name": namespaceName2,
					},
				},
			}, namespaceName1, []string{ip3}, true, netInfo),
		)
	}
	ginkgo.It("on initial sync deletes unreferenced and updates referenced address sets", func() {
		unusedPodSelIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", controllerName, false)
		unusedPodSelAS, _ := addressset.GetTestDbAddrSets(unusedPodSelIDs, []string{"1.1.1.2"})
		refNetpolIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName2", controllerName, false)
		refNetpolAS, _ := addressset.GetTestDbAddrSets(refNetpolIDs, []string{"1.1.1.3"})
		netpolACL := libovsdbops.BuildACL(
			"netpolACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refNetpolAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		netpolACL.UUID = "netpolACL-UUID"
		refPodSelIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName3", controllerName, false)
		refPodSelAS, _ := addressset.GetTestDbAddrSets(refPodSelIDs, []string{"1.1.1.4"})
		podSelACL := libovsdbops.BuildACL(
			"podSelACL",
			nbdb.ACLDirectionFromLport,
			types.EgressFirewallStartPriority,
			fmt.Sprintf("ip4.src == {$%s} && outport == @a13757631697825269621", refPodSelAS.Name),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			"",
			false,
			nil,
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		podSelACL.UUID = "podSelACL-UUID"

		initialDb := []libovsdbtest.TestData{
			unusedPodSelAS,
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		dbSetup := libovsdbtest.TestSetup{NBData: initialDb}
		startAddrSetManager(dbSetup, nil, nil)

		finalDB := []libovsdbtest.TestData{
			refNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(finalDB))
		// now request address sets for the referenced netpol and pod selector, addresses should be updated to empty
		// because no pods exist in that namespace
		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, "nsName2", "backref", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		updatedNetpolAS, _ := addressset.GetTestDbAddrSets(refNetpolIDs, []string{})
		updatedDB := []libovsdbtest.TestData{
			updatedNetpolAS,
			netpolACL,
			refPodSelAS,
			podSelACL,
			&nbdb.LogicalSwitch{
				UUID: "node",
				ACLs: []string{podSelACL.UUID, netpolACL.UUID},
			},
		}
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData(updatedDB))

	})
	ginkgo.It("reconciles a completed and deleted pod whose IP has been assigned to a running pod", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed)
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, an update event will be generated
		completedPod.Status.Phase = corev1.PodSucceeded
		_, err = clientSet.KubeClient.CoreV1().Pods(completedPod.Namespace).Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// make sure the delete event is handled and address set is not changed
		time.Sleep(100 * time.Millisecond)
		// Running pod policy should not be affected by pod deletions
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)
	})
	ginkgo.It("reconciles a completed pod whose IP has been assigned to a running pod with non-matching namespace selector", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		namespace2 := *testing.NewNamespace(namespaceName2)
		nodeName := "node1"
		podIP := "10.128.1.3"
		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": namespaceName1,
				},
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1, namespace2}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Start a pod
		completedPod, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace1.Name, "completed-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// pod should be added to the address set
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, namespace1.Name, podIP)

		// Spawn a pod with an IP address that collides with a completed pod (we don't watch pods in this test,
		// therefore the same ip is allowed). This pod has another namespace that is not matched by the address set
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace2.Name).
			Create(
				context.TODO(),
				testing.NewPod(namespace2.Name, "running-pod", nodeName, podIP),
				metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Mark the pod as Completed, so delete event will be generated
		completedPod.Status.Phase = corev1.PodSucceeded
		_, err = clientSet.KubeClient.CoreV1().Pods(completedPod.Namespace).Update(context.TODO(), completedPod, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// IP should be deleted from the address set on delete event, since the new pod with the same ip
		// should not be present in given address set
		eventuallyExpectEmptyAddressSetsExist(addressSetManager.nbClient, peer, namespace1.Name)
	})
	ginkgo.It("saves db transactions when IPs don't change", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		ns1pod1 := testing.NewPod(namespace1.Name, "ns1pod1", nodeName, ip1)

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*ns1pod1})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			&metav1.LabelSelector{}, nil, nil, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		dbIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		// expect 2 transactions: 1 to create addr set and 1 to set IPs
		gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(int64(2)))

		// Now update pod labels, which will cause the address set reconciliation
		ns1pod1.Labels["newLabel"] = "newValue"
		_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), ns1pod1, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Consistently(addressSetManager.nbClient, 200*time.Millisecond, 50*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		// no new transactions should happen since IPs don't change
		gomega.Expect(countingNBClient.transactCount.Load()).To(gomega.Equal(int64(2)))
	})

	ginkgo.It("CleanupForController removes controller entries", func() {
		namespace1 := *testing.NewNamespace(namespaceName1)
		pod1 := testing.NewPod(namespace1.Name, "pod1", nodeName, ip1)
		startAddrSetManager(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod1})

		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
		}
		backRef := "NetworkPolicy/namespace1/testpolicy"

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, backRef, controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dbIDs := GetPodSelectorAddrSetDbIDs(peer.PodSelector, peer.NamespaceSelector, nil, namespace1.Name, controllerName, false)
		expectedAS, _ := addressset.GetTestDbAddrSets(dbIDs, []string{ip1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		gomega.Expect(addressSetManager.CleanupForController(controllerName)).To(gomega.Succeed())
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveEmptyData())
	})

	ginkgo.When("node selector is set", func() {
		const (
			nodeLabelKey         = "nodeType"
			nodeTypeWorker       = "worker"
			nodeTypeControlPlane = "control-plane"

			podLabelKey = "app"
			podAppVideo = "video"
		)
		node1 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node1",
				Labels: map[string]string{nodeLabelKey: nodeTypeWorker},
			},
		}
		node2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node2",
				Labels: map[string]string{nodeLabelKey: nodeTypeControlPlane},
			},
		}
		namespace1 := *testing.NewNamespace(namespaceName1)
		pod1 := testing.NewPod(namespace1.Name, "pod1", "node1", ip1)
		pod2 := testing.NewPod(namespace1.Name, "pod2", "node2", ip2)
		podSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{podLabelKey: podAppVideo},
		}
		nodeSelector := &metav1.LabelSelector{
			MatchLabels: map[string]string{nodeLabelKey: nodeTypeWorker},
		}
		peerASIDs := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelector, namespace1.Name, controllerName, false)

		ginkgo.BeforeEach(func() {
			startAddrSetManagerWithNodes(initialDB, []corev1.Namespace{namespace1}, []corev1.Pod{*pod1}, []corev1.Node{node1})
		})

		ginkgo.It("same pod and namespace selector with different nodeSelector create different address sets", func() {
			// add node2
			_, err := clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &node2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// add pod2
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Create(context.TODO(), pod2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			podSelector := &metav1.LabelSelector{}
			nodeSelWorker := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeTypeWorker}}
			nodeSelControlPlane := &metav1.LabelSelector{MatchLabels: map[string]string{nodeLabelKey: nodeTypeControlPlane}}
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelWorker, namespace1.Name, "backRef1", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelControlPlane, namespace1.Name, "backRef2", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			dbIDsWorker := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelWorker, namespace1.Name, controllerName, false)
			dbIDsControlPlane := GetPodSelectorAddrSetDbIDs(podSelector, nil, nodeSelControlPlane, namespace1.Name, controllerName, false)
			// expect 2 address sets with IPs populated
			workerAS, _ := addressset.GetTestDbAddrSets(dbIDsWorker, []string{ip1})
			controlPlaneAS, _ := addressset.GetTestDbAddrSets(dbIDsControlPlane, []string{ip2})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{workerAS, controlPlaneAS}))
		})

		ginkgo.It("doesn't add pod IP to address set if its labels don't match pod label selector", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Consistently(addressSetManager.nbClient, 500*time.Millisecond).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})

		ginkgo.It("adds pod IP to address set if its labels match pod label selector", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		})

		ginkgo.It("adds pod IP to address set if its node labels match node label selector", func() {
			specialNodeSelector := &metav1.LabelSelector{
				MatchLabels: map[string]string{
					nodeLabelKey: "special-node",
				},
			}
			podSelector := &metav1.LabelSelector{}
			asID := GetPodSelectorAddrSetDbIDs(podSelector, nil, specialNodeSelector, namespace1.Name, controllerName, false)
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, specialNodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// expect the address set to be empty initially since nodeType "special-node" doesn't match "worker"
			emptyAS, _ := addressset.GetTestDbAddrSets(asID, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
			// label the node as "special-node"
			node1Copy := node1.DeepCopy()
			node1Copy.Labels = map[string]string{nodeLabelKey: "special-node"}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// expect the address set to be populated
			expectedAS, _ := addressset.GetTestDbAddrSets(asID, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		})

		ginkgo.It("deletes pod IP from address set when pod is deleted", func() {
			// label the pod to match the pod selector
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err := clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, _, _, err = addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
			// delete the pod
			err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Delete(context.TODO(), pod1.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})

		ginkgo.It("deletes pod IP from address set when node label changes", func() {
			_, _, _, err := addressSetManager.EnsureAddressSet(podSelector, nil, nodeSelector, namespace1.Name, "backRef", controllerName, &util.DefaultNetInfo{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// add the pod to the address set
			pod1Copy := pod1.DeepCopy()
			pod1Copy.Labels = map[string]string{podLabelKey: podAppVideo}
			_, err = clientSet.KubeClient.CoreV1().Pods(namespace1.Name).Update(context.TODO(), pod1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{ip1})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
			// change the node label to not match the node selector
			node1Copy := node1.DeepCopy()
			node1Copy.Labels = map[string]string{nodeLabelKey: "control-plane"}
			_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1Copy, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			emptyAS, _ := addressset.GetTestDbAddrSets(peerASIDs, []string{})
			gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		})
	})

	ginkgo.It("reconciles HostNetworkNamespace IPs", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
		// this is calculated based on node-id
		gwIP1 := "100.64.0.1"

		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// imitate node update, even though that should never happen
		gwIP2 := "100.64.0.2"
		testNode.Annotations["k8s.ovn.org/node-id"] = "2"
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
		node2MgmtIP := "10.244.1.2"
		testNode2 := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node2",
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", "10.244.1.0/24"),
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode2, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2, node2MgmtIP})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// delete second node and check if IPs are removed from address set
		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode2.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP2})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Imitate restart replacing node1 with node2 to make sure HostNetworkNamespace IPs are fully updated on initialSync
		ginkgo.By("restarting addressSetManager with different node in the cluster")
		addressSetManager.Stop()
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode2, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		err = clientSet.KubeClient.CoreV1().Nodes().Delete(context.TODO(), testNode.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		addressSetManager = NewAddressSetManager(wf.PodCoreInformer(), wf.NamespaceInformer(), wf.NodeCoreInformer(), libovsdbNBClient,
			func(_ string) string { return "" })
		err = addressSetManager.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		// run EnsureAddressSet again, otherwise address set won't be reconciled with no users
		_, _, _, err = addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		restartedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node2MgmtIP})
		gomega.Expect(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{restartedAS}))
	})
	ginkgo.It("updates IPs for existing nodes when the host network traffic namespace is created", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		// this is calculated based on node-id
		gwIP1 := "100.64.0.1"
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		startAddrSetManagerWithNodes(initialDB, nil, nil, []corev1.Node{testNode})
		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		hostNetNamespace := testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		_, err = clientSet.KubeClient.CoreV1().Namespaces().Create(context.TODO(), hostNetNamespace, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP1})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

	})
	ginkgo.It("includes node primary IP in HostNetworkNamespace address_set when NoOverlay mode is enabled", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		// Enable NoOverlay transport mode
		config.Default.Transport = "no-overlay"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		// Start with empty NodeList, then add node after namespace is created
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		// Wait for empty address set to be created
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		// Create the node with only 1 annotation, it shouldn't fail and will parse what we have
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					util.OvnNodeIfAddr: "{\"ipv4\":\"192.168.1.10/24\"}",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Build expected IPs following the same pattern as the remote zone test
		// When NoOverlay is enabled, primary interface IPv4 should also be included
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{"192.168.1.10"})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Now update primary IP. Even though it should never happen, make sure we handle it correctly
		testNode.Annotations[util.OvnNodeIfAddr] = "{\"ipv4\":\"192.168.1.11/24\"}"
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{"192.168.1.11"})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))
	})
	ginkgo.It("correctly updates HostNetworkNamespace for hybrid overlay nodes", func() {
		config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
		hostNetNamespace := *testing.NewNamespace(config.Kubernetes.HostNetworkNamespace)
		// set hybrid overlay selector
		config.HybridOverlay.Enabled = true
		config.Kubernetes.NoHostSubnetNodes, _ = metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}})
		// Start with empty NodeList, then add node after namespace is created
		startAddrSetManager(initialDB, []corev1.Namespace{hostNetNamespace}, nil)

		_, _, _, err := addressSetManager.EnsureAddressSet(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", "backRef", controllerName, &util.DefaultNetInfo{}, true)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		hostNSASIDs := GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, &metav1.LabelSelector{}, nil,
			"", controllerName, true)
		// Wait for empty address set to be created
		emptyAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))

		// this is calculated based on node-id
		gwIP := "100.64.0.1"
		// Create node that doesn't match hybrid overlay selector, it should be added to HostNetworkNamespace address set
		testNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet),
					"k8s.ovn.org/node-id":      "1",
				},
			},
		}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Create(context.TODO(), &testNode, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		expectedAS, _ := addressset.GetTestDbAddrSets(hostNSASIDs, []string{node1MgmtIP, gwIP})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{expectedAS}))

		// Now label node as hybrid overlay, should be removed
		testNode.Labels = map[string]string{"a": "b"}
		_, err = clientSet.KubeClient.CoreV1().Nodes().Update(context.TODO(), &testNode, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		emptyAS, _ = addressset.GetTestDbAddrSets(hostNSASIDs, []string{})
		gomega.Eventually(addressSetManager.nbClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{emptyAS}))
	})
	ginkgo.It("reconciles address sets with exact-name namespace selectors on namespace events", func() {
		namespace1 := *testing.NewNamespaceWithLabels(namespaceName1, map[string]string{
			corev1.LabelMetadataName: namespaceName1,
			"tier":                   "backend",
		})
		namespace2 := *testing.NewNamespaceWithLabels(namespaceName2, map[string]string{
			corev1.LabelMetadataName: namespaceName2,
			"tier":                   "backend",
		})
		nodeName := "node1"
		pod1IP := "10.128.1.3"
		pod2IP := "10.128.1.4"
		pod1 := testing.NewPod(namespace1.Name, "pod1", nodeName, pod1IP)
		pod2 := testing.NewPod(namespace2.Name, "pod2", nodeName, pod2IP)

		peer := knet.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{},
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					corev1.LabelMetadataName: namespaceName1,
				},
			},
		}

		startAddrSetManager(initialDB, []corev1.Namespace{namespace1, namespace2}, []corev1.Pod{*pod1, *pod2})

		_, _, _, err := addressSetManager.EnsureAddressSet(
			peer.PodSelector, peer.NamespaceSelector, nil, "", "backRef", controllerName, &util.DefaultNetInfo{}, false)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// pod1 should be in the address set, pod2 should not
		eventuallyExpectAddressSetsWithIP(addressSetManager.nbClient, peer, "", pod1IP)

		// Delete namespace1; address set should be reconciled to empty
		err = clientSet.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespace1.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		eventuallyExpectEmptyAddressSetsExist(addressSetManager.nbClient, peer, "")
	})
})

var _ = ginkgo.Describe("selectsNamespace and getSelectedNamespaces", func() {
	var (
		indexer cache.Indexer
		m       *AddressSetManager
	)

	ginkgo.BeforeEach(func() {
		indexer = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		m = &AddressSetManager{namespaceLister: listers.NewNamespaceLister(indexer)}

		ns1 := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ns1",
				Labels: map[string]string{
					corev1.LabelMetadataName: "ns1",
					"env":                    "prod",
				},
			},
		}
		ns2 := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ns2",
				Labels: map[string]string{
					corev1.LabelMetadataName: "ns2",
					"env":                    "dev",
				},
			},
		}
		gomega.Expect(indexer.Add(ns1)).To(gomega.Succeed())
		gomega.Expect(indexer.Add(ns2)).To(gomega.Succeed())
	})

	ginkgo.Describe("selectsNamespace", func() {
		ginkgo.It("handles static namespace", func() {
			ps := &podSelectorAddressSet{namespace: "ns1"}
			matches, err := m.selectsNamespace(ps, "ns1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeTrue())

			matches, err = m.selectsNamespace(ps, "ns2")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())
		})

		ginkgo.It("handles empty namespace selector (matches any namespace)", func() {
			ps := &podSelectorAddressSet{namespaceSelector: labels.Everything()}
			matches, err := m.selectsNamespace(ps, "ns1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeTrue())
		})

		ginkgo.It("handles pinned kubernetes.io/metadata.name selector", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: "ns1"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			// Matching namespace
			matches, err := m.selectsNamespace(ps, "ns1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeTrue())

			// Non-matching namespace (triggers early exit without lister lookup)
			matches, err = m.selectsNamespace(ps, "ns2")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())

			// Non-existent namespace
			matches, err = m.selectsNamespace(ps, "non-existent")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())
		})

		ginkgo.It("handles compound selector with pinned name and secondary labels", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					corev1.LabelMetadataName: "ns1",
					"env":                    "dev", // ns1 has env: prod, so should not match
				},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			matches, err := m.selectsNamespace(ps, "ns1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())
		})

		ginkgo.It("handles arbitrary label selector", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			matches, err := m.selectsNamespace(ps, "ns1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeTrue())

			matches, err = m.selectsNamespace(ps, "ns2")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())

			// Deleted/missing namespace
			matches, err = m.selectsNamespace(ps, "deleted-ns")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(matches).To(gomega.BeFalse())
		})
	})

	ginkgo.Describe("getSelectedNamespaces", func() {
		ginkgo.It("handles static namespace", func() {
			ps := &podSelectorAddressSet{namespace: "ns1"}
			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.all).To(gomega.BeFalse())
			gomega.Expect(selected.set.Has("ns1")).To(gomega.BeTrue())
		})

		ginkgo.It("handles empty namespace selector", func() {
			ps := &podSelectorAddressSet{namespaceSelector: labels.Everything()}
			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.all).To(gomega.BeTrue())
		})

		ginkgo.It("handles pinned kubernetes.io/metadata.name selector", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: "ns1"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.set.Len()).To(gomega.Equal(1))
			gomega.Expect(selected.set.Has("ns1")).To(gomega.BeTrue())
		})

		ginkgo.It("handles pinned kubernetes.io/metadata.name when namespace does not exist", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: "missing-ns"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.set.Len()).To(gomega.Equal(0))
		})

		ginkgo.It("handles pinned kubernetes.io/metadata.name with non-matching additional label", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{
					corev1.LabelMetadataName: "ns1",
					"env":                    "dev",
				},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.set.Len()).To(gomega.Equal(0))
		})

		ginkgo.It("falls back to listing for general label selectors", func() {
			sel, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ps := &podSelectorAddressSet{namespaceSelector: sel}

			selected, err := m.getSelectedNamespaces(ps)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(selected.set.Len()).To(gomega.Equal(1))
			gomega.Expect(selected.set.Has("ns1")).To(gomega.BeTrue())
		})
	})
})

var _ = ginkgo.Describe("shortLabelSelectorString function", func() {
	ginkgo.It("handles LabelSelectorRequirement.Values order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v1", "v2", "v3"},
			}},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "key",
				Operator: "",
				Values:   []string{"v3", "v2", "v1"},
			}},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchExpressions order", func() {
		ls1 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		ls2 := &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "key2",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key1",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
				{
					Key:      "key3",
					Operator: "",
					Values:   []string{"v1", "v2", "v3"},
				},
			},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
	ginkgo.It("handles MatchLabels order", func() {
		ls1 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k1": "v1", "k2": "v2", "k3": "v3"},
		}
		ls2 := &metav1.LabelSelector{
			MatchLabels: map[string]string{"k2": "v2", "k1": "v1", "k3": "v3"},
		}
		gomega.Expect(shortLabelSelectorString(ls1)).To(gomega.Equal(shortLabelSelectorString(ls2)))
	})
})
