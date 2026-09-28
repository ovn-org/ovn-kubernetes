// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"encoding/json"
	"net"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OVN MAC security mode LSP configuration", func() {
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

		fakeOvn = NewFakeOVN(false, nodeName)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}

		config.OVNKubernetesFeature = *minimalFeatureConfig()
		config.Gateway.V4MasqueradeSubnet = dummyMasqueradeSubnet().String()
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	DescribeTable("configures the pod's logical switch port according to macSecurityMode",
		func(role, topology, macSecMode string) {
			const podMAC = "0a:58:0a:80:01:03"
			const netName = "test-mac-security"

			node, err := newNodeWithUserDefinedNetworks(nodeName, "192.168.126.202/24")
			Expect(err).NotTo(HaveOccurred())
			testNS := newUDNNamespace("test-mac-security")
			nadKey := util.GetNADName(testNS.Name, netName)
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: netName,
					Type: "ovn-k8s-cni-overlay",
				},
				Topology:        topology,
				Role:            role,
				NADName:         nadKey,
				MACSecurityMode: macSecMode,
			}
			nad, err := newNetworkAttachmentDefinition(testNS.Name, netName, *netConf)
			Expect(err).NotTo(HaveOccurred())

			mac, err := net.ParseMAC(podMAC)
			Expect(err).NotTo(HaveOccurred())
			nse, err := json.Marshal([]nadapi.NetworkSelectionElement{{Name: netName, Namespace: testNS.Name}})
			Expect(err).NotTo(HaveOccurred())
			pod := testing.NewPod(testNS.Name, "mac-security-pod", nodeName, "")
			pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: string(nse)}
			pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
				MAC:      mac,
				TunnelID: 5,
				Role:     role,
			}, nadKey)
			Expect(err).NotTo(HaveOccurred())

			app.Action = func(*cli.Context) error {
				nbZone := &nbdb.NBGlobal{Name: nodeName, UUID: nodeName}
				initialDB.NBData = append(initialDB.NBData, nbZone)
				fakeOvn.startWithDBSetup(
					initialDB,
					&corev1.NodeList{Items: []corev1.Node{*node}},
					&corev1.NamespaceList{Items: []corev1.Namespace{*testNS}},
					&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				Expect(fakeOvn.networkManager.Start()).To(Succeed())
				defer fakeOvn.networkManager.Stop()

				// init() alone creates the network's logical switch (and seeds the
				// logical switch manager cache) for both layer2 and localnet secondary
				// controllers; node reconciliation is not needed for this scenario.
				switch topology {
				case ovntypes.Layer2Topology:
					l2Controller, ok := fakeOvn.fullL2UDNControllers[netName]
					Expect(ok).To(BeTrueBecause("should have l2 UDN controller for l2 topology NAD"))
					Expect(l2Controller.init(context.Background())).To(Succeed())
				case ovntypes.LocalnetTopology:
					localnetController, ok := fakeOvn.fullLocalnetUDNControllers[netName]
					Expect(ok).To(BeTrueBecause("should have localnet UDN controller for localnet topology NAD"))
					Expect(localnetController.init()).To(Succeed())
				default:
					Fail("unsupported topology for this test: " + topology)
				}

				udnNetController, ok := fakeOvn.userDefinedNetworkControllers[netName]
				Expect(ok).To(BeTrueBecause("should have UDN controller for UDN NAD"))
				Expect(udnNetController.bnc.WatchNamespaces()).To(Succeed())
				Expect(udnNetController.bnc.WatchPods()).To(Succeed())

				lspName := udnNetController.bnc.GetLogicalPortName(pod, nadKey)
				Eventually(func(g Gomega) {
					lsp, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
					g.Expect(err).NotTo(HaveOccurred())
					if macSecMode == ovntypes.MACSecurityModeDisabled {
						g.Expect(lsp.Addresses).To(ConsistOf(podMAC, "unknown"))
						g.Expect(lsp.PortSecurity).To(BeEmpty())
					} else {
						g.Expect(lsp.Addresses).To(ConsistOf(podMAC))
						g.Expect(lsp.PortSecurity).To(ConsistOf(podMAC))
					}
				}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		},
		Entry("layer2, secondary, macSecurityMode unset (behavior unchanged)",
			ovntypes.NetworkRoleSecondary, ovntypes.Layer2Topology, ""),
		Entry("layer2, secondary, macSecurityMode enabled (behavior unchanged)",
			ovntypes.NetworkRoleSecondary, ovntypes.Layer2Topology, ovntypes.MACSecurityModeEnabled),
		Entry("layer2, secondary, macSecurityMode disabled",
			ovntypes.NetworkRoleSecondary, ovntypes.Layer2Topology, ovntypes.MACSecurityModeDisabled),
		Entry("localnet, secondary, macSecurityMode unset (behavior unchanged)",
			ovntypes.NetworkRoleSecondary, ovntypes.LocalnetTopology, ""),
		Entry("localnet, secondary, macSecurityMode enabled (behavior unchanged)",
			ovntypes.NetworkRoleSecondary, ovntypes.LocalnetTopology, ovntypes.MACSecurityModeEnabled),
		Entry("localnet, secondary, macSecurityMode disabled",
			ovntypes.NetworkRoleSecondary, ovntypes.LocalnetTopology, ovntypes.MACSecurityModeDisabled),
	)
})
