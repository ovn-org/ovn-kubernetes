// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	uplinkinformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/informers/externalversions"
	factoryMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory/mocks"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	nadinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlistermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	coreinformermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	corelistermocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func genListStalePortsCmd() string {
	return "ovs-vsctl --timeout=15 --data=bare --no-headings --columns=name find interface ofport=-1"
}

func genDeleteStalePortCmd(ifaces ...string) string {
	staleIfacesCmd := ""
	for _, iface := range ifaces {
		if len(staleIfacesCmd) > 0 {
			staleIfacesCmd += fmt.Sprintf(" -- --if-exists --with-iface del-port %s", iface)
		} else {
			staleIfacesCmd += fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists --with-iface del-port %s", iface)
		}
	}
	return staleIfacesCmd
}

func newTestOVSClient(ovsData []libovsdbtest.TestData) (libovsdbclient.Client, *libovsdbtest.Context) {
	ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
		OVSData: ovsData,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return ovsClient, testCtx
}

func ovsPortAndInterface(portUUID, ifaceUUID, name string, extIDs map[string]string) (*vswitchd.Port, *vswitchd.Interface) {
	return &vswitchd.Port{UUID: portUUID, Name: name, Interfaces: []string{ifaceUUID}},
		&vswitchd.Interface{UUID: ifaceUUID, Name: name, ExternalIDs: extIDs}
}

func expectUplinkInformers(factoryMock *factoryMocks.NodeWatchFactory) {
	uplinkClient := uplinkfake.NewSimpleClientset()
	uplinkFactory := uplinkinformerfactory.NewSharedInformerFactory(uplinkClient, time.Second)

	factoryMock.On("UplinkInformer").Return(uplinkFactory.K8s().V1alpha1().Uplinks())
	factoryMock.On("UplinkStateInformer").Return(uplinkFactory.K8s().V1alpha1().UplinkStates())
}

func expectNodeInformer(nodeInformerMock *coreinformermocks.NodeInformer) {
	nodeInformerMock.On("Informer").Return(cache.NewSharedIndexInformer(
		&cache.ListWatch{},
		&corev1.Node{},
		time.Second,
		cache.Indexers{},
	))
}

