// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/knftables"
	"sigs.k8s.io/yaml"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 "kubelet.slice/kubelet.service" ip daddr @udn-pod-default-ips-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation ip6 daddr . meta l4proto . th dport @udn-open-ports-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-open-ports-icmp-v6 meta l4proto icmpv6 accept
add rule inet ovn-kubernetes udn-isolation socket cgroupv2 level 2 "kubelet.slice/kubelet.service" ip6 daddr @udn-pod-default-ips-v6 accept
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

		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)

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
		Expect(config.PrepareTestConfig()).To(Succeed())
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
		hostNetPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "hostnet",
				UID:       ktypes.UID("hostnet"),
				Namespace: defaultNamespace,
			},
		}
		hostNetPod.Spec.HostNetwork = true
		notReadyPod := &corev1.Pod{
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
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)
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

	It("correctly handles not ready pods", func() {
		notReadyPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "notready",
				UID:       ktypes.UID("notready"),
				Namespace: defaultNamespace,
			},
		}
		fakeClient = util.GetOVNClientset(notReadyPod).GetNodeClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(fakeClient, "node1")
		Expect(err).NotTo(HaveOccurred())
		manager = NewUDNHostIsolationManager(true, true, wf.PodCoreInformer(), "node1", nil)
		Expect(wf.Start()).To(Succeed())
		Expect(manager.reconcilePod(context.Background(), notReadyPod.Namespace+"/"+notReadyPod.Name)).To(Succeed())
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

		It("on pod update", func() {
			start(
				newPodWithIPs(nadNamespace, "pod1", true, []string{"1.1.1.1", "2014:100:200::1"}))
			err := nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, nil), nft.Dump())
			Expect(err).NotTo(HaveOccurred())
			pod, err := fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Get(context.TODO(),
				"pod1", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			pod.Annotations[util.UDNOpenPortsAnnotationName] = getOpenPortAnnotation([]util.OpenPort{{Protocol: "tcp", Port: intRef(80)}})[util.UDNOpenPortsAnnotationName]
			_, err = fakeClient.KubeClient.CoreV1().Pods(nadNamespace).Update(context.TODO(),
				pod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				return nodenft.MatchNFTRules(getExpectedDumpWithOpenPorts([]string{"1.1.1.1"}, []string{"2014:100:200::1"}, map[string][]*util.OpenPort{
					"1.1.1.1":         {{Protocol: "tcp", Port: intRef(80)}},
					"2014:100:200::1": {{Protocol: "tcp", Port: intRef(80)}},
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
func newPodWithIPs(namespace, name string, primaryUDN bool, ips []string, openPorts ...util.OpenPort) *corev1.Pod {
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
	annotations[types.OvnPodAnnotationName] = fmt.Sprintf(`{"default": {"role": "%s", "ip_addresses":[%s], "mac_address":"0a:58:0a:f4:02:03"}}`,
		role, strings.Join(annoPodIPs, ","))

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			UID:         ktypes.UID(name),
			Namespace:   namespace,
			Annotations: annotations,
		},
	}
}

// nftRunFailer fails the first `failures` calls to Run, then delegates to the wrapped
// interface. It stands in for a kernel that rejects the socket cgroupv2 match.
type nftRunFailer struct {
	knftables.Interface
	failures int
}

func (f *nftRunFailer) Run(ctx context.Context, tx *knftables.Transaction) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("Could not process rule: No such file or directory")
	}
	return f.Interface.Run(ctx, tx)
}

var _ = Describe("UDN Host isolation setup", func() {
	const nodeName = "node1"

	var (
		manager  *UDNHostIsolationManager
		fakeNFT  *knftables.Fake
		recorder *record.FakeRecorder
	)

	// expectedDump returns the isolation ruleset, with the kubelet rule only when a
	// kubelet cgroup path is known.
	expectedDump := func(kubeletCgroupPath string) string {
		result := `add table inet ovn-kubernetes
add chain inet ovn-kubernetes udn-isolation { type filter hook output priority 0 ; comment "Host isolation for user defined networks" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-icmp-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks that allow ICMP (IPv6)" ; }
add set inet ovn-kubernetes udn-open-ports-v4 { type ipv4_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-open-ports-v6 { type ipv6_addr . inet_proto . inet_service ; comment "default network open ports of pods in user defined networks (IPv6)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v4 { type ipv4_addr ; comment "default network IPs of pods in user defined networks (IPv4)" ; }
add set inet ovn-kubernetes udn-pod-default-ips-v6 { type ipv6_addr ; comment "default network IPs of pods in user defined networks (IPv6)" ; }
add rule inet ovn-kubernetes udn-isolation ip daddr . meta l4proto . th dport @udn-open-ports-v4 accept
add rule inet ovn-kubernetes udn-isolation ip daddr @udn-open-ports-icmp-v4 meta l4proto icmp accept
`
		if kubeletCgroupPath != "" {
			result += fmt.Sprintf("add rule inet ovn-kubernetes udn-isolation socket cgroupv2 %s %q ip daddr @udn-pod-default-ips-v4 accept\n",
				cgroupv2Level(kubeletCgroupPath), kubeletCgroupPath)
		}
		result += `add rule inet ovn-kubernetes udn-isolation ip daddr @udn-pod-default-ips-v4 drop
add rule inet ovn-kubernetes udn-isolation ip6 daddr . meta l4proto . th dport @udn-open-ports-v6 accept
add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-open-ports-icmp-v6 meta l4proto icmpv6 accept
`
		if kubeletCgroupPath != "" {
			result += fmt.Sprintf("add rule inet ovn-kubernetes udn-isolation socket cgroupv2 %s %q ip6 daddr @udn-pod-default-ips-v6 accept\n",
				cgroupv2Level(kubeletCgroupPath), kubeletCgroupPath)
		}
		result += "add rule inet ovn-kubernetes udn-isolation ip6 daddr @udn-pod-default-ips-v6 drop\n"
		return result
	}

	BeforeEach(func() {
		fakeNFT = nodenft.SetFakeNFTablesHelper()
		recorder = record.NewFakeRecorder(10)
		manager = &UDNHostIsolationManager{
			ipv4:     true,
			ipv6:     true,
			nodeName: nodeName,
			recorder: recorder,
			nft:      fakeNFT,

			udnPodIPsv4:        newNFTPodElementsSet(nftablesUDNPodIPsv4, false),
			udnPodIPsv6:        newNFTPodElementsSet(nftablesUDNPodIPsv6, false),
			udnOpenPortsv4:     newNFTPodElementsSet(nftablesUDNOpenPortsv4, true),
			udnOpenPortsv6:     newNFTPodElementsSet(nftablesUDNOpenPortsv6, true),
			udnOpenPortsICMPv4: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv4, false),
			udnOpenPortsICMPv6: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv6, false),
		}
	})

	Context("kubelet cgroup lookup", func() {
		It("finds the kubelet cgroup in the systemd layout", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "kubelet.slice", kubeletCgroupName), 0o755)).To(Succeed(), "create cgroup dir")

			path, err := findKubeletCgroupPath(cgroupRoot)
			Expect(err).NotTo(HaveOccurred(), "lookup succeeds")
			Expect(path).To(Equal(filepath.Join("kubelet.slice", kubeletCgroupName)), "resolved path is relative to the cgroup root")
		})

		It("fails when kubelet does not run under a systemd-managed cgroup", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed(), "create non-systemd cgroup dir")

			_, err := findKubeletCgroupPath(cgroupRoot)
			Expect(err).To(MatchError(ContainSubstring("no \"kubelet.service\" directory found")), "reports the missing directory")
		})
	})

	Context("nftables setup", func() {
		It("allows kubelet to reach UDN pods when the kubelet cgroup is known", func() {
			manager.kubeletCgroupPath = "kubelet.slice/kubelet.service"

			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump("kubelet.slice/kubelet.service"), fakeNFT.Dump())).To(Succeed())
		})

		It("still isolates UDN pods when the kubelet cgroup is unknown", func() {
			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump(""), fakeNFT.Dump())).To(Succeed())
		})
	})

	DescribeTable("derives the socket cgroupv2 match level from the cgroup path depth",
		func(cgroupPath, expectedLevel string) {
			Expect(cgroupv2Level(cgroupPath)).To(Equal(expectedLevel))
		},
		Entry("systemd layout", "kubelet.slice/kubelet.service", "level 2"),
		Entry("single component", "kubelet", "level 1"),
		Entry("nested layout", "runtime.slice/kubelet.slice/kubelet.service", "level 3"),
	)

	DescribeTable("quotes the cgroup path for the socket cgroupv2 match",
		func(cgroupPath, expected string) {
			Expect(quoteCgroupPath(cgroupPath)).To(Equal(expected))
		},
		Entry("plain path", "kubelet.slice/kubelet.service", `"kubelet.slice/kubelet.service"`),
		// "@" starts a set reference in nft, so an unquoted template unit is a parse
		// error rather than a match that never fires.
		Entry("template unit", "system-getty.slice/getty@tty1.service", `"system-getty.slice/getty@tty1.service"`),
		Entry("colon in name", "system.slice/foo:bar.service", `"system.slice/foo:bar.service"`),
		Entry("backslash is escaped", `system.slice/systemd\x2dcryptsetup.slice`, `"system.slice/systemd\\x2dcryptsetup.slice"`),
		Entry("quote cannot end the string early", `system.slice/foo".service`, `"system.slice/foo\".service"`),
	)

	Context("configured kubelet cgroup path", func() {
		AfterEach(func() {
			config.OvnKubeNode.KubeletCgroupPath = ""
		})

		It("uses the configured path instead of looking one up", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed(), "create cgroup dir")
			config.OvnKubeNode.KubeletCgroupPath = "podruntime/kubelet"

			path, err := manager.kubeletCgroupPathToUse(cgroupRoot)
			Expect(err).NotTo(HaveOccurred(), "configured path is used")
			Expect(path).To(Equal("podruntime/kubelet"), "configured path is returned as given")
		})

		It("fails when the configured path does not exist", func() {
			config.OvnKubeNode.KubeletCgroupPath = "podruntime/kubelet"

			_, err := manager.kubeletCgroupPathToUse(GinkgoT().TempDir())
			Expect(err).To(MatchError(ContainSubstring("configured kubelet cgroup path")), "names the configured path")
		})

		It("looks the path up when none is configured", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "kubelet.slice", kubeletCgroupName), 0o755)).To(Succeed(), "create cgroup dir")

			path, err := manager.kubeletCgroupPathToUse(cgroupRoot)
			Expect(err).NotTo(HaveOccurred(), "lookup is used")
			Expect(path).To(Equal(filepath.Join("kubelet.slice", kubeletCgroupName)), "looked up path")
		})
	})

	Context("degrading when the kernel rejects the match", func() {
		BeforeEach(func() {
			if !hostUsesCgroupv2() {
				Skip("the resolve path only reaches the kernel check on a cgroup v2 host")
			}
		})
		AfterEach(func() {
			config.OvnKubeNode.KubeletCgroupPath = ""
		})

		It("still isolates UDN pods, without the kubelet rule, and reports it", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed(), "create cgroup dir")
			config.OvnKubeNode.KubeletCgroupPath = "podruntime/kubelet"
			// the kernel rejects the match, as one built without CONFIG_NFT_SOCKET does.
			manager.nft = &nftRunFailer{Interface: fakeNFT, failures: 1}

			manager.setKubeletCgroupPath(cgroupRoot)

			Expect(manager.kubeletCgroupPath).To(BeEmpty(), "the kubelet rule is left out")
			var event string
			Expect(recorder.Events).To(Receive(&event), "the node is told")
			Expect(event).To(ContainSubstring("UDNKubeletProbesNotSupported"), "event reason")
			Expect(event).To(ContainSubstring("socket cgroupv2 match"), "event blames the kernel, not the path")

			// host isolation is still set up, only the kubelet rule is missing.
			manager.nft = fakeNFT
			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump(""), fakeNFT.Dump())).To(Succeed(), "UDN pods stay isolated from the host")
		})

		It("uses the configured path when the kernel accepts the match", func() {
			cgroupRoot := GinkgoT().TempDir()
			Expect(os.MkdirAll(filepath.Join(cgroupRoot, "podruntime", "kubelet"), 0o755)).To(Succeed(), "create cgroup dir")
			config.OvnKubeNode.KubeletCgroupPath = "podruntime/kubelet"

			manager.setKubeletCgroupPath(cgroupRoot)

			Expect(manager.kubeletCgroupPath).To(Equal("podruntime/kubelet"), "the configured path is used")
			Expect(recorder.Events).NotTo(Receive(), "nothing is reported")
			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump("podruntime/kubelet"), fakeNFT.Dump())).To(Succeed(), "kubelet rule is installed")
		})
	})

	Context("socket cgroupv2 kernel support", func() {
		It("reports support and leaves nothing behind", func() {
			Expect(manager.socketCgroupv2MatchSupported("kubelet.slice/kubelet.service")).To(Succeed(), "match is supported")
			Expect(fakeNFT.Dump()).NotTo(ContainSubstring(socketMatchProbeChain), "probe chain is removed")
		})

		It("reports no support when the kernel rejects the match", func() {
			manager.nft = &nftRunFailer{Interface: fakeNFT, failures: 1}

			Expect(manager.socketCgroupv2MatchSupported("kubelet.slice/kubelet.service")).NotTo(Succeed(), "match is unsupported")
		})
	})

	DescribeTable("derives the systemd unit that owns the kubelet cgroup",
		func(cgroupPath, expectedUnit string) {
			unit, err := kubeletSystemdUnit(cgroupPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(unit).To(Equal(expectedUnit))
		},
		Entry("systemd layout", "kubelet.slice/kubelet.service", "kubelet.service"),
		Entry("renamed unit", "system.slice/rke2-agent.service", "rke2-agent.service"),
		Entry("scope", "kubelet.slice/kubelet.scope", "kubelet.scope"),
	)

	DescribeTable("reports no systemd unit for a cgroup that is not a unit",
		func(cgroupPath string) {
			_, err := kubeletSystemdUnit(cgroupPath)
			// Start distinguishes this from a real tracker failure, so the type matters
			// as well as the text.
			Expect(err).To(MatchError(errNoSystemdUnit), "reports the expected non-systemd case")
			Expect(err.Error()).To(ContainSubstring(cgroupPath), "names the cgroup")
		},
		// a host that does not run kubelet under systemd puts it in a plain cgroup.
		Entry("non-systemd host", "podruntime/kubelet"),
		// an ancestor cgroup is not a unit either.
		Entry("slice", "kubelet.slice"),
	)

	DescribeTable("decodes systemd unit names from D-Bus object paths",
		func(escapedUnit, expectedUnit string) {
			Expect(decodeSystemdUnitName(escapedUnit)).To(Equal(expectedUnit))
		},
		Entry("dot only", "kubelet_2eservice", "kubelet.service"),
		// systemd escapes every character outside [A-Za-z0-9], so a renamed unit
		// carries more than the dot and is not matched by decoding the dot alone.
		Entry("hyphen and dot", "custom_2dkubelet_2eservice", "custom-kubelet.service"),
		Entry("uppercase hex", "custom_2Dkubelet_2Eservice", "custom-kubelet.service"),
		Entry("template unit", "getty_40tty1_2eservice", "getty@tty1.service"),
		Entry("nothing to decode", "kubelet", "kubelet"),
		Entry("incomplete escape is kept", "kubelet_2", "kubelet_2"),
		Entry("invalid escape is kept", "custom_zzkubelet", "custom_zzkubelet"),
	)

	Context("kubelet restart signals", func() {
		const kubeletUnit = "kubelet.service"

		propertiesChanged := func(unitPath string, body ...interface{}) *dbus.Signal {
			return &dbus.Signal{
				Path: dbus.ObjectPath(unitPath),
				Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
				Body: body,
			}
		}
		activeState := func(state interface{}) []interface{} {
			return []interface{}{
				"org.freedesktop.systemd1.Unit",
				map[string]dbus.Variant{"ActiveState": dbus.MakeVariant(state)},
			}
		}

		// The rules are installed for the original path, then the resolved path is
		// changed before the signal is delivered. Re-application is therefore visible
		// as the ruleset switching to the new path, rather than being asserted against
		// what the setup already produced.
		const originalPath = "kubelet.slice/kubelet.service"
		const reresolvedPath = "kubelet.slice/kubelet-1234.service"

		BeforeEach(func() {
			manager.kubeletCgroupPath = originalPath
			Expect(manager.setupUDNIsolationFromHost()).To(Succeed())
			Expect(nodenft.MatchNFTRules(expectedDump(originalPath), fakeNFT.Dump())).To(Succeed(), "rules start on the original path")
			manager.kubeletCgroupPath = reresolvedPath
		})

		It("re-applies the rules when the kubelet unit becomes active", func() {
			signal := propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice", activeState("active")...)

			manager.handleKubeletRestartSignal(signal, kubeletUnit)
			Expect(nodenft.MatchNFTRules(expectedDump(reresolvedPath), fakeNFT.Dump())).To(Succeed(), "rules are rebuilt for the re-resolved path")
		})

		It("re-applies the rules for a renamed unit", func() {
			// systemd escapes every character outside [A-Za-z0-9], so this only matches
			// if the object path is decoded in full rather than just "_2e".
			signal := propertiesChanged("/org/freedesktop/systemd1/unit/rke2_2dagent_2eservice", activeState("active")...)

			manager.handleKubeletRestartSignal(signal, "rke2-agent.service")
			Expect(nodenft.MatchNFTRules(expectedDump(reresolvedPath), fakeNFT.Dump())).To(Succeed(), "renamed unit is recognised and the rules are rebuilt")
		})

		DescribeTable("ignores signals it cannot act on, without panicking",
			func(signal *dbus.Signal) {
				manager.handleKubeletRestartSignal(signal, kubeletUnit)
				Expect(nodenft.MatchNFTRules(expectedDump(originalPath), fakeNFT.Dump())).To(Succeed(), "rules are left alone")
			},
			Entry("another unit", propertiesChanged("/org/freedesktop/systemd1/unit/sshd_2eservice", activeState("active")...)),
			Entry("not a unit path", propertiesChanged("/org/freedesktop/systemd1/job/42", activeState("active")...)),
			Entry("short path", propertiesChanged("/org/freedesktop", activeState("active")...)),
			Entry("inactive state", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice", activeState("inactive")...)),
			// the systemd private connection is not a bus and delivers every signal, so
			// the body can be any shape at all. None of these may panic the tracker.
			Entry("empty body", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice")),
			Entry("body too short", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice", "org.freedesktop.systemd1.Unit")),
			Entry("changes not a variant map", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice",
				"org.freedesktop.systemd1.Unit", "not-a-map")),
			Entry("no ActiveState key", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice",
				"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"SubState": dbus.MakeVariant("running")})),
			Entry("ActiveState not a string", propertiesChanged("/org/freedesktop/systemd1/unit/kubelet_2eservice", activeState(42)...)),
		)
	})

	It("reports unsupported kubelet probes on the node", func() {
		manager.reportKubeletProbesUnsupported("the node uses cgroup v1")

		var event string
		Expect(recorder.Events).To(Receive(&event), "an event is recorded")
		Expect(event).To(ContainSubstring("UDNKubeletProbesNotSupported"), "event reason")
		Expect(event).To(ContainSubstring("the node uses cgroup v1"), "event carries the reason")
	})
})
