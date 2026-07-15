// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	linkMock "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// failOnceClient wraps a libovsdb Client and makes the first Transact call
// return an error, then unblocks. All other methods are forwarded unchanged.
// Used to exercise retry loops without depending on real network/timing
// behavior from the test harness.
type failOnceClient struct {
	libovsdbclient.Client
	fired atomic.Bool
}

func (f *failOnceClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	if f.fired.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("injected transient ovsdb failure")
	}
	return f.Client.Transact(ctx, ops...)
}

func emulateVSwitchdConfig(ovsClient libovsdbclient.Client) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ovs, err := libovsdbops.GetOpenvSwitch(ovsClient)
				if err != nil || ovs.CurCfg >= ovs.NextCfg {
					continue
				}
				updated := &vswitchd.OpenvSwitch{UUID: ovs.UUID, CurCfg: ovs.NextCfg}
				ops, err := ovsClient.Where(&vswitchd.OpenvSwitch{UUID: ovs.UUID}).Update(updated, &updated.CurCfg)
				if err == nil {
					_, _ = libovsdbops.TransactAndCheck(ovsClient, ops)
				}
			}
		}
	}()
	return func() { close(stop) }
}

type ovnInstalledClient struct {
	libovsdbclient.Client
	ifaceName string
}

func (c *ovnInstalledClient) Where(models ...model.Model) libovsdbclient.ConditionalAPI {
	return &ovnInstalledConditional{
		ConditionalAPI: c.Client.Where(models...),
		ifaceName:      c.ifaceName,
	}
}

type ovnInstalledConditional struct {
	libovsdbclient.ConditionalAPI
	ifaceName string
}

func (c *ovnInstalledConditional) List(ctx context.Context, result any) error {
	err := c.ConditionalAPI.List(ctx, result)
	if err != nil {
		return err
	}
	markOVNInstalled(result, c.ifaceName)
	return nil
}

func markOVNInstalled(result any, ifaceName string) {
	switch ifaces := result.(type) {
	case *[]*vswitchd.Interface:
		for _, iface := range *ifaces {
			markInterfaceOVNInstalled(iface, ifaceName)
		}
	case *[]vswitchd.Interface:
		for i := range *ifaces {
			markInterfaceOVNInstalled(&(*ifaces)[i], ifaceName)
		}
	}
}

func markInterfaceOVNInstalled(iface *vswitchd.Interface, ifaceName string) {
	if iface == nil || iface.Name != ifaceName {
		return
	}
	if iface.ExternalIDs == nil {
		iface.ExternalIDs = map[string]string{}
	}
	iface.ExternalIDs["ovn-installed"] = "true"
}

func genIfaceID(podNamespace, podName string) string {
	return fmt.Sprintf("%s_%s", podNamespace, podName)
}

func newFakeKubeClientWithPod(pod *corev1.Pod) *fake.Clientset {
	return fake.NewSimpleClientset(&corev1.PodList{Items: []corev1.Pod{*pod}})
}

