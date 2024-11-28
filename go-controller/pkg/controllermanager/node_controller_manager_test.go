package controllermanager

import (
	"fmt"
	"sync"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	nadinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlistermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	corelistermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/stretchr/testify/mock"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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

func genDeleteStaleRepPortCmd(iface string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists --with-iface del-port %s", iface)
}

func genFindInterfaceWithSandboxCmd() string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --columns=name,external_ids --data=bare --no-headings " +
		"--format=csv find Interface external_ids:sandbox!=\"\" external_ids:vf-netdev-name!=\"\"")
}

var _ = Describe("Healthcheck tests", func() {
	var execMock *ovntest.FakeExec
	var factoryMock factoryMocks.NodeWatchFactory
	var fakeClient *util.OVNClientset
	var err error

	BeforeEach(func() {
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

	Describe("checkForStaleOVSRepresentorInterfaces", func() {
		var ncm *NodeControllerManager
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

		BeforeEach(func() {
			// setup kube output
			factoryMock.On("NADInformer").Return(nil)
			ncm, err = NewNodeControllerManager(fakeClient, &factoryMock, nodeName, &sync.WaitGroup{}, nil, routeManager)
			Expect(err).NotTo(HaveOccurred())
			factoryMock.On("GetPods", "").Return(podList, nil)
		})

		Context("bridge has stale representor ports", func() {
			It("removes stale VF rep ports from bridge", func() {
				// mock call to find OVS interfaces with non-empty external_ids:sandbox
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod iface-id-ver=pod-a-uuid-1 vf-netdev-name=blah\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod iface-id-ver=pod-b-uuid-2 vf-netdev-name=blah\n" +
						"stale-pod-ifc,sandbox=123abcfaa iface-id=stale-ns_stale-pod iface-id-ver=pod-stale-uuid-3 vf-netdev-name=blah\n",
					Err: nil,
				})
				// mock calls to remove only stale-port
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genDeleteStaleRepPortCmd("stale-pod-ifc"),
					Output: "",
					Err:    nil,
				})
				ncm.checkForStaleOVSRepresentorInterfaces()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

		Context("bridge does not have stale representor ports", func() {
			It("does not remove any port from bridge", func() {
				// ports in br-int
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod iface-id-ver=pod-a-uuid-1 vf-netdev-name=blah\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod iface-id-ver=pod-b-uuid-2 vf-netdev-name=blah\n",
					Err: nil,
				})
				ncm.checkForStaleOVSRepresentorInterfaces()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

	})

	Context("verify cleanup of deleted networks", func() {
		var (
			staleNetID uint   = 1000
			nodeName   string = "worker1"
			nad               = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
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
				KubeClient: fake.NewSimpleClientset(v1Objects...),
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
			factoryMock.On("GetNode", nodeName).Return(nodeList[0], nil)
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
			factoryMock.On("NodeCoreInformer").Return(nodeInformerMock)

			ncm, err := NewNodeControllerManager(fakeClient, &factoryMock, nodeName, &sync.WaitGroup{}, nil, routeManager)
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
	})
})
