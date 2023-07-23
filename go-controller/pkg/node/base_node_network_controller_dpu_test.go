package node

import (
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	factorymocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	linkMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/vswitchdb"

	"k8s.io/client-go/kubernetes/fake"
)

func genOVSFindCmd(timeout, table, column, condition string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=%s --no-heading --format=csv --data=bare --columns=%s find %s %s",
		timeout, column, table, condition)
}

func genOVSAddPortCmd(hostIfaceName, ifaceID, mac, ip, sandboxID, podUID string) string {
	ipAddrExtID := ""
	if ip != "" {
		ipAddrExtID = fmt.Sprintf("external_ids:ip_addresses=%s ", ip)
	}
	return fmt.Sprintf("ovs-vsctl --timeout=30 --may-exist add-port br-int %s other_config:transient=true "+
		"-- set interface %s external_ids:attached_mac=%s external_ids:iface-id=%s external_ids:iface-id-ver=%s "+
		"%sexternal_ids:sandbox=%s external_ids:vf-netdev-name=%s "+
		"-- --if-exists remove interface %s external_ids k8s.ovn.org/network "+
		"-- --if-exists remove interface %s external_ids k8s.ovn.org/nad",
		hostIfaceName, hostIfaceName, mac, ifaceID, podUID, ipAddrExtID, sandboxID, hostIfaceName, hostIfaceName, hostIfaceName)
}

func genOVSDelPortCmd(portName string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists del-port br-int %s", portName)
}

func genOVSGetCmd(table, record, column, key string) string {
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=30 --if-exists get %s %s %s", table, record, column)
}

func genOfctlDumpFlowsCmd(queryStr string) string {
	return fmt.Sprintf("ovs-ofctl --timeout=10 --no-stats --strict dump-flows br-int %s", queryStr)
}

func genIfaceID(podNamespace, podName string) string {
	return fmt.Sprintf("%s_%s", podNamespace, podName)
}

func checkOVSPortPodInfo(execMock *ovntest.FakeExec, vfRep string, exists bool, timeout, sandbox string, nadName string) {
	output := ""
	if exists {
		output = fmt.Sprintf("sandbox=%s", sandbox)
		if nadName != types.DefaultNetworkName {
			output = output + " k8s.ovn.org/nad=" + nadName
		}
	}
	execMock.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    genOVSFindCmd(timeout, "Interface", "external_ids", "name="+vfRep),
		Output: output,
	})
}

func newFakeKubeClientWithPod(pod *v1.Pod) *fake.Clientset {
	return fake.NewSimpleClientset(&v1.PodList{Items: []v1.Pod{*pod}})
}

