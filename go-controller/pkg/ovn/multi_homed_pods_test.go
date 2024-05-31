package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
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
	netName  string
	nadName  string
	podIP    string
	podMAC   string
	topology string
}

var _ = Describe("OVN Multi-Homed pod operations", func() {
	const (
		nadName              = "blue-net"
		nodeName             = "node1"
		ns                   = "namespace1"
		secondaryNetworkName = "isolatednet"
	)
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

	It("reconciles a new pod", func() {
		app.Action = func(ctx *cli.Context) error {
			namespaceT := *newNamespace(ns)
			isolatedNetInfo := secondaryNetInfo{
				netName:  secondaryNetworkName,
				nadName:  namespacedName(ns, nadName),
				podMAC:   "02:03:04:05:06:07",
				podIP:    "192.168.200.10",
				topology: ovntypes.Layer2Topology,
			}
			t := newMultiHomedTestPod(
				nodeName,
				"10.128.1.0/24",
				"10.128.1.2",
				"10.128.1.1",
				"myPod",
				"10.128.1.3",
				"0a:58:0a:80:01:03",
				namespaceT.Name,
				isolatedNetInfo,
			)

			netConf := newNetconf(
				isolatedNetInfo.netName,
				isolatedNetInfo.topology,
				isolatedNetInfo.nadName,
			)
			nad, err := newNetworkAttachmentDefinition(
				ns,
				nadName,
				netConf,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(isolatedNetInfo.setupOVNDependencies(&initialDB, netConf)).To(Succeed())

			fakeOvn.startWithDBSetup(
				initialDB,
				&v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				},
				&v1.NodeList{
					Items: []v1.Node{
						*newNode(nodeName, "192.168.126.202/24"),
					},
				},
				&v1.PodList{
					Items: []v1.Pod{
						*newMultiHomedPod(t.namespace, t.podName, t.nodeName, t.podIP, isolatedNetInfo),
					},
				},
				&nadapi.NetworkAttachmentDefinitionList{
					Items: []nadapi.NetworkAttachmentDefinition{*nad},
				},
			)
			t.populateLogicalSwitchCache(fakeOvn)

			// pod exists, networks annotations don't
			pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, ok := pod.Annotations[util.OvnPodAnnotationName]
			Expect(ok).To(BeFalse())

			Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
			Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
			secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
			Expect(ok).To(BeTrue())

			t.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
			Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
			Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

			// check that after start networks annotations and nbdb will be updated
			Eventually(func() string {
				return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
			}).WithTimeout(2 * time.Second).Should(MatchJSON(t.getAnnotationsJson()))

			Eventually(fakeOvn.nbClient).Should(
				libovsdbtest.HaveData(
					append(
						getExpectedDataPodsAndSwitches([]testPod{t}, []string{nodeName}),
						getExpectedDataPodsAndSwitchesForSecondaryNetwork(fakeOvn, []testPod{t})...)))
			return nil
		}

		Expect(app.Run([]string{app.Name})).To(Succeed())
	})
})

func newNetconf(networkName, topology, nadName string, subnets ...string) ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"
	return ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: networkName,
			Type: plugin,
		},
		Topology: topology,
		NADName:  nadName,
		Subnets:  strings.Join(subnets, ","),
	}
}

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
		ns := namespace
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

func (sni *secondaryNetInfo) setupOVNDependencies(dbData *libovsdbtest.TestSetup, netConf ovncnitypes.NetConf) error {
	netInfo, err := util.NewNetInfo(&netConf)
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
