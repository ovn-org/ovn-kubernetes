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

	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/vrfmanager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getCreationFakeOVSCommands(fexec *ovntest.FakeExec, mgtPort, mgtPortMAC, netName, nodeName string, mtu int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovs-vsctl --timeout=15 -- --if-exists del-port br-int " + mgtPort +
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
		netName                  = "bluenet"
		netID                    = "3"
		nodeName          string = "worker1"
		mgtPortMAC        string = "00:00:00:55:66:77"
		fexec             *ovntest.FakeExec
		testNS            ns.NetNS
		nodeAnnotatorMock *kubeMocks.Annotator
		vrf               *vrfmanager.Controller
		v4NodeSubnet      = "10.128.0.0/24"
		v6NodeSubnet      = "ae70::66/112"
		mgtPort           = fmt.Sprintf("%s%s", types.K8sMgmtIntfNamePrefix, netID)
	)
	BeforeEach(func() {
		// Set up a fake vsctl command mock interface
		fexec = ovntest.NewFakeExec()
		err := util.SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())
		// Set up a fake k8sMgmt interface
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			ovntest.AddLink(mgtPort)
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		nodeAnnotatorMock = &kubeMocks.Annotator{}
		wg := &sync.WaitGroup{}
		vrf = vrfmanager.NewController()
		stopCh := make(chan struct{})
		defer func() {
			close(stopCh)
			wg.Wait()
		}()
		wg.Add(1)
		go testNS.Do(func(netNS ns.NetNS) error {
			defer wg.Done()
			defer GinkgoRecover()
			vrf.Run(stopCh, wg)
			return nil
		})
	})
	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
		Expect(testutils.UnmountNS(testNS)).To(Succeed())
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L3 user defined network", func() {
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
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{netName: mgtPortMAC}).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nodeAnnotatorMock, vrf)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

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
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.deleteUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
	ovntest.OnSupportedPlatformsIt("should create management port for a L2 user defined network", func() {
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
		nodeAnnotatorMock.On("Set", mock.Anything, map[string]string{netName: mgtPortMAC}).Return(nil)
		nodeAnnotatorMock.On("Run").Return(nil)
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nodeAnnotatorMock, vrf)
		Expect(err).NotTo(HaveOccurred())
		getCreationFakeOVSCommands(fexec, mgtPort, mgtPortMAC, netName, nodeName, netInfo.MTU())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.addUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

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
		udnGateway := NewUserDefinedNetworkGateway(netInfo, 3, node, nil, vrf)
		Expect(err).NotTo(HaveOccurred())
		getDeletionFakeOVSCommands(fexec, mgtPort)
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()
			err = udnGateway.deleteUDNManagementPort()
			Expect(err).NotTo(HaveOccurred())
			return nil
		})

		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})
