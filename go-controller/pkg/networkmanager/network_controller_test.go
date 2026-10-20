// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestSetAdvertisements(t *testing.T) {
	testNodeName := "testNode"
	testNADName := "test/NAD"
	testRAName := "testRA"
	testVRFName := "testVRF"

	defaultNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	primaryNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "primary",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: "layer3",
		Role:     "primary",
		MTU:      1400,
	}

	noOverlayNetwork := *primaryNetwork
	noOverlayNetwork.Transport = types.NetworkTransportNoOverlay
	evpnNetwork := *primaryNetwork
	evpnNetwork.Transport = types.NetworkTransportEVPN

	podNetworkRA := ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:    testVRFName,
			NodeSelector: metav1.LabelSelector{},
			Advertisements: []ratypes.AdvertisementType{
				ratypes.PodNetwork,
			},
		},
		Status: ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	nonPodNetworkRA := ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:    testVRFName,
			NodeSelector: metav1.LabelSelector{},
		},
		Status: ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{
				{
					Type:   "Accepted",
					Status: metav1.ConditionTrue,
				},
			},
		},
	}
	podNetworkRADegraded := *podNetworkRA.DeepCopy()
	podNetworkRADegraded.Status.Conditions = append(podNetworkRADegraded.Status.Conditions,
		metav1.Condition{Type: "Degraded", Status: metav1.ConditionTrue, Reason: "NodeConfigurationFailed"})
	degradedDefaultVRFRA := *podNetworkRADegraded.DeepCopy()
	degradedDefaultVRFRA.Spec.TargetVRF = ""

	podNetworkRANotAccepted := podNetworkRA
	podNetworkRANotAccepted.Status = ratypes.RouteAdvertisementsStatus{}
	podNetworkRARejected := *podNetworkRA.DeepCopy()
	podNetworkRARejected.Status.Conditions[0].Status = metav1.ConditionFalse
	podNetworkRAOutdated := podNetworkRA
	podNetworkRAOutdated.Generation = 1

	testNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
	}
	otherNode := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "otherNode",
		},
	}

	tests := []struct {
		name                      string
		network                   *ovncnitypes.NetConf
		ra                        *ratypes.RouteAdvertisements
		node                      corev1.Node
		missingRA                 bool
		expectNoNetwork           bool
		existingPodAdvertisements map[string][]string
		existingEIPAdvertisements map[string][]string
		expected                  map[string][]string
		expectedEIP               map[string][]string
	}{
		{
			name:    "reconciles VRF advertisements for selected node of default node network controller",
			network: defaultNetwork,
			ra:      &podNetworkRA,
			node:    testNode,
			expected: map[string][]string{
				testNodeName: {testVRFName},
			},
		},
		{
			name:    "reconciles VRF advertisements for selected node of primary network controller",
			network: primaryNetwork,
			ra:      &podNetworkRA,
			node:    testNode,
			expected: map[string][]string{
				testNodeName: {testVRFName},
			},
		},
		{
			name:    "does not advertise a degraded network without generated node configs",
			network: primaryNetwork,
			ra:      &podNetworkRADegraded,
			node:    testNode,
		},
		{
			name:    "retains default VRF for degraded no-overlay without generated configs",
			network: &noOverlayNetwork, ra: &degradedDefaultVRFRA, node: testNode,
			expected: map[string][]string{testNodeName: {types.DefaultNetworkName}},
		},
		{
			name:    "retains non-default VRF for degraded no-overlay without generated configs",
			network: &noOverlayNetwork, ra: &podNetworkRADegraded, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}},
		},
		{
			name:    "retains target VRF for degraded EVPN without generated configs",
			network: &evpnNetwork, ra: &podNetworkRADegraded, node: testNode,
			expected: map[string][]string{testNodeName: {testVRFName}},
		},
		{
			name:    "ignores advertisements that are not for the pod network",
			network: defaultNetwork,
			ra:      &nonPodNetworkRA,
			node:    testNode,
		},
		{
			name:    "ignores advertisements that are not for applicable node",
			network: defaultNetwork,
			ra:      &podNetworkRA,
			node:    otherNode,
		},
		{
			name:    "ignores advertisements with no Accepted condition",
			network: defaultNetwork,
			ra:      &podNetworkRANotAccepted,
			node:    testNode,
		},
		{
			name:    "starts new network without advertisements when advertisements are rejected",
			network: primaryNetwork,
			ra:      &podNetworkRARejected,
			node:    testNode,
		},
		{
			name:    "starts new network without advertisements when advertisements are old",
			network: primaryNetwork,
			ra:      &podNetworkRAOutdated,
			node:    testNode,
		},
		{
			name:    "preserves existing advertisements when advertisements are rejected",
			network: primaryNetwork,
			ra:      &podNetworkRARejected,
			node:    testNode,
			existingPodAdvertisements: map[string][]string{
				testNodeName: {"previous-pod-vrf"},
			},
			existingEIPAdvertisements: map[string][]string{
				testNodeName: {"previous-eip-vrf"},
			},
			expected: map[string][]string{
				testNodeName: {"previous-pod-vrf"},
			},
			expectedEIP: map[string][]string{
				testNodeName: {"previous-eip-vrf"},
			},
		},
		{
			name:      "preserves existing advertisements when route advertisement is missing",
			network:   primaryNetwork,
			ra:        &podNetworkRA,
			node:      testNode,
			missingRA: true,
			existingPodAdvertisements: map[string][]string{
				testNodeName: {"previous-pod-vrf"},
			},
			existingEIPAdvertisements: map[string][]string{
				testNodeName: {"previous-eip-vrf"},
			},
			expected: map[string][]string{
				testNodeName: {"previous-pod-vrf"},
			},
			expectedEIP: map[string][]string{
				testNodeName: {"previous-eip-vrf"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, testNodeName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkController("", testNodeName, tcm, wf)

			namespace, name, err := cache.SplitMetaNamespaceKey(testNADName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nadAnnotations := map[string]string{
				types.OvnRouteAdvertisementsKey: "[\"" + tt.ra.Name + "\"]",
			}
			nad, err := buildNADWithAnnotations(name, namespace, tt.network, nadAnnotations)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &tt.node, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			if !tt.missingRA {
				_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}
			_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			netInfo, err := util.NewNetInfo(tt.network)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.AddNADs(testNADName)

			if tt.existingPodAdvertisements != nil || tt.existingEIPAdvertisements != nil {
				existingNetInfo := util.NewMutableNetInfo(netInfo)
				existingNetInfo.AddNADs(testNADName)
				existingNetInfo.SetPodNetworkAdvertisedVRFs(tt.existingPodAdvertisements)
				existingNetInfo.SetEgressIPAdvertisedVRFs(tt.existingEIPAdvertisements)
				existingController := &testNetworkController{
					ReconcilableNetInfo: util.NewReconcilableNetInfo(existingNetInfo),
					tcm:                 tcm,
				}
				tcm.controllers[testNetworkKey(netInfo)] = existingController
				nm.setNetworkState(existingNetInfo.GetNetworkName(), &networkControllerState{controller: existingController})
			}

			nm.getNADKeysForNetwork = func(networkName string) []string {
				if networkName == mutableNetInfo.GetNetworkName() {
					return []string{testNADName}
				}
				return nil
			}

			nm.EnsureNetwork(mutableNetInfo)
			g.Expect(nm.Start()).To(gomega.Succeed())
			defer nm.Stop()

			meetsExpectations := func(g gomega.Gomega) {
				tcm.Lock()
				defer tcm.Unlock()
				var reconcilable ReconcilableNetworkController
				switch tt.network.Name {
				case types.DefaultNetworkName:
					reconcilable = tcm.GetDefaultNetworkController()
				default:
					reconcilable = tcm.controllers[testNetworkKey(netInfo)]
				}

				if tt.expectNoNetwork {
					g.Expect(reconcilable).To(gomega.BeNil())
					return
				}
				g.Expect(reconcilable).ToNot(gomega.BeNil())

				if tt.expected == nil {
					tt.expected = map[string][]string{}
				}
				g.Expect(reconcilable.GetPodNetworkAdvertisedVRFs()).To(gomega.Equal(tt.expected))
				if tt.expectedEIP == nil {
					tt.expectedEIP = map[string][]string{}
				}
				g.Expect(reconcilable.GetEgressIPAdvertisedVRFs()).To(gomega.Equal(tt.expectedEIP))
			}

			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			g.Consistently(meetsExpectations).Should(gomega.Succeed())
			if tt.ra != &podNetworkRADegraded || tt.network.Transport != "" {
				return
			}
			// The RA stays degraded while this node recovers or fails again.
			// FRR events alone must update the advertised state, without polling.
			cfg := &frrtypes.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{
				Name: "generated", Namespace: "frr",
				Labels:      map[string]string{types.OvnRouteAdvertisementsKey: testRAName, types.OvnRouteAdvertisementsNodeKey: testNodeName},
				Annotations: map[string]string{types.OvnRouteAdvertisementsReadyKey: util.RouteAdvertisementsReadyToken(tt.ra, 1)},
			}}
			cfg, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Create(context.Background(), cfg, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			tt.expected = map[string][]string{testNodeName: {testVRFName}}
			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			delete(cfg.Annotations, types.OvnRouteAdvertisementsReadyKey)
			cfg, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Update(context.Background(), cfg, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			tt.expected = map[string][]string{}
			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			cfg.Annotations[types.OvnRouteAdvertisementsReadyKey] = util.RouteAdvertisementsReadyToken(tt.ra, 1)
			_, err = fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Update(context.Background(), cfg, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			tt.expected = map[string][]string{testNodeName: {testVRFName}}
			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			g.Expect(fakeClient.FRRClient.ApiV1beta1().FRRConfigurations("frr").Delete(context.Background(), cfg.Name, metav1.DeleteOptions{})).To(gomega.Succeed())
			tt.expected = map[string][]string{}
			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			// Clearing Degraded restores the healthy path without FRR lookups.
			ra := tt.ra.DeepCopy()
			ra.ResourceVersion = "2"
			meta.FindStatusCondition(ra.Status.Conditions, "Degraded").Status = metav1.ConditionFalse
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Update(context.Background(), ra, metav1.UpdateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			tt.expected = map[string][]string{testNodeName: {testVRFName}}
			g.Eventually(meetsExpectations).Should(gomega.Succeed())
		})
	}
}

func TestNetworkControllerIsNodeManaged(t *testing.T) {
	const localNode = "local-node"

	tests := []struct {
		name       string
		controller *networkController
		node       *corev1.Node
		want       bool
	}{
		{
			name:       "cluster manager manages unannotated node",
			controller: &networkController{},
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			want:       true,
		},
		{
			name:       "node manager manages its own unannotated node",
			controller: &networkController{node: localNode},
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: localNode}},
			want:       true,
		},
		{
			name:       "node manager ignores foreign unannotated node",
			controller: &networkController{node: localNode},
			node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other-node"}},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(tt.controller.isNodeManaged(tt.node)).To(gomega.Equal(tt.want))
		})
	}
}