var _ = Describe("Node DPU tests", func() {
	var sriovnetOpsMock utilMocks.SriovnetOps
	var netlinkOpsMock utilMocks.NetLinkOps
	var execMock *ovntest.FakeExec
	var kubeMock kubemocks.Interface
	var factoryMock factorymocks.NodeWatchFactory
	var pod v1.Pod
	var dnnc *DefaultNodeNetworkController
	var podInformer coreinformermocks.PodInformer
	var podLister v1mocks.PodLister
	var podNamespaceLister v1mocks.PodNamespaceLister
	var clientset *cni.ClientSet
	var testdbCtx *libovsdbtest.Context

	origSriovnetOps := util.GetSriovnetOps()
	origNetlinkOps := util.GetNetLinkOps()

	BeforeEach(func() {
		sriovnetOpsMock = utilMocks.SriovnetOps{}
		netlinkOpsMock = utilMocks.NetLinkOps{}
		execMock = ovntest.NewFakeExec()

		util.SetSriovnetOpsInst(&sriovnetOpsMock)
		util.SetNetLinkOpMockInst(&netlinkOpsMock)
		err := util.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		err = cni.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())

		kubeMock = kubemocks.Interface{}
		apbExternalRouteClient := adminpolicybasedrouteclient.NewSimpleClientset()
		factoryMock = factorymocks.NodeWatchFactory{}
		cnnci := newCommonNodeNetworkControllerInfo(nil, &kubeMock, apbExternalRouteClient, &factoryMock, nil, "")

		dnnc = newDefaultNodeNetworkController(cnnci, make(chan struct{}), &sync.WaitGroup{})
		dnnc.vsClient, testdbCtx, err = libovsdbtest.NewVSTestHarness(libovsdbtest.TestSetup{}, nil)
		Expect(err).NotTo(HaveOccurred())

		podInformer = coreinformermocks.PodInformer{}
		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
		podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)

		pod = v1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "a-pod",
			Namespace:   "foo-ns",
			UID:         "a-pod",
			Annotations: map[string]string{},
		}}
	})

	AfterEach(func() {
		dnnc.Stop()
		testdbCtx.Cleanup()

		// Restore mocks so it does not affect other tests in the suite
		util.SetSriovnetOpsInst(origSriovnetOps)
		util.SetNetLinkOpMockInst(origNetlinkOps)
		cni.ResetRunner()
		util.ResetRunner()
	})

	Context("addRepPort", func() {
		var vfRep string
		var vfLink *linkMock.Link
		var ifInfo *cni.PodInterfaceInfo
		var scd util.DPUConnectionDetails

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfLink = &linkMock.Link{}
			ifInfo = &cni.PodInterfaceInfo{
				PodAnnotation: util.PodAnnotation{},
				MTU:           1500,
				Ingress:       -1,
				Egress:        -1,
				IsDPUHostMode: true,
				NetName:       types.DefaultNetworkName,
				NADName:       types.DefaultNetworkName,
				PodUID:        "a-pod",
			}

			fakeClient := newFakeKubeClientWithPod(&pod)
			clientset = cni.NewClientSet(fakeClient, &podLister)
			scd = util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "9",
				SandboxId: "a8d09931",
			}
			podAnnot, err := util.MarshalPodDPUConnDetails(nil, &scd, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			// set pod annotations
			pod.Annotations = podAnnot
		})

		It("Fails if GetVfRepresentorDPU fails", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return("", fmt.Errorf("failed to get VF representor"))
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			// call addRepPort()
			err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get VF representor"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)

			// Don't pre-seed the vswitch DB with br-int to ensure ConfigureOVS fails

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			// call addRepPort()
			err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to find bridge br-int: object not found"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())

			// No changes should have been made to the vswitch
			matcher := libovsdbtest.HaveData([]libovsdbtest.TestData{})
			ok, err := matcher.Match(dnnc.vsClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
		})

		It("Fails if configure OVS fails but OVS interface is added", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)

			// Mock netlink/ovs calls for cleanup
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			dbSetup := []libovsdbtest.TestData{
				&vswitchdb.Bridge{
					UUID: "bridge-uuid",
					Name: "br-int",
				},
			}
			err := testdbCtx.VSServer.CreateTestData(dbSetup)
			Expect(err).NotTo(HaveOccurred())

			// call addRepPort(); since there's no ovs-vswitchd to
			// assign the Interface an ofport, the call will fail
			err = dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("ovs interface \"%s\" ofport not set", vfRep)))

			// Ensure ovsdb contents are as expected; everything should
			// have been cleaned up
			Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData(dbSetup))
		})

		Context("After successfully calling ConfigureOVS", func() {
			ofport := 1
			var bridge *vswitchdb.Bridge
			BeforeEach(func() {
				sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)
				// waitForPodFlows
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOfctlDumpFlowsCmd("table=9,dl_src="),
					Output: "non-empty-output",
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOfctlDumpFlowsCmd(fmt.Sprintf("table=0,in_port=%d", ofport)),
					Output: "non-empty-output",
				})

				bridge = &vswitchdb.Bridge{
					UUID: "bridge-uuid",
					Name: "br-int",
				}
				dbSetup := []libovsdbtest.TestData{
					bridge,
					// Pre-seed the DB with an interface that has an Ofport
					// so the !DPUHost codepath works correctly
					&vswitchdb.Interface{
						UUID:   "iface-uuid",
						Name:   vfRep,
						Ofport: &ofport,
					},
				}
				err := testdbCtx.VSServer.CreateTestData(dbSetup)
				Expect(err).NotTo(HaveOccurred())
			})

			Context("Fails if link configuration fails on", func() {
				It("LinkByName()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())

					Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
				})

				It("LinkSetMTU()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(fmt.Errorf("failed to set mtu"))
					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					// Mock netlink calls for cleanup
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())

					Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
				})

				It("LinkSetUp()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
					netlinkOpsMock.On("LinkSetUp", vfLink).Return(fmt.Errorf("failed to set link up"))
					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					// Mock netlink calls for cleanup
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())

					Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
				})
			})

			It("Sets dpu.connection-status pod annotation on success", func() {
				var err error
				netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
				netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
				netlinkOpsMock.On("LinkSetUp", vfLink).Return(nil)
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, &dcs, types.DefaultNetworkName)
				Expect(err).ToNot(HaveOccurred())

				factoryMock.On("PodCoreInformer").Return(&podInformer)
				podInformer.On("Lister").Return(&podLister)
				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("UpdatePod", cpod).Return(nil)

				err = dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
				Expect(err).ToNot(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})

			It("cleans up representor port if set pod annotation fails", func() {
				var err error
				netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
				netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
				netlinkOpsMock.On("LinkSetUp", vfLink).Return(nil)
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, &dcs, types.DefaultNetworkName)
				Expect(err).ToNot(HaveOccurred())
				// Mock netlink calls for cleanup
				netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

				factoryMock.On("PodCoreInformer").Return(&podInformer)
				podInformer.On("Lister").Return(&podLister)
				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("UpdatePod", cpod).Return(fmt.Errorf("failed to set pod annotations"))

				err = dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
				Expect(err).To(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())

				Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
			})
		})
	})

	Context("delRepPort", func() {
		const vfRep string = "pf0vf9"
		const sandboxID string = "a8d09931"
		const ifaceUUID string = "u5553535353"
		const portUUID string = "u124125521521"
		var vfLink *linkMock.Link
		var scd util.DPUConnectionDetails
		var bridge *vswitchdb.Bridge
		var intf *vswitchdb.Interface
		var port *vswitchdb.Port

		BeforeEach(func() {
			vfLink = &linkMock.Link{}
			scd = util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "9",
				SandboxId: sandboxID,
			}

			bridge = &vswitchdb.Bridge{
				UUID:  "bridge-uuid",
				Name:  "br-int",
				Ports: []string{portUUID},
			}
			intf = &vswitchdb.Interface{
				UUID: ifaceUUID,
				Name: vfRep,
				ExternalIDs: map[string]string{
					"sandbox": sandboxID,
				},
			}
			port = &vswitchdb.Port{
				UUID: portUUID,
				Name: vfRep,
				OtherConfig: map[string]string{
					"transient": "true",
				},
				Interfaces: []string{ifaceUUID},
			}
		})

		It("Sets link down for VF representor and removes VF representor from OVS", func() {
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			err := testdbCtx.VSServer.CreateTestData([]libovsdbtest.TestData{bridge, intf, port})
			Expect(err).NotTo(HaveOccurred())

			err = dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())

			bridge.Ports = nil
			Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
		})

		It("Does not fail if LinkByName failed", func() {
			netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))

			err := testdbCtx.VSServer.CreateTestData([]libovsdbtest.TestData{bridge, intf, port})
			Expect(err).NotTo(HaveOccurred())

			err = dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())

			bridge.Ports = nil
			Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
		})

		It("Does not fail if removal of VF representor from OVS fails once", func() {
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			// Add only the interface to OVS to ensure the first call
			// to DeletePort fails becuase the bridge doesn't exist
			err := testdbCtx.VSServer.CreateTestData([]libovsdbtest.TestData{intf, port})
			Expect(err).NotTo(HaveOccurred())

			wg := sync.WaitGroup{}
			wg.Add(1)
			go func() {
				wg.Done()
				<-time.After(1 * time.Second)
				// Add the bridge after a short delay to allow
				// DeletePort to succeed
				port, err := libovsdbops.FindPortByName(dnnc.vsClient, port.Name)
				Expect(err).NotTo(HaveOccurred())
				bridge.Ports = []string{port.UUID}
				err = testdbCtx.VSServer.CreateTestData([]libovsdbtest.TestData{bridge})
				Expect(err).NotTo(HaveOccurred())
			}()
			wg.Wait()

			err = dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())

			bridge.Ports = nil
			Eventually(dnnc.vsClient).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{bridge}))
		})
	})
})
