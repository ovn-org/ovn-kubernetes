// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedroutefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	routeadvertisementsfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	factorymocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager"
	ndmmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/netlinkdevicemanager/mocks"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	vtepinfmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
	vteplistmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	multinetworkmocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks/multinetwork"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func newTestOVSClient() (libovsdbclient.Client, *libovsdbtest.Context) {
	bridgeUUID := "bridge-br-int-uuid"
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: []libovsdbtest.TestData{
			&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{bridgeUUID}},
			&vswitchd.Bridge{UUID: bridgeUUID, Name: ovsBridgeInt},
		},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return ovsClient, testCtx
}

var _ = Describe("EVPN node controller", func() {
	const (
		nodeName = "node1"
		vtepName = "vtep1"
	)

	Describe("reconcile", func() {
		var (
			ovsClient  libovsdbclient.Client
			ovsCleanup *libovsdbtest.Context
		)

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			ovsClient, ovsCleanup = newTestOVSClient()
		})

		AfterEach(func() {
			if ovsCleanup != nil {
				ovsCleanup.Cleanup()
			}
		})

		It("creates bridge and VXLANs for an unmanaged dual-stack VTEP", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("fd00::1")}},
				stopChan:       make(chan struct{}),
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == bridgeName
			})).Return(nil)
			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan4Name
			})).Return(nil).Run(func(args mock.Arguments) {
				cfg := args.Get(0).(netlinkdevicemanager.DeviceConfig)
				vxlan, isVxlan := cfg.Link.(*netlink.Vxlan)
				Expect(isVxlan).To(BeTrue())
				Expect(vxlan.SrcAddr.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
				Expect(cfg.Master).To(Equal(bridgeName))
			})
			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan6Name
			})).Return(nil).Run(func(args mock.Arguments) {
				cfg := args.Get(0).(netlinkdevicemanager.DeviceConfig)
				vxlan, isVxlan := cfg.Link.(*netlink.Vxlan)
				Expect(isVxlan).To(BeTrue())
				Expect(vxlan.SrcAddr.Equal(net.ParseIP("fd00::1"))).To(BeTrue())
				Expect(cfg.Master).To(Equal(bridgeName))
			})

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
		})

		It("cleans up devices for a managed VTEP", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeManaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				kube:         kubeMock,
				ndm:          ndm,
				networkMgr:   &networkmanager.FakeNetworkManager{},
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: make(map[string]sets.Set[string]),
				stopChan:     make(chan struct{}),
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", bridgeName).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
			ndm.AssertNotCalled(GinkgoT(), "EnsureLink", mock.Anything)
			kubeMock.AssertNotCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", mock.Anything, mock.Anything, mock.Anything)
		})

		It("deletes all devices when VTEP is not found", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "k8s.ovn.org", Resource: "vteps"}, vtepName))

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				kube:         kubeMock,
				ndm:          ndm,
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: make(map[string]sets.Set[string]),
				stopChan:     make(chan struct{}),
			}

			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", GetEVPNBridgeName(vtepName)).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertExpectations(GinkgoT())
			kubeMock.AssertNotCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", mock.Anything, mock.Anything, mock.Anything)
		})

		It("creates L3 and L2 SVIs with correct VLAN IDs and VRF master", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return(vtepName)
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("EVPNIPVRFVID").Return(200)
			netInfo.On("EVPNIPVRFVNI").Return(int32(10200))
			netInfo.On("GetNetworkName").Return("mynet")
			netInfo.On("GetNetworkID").Return(5)
			netInfo.On("GetNetworkScopedSwitchName", mock.Anything).Return("mynet_ovn_layer2_switch")

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{"test-ns": netInfo},
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			l3SVIName := GetEVPNL3SVIName(netInfo)
			l2SVIName := GetEVPNL2SVIName(netInfo)
			vrfName := util.GetNetworkVRFName(netInfo)

			ndm := &ndmmocks.Interface{}
			ndm.On("EnsureLink", mock.Anything).Return(nil)

			ctrl := &Controller{ndm: ndm, networkMgr: fakeNM, svisByBridge: make(map[string]sets.Set[string])}
			networks, err := ctrl.collectEVPNNetworks(vtepName)
			Expect(err).NotTo(HaveOccurred())

			Expect(ctrl.reconcileSVIs(bridgeName, networks)).To(Succeed())

			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				return ok && cfg.Link.Attrs().Name == l3SVIName && vlan.VlanId == 200 &&
					cfg.VLANParent == bridgeName && cfg.Master == vrfName
			}))
			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				return ok && cfg.Link.Attrs().Name == l2SVIName && vlan.VlanId == 100 &&
					cfg.VLANParent == bridgeName && cfg.Master == vrfName
			}))
		})

		It("removes stale SVIs that are no longer desired", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			bridgeName := GetEVPNBridgeName(vtepName)
			staleSVI := "evsvi-old"

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				kube:         kubeMock,
				ndm:          ndm,
				networkMgr:   &networkmanager.FakeNetworkManager{},
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: map[string]sets.Set[string]{
					bridgeName: sets.New(staleSVI),
				},
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}},
				stopChan:       make(chan struct{}),
			}

			ndm.On("EnsureLink", mock.Anything).Return(nil)
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "DeleteLink", staleSVI)
		})

		It("deletes SVIs parented to the bridge when VTEP is not found", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "k8s.ovn.org", Resource: "vteps"}, vtepName))

			bridgeName := GetEVPNBridgeName(vtepName)
			sviName := "evsvi-net1"

			ctrl := &Controller{
				nodeName:     nodeName,
				watchFactory: wf,
				kube:         kubeMock,
				ndm:          ndm,
				ovsClient:    ovsClient,
				nadVTEPInfo:  make(map[string]string),
				svisByBridge: map[string]sets.Set[string]{
					bridgeName: sets.New(sviName),
				},
				stopChan: make(chan struct{}),
			}

			ndm.On("DeleteLink", sviName).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv4)).Return(nil)
			ndm.On("DeleteLink", GetEVPNVXLANName(vtepName, utilnet.IPv6)).Return(nil)
			ndm.On("DeleteLink", bridgeName).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "DeleteLink", sviName)
			ndm.AssertExpectations(GinkgoT())
			kubeMock.AssertNotCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", mock.Anything, mock.Anything, mock.Anything)
		})

		It("fails reconciliation when hybrid overlay is enabled with conflicting VXLAN port", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}},
				stopChan:       make(chan struct{}),
			}

			By("enabling hybrid overlay with the default VXLAN port")
			config.HybridOverlay.Enabled = true
			config.HybridOverlay.VXLANPort = config.DefaultVXLANPort

			err := ctrl.reconcile(vtepName)
			Expect(err).To(HaveOccurred())

			By("using a different VXLAN port for hybrid overlay, reconciliation proceeds")
			config.HybridOverlay.VXLANPort = 4790

			ndm := &ndmmocks.Interface{}
			ndm.On("EnsureLink", mock.Anything).Return(nil)
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ctrl.ndm = ndm
			ctrl.networkMgr = &networkmanager.FakeNetworkManager{}

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
		})

		It("reconfigures VXLAN source IP when host-cidrs change mid-life", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil).Run(func(args mock.Arguments) {
				for k, v := range args.Get(1).(map[string]interface{}) {
					if node.Annotations == nil {
						node.Annotations = make(map[string]string)
					}
					node.Annotations[k] = v.(string)
				}
			})
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			am := &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}}
			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: am,
				stopChan:       make(chan struct{}),
			}

			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)

			By("first reconcile with address manager containing 100.64.0.1")
			var firstSrcAddr, secondSrcAddr net.IP
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				cfg := args.Get(0).(netlinkdevicemanager.DeviceConfig)
				if vxlan, ok := cfg.Link.(*netlink.Vxlan); ok && cfg.Link.Attrs().Name == vxlan4Name {
					if firstSrcAddr == nil {
						firstSrcAddr = vxlan.SrcAddr
					} else {
						secondSrcAddr = vxlan.SrcAddr
					}
				}
			})
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			Expect(firstSrcAddr.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())

			By("second reconcile with address manager changed to 100.64.0.5")
			am.SetIPs([]net.IP{net.ParseIP("100.64.0.5")})

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			Expect(secondSrcAddr.Equal(net.ParseIP("100.64.0.5"))).To(BeTrue())
		})

		It("deletes IPv6 VXLAN when VTEP loses IPv6 host-cidrs", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			am := &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("fd00::1")}}
			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: am,
				stopChan:       make(chan struct{}),
			}

			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			By("first reconcile with dual-stack addresses")
			ndm.On("EnsureLink", mock.Anything).Return(nil)
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan6Name
			}))

			By("second reconcile with only IPv4 addresses — IPv6 VXLAN should be deleted")
			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})

			Expect(ctrl.reconcile(vtepName)).To(Succeed())
			ndm.AssertCalled(GinkgoT(), "DeleteLink", vxlan6Name)
		})

		It("isolates devices between multiple VTEPs", func() {
			const vtepNameA = "vtep-a"
			const vtepNameB = "vtep-b"

			vtepA := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepNameA},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}
			vtepB := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepNameB},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.65.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepNameA).Return(vtepA, nil)
			lister.On("Get", vtepNameB).Return(vtepB, nil)

			var ensuredCfgs []netlinkdevicemanager.DeviceConfig
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("100.65.0.1")}},
				stopChan:       make(chan struct{}),
			}

			Expect(ctrl.reconcile(vtepNameA)).To(Succeed())
			Expect(ctrl.reconcile(vtepNameB)).To(Succeed())

			bridgeA := GetEVPNBridgeName(vtepNameA)
			bridgeB := GetEVPNBridgeName(vtepNameB)
			vxlan4A := GetEVPNVXLANName(vtepNameA, utilnet.IPv4)
			vxlan4B := GetEVPNVXLANName(vtepNameB, utilnet.IPv4)

			By("verifying each VTEP got its own bridge and VXLAN with correct source IPs")
			Expect(bridgeA).NotTo(Equal(bridgeB))
			for _, cfg := range ensuredCfgs {
				vxlan, ok := cfg.Link.(*netlink.Vxlan)
				if !ok {
					continue
				}
				switch cfg.Link.Attrs().Name {
				case vxlan4A:
					Expect(vxlan.SrcAddr.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
					Expect(cfg.Master).To(Equal(bridgeA))
				case vxlan4B:
					Expect(vxlan.SrcAddr.Equal(net.ParseIP("100.65.0.1"))).To(BeTrue())
					Expect(cfg.Master).To(Equal(bridgeB))
				}
			}

			By("deleting vtep-a and verifying vtep-b devices are untouched")
			lister.On("Get", vtepNameA).Unset()
			lister.On("Get", vtepNameA).Return(nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "k8s.ovn.org", Resource: "vteps"}, vtepNameA))

			Expect(ctrl.reconcile(vtepNameA)).To(Succeed())

			ndm.AssertCalled(GinkgoT(), "DeleteLink", bridgeA)
			ndm.AssertCalled(GinkgoT(), "DeleteLink", GetEVPNVXLANName(vtepNameA, utilnet.IPv4))
			ndm.AssertNotCalled(GinkgoT(), "DeleteLink", bridgeB)
			ndm.AssertNotCalled(GinkgoT(), "DeleteLink", vxlan4B)
		})

		It("returns error when VXLAN creation fails", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)

			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == bridgeName
			})).Return(nil)
			ndm.On("EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan4Name
			})).Return(fmt.Errorf("device busy"))

			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}},
				stopChan:       make(chan struct{}),
			}

			err := ctrl.reconcile(vtepName)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("device busy"))

			By("verifying bridge was still created despite VXLAN failure")
			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == bridgeName
			}))
		})

		It("creates only MAC-VRF SVI for Layer2-only EVPN network", func() {
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return(vtepName)
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("EVPNIPVRFVID").Return(0)
			netInfo.On("EVPNIPVRFVNI").Return(int32(0))
			netInfo.On("GetNetworkName").Return("l2only")
			netInfo.On("GetNetworkID").Return(7)
			netInfo.On("GetNetworkScopedSwitchName", mock.Anything).Return("l2only_ovn_layer2_switch")

			fakeNM := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{"test-ns": netInfo},
			}

			bridgeName := GetEVPNBridgeName(vtepName)
			l2SVIName := GetEVPNL2SVIName(netInfo)
			l3SVIName := GetEVPNL3SVIName(netInfo)

			ndm := &ndmmocks.Interface{}
			ndm.On("EnsureLink", mock.Anything).Return(nil)

			ctrl := &Controller{ndm: ndm, networkMgr: fakeNM, svisByBridge: make(map[string]sets.Set[string])}
			networks, err := ctrl.collectEVPNNetworks(vtepName)
			Expect(err).NotTo(HaveOccurred())

			Expect(ctrl.reconcileSVIs(bridgeName, networks)).To(Succeed())

			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				return ok && cfg.Link.Attrs().Name == l2SVIName && vlan.VlanId == 100
			}))
			ndm.AssertNotCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == l3SVIName
			}))
		})

		It("creates single VXLAN for IPv4-only VTEP", func() {
			vtep := &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeMock := &kubemocks.Interface{}
			kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil)
			ndm := &ndmmocks.Interface{}
			lister := &vteplistmocks.VTEPLister{}
			informer := &vtepinfmocks.VTEPInformer{}
			informer.On("Lister").Return(lister)
			wf := &factorymocks.NodeWatchFactory{}
			wf.On("VTEPInformer").Return(informer)
			wf.On("GetNode", nodeName).Return(node, nil)
			lister.On("Get", vtepName).Return(vtep, nil)

			ndm.On("EnsureLink", mock.Anything).Return(nil)
			ndm.On("DeleteLink", mock.Anything).Return(nil)

			ctrl := &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				ndm:            ndm,
				networkMgr:     &networkmanager.FakeNetworkManager{},
				ovsClient:      ovsClient,
				nadVTEPInfo:    make(map[string]string),
				svisByBridge:   make(map[string]sets.Set[string]),
				addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}},
				stopChan:       make(chan struct{}),
			}

			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			Expect(ctrl.reconcile(vtepName)).To(Succeed())

			ndm.AssertCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				vxlan, ok := cfg.Link.(*netlink.Vxlan)
				return ok && cfg.Link.Attrs().Name == vxlan4Name && vxlan.SrcAddr.Equal(net.ParseIP("100.64.0.1"))
			}))
			ndm.AssertCalled(GinkgoT(), "DeleteLink", vxlan6Name)
			ndm.AssertNotCalled(GinkgoT(), "EnsureLink", mock.MatchedBy(func(cfg netlinkdevicemanager.DeviceConfig) bool {
				return cfg.Link.Attrs().Name == vxlan6Name
			}))
		})

		It("cleans up stale OVS ports for a VTEP", func() {
			// Create a stale OVS port using the production helper
			stalePortName := "evovs-stale"
			Expect(libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, ovsBridgeInt, stalePortName,
				map[string]string{types.EVPNVTEPExternalID: vtepName}, nil)).To(Succeed())

			// Verify the port exists before cleanup
			_, err := libovsdbops.GetOVSPort(ovsClient, stalePortName)
			Expect(err).NotTo(HaveOccurred())

			ctrl := &Controller{ovsClient: ovsClient}

			By("calling reconcileOVSPorts with no desired ports")
			err = ctrl.reconcileOVSPorts(vtepName, GetEVPNBridgeName(vtepName), nil)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the stale OVS port was removed")
			_, err = libovsdbops.GetOVSPort(ovsClient, stalePortName)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("address change reconciliation", func() {
		var (
			kubeClient *fake.Clientset
			vtepClient *vtepfake.Clientset
			wf         *factory.WatchFactory
			ndm        *ndmmocks.Interface
			fakeNM     *networkmanager.FakeNetworkManager
			ovsCleanup *libovsdbtest.Context
			ctrl       *Controller
			am         *fakeAddressManager
		)

		BeforeEach(func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEVPN = true
			config.Gateway.Mode = config.GatewayModeLocal

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			kubeClient = fake.NewClientset(node)
			vtepClient = vtepfake.NewSimpleClientset()
			ndm = &ndmmocks.Interface{}
			am = &fakeAddressManager{}

			var err error
			wf, err = factory.NewNodeWatchFactory(&util.OVNNodeClientset{
				KubeClient:                kubeClient,
				EgressServiceClient:       egressservicefake.NewClientset(),
				EgressIPClient:            egressipfake.NewClientset(),
				AdminPolicyRouteClient:    adminpolicybasedroutefake.NewClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				UserDefinedNetworkClient:  udnfake.NewClientset(),
				RouteAdvertisementsClient: routeadvertisementsfake.NewClientset(),
				UplinkClient:              uplinkfake.NewSimpleClientset(),
				VTEPClient:                vtepClient,
			}, nodeName)
			Expect(err).NotTo(HaveOccurred())
			Expect(wf.Start()).To(Succeed())

			fakeNM = &networkmanager.FakeNetworkManager{}
			var ovsClient libovsdbclient.Client
			ovsClient, ovsCleanup = newTestOVSClient()
			ctrl, err = NewController(nodeName, wf, &kube.Kube{KClient: kubeClient}, ndm, fakeNM, ovsClient, am)
			Expect(err).NotTo(HaveOccurred())
			Expect(ctrl.Start()).To(Succeed())
		})

		AfterEach(func() {
			if ctrl != nil {
				ctrl.Stop()
			}
			if wf != nil {
				wf.Shutdown()
			}
			if ovsCleanup != nil {
				ovsCleanup.Cleanup()
			}
		})

		It("reconciles VTEPs when node addresses change", func() {
			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)

			// testify's mock.Calls is not safe to read concurrently, so we
			// collect EnsureLink configs via a Run callback under our own lock.
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			By("allowing any NDM calls throughout the test")
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and waiting for informer sync")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			By("updating address manager IPs and triggering reconciliation")
			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			By("verifying the full chain: reconcileNodeAddressChange → reconcile → NDM creates bridge and VXLAN")
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var names []string
				for _, cfg := range ensuredCfgs {
					names = append(names, cfg.Link.Attrs().Name)
				}
				return names
			}).Should(ContainElements(bridgeName, vxlan4Name))

			By("verifying IPv6 VXLAN was deleted (no IPv6 address)")
			Expect(ndm.AssertCalled(GinkgoT(), "DeleteLink", vxlan6Name)).To(BeTrue())
		})

		It("reconciles when VTEP exists before addresses are available", func() {
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP before addresses are available")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			By("updating address manager and triggering reconciliation")
			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)

			By("verifying bridge and VXLAN are created after addresses arrive")
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var names []string
				for _, cfg := range ensuredCfgs {
					names = append(names, cfg.Link.Attrs().Name)
				}
				return names
			}).Should(ContainElements(bridgeName, vxlan4Name))
		})

		It("skips reconciliation when annotated IPs are still valid", func() {
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and performing initial reconciliation")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			bridgeName := GetEVPNBridgeName(vtepName)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var names []string
				for _, cfg := range ensuredCfgs {
					names = append(names, cfg.Link.Attrs().Name)
				}
				return names
			}).Should(ContainElements(bridgeName, vxlan4Name))

			By("recording call count after initial reconciliation completes")
			ensuredMu.Lock()
			initialCount := len(ensuredCfgs)
			ensuredMu.Unlock()

			By("triggering address change with same IPs — no new NDM calls expected")
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)
			Consistently(func() int {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				return len(ensuredCfgs)
			}).Should(Equal(initialCount))
		})

		It("reconciles when annotated IP is no longer on the node", func() {
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and performing initial reconciliation")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			Eventually(func() bool {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name {
						return true
					}
				}
				return false
			}).Should(BeTrue())

			By("changing node IP — should reconcile with new source IP")
			ensuredMu.Lock()
			ensuredCfgs = nil
			ensuredMu.Unlock()

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.5")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			Eventually(func() net.IP {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if vxlan, ok := cfg.Link.(*netlink.Vxlan); ok && cfg.Link.Attrs().Name == vxlan4Name {
						return vxlan.SrcAddr
					}
				}
				return nil
			}).Should(Equal(net.ParseIP("100.64.0.5")))
		})

		It("reconciles when a required IP family becomes available", func() {
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating a dual-stack VTEP and reconciling with only IPv4")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			Eventually(func() bool {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name {
						return true
					}
				}
				return false
			}).Should(BeTrue())

			By("adding IPv6 address — should reconcile and create IPv6 VXLAN")
			ensuredMu.Lock()
			ensuredCfgs = nil
			ensuredMu.Unlock()

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("fd00::1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			vxlan6Name := GetEVPNVXLANName(vtepName, utilnet.IPv6)
			Eventually(func() net.IP {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if vxlan, ok := cfg.Link.(*netlink.Vxlan); ok && cfg.Link.Attrs().Name == vxlan6Name {
						return vxlan.SrcAddr
					}
				}
				return nil
			}).Should(Equal(net.ParseIP("fd00::1")))
		})

		It("restores VTEP annotation when externally removed", func() {
			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and performing initial reconciliation")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			ctrl.vtepController.Reconcile(reconcileNodeAddressChange)

			By("waiting for the VTEP annotation to be set on the node")
			Eventually(func() (map[string]util.VTEPNodeAnnotation, error) {
				node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				return util.ParseNodeVTEPs(node)
			}).Should(HaveKey(vtepName))

			By("externally removing the VTEP annotation")
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(node.Annotations, util.OVNNodeVTEPs)
			_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the controller restores the annotation automatically")
			Eventually(func() ([]string, error) {
				node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				vteps, err := util.ParseNodeVTEPs(node)
				if err != nil {
					return nil, err
				}
				return vteps[vtepName].IPs, nil
			}).Should(ConsistOf("100.64.0.1"))
		})

		DescribeTable("repairs forged VTEP annotations even with the controller's field manager", func(ips string) {
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil)
			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}, Mode: vtepv1.VTEPModeUnmanaged},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "creating unmanaged VTEP")
			readIPs := func() ([]string, error) {
				node, readErr := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
				if readErr != nil {
					return nil, readErr
				}
				vteps, parseErr := util.ParseNodeVTEPs(node)
				return vteps[vtepName].IPs, parseErr
			}
			Eventually(readIPs, 5*time.Second, 100*time.Millisecond).Should(ConsistOf("100.64.0.1"), "the initial annotation must contain the discovered node address")
			node, err := kubeClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "reading node before forging its VTEP annotation")
			node.Annotations[util.OVNNodeVTEPs] = `{"vtep1":{"ips":` + ips + `}}`
			node.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: vtepAnnotationFieldManager, Time: &metav1.Time{Time: metav1.Now().Time}}}
			_, err = kubeClient.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred(), "updating forged VTEP annotation")
			Eventually(readIPs, 5*time.Second, 100*time.Millisecond).Should(ConsistOf("100.64.0.1"), "repair must replace the forged entry with the discovered node address")
		},
			Entry("replaced address", `["100.64.0.2"]`),
			Entry("appended address", `["100.64.0.1","100.64.0.99"]`),
			Entry("unsupported family", `["100.64.0.1","fd00::99"]`),
		)

		It("reconciles VTEP with VID/VNI mappings when a network is added or removed", func() {
			const nadKey = "test-ns/test-nad"

			var ensuredMu sync.Mutex
			var ensuredCfgs []netlinkdevicemanager.DeviceConfig

			By("allowing any NDM calls throughout the test")
			ndm.On("DeleteLink", mock.Anything).Return(nil)
			ndm.On("EnsureLink", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				ensuredCfgs = append(ensuredCfgs, args.Get(0).(netlinkdevicemanager.DeviceConfig))
			})

			By("creating an unmanaged VTEP and setting up addresses")
			_, err := vtepClient.K8sV1().VTEPs().Create(context.Background(), &vtepv1.VTEP{
				ObjectMeta: metav1.ObjectMeta{Name: vtepName},
				Spec: vtepv1.VTEPSpec{
					CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
					Mode:  vtepv1.VTEPModeUnmanaged,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := wf.VTEPInformer().Lister().Get(vtepName)
				return err
			}).Should(Succeed())

			am.SetIPs([]net.IP{net.ParseIP("100.64.0.1")})

			By("simulating a network add with EVPN VID/VNI mappings")
			netInfo := &multinetworkmocks.NetInfo{}
			netInfo.On("EVPNVTEPName").Return(vtepName)
			netInfo.On("EVPNMACVRFVID").Return(100)
			netInfo.On("EVPNMACVRFVNI").Return(int32(10100))
			netInfo.On("EVPNIPVRFVID").Return(200)
			netInfo.On("EVPNIPVRFVNI").Return(int32(10200))
			netInfo.On("GetNetworkName").Return("mynet")
			netInfo.On("GetNetworkID").Return(5)
			netInfo.On("GetNetworkScopedSwitchName", mock.Anything).Return("mynet_ovn_layer2_switch")
			fakeNM.NADNetworks = map[string]util.NetInfo{nadKey: netInfo}
			fakeNM.PrimaryNetworks = map[string]util.NetInfo{"test-ns": netInfo}
			fakeNM.TriggerHandlers(nadKey, netInfo, false)

			l3SVIName := GetEVPNL3SVIName(netInfo)
			l2SVIName := GetEVPNL2SVIName(netInfo)
			vxlan4Name := GetEVPNVXLANName(vtepName, utilnet.IPv4)
			bridgeName := GetEVPNBridgeName(vtepName)

			By("verifying VID/VNI mappings on the VXLAN device")
			Eventually(func() []netlinkdevicemanager.VIDVNIMapping {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name && len(cfg.VIDVNIMappings) > 0 {
						return cfg.VIDVNIMappings
					}
				}
				return nil
			}).Should(ConsistOf(
				netlinkdevicemanager.VIDVNIMapping{VID: 100, VNI: 10100},
				netlinkdevicemanager.VIDVNIMapping{VID: 200, VNI: 10200},
			))

			By("verifying L3 and L2 SVIs were created with correct VLAN IDs and bridge parent")
			Eventually(func() []string {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				var svis []string
				for _, cfg := range ensuredCfgs {
					if _, ok := cfg.Link.(*netlink.Vlan); ok {
						svis = append(svis, cfg.Link.Attrs().Name)
					}
				}
				return svis
			}).Should(ContainElements(l3SVIName, l2SVIName))

			ensuredMu.Lock()
			for _, cfg := range ensuredCfgs {
				vlan, ok := cfg.Link.(*netlink.Vlan)
				if !ok {
					continue
				}
				switch cfg.Link.Attrs().Name {
				case l3SVIName:
					Expect(vlan.VlanId).To(Equal(200))
					Expect(cfg.VLANParent).To(Equal(bridgeName))
				case l2SVIName:
					Expect(vlan.VlanId).To(Equal(100))
					Expect(cfg.VLANParent).To(Equal(bridgeName))
				}
			}
			ensuredMu.Unlock()

			By("simulating network removal and verifying VXLAN has no mappings")
			ensuredMu.Lock()
			ensuredCfgs = nil
			ensuredMu.Unlock()
			fakeNM.Lock()
			fakeNM.NADNetworks = nil
			fakeNM.PrimaryNetworks = nil
			fakeNM.Unlock()
			fakeNM.TriggerHandlers(nadKey, nil, true)

			Eventually(func() bool {
				ensuredMu.Lock()
				defer ensuredMu.Unlock()
				for _, cfg := range ensuredCfgs {
					if cfg.Link.Attrs().Name == vxlan4Name {
						return len(cfg.VIDVNIMappings) == 0
					}
				}
				return false
			}).Should(BeTrue())

			By("verifying stale SVIs were deleted after network removal")
			Eventually(func() bool {
				return ndm.AssertCalled(GinkgoT(), "DeleteLink", l3SVIName) &&
					ndm.AssertCalled(GinkgoT(), "DeleteLink", l2SVIName)
			}).Should(BeTrue())
		})
	})
})