func TestNetworkControllerReconcilePendingNetworkRefChange(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tests := []struct {
		name           string
		nodeHasNetwork bool
	}{
		{
			name:           "active",
			nodeHasNetwork: true,
		},
		{
			name:           "inactive",
			nodeHasNetwork: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkController("", "", tcm, nil)
			nm.nodeHasNetwork = func(_, _ string) bool { return tt.nodeHasNetwork }

			networkName := netInfo.GetNetworkName()
			mutableNetInfo := util.NewMutableNetInfo(netInfo)
			mutableNetInfo.SetNADs(netConf.NADName)
			nm.setNetwork(networkName, mutableNetInfo)

			var gotNode string
			var gotActive bool
			var callCount int
			testController := &testNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				tcm:                 tcm,
				handleRefChange: func(node string, active bool) {
					gotNode = node
					gotActive = active
					callCount++
				},
			}
			nm.networkControllers[networkName] = &networkControllerState{
				controller: testController,
			}

			nm.NotifyNetworkRefChange(networkName, "node1")
			err := nm.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Expect(callCount).To(gomega.Equal(1))
			g.Expect(gotNode).To(gomega.Equal("node1"))
			g.Expect(gotActive).To(gomega.Equal(tt.nodeHasNetwork))

			nm.NotifyNetworkRefChange(networkName, "node1")
			err = nm.syncNetwork(networkName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(callCount).To(gomega.Equal(1))
		})
	}
}

