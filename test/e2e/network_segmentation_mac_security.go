// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/kubernetes/test/e2e/framework"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
)

var _ = Describe("Network Segmentation: MAC security", feature.NetworkSegmentation, feature.MACSecurity, func() {
	f := wrappedTestFramework("network-segmentation-mac-sec")

	// test cases for layer2 secondary network
	layer2Cases := []TableEntry{
		Entry("over secondary layer2, when mac-security enabled, should fail",
			networkAttachmentConfigParams{
				role:               "secondary",
				topology:           "layer2",
				disableMACSecurity: false,
			},
			assertConnectivityFailure,
		),
		Entry("over secondary layer2, when mac-security disabled, should succeed (MAC spoofed traffic is allowed)",
			networkAttachmentConfigParams{
				role:               "secondary",
				topology:           "layer2",
				disableMACSecurity: true,
			},
			assertConnectivitySuccess,
		),
	}

	// test cases for localnet network
	localnetCases := []TableEntry{
		Entry("over localnet, when mac-security enabled, should fail",
			networkAttachmentConfigParams{
				role:               "secondary",
				topology:           "localnet",
				vlanID:             100,
				disableMACSecurity: false,
			},
			assertConnectivityFailure,
		),
		Entry("over localnet, when mac-security disabled, should succeed (MAC spoofed traffic is allowed)",
			networkAttachmentConfigParams{
				role:               "secondary",
				topology:           "localnet",
				vlanID:             100,
				disableMACSecurity: true,
			},
			assertConnectivitySuccess,
		),
	}

	type provisionNetResourceFn func(ctx context.Context, f *framework.Framework, params networkAttachmentConfigParams)

	// test client-server TCP connectivity according to MAC security settings
	testBody := func(
		ctx context.Context,
		netConf networkAttachmentConfigParams,
		provisionNetworkResource provisionNetResourceFn,
		assertConnectivity asserConnectivityFn,
	) {
		const (
			clientSpoofedMAC = "02:11:22:33:44:55"
			clientCIDRv4     = "10.10.10.10/24"
			clientCIDRv6     = "2001:db8:abcd:1234::10/64"
			serverPort       = 9100
			serverIPV4       = "10.10.10.20"
			serverCIDRv4     = serverIPV4 + "/24"
			serverIPV6       = "2001:db8:abcd:1234::20"
			serverCIDRv6     = serverIPV6 + "/64"
		)
		cs := f.ClientSet
		// use IP addresses that fit the environment IP family
		clientCIDRs := filterCIDRs(cs, clientCIDRv4, clientCIDRv6)
		serverCIDRs := filterCIDRs(cs, serverCIDRv4, serverCIDRv6)
		serverAddrs := filterIPs(cs, serverIPV4, serverIPV6)
		// unique meta-names must be generated at the test spec container
		netConf.name = uniqueMetaName("l2-mac-sec")
		netConf.namespace = f.Namespace.Name

		By("creating network resource")
		provisionNetworkResource(ctx, f, netConf)

		By("create test pods")
		serverPodCfg := podConfiguration{
			name:      "server",
			namespace: netConf.namespace,
			attachments: []nadapi.NetworkSelectionElement{{
				Name:      netConf.name,
				IPRequest: serverCIDRs,
			}},
			containerCmd: httpServerContainerCmd(serverPort),
		}
		clientPodCfg := podConfiguration{
			name:      "client",
			namespace: netConf.namespace,
			attachments: []nadapi.NetworkSelectionElement{{
				Name:      netConf.name,
				IPRequest: clientCIDRs,
			}},
			// required for changing MAC address after pod is created
			isPrivileged: true,
		}
		_, err := cs.CoreV1().Pods(serverPodCfg.namespace).Create(context.Background(), generatePodSpec(serverPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = cs.CoreV1().Pods(clientPodCfg.namespace).Create(context.Background(), generatePodSpec(clientPodCfg), metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), cs, serverPodCfg.name, serverPodCfg.namespace)).To(Succeed())
		Expect(e2epod.WaitForPodNameRunningInNamespace(context.Background(), cs, clientPodCfg.name, clientPodCfg.namespace)).To(Succeed())

		By("test connectivity from client to server")
		Eventually(func(g Gomega) {
			for _, addr := range serverAddrs {
				g.Expect(connectToServer(clientPodCfg, addr, serverPort)).To(Succeed())
			}
		}).WithTimeout(10*time.Second).WithPolling(1*time.Second).Should(Succeed(),
			"connectivity should succeed when no MAC spoofing is performed")

		By("change client pod MAC address (simulating MAC spoofed traffic)")
		changeMacCmd := fmt.Sprintf("ip link set dev net1 address %s", clientSpoofedMAC)
		_, err = e2ekubectl.RunKubectl(clientPodCfg.namespace, "exec", clientPodCfg.name, "--",
			"sh", "-c", changeMacCmd)
		Expect(err).NotTo(HaveOccurred())
		By("verify client pod MAC address has been changed")
		macRAW, err := e2ekubectl.RunKubectl(clientPodCfg.namespace, "exec", clientPodCfg.name, "--", "cat", "/sys/class/net/net1/address")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(macRAW)).To(Equal(clientSpoofedMAC))

		By("test connectivity where client use spoofed MAC address")
		assertConnectivity(clientPodCfg, serverAddrs, serverPort)
	}

	DescribeTable("using ClusterUserDefinedNetwork, connectivity between client and server pods",
		func(ctx SpecContext, netConf networkAttachmentConfigParams, assertConnectivityFn asserConnectivityFn) {
			testBody(ctx, netConf, provisionCUDN, assertConnectivityFn)
		},
		layer2Cases,
		localnetCases,
	)

	DescribeTable("using UserDefinedNetwork, connectivity between client and server pods",
		func(ctx SpecContext, netConf networkAttachmentConfigParams, assertConnectivityFn asserConnectivityFn) {
			testBody(ctx, netConf, provisionUDN, assertConnectivityFn)
		},
		layer2Cases,
	)
})

