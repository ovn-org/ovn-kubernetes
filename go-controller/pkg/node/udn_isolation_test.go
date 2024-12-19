package node

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"net"
	"sigs.k8s.io/yaml"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("nftPodElementsSet", func() {
	const setName = "test-set"
	var nft *knftables.Fake

	for _, composed := range []bool{false, true} {
		Context(fmt.Sprintf("composed=%v", composed), func() {
			composed := composed
			setType := "ipv4_addr"
			if composed {
				setType = "ipv4_addr . inet_proto . inet_service"
			}

			BeforeEach(func() {
				nft = nodenft.SetFakeNFTablesHelper()
				tx := nft.NewTransaction()
				tx.Add(&knftables.Set{
					Name: setName,
					Type: setType,
				})
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
			})

			getElem := func(ip string) string {
				if !composed {
					return ip
				}
				return fmt.Sprintf("%s . tcp . 8080", ip)
			}

			getElemKey := func(ip string) []string {
				if !composed {
					return []string{ip}
				}
				return []string{ip, "tcp", "8080"}
			}

			getExpectedDump := func(ips ...string) string {
				result := fmt.Sprintf(`add table inet ovn-kubernetes
add set inet ovn-kubernetes %s { type %s ; }
`, setName, setType)
				for _, ip := range ips {
					result += fmt.Sprintf("add element inet ovn-kubernetes %s { %s }\n", setName, getElem(ip))
				}
				return result
			}

			It("fullSync should update sets and build local cache", func() {
				tx := nft.NewTransaction()
				tx.Add(&knftables.Element{
					Set: setName,
					Key: getElemKey("1.1.1.1"),
				})
				tx.Add(&knftables.Element{
					Set: setName,
					Key: getElemKey("1.1.1.2"),
				})
				Expect(nft.Run(context.Background(), tx)).To(Succeed())

				s := newNFTPodElementsSet(setName, composed)
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())

				Expect(s.podElements).To(Equal(newElems))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod1", "ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod add", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()

				s.updatePodElementsTX("ns1/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns2/pod1": sets.New(getElem("1.1.1.1")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1", "ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod update", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()
				//setup existing pod IPs
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())

				s.updatePodElementsTX("ns1/pod1", sets.New(getElem("1.1.1.1")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New(getElem("1.1.1.1")))

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1", "1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.3")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New(getElem("1.1.1.4")), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New(getElem("1.1.1.4")))
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.1", "1.1.1.2", "1.1.1.4"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod1": sets.New(getElem("1.1.1.1")),
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.4")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.1"): sets.New("ns1/pod1"),
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.4"): sets.New("ns2/pod1"),
				}))
			})

			It("updatePodElements should update sets and local cache on pod delete", func() {
				s := newNFTPodElementsSet(setName, false)
				tx := nft.NewTransaction()
				//setup existing pod IPs
				newElems := map[string]sets.Set[string]{}
				newElems["ns1/pod1"] = sets.New(getElem("1.1.1.2"))
				newElems["ns1/pod2"] = sets.New(getElem("1.1.1.2"))
				newElems["ns2/pod1"] = sets.New(getElem("1.1.1.3"))
				Expect(s.fullSync(nft, newElems)).To(Succeed())

				s.updatePodElementsTX("ns1/pod1", sets.New[string](), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns1/pod1", sets.New[string]())

				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2", "1.1.1.3"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
					"ns2/pod1": sets.New(getElem("1.1.1.3")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
					getElem("1.1.1.3"): sets.New("ns2/pod1"),
				}))

				s.updatePodElementsTX("ns2/pod1", sets.New[string](), tx)
				Expect(nft.Run(context.Background(), tx)).To(Succeed())
				s.updatePodElementsAfterTX("ns2/pod1", sets.New[string]())
				Expect(nodenft.MatchNFTRules(getExpectedDump("1.1.1.2"), nft.Dump())).To(Succeed())
				Expect(s.podElements).To(Equal(map[string]sets.Set[string]{
					"ns1/pod2": sets.New(getElem("1.1.1.2")),
				}))
				Expect(s.elementToPods).To(Equal(map[string]sets.Set[string]{
					getElem("1.1.1.2"): sets.New("ns1/pod2"),
				}))
			})
		})
	}
})

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
add set inet ovn-kubernetes udn-open-ports-icmp-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv6)" ; }
add set inet ovn-kubernetes udn-open-ports-v4 { type ipv4_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-v6 { type ipv6_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv6)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks (IPv6)" ; }
add rule inet ovn-kubernetes udn-isolation ip daddr . meta l4proto . th dport @udn-open-ports-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-open-ports-icmp-v4 meta l4proto icmp accept
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 kubelet.slice/kubelet.service ip daddr @udn-pod-default-ips-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation ip6 daddr . meta l4proto . th dport @udn-open-ports-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-open-ports-icmp-v6 meta l4proto icmpv6 accept
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

	getExpectedDumpWithOpenPorts := func(v4ips, v6ips []string, openPorts map[string][]*util.OpenPort) string {
		result := getExpectedDump(v4ips, v6ips)
		for ip, openPorts := range openPorts {
			netIP := net.ParseIP(ip)
			for _, openPort := range openPorts {
				if openPort.Protocol == "icmp" {
					if netIP.To4() != nil {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-icmp-v4 { %s }\n", ip)
					} else {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-icmp-v6 { %s }\n", ip)
					}
				} else {
					if netIP.To4() != nil {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-v4 { %s . %s . %d }\n", ip, openPort.Protocol, *openPort.Port)
					} else {
						result += fmt.Sprintf("add element inet ovn-kubernetes udn-open-ports-v6 { %s . %s . %d }\n", ip, openPort.Protocol, *openPort.Port)
					}
				}
			}
		}
		return result
	}

	start := func(objects ...runtime.Object) {
		fakeClient = util.GetOVNClientset(objects...).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())

		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer())

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// Copy manager.Start() sequence, but using fake nft and without running systemd tracker
		manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"
		nft = nodenft.SetFakeNFTablesHelper()
		manager.nft = nft
		err = manager.setupUDNIsolationFromHost()
		Expect(err).NotTo(HaveOccurred())
		err = controller.StartWithInitialSync(manager.podInitialSync, manager.podController)
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		config.PrepareTestConfig()
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode = true
		config.IPv6Mode = true

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

	It("correctly handles host-network and not ready pods on initial sync", func() {
		hostNetPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hostnet",
				UID:       ktypes.UID("hostnet"),
				Namespace: defaultNamespace,
			},
		}
		hostNetPod.Spec.HostNetwork = true
		notReadyPod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "notready",
				UID:       ktypes.UID("notready"),
				Namespace: defaultNamespace,
			},
		}

		fakeClient = util.GetOVNClientset(hostNetPod, notReadyPod).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer())
		nft = nodenft.SetFakeNFTablesHelper()
		manager.nft = nft

		Expect(wf.Start()).To(Succeed())
		Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
		Expect(manager.podInitialSync()).To(Succeed())
	})

	It("correctly generates initial rules", func() {
		start()
		Expect(nft.Dump()).To(Equal(getExpectedDump(nil, nil)))
	})

	Context("updates pod IPs", func() {
		It("on restart", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3"}))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
		})

		It("on pod add", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}))
			err := nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1"}, []string{"2014:100:200::1"}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Create(context.TODO(),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
			_, err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Create(context.TODO(),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3", "2014:100:200::3"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDump([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod delete", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.2"}))
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

	Context("updates open ports", func() {
		intRef := func(i int) *int {
			return &i
		}

		It("on restart", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2"}, util.OpenPort{Protocol: "icmp"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				"1.1.1.2":         {{Protocol: "icmp"}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
		})

		It("on pod add", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Create(context.TODO(),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}, util.OpenPort{Protocol: "icmp"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())
			_, err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Create(context.TODO(),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.3", "2014:100:200::3"}, util.OpenPort{Protocol: "icmp"}), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())
		})

		It("on pod delete", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}, util.OpenPort{Protocol: "tcp", Port: intRef(80)}),
				newPodWithIPs(nadNamespace, "pod2", true, []string{"1.1.1.2", "2014:100:200::2"}, util.OpenPort{Protocol: "icmp"}),
				newPodWithIPs(defaultNamespace, "pod3", false, []string{"1.1.1.2"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
				"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
				"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				"1.1.1.2":         {{Protocol: "icmp"}},
				"2014:100:200::2": {{Protocol: "icmp"}},
			}), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			err = fakeClient.KubeClient.CoreV1().Pods(defaultNamespace).Delete(context.TODO(), "pod3", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Consistently(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1", "1.1.1.2"}, []string{"2014:100:200::1", "2014:100:200::2"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
					"1.1.1.2":         {{Protocol: "icmp"}},
					"2014:100:200::2": {{Protocol: "icmp"}},
				}), nft.Dump())
			}).Should(Succeed())

			err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Delete(context.TODO(), "pod2", metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
				}), nft.Dump())
			}).Should(Succeed())
		})
	})
})

func getOpenPortAnnotation(openPorts []util.OpenPort) map[string]string {
	res, err := yaml.Marshal(openPorts)
	Expect(err).NotTo(HaveOccurred())
	anno := make(map[string]string)
	if len(res) > 0 {
		anno[util.UDNOpenPortsAnnotationName] = string(res)
	}
	return anno
}

// newPodWithIPs creates a new pod with the given IPs, only filled for default network.
func newPodWithIPs(namespace, name string, primaryUDN bool, ips []string, openPorts ...util.OpenPort) *v1.Pod {
	annoPodIPs := make([]string, len(ips))
	for i, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			annoPodIPs[i] = "\"" + ip + "/24\""
		} else {
			annoPodIPs[i] = "\"" + ip + "/64\""
		}
	}
	annotations := getOpenPortAnnotation(openPorts)
	role := types.NetworkRolePrimary
	if primaryUDN {
		role = types.NetworkRoleInfrastructure
	}
	annotations[util.OvnPodAnnotationName] = fmt.Sprintf(`{"default": {"role": "%s", "ip_addresses":[%s], "mac_address":"0a:58:0a:f4:02:03"}}`,
		role, strings.Join(annoPodIPs, ","))

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         ktypes.UID(name),
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}