func TestNetworkControllerClearsPendingNetworkRefOnDelete(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("", "", tcm, nil)
	nm.nodeHasNetwork = func(_, _ string) bool { return true }

	networkName := netInfo.GetNetworkName()
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	nm.setNetwork(networkName, mutableNetInfo)

	var callCount int
	testController := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
		handleRefChange: func(string, bool) {
			callCount++
		},
	}
	nm.networkControllers[networkName] = &networkControllerState{
		controller: testController,
	}

	nm.NotifyNetworkRefChange(networkName, "node1")
	err = nm.deleteNetwork(networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(callCount).To(gomega.Equal(0))

	var followupCalls int
	followupController := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
		handleRefChange: func(string, bool) {
			followupCalls++
		},
	}
	nm.networkControllers[networkName] = &networkControllerState{
		controller: followupController,
	}
	nm.setNetwork(networkName, mutableNetInfo)
	err = nm.syncNetwork(networkName)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(followupCalls).To(gomega.Equal(0))
}

func TestNetworkControllerCleansStoppedNetworkAfterStartFailureAndDeletion(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: fmt.Errorf("start failed"),
	}
	nm := newNetworkController("", "", tcm, nil)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)

	err = nm.syncNetwork(networkName)
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("failed to start network"))
	failedState := nm.getNetworkState(networkName)
	g.Expect(failedState.controller).ToNot(gomega.BeNil())
	g.Expect(failedState.startFailed).To(gomega.BeTrue())

	nm.setNetwork(networkName, nil)
	g.Expect(nm.syncNetwork(networkName)).To(gomega.Succeed())
	g.Expect(nm.getNetworkState(networkName).controller).To(gomega.BeNil())

	tcm.Lock()
	defer tcm.Unlock()
	expectedNetworkKey := testNetworkKey(netInfo)
	g.Expect(tcm.started).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.stopped).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.cleaned).To(gomega.Equal([]string{expectedNetworkKey}))
}

