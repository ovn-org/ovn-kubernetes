package controller

import (
	"context"
	"sync"

	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

	"github.com/vishvananda/netlink"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	hoNodeName   string = "worker-1"
	hoNodeIP     string = "10.0.0.1"
	hoNodeSubnet string = "20.0.0.0/24"
	hoNodeDRMAC  string = "22:33:44:55:66:77"
)

func appHONRun(app *cli.App) {
	err := app.Run([]string{
		app.Name,
		"-no-hostsubnet-nodes=" + v1.LabelOSStable + "=linux",
	})
	Expect(err).NotTo(HaveOccurred())
}

func addHybridOverlayAddNodeMocks(nlMock *mocks.NetLinkOps) {
	mockEth0IP := netlink.Addr{IPNet: ovntest.MustParseIPNet(hoNodeIP + "/24")}
	mockEth0 := &fakeLink{
		attrs: &netlink.LinkAttrs{
			Index:        777,
			HardwareAddr: ovntest.MustParseMAC(hoNodeDRMAC),
		},
	}

	nlMocks := []ovntest.TestifyMockHelper{
		{
			OnCallMethodName: "LinkList",
			RetArgList:       []interface{}{[]netlink.Link{mockEth0}, nil},
		},
		{
			OnCallMethodName: "AddrList",
			OnCallMethodArgs: []interface{}{mockEth0, netlink.FAMILY_ALL},
			RetArgList: []interface{}{
				[]netlink.Addr{
					mockEth0IP,
				},
				nil,
			},
		},
	}
	ovntest.ProcessMockFnList(&nlMock.Mock, nlMocks)
}

var _ = Describe("Hybrid Overlay Node Linux Operations", func() {

	var (
		app      *cli.App
		stopChan chan struct{}
		wg       *sync.WaitGroup
		nlMock   *mocks.NetLinkOps
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		stopChan = make(chan struct{})
		wg = &sync.WaitGroup{}

		// prepare eth0 in netlink mock
		nlMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)
	})

	AfterEach(func() {
		close(stopChan)
		wg.Wait()
		util.ResetNetLinkOpMockInst()
	})

	ovntest.OnSupportedPlatformsIt("Set VTEP and gateway MAC address to its own node object annotations", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			addHybridOverlayAddNodeMocks(nlMock)

			testNode := createNode(hoNodeName, "linux", hoNodeIP, map[string]string{
				types.HybridOverlayNodeSubnet: hoNodeSubnet,
			})

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					*testNode,
				},
			})

			f := informers.NewSharedInformerFactory(fakeClient, informer.DefaultResyncInterval)

			n, err := NewNode(
				&kube.Kube{KClient: fakeClient},
				hoNodeName,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				true,
			)
			Expect(err).NotTo(HaveOccurred())

			err = n.controller.AddNode(testNode)
			Expect(err).NotTo(HaveOccurred())
			f.WaitForCacheSync(stopChan)

			Eventually(func() (map[string]string, error) {
				updatedNode, err := fakeClient.CoreV1().Nodes().Get(context.TODO(), hoNodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return updatedNode.Annotations, nil
			}, 2).Should(HaveKeyWithValue(types.HybridOverlayDRMAC, hoNodeDRMAC))
			return nil
		}
		appHONRun(app)
	})

	ovntest.OnSupportedPlatformsIt("Remove ovnkube annotations for pod on Hybrid Overlay node", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			addHybridOverlayAddNodeMocks(nlMock)

			testPod1 := createPod("default", "testpod-1", hoNodeName, "2.2.3.5/24", "aa:bb:cc:dd:ee:ff")
			testPod2 := createPod("default", "testpod-2", "other-worker", "2.2.3.6/24", "ab:bb:cc:dd:ee:ff")
			testNode := createNode(hoNodeName, "linux", hoNodeIP, map[string]string{
				types.HybridOverlayNodeSubnet: hoNodeSubnet,
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
				hoNodeName,
				f.Core().V1().Nodes().Informer(),
				f.Core().V1().Pods().Informer(),
				informer.NewTestEventHandler,
				true,
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
		appHONRun(app)
	})
})
