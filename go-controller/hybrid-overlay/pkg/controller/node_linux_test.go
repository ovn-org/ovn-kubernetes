package controller

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	testNodeName   string = "worker-1"
	testDRMAC      string = "00:00:00:7a:af:04"
	testNodeSubnet string = "2.2.3.0/24"
	testNodeIP     string = "1.2.3.3/24"
)

func createNode(name, ip string, annotations map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: ip},
			},
		},
	}
}

func createPod(namespace, name, node, podIP, podMAC string) *v1.Pod {
	annotations := map[string]string{}
	if podIP != "" || podMAC != "" {
		ipn := ovntest.MustParseIPNet(podIP)
		gatewayIP := util.NextIP(ipn.IP)
		annotations[util.OvnPodAnnotationName] = fmt.Sprintf(`{"default": {"ip_address":"` + podIP + `", "mac_address":"` + podMAC + `", "gateway_ip": "` + gatewayIP.String() + `"}}`)
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Annotations: annotations,
		},
		Spec: v1.PodSpec{
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
	}
}

func appRun(app *cli.App, netns ns.NetNS) {
	_ = netns.Do(func(netns ns.NetNS) error {
		defer GinkgoRecover()
		err := app.Run([]string{
			app.Name,
		})
		Expect(err).NotTo(HaveOccurred())
		return nil
	})
}

var _ = Describe("Hybrid Overlay Node Linux Operations", func() {
	var (
		app      *cli.App
		netns    ns.NetNS
		stopChan chan struct{}
		wg       *sync.WaitGroup
		err      error
	)

	BeforeEach(func() {
		app = cli.NewApp()
		app.Name = "test"

		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}

		netns, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())

		// prepare eth0 in original namespace
		_ = netns.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			// Set up default interface
			link := ovntest.AddLink("eth0")
			drMAC, err := net.ParseMAC(testDRMAC)
			Expect(err).NotTo(HaveOccurred())
			netlink.LinkSetHardwareAddr(link, drMAC)
			Expect(err).NotTo(HaveOccurred())
			nodeIP, err := netlink.ParseAddr(testNodeIP)
			Expect(err).NotTo(HaveOccurred())
			netlink.AddrAdd(link, nodeIP)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
	})

	AfterEach(func() {
		close(stopChan)
		wg.Wait()
		Expect(netns.Close()).To(Succeed())
		Expect(testutils.UnmountNS(netns)).To(Succeed())
	})

	FIt("Set VTEP and gateway MAC address to its own node object annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			ip := strings.Split(testNodeIP, "/")
			testNode := createNode(testNodeName, ip[0], map[string]string{
				types.HybridOverlayNodeSubnet: testNodeSubnet,
			})

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*testNode,
				},
			})

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				testNodeName,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			err = n.controller.AddNode(testNode)
			Expect(err).NotTo(HaveOccurred())
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), testNodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(types.HybridOverlayDRMAC, testDRMAC))
			return nil
		}
		appRun(app, netns)
	})

	FIt("Remove ovnkube annotations for pod on Hybrid Overlay node", func() {
		app.Action = func(ctx *cli.Context) error {
			testPod1 := createPod("default", "testpod-1", testNodeName, "2.2.3.5/24", "aa:bb:cc:dd:ee:ff")
			testPod2 := createPod("default", "testpod-2", "other-worker", "2.2.3.6/24", "ab:bb:cc:dd:ee:ff")
			ip := strings.Split(testNodeIP, "/")
			testNode := createNode(testNodeName, ip[0], map[string]string{
				types.HybridOverlayNodeSubnet: testNodeSubnet,
			})

			fakeClient := fake.NewSimpleClientset(&v1.PodList{
				Items: []v1.Pod{
					*testPod1,
					*testPod2,
				},
			}, &v1.NodeList{
				Items: []v1.Node{
					*testNode,
				},
			})

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				testNodeName,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
			)
			Expect(err).NotTo(HaveOccurred())

			err = n.controller.AddNode(testNode)
			Expect(err).NotTo(HaveOccurred())
			err = n.controller.AddPod(testPod1)
			Expect(err).NotTo(HaveOccurred())
			err = n.controller.AddPod(testPod2)
			Expect(err).NotTo(HaveOccurred())
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedPod, err := fakeClient.CoreV1().Pods("default").Get(context.TODO(), "testpod-1", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedPod.Annotations, nil
			}, 2).ShouldNot(HaveKey(util.OvnPodAnnotationName))

			Eventually(func() (map[string]string, error) {
				updatedPod, err := fakeClient.CoreV1().Pods("default").Get(context.TODO(), "testpod-2", metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedPod.Annotations, nil
			}, 2).Should(HaveKey(util.OvnPodAnnotationName))
			return nil
		}
		appRun(app, netns)
	})
})
