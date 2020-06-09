package controller

import (
	"fmt"
	"strings"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func addGetPortAddressesCmds(fexec *ovntest.FakeExec, nodeName, hybMAC, hybIP string) {
	addresses := hybMAC + " " + hybIP
	addresses = strings.TrimSpace(addresses)

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovn-nbctl --timeout=15 get logical_switch_port int-" + nodeName + " dynamic_addresses addresses",
		// hybrid overlay ports have static addresses
		Output: "[]\n[" + addresses + "]\n",
	})
}

func newTestNode(name, os, ovnHostSubnet, hybridHostSubnet, drMAC string) v1.Node {
	annotations := make(map[string]string)
	if ovnHostSubnet != "" {
		subnetAnnotations, err := util.CreateNodeHostSubnetAnnotation(ovntest.MustParseIPNets(ovnHostSubnet))
		Expect(err).NotTo(HaveOccurred())
		for k, v := range subnetAnnotations {
			annotations[k] = fmt.Sprintf("%s", v)
		}
	}
	if hybridHostSubnet != "" {
		annotations[types.HybridOverlayNodeSubnet] = hybridHostSubnet
	}
	if drMAC != "" {
		annotations[types.HybridOverlayDRMAC] = drMAC
	}
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{v1.LabelOSStable: os},
			Annotations: annotations,
		},
	}
}