func TestNetworkControllerCleansFailedStartBeforeRetry(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	startErr := fmt.Errorf("start failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)

	g.Expect(nm.syncNetwork(networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(networkName)).To(gomega.Succeed())

	state := nm.getNetworkState(networkName)
	g.Expect(state.controller).ToNot(gomega.BeNil())
	g.Expect(state.startFailed).To(gomega.BeFalse())
	tcm.Lock()
	defer tcm.Unlock()
	expectedNetworkKey := testNetworkKey(netInfo)
	g.Expect(tcm.started).To(gomega.Equal([]string{expectedNetworkKey, expectedNetworkKey}))
	g.Expect(tcm.stopped).To(gomega.Equal([]string{expectedNetworkKey}))
	g.Expect(tcm.cleaned).To(gomega.Equal([]string{expectedNetworkKey}))
}

func TestNetworkControllerCleansFailedStartBeforeIncompatibleReplacement(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false
	config.OVNKubernetesFeature.EnableUplink = true

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
		Uplink:   "uplink-a",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	startErr := fmt.Errorf("start failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil)

	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)
	g.Expect(nm.syncNetwork(networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	replacementConf := *netConf
	replacementConf.Uplink = "uplink-b"
	replacementInfo, err := util.NewNetInfo(&replacementConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(util.AreNetworksCompatible(netInfo, replacementInfo)).To(gomega.BeFalse())
	replacement := util.NewMutableNetInfo(replacementInfo)
	replacement.SetNADs(replacementConf.NADName)
	nm.setNetwork(networkName, replacement)

	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(networkName)).To(gomega.Succeed())

	tcm.Lock()
	defer tcm.Unlock()
	g.Expect(tcm.started).To(gomega.HaveLen(2))
	g.Expect(tcm.stopped).To(gomega.HaveLen(1))
	g.Expect(tcm.cleaned).To(gomega.HaveLen(1))
}

func TestNetworkControllerFinishesTerminalCleanupBeforeFailedStartRecreation(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() {
		g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = false

	netConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: "udn-net",
			Type: "ovn-k8s-cni-overlay",
		},
		Topology: types.Layer3Topology,
		Role:     types.NetworkRolePrimary,
		NADName:  "ns1/primary",
		Subnets:  "10.128.0.0/14",
	}
	netInfo, err := util.NewNetInfo(netConf)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.SetNADs(netConf.NADName)

	startErr := fmt.Errorf("start failed")
	cleanupErr := fmt.Errorf("cleanup failed")
	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
		raiseErrorWhenStartingController: startErr,
	}
	nm := newNetworkController("", "", tcm, nil)
	networkName := mutableNetInfo.GetNetworkName()
	nm.setNetwork(networkName, mutableNetInfo)
	g.Expect(nm.syncNetwork(networkName)).To(gomega.MatchError(gomega.ContainSubstring("failed to start network")))

	nm.setNetwork(networkName, nil)
	tcm.Lock()
	tcm.raiseErrorWhenCleaningController = cleanupErr
	tcm.Unlock()
	g.Expect(nm.syncNetwork(networkName)).To(gomega.MatchError(gomega.ContainSubstring("cleanup failed")))

	nm.setNetwork(networkName, mutableNetInfo)
	tcm.Lock()
	tcm.raiseErrorWhenStartingController = nil
	tcm.raiseErrorWhenCleaningController = nil
	tcm.Unlock()
	g.Expect(nm.syncNetwork(networkName)).To(gomega.Succeed())

	state := nm.getNetworkState(networkName)
	g.Expect(state.controller).ToNot(gomega.BeNil())
	g.Expect(state.startFailed).To(gomega.BeFalse())
	tcm.Lock()
	defer tcm.Unlock()
	g.Expect(tcm.started).To(gomega.HaveLen(2))
	g.Expect(tcm.stopped).To(gomega.HaveLen(1))
	g.Expect(tcm.cleaned).To(gomega.HaveLen(2))
}

// TestNetworkController_ConcurrentReconciliation validates that the networkReconciler
// can safely handle concurrent network additions and deletions without data races.
func TestNetworkController_ConcurrentReconciliation(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("test", "", tcm, wf)

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	g.Expect(nm.Start()).To(gomega.Succeed())
	defer nm.Stop()

	const numNetworks = 20
	const numIterations = 3

	for iteration := 0; iteration < numIterations; iteration++ {
		t.Logf("Iteration %d/%d: Testing concurrent add/delete of %d networks", iteration+1, numIterations, numNetworks)

		// Phase 1: Concurrent network additions
		var addWg sync.WaitGroup
		for i := 0; i < numNetworks; i++ {
			addWg.Add(1)
			go func(idx int) {
				defer addWg.Done()

				// Create a test network
				networkName := fmt.Sprintf("test-net-%d-%d", iteration, idx)
				netConf := &ovncnitypes.NetConf{
					NetConf: cnitypes.NetConf{
						Name: networkName,
						Type: "ovn-k8s-cni-overlay",
					},
					Topology: "layer2",
					Role:     "secondary",
					MTU:      1400,
				}

				netInfo, err := util.NewNetInfo(netConf)
				if err != nil {
					t.Errorf("Failed to create NetInfo for %s: %v", networkName, err)
					return
				}

				mutableNetInfo := util.NewMutableNetInfo(netInfo)
				nm.EnsureNetwork(mutableNetInfo)
			}(i)
		}
		addWg.Wait()

		// getAllNetworks() returns only secondary networks, not the default network
		g.Eventually(nm.getAllNetworks, 5*time.Second, 100*time.Millisecond).
			Should(gomega.HaveLen(numNetworks))

		// Phase 2: Concurrent network deletions
		var delWg sync.WaitGroup
		for i := 0; i < numNetworks; i++ {
			delWg.Add(1)
			go func(idx int) {
				defer delWg.Done()
				networkName := fmt.Sprintf("test-net-%d-%d", iteration, idx)
				nm.DeleteNetwork(networkName)
			}(i)
		}
		delWg.Wait()

		g.Eventually(nm.getAllNetworks).WithTimeout(5*time.Second).
			WithPolling(100*time.Millisecond).Should(gomega.BeEmpty(),
			"all test networks should be deleted")
	}
}

// TestNetworkController_ConcurrentReconciliationMixed validates concurrent operations
// with mixed add, update, and delete operations happening simultaneously.
func TestNetworkController_ConcurrentReconciliationMixed(t *testing.T) {
	g := gomega.NewWithT(t)

	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
	wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tcm := &testControllerManager{
		controllers: map[string]NetworkController{},
		defaultNetwork: &testNetworkController{
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	nm := newNetworkController("test", "", tcm, wf)

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	g.Expect(nm.Start()).To(gomega.Succeed())
	defer nm.Stop()

	const numOperations = 30
	const numUniqueNetworks = 10
	var wg sync.WaitGroup

	for i := 0; i < numOperations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			networkName := fmt.Sprintf("mixed-net-%d", idx%numUniqueNetworks)
			netConf := &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: networkName,
					Type: "ovn-k8s-cni-overlay",
				},
				Topology: "layer2",
				Role:     "secondary",
				MTU:      1400,
			}

			netInfo, err := util.NewNetInfo(netConf)
			if err != nil {
				t.Errorf("Failed to create NetInfo: %v", err)
				return
			}

			mutableNetInfo := util.NewMutableNetInfo(netInfo)

			// Randomly add or delete
			if idx%3 == 0 {
				nm.DeleteNetwork(networkName)
			} else {
				nm.EnsureNetwork(mutableNetInfo)
			}
		}(i)
	}
	wg.Wait()

	// Ensure all networks exist to reach a deterministic final state
	for i := 0; i < numUniqueNetworks; i++ {
		networkName := fmt.Sprintf("mixed-net-%d", i)
		netConf := &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: networkName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology: "layer2",
			Role:     "secondary",
			MTU:      1400,
		}

		netInfo, err := util.NewNetInfo(netConf)
		g.Expect(err).ToNot(gomega.HaveOccurred())

		mutableNetInfo := util.NewMutableNetInfo(netInfo)
		nm.EnsureNetwork(mutableNetInfo)
	}

	// Verify all networks exist (deterministic final state)
	g.Eventually(func(g gomega.Gomega) {
		networks := nm.getAllNetworks()
		// getAllNetworks() returns secondary networks only
		g.Expect(networks).To(gomega.HaveLen(numUniqueNetworks),
			"Expected %d networks after mixed operations, got %d", numUniqueNetworks, len(networks))

		// Verify each network can be retrieved
		for _, network := range networks {
			retrieved := nm.getNetwork(network.GetNetworkName())
			g.Expect(retrieved).ToNot(gomega.BeNil())
		}
	}).Should(gomega.Succeed())
}