var _ = Describe("Healthcheck tests", func() {
	var execMock *ovntest.FakeExec
	var factoryMock factoryMocks.NodeWatchFactory
	var fakeClient *util.OVNClientset
	var err error

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		execMock = ovntest.NewFakeExec()
		Expect(util.SetExec(execMock)).To(Succeed())
		factoryMock = factoryMocks.NodeWatchFactory{}
		v1Objects := []runtime.Object{}
		fakeClient = &util.OVNClientset{
			KubeClient: fake.NewSimpleClientset(v1Objects...),
		}
	})

	AfterEach(func() {
		util.ResetRunner()
	})

	Describe("checkForStaleOVSInternalPorts", func() {

		Context("bridge has stale ports", func() {
			It("removes stale ports from bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genListStalePortsCmd(),
					Output: "foo\n\nbar\n\n" + types.K8sMgmtIntfName + "\n\n",
					Err:    nil,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genDeleteStalePortCmd("foo", "bar"),
					Output: "",
					Err:    nil,
				})
				checkForStaleOVSInternalPorts()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

		Context("bridge does not have stale ports", func() {
			It("Does not remove any ports from bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genListStalePortsCmd(),
					Output: types.K8sMgmtIntfName + "\n\n",
					Err:    nil,
				})
				checkForStaleOVSInternalPorts()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})
	})

	Describe("checkForStaleOVSPodInterfaces", func() {
		var ncm *NodeControllerManager
		var ovsCleanup *libovsdbtest.Context
		nodeName := "localNode"
		routeManager := routemanager.NewController()
		podList := []*corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "a-pod",
					Namespace:   "a-ns",
					Annotations: map[string]string{},
					UID:         "pod-a-uuid-1",
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "b-pod",
					Namespace:   "b-ns",
					Annotations: map[string]string{},
					UID:         "pod-b-uuid-2",
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
			},
		}

		setupNCM := func(ovsData []libovsdbtest.TestData) {
			factoryMock.On("GetPods", "").Return(podList, nil)
			nadListerMock := &nadlistermocks.NetworkAttachmentDefinitionLister{}
			nadInformerMock := &nadinformermocks.NetworkAttachmentDefinitionInformer{}
			nadInformerMock.On("Lister").Return(nadListerMock)
			nadInformerMock.On("Informer").Return(nil)
			factoryMock.On("NADInformer").Return(nadInformerMock)
			nodeInformerMock := &coreinformermocks.NodeInformer{}
			nodeListerMock := &corelistermocks.NodeLister{}
			nodeListerMock.On("List", mock.Anything).Return(nil, nil)
			nodeInformerMock.On("Lister").Return(nodeListerMock)
			factoryMock.On("NodeCoreInformer").Return(nodeInformerMock)

			var ovsClient libovsdbclient.Client
			ovsClient, ovsCleanup = newTestOVSClient(ovsData)

			ncm, err = NewNodeControllerManager(fakeClient, &factoryMock, nil, nil, nodeName, &sync.WaitGroup{}, nil, routeManager, ovsClient)
			Expect(err).NotTo(HaveOccurred())
		}

		AfterEach(func() {
			if ovsCleanup != nil {
				ovsCleanup.Cleanup()
			}
		})

		Context("bridge has stale representor ports", func() {
			It("removes stale VF rep ports from bridge", func() {
				portA, ifaceA := ovsPortAndInterface("port-1", "iface-1", "pod-a-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "a-ns_a-pod", "iface-id-ver": "pod-a-uuid-1", "vf-netdev-name": "blah"})
				portB, ifaceB := ovsPortAndInterface("port-2", "iface-2", "pod-b-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "b-ns_b-pod", "iface-id-ver": "pod-b-uuid-2", "vf-netdev-name": "blah"})
				portStale, ifaceStale := ovsPortAndInterface("port-3", "iface-3", "stale-pod-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "stale-ns_stale-pod", "iface-id-ver": "pod-stale-uuid-3", "vf-netdev-name": "blah"})
				setupNCM([]libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-uuid"}},
					&vswitchd.Bridge{UUID: "bridge-uuid", Name: "br-int", Ports: []string{"port-1", "port-2", "port-3"}},
					portA, ifaceA, portB, ifaceB, portStale, ifaceStale,
				})
				ncm.checkForStaleOVSPodInterfaces()
				_, err := libovsdbops.GetOVSPort(ncm.ovsClient, "pod-a-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "pod-b-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "stale-pod-ifc")
				Expect(err).To(HaveOccurred())
			})
		})

		Context("bridge does not have stale representor ports", func() {
			It("does not remove any port from bridge", func() {
				portA, ifaceA := ovsPortAndInterface("port-1", "iface-1", "pod-a-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "a-ns_a-pod", "iface-id-ver": "pod-a-uuid-1", "vf-netdev-name": "blah"})
				portB, ifaceB := ovsPortAndInterface("port-2", "iface-2", "pod-b-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "b-ns_b-pod", "iface-id-ver": "pod-b-uuid-2", "vf-netdev-name": "blah"})
				setupNCM([]libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-uuid"}},
					&vswitchd.Bridge{UUID: "bridge-uuid", Name: "br-int", Ports: []string{"port-1", "port-2"}},
					portA, ifaceA, portB, ifaceB,
				})
				ncm.checkForStaleOVSPodInterfaces()
				_, err := libovsdbops.GetOVSPort(ncm.ovsClient, "pod-a-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "pod-b-ifc")
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("bridge has stale VFIO representor ports", func() {
			It("removes stale VFIO rep ports identified by vf-is-vfio=true", func() {
				portA, ifaceA := ovsPortAndInterface("port-1", "iface-1", "pod-a-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "a-ns_a-pod", "iface-id-ver": "pod-a-uuid-1", "vf-netdev-name": "blah"})
				portVfio, ifaceVfio := ovsPortAndInterface("port-4", "iface-4", "vfio-pod-ifc", map[string]string{
					"sandbox": "456defbbb", "iface-id": "vfio-ns_vfio-pod", "iface-id-ver": "pod-vfio-uuid-4", "vf-is-vfio": "true"})
				setupNCM([]libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-uuid"}},
					&vswitchd.Bridge{UUID: "bridge-uuid", Name: "br-int", Ports: []string{"port-1", "port-4"}},
					portA, ifaceA, portVfio, ifaceVfio,
				})
				ncm.checkForStaleOVSPodInterfaces()
				_, err := libovsdbops.GetOVSPort(ncm.ovsClient, "pod-a-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "vfio-pod-ifc")
				Expect(err).To(HaveOccurred())
			})

			It("does not remove VFIO rep ports for existing pods", func() {
				portA, ifaceA := ovsPortAndInterface("port-1", "iface-1", "pod-a-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "a-ns_a-pod", "iface-id-ver": "pod-a-uuid-1", "vf-is-vfio": "true"})
				portB, ifaceB := ovsPortAndInterface("port-2", "iface-2", "pod-b-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "b-ns_b-pod", "iface-id-ver": "pod-b-uuid-2", "vf-is-vfio": "true"})
				setupNCM([]libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-uuid"}},
					&vswitchd.Bridge{UUID: "bridge-uuid", Name: "br-int", Ports: []string{"port-1", "port-2"}},
					portA, ifaceA, portB, ifaceB,
				})
				ncm.checkForStaleOVSPodInterfaces()
				_, err := libovsdbops.GetOVSPort(ncm.ovsClient, "pod-a-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "pod-b-ifc")
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("bridge has stale veth host-side interfaces", func() {
			It("removes stale veth interfaces (no representor markers) for gone pods", func() {
				portVeth, ifaceVeth := ovsPortAndInterface("port-5", "iface-5", "veth-ifc", map[string]string{
					"sandbox": "789abc", "iface-id": "stale-ns_stale-pod", "iface-id-ver": "pod-stale-uuid-5"})
				portA, ifaceA := ovsPortAndInterface("port-1", "iface-1", "pod-a-ifc", map[string]string{
					"sandbox": "123abcfaa", "iface-id": "a-ns_a-pod", "iface-id-ver": "pod-a-uuid-1"})
				setupNCM([]libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs", Bridges: []string{"bridge-uuid"}},
					&vswitchd.Bridge{UUID: "bridge-uuid", Name: "br-int", Ports: []string{"port-5", "port-1"}},
					portVeth, ifaceVeth, portA, ifaceA,
				})
				ncm.checkForStaleOVSPodInterfaces()
				_, err := libovsdbops.GetOVSPort(ncm.ovsClient, "pod-a-ifc")
				Expect(err).NotTo(HaveOccurred())
				_, err = libovsdbops.GetOVSPort(ncm.ovsClient, "veth-ifc")
				Expect(err).To(HaveOccurred())
			})
		})

	})

	Describe("NewNodeControllerManager", func() {
		It("creates a VRF manager in DPU mode", func() {
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OvnKubeNode.Mode = types.NodeModeDPU

			factoryMock := factoryMocks.NodeWatchFactory{}
			factoryMock.On("UserDefinedNetworkInformer").Return(nil)
			factoryMock.On("ClusterUserDefinedNetworkInformer").Return(nil)
			factoryMock.On("NamespaceInformer").Return(nil)
			nadListerMock := &nadlistermocks.NetworkAttachmentDefinitionLister{}
			nadInformerMock := &nadinformermocks.NetworkAttachmentDefinitionInformer{}
			nadInformerMock.On("Lister").Return(nadListerMock)
			nadInformerMock.On("Informer").Return(nil)
			factoryMock.On("NADInformer").Return(nadInformerMock)
			nodeInformerMock := &coreinformermocks.NodeInformer{}
			nodeListerMock := &corelistermocks.NodeLister{}
			nodeInformerMock.On("Lister").Return(nodeListerMock)
			expectNodeInformer(nodeInformerMock)
			factoryMock.On("NodeCoreInformer").Return(nodeInformerMock)
			fakeClient := &util.OVNClientset{
				KubeClient:   fake.NewSimpleClientset(),
				UplinkClient: uplinkfake.NewSimpleClientset(),
			}
			expectUplinkInformers(&factoryMock)

			ncm, err := NewNodeControllerManager(fakeClient, &factoryMock, nil, nil, "worker1",
				&sync.WaitGroup{}, nil, routemanager.NewController(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(ncm.vrfManager).NotTo(BeNil())
			Expect(ncm.ruleManager).To(BeNil())
		})
	})

	Context("verify cleanup of deleted networks", func() {
		var (
			staleNetID uint = 1000
			nodeName        = "worker1"
			nad             = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
				types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
			netName      = "bluenet"
			netID        = 1003
			v4NodeSubnet = "10.128.0.0/24"
			v6NodeSubnet = "ae70::66/112"
			testNS       ns.NetNS
			fakeClient   *util.OVNClientset
			routeManager = routemanager.NewController()
		)

		BeforeEach(func() {
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())

			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			v1Objects := []runtime.Object{}
			fakeClient = &util.OVNClientset{
				KubeClient:   fake.NewSimpleClientset(v1Objects...),
				UplinkClient: uplinkfake.NewSimpleClientset(),
			}
		})

		AfterEach(func() {
			Expect(testNS.Close()).To(Succeed())
			Expect(testutils.UnmountNS(testNS)).To(Succeed())
		})

		ovntest.OnSupportedPlatformsIt("check vrf devices are cleaned for deleted networks", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			factoryMock := factoryMocks.NodeWatchFactory{}
			netInfo, err := util.ParseNADInfo(nad)
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			Expect(err).NotTo(HaveOccurred())
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%d\"}", netName, netID),
						"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet)},
				},
			}
			nodeList := []*corev1.Node{node}
			factoryMock.On("GetNodeForWindows", nodeName).Return(nodeList[0], nil)
			factoryMock.On("GetNodes").Return(nodeList, nil)
			factoryMock.On("UserDefinedNetworkInformer").Return(nil)
			factoryMock.On("ClusterUserDefinedNetworkInformer").Return(nil)
			factoryMock.On("NamespaceInformer").Return(nil)
			nadListerMock := &nadlistermocks.NetworkAttachmentDefinitionLister{}
			nadInformerMock := &nadinformermocks.NetworkAttachmentDefinitionInformer{}
			nadInformerMock.On("Lister").Return(nadListerMock)
			nadInformerMock.On("Informer").Return(nil)
			factoryMock.On("NADInformer").Return(nadInformerMock)
			nodeListerMock := &corelistermocks.NodeLister{}
			nodeListerMock.On("List", mock.Anything).Return(nodeList, nil)
			nodeInformerMock := &coreinformermocks.NodeInformer{}
			nodeInformerMock.On("Lister").Return(nodeListerMock)
			expectNodeInformer(nodeInformerMock)
			factoryMock.On("NodeCoreInformer").Return(nodeInformerMock)
			expectUplinkInformers(&factoryMock)

			ncm, err := NewNodeControllerManager(fakeClient, &factoryMock, nil, nil, nodeName, &sync.WaitGroup{}, nil, routeManager, nil)
			Expect(err).NotTo(HaveOccurred())

			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				mutableNetInfo.SetNetworkID(int(staleNetID))
				staleVrfDevice := util.GetNetworkVRFName(mutableNetInfo)
				ovntest.AddVRFLink(staleVrfDevice, uint32(staleNetID))
				_, err = util.GetNetLinkOps().LinkByName(staleVrfDevice)
				Expect(err).NotTo(HaveOccurred())

				mutableNetInfo.SetNetworkID(int(int(netID)))
				validVrfDevice := util.GetNetworkVRFName(mutableNetInfo)
				ovntest.AddVRFLink(validVrfDevice, uint32(netID))
				_, err = util.GetNetLinkOps().LinkByName(validVrfDevice)
				Expect(err).NotTo(HaveOccurred())

				err = ncm.CleanupStaleNetworks(mutableNetInfo)
				Expect(err).NotTo(HaveOccurred())

				// Verify CleanupDeletedNetworks cleans up VRF configuration for
				// already deleted network.
				_, err = util.GetNetLinkOps().LinkByName(staleVrfDevice)
				Expect(err).To(HaveOccurred())

				// Verify CleanupDeletedNetworks didn't cleanup VRF configuration for
				// existing network.
				_, err = util.GetNetLinkOps().LinkByName(validVrfDevice)
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		ovntest.OnSupportedPlatformsIt("check stale mpx devices are cleaned for deleted networks", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			staleMgtPort := fmt.Sprintf("%s%d", types.K8sMgmtIntfNamePrefix, staleNetID)
			fexec := ovntest.NewFakeExec()
			Expect(util.SetExec(fexec)).To(Succeed())
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15" +
					" --if-exists del-port br-int " + staleMgtPort,
			})
			factoryMock := factoryMocks.NodeWatchFactory{}
			netInfo, err := util.ParseNADInfo(nad)
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			Expect(err).NotTo(HaveOccurred())
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%d\"}", netName, netID),
						"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet)},
				},
			}
			nodeList := []*corev1.Node{node}
			factoryMock.On("GetNodeForWindows", nodeName).Return(nodeList[0], nil)
			factoryMock.On("GetNodes").Return(nodeList, nil)
			factoryMock.On("UserDefinedNetworkInformer").Return(nil)
			factoryMock.On("ClusterUserDefinedNetworkInformer").Return(nil)
			factoryMock.On("NamespaceInformer").Return(nil)
			nadListerMock := &nadlistermocks.NetworkAttachmentDefinitionLister{}
			nadInformerMock := &nadinformermocks.NetworkAttachmentDefinitionInformer{}
			nadInformerMock.On("Lister").Return(nadListerMock)
			nadInformerMock.On("Informer").Return(nil)
			factoryMock.On("NADInformer").Return(nadInformerMock)
			nodeListerMock := &corelistermocks.NodeLister{}
			nodeListerMock.On("List", mock.Anything).Return(nodeList, nil)
			nodeInformerMock := &coreinformermocks.NodeInformer{}
			nodeInformerMock.On("Lister").Return(nodeListerMock)
			expectNodeInformer(nodeInformerMock)
			factoryMock.On("NodeCoreInformer").Return(nodeInformerMock)
			expectUplinkInformers(&factoryMock)
			Expect(err).NotTo(HaveOccurred())
			ncm, err := NewNodeControllerManager(fakeClient, &factoryMock, nil, nil, nodeName, &sync.WaitGroup{}, nil, routeManager, nil)
			Expect(err).NotTo(HaveOccurred())

			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				By("Add stale kernel mpx interface")
				ovntest.AddLink(staleMgtPort)

				By("Add active UDN kernel mpx interface")
				validMgtPort := fmt.Sprintf("%s%d", types.K8sMgmtIntfNamePrefix, netID)
				ovntest.AddLink(validMgtPort)

				mutableNetInfo.SetNetworkID(int(netID))

				By("Cleaning up stale networks")
				err = ncm.CleanupStaleNetworks(mutableNetInfo)
				Expect(err).NotTo(HaveOccurred())

				By("Stale mpx interface should have been removed")
				_, err = util.GetNetLinkOps().LinkByName(staleMgtPort)
				var notFoundErr netlink.LinkNotFoundError
				Expect(errors.As(err, &notFoundErr)).To(BeTrue())

				By("Valid mpx interface should NOT have been removed")
				_, err = util.GetNetLinkOps().LinkByName(validMgtPort)
				Expect(err).NotTo(HaveOccurred())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