var _ = Describe("cleanupStaleOVSPorts", func() {
	const vtepName = "vtep1"

	It("removes OVS ports for deleted VTEPs and preserves active ones", func() {
		Expect(config.PrepareTestConfig()).To(Succeed())

		ovsClient, ovsCleanup := newTestOVSClient()
		defer ovsCleanup.Cleanup()

		By("creating OVS ports that belonged to a now-deleted VTEP")
		Expect(libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, ovsBridgeInt, "evovs-stale1",
			map[string]string{types.EVPNVTEPExternalID: "deleted-vtep"}, nil)).To(Succeed())
		Expect(libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, ovsBridgeInt, "evovs-stale2",
			map[string]string{types.EVPNVTEPExternalID: "deleted-vtep"}, nil)).To(Succeed())

		By("creating an OVS port for a VTEP that still exists")
		Expect(libovsdbops.CreateOrUpdatePortWithInterface(ovsClient, ovsBridgeInt, "evovs-active",
			map[string]string{types.EVPNVTEPExternalID: vtepName}, nil)).To(Succeed())

		ctrl := &Controller{ovsClient: ovsClient}
		activeVTEPs := sets.New[string](vtepName)

		By("running cleanupStaleOVSPorts")
		Expect(ctrl.cleanupStaleOVSPorts(activeVTEPs)).To(Succeed())

		By("verifying stale OVS ports for deleted VTEP were removed")
		_, err := libovsdbops.GetOVSPort(ovsClient, "evovs-stale1")
		Expect(err).To(HaveOccurred())
		_, err = libovsdbops.GetOVSPort(ovsClient, "evovs-stale2")
		Expect(err).To(HaveOccurred())

		By("verifying active VTEP's OVS port was preserved")
		_, err = libovsdbops.GetOVSPort(ovsClient, "evovs-active")
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("vtepNeedsUpdate", func() {
	It("triggers reconcile when a new VTEP is created/deleted", func() {
		ctrl := &Controller{}
		vtep := &vtepv1.VTEP{Spec: vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.0.0.0/24"}}}
		Expect(ctrl.vtepNeedsUpdate(nil, vtep)).To(BeTrue())
		Expect(ctrl.vtepNeedsUpdate(vtep, nil)).To(BeTrue())
	})

	It("triggers reconcile when a VTEP CIDR is added/removed", func() {
		ctrl := &Controller{}
		old := &vtepv1.VTEP{Spec: vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"10.0.0.0/24"}}}
		updated := old.DeepCopy()
		updated.Spec.CIDRs = []vtepv1.CIDR{"10.0.0.0/24", "fd00::/64"}
		Expect(ctrl.vtepNeedsUpdate(old, updated)).To(BeTrue())
		old = updated
		updated = old.DeepCopy()
		updated.Spec.CIDRs = []vtepv1.CIDR{"fd00::/64"}
		Expect(ctrl.vtepNeedsUpdate(old, updated)).To(BeTrue())
	})
})

type fakeAddressManager struct {
	mu  sync.Mutex
	ips []net.IP
}

func (f *fakeAddressManager) SetIPs(ips []net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips = ips
}

func (f *fakeAddressManager) ListAddresses() ([]net.IP, []*net.IPNet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]net.IP, len(f.ips))
	copy(out, f.ips)
	return out, nil
}