var _ = Describe("Node DPU tests", func() {
	var sriovnetOpsMock utilMocks.SriovnetOps
	var netlinkOpsMock utilMocks.NetLinkOps
	var execMock *ovntest.FakeExec
	var kubeMock kubemocks.Interface
	var pod corev1.Pod
	var ctrl *Controller
	var podLister v1mocks.PodLister
	var podNamespaceLister v1mocks.PodNamespaceLister
	var clientset *cni.ClientSet

	origSriovnetOps := util.GetSriovnetOps()
	origNetlinkOps := util.GetNetLinkOps()

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
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

		podNamespaceLister = v1mocks.PodNamespaceLister{}
		podLister = v1mocks.PodLister{}
		podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)

		ctrl = &Controller{
			kube:      &kubeMock,
			podLister: &podLister,
			podStates: syncmap.NewSyncMap[*podDPUState](),
		}

		pod = corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:        "a-pod",
			Namespace:   "foo-ns",
			UID:         "a-pod",
			Annotations: map[string]string{},
		}}
	})

	AfterEach(func() {
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
		var state *dpuConnectionState

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
				NADKey:        types.DefaultNetworkName,
				PodUID:        "a-pod",
			}

			fakeClient := newFakeKubeClientWithPod(&pod)
			clientset = cni.NewClientSet(fakeClient, &podLister)
			scd := util.DPUConnectionDetails{
				PfId:      "0",
				VfId:      "9",
				SandboxId: "a8d09931",
			}
			state = &dpuConnectionState{
				vfRepName: vfRep,
				sandboxId: scd.SandboxId,
			}
			podAnnot, err := util.MarshalPodDPUConnDetails(nil, &scd, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			pod.Annotations = podAnnot
		})

		It("Fails if GetPCIFromDeviceName fails", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return("", fmt.Errorf("could not find PCI Address"))
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err := ctrl.addRepPort(&pod, state, ifInfo, clientset, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("could not find PCI Address"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, nil)
			defer cleanup()
			ctrl.ovsClient = &failOnceClient{Client: ctrl.ovsClient}
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err := ctrl.addRepPort(&pod, state, ifInfo, clientset, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("injected transient ovsdb failure"))
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails if configure OVS fails but OVS interface is added", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)

			// Seed the harness with a pre-existing OVS port for vfRep owned by
			// a different iface-id: cni.ConfigureOVS (libovsdb path) will fail
			// at the iface-id-conflict check, and the cleanup path runs
			// delRepPort which deletes the real port from the harness.
			ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
					&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int", Ports: []string{"vfrep-port-uuid"}},
					&vswitchd.Port{UUID: "vfrep-port-uuid", Name: vfRep, Interfaces: []string{"vfrep-iface-uuid"}},
					&vswitchd.Interface{
						UUID:        "vfrep-iface-uuid",
						Name:        vfRep,
						ExternalIDs: map[string]string{"iface-id": "someone-else", "sandbox": "a8d09931"},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			defer ovsCleanup.Cleanup()
			ctrl.ovsClient = ovsClient

			// Cleanup reads interface ownership from the OVSDB harness.

			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err = ctrl.addRepPort(&pod, state, ifInfo, clientset, false)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("was added for iface-id"))
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, vfRep)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Keeps the representor port when revalidating", func() {
			sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)

			ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
					&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int", Ports: []string{"vfrep-port-uuid"}},
					&vswitchd.Port{UUID: "vfrep-port-uuid", Name: vfRep, Interfaces: []string{"vfrep-iface-uuid"}},
					&vswitchd.Interface{
						UUID:        "vfrep-iface-uuid",
						Name:        vfRep,
						ExternalIDs: map[string]string{"iface-id": "someone-else", "sandbox": "a8d09931"},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			defer ovsCleanup.Cleanup()
			ctrl.ovsClient = ovsClient

			podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)

			err = ctrl.addRepPort(&pod, state, ifInfo, clientset, true)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("was added for iface-id"))
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, vfRep)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		Context("After successfully calling ConfigureOVS", func() {
			var ovsCleanup *libovsdbtest.Context

			BeforeEach(func() {
				sriovnetOpsMock.On("GetPCIFromDeviceName", vfRep).Return(vfPciAddress, nil)

				// Seed an OVSDB harness with an empty br-int so cni.ConfigureOVS
				// takes the libovsdb path: GetBridge succeeds, no stale ports
				// match, and CreateOrUpdatePodPort writes the new port. The
				// cleanup path's DeletePortWithInterfaces then finds and deletes
				// that real port.
				ovsClient, ctx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
					OVSData: []libovsdbtest.TestData{
						&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
						&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int"},
					},
				})
				Expect(err).NotTo(HaveOccurred())
				ovsCleanup = ctx
				DeferCleanup(emulateVSwitchdConfig(ovsClient))
				ctrl.ovsClient = &ovnInstalledClient{
					Client:    ovsClient,
					ifaceName: vfRep,
				}

				// ovnInstalledClient marks waitForPodInterface's libovsdb lookup as installed.
				netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
				netlinkOpsMock.On("LinkSetMTU", vfLink, ifInfo.MTU).Return(nil)
				netlinkOpsMock.On("LinkSetUp", vfLink).Return(nil)
			})

			AfterEach(func() {
				if ovsCleanup != nil {
					ovsCleanup.Cleanup()
					ovsCleanup = nil
				}
			})

			It("Sets dpu.connection-status pod annotation on success", func() {
				var err error
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: &dcs})
				Expect(err).ToNot(HaveOccurred())

				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("PatchPodStatusAnnotations", &pod, cpod).Return(nil)

				err = ctrl.addRepPort(&pod, state, ifInfo, clientset, false)
				Expect(err).ToNot(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})

			It("cleans up representor port if set pod annotation fails", func() {
				var err error
				dcs := util.DPUConnectionStatus{
					Status: "Ready",
				}
				cpod := pod.DeepCopy()
				cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: &dcs})
				Expect(err).ToNot(HaveOccurred())

				netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

				podLister.On("Pods", mock.AnythingOfType("string")).Return(&podNamespaceLister)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(&pod, nil)
				kubeMock.On("PatchPodStatusAnnotations", &pod, cpod).Return(fmt.Errorf("failed to set pod annotations"))

				err = ctrl.addRepPort(&pod, state, ifInfo, clientset, false)
				Expect(err).To(HaveOccurred())
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			})
		})
	})

	Context("delRepPort", func() {
		var vfRep string
		var vfLink *linkMock.Link
		var state *dpuConnectionState

		BeforeEach(func() {
			vfRep = "pf0vf9"
			vfLink = &linkMock.Link{}
			state = &dpuConnectionState{
				vfRepName: vfRep,
				sandboxId: "a8d09931",
			}
		})

		It("Fails when the OVS client is missing", func() {
			ctrl.ovsClient = nil
			err := ctrl.delRepPort(&pod, state, types.DefaultNetworkName)
			Expect(err).To(MatchError(ContainSubstring("without an OVS client")))
		})

		It("Sets link down for VF representor and removes VF representor from OVS", func() {

			netlinkOpsMock.On("LinkByName", vfRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)
			ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
					&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int", Ports: []string{"vfrep-port-uuid"}},
					&vswitchd.Port{UUID: "vfrep-port-uuid", Name: "pf0vf9", Interfaces: []string{"vfrep-iface-uuid"}},
					&vswitchd.Interface{UUID: "vfrep-iface-uuid", Name: "pf0vf9", ExternalIDs: map[string]string{"sandbox": state.sandboxId}},
				},
			})
			Expect(err).ToNot(HaveOccurred())
			defer ovsCleanup.Cleanup()
			ctrl.ovsClient = ovsClient
			err = ctrl.delRepPort(&pod, state, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			expectOVSPortAndInterfaceAbsent(ovsClient, vfRep)
		})

		It("Does not fail if LinkByName failed", func() {

			netlinkOpsMock.On("LinkByName", vfRep).Return(nil, fmt.Errorf("failed to get link"))
			ovsClient, ovsCleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
					&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int", Ports: []string{"vfrep-port-uuid"}},
					&vswitchd.Port{UUID: "vfrep-port-uuid", Name: "pf0vf9", Interfaces: []string{"vfrep-iface-uuid"}},
					&vswitchd.Interface{UUID: "vfrep-iface-uuid", Name: "pf0vf9", ExternalIDs: map[string]string{"sandbox": state.sandboxId}},
				},
			})
			Expect(err).ToNot(HaveOccurred())
			defer ovsCleanup.Cleanup()
			ctrl.ovsClient = ovsClient
			err = ctrl.delRepPort(&pod, state, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
			expectOVSPortAndInterfaceAbsent(ovsClient, vfRep)
		})
	})

	Context("bootstrapDPUPodMapFromOVS", func() {
		It("No-ops when no representor interfaces exist", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9"},
			})
			defer cleanup()

			podLister.On("List", mock.Anything).Return([]*corev1.Pod{}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			Expect(ctrl.podStates.GetKeys()).To(BeEmpty())
		})

		It("Populates state for an existing pod on default network", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_a-pod", ifaceIDVer: "uid-1"},
			})
			defer cleanup()

			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-1",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{existingPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-1")))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			Expect(ps.nadStates[types.DefaultNetworkName].vfRepName).To(Equal("pf0vf9"))
			Expect(ps.nadStates[types.DefaultNetworkName].sandboxId).To(Equal("sb1"))
		})

		It("Populates state for an existing pod on a UDN", func() {
			udnNADKey := "ns1/nad1"
			udnPrefix := util.GetUserDefinedNetworkPrefix(udnNADKey)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10",
					ifaceID: udnPrefix + "foo-ns_a-pod", ifaceIDVer: "uid-2",
					nadKey: udnNADKey,
				},
			})
			defer cleanup()

			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-2",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{existingPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-2")))
			Expect(ps.nadStates).To(HaveKey(udnNADKey))
			Expect(ps.nadStates[udnNADKey].vfRepName).To(Equal("pf0vf10"))
			Expect(ps.nadStates[udnNADKey].sandboxId).To(Equal("sb2"))
		})

		It("Revalidates a representor the pod does not report ready", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_a-pod", ifaceIDVer: "uid-1"},
			})
			defer cleanup()

			// Nothing reported the NAD ready, so the configuration may have
			// been interrupted part way.
			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-1",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{existingPod}, nil)

			Expect(ctrl.bootstrapDPUPodMapFromOVS()).To(Succeed())

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates[types.DefaultNetworkName].revalidate).To(BeTrue())
		})

		It("Trusts a representor the pod already reports ready", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_a-pod", ifaceIDVer: "uid-1"},
			})
			defer cleanup()

			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-1",
			}}
			var err error
			existingPod.Annotations, err = util.MarshalPodDPUConnStatus(nil,
				map[string]*util.DPUConnectionStatus{
					types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady},
				})
			Expect(err).ToNot(HaveOccurred())
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{existingPod}, nil)

			Expect(ctrl.bootstrapDPUPodMapFromOVS()).To(Succeed())

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates[types.DefaultNetworkName].revalidate).To(BeFalse())
		})

		It("Trusts a UDN representor the pod already reports ready", func() {
			udnNADKey := "ns1/nad1"
			udnPrefix := util.GetUserDefinedNetworkPrefix(udnNADKey)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10",
					ifaceID: udnPrefix + "foo-ns_a-pod", ifaceIDVer: "uid-2",
					nadKey: udnNADKey,
				},
			})
			defer cleanup()

			// The status and the OVS external id are keyed the same way, so a
			// UDN NAD is matched as well as the default network.
			existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "a-pod", Namespace: "foo-ns", UID: "uid-2",
			}}
			var err error
			existingPod.Annotations, err = util.MarshalPodDPUConnStatus(nil,
				map[string]*util.DPUConnectionStatus{
					udnNADKey: {Status: util.DPUConnectionStatusReady},
				})
			Expect(err).ToNot(HaveOccurred())
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{existingPod}, nil)

			Expect(ctrl.bootstrapDPUPodMapFromOVS()).To(Succeed())

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates[udnNADKey].revalidate).To(BeFalse())
		})

		It("Deletes orphaned representor port when pod no longer exists", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb-old", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_gone-pod", ifaceIDVer: "uid-gone"},
			})
			defer cleanup()

			podLister.On("List", mock.Anything).Return([]*corev1.Pod{}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf9").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			Expect(ctrl.podStates.GetKeys()).To(BeEmpty())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, "pf0vf9")
		})

		It("Handles mix of existing and orphaned ports", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_alive-pod", ifaceIDVer: "uid-alive"},
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_dead-pod", ifaceIDVer: "uid-dead"},
			})
			defer cleanup()

			alivePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "alive-pod", Namespace: "foo-ns", UID: "uid-alive",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{alivePod}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf10").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			ps, ok := ctrl.podStates.Load("foo-ns/alive-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-alive")))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))

			_, ok = ctrl.podStates.Load("foo-ns/dead-pod")
			Expect(ok).To(BeFalse())

			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, "pf0vf10")
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, "pf0vf9")
		})

		It("Populates multi-NAD state for the same pod with indexed NAD key", func() {
			// Use an indexed NAD key ("ns1/nad1/1") to verify that the full
			// indexed key is stored in the map and podKeyFromIfaceID correctly
			// strips the prefix derived from it.
			indexedNADKey := "ns1/nad1/1"
			udnPrefix := util.GetUserDefinedNetworkPrefix(indexedNADKey)
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_multi-pod", ifaceIDVer: "uid-multi"},
				{
					name: "pf0vf10", sandbox: "sb1", vfNetdevName: "pf0vf10",
					ifaceID: udnPrefix + "foo-ns_multi-pod", ifaceIDVer: "uid-multi",
					nadKey: indexedNADKey,
				},
			})
			defer cleanup()

			multiPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "multi-pod", Namespace: "foo-ns", UID: "uid-multi",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{multiPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			ps, ok := ctrl.podStates.Load("foo-ns/multi-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-multi")))
			Expect(ps.nadStates).To(HaveLen(2))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			Expect(ps.nadStates[types.DefaultNetworkName].vfRepName).To(Equal("pf0vf9"))
			Expect(ps.nadStates).To(HaveKey(indexedNADKey))
			Expect(ps.nadStates[indexedNADKey].vfRepName).To(Equal("pf0vf10"))
		})

		It("Skips interfaces with incomplete external IDs", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceIDVer: "uid-1"},
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_ok-pod", ifaceIDVer: "uid-ok"},
			})
			defer cleanup()

			okPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "ok-pod", Namespace: "foo-ns", UID: "uid-ok",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{okPod}, nil)

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			// The interface without an iface-id is skipped entirely, so the
			// only tracked pod is the one with complete external IDs.
			Expect(ctrl.podStates.GetKeys()).To(ConsistOf("foo-ns/ok-pod"))

			ps, ok := ctrl.podStates.Load("foo-ns/ok-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-ok")))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
		})

		It("Populates existing pod state even when orphan cleanup runs", func() {
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{name: "pf0vf9", sandbox: "sb1", vfNetdevName: "pf0vf9", ifaceID: "foo-ns_orphan", ifaceIDVer: "uid-orphan"},
				{name: "pf0vf10", sandbox: "sb2", vfNetdevName: "pf0vf10", ifaceID: "foo-ns_alive-pod", ifaceIDVer: "uid-alive"},
			})
			defer cleanup()

			alivePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "alive-pod", Namespace: "foo-ns", UID: "uid-alive",
			}}
			podLister.On("List", mock.Anything).Return([]*corev1.Pod{alivePod}, nil)
			netlinkOpsMock.On("LinkByName", "pf0vf9").Return(nil, fmt.Errorf("no link"))

			err := ctrl.bootstrapDPUPodMapFromOVS()
			Expect(err).ToNot(HaveOccurred())

			ps, ok := ctrl.podStates.Load("foo-ns/alive-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.uid).To(Equal(k8stypes.UID("uid-alive")))
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))

			_, ok = ctrl.podStates.Load("foo-ns/orphan")
			Expect(ok).To(BeFalse())

			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, "pf0vf9")
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, "pf0vf10")
		})
	})

	Context("reconcileDPUPod", func() {
		It("Cleans up the state of a pod recreated with the same name", func() {
			staleRep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: staleRep, sandbox: "sb-old", vfNetdevName: staleRep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-old",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-old"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: staleRep, sandboxId: "sb-old"},
				},
			})

			// Same name, different UID, and no connection details annotation yet.
			recreated := pod.DeepCopy()
			recreated.UID = k8stypes.UID("uid-new")
			recreated.Spec.NodeName = ctrl.nodeName
			podNamespaceLister.On("Get", "a-pod").Return(recreated, nil)

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", staleRep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, staleRep)
			// The status belongs to the pod that is gone, not to the new one.
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Reclaims the state of a pod that came back on another node", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			// The informer is cluster-wide, so the same name can come back on
			// another node. Its representor is that node's DPU to plumb, and
			// its annotations are that DPU's to write.
			otherPod := pod.DeepCopy()
			otherPod.UID = k8stypes.UID("uid-2")
			otherPod.Spec.NodeName = "other-host"
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb2"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot, map[string]*util.DPUConnectionStatus{
				types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady},
			})
			Expect(err).ToNot(HaveOccurred())
			otherPod.Annotations = annot

			// Armed so that acting on this pod, which must not happen, fails on
			// an assertion rather than on an unexpected mock call.
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)
			podNamespaceLister.On("Get", "a-pod").Return(otherPod, nil)
			kubeMock.On("PatchPodStatusAnnotations", mock.Anything, mock.Anything).Return(nil)

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, rep)
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Reclaims the state of a host-network pod instead of configuring it", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			// A host-network pod has no representor, so anything tracked under
			// its name is reclaimed. The connection details it carries here are
			// what the check defends against: the host CNI never writes them
			// for a host-network pod, and the sandbox they name was never
			// plumbed.
			hostNetPod := pod.DeepCopy()
			hostNetPod.UID = k8stypes.UID("uid-1")
			hostNetPod.Spec.NodeName = ctrl.nodeName
			hostNetPod.Spec.HostNetwork = true
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb2"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			hostNetPod.Annotations = annot
			podNamespaceLister.On("Get", "a-pod").Return(hostNetPod, nil)

			// Armed so that configuring this pod, which must not happen, fails
			// on an assertion rather than on an unexpected mock call.
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, rep)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Clears a leftover connection status when nothing is tracked", func() {
			ctrl.nodeName = "dpu-host"

			// No representor was found to bootstrap from, so the pod is not in
			// podStates at all, yet it still advertises a NAD as ready.
			annot, err := util.MarshalPodDPUConnStatus(nil, map[string]*util.DPUConnectionStatus{
				types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady},
			})
			Expect(err).ToNot(HaveOccurred())

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Annotations = annot
			livePod.Spec.NodeName = ctrl.nodeName

			clearedPod := livePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, clearedPod).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, clearedPod)
		})

		It("Re-adds the connection status of an already configured NAD", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			// The representor is tracked and still matches what the pod asks
			// for, so nothing is added or deleted, but the status it should
			// carry is missing.
			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)

			readyPod := livePod.DeepCopy()
			readyPod.Annotations, err = util.MarshalPodDPUConnStatus(readyPod.Annotations,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady}})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, readyPod).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, readyPod)
			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			// The default network has no NAD to be requeued for.
			Expect(ctrl.nadPods.pods(types.DefaultNetworkName)).To(BeEmpty())
		})

		It("Configures again a representor recovered from OVS", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1", revalidate: true},
				},
			})

			// The pod is already reported ready, and the representor is there,
			// but it was recovered from OVS rather than configured by us. The
			// pod has no OVN annotation, so the configuration fails and the
			// attempt is what this asserts.
			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady}})
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(MatchError(
				ContainSubstring("failed to get pod interface information")))

			// The status the pod carries is left alone, and so is the port.
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, rep)

			// The port is still there, so it stays tracked for the retry.
			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			Expect(ps.nadStates[types.DefaultNetworkName].revalidate).To(BeTrue())
		})

		It("Does not report a recovered representor ready before configuring it", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1", revalidate: true},
				},
			})

			// A restart between the OVS port add and the status update leaves
			// the port without a status, so there is nothing to preserve and
			// nothing to claim either.
			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)
			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(MatchError(
				ContainSubstring("failed to get pod interface information")))

			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, rep)
		})

		It("Tears down a representor whose revalidation never succeeded", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1", revalidate: true},
				},
			})

			readyStatus := map[string]*util.DPUConnectionStatus{
				types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady},
			}
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot, readyStatus)
			Expect(err).ToNot(HaveOccurred())

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			livePod.Annotations = annot
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)
			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil).Once()

			// The revalidation fails, leaving the port in place.
			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(MatchError(
				ContainSubstring("failed to get pod interface information")))
			expectOVSPortAndInterfacePresent(ctrl.ovsClient, rep)

			// The host then takes the pod's connection details away. The
			// representor has to go even though no reconcile ever configured it.
			detailsGonePod := livePod.DeepCopy()
			detailsGonePod.Annotations, err = util.MarshalPodDPUConnStatus(nil, readyStatus)
			Expect(err).ToNot(HaveOccurred())
			clearedPod := detailsGonePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(detailsGonePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", detailsGonePod, clearedPod).Return(nil)

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, rep)
			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Fails the pod on a malformed NAD key instead of unconfiguring it", func() {
			ctrl.nodeName = "dpu-host"
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: "pf0vf9", sandboxId: "sb1"},
				},
			})

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, "ns1/nad1/not-a-number")
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(
				MatchError(ContainSubstring("malformed index for NAD key")))

			// The representor already configured for this pod is left alone.
			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates).To(HaveKey(types.DefaultNetworkName))
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
		})

		It("Untracks and re-adds a NAD whose representor is gone from OVS", func() {
			rep := "pf0vf9"
			// br-int has no representor, but the state still tracks one.
			cleanup := setupOVSHarnessWithInterfaces(ctrl, nil)
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)

			// The NAD is untracked so that it is added again, and the add is
			// what fails here: the pod has no OVN annotation to configure from.
			err = ctrl.reconcileDPUPod("foo-ns/a-pod")
			Expect(err).To(MatchError(ContainSubstring("failed to get pod interface information")))

			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates).ToNot(HaveKey(types.DefaultNetworkName))
		})

		It("Keeps the representor of the old sandbox when its status cannot be cleared", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			// The pod moved to a new sandbox, so the representor configured for
			// the old one has to go, and the status reporting it with it.
			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Spec.NodeName = ctrl.nodeName
			annot, err := util.MarshalPodDPUConnDetails(nil,
				&util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb2"}, types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady}})
			Expect(err).ToNot(HaveOccurred())
			livePod.Annotations = annot
			sriovnetOpsMock.On("GetVfRepresentorDPU", "0", "9").Return(rep, nil)

			clearedPod := livePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, clearedPod).Return(fmt.Errorf("API is down"))

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			// The host side waits on this status to plumb the new sandbox, so
			// the old representor cannot be deleted while it still reads ready.
			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).ToNot(Succeed())

			expectOVSPortAndInterfacePresent(ctrl.ovsClient, rep)
			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates[types.DefaultNetworkName].sandboxId).To(Equal("sb1"))
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkSetDown", vfLink)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Clears the connection status when the sandbox is gone but the pod remains", func() {
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1",
				},
			})
			defer cleanup()

			ctrl.nodeName = "dpu-host"

			// The connection details annotation went away with the old sandbox,
			// but the pod itself is still running and still carries the status.
			annot, err := util.MarshalPodDPUConnStatus(nil, map[string]*util.DPUConnectionStatus{
				types.DefaultNetworkName: {Status: util.DPUConnectionStatusReady},
			})
			Expect(err).ToNot(HaveOccurred())

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Annotations = annot
			livePod.Spec.NodeName = ctrl.nodeName

			clearedPod := livePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{types.DefaultNetworkName: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, clearedPod).Return(nil)

			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					types.DefaultNetworkName: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, rep)
			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, clearedPod)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Tears down the representor when the pod's only NAD is deleted", func() {
			nadKey := "ns1/nad1"
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1", nadKey: nadKey,
				},
			})
			defer cleanup()

			// The NAD is gone from networkMgr, but the pod still requests it
			// through its connection details annotation.
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{NADNetworks: map[string]util.NetInfo{}}
			ctrl.nodeName = "dpu-host"

			scd := &util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}
			annot, err := util.MarshalPodDPUConnDetails(nil, scd, nadKey)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot, map[string]*util.DPUConnectionStatus{
				nadKey: {Status: util.DPUConnectionStatusReady},
			})
			Expect(err).ToNot(HaveOccurred())

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Annotations = annot
			livePod.Spec.NodeName = ctrl.nodeName

			// The pod outlives its NAD, so its connection status has to be
			// cleared along with the representor.
			clearedPod := livePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{nadKey: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, clearedPod).Return(nil)

			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					nadKey: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			_, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeFalse())
			expectOVSPortAndInterfaceAbsent(ctrl.ovsClient, rep)
			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, clearedPod)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})

		It("Keeps the representor of a deleted NAD when its status cannot be cleared", func() {
			nadKey := "ns1/nad1"
			rep := "pf0vf9"
			cleanup := setupOVSHarnessWithInterfaces(ctrl, []ovsInterfaceData{
				{
					name: rep, sandbox: "sb1", vfNetdevName: rep,
					ifaceID: genIfaceID("foo-ns", "a-pod"), ifaceIDVer: "uid-1", nadKey: nadKey,
				},
			})
			defer cleanup()

			ctrl.networkMgr = &networkmanager.FakeNetworkManager{NADNetworks: map[string]util.NetInfo{}}
			ctrl.nodeName = "dpu-host"

			scd := &util.DPUConnectionDetails{PfId: "0", VfId: "9", SandboxId: "sb1"}
			annot, err := util.MarshalPodDPUConnDetails(nil, scd, nadKey)
			Expect(err).ToNot(HaveOccurred())
			annot, err = util.MarshalPodDPUConnStatus(annot, map[string]*util.DPUConnectionStatus{
				nadKey: {Status: util.DPUConnectionStatusReady},
			})
			Expect(err).ToNot(HaveOccurred())

			livePod := pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
			livePod.Annotations = annot
			livePod.Spec.NodeName = ctrl.nodeName

			clearedPod := livePod.DeepCopy()
			clearedPod.Annotations, err = util.MarshalPodDPUConnStatus(clearedPod.Annotations,
				map[string]*util.DPUConnectionStatus{nadKey: nil})
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", "a-pod").Return(livePod, nil)
			kubeMock.On("PatchPodStatusAnnotations", livePod, clearedPod).Return(fmt.Errorf("API is down"))

			ctrl.podStates.Store("foo-ns/a-pod", &podDPUState{
				uid: k8stypes.UID("uid-1"),
				nadStates: map[string]*dpuConnectionState{
					nadKey: {vfRepName: rep, sandboxId: "sb1"},
				},
			})

			vfLink := &linkMock.Link{}
			netlinkOpsMock.On("LinkByName", rep).Return(vfLink, nil)
			netlinkOpsMock.On("LinkSetDown", vfLink).Return(nil)

			// Deleting the port while the pod still advertises it as ready would
			// let the host side plumb a sandbox onto a representor that is gone,
			// so the teardown waits for the next retry instead.
			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).ToNot(Succeed())

			expectOVSPortAndInterfacePresent(ctrl.ovsClient, rep)
			ps, ok := ctrl.podStates.Load("foo-ns/a-pod")
			Expect(ok).To(BeTrue())
			Expect(ps.nadStates).To(HaveKey(nadKey))
			netlinkOpsMock.AssertNotCalled(GinkgoT(), "LinkSetDown", vfLink)
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc())
		})
	})

	Context("reconcileDPUConnStatus", func() {
		var livePod *corev1.Pod

		// expectStatusPatch arms the mocks for the single annotation update that
		// statusMap should produce, and returns the pod as it must end up.
		expectStatusPatch := func(from *corev1.Pod, statusMap map[string]*util.DPUConnectionStatus) *corev1.Pod {
			patched := from.DeepCopy()
			var err error
			patched.Annotations, err = util.MarshalPodDPUConnStatus(patched.Annotations, statusMap)
			Expect(err).ToNot(HaveOccurred())

			podNamespaceLister.On("Get", from.Name).Return(from, nil)
			kubeMock.On("PatchPodStatusAnnotations", from, patched).Return(nil)
			return patched
		}

		withStatus := func(p *corev1.Pod, nadKey string) {
			var err error
			p.Annotations, err = util.MarshalPodDPUConnStatus(p.Annotations,
				map[string]*util.DPUConnectionStatus{nadKey: {Status: util.DPUConnectionStatusReady}})
			Expect(err).ToNot(HaveOccurred())
		}

		BeforeEach(func() {
			livePod = pod.DeepCopy()
			livePod.UID = k8stypes.UID("uid-1")
		})

		It("Sets the status of a tracked NAD that is missing it", func() {
			patched := expectStatusPatch(livePod, map[string]*util.DPUConnectionStatus{
				"ns1/nad1": {Status: util.DPUConnectionStatusReady},
			})

			tracked := map[string]*dpuConnectionState{"ns1/nad1": {vfRepName: "pf0vf9", sandboxId: "sb1"}}
			Expect(ctrl.reconcileDPUConnStatus(livePod, "foo-ns/a-pod", tracked)).To(Succeed())
			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, patched)
		})

		It("Does not set the status of a NAD awaiting revalidation", func() {
			podNamespaceLister.On("Get", livePod.Name).Return(livePod, nil)

			tracked := map[string]*dpuConnectionState{
				"ns1/nad1": {vfRepName: "pf0vf9", sandboxId: "sb1", revalidate: true},
			}
			Expect(ctrl.reconcileDPUConnStatus(livePod, "foo-ns/a-pod", tracked)).To(Succeed())
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
		})

		It("Clears the status of a NAD that is no longer tracked", func() {
			withStatus(livePod, "ns1/nad1")
			patched := expectStatusPatch(livePod, map[string]*util.DPUConnectionStatus{"ns1/nad1": nil})

			Expect(ctrl.reconcileDPUConnStatus(livePod, "foo-ns/a-pod", nil)).To(Succeed())
			kubeMock.AssertCalled(GinkgoT(), "PatchPodStatusAnnotations", livePod, patched)
		})

		It("Does not update the pod when the status already matches", func() {
			withStatus(livePod, "ns1/nad1")

			tracked := map[string]*dpuConnectionState{"ns1/nad1": {vfRepName: "pf0vf9", sandboxId: "sb1"}}
			Expect(ctrl.reconcileDPUConnStatus(livePod, "foo-ns/a-pod", tracked)).To(Succeed())
			kubeMock.AssertNotCalled(GinkgoT(), "PatchPodStatusAnnotations", mock.Anything, mock.Anything)
		})
	})

	Context("nadPods", func() {
		// podWithNADs builds a pod on this node with DPU connection details for
		// each of the given NAD keys. None of those NADs exist, so reconciling
		// the pod does no more than index it.
		podWithNADs := func(name string, nadKeys ...string) *corev1.Pod {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "foo-ns"}}
			p.Spec.NodeName = "dpu-host"
			for i, nadKey := range nadKeys {
				scd := &util.DPUConnectionDetails{PfId: "0", VfId: fmt.Sprintf("%d", i), SandboxId: "sb-" + name}
				annot, err := util.MarshalPodDPUConnDetails(p.Annotations, scd, nadKey)
				Expect(err).ToNot(HaveOccurred())
				p.Annotations = annot
			}
			return p
		}

		BeforeEach(func() {
			ctrl.nodeName = "dpu-host"
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}
		})

		It("Indexes a pod by every NAD it asks for, under the base key", func() {
			p := podWithNADs("waiting", "ns1/nad1", "ns1/nad1/1", "ns1/nad2")

			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/waiting", p, nil)).To(Succeed())

			Expect(ctrl.nadPods.pods("ns1/nad1")).To(ConsistOf("foo-ns/waiting"))
			Expect(ctrl.nadPods.pods("ns1/nad2")).To(ConsistOf("foo-ns/waiting"))
		})

		It("Indexes only the pods that ask for the NAD", func() {
			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/wants-nad", podWithNADs("wants-nad", "ns1/nad1"), nil)).To(Succeed())
			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/other-nad", podWithNADs("other-nad", "ns1/nad2"), nil)).To(Succeed())
			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/no-nad", podWithNADs("no-nad"), nil)).To(Succeed())

			Expect(ctrl.nadPods.pods("ns1/nad1")).To(ConsistOf("foo-ns/wants-nad"))
		})

		It("Drops a NAD the pod no longer asks for", func() {
			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/a-pod", podWithNADs("a-pod", "ns1/nad1", "ns1/nad2"), nil)).To(Succeed())

			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/a-pod", podWithNADs("a-pod", "ns1/nad2"), nil)).To(Succeed())

			Expect(ctrl.nadPods.pods("ns1/nad1")).To(BeEmpty())
			Expect(ctrl.nadPods.pods("ns1/nad2")).To(ConsistOf("foo-ns/a-pod"))
		})

		It("Does not index a pod of another node", func() {
			p := podWithNADs("elsewhere", "ns1/nad1")
			p.Spec.NodeName = "other-host"
			podNamespaceLister.On("Get", "elsewhere").Return(p, nil)

			Expect(ctrl.reconcileDPUPod("foo-ns/elsewhere")).To(Succeed())

			Expect(ctrl.nadPods.pods("ns1/nad1")).To(BeEmpty())
		})

		It("Forgets a pod that is gone", func() {
			Expect(ctrl.handleAddOrUpdateDPUPod("foo-ns/a-pod", podWithNADs("a-pod", "ns1/nad1"), nil)).To(Succeed())
			podNamespaceLister.On("Get", "a-pod").Return(nil, apierrors.NewNotFound(corev1.Resource("pods"), "a-pod"))

			Expect(ctrl.reconcileDPUPod("foo-ns/a-pod")).To(Succeed())

			Expect(ctrl.nadPods.pods("ns1/nad1")).To(BeEmpty())
		})
	})

	Context("dpuPodNeedsUpdate", func() {
		var oldPod, newPod *corev1.Pod

		BeforeEach(func() {
			ctrl.nodeName = "dpu-host"
			oldPod = pod.DeepCopy()
			oldPod.Spec.NodeName = ctrl.nodeName
			oldPod.Annotations = map[string]string{
				util.DPUConnectionDetailsAnnot: `{"default":{"pfId":"0","vfId":"9","sandboxId":"sb1"}}`,
				types.OvnPodAnnotationName:     `{"default":{"mac_address":"0a:58:fd:98:00:01"}}`,
			}
			newPod = oldPod.DeepCopy()
		})

		It("Skips an update that changes nothing it acts on", func() {
			newPod.Labels = map[string]string{"new": "label"}
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeFalse())
		})

		It("Reconciles when the connection details change", func() {
			newPod.Annotations[util.DPUConnectionDetailsAnnot] = `{"default":{"pfId":"0","vfId":"9","sandboxId":"sb2"}}`
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeTrue())
		})

		It("Reconciles when the OVN annotation arrives", func() {
			delete(oldPod.Annotations, types.OvnPodAnnotationName)
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeTrue())
		})

		It("Reconciles when the pod is scheduled", func() {
			oldPod.Spec.NodeName = ""
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeTrue())
		})

		It("Skips an update to a pod of another node", func() {
			oldPod.Spec.NodeName = "other-host"
			newPod.Spec.NodeName = "other-host"
			newPod.Annotations[util.DPUConnectionDetailsAnnot] = `{"default":{"pfId":"0","vfId":"9","sandboxId":"sb2"}}`
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeFalse())
		})

		It("Reconciles when only one side of the update is on this node", func() {
			newPod.Spec.NodeName = "other-host"
			Expect(ctrl.dpuPodNeedsUpdate(oldPod, newPod)).To(BeTrue())
		})
	})

	Context("getNetInfoForNADKey", func() {
		var fakeNetMgr *networkmanager.FakeNetworkManager

		BeforeEach(func() {
			fakeNetMgr = &networkmanager.FakeNetworkManager{
				NADNetworks: map[string]util.NetInfo{
					"ns1/nad1": &util.DefaultNetInfo{},
				},
			}
			ctrl.networkMgr = fakeNetMgr
		})

		It("Returns NetInfo for default network", func() {
			ni, err := ctrl.getNetInfoForNADKey(types.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(ni).NotTo(BeNil())
		})

		It("Returns NetInfo for base NAD key", func() {
			ni, err := ctrl.getNetInfoForNADKey("ns1/nad1")
			Expect(err).ToNot(HaveOccurred())
			Expect(ni).NotTo(BeNil())
		})

		It("Returns NetInfo for indexed NAD key", func() {
			ni, err := ctrl.getNetInfoForNADKey("ns1/nad1/1")
			Expect(err).ToNot(HaveOccurred())
			Expect(ni).NotTo(BeNil())
		})

		It("Returns nil for unknown NAD key", func() {
			ni, err := ctrl.getNetInfoForNADKey("ns1/unknown")
			Expect(err).ToNot(HaveOccurred())
			Expect(ni).To(BeNil())
		})

		It("Returns an error for a malformed NAD key", func() {
			ni, err := ctrl.getNetInfoForNADKey("ns1/nad1/not-a-number")
			Expect(err).To(MatchError(ContainSubstring("malformed index for NAD key")))
			Expect(ni).To(BeNil())
		})
	})
})

