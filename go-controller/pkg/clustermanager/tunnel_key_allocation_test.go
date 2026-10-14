// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"strconv"
	"testing"

	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestInitTunnelKeysAllocatorPreservesKeysAfterConfigChange(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatal(err)
	}
	// The role changed while CM was stopped, leaving the primary network's keys annotated.
	nad := ovntesting.GenerateNAD("test", "test", "test", types.Layer2Topology, "10.0.0.0/24", types.NetworkRoleSecondary)
	nad.Annotations = map[string]string{
		types.OvnNetworkIDAnnotation:         "1",
		types.OvnNetworkTunnelKeysAnnotation: "[16711684,16715779]",
	}
	client := util.GetOVNClientset(nad)
	allocator, err := initTunnelKeysAllocator(client.NetworkAttchDefClient, client.NetworkConnectClient)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the persisted key reserved until reconciliation saves the shorter annotation.
	if err := allocator.ReserveKeys(id.NetworkTunnelKeyOwner("other"), []int{16715779}); err == nil {
		t.Fatal("startup released a key still referenced by the NAD")
	}
	if err := allocator.ReserveKeys(id.NetworkTunnelKeyOwner("test"), []int{16711684, 16715779}); err != nil {
		t.Fatalf("startup did not preserve the network's existing keys: %v", err)
	}
}

func TestInitTunnelKeysAllocatorPreservesCNCAcrossUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cncOwner string
	}{
		{name: "legacy shared key", cncOwner: "shared"},
		{name: "distinct keys", cncOwner: "other-owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			config.IPv4Mode = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableNetworkConnect = true

			// Before owner prefixes, a same-named network and CNC reused the same dynamic key.
			legacy := id.NewTunnelKeyAllocator("legacy")
			networkKeys, err := legacy.AllocateKeys("shared", 1, 2)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			cncKeys, err := legacy.AllocateKeys(tc.cncOwner, types.MaxNetworks, 1)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			keyAnnotation, err := util.FormatTunnelKeysAnnotation(networkKeys)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			nad := ovntesting.GenerateNAD("shared", "nad", "ns", types.Layer2Topology, "10.0.0.0/24", types.NetworkRolePrimary)
			nad.Annotations = map[string]string{
				types.OvnNetworkIDAnnotation:         "1",
				types.OvnNetworkTunnelKeysAnnotation: keyAnnotation,
			}
			cnc := &networkconnectv1.ClusterNetworkConnect{ObjectMeta: metav1.ObjectMeta{
				Name:        "shared",
				Annotations: map[string]string{util.OvnConnectRouterTunnelKeyAnnotation: strconv.Itoa(cncKeys[0])},
			}}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "ns", Labels: map[string]string{types.RequiredUDNNamespaceLabel: ""},
			}}
			client := util.GetOVNClientset(nad, cnc, namespace).GetClusterManagerClientset()
			allocator, err := initTunnelKeysAllocator(client.NetworkAttchDefClient, client.NetworkConnectClient)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(allocator.ReserveKeys(id.NetworkConnectTunnelKeyOwner(cnc.Name), cncKeys)).To(gomega.Succeed())

			wf, err := factory.NewClusterManagerWatchFactory(client)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(wf.Start()).To(gomega.Succeed())
			t.Cleanup(wf.Shutdown)
			manager, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, client, nil, allocator)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(manager.Stop)
			g.Expect(manager.Start()).To(gomega.Succeed())

			updatedNAD, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			updatedKeys, err := util.ParseTunnelKeysAnnotation(updatedNAD.Annotations[types.OvnNetworkTunnelKeysAnnotation])
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(updatedKeys).To(gomega.HaveLen(2))
			g.Expect(updatedKeys[0]).To(gomega.Equal(networkKeys[0]))
			g.Expect(updatedKeys[1]).NotTo(gomega.Equal(cncKeys[0]))
			g.Expect(updatedNAD.Annotations[types.OvnNetworkIDAnnotation]).To(gomega.Equal("1"))
			if tc.cncOwner != "shared" {
				g.Expect(updatedKeys).To(gomega.Equal(networkKeys))
			}
			updatedCNC, err := client.NetworkConnectClient.K8sV1().ClusterNetworkConnects().Get(context.Background(), cnc.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(updatedCNC.Annotations).To(gomega.Equal(cnc.Annotations))
			g.Expect(allocator.ReserveKeys("probe", cncKeys)).NotTo(gomega.Succeed())
			g.Expect(allocator.ReserveKeys("probe", updatedKeys[1:])).NotTo(gomega.Succeed())
		})
	}
}

func TestInitTunnelKeysAllocatorSkipsUnusableConfig(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.IPv4Mode = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	// A previously allocated primary NAD was changed to an invalid localnet config while CM was stopped.
	nad := ovntesting.GenerateNAD("test", "nad", "ns", types.LocalnetTopology, "not-a-cidr", types.NetworkRoleSecondary)
	nad.Annotations = map[string]string{types.OvnNetworkIDAnnotation: "1", types.OvnNetworkTunnelKeysAnnotation: "[16711684,16715779]"}
	conf, err := util.ParseNetConf(nad)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = util.NewNetInfo(conf)
	g.Expect(err).To(gomega.HaveOccurred(), "fixture must be rejected during NAD reconciliation")
	client := util.GetOVNClientset(nad).GetClusterManagerClientset()
	allocator, err := initTunnelKeysAllocator(client.NetworkAttchDefClient, client.NetworkConnectClient)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	wf, err := factory.NewClusterManagerWatchFactory(client)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(wf.Start()).To(gomega.Succeed())
	t.Cleanup(wf.Shutdown)
	manager, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, client, nil, allocator)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(manager.Stop)
	g.Expect(manager.Start()).To(gomega.Succeed())
	g.Expect(client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Delete(context.Background(), nad.Name, metav1.DeleteOptions{})).To(gomega.Succeed())
	g.Eventually(func() error { return allocator.ReserveKeys("probe", []int{16715779}) }).Should(gomega.Succeed(), "a deleted untracked NAD must not retain a tunnel-key reservation")
}