func (f *fakeAddressManager) AddOnAddressesChangedHandler(func()) {}

var _ = Describe("discoverUnmanagedVTEPIPs", func() {
	const nodeName = "node1"

	type testController struct {
		*Controller
		node     *corev1.Node
		kubeMock *kubemocks.Interface
	}

	newTestController := func(ips []net.IP, annotations map[string]string) *testController {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: annotations}}
		kubeMock := &kubemocks.Interface{}
		kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(nil).Run(func(args mock.Arguments) {
			for k, v := range args.Get(1).(map[string]interface{}) {
				if node.Annotations == nil {
					node.Annotations = make(map[string]string)
				}
				node.Annotations[k] = v.(string)
			}
		})
		wf := &factorymocks.NodeWatchFactory{}
		wf.On("GetNode", nodeName).Return(node, nil)
		return &testController{
			Controller: &Controller{
				nodeName:       nodeName,
				watchFactory:   wf,
				kube:           kubeMock,
				addressManager: &fakeAddressManager{ips: ips},
			},
			node:     node,
			kubeMock: kubeMock,
		}
	}

	It("discovers a single IPv4 match and annotates it", func() {
		tc := newTestController([]net.IP{net.ParseIP("100.64.0.1")}, nil)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
		Expect(v6).To(BeNil())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.1"))
	})

	It("discovers a single IPv6 match and annotates it", func() {
		tc := newTestController([]net.IP{net.ParseIP("fd00::1")}, nil)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"fd00::/64"}},
		}

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4).To(BeNil())
		Expect(v6.Equal(net.ParseIP("fd00::1"))).To(BeTrue())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("fd00::1"))
	})

	It("discovers dual-stack IPs and annotates both", func() {
		tc := newTestController([]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("fd00::1")}, nil)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"}},
		}

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
		Expect(v6.Equal(net.ParseIP("fd00::1"))).To(BeTrue())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.1", "fd00::1"))
	})

	It("filters keepalived VIPs when multiple IPs match and annotates the selected one", func() {
		tc := newTestController([]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("100.64.0.2")}, nil)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		nlMock := &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)
		defer util.ResetNetLinkOpMockInst()

		nlMock.On("AddrList", mock.Anything, netlink.FAMILY_V4).Return([]netlink.Addr{
			{IPNet: &net.IPNet{IP: net.ParseIP("100.64.0.1"), Mask: net.CIDRMask(24, 32)}},
			{IPNet: &net.IPNet{IP: net.ParseIP("100.64.0.2"), Mask: net.CIDRMask(24, 32)}, Label: "eth0:vip"},
		}, nil)

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())
		Expect(v6).To(BeNil())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.1"))
	})

	It("picks the lowest IP when multiple non-VIP IPs match and annotates it", func() {
		tc := newTestController([]net.IP{net.ParseIP("100.64.0.5"), net.ParseIP("100.64.0.2")}, nil)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		nlMock := &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(nlMock)
		defer util.ResetNetLinkOpMockInst()

		nlMock.On("AddrList", mock.Anything, netlink.FAMILY_V4).Return([]netlink.Addr{
			{IPNet: &net.IPNet{IP: net.ParseIP("100.64.0.5"), Mask: net.CIDRMask(24, 32)}},
			{IPNet: &net.IPNet{IP: net.ParseIP("100.64.0.2"), Mask: net.CIDRMask(24, 32)}},
		}, nil)

		v4, _, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.2"))).To(BeTrue())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.2"))
	})

	It("reuses annotated IP when still valid and does not write annotation", func() {
		tc := newTestController(
			[]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("100.64.0.5")},
			map[string]string{util.OVNNodeVTEPs: `{"vtep1":{"ips":["100.64.0.5"]}}`},
		)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.5"))).To(BeTrue(), "should reuse the annotated IP, not pick a new one")
		Expect(v6).To(BeNil())

		tc.kubeMock.AssertNotCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", mock.Anything, mock.Anything, mock.Anything)
	})

	It("selects a new IP and updates annotation when annotated IP is no longer on the node", func() {
		tc := newTestController(
			[]net.IP{net.ParseIP("100.64.0.5")},
			map[string]string{util.OVNNodeVTEPs: `{"vtep1":{"ips":["100.64.0.1"]}}`},
		)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		v4, _, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.5"))).To(BeTrue())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.5"))
	})

	It("selects a new IP when annotated IP is outside VTEP CIDRs", func() {
		tc := newTestController(
			[]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("10.0.0.1")},
			map[string]string{util.OVNNodeVTEPs: `{"vtep1":{"ips":["10.0.0.1"]}}`},
		)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		v4, _, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue())

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
	})

	It("restores cached annotation when SetAnnotationsOnNodeWithFieldManager fails", func() {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
		kubeMock := &kubemocks.Interface{}
		kubeMock.On("SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager).Return(fmt.Errorf("API unavailable"))
		wf := &factorymocks.NodeWatchFactory{}
		wf.On("GetNode", nodeName).Return(node, nil)

		tc := &Controller{
			nodeName:       nodeName,
			watchFactory:   wf,
			kube:           kubeMock,
			addressManager: &fakeAddressManager{ips: []net.IP{net.ParseIP("100.64.0.1")}},
		}

		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24"}},
		}

		_, _, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).To(HaveOccurred())

		By("verifying the in-memory annotation cache was restored to its previous (empty) state")
		Expect(tc.vtepsAnnotation).NotTo(HaveKey("vtep1"))
	})

	It("reuses valid IPv4 and selects new IPv6 when only IPv6 changes", func() {
		tc := newTestController(
			[]net.IP{net.ParseIP("100.64.0.1"), net.ParseIP("fd00::5")},
			map[string]string{util.OVNNodeVTEPs: `{"vtep1":{"ips":["100.64.0.1","fd00::1"]}}`},
		)
		vtep := &vtepv1.VTEP{
			ObjectMeta: metav1.ObjectMeta{Name: "vtep1"},
			Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"}},
		}

		v4, v6, err := tc.discoverUnmanagedVTEPIPs(vtep)
		Expect(err).NotTo(HaveOccurred())
		Expect(v4.Equal(net.ParseIP("100.64.0.1"))).To(BeTrue(), "IPv4 should be reused")
		Expect(v6.Equal(net.ParseIP("fd00::5"))).To(BeTrue(), "IPv6 should be newly selected")

		tc.kubeMock.AssertCalled(GinkgoT(), "SetAnnotationsOnNodeWithFieldManager", nodeName, mock.Anything, vtepAnnotationFieldManager)
		vteps, err := util.ParseNodeVTEPs(tc.node)
		Expect(err).NotTo(HaveOccurred())
		Expect(vteps).To(HaveLen(1))
		Expect(vteps["vtep1"].IPs).To(ConsistOf("100.64.0.1", "fd00::5"))
	})
})
