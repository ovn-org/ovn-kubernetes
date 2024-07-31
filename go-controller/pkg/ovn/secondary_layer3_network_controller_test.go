package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type secondaryNetInfo struct {
	netName   string
	nadName   string
	subnets   string
	topology  string
	isPrimary bool
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

		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	table.DescribeTable(
		"reconciles a new",
		func(netInfo secondaryNetInfo) {
			podInfo := dummyTestPod(ns, netInfo)
			app.Action = func(ctx *cli.Context) error {
				nad, err := newNetworkAttachmentDefinition(
					ns,
					nadName,
					*netInfo.netconf(),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(netInfo.setupOVNDependencies(&initialDB)).To(Succeed())

				if netInfo.isPrimary {
					initialDB.NBData = append(
						initialDB.NBData,
						&nbdb.LogicalSwitch{
							Name: fmt.Sprintf("%s_join", netInfo.netName),
						},
						&nbdb.LogicalRouter{
							Name: fmt.Sprintf("%s_ovn_cluster_router", netInfo.netName),
						},
						&nbdb.LogicalRouterPort{
							Name: fmt.Sprintf("rtos-%s_%s", netInfo.netName, nodeName),
						},
					)
				}

				testNode, err := newNodeWithSecondaryNets(nodeName, "192.168.126.202/24", netInfo)
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.startWithDBSetup(
					initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							*newNamespace(ns),
						},
					},
					&v1.NodeList{
						Items: []v1.Node{*testNode},
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
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podInfo.namespace).Get(context.Background(), podInfo.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				Expect(fakeOvn.controller.WatchNamespaces()).NotTo(HaveOccurred())
				Expect(fakeOvn.controller.WatchPods()).NotTo(HaveOccurred())
				secondaryNetController, ok := fakeOvn.secondaryControllers[secondaryNetworkName]
				Expect(ok).To(BeTrue())

				secondaryNetController.bnc.ovnClusterLRPToJoinIfAddrs = dummyJoinIPs()
				podInfo.populateSecondaryNetworkLogicalSwitchCache(fakeOvn, secondaryNetController)
				Expect(secondaryNetController.bnc.WatchNodes()).To(Succeed())
				Expect(secondaryNetController.bnc.WatchPods()).To(Succeed())

				// assert the OVN annotations on the pod are the expected ones
				Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, podInfo.namespace, podInfo.podName)
				}).WithTimeout(2 * time.Second).Should(MatchJSON(podInfo.getAnnotationsJson()))

				return nil
			}

			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		table.Entry("pod on a user defined secondary network",
			dummySecondaryUserDefinedNetwork("192.168.0.0/16"),
		),

		table.Entry("pod on a user defined primary network",
			dummyPrimaryUserDefinedNetwork("192.168.0.0/16"),
		),
	)

})