// ovsInterfaceData holds the external IDs for setting up an OVS interface in tests.
type ovsInterfaceData struct {
	name         string
	sandbox      string
	vfNetdevName string
	ifaceID      string
	ifaceIDVer   string
	nadKey       string // empty = default network
}

// setupOVSHarnessWithInterfaces creates a fresh OVS test harness with the specified
// interfaces on br-int and updates ctrl.ovsClient. Returns a cleanup function.
func setupOVSHarnessWithInterfaces(ctrl *Controller, ifaces []ovsInterfaceData) func() {
	var portUUIDs []string
	var ovsData []libovsdbtest.TestData

	for _, iface := range ifaces {
		portUUID := fmt.Sprintf("port-%s-uuid", iface.name)
		intfUUID := fmt.Sprintf("intf-%s-uuid", iface.name)
		portUUIDs = append(portUUIDs, portUUID)

		extIDs := map[string]string{}
		if iface.sandbox != "" {
			extIDs["sandbox"] = iface.sandbox
		}
		if iface.vfNetdevName != "" {
			extIDs["vf-netdev-name"] = iface.vfNetdevName
		}
		if iface.ifaceID != "" {
			extIDs["iface-id"] = iface.ifaceID
		}
		if iface.ifaceIDVer != "" {
			extIDs["iface-id-ver"] = iface.ifaceIDVer
		}
		if iface.nadKey != "" {
			extIDs[types.NADExternalID] = iface.nadKey
		}

		ovsData = append(ovsData,
			&vswitchd.Port{UUID: portUUID, Name: iface.name, Interfaces: []string{intfUUID}},
			&vswitchd.Interface{UUID: intfUUID, Name: iface.name, ExternalIDs: extIDs},
		)
	}

	ovsData = append(ovsData,
		&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-br-int-uuid"}},
		&vswitchd.Bridge{UUID: "bridge-br-int-uuid", Name: "br-int", Ports: portUUIDs},
	)

	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: ovsData,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	ctrl.ovsClient = ovsClient
	return testCtx.Cleanup
}

func expectOVSPortAndInterfaceAbsent(ovsClient libovsdbclient.Client, name string) {
	GinkgoHelper()
	_, err := libovsdbops.GetOVSPort(ovsClient, name)
	Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	_, err = libovsdbops.GetOVSInterface(ovsClient, name)
	Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
}

func expectOVSPortAndInterfacePresent(ovsClient libovsdbclient.Client, name string) {
	GinkgoHelper()
	_, err := libovsdbops.GetOVSPort(ovsClient, name)
	Expect(err).NotTo(HaveOccurred())
	_, err = libovsdbops.GetOVSInterface(ovsClient, name)
	Expect(err).NotTo(HaveOccurred())
}
