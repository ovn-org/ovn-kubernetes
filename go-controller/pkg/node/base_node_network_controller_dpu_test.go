package node

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	factorymocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	linkMock "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"

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
	var routeManager *routemanager.Controller

	origSriovnetOps := util.GetSriovnetOps()
	origNetlinkOps := util.GetNetLinkOps()

	BeforeEach(func() {
		config.PrepareTestConfig()
		sriovnetOpsMock = utilMocks.SriovnetOps{}
		netlinkOpsMock = utilMocks.NetLinkOps{}
		execMock = ovntest.NewFakeExec()

		util.SetSriovnetOpsInst(&sriovnetOpsMock)
		util.SetNetLinkOpMockInst(&netlinkOpsMock)
		err := util.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		err = cni.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		routeManager = routemanager.NewController()
		Expect(routeManager).NotTo(BeNil())

		kubeMock = kubemocks.Interface{}
		apbExternalRouteClient := adminpolicybasedrouteclient.NewSimpleClientset()
		factoryMock = factorymocks.NodeWatchFactory{}
		cnnci := newCommonNodeNetworkControllerInfo(nil, &kubeMock, apbExternalRouteClient, &factoryMock, nil, "", routeManager)
		dnnc = newDefaultNodeNetworkController(cnnci, nil, nil, routeManager, nil)

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
		// Restore mocks so it does not affect other tests in the suite
		util.SetSriovnetOpsInst(origSriovnetOps)
		util.SetNetLinkOpMockInst(origNetlinkOps)
		cni.ResetRunner()
		util.ResetRunner()
	})

	Context("addRepPort", func() {
		var vfRep string
		var vfPciAddress string
		var vfLink *linkMock.Link
		var ifInfo *cni.PodInterfaceInfo
		var scd util.DPUConnectionDetails

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfPciAddress = "0000:03:00.0"
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

		It("Fails if GetPCIFromDeviceName fails", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return("", fmt.Errorf("could not find PCI Address"))
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(pod, nil)

			// call addRepPort()
			err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("could not find PCI Address"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)

			sriovnetOpsMock.On("GetPciFromNetDevice", vfRep).Return("0000:03:00.8", nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			// set ovs CMD output
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
			})
			checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: "",
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				Err: fmt.Errorf("failed to run ovs command"),
			})
			// Mock netlink/ovs calls for cleanup
			checkOVSPortPodInfo(execMock, vfRep, false, "15", "", "")

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			// call addRepPort()
			err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to run ovs command"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails but OVS interface is added", func() {
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
			sriovnetOpsMock.On("GetPciFromNetDevice", vfRep).Return("0000:03:00.8", nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
			})
			// set ovs CMD output
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSFindCmd("30", "Interface", "name",
					"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
			})
			checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
				Output: "",
			})
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				Err: fmt.Errorf("failed to run ovs command"),
			})
			checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
			// Mock netlink/ovs calls for cleanup
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSDelPortCmd(vfRep),
			})
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			// call addRepPort()
			err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to run ovs command"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		Context("After successfully calling ConfigureOVS", func() {
			BeforeEach(func() {
				sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(vfRep, nil)
				sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSGetCmd("bridge", "br-int", "datapath_type", ""),
				})
				// set ovs CMD output so cni.ConfigureOVS passes without error
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "Interface", "name",
						"external-ids:iface-id="+genIfaceID(pod.Namespace, pod.Name)),
				})
				checkOVSPortPodInfo(execMock, vfRep, false, "30", "", "")
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Open_vSwitch", ".", "external_ids", "ovn-pf-encap-ip-mapping"),
					Output: "",
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSAddPortCmd(vfRep, genIfaceID(pod.Namespace, pod.Name), "", "", "a8d09931", string(pod.UID)),
				})
				// clearPodBandwidth
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "interface", "name",
						"external-ids:sandbox=a8d09931"),
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSFindCmd("30", "qos", "_uuid",
						"external-ids:sandbox=a8d09931"),
				})
				// getIfaceOFPort
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Interface", "pf0vf9", "ofport", ""),
					Output: "1",
				})
				// waitForPodFlows
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOVSGetCmd("Interface", "pf0vf9", "external-ids", "iface-id"),
					Output: genIfaceID(pod.Namespace, pod.Name),
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOfctlDumpFlowsCmd("table=9,dl_src="),
					Output: "non-empty-output",
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genOfctlDumpFlowsCmd("table=0,in_port=1"),
					Output: "non-empty-output",
				})
			})

			Context("Fails if link configuration fails on", func() {
				It("LinkByName()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
					// Mock ovs calls for cleanup
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
					execMock.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: genOVSDelPortCmd("pf0vf9"),
					})

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
				})

				It("LinkSetMTU()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(fmt.Errorf("failed to set mtu"))
					// Mock netlink/ovs calls for cleanup
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
					execMock.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: genOVSDelPortCmd("pf0vf9"),
					})

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
				})

				It("LinkSetUp()", func() {
					netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
					netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
					netlinkOpsMock.On("LinkSetUp", vfLink).Return(fmt.Errorf("failed to set link up"))
					// Mock netlink/ovs calls for cleanup
					checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
					netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
					execMock.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: genOVSDelPortCmd("pf0vf9"),
					})

					podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

					err := dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
					Expect(err).To(HaveOccurred())
					Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
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
				kubeMock.On("UpdatePodStatus", cpod).Return(nil)

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
				// Mock netlink/ovs calls for cleanup
				checkOVSPortPodInfo(execMock, vfRep, true, "15", "a8d09931", "default")
				netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genOVSDelPortCmd("pf0vf9"),
				})

				factoryMock.On("PodCoreInformer").Return(&podInformer)
				podInformer.On("Lister").Return(&podLister)
				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("UpdatePodStatus", cpod).Return(fmt.Errorf("failed to set pod annotations"))

				err = dnnc.addRepPort(&pod, &scd, ifInfo, clientset)
				Expect(err).To(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})
		})
	})

	Context("delRepPort", func() {
		var vfRep string
		var vfLink *linkMock.Link
		var scd util.DPUConnectionDetails

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfLink = &linkMock.Link{}
			scd = util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "9",
				SandboxId: "a8d09931",
			}
		})

		It("Sets link down for VF representor and removes VF representor from OVS", func() {
			checkOVSPortPodInfo(execMock, vfRep, true, "15", scd.SandboxId, types.DefaultNetworkName)
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists del-port br-int %s", "pf0vf9"),
			})
			err := dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Does not fail if LinkByName failed", func() {
			checkOVSPortPodInfo(execMock, vfRep, true, "15", scd.SandboxId, types.DefaultNetworkName)
			netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSDelPortCmd("pf0vf9"),
			})
			err := dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Does not fail if removal of VF representor from OVS fails once", func() {
			checkOVSPortPodInfo(execMock, vfRep, true, "15", scd.SandboxId, types.DefaultNetworkName)
			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			// fail on first try
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSDelPortCmd("pf0vf9"),
				Err: fmt.Errorf("ovs command failed"),
			})
			// pass on the second
			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: genOVSDelPortCmd("pf0vf9"),
				Err: nil,
			})
			// pass on the second
			err := dnnc.delRepPort(&pod, &scd, vfRep, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})
	})
})