func newPodWithPrimaryUDN(
	nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIPs, podMAC, namespace string,
	primaryUDNConfig secondaryNetInfo,
) testPod {
	pod := newTPod(nodeName, nodeSubnet, nodeMgtIP, "", podName, podIPs, podMAC, namespace)
	if primaryUDNConfig.isPrimary {
		pod.networkRole = "infrastructure-locked"
		pod.routes = append(
			pod.routes,
			util.PodRoute{
				Dest:    testing.MustParseIPNet("10.128.0.0/14"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
			util.PodRoute{
				Dest:    testing.MustParseIPNet("100.64.0.0/16"),
				NextHop: testing.MustParseIP("10.128.1.1"),
			},
		)
	}
	pod.addNetwork(
		primaryUDNConfig.netName,
		primaryUDNConfig.nadName,
		primaryUDNConfig.subnets,
		"",
		nodeGWIP,
		"192.168.0.3/16",
		"0a:58:c0:a8:00:03",
		"primary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
			{
				Dest:    testing.MustParseIPNet("172.16.1.0/24"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
			{
				Dest:    testing.MustParseIPNet("100.65.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
		},
	)
	return pod
}

func newMultiHomedPod(namespace, name, node, podIP string, multiHomingConfigs ...secondaryNetInfo) *v1.Pod {
	pod := newPod(namespace, name, node, podIP)
	var secondaryNetworks []nadapi.NetworkSelectionElement
	for _, multiHomingConf := range multiHomingConfigs {
		if multiHomingConf.isPrimary {
			continue // these will be automatically plugged in
		}
		nadNamePair := strings.Split(multiHomingConf.nadName, "/")
		ns := pod.Namespace
		attachmentName := multiHomingConf.nadName
		if len(nadNamePair) > 1 {
			ns = nadNamePair[0]
			attachmentName = nadNamePair[1]
		}
		nse := nadapi.NetworkSelectionElement{
			Name:      attachmentName,
			Namespace: ns,
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
	case ovntypes.Layer3Topology:
		dbData.NBData = append(dbData.NBData, &nbdb.LogicalSwitch{
			Name:        netInfo.GetNetworkScopedName(nodeName),
			UUID:        netInfo.GetNetworkScopedName(nodeName) + "_UUID",
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

func (sni *secondaryNetInfo) netconf() *ovncnitypes.NetConf {
	const plugin = "ovn-k8s-cni-overlay"

	role := ovntypes.NetworkRoleSecondary
	if sni.isPrimary {
		role = ovntypes.NetworkRolePrimary
	}
	return &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: sni.netName,
			Type: plugin,
		},
		Topology: sni.topology,
		NADName:  sni.nadName,
		Subnets:  sni.subnets,
		Role:     role,
	}
}

func dummyTestPod(nsName string, info secondaryNetInfo) testPod {
	const nodeSubnet = "10.128.1.0/24"
	if info.isPrimary {
		return newPodWithPrimaryUDN(
			nodeName,
			nodeSubnet,
			"10.128.1.2",
			"192.168.0.1",
			"myPod",
			"10.128.1.3",
			"0a:58:0a:80:01:03",
			nsName,
			info,
		)
	}
	pod := newTPod(nodeName, nodeSubnet, "10.128.1.2", "10.128.1.1", podName, "10.128.1.3", "0a:58:0a:80:01:03", nsName)
	pod.addNetwork(
		info.netName,
		info.nadName,
		info.subnets,
		"",
		"",
		"192.168.0.3/16",
		"0a:58:c0:a8:00:03",
		"secondary",
		0,
		[]util.PodRoute{
			{
				Dest:    testing.MustParseIPNet("192.168.0.0/16"),
				NextHop: testing.MustParseIP("192.168.0.1"),
			},
		},
	)
	return pod
}

func dummySecondaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	return secondaryNetInfo{
		netName:  secondaryNetworkName,
		nadName:  namespacedName(ns, nadName),
		topology: ovntypes.Layer3Topology,
		subnets:  subnets,
	}
}

func dummyPrimaryUserDefinedNetwork(subnets string) secondaryNetInfo {
	secondaryNet := dummySecondaryUserDefinedNetwork(subnets)
	secondaryNet.isPrimary = true
	return secondaryNet
}

func (sni *secondaryNetInfo) String() string {
	return fmt.Sprintf("%q: %q", sni.netName, sni.subnets)
}

func newNodeWithSecondaryNets(nodeName string, nodeIPv4CIDR string, netInfos ...secondaryNetInfo) (*v1.Node, error) {
	var nodeSubnetInfo []string
	for _, info := range netInfos {
		nodeSubnetInfo = append(nodeSubnetInfo, info.String())
	}

	nodeIP, nodeCIDR, err := net.ParseCIDR(nodeIPv4CIDR)
	if err != nil {
		return nil, err
	}
	nextHopIP := util.GetNodeGatewayIfAddr(nodeCIDR).IP
	nodeCIDR.IP = nodeIP

	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr":      fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", nodeIPv4CIDR, ""),
				"k8s.ovn.org/node-subnets":             fmt.Sprintf("{\"default\":\"%s\", %s}", v4Node1Subnet, strings.Join(nodeSubnetInfo, ",")),
				util.OVNNodeHostCIDRs:                  fmt.Sprintf("[\"%s\"]", nodeIPv4CIDR),
				"k8s.ovn.org/zone-name":                "global",
				"k8s.ovn.org/l3-gateway-config":        fmt.Sprintf("{\"default\":{\"mode\":\"shared\",\"bridge-id\":\"breth0\",\"interface-id\":\"breth0_ovn-worker\",\"mac-address\":%q,\"ip-addresses\":[%[2]q],\"ip-address\":%[2]q,\"next-hops\":[%[3]q],\"next-hop\":%[3]q,\"node-port-enable\":\"true\",\"vlan-id\":\"0\"}}", util.IPAddrToHWAddr(nodeIP), nodeCIDR, nextHopIP),
				util.OvnNodeChassisID:                  "abdcef",
				"k8s.ovn.org/network-ids":              "{\"default\":\"0\",\"isolatednet\":\"2\"}",
				util.OvnNodeManagementPortMacAddresses: fmt.Sprintf("{\"isolatednet\":%q}", dummyMACAddr),
			},
			Labels: map[string]string{
				"k8s.ovn.org/egress-assignable": "",
			},
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}, nil
}

func dummyJoinIPs() []*net.IPNet {
	return []*net.IPNet{dummyJoinIP()}
}

func dummyJoinIP() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.13"),
		Mask: net.CIDRMask(24, 32),
	}
}
func dummyMasqueradeSubnet() *net.IPNet {
	return &net.IPNet{
		IP:   net.ParseIP("169.254.169.0"),
		Mask: net.CIDRMask(24, 32),
	}
}
