package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type secondaryNetInfo struct {
	netName              string
	nadName              string
	podIP                string
	podMAC               string
	topology             string
	disablePortSecurity  bool
	allowUnkownL2Traffic bool
}

const (
	dummyMACAddr         = "02:03:04:05:06:07"
	nadName              = "blue-net"
	ns                   = "namespace1"
	secondaryNetworkName = "isolatednet"
)

var _ = Describe("OVN Multi-Homed pod operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed()) // reset defaults

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: nodeName,
				},
			},
		}
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	table.DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo, expectationOptions ...option) {
			podInfo := dummyTestPod(ns, netInfo)
			app.Action = func(ctx *cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				fakeOvn.startWithDBSetup(
					initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(ns),
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*newNode(nodeName, "192.168.126.202/24"),
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newMultiHomedPod(podInfo.namespace, podInfo.podName, podInfo.nodeName, podInfo.podIP, netInfo),
						},
					},
					&nadapi.NetworkAttachmentDefinitionList{
						Items: []nadapi.NetworkAttachmentDefinition{*nad},
					},
				)
				podInfo.populateLogicalSwitchCache(fakeOvn)

				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.TODO(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// check that after start networks annotations and nbdb will be updated
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(
						append(
							getExpectedDataPodsAndSwitches([]testPod{podInfo}, []string{nodeName}),
							newSecondaryNetworkExpectationMachine(
								fakeOvn,
								[]testPod{podInfo},
								expectationOptions...,
							).expectedLogicalSwitchesAndPorts()...)))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod with port security and not allowing unknown MAC addresses",
			dummySecondaryNetInfo(),
		),

		table.Entry("pod without port security, and not allowing unknown MAC addresses",
			dummySecondaryNetInfoWithoutPortSec(),
			withoutPortSecurity(),
		),

		table.Entry("pod with port security, and allowing unknown addresses",
			dummySecondaryNetInfoAllowingL2Traffic(),
			allowSendingToUnknownL2Addrs(),
		),

		table.Entry("pod without port security and allowing unknown addresses",
			dummySecondaryNetInfoWithoutPortSecAndAllowingL2Traffic(),
			withoutPortSecurity(),
			allowSendingToUnknownL2Addrs(),
		),
	)
})

func newMultiHomedTestPod(
	nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string,
	multiHomingConfigs ...secondaryNetInfo,
) testPod {
	pod := newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace)
	for _, multiHomingConf := range multiHomingConfigs {
		pod.addNetwork(
			multiHomingConf.netName,
			multiHomingConf.nadName,
			"",
			"",
			"",
			multiHomingConf.podIP,
			multiHomingConf.podMAC,
			0,
		)
	}
	return pod
}

func newMultiHomedPod(namespace, name, node, podIP string, multiHomingConfigs ...secondaryNetInfo) *v1.Pod {
	pod := newPod(namespace, name, node, podIP)
	var secondaryNetworks []nadapi.NetworkSelectionElement
	for _, multiHomingConf := range multiHomingConfigs {
		nadNamePair := strings.Split(multiHomingConf.nadName, "/")
		ns := pod.Namespace
		attachmentName := multiHomingConf.nadName
		if len(nadNamePair) > 1 {
			ns = nadNamePair[0]
			attachmentName = nadNamePair[1]
		}
		nse := nadapi.NetworkSelectionElement{
			Name:       attachmentName,
			Namespace:  ns,
			MacRequest: multiHomingConf.podMAC,
		}
		if len(multiHomingConf.podIP) > 0 {
			nse.IPRequest = []string{multiHomingConf.podIP + "/24"}
		}
		secondaryNetworks = append(secondaryNetworks, nse)
	}
	serializedNetworkSelectionElements, _ := json.Marshal(secondaryNetworks)
	pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: string(serializedNetworkSelectionElements)}
	return pod
}

func namespacedName(ns, name string) string { return fmt.Sprintf("%s/%s", ns, name) }

func (sni *secondaryNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup) error {
	netInfo, err := util.NewNetInfo(sni.netconf())
	if err != nil {
		return err
	}

	switch sni.topology {
	case ovntypes.Layer2Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch),
			UUID:        netInfo.GetNetworkScopedName(ovntypes.OVNLayer2Switch) + "_UUID",
			ExternalIDs: map[string]string{ovntypes.NetworkExternalID: sni.netName},
		})
	case ovntypes.LocalnetTopology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch),
			UUID:        netInfo.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch) + "_UUID",
			ExternalIDs: map[string]string{ovntypes.NetworkExternalID: sni.netName},
		})
	default:
		return fmt.Errorf("missing topology in the network configuration: %v", sni)
	}
	return nil
}

func (sni *secondaryNetInfo) netconf(subnets ...string) *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"
	return &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology:            sni.topology,
		NADName:             sni.nadName,
		Subnets:             strings.Join(subnets, ","),
		DisablePortSecurity: sni.disablePortSecurity,
		EnableL2Unknown:     sni.allowUnkownL2Traffic,
	}
}

func dummyTestPod(nsName string, info ...secondaryNetInfo) testPod {
	return newMultiHomedTestPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod",
		"10.128.1.3",
		"0a:58:0a:80:01:03",
		nsName,
		info...,
	)
}

func dummySecondaryNetInfo() secondaryNetInfo {
	return secondaryNetInfo{
		netName:  secondaryNetworkName,
		nadName:  namespacedName(ns, nadName),
		podMAC:   dummyMACAddr,
		podIP:    "192.168.200.10",
		topology: ovntypes.Layer2Topology,
	}
}

func dummySecondaryNetInfoWithoutPortSec() secondaryNetInfo {
	secondaryNet := dummySecondaryNetInfo()
	secondaryNet.disablePortSecurity = true
	return secondaryNet
}

func dummySecondaryNetInfoAllowingL2Traffic() secondaryNetInfo {
	secondaryNet := dummySecondaryNetInfo()
	secondaryNet.allowUnkownL2Traffic = true
	return secondaryNet
}

func dummySecondaryNetInfoWithoutPortSecAndAllowingL2Traffic() secondaryNetInfo {
	secondaryNet := dummySecondaryNetInfo()
	secondaryNet.disablePortSecurity = true
	secondaryNet.allowUnkownL2Traffic = true
	return secondaryNet
}