func provisionCUDN(ctx context.Context, f *framework.Framework, netConf networkAttachmentConfigParams) {
	GinkgoHelper()
	if netConf.topology == "localnet" {
		netConf.physicalNetworkName = uniqueMetaName("mac-sec")
		By("setup underlay")
		Expect(infraprovider.SetupUnderlay(infraprovider.Get().NewTestContext(), f, infraapi.Underlay{
			LogicalNetworkName: netConf.physicalNetworkName,
			VlanID:             netConf.vlanID,
		})).To(Succeed())
	}

	cr, err := json.Marshal(generateTestMACSecurityCUDNManifest(netConf))
	Expect(err).NotTo(HaveOccurred())
	_, err = e2ekubectl.RunKubectlInput(netConf.namespace, string(cr), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", netConf.namespace))
		Expect(f.ClientSet.CoreV1().Pods(netConf.namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
		By("delete the CUDN CR")
		Expect(f.DynamicClient.Resource(clusterUDNGVR).Delete(context.Background(), netConf.name, metav1.DeleteOptions{})).To(Succeed())
	})
	Eventually(ctx, clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, netConf.name)).
		WithTimeout(5*time.Second).WithPolling(1*time.Second).Should(Succeed(), "CUDN should become ready")
}

func generateTestMACSecurityCUDNManifest(param networkAttachmentConfigParams) udnv1.ClusterUserDefinedNetwork {
	macSecurity := &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityEnabled}
	if param.disableMACSecurity {
		macSecurity.Mode = udnv1.MACSecurityDisabled
	}
	ipam := &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled}
	spec := udnv1.NetworkSpec{}
	switch param.topology {
	case "layer2":
		spec.Topology = udnv1.NetworkTopologyLayer2
		spec.Layer2 = &udnv1.Layer2Config{
			Role:        udnv1.NetworkRoleSecondary,
			IPAM:        ipam,
			MACSecurity: macSecurity,
		}
	case "localnet":
		var vlan *udnv1.VLANConfig
		if param.vlanID != 0 {
			vlan = &udnv1.VLANConfig{
				Mode:   udnv1.VLANModeAccess,
				Access: &udnv1.AccessVLANConfig{ID: int32(param.vlanID)},
			}
		}
		spec.Topology = udnv1.NetworkTopologyLocalnet
		spec.Localnet = &udnv1.LocalnetConfig{
			Role:                udnv1.NetworkRoleSecondary,
			IPAM:                ipam,
			PhysicalNetworkName: param.physicalNetworkName,
			MACSecurity:         macSecurity,
			VLAN:                vlan,
		}
	default:
		panic("unsupported topology")

	}
	return udnv1.ClusterUserDefinedNetwork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: udnv1.SchemeGroupVersion.String(),
			Kind:       "ClusterUserDefinedNetwork",
		},
		ObjectMeta: metav1.ObjectMeta{Name: param.name},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{param.namespace},
			}}},
			Network: spec,
		},
	}
}

func provisionUDN(ctx context.Context, f *framework.Framework, netConf networkAttachmentConfigParams) {
	GinkgoHelper()
	cr, err := json.Marshal(generateTestMACSecurityUDNManifest(netConf))
	Expect(err).NotTo(HaveOccurred())
	_, err = e2ekubectl.RunKubectlInput(netConf.namespace, string(cr), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, netConf.namespace, netConf.name), 5*time.Second, time.Second).Should(Succeed())
}

func generateTestMACSecurityUDNManifest(param networkAttachmentConfigParams) udnv1.UserDefinedNetwork {
	macSecurity := &udnv1.MACSecurityConfig{Mode: udnv1.MACSecurityEnabled}
	if param.disableMACSecurity {
		macSecurity.Mode = udnv1.MACSecurityDisabled
	}
	ipam := &udnv1.IPAMConfig{Mode: udnv1.IPAMDisabled}
	spec := udnv1.UserDefinedNetworkSpec{}
	switch param.topology {
	case "layer2":
		spec.Topology = udnv1.NetworkTopologyLayer2
		spec.Layer2 = &udnv1.Layer2Config{
			Role:        udnv1.NetworkRoleSecondary,
			IPAM:        ipam,
			MACSecurity: macSecurity,
		}
	default:
		panic("unsupported topology")

	}
	return udnv1.UserDefinedNetwork{
		TypeMeta: metav1.TypeMeta{
			APIVersion: udnv1.SchemeGroupVersion.String(),
			Kind:       "UserDefinedNetwork",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      param.name,
			Namespace: param.namespace,
		},
		Spec: spec,
	}
}

type asserConnectivityFn func(clientCfg podConfiguration, serverAddrs []string, serverPort uint16)

func assertConnectivitySuccess(clientCfg podConfiguration, serverAddrs []string, serverPort uint16) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		for _, addr := range serverAddrs {
			g.Expect(connectToServer(clientCfg, addr, serverPort)).To(Succeed())
		}
	}).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(Succeed())
}

func assertConnectivityFailure(clientCfg podConfiguration, serverAddrs []string, serverPort uint16) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		for _, addr := range serverAddrs {
			g.Expect(connectToServer(clientCfg, addr, serverPort)).ToNot(Succeed())
		}
	}).WithTimeout(5 * time.Second).WithPolling(1 * time.Second).Should(Succeed())
}
