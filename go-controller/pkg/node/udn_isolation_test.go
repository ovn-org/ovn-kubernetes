package node

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("UDN Host isolation", func() {
	var (
		manager    *UDNHostIsolationManager
		wf         *factory.WatchFactory
		fakeClient *util.OVNNodeClientset
		nft        *knftables.Fake
	)

	const (
		nadNamespace     = "nad-namespace"
		defaultNamespace = "default-namespace"
	)

	getExpectedDump := func(v4ips, v6ips []string) string {
		result :=
			`add table inet ovn-kubernetes
add chain inet ovn-kubernetes udn-isolation { type filter hook output priority 0 ; comment "Host isolation for user defined networks" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks (IPv6)" ; }
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 kubelet.slice/kubelet.service ip daddr @udn-pod-default-ips-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 kubelet.slice/kubelet.service ip6 daddr @udn-pod-default-ips-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-pod-default-ips-v6 drop
`
		for _, ip := range v4ips {
			result += fmt.Sprintf("add element inet ovn-kubernetes udn-pod-default-ips-v4 { %s }\n", ip)
		}
		for _, ip := range v6ips {
			result += fmt.Sprintf("add element inet ovn-kubernetes udn-pod-default-ips-v6 { %s }\n", ip)
		}
		return result
	}

	start := func(objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), wf.NADInformer().Lister())

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Copy manager.Start() sequence, but using fake nft and without running systemd tracker
		manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"
		nft = nodenft.SetFakeNFTablesHelper()
		manager.nft = nft
		err = manager.setupUDNFromHostIsolation()
		Expect(err).NotTo(HaveOccurred())
		err = controller.StartWithInitialSync(manager.podInitialSync, manager.podController)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		wf = nil
		manager = nil
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
		}
		if manager != nil {
			manager.Stop()
		}
	})

	It("correctly generates initial rules", func() {
		start()
		Expect(nft.Dump()).To(Equal(getExpectedDump(nil, nil)))
	})

	Context("updates pod IPs", func() {
		It("on restart", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", []string{"1.1.1.2"}),
				newPodWithIPs(defaultNamespace, "pod3", []string{"1.1.1.3"}),
				primaryNetNAD(nadNamespace, "nad"))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
		})

		It("on pod add", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", []string{"1.1.1.1", "2014:100:200::1"}),
				primaryNetNAD(nadNamespace, "nad"))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Create(context.TODO(),
				newPodWithIPs(nadNamespace, "pod2", []string{"1.1.1.2", "2014:100:200::2"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
			_, err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Create(context.TODO(),
				newPodWithIPs(defaultNamespace, "pod3", []string{"1.1.1.3", "2014:100:200::3"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod delete", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", []string{"1.1.1.2", "2014:100:200::2"}),
				newPodWithIPs(defaultNamespace, "pod3", []string{"1.1.1.2"}),
				primaryNetNAD(nadNamespace, "nad"))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Delete(context.TODO(), "pod3", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())

			err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Delete(context.TODO(), "pod2", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1"}, []string{"2014:100:200::1"}), nft.Dump())
			}).Should(Succeed())
		})
	})

})

func newPodWithIPs(namespace, name string, ips []string) *v1.Pod {
	podIPs := make([]v1.PodIP, len(ips))
	for i, ip := range ips {
		podIPs[i] = v1.PodIP{IP: ip}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
		},
		Status: v1.PodStatus{
			Phase:  v1.PodRunning,
			PodIP:  ips[0],
			PodIPs: podIPs,
		},
	}
}

func primaryNetNAD(namespace, name string) *netv1.NetworkAttachmentDefinition {
	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{
			Config: fmt.Sprintf(`{
				"cniVersion": "1.0.0",
				"type": "ovn-k8s-cni-overlay",
				"name": "test-net",
				"netAttachDefName": "%s/%s",
				"role": "primary",
				"topology": "layer3",
				"subnets": "1.1.0.0/16,2014:100:200::0/60",
				"mtu": 1500
			}`, namespace, name),
		},
	}
}