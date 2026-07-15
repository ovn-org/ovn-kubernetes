// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package managementport

import (
	"errors"
	"fmt"
	"net"

	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	kubeMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func mockOVSListInterfaceMgmtPortNotExistCmd(execMock *ovntest.FakeExec, mgmtPortName string) {
	execMock.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 --no-headings --data bare --format csv --columns type,name find Interface name=" + mgmtPortName,
	})
}

func genGetOvsEntry(table, record, column, key string) string {
	if key != "" {
		column = column + ":" + key
	}
	return fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists get %s %s %s", table, record, column)
}

var _ = Describe("Mananagement port DPU tests", func() {
	origNetlinkOps := util.GetNetLinkOps()
	var netlinkOpsMock *utilMocks.NetLinkOps
	var execMock *ovntest.FakeExec
	var nodeAnnotatorMock *kubeMocks.Annotator
	var ovsClient libovsdbclient.Client
	var ovsCleanup *libovsdbtest.Context

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		util.ResetRunner()

		netlinkOpsMock = &utilMocks.NetLinkOps{}
		nodeAnnotatorMock = &kubeMocks.Annotator{}
		execMock = ovntest.NewFakeExec()
		err := util.SetExec(execMock)
		Expect(err).NotTo(HaveOccurred())
		util.SetNetLinkOpMockInst(netlinkOpsMock)
		nftables.SetFakeNFTablesHelper()

		ovsClient, ovsCleanup, err = libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
			OVSData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"br-int-uuid"}},
				&vswitchd.Bridge{UUID: "br-int-uuid", Name: "br-int"},
			},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		util.SetNetLinkOpMockInst(origNetlinkOps)
		ovsCleanup.Cleanup()
	})

	Context("Create Management port DPU", func() {
		It("Fails if representor link lookup failed with error", func() {
			mgmtPortDpu := managementPortRepresentor{
				repDevName: "non-existent-netdev",
			}

			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(nil, fmt.Errorf("netlink mock error"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(false)

			err := mgmtPortDpu.create()
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(err).To(HaveOccurred())
		})

		It("Fails if representor and ovn-k8s-mp0 netdev is not found", func() {
			mgmtPortDpu := managementPortRepresentor{
				repDevName: "non-existent-netdev",
			}
			netlinkOpsMock.On("LinkByName", "non-existent-netdev").Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(
				nil, fmt.Errorf("failed to get interface"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)

			err := mgmtPortDpu.create()
			Expect(err).To(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
		})

		It("Fails if set Name to ovn-k8s-mp0 fails", func() {
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", nil, ovsClient)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: "enp3s0f0v0", MTU: 1400})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName+"_0").Return(
				nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName+"_0").Return(fmt.Errorf("failed to set name"))
			err := mgmtPortDpu.create()
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(err).To(HaveOccurred())
		})

		It("Configures VF representor and connects it to OVS bridge", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "k8s-worker0",
					Annotations: map[string]string{
						"k8s.ovn.org/node-mgmt-port-mac-addresses": fmt.Sprintf(`{"default":"%s"}`, expectedMgmtPortMac.String()),
					},
				},
			}
			cfg := &managementPortConfig{
				nodeName:    "k8s-worker0",
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", cfg, ovsClient)
			nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{"default": expectedMgmtPortMac.String()}).Return(nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: "enp3s0f0v0", MTU: 1500})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName+"_0").Return(
				nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName+"_0").Return(nil)
			netlinkOpsMock.On("LinkSetAlias", linkMock, "enp3s0f0v0").Return(nil)
			netlinkOpsMock.On("LinkSetMTU", linkMock, config.Default.MTU).Return(nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)
			fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
				Items: []corev1.Node{*node},
			})
			fakeNodeClient := &util.OVNNodeClientset{
				KubeClient: fakeClient,
			}
			watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, node.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(watchFactory.Start()).To(Succeed())

			err = mgmtPortDpu.create()
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(mgmtPortDpu.link).To(Equal(linkMock))
		})

		It("Brings interface up and attemps to add ovn-k8s-mp0 to OVS if interface already configured", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "k8s-worker0",
					Annotations: map[string]string{
						"k8s.ovn.org/node-mgmt-port-mac-addresses": fmt.Sprintf(`{"default":"%s"}`, expectedMgmtPortMac.String()),
					},
				},
			}
			cfg := &managementPortConfig{
				nodeName:    "k8s-worker0",
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpu := newManagementPortRepresentor(types.K8sMgmtIntfName+"_0", "enp3s0f0v0", cfg, ovsClient)
			nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{"default": expectedMgmtPortMac.String()}).Return(nil)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{Name: types.K8sMgmtIntfName + "_0", MTU: config.Default.MTU})

			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(
				linkMock, nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)
			fakeClient := fake.NewSimpleClientset(&corev1.NodeList{
				Items: []corev1.Node{*node},
			})
			fakeNodeClient := &util.OVNNodeClientset{
				KubeClient: fakeClient,
			}
			watchFactory, err := factory.NewNodeWatchFactory(fakeNodeClient, node.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(watchFactory.Start()).To(Succeed())

			err = mgmtPortDpu.create()
			Expect(err).ToNot(HaveOccurred())
			Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			Expect(mgmtPortDpu.link).To(Equal(linkMock))
		})
	})

	Context("Create Management port DPU host", func() {
		const deviceID = "0000:03:00.2"
		var sriovnetOpsMock *utilMocks.SriovnetOps
		var vdpaOpsMock *utilMocks.VdpaOps
		var origSriovnetOps util.SriovnetOps
		var origVdpaOps util.VdpaOps

		BeforeEach(func() {
			origSriovnetOps = util.GetSriovnetOps()
			origVdpaOps = util.GetVdpaOps()
			sriovnetOpsMock = &utilMocks.SriovnetOps{}
			vdpaOpsMock = &utilMocks.VdpaOps{}
			util.SetSriovnetOpsInst(sriovnetOpsMock)
			util.SetVdpaOpsInst(vdpaOpsMock)
		})

		AfterEach(func() {
			util.SetSriovnetOpsInst(origSriovnetOps)
			util.SetVdpaOpsInst(origVdpaOps)
		})

		// mockDeviceIDToNetdev sets up mock expectations for findNetdevByDeviceID
		mockDeviceIDToNetdev := func(pciAddr, netdevName string) {
			vdpaOpsMock.On("GetVdpaDeviceByPci", pciAddr).Return(nil, fmt.Errorf("no vdpa device"))
			sriovnetOpsMock.On("GetNetDevicesFromPci", pciAddr).Return([]string{netdevName}, nil)
		}

		It("Fails with errMgmtPortDeviceNotFound when PCI device is gone", func() {
			mgmtPortDpuHost := &managementPortNetdev{
				deviceID: deviceID,
			}
			vdpaOpsMock.On("GetVdpaDeviceByPci", deviceID).Return(nil, fmt.Errorf("no vdpa device"))
			sriovnetOpsMock.On("GetNetDevicesFromPci", deviceID).Return(nil, fmt.Errorf("no device"))

			err := mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, errMgmtPortDeviceNotFound)).To(BeTrue())
		})

		It("Fails when deviceID is empty", func() {
			mgmtPortDpuHost := &managementPortNetdev{
				deviceID: "",
			}

			err := mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, errMgmtPortDeviceNotFound)).To(BeFalse(),
				"empty deviceID should not be confused with PCI device gone")
		})

		It("Resolves a renamed netdev through its link alias", func() {
			// A previous instance renamed the netdev to the management port
			// name, keeping the original name as the link alias. Name-based
			// resolution (simulated devices) must fall back to the alias.
			mgmtPortDpuHost := &managementPortNetdev{
				deviceID: deviceID,
			}
			mockDeviceIDToNetdev(deviceID, "eth0-2")
			renamedLink := &mocks.Link{}
			renamedLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "ovn-k8s-mp0", Alias: "eth0-2"})
			netlinkOpsMock.On("LinkByName", "eth0-2").Return(nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkList").Return([]netlink.Link{renamedLink}, nil)

			link, err := mgmtPortDpuHost.findNetdevByDeviceID()
			Expect(err).NotTo(HaveOccurred())
			Expect(link.Attrs().Name).To(Equal("ovn-k8s-mp0"))
		})

		It("Fails when the netdev is missing and no link carries its alias", func() {
			mgmtPortDpuHost := &managementPortNetdev{
				deviceID: deviceID,
			}
			mockDeviceIDToNetdev(deviceID, "eth0-2")
			otherLink := &mocks.Link{}
			otherLink.On("Attrs").Return(&netlink.LinkAttrs{Name: "ovn-k8s-mp0", Alias: "eth0-9"})
			netlinkOpsMock.On("LinkByName", "eth0-2").Return(nil, fmt.Errorf("link not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			netlinkOpsMock.On("LinkList").Return([]netlink.Link{otherLink}, nil)

			_, err := mgmtPortDpuHost.findNetdevByDeviceID()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("the link lookup failed"))
		})

		It("Fails with errMgmtPortDeviceNotFound when device ID resolves to empty netdev name", func() {
			mgmtPortDpuHost := &managementPortNetdev{
				deviceID: deviceID,
			}
			vdpaOpsMock.On("GetVdpaDeviceByPci", deviceID).Return(nil, fmt.Errorf("no vdpa device"))
			sriovnetOpsMock.On("GetNetDevicesFromPci", deviceID).Return([]string{""}, nil)

			err := mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, errMgmtPortDeviceNotFound)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("resolved to empty netdev name"))
		})

		It("Configures VF and calls createPlatformManagementPort", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			currentMgmtPortMac, err := net.ParseMAC("00:bb:cc:dd:ee:11")
			Expect(err).ToNot(HaveOccurred())
			config.Default.MTU = 1400
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpuHost := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: "enp3s0f0v0", MTU: 1500, HardwareAddr: currentMgmtPortMac})

			mockDeviceIDToNetdev(deviceID, "enp3s0f0v0")
			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(linkMock, nil)
			netlinkOpsMock.On("LinkSetDown", linkMock).Return(nil)
			netlinkOpsMock.On("LinkSetHardwareAddr", linkMock, expectedMgmtPortMac).Return(nil)
			netlinkOpsMock.On("LinkSetName", linkMock, types.K8sMgmtIntfName).Return(nil)
			netlinkOpsMock.On("LinkSetAlias", linkMock, "enp3s0f0v0").Return(nil)
			netlinkOpsMock.On("LinkSetMTU", linkMock, config.Default.MTU).Return(nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil, nil)
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			mockOVSListInterfaceMgmtPortNotExistCmd(execMock, types.K8sMgmtIntfName)

			// mock createPlatformManagementPort, we fail it as it covers what we want to test without the
			// need to mock the entire flow down to routes and nftables rules.
			netlinkOpsMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf(
				"createPlatformManagementPort error"))

			err = mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("createPlatformManagementPort error"))

			ovs, err := ovsops.GetOpenvSwitch(ovsClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(ovs.ExternalIDs).To(HaveKeyWithValue("ovn-orig-mgmt-port-netdev-name", "enp3s0f0v0"))
		})

		It("Does not configure VF if already configured as ovn-k8s-mp0", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPortDpuHost := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: types.K8sMgmtIntfName, MTU: 1400, HardwareAddr: expectedMgmtPortMac})

			mockDeviceIDToNetdev(deviceID, types.K8sMgmtIntfName)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(linkMock, nil).Once()
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)

			// mock createPlatformManagementPort, we fail it as it covers what we want to test without the
			// need to mock the entire flow down to routes and nftables rules.
			netlinkOpsMock.On("LinkByName", mock.Anything).Return(nil, fmt.Errorf(
				"createPlatformManagementPort error"))

			err = mgmtPortDpuHost.create()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("createPlatformManagementPort error"))
		})
	})

	Context("doReconcile Management port DPU host", func() {
		const deviceID = "0000:03:00.2"
		var sriovnetOpsMock *utilMocks.SriovnetOps
		var vdpaOpsMock *utilMocks.VdpaOps
		var origSriovnetOps util.SriovnetOps
		var origVdpaOps util.VdpaOps

		BeforeEach(func() {
			origSriovnetOps = util.GetSriovnetOps()
			origVdpaOps = util.GetVdpaOps()
			sriovnetOpsMock = &utilMocks.SriovnetOps{}
			vdpaOpsMock = &utilMocks.VdpaOps{}
			util.SetSriovnetOpsInst(sriovnetOpsMock)
			util.SetVdpaOpsInst(vdpaOpsMock)
		})

		AfterEach(func() {
			util.SetSriovnetOpsInst(origSriovnetOps)
			util.SetVdpaOpsInst(origVdpaOps)
		})

		mockDeviceIDToNetdev := func(pciAddr, netdevName string) {
			vdpaOpsMock.On("GetVdpaDeviceByPci", pciAddr).Return(nil, fmt.Errorf("no vdpa device"))
			sriovnetOpsMock.On("GetNetDevicesFromPci", pciAddr).Return([]string{netdevName}, nil)
		}

		It("Succeeds when createPlatformManagementPort succeeds (no recreation needed)", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			netInfoMock := &multinetworkmocks.NetInfo{}
			netInfoMock.On("IsPrimaryNetwork").Return(false)
			netInfoMock.On("Transport").Return("")
			netInfoMock.On("GetPodNetworkAdvertisedOnNodeVRFs", mock.Anything).Return(nil)
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
				netInfo:     netInfoMock,
			}
			Expect(SetupManagementPortNFTSets()).To(Succeed())
			mgmtPort := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: types.K8sMgmtIntfName, MTU: 1400, HardwareAddr: expectedMgmtPortMac,
				Index: 10})

			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(linkMock, nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)

			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
				Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
			})

			err = mgmtPort.doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		It("Recreates management port successfully when reconciliation fails but VF exists", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			expectedMgmtPortMac := util.IPAddrToHWAddr(util.GetNodeManagementIfAddr(ipnet).IP)
			config.Default.MTU = 1400
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			netInfoMock := &multinetworkmocks.NetInfo{}
			netInfoMock.On("IsPrimaryNetwork").Return(false)
			netInfoMock.On("Transport").Return("")
			netInfoMock.On("GetPodNetworkAdvertisedOnNodeVRFs", mock.Anything).Return(nil)
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
				netInfo:     netInfoMock,
			}
			Expect(SetupManagementPortNFTSets()).To(Succeed())
			mgmtPort := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)
			linkMock := &mocks.Link{}
			linkMock.On("Attrs").Return(&netlink.LinkAttrs{
				Name: types.K8sMgmtIntfName, MTU: 1400, HardwareAddr: expectedMgmtPortMac,
				Index: 10})

			// createPlatformManagementPort fails on first attempt
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(nil, fmt.Errorf("link gone")).Once()

			// create() succeeds: device ID resolves, VF already named ovn-k8s-mp0
			mockDeviceIDToNetdev(deviceID, types.K8sMgmtIntfName)
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(linkMock, nil)
			netlinkOpsMock.On("LinkSetUp", linkMock).Return(nil)

			execMock.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "sysctl -w net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
				Output: "net.ipv4.conf.ovn-k8s-mp0.forwarding = 1",
			})

			err = mgmtPort.doReconcile()
			Expect(err).NotTo(HaveOccurred())
		})

		It("Returns error on transient create() failure (VF exists but config fails)", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			config.Default.MTU = 1400
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPort := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)

			// createPlatformManagementPort fails
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(nil, fmt.Errorf("link gone")).Once()

			// create() finds the VF via device ID; VF has a different name so syncMgmtPortInterface runs
			mockDeviceIDToNetdev(deviceID, "enp3s0f0v0")
			linkMock2 := &mocks.Link{}
			linkMock2.On("Attrs").Return(&netlink.LinkAttrs{
				Name: "enp3s0f0v0", MTU: 1500})
			netlinkOpsMock.On("LinkByName", "enp3s0f0v0").Return(linkMock2, nil)
			// syncMgmtPortInterface calls LinkByName("ovn-k8s-mp0") → not found (already consumed .Once())
			// then unconfigureMgmtNetdevicePort calls LinkByName("ovn-k8s-mp0") and IsLinkNotFoundError
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(nil, fmt.Errorf("not found"))
			netlinkOpsMock.On("IsLinkNotFoundError", mock.Anything).Return(true)
			mockOVSListInterfaceMgmtPortNotExistCmd(execMock, types.K8sMgmtIntfName)
			// bringupManagementPortLink fails with transient error
			netlinkOpsMock.On("LinkSetDown", linkMock2).Return(fmt.Errorf("transient error"))

			err = mgmtPort.doReconcile()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to recreate management port"))
			Expect(errors.Is(err, errMgmtPortDeviceNotFound)).To(BeFalse())
		})

		It("Fatals when PCI device is gone during doReconcile", func() {
			_, ipnet, err := net.ParseCIDR("192.168.0.1/24")
			Expect(err).ToNot(HaveOccurred())
			config.Default.MTU = 1400
			cfg := &managementPortConfig{
				hostSubnets: []*net.IPNet{ipnet},
			}
			mgmtPort := newManagementPortNetdev(deviceID, cfg, nil, ovsClient)

			// createPlatformManagementPort fails
			netlinkOpsMock.On("LinkByName", types.K8sMgmtIntfName).Return(nil, fmt.Errorf("link gone")).Once()

			// create() fails: PCI device not found
			vdpaOpsMock.On("GetVdpaDeviceByPci", deviceID).Return(nil, fmt.Errorf("no vdpa device"))
			sriovnetOpsMock.On("GetNetDevicesFromPci", deviceID).Return(nil, fmt.Errorf("no such device"))

			origOsExit := klog.OsExit
			defer func() { klog.OsExit = origOsExit }()
			klog.OsExit = func(_ int) {
				panic("klog.Fatal called")
			}

			Expect(func() { _ = mgmtPort.doReconcile() }).To(PanicWith("klog.Fatal called"))
		})
	})
})