func TestDegradedNodeAdvertisementsReady(t *testing.T) {
	for _, nodeName := range []string{"node", strings.Repeat("a", 63) + ".example.org"} {
		t.Run(nodeName, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			wf, err := factory.NewClusterManagerWatchFactory(util.GetOVNClientset().GetClusterManagerClientset())
			g.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()
			indexer := wf.FRRConfigurationsInformer().Informer().GetIndexer()
			c := &networkController{frrIndexer: indexer}
			ra := &ratypes.RouteAdvertisements{ObjectMeta: metav1.ObjectMeta{Name: "ra", UID: "uid", Generation: 2}}
			cfg := func(name, node, token string) *frrtypes.FRRConfiguration {
				return &frrtypes.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "frr",
					Labels:      map[string]string{types.OvnRouteAdvertisementsKey: ra.Name, types.OvnRouteAdvertisementsNodeKey: util.RouteAdvertisementsNodeLabel(node)},
					Annotations: map[string]string{types.OvnRouteAdvertisementsReadyKey: token}}}
			}
			check := func(expected bool) {
				t.Helper()
				ready, err := c.nodeAdvertisementsReady(ra, nodeName)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(ready).To(gomega.Equal(expected))
			}
			check(false)
			// Other nodes cannot make this node ready.
			g.Expect(indexer.Add(cfg("other", "other-node", util.RouteAdvertisementsReadyToken(ra, 1)))).To(gomega.Succeed())
			check(false)
			first := cfg("first", nodeName, util.RouteAdvertisementsReadyToken(ra, 2))
			g.Expect(indexer.Add(first)).To(gomega.Succeed())
			check(false) // one of two templates is still absent
			second := cfg("second", nodeName, util.RouteAdvertisementsReadyToken(ra, 2))
			g.Expect(indexer.Add(second)).To(gomega.Succeed())
			check(true)
			for _, token := range []string{"", "uid/1/2", "previous-uid/2/2", "uid/2/1"} {
				stale := second.DeepCopy()
				stale.Annotations[types.OvnRouteAdvertisementsReadyKey] = token
				g.Expect(indexer.Update(stale)).To(gomega.Succeed())
				check(false)
			}
			g.Expect(indexer.Update(second)).To(gomega.Succeed())
			check(true)
			deleting := second.DeepCopy()
			now := metav1.Now()
			deleting.DeletionTimestamp = &now
			g.Expect(indexer.Update(deleting)).To(gomega.Succeed())
			check(false)

			// Events for this node must trigger degraded reconciliation; other nodes
			// must remain excluded even when their names require encoding too.
			c.node = nodeName
			c.raLister = wf.RouteAdvertisementsInformer().Lister()
			ra.Status.Conditions = []metav1.Condition{{Type: "Degraded", Status: metav1.ConditionTrue}}
			g.Expect(wf.RouteAdvertisementsInformer().Informer().GetIndexer().Add(ra)).To(gomega.Succeed())
			g.Expect(c.frrNeedsUpdate(nil, first)).To(gomega.BeTrue())
			g.Expect(c.frrNeedsUpdate(first, nil)).To(gomega.BeTrue())
			g.Expect(c.frrNeedsUpdate(nil, cfg("remote", strings.Repeat("b", 63)+".example.org", ""))).To(gomega.BeFalse())
		})
	}
}