var _ = Describe("Hybrid SDN Master Operations", func() {
	var (
		app      *cli.App
		f        *factory.WatchFactory
		stopChan chan struct{}
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		stopChan = make(chan struct{})
	})

	AfterEach(func() {
		close(stopChan)
		f.Shutdown()
	})

	const hybridOverlayClusterCIDR string = "11.1.0.0/16/24"
	It("allocates and assigns a hybrid-overlay subnet to a Windows node that doesn't have one", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "11.1.0.0/24"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "windows", "", "", ""),
				},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f, err = factory.NewWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())

			err = StartMaster(&kube.Kube{KClient: fakeClient}, f)
			Expect(err).NotTo(HaveOccurred())

			// Windows node should be allocated a subnet
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(types.HybridOverlayNodeSubnet, nodeSubnet))
			_, err = util.ParseNodeHostSubnetAnnotation(updatedNode)
			Expect(err).To(MatchError(fmt.Sprintf("node %q has no \"k8s.ovn.org/node-subnets\" annotation", nodeName)))

			Expect(fexec.CalledMatchesExpected()).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-no-hostsubnet-nodes=" + v1.LabelOSStable + "=windows",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up and cleans up a Linux node with a OVN hostsubnet annotation", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", ""),
				},
			})

			fexec := ovntest.NewFakeExec()
			addGetPortAddressesCmds(fexec, nodeName, nodeHOMAC, nodeHOIP)

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f, err = factory.NewWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())

			err = StartMaster(&kube.Kube{KClient: fakeClient}, f)
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(types.HybridOverlayDRMAC, nodeHOMAC))

			// Test that deleting the node cleans up the OVN objects
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists lsp-del int-node1",
			})

			err = fakeClient.CoreV1().Nodes().Delete(nodeName, metav1.NewDeleteOptions(0))
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cleans up a Linux node when the OVN hostsubnet annotation is removed", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC),
				},
			})

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists lsp-del int-node1",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			f, err = factory.NewWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())

			err = StartMaster(&kube.Kube{KClient: fakeClient}, f)
			Expect(err).NotTo(HaveOccurred())

			k := &kube.Kube{KClient: fakeClient}
			updatedNode, err := k.GetNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(k, updatedNode)
			util.DeleteNodeHostSubnetAnnotation(nodeAnnotator)
			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			Eventually(fexec.CalledMatchesExpected, 2).Should(BeTrue(), fexec.ErrorDesc)

			updatedNode, err = k.GetNode(nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).NotTo(HaveKey(types.HybridOverlayDRMAC))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("copies namespace annotations when a pod is added", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nsName     string = "nstest"
				nsVTEP            = "1.1.1.1"
				nsExGw            = "2.2.2.2"
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOMAC  string = "00:00:00:52:19:d2"
				pod1Name   string = "pod1"
				pod1IP     string = "1.2.3.5"
				pod1CIDR   string = pod1IP + "/24"
				pod1MAC    string = "aa:bb:cc:dd:ee:ff"
			)

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  k8stypes.UID(nsName),
					Name: nsName,
					Annotations: map[string]string{
						types.HybridOverlayVTEP:       nsVTEP,
						types.HybridOverlayExternalGw: nsExGw,
					},
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}
			fakeClient := fake.NewSimpleClientset([]runtime.Object{
				ns,
				createPod(nsName, pod1Name, nodeName, pod1CIDR, pod1MAC),
				&v1.NodeList{Items: []v1.Node{newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC)}},
			}...)

			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			f, err = factory.NewWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())

			err = StartMaster(&kube.Kube{KClient: fakeClient}, f)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				pod, err := fakeClient.CoreV1().Pods(nsName).Get(pod1Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if pod.Annotations[types.HybridOverlayVTEP] != nsVTEP {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", types.HybridOverlayVTEP, nsVTEP, pod.Annotations[types.HybridOverlayVTEP])
				}
				if pod.Annotations[types.HybridOverlayExternalGw] != nsExGw {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", types.HybridOverlayVTEP, nsExGw, pod.Annotations[types.HybridOverlayExternalGw])
				}
				return nil
			}, 2).Should(Succeed())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update pod annotations when a namespace is updated", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nsName        string = "nstest"
				nsVTEP        string = "1.1.1.1"
				nsVTEPUpdated string = "3.3.3.3"
				nsExGw        string = "2.2.2.2"
				nsExGwUpdated string = "4.4.4.4"
				nodeName      string = "node1"
				nodeSubnet    string = "10.1.2.0/24"
				nodeHOMAC     string = "00:00:00:52:19:d2"
				pod1Name      string = "pod1"
				pod1IP        string = "1.2.3.5"
				pod1CIDR      string = pod1IP + "/24"
				pod1MAC       string = "aa:bb:cc:dd:ee:ff"
			)

			ns := &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					UID:  k8stypes.UID(nsName),
					Name: nsName,
					Annotations: map[string]string{
						types.HybridOverlayVTEP:       nsVTEP,
						types.HybridOverlayExternalGw: nsExGw,
					},
				},
				Spec:   v1.NamespaceSpec{},
				Status: v1.NamespaceStatus{},
			}
			fakeClient := fake.NewSimpleClientset([]runtime.Object{
				ns,
				&v1.NodeList{Items: []v1.Node{newTestNode(nodeName, "linux", nodeSubnet, "", nodeHOMAC)}},
				createPod(nsName, pod1Name, nodeName, pod1CIDR, pod1MAC),
			}...)

			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			f, err = factory.NewWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())

			k := &kube.Kube{KClient: fakeClient}
			err = StartMaster(k, f)
			Expect(err).NotTo(HaveOccurred())

			updatedNs, err := fakeClient.CoreV1().Namespaces().Get(nsName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			nsAnnotator := kube.NewNamespaceAnnotator(k, updatedNs)
			nsAnnotator.Set(types.HybridOverlayVTEP, nsVTEPUpdated)
			nsAnnotator.Set(types.HybridOverlayExternalGw, nsExGwUpdated)
			err = nsAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				pod, err := fakeClient.CoreV1().Pods(nsName).Get(pod1Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if pod.Annotations[types.HybridOverlayVTEP] != nsVTEP {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", types.HybridOverlayVTEP, nsVTEPUpdated, pod.Annotations[types.HybridOverlayVTEP])
				}
				if pod.Annotations[types.HybridOverlayExternalGw] != nsExGw {
					return fmt.Errorf("error with annotation %s. expected: %s, got: %s", types.HybridOverlayVTEP, nsExGwUpdated, pod.Annotations[types.HybridOverlayExternalGw])
				}
				return nil
			}, 2).Should(Succeed())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=" + hybridOverlayClusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
