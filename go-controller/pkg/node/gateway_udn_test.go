package node

import (
	"fmt"
	"strings"
	"sync"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	coreinformermocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getCreationFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15" +
			" -- --may-exist add-port br-int " + mgtPort +
			" -- set interface " + mgtPort +
			" type=internal mtu_request=" + fmt.Sprintf("%d", mtu) +
			" external-ids:iface-id=" + types.K8sPrefix + netName + "_" + nodeName,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + mgtPort + " mac_in_use",
		Output: mgtPortMAC,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 set interface " + mgtPort + " " + fmt.Sprintf("mac=%s", strings.ReplaceAll(mgtPortMAC, ":", "\\:")),
	})
}

func getDeletionFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort string) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mgtPort,
	})
}

var _ = Describe("UserDefinedNetworkGateway", func() {
	var (
		netName             = "bluenet"
		netID               = "3"
		nodeName     string = "worker1"
		mgtPortMAC   string = "00:00:00:55:66:77" // dummy MAC used for fake commands
		fexec        *ovntest.FakeExec
		testNS       ns.NetNS
		factoryMock  factoryMocks.NodeWatchFactory
		kubeMock     kubemocks.Interface
		nodeLister   v1mocks.NodeLister
		vrf          *vrfmanager.Controller
		wg           sync.WaitGroup
		stopCh       chan struct{}
		v4NodeSubnet = "100.128.0.0/24"
		v6NodeSubnet = "ae70::66/112"
		mgtPort      = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		Expect(util.SetExec(fexec)).To(Succeed())
		// Set up a fake k8sMgmt interface
		var err error
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			// given the netdevice is created using add-port command in OVS
			// we need to mock create a dummy link for things to work in unit tests
			ovntest.AddLink(mgtPort)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		factoryMock = factoryMocks.NodeWatchFactory{}
		nodeInformer := coreinformermocks.NodeInformer{}
		factoryMock.On("NodeCoreInformer").Return(&nodeInformer)
		nodeLister = v1mocks.NodeLister{}
		nodeInformer.On("Lister").Return(&nodeLister)
		kubeMock = kubemocks.Interface{}
		wg = sync.WaitGroup{}
		stopCh = make(chan struct{})
		vrf = vrfmanager.NewController()
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer GinkgoRecover()
			err = vrf.Run(stopCh, &wg)
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
	})
	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		cnode.Annotations[util.OvnNodeManagementPortMacAddresses] = `{"bluenet":"00:00:00:55:66:77"}`
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/24"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/112"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L3 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids":  fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
					"k8s.ovn.org/node-subnets": fmt.Sprintf("{\"%s\":[\"%s\", \"%s\"]}", netName, v4NodeSubnet, v6NodeSubnet),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16/24,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			Expect(udnGateway.deleteUDNManagementPort()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
		config.IPv6Mode = true
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		cnode.Annotations[util.OvnNodeManagementPortMacAddresses] = `{"bluenet":"00:00:00:55:66:77"}`
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			mpLink, err := udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			Expect(mpLink).NotTo(BeNil())
			exists, err := util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("100.128.0.2/16"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			exists, err = util.LinkAddrExist(mpLink, ovntest.MustParseIPNet("ae70::2/60"))
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should delete management port for a L2 user defined network", func() {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					"k8s.ovn.org/network-ids": fmt.Sprintf("{\"%s\": \"%s\"}", netName, netID),
				},
			},
		}
		nad := ovntest.GenerateNAD(netName, "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16,ae70::66/60", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		udnGateway, err := NewUserDefinedNetworkGateway(netInfo, 3, node, factoryMock.NodeCoreInformer().Lister(), &kubeMock, vrf, &gateway{})
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		nodeLister.On("Get", mock.AnythingOfType("string")).Return(node, nil)
		factoryMock.On("GetNode", "worker1").Return(node, nil)
		cnode := node.DeepCopy()
		kubeMock.On("UpdateNodeStatus", cnode).Return(nil) // check if network key gets deleted from annotation
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			Expect(udnGateway.deleteUDNManagementPort()).To(Succeed())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})
