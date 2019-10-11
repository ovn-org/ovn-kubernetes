package controller

import (
	"net"
	"strings"

	"github.com/urfave/cli"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func mustParseCIDR(cidr string) *net.IPNet {
	_, net, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("bad CIDR string constant " + cidr)
	}
	return net
}

func addGetPortAddressesCmds(fexec *ovntest.FakeExec, nodeName, hybMAC, hybIP string) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovn-nbctl --timeout=15 get logical_switch_port int-" + nodeName + " dynamic_addresses",
		// hybrid overlay ports have static addresses
		Output: "[]",
	})
	addresses := hybMAC + " " + hybIP
	addresses = strings.TrimSpace(addresses)
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: "ovn-nbctl --timeout=15 get logical_switch_port int-" + nodeName + " addresses",
		// hybrid overlay ports have static addresses
		Output: "[" + addresses + "]",
	})
}

func newTestNode(name, os, ovnHostSubnet, hybridHostSubnet string) v1.Node {
	annotations := make(map[string]string)
	if ovnHostSubnet != "" {
		annotations[ovn.OvnHostSubnet] = ovnHostSubnet
	}
	if hybridHostSubnet != "" {
		annotations[types.HybridOverlayHostSubnet] = hybridHostSubnet
	}
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{v1.LabelOSStable: os},
			Annotations: annotations,
		},
	}
}

var _ = Describe("Hybrid SDN Master Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	const extIPNet string = "11.1.0.0"

	extCIDR := []config.CIDRNetworkEntry{
		{
			CIDR:             mustParseCIDR(extIPNet + "/16"),
			HostSubnetLength: 24,
		},
	}

	It("allocates and assigns an hybrid-overlay HostSubnet to a Windows node that doesn't have one", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "11.1.0.0/24"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "windows", "", ""),
				},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			m, err := NewMaster(fakeClient, extCIDR)
			Expect(err).NotTo(HaveOccurred())

			err = m.Start(f)
			Expect(err).NotTo(HaveOccurred())

			// Windows node should be allocated a subnet
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(types.HybridOverlayHostSubnet, nodeSubnet))

			Expect(fexec.CalledMatchesExpected()).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cleans up a Linux node without an OVN hostsubnet annotation", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName string = "node1"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", "", ""),
				},
			})

			fexec := ovntest.NewFakeExec()
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			m, err := NewMaster(fakeClient, extCIDR)
			Expect(err).NotTo(HaveOccurred())

			err = m.Start(f)
			Expect(err).NotTo(HaveOccurred())

			// Linux node (without OVN subnet annotation) should not have an hybrid overlay subnet annotation
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).NotTo(HaveKey(types.HybridOverlayHostSubnet))

			Consistently(fexec.CalledMatchesExpected()).Should(BeTrue())

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sets up a Linux node with a OVN hostsubnet annotation but without a hybrid overlay hostsubnet annotation", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, ""),
				},
			})

			fexec := ovntest.NewFakeExec()
			addGetPortAddressesCmds(fexec, nodeName, nodeHOMAC, nodeHOIP)

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			m, err := NewMaster(fakeClient, extCIDR)
			Expect(err).NotTo(HaveOccurred())

			err = m.Start(f)
			Expect(err).NotTo(HaveOccurred())

			// Linux node #1 should have the same hybrid overlay subnet annotation
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(types.HybridOverlayHostSubnet, nodeSubnet))

			Expect(fexec.CalledMatchesExpected()).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("leaves a Linux node's matching OVN hostsubnet annotation and hybrid overlay hostsubnet annotation unchanged", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, nodeSubnet),
				},
			})

			fexec := ovntest.NewFakeExec()
			addGetPortAddressesCmds(fexec, nodeName, nodeHOMAC, nodeHOIP)

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			m, err := NewMaster(fakeClient, extCIDR)
			Expect(err).NotTo(HaveOccurred())

			err = m.Start(f)
			Expect(err).NotTo(HaveOccurred())

			// Linux node #1 should have the same hybrid overlay subnet annotation
			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(types.HybridOverlayHostSubnet, nodeSubnet))

			Expect(fexec.CalledMatchesExpected()).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})

	It("removes a Linux node's hybrid overlay hostsubnet annotation when the OVN hostsubnet annotation disappears", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName   string = "node1"
				nodeSubnet string = "10.1.2.0/24"
				nodeHOIP   string = "10.1.2.3"
				nodeHOMAC  string = "00:00:00:52:19:d2"
			)

			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{
					newTestNode(nodeName, "linux", nodeSubnet, nodeSubnet),
				},
			})

			fexec := ovntest.NewFakeExec()
			addGetPortAddressesCmds(fexec, nodeName, nodeHOMAC, nodeHOIP)
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 -- --if-exists lsp-del int-node1",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			m, err := NewMaster(fakeClient, extCIDR)
			Expect(err).NotTo(HaveOccurred())

			err = m.Start(f)
			Expect(err).NotTo(HaveOccurred())

			kube := &kube.Kube{KClient: fakeClient}
			updatedNode, err := kube.GetNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			err = kube.DeleteAnnotationOnNode(updatedNode, "ovn_host_subnet")
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() map[string]string {
				updatedNode, err = kube.GetNode(nodeName)
				Expect(err).NotTo(HaveOccurred())
				return updatedNode.Annotations
			}, 5, 1).ShouldNot(HaveKey(types.HybridOverlayHostSubnet))

			Expect(fexec.CalledMatchesExpected()).Should(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
