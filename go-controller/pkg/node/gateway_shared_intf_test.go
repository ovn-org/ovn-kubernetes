// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/knftables"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	udnfakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	netlink_mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/vishvananda/netlink"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilMocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMasqueradeLinkOperationsAreSerialized(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.IPv4Mode = false
	config.IPv6Mode = false

	netlinkMock := new(utilMocks.NetLinkOps)
	originalNetlinkOps := util.GetNetLinkOps()
	util.SetNetLinkOpMockInst(netlinkMock)
	t.Cleanup(func() { util.SetNetLinkOpMockInst(originalNetlinkOps) })

	firstLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "br-a", Index: 1}}
	secondLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "br-b", Index: 2}}
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	netlinkMock.On("LinkByName", "br-a").
		Run(func(mock.Arguments) {
			close(firstEntered)
			<-releaseFirst
		}).
		Return(firstLink, nil).
		Once()
	netlinkMock.On("LinkSetUp", firstLink).Return(nil).Once()
	netlinkMock.On("LinkByName", "br-b").
		Run(func(mock.Arguments) { close(secondEntered) }).
		Return(secondLink, nil).
		Once()
	netlinkMock.On("LinkSetUp", secondLink).Return(nil).Once()

	firstDone := make(chan error, 1)
	go func() { firstDone <- setNodeMasqueradeIPOnExtBridge("br-a") }()
	<-firstEntered

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- addHostMACBindings("br-b")
	}()
	<-secondStarted
	enteredConcurrently := false
	select {
	case <-secondEntered:
		enteredConcurrently = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("masquerade address operation failed: %v", err)
	}
	if !enteredConcurrently {
		select {
		case <-secondEntered:
		case <-time.After(time.Second):
			t.Fatal("masquerade neighbor operation did not run")
		}
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("masquerade neighbor operation failed: %v", err)
	}
	if enteredConcurrently {
		t.Fatal("masquerade address and neighbor operations ran concurrently")
	}
	netlinkMock.AssertExpectations(t)
}

func TestHostNetworkServiceOpenFlowsUsesGroupForMultipleHostNetworkTargetPorts(t *testing.T) {
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "namespace1",
			Name:      "service1",
		},
	}
	npw := &nodePortWatcher{ofportPhys: "eth0"}
	key := "NodePort_namespace1_service1_tcp_31111"

	flows, groups := npw.hostNetworkServiceOpenFlows(
		service,
		key,
		"0x123",
		"tcp",
		"in_port=eth0, tcp, tp_dst=31111",
		"10.244.0.1",
		util.LBEndpoints{
			{Port: 8080, V4IPs: []string{"10.128.0.2"}},    // local OVN-networked endpoint, not a host DNAT target
			{Port: 9090, V4IPs: []string{"192.168.18.15"}}, // local host-networked endpoint
			{Port: 10090, V4IPs: []string{"192.168.18.15"}},
		},
		[]net.IP{net.ParseIP("192.168.18.15")},
		"eth0",
	)

	groupID := hostNetworkServiceGroupID(key)
	expectedGroup := fmt.Sprintf("group_id=%d,type=select,"+
		"bucket=actions=ct(commit,zone=64003,nat(dst=10.244.0.1:9090),table=6),"+
		"bucket=actions=ct(commit,zone=64003,nat(dst=10.244.0.1:10090),table=6)", groupID)
	if len(groups) != 1 || groups[0] != expectedGroup {
		t.Fatalf("unexpected groups: %#v", groups)
	}
	if len(flows) != 5 {
		t.Fatalf("expected 5 flows, got %d: %#v", len(flows), flows)
	}
	expectedIngressFlow := fmt.Sprintf("cookie=0x123, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=group:%d", groupID)
	if flows[0] != expectedIngressFlow {
		t.Fatalf("unexpected ingress flow: %q", flows[0])
	}
	for _, line := range append(flows, groups...) {
		if strings.Contains(line, "10.244.0.1:8080") || strings.Contains(line, "tp_src=8080") {
			t.Fatalf("non-host endpoint target port was programmed: %q", line)
		}
	}
}

// Note: Local mocks are used instead of FakeNetworkManager to test specific error conditions
// (NotFound, InvalidPrimaryNetworkError) from GetActiveNetworkForNamespace. FakeNetworkManager
// doesn't support error injection. And the tests here are not dependent on the methods that
// FakeNetworkManager implements. If more node tests need this, we will enhance FakeNetworkManager.

// mockNetworkManagerWithNamespaceNotFoundError simulates namespace deletion race condition
type mockNetworkManagerWithNamespaceNotFoundError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithNamespaceNotFoundError) GetPrimaryNADForNamespace(namespace string) (string, error) {
	// Namespace absent from the informer cache (not proof of deletion).
	return "", fmt.Errorf("failed to fetch namespace %q: %w", namespace,
		apierrors.NewNotFound(corev1.Resource("namespaces"), namespace))
}

func (m *mockNetworkManagerWithNamespaceNotFoundError) GetActiveNetworkForNamespace(_ string) (util.NetInfo, error) {
	return nil, fmt.Errorf("failed to get namespace %q: %w", "test-ns",
		apierrors.NewNotFound(corev1.Resource("namespaces"), "test-ns"))
}

// mockNetworkManagerWithInvalidPrimaryNetworkError simulates UDN deletion scenario
type mockNetworkManagerWithInvalidPrimaryNetworkError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkError) GetPrimaryNADForNamespace(_ string) (string, error) {
	// just a trigger to ensure GetActiveNetworkForNamespace gets called
	return types.DefaultNetworkName, nil
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

// mockNetworkManagerWithError tests that non-graceful errors are properly propagated
type mockNetworkManagerWithError struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithError) GetPrimaryNADForNamespace(_ string) (string, error) {
	// just a trigger to ensure GetActiveNetworkForNamespace gets called
	return types.DefaultNetworkName, nil
}

func (m *mockNetworkManagerWithError) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, fmt.Errorf("network lookup failed for namespace %q", namespace)
}

// mockNetworkManagerWithInvalidPrimaryNetworkSkip simulates a namespace that
// requires a primary UDN but is currently in invalid primary network state.
type mockNetworkManagerWithInvalidPrimaryNetworkSkip struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkSkip) GetPrimaryNADForNamespace(namespace string) (string, error) {
	return "", util.NewInvalidPrimaryNetworkError(namespace)
}

func (m *mockNetworkManagerWithInvalidPrimaryNetworkSkip) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return nil, util.NewInvalidPrimaryNetworkError(namespace)
}

// mockNetworkManagerWithInactiveNode simulates a UDN where the node is inactive for the network.
type mockNetworkManagerWithInactiveNode struct {
	networkmanager.Interface
}

func (m *mockNetworkManagerWithInactiveNode) GetPrimaryNADForNamespace(_ string) (string, error) {
	return "test-namespace/test-nad", nil
}

func (m *mockNetworkManagerWithInactiveNode) GetNetworkNameForNADKey(_ string) string {
	return "test-udn"
}

func (m *mockNetworkManagerWithInactiveNode) NodeHasNetwork(_, _ string) bool {
	return false
}

func (m *mockNetworkManagerWithInactiveNode) GetActiveNetworkForNamespace(_ string) (util.NetInfo, error) {
	// New code paths resolve activity directly via GetActiveNetworkForNamespace.
	// Returning nil netInfo means "network not active on this node".
	return nil, nil
}

// mockNetworkManagerWithActiveUDN simulates a UDN active on this node.
type mockNetworkManagerWithActiveUDN struct {
	networkmanager.Interface
	netInfo util.NetInfo
}

func (m *mockNetworkManagerWithActiveUDN) GetPrimaryNADForNamespace(_ string) (string, error) {
	return "test-namespace/test-nad", nil
}

func (m *mockNetworkManagerWithActiveUDN) GetNetworkNameForNADKey(_ string) string {
	return m.netInfo.GetNetworkName()
}

func (m *mockNetworkManagerWithActiveUDN) NodeHasNetwork(_, _ string) bool {
	return true
}

func (m *mockNetworkManagerWithActiveUDN) GetActiveNetworkForNamespace(_ string) (util.NetInfo, error) {
	return m.netInfo, nil
}

// verifyNFTablesRule checks if an nftables rule exists and asserts the expected state
func verifyNFTablesRule(nft knftables.Interface, serviceIP string, servicePort, nodePort int32, shouldExist bool, message string) {
	elements, err := nft.ListElements(context.Background(), "map", "nodeports-v4")
	Expect(err).NotTo(HaveOccurred())

	servicePortStr := fmt.Sprintf("%d", servicePort)
	nodePortStr := fmt.Sprintf("%d", nodePort)

	exists := false
	for _, elem := range elements {
		if elem.Key[0] == "tcp" && elem.Key[1] == nodePortStr && elem.Value[0] == serviceIP && elem.Value[1] == servicePortStr {
			exists = true
			break
		}
	}
	if shouldExist {
		Expect(exists).To(BeTrue(), message)
	} else {
		Expect(exists).To(BeFalse(), message)
	}
}

// setupServiceAndEndpointSliceWithRules creates a service and endpoint slice, adds them to npw,
// and verifies nftables rules are created. Returns the created endpoint slice.
func setupServiceAndEndpointSliceWithRules(npw *nodePortWatcher, nft knftables.Interface, svcName, namespace, serviceIP, endpointIP string, servicePort, nodePort int32, annotations map[string]string) *discovery.EndpointSlice {
	// Create service
	service := newService(svcName, namespace, serviceIP,
		[]corev1.ServicePort{{
			Name:       "http",
			Protocol:   corev1.ProtocolTCP,
			Port:       servicePort,
			TargetPort: intstr.FromInt(int(servicePort) + 8000), // e.g., 80 -> 8080
			NodePort:   nodePort,
		}},
		corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

	// Create endpoint slice with endpoints
	epPortName := "http"
	epPortValue := servicePort + 8000 // Match targetPort
	epPortProtocol := corev1.ProtocolTCP
	epSlice := newEndpointSlice(
		svcName,
		namespace,
		[]discovery.Endpoint{
			{
				Addresses: []string{endpointIP},
			},
		},
		[]discovery.EndpointPort{
			{
				Name:     &epPortName,
				Protocol: &epPortProtocol,
				Port:     &epPortValue,
			},
		},
	)

	// Apply annotations if provided
	if len(annotations) > 0 {
		if epSlice.Annotations == nil {
			epSlice.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			epSlice.Annotations[k] = v
		}
	}

	// Add service and endpoint slice
	err := npw.AddService(service)
	Expect(err).NotTo(HaveOccurred())

	err = npw.AddEndpointSlice(epSlice)
	Expect(err).NotTo(HaveOccurred())

	// Verify nftables rules were created
	verifyNFTablesRule(nft, serviceIP, servicePort, nodePort, true, "nftables rule should exist before deletion")

	return epSlice
}

var _ = Describe("DeleteEndpointSlice", func() {
	var (
		fakeClient *util.OVNNodeClientset
		watcher    *factory.WatchFactory
		npw        *nodePortWatcher
		nft        knftables.Interface
	)

	const (
		nodeName      = "test-node"
		testNamespace = "test-namespace"
		testService   = "test-service"
	)

	BeforeEach(func() {
		var err error
		// Restore global default values before each test
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.Gateway.Mode = config.GatewayModeLocal
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeClient = &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(),
		}
		fakeClient.AdminPolicyRouteClient = adminpolicybasedrouteclient.NewSimpleClientset()
		fakeClient.NetworkAttchDefClient = nadfake.NewSimpleClientset()
		fakeClient.UserDefinedNetworkClient = udnfakeclient.NewSimpleClientset()

		watcher, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = watcher.Start()
		Expect(err).NotTo(HaveOccurred())

		// Initialize nodePortWatcher with default network manager
		nft = nodenft.SetFakeNFTablesHelper()
		npw = initFakeNodePortWatcher()
		npw.watchFactory = watcher
		npw.networkManager = networkmanager.Default().Interface()

		// Initialize nodeIPManager (required for GetLocalEligibleEndpointAddresses)
		k := &kube.Kube{KClient: fakeClient.KubeClient}
		npw.nodeIPManager = newAddressManagerInternal(nodeName, k, nil, watcher, nil, nil, false)

		// Since the tests here never call startNodePortWatcher(), we have to call
		// initGatewayNFTables() ourselves to set up the sets, maps, etc.
		err = initGatewayNFTables()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	DescribeTable("preserves Service steering across endpoint changes", func(mode config.GatewayMode, serviceType corev1.ServiceType, nodePort bool, etpLocal, itpLocal, udn bool) {
		config.Gateway.Mode = mode
		config.IPv6Mode = true
		Expect(nft.(*knftables.Fake).ParseDump(nftablesRulesBase + nftablesRulesGatewayServices)).To(Succeed())
		npw.ofm.flowChan = make(chan struct{}, 20)
		networkName := types.DefaultNetworkName
		if udn {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: cnitypes.NetConf{Name: "blue"},
				Topology: types.Layer2Topology, Role: types.NetworkRolePrimary, Subnets: "10.244.0.0/16,fd00:10:244::/48"})
			Expect(err).NotTo(HaveOccurred())
			npw.networkManager = &mockNetworkManagerWithActiveUDN{netInfo: netInfo}
			Expect(npw.ofm.defaultBridge.AddNetworkConfig(netInfo, nil, nil, 1, 1, nil, nil)).To(Succeed())
			npw.ofm.defaultBridge.GetNetworkConfig("blue").OfPortPatch = "udn-patch"
			networkName = "blue"
			npw.dpuMode = true
		}
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl show breth0"})
		Expect(util.SetExec(fexec)).To(Succeed())
		svc := newService(testService, testNamespace, "10.96.0.2",
			[]corev1.ServicePort{{Name: "iperf", Protocol: corev1.ProtocolTCP, Port: 5201, NodePort: 32001}},
			serviceType, []string{"192.0.2.10", "2001:db8::10"}, corev1.ServiceStatus{}, etpLocal, itpLocal)
		if !nodePort {
			svc.Spec.Ports[0].NodePort = 0
			svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
		}
		if serviceType == corev1.ServiceTypeLoadBalancer {
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}, {IP: "2001:db8::20"}}
		}
		svc.Spec.ClusterIPs = []string{"10.96.0.2", "fd00:10:96::2"}
		_, err := fakeClient.KubeClient.CoreV1().Services(testNamespace).Create(context.Background(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := watcher.GetService(testNamespace, testService)
			return err
		}).Should(Succeed())
		Expect(npw.AddService(svc)).To(Succeed())
		originalFlows := npw.ofm.defaultBridge.flows()
		originalNFT := nft.(*knftables.Fake).Dump()
		Expect(originalFlows).NotTo(BeEmpty())
		for len(npw.ofm.flowChan) > 0 {
			<-npw.ofm.flowChan
		}

		// Readiness, locality, and port changes must not affect the rules
		// steering non-host backends into OVN, even with no eligible endpoints.
		for _, address := range []string{"10.244.0.2", "fd00:10:244::2"} {
			ep := newEndpointSlice(testService, testNamespace,
				[]discovery.Endpoint{{Addresses: []string{address}, NodeName: ptr.To(nodeName),
					Conditions: discovery.EndpointConditions{Ready: ptr.To(true)}}},
				[]discovery.EndpointPort{{Name: ptr.To("iperf"), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(int32(5201))}})
			if net.ParseIP(address).To4() == nil {
				ep.AddressType = discovery.AddressTypeIPv6
				ep.Name += "-v6"
			}
			if udn {
				ep.Labels = map[string]string{types.LabelUserDefinedServiceName: testService}
				ep.Annotations = map[string]string{types.UserDefinedNetworkEndpointSliceAnnotation: networkName}
			}
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Create(context.Background(), ep, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			waitForSlice := func(expected *discovery.EndpointSlice) {
				Eventually(func() bool {
					slices, err := watcher.GetServiceEndpointSlices(testNamespace, testService, networkName)
					if err != nil {
						return false
					}
					for _, slice := range slices {
						if slice.Name == expected.Name {
							return reflect.DeepEqual(slice.Endpoints, expected.Endpoints) && reflect.DeepEqual(slice.Ports, expected.Ports)
						}
					}
					return false
				}).Should(BeTrue())
			}
			waitForSlice(ep)
			Expect(npw.AddEndpointSlice(ep)).To(Succeed())
			// Named target ports can change while addresses stay the same.
			changedPort := ep.DeepCopy()
			changedPort.Ports[0].Port = ptr.To(int32(5202))
			_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), changedPort, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			waitForSlice(changedPort)
			Expect(npw.UpdateEndpointSlice(ep, changedPort)).To(Succeed())
			ep = changedPort
			for _, ready := range []bool{false, true} {
				next := ep.DeepCopy()
				next.Endpoints[0].Conditions.Ready = ptr.To(ready)
				if ready {
					next.Endpoints[0].NodeName = ptr.To("other-node")
				}
				_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), next, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				waitForSlice(next)
				Expect(npw.UpdateEndpointSlice(ep, next)).To(Succeed())
				ep = next
			}
			Expect(fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Delete(context.Background(), ep.Name, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() bool {
				slices, err := watcher.GetServiceEndpointSlices(testNamespace, testService, networkName)
				return err == nil && len(slices) == 0
			}).Should(BeTrue())
			Expect(npw.DeleteEndpointSlice(ep)).To(Succeed())
			Expect(npw.ofm.flowChan).To(BeEmpty(), "endpoint changes must not schedule removal/reinstallation of unchanged steering")
			Expect(npw.ofm.defaultBridge.flows()).To(ConsistOf(originalFlows))
			Expect(nft.(*knftables.Fake).Dump()).To(Equal(originalNFT))
		}

		// Actual Service deletion must still remove steering and host rules.
		Expect(delServiceRules(svc, nil, npw)).To(Succeed())
		Expect(npw.ofm.flowChan).NotTo(BeEmpty())
		Expect(npw.ofm.defaultBridge.flows()).To(BeEmpty())
		verifyNFTablesRule(nft, "10.96.0.2", 5201, 32001, false, "deleted Service must not retain host rules")
	},
		Entry("shared NodePort", config.GatewayModeShared, corev1.ServiceTypeNodePort, true, false, false, false),
		Entry("shared NodePort ETP Local", config.GatewayModeShared, corev1.ServiceTypeNodePort, true, true, false, false),
		Entry("shared NodePort ITP Local", config.GatewayModeShared, corev1.ServiceTypeNodePort, true, false, true, false),
		Entry("shared LoadBalancer", config.GatewayModeShared, corev1.ServiceTypeLoadBalancer, true, false, false, false),
		Entry("shared LoadBalancer without NodePorts", config.GatewayModeShared, corev1.ServiceTypeLoadBalancer, false, true, false, false),
		Entry("shared ClusterIP with ExternalIPs", config.GatewayModeShared, corev1.ServiceTypeClusterIP, false, false, false, false),
		Entry("local NodePort", config.GatewayModeLocal, corev1.ServiceTypeNodePort, true, true, false, false),
		Entry("local LoadBalancer without NodePorts", config.GatewayModeLocal, corev1.ServiceTypeLoadBalancer, false, true, false, false),
		Entry("shared primary UDN", config.GatewayModeShared, corev1.ServiceTypeNodePort, true, false, false, true),
	)

	DescribeTable("replaces local host-network forwarding without removing Service steering", func(mode config.GatewayMode, withNodePort bool) {
		config.Gateway.Mode = mode
		config.IPv6Mode = true
		Expect(nft.(*knftables.Fake).ParseDump(nftablesRulesBase + nftablesRulesGatewayServices)).To(Succeed())
		npw.nodeIPManager.cidrs.Insert("192.168.18.15/32", "fd00::15/128")
		svc := newService(testService, testNamespace, "10.96.0.2",
			[]corev1.ServicePort{{Name: "iperf", Protocol: corev1.ProtocolTCP, Port: 5201, NodePort: 32001}},
			corev1.ServiceTypeLoadBalancer, []string{"192.0.2.10", "2001:db8::10"},
			corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}}}, true, false)
		svc.Spec.ClusterIPs = []string{"10.96.0.2", "fd00:10:96::2"}
		if !withNodePort {
			svc.Spec.Ports[0].NodePort = 0
			svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
		}
		updating := false
		fexec := ovntest.NewFakeExec()
		fexec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl show breth0", Action: func() error {
			if updating {
				// This command runs while the replacement is being built. A
				// delete/add implementation would already have removed these.
				Expect(npw.ofm.getFlowsByKey("External_test-namespace_test-service_192.0.2.10_5201")).NotTo(BeEmpty())
				Expect(npw.ofm.getFlowsByKey("Ingress_test-namespace_test-service_192.0.2.20_5201")).NotTo(BeEmpty())
			}
			return nil
		}}, 4)
		Expect(util.SetExec(fexec)).To(Succeed())
		Expect(addServiceRules(svc, &util.DefaultNetInfo{}, nil, false, npw)).To(Succeed())
		originalFlows := npw.ofm.defaultBridge.flows()
		originalNFT := nft.(*knftables.Fake).Dump()
		updating = true
		old := &serviceConfig{service: svc}
		for _, endpoints := range []util.PortToLBEndpoints{
			{"TCP/iperf": {{Port: 8080, V4IPs: []string{"192.168.18.15"}}, {Port: 8081, V4IPs: []string{"192.168.18.15"}}}},
			{"TCP/iperf": {{Port: 9090, V4IPs: []string{"192.168.18.15"}, V6IPs: []string{"fd00::15"}}}},
			nil,
		} {
			hasHost := endpoints != nil
			Expect(npw.updateEndpointRules(svc, &util.DefaultNetInfo{}, old, endpoints, hasHost)).To(Succeed())
			if hasHost {
				Expect(npw.ofm.defaultBridge.flows()).NotTo(ConsistOf(originalFlows))
			}
			old = &serviceConfig{service: svc, localEndpoints: endpoints, hasLocalHostNetworkEp: hasHost}
		}
		Expect(npw.ofm.defaultBridge.flows()).To(ConsistOf(originalFlows))
		Expect(npw.ofm.defaultBridge.groupCache).To(BeEmpty(), "old direct-host groups must be removed")
		Expect(nft.(*knftables.Fake).Dump()).To(Equal(originalNFT))
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	},
		Entry("shared LoadBalancer with NodePorts", config.GatewayModeShared, true),
		Entry("shared LoadBalancer without NodePorts", config.GatewayModeShared, false),
		Entry("local LoadBalancer with NodePorts", config.GatewayModeLocal, true),
		Entry("local LoadBalancer without NodePorts", config.GatewayModeLocal, false),
	)

	DescribeTable("repairs partial endpoint rule updates after nftables failure", func(mode config.GatewayMode, withNodePort, failDelete, deleteSlices bool) {
		config.Gateway.Mode = mode
		config.IPv6Mode = true
		fakeNFT := nft.(*knftables.Fake)
		Expect(fakeNFT.ParseDump(nftablesRulesBase + nftablesRulesGatewayServices)).To(Succeed())
		npw.nodeIPManager.cidrs.Insert("192.168.18.15/32", "fd00::15/128")
		npw.ofm.flowChan = make(chan struct{}, 20)
		svc := newService(testService, testNamespace, "10.96.0.2",
			[]corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(8080), NodePort: 32001}},
			corev1.ServiceTypeLoadBalancer, []string{"192.0.2.10", "2001:db8::10"},
			corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}, {IP: "2001:db8::20"}}}}, true, true)
		svc.Spec.ClusterIPs = []string{"10.96.0.2", "fd00:10:96::2"}
		if !withNodePort {
			svc.Spec.Ports[0].NodePort = 0
			svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
		}
		_, err := fakeClient.KubeClient.CoreV1().Services(testNamespace).Create(context.Background(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := watcher.GetService(testNamespace, testService)
			return err
		}).Should(Succeed())
		fexec := ovntest.NewFakeExec()
		fexec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl show breth0"}, 3)
		Expect(util.SetExec(fexec)).To(Succeed())
		Expect(npw.AddService(svc)).To(Succeed())
		originalFlows := npw.ofm.defaultBridge.flows()
		originalNFT := fakeNFT.Dump()

		var slices []*discovery.EndpointSlice
		for _, address := range []string{"192.168.18.15", "fd00::15"} {
			// Multiple target ports exercise direct-host group cleanup as well as flows.
			for _, targetPort := range []int32{8080, 9090} {
				ep := newEndpointSlice(testService, testNamespace,
					[]discovery.Endpoint{{Addresses: []string{address}, NodeName: ptr.To(nodeName), Conditions: discovery.EndpointConditions{Ready: ptr.To(true)}}},
					[]discovery.EndpointPort{{Name: ptr.To("http"), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(targetPort)}})
				if net.ParseIP(address).To4() == nil {
					ep.AddressType = discovery.AddressTypeIPv6
				}
				ep.Name = fmt.Sprintf("http-%s-%d", strings.ToLower(string(ep.AddressType)), targetPort)
				_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Create(context.Background(), ep, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				slices = append(slices, ep)
			}
		}
		Eventually(func() int {
			eps, err := watcher.GetServiceEndpointSlices(testNamespace, testService, types.DefaultNetworkName)
			Expect(err).NotTo(HaveOccurred())
			return len(eps)
		}).Should(Equal(len(slices)))

		// Removing an old-only set fails deletion; removing a new-only map
		// allows deletion to commit but fails the subsequent addition.
		markSet := fakeNFT.Table.Sets[nftablesITPMarkSetV4]
		redirectMap := fakeNFT.Table.Maps[nftablesITPServicesMapV4]
		if failDelete {
			delete(fakeNFT.Table.Sets, nftablesITPMarkSetV4)
		} else {
			delete(fakeNFT.Table.Maps, nftablesITPServicesMapV4)
		}
		Expect(npw.AddEndpointSlice(slices[0])).To(MatchError(ContainSubstring("no such")))
		Expect(npw.ofm.defaultBridge.flows()).NotTo(ConsistOf(originalFlows))
		Expect(npw.ofm.defaultBridge.groupCache).NotTo(BeEmpty(), "host forwarding must be installed before nftables fails")
		if failDelete {
			fakeNFT.Table.Sets[nftablesITPMarkSetV4] = markSet
		} else {
			fakeNFT.Table.Maps[nftablesITPServicesMapV4] = redirectMap
		}

		// Withdraw every host endpoint before retrying, returning to the last
		// successfully cached endpoint state through either event handler.
		for _, ep := range slices {
			if deleteSlices {
				Expect(fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Delete(context.Background(), ep.Name, metav1.DeleteOptions{})).To(Succeed())
			} else {
				empty := ep.DeepCopy()
				empty.Endpoints = nil
				_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), empty, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		Eventually(func() int {
			eps, err := watcher.GetServiceEndpointSlices(testNamespace, testService, types.DefaultNetworkName)
			Expect(err).NotTo(HaveOccurred())
			count := 0
			for _, ep := range eps {
				count += len(ep.Endpoints)
			}
			return count
		}).Should(BeZero())
		if deleteSlices {
			Expect(npw.DeleteEndpointSlice(slices[0])).To(Succeed())
		} else {
			empty := slices[0].DeepCopy()
			empty.Endpoints = nil
			Expect(npw.UpdateEndpointSlice(slices[0], empty)).To(Succeed())
		}
		Expect(npw.ofm.defaultBridge.flows()).To(ConsistOf(originalFlows))
		Expect(npw.ofm.defaultBridge.groupCache).To(BeEmpty(), "withdrawn endpoints must not retain direct-host groups")
		Expect(fakeNFT.Dump()).To(Equal(originalNFT), "restore deleted rules and remove any stale host rules")
		for len(npw.ofm.flowChan) > 0 {
			<-npw.ofm.flowChan
		}
		Expect(npw.AddEndpointSlice(slices[0])).To(Succeed())
		Expect(npw.ofm.flowChan).To(BeEmpty(), "successful reconciliation must clear the dirty state")
		Expect(fakeNFT.Dump()).To(Equal(originalNFT))
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	},
		Entry("shared NodePort, delete failure, withdrawal by deletion", config.GatewayModeShared, true, true, true),
		Entry("shared NodePort, add failure, withdrawal by update", config.GatewayModeShared, true, false, false),
		Entry("local NodePort, delete failure, withdrawal by update", config.GatewayModeLocal, true, true, false),
		Entry("local NodePort, add failure, withdrawal by deletion", config.GatewayModeLocal, true, false, true),
		Entry("shared without NodePort, delete failure, withdrawal by update", config.GatewayModeShared, false, true, false),
		Entry("shared without NodePort, add failure, withdrawal by deletion", config.GatewayModeShared, false, false, true),
		Entry("local without NodePort, delete failure, withdrawal by deletion", config.GatewayModeLocal, false, true, true),
		Entry("local without NodePort, add failure, withdrawal by update", config.GatewayModeLocal, false, false, false),
	)

	DescribeTable("cleans up committed nftables rules after a partial OpenFlow failure", func(mode config.GatewayMode) {
		config.Gateway.Mode = mode
		config.IPv6Mode = true
		fakeNFT := nft.(*knftables.Fake)
		Expect(fakeNFT.ParseDump(nftablesRulesBase + nftablesRulesGatewayServices)).To(Succeed())
		npw.nodeIPManager.cidrs.Insert("192.168.18.15/32", "fd00::15/128")
		svc := newService(testService, testNamespace, "10.96.0.2",
			[]corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(8080), NodePort: 32001}},
			corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, true, true)
		svc.Spec.ClusterIPs = []string{"10.96.0.2", "fd00:10:96::2"}
		Expect(addServiceRules(svc, &util.DefaultNetInfo{}, nil, false, npw)).To(Succeed())
		originalFlows := npw.ofm.defaultBridge.flows()
		originalNFT := fakeNFT.Dump()
		old := &serviceConfig{service: svc}
		endpoints := util.PortToLBEndpoints{"TCP/http": {
			{Port: 8080, V4IPs: []string{"192.168.18.15"}, V6IPs: []string{"fd00::15"}},
			{Port: 9090, V4IPs: []string{"192.168.18.15"}, V6IPs: []string{"fd00::15"}},
		}}
		// IPv4 flows are installed, IPv6 fails, and nftables still commits.
		npw.gatewayIPv6 = ""
		Expect(npw.updateEndpointRules(svc, &util.DefaultNetInfo{}, old, endpoints, true)).To(MatchError(ContainSubstring("gateway IP not available for protocol tcp6")))
		Expect(npw.ofm.defaultBridge.groupCache).NotTo(BeEmpty())
		Expect(fakeNFT.Dump()).NotTo(Equal(originalNFT))
		Expect(npw.updateEndpointRules(svc, &util.DefaultNetInfo{}, old, nil, false)).To(Succeed())
		Expect(npw.ofm.defaultBridge.flows()).To(ConsistOf(originalFlows))
		Expect(npw.ofm.defaultBridge.groupCache).To(BeEmpty())
		Expect(fakeNFT.Dump()).To(Equal(originalNFT), "the committed host configuration must be remembered for cleanup")
	},
		Entry("shared gateway", config.GatewayModeShared),
		Entry("local gateway", config.GatewayModeLocal),
	)

	DescribeTable("falls back for a missing Service port while another keeps host-network backends", func(mode config.GatewayMode, withNodePorts, deleteSlices bool) {
		config.Gateway.Mode = mode
		config.IPv6Mode = true
		Expect(nft.(*knftables.Fake).ParseDump(nftablesRulesBase + nftablesRulesGatewayServices)).To(Succeed())
		npw.nodeIPManager.cidrs.Insert("192.168.18.15/32", "fd00::15/128")
		svc := newService(testService, testNamespace, "10.96.0.2",
			[]corev1.ServicePort{
				{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 32001},
				{Name: "metrics", Protocol: corev1.ProtocolTCP, Port: 81, NodePort: 32002},
			}, corev1.ServiceTypeLoadBalancer, []string{"192.0.2.10", "2001:db8::10"},
			corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}, {IP: "2001:db8::20"}}}}, true, false)
		svc.Spec.ClusterIPs = []string{"10.96.0.2", "fd00:10:96::2"}
		if !withNodePorts {
			for i := range svc.Spec.Ports {
				svc.Spec.Ports[i].NodePort = 0
			}
			svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
		}
		_, err := fakeClient.KubeClient.CoreV1().Services(testNamespace).Create(context.Background(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := watcher.GetService(testNamespace, testService)
			return err
		}).Should(Succeed())
		fexec := ovntest.NewFakeExec()
		// Two ports at initial setup, adding host backends, and four slice changes.
		fexec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl show breth0"}, 12)
		Expect(util.SetExec(fexec)).To(Succeed())
		Expect(npw.AddService(svc)).To(Succeed())
		normalFlows := make(map[string][]string)
		for key, flows := range npw.ofm.defaultBridge.flowCache {
			normalFlows[key] = flows
		}
		var httpSlices []*discovery.EndpointSlice
		for _, servicePort := range svc.Spec.Ports {
			for _, address := range []string{"192.168.18.15", "fd00::15"} {
				// Multiple target ports exercise group cleanup as well as flows.
				for _, targetPort := range []int32{servicePort.Port + 8000, servicePort.Port + 9000} {
					ep := newEndpointSlice(testService, testNamespace,
						[]discovery.Endpoint{{Addresses: []string{address}, NodeName: ptr.To(nodeName), Conditions: discovery.EndpointConditions{Ready: ptr.To(true)}}},
						[]discovery.EndpointPort{{Name: ptr.To(servicePort.Name), Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(targetPort)}})
					if net.ParseIP(address).To4() == nil {
						ep.AddressType = discovery.AddressTypeIPv6
					}
					ep.Name = fmt.Sprintf("%s-%s-%d", servicePort.Name, strings.ToLower(string(ep.AddressType)), targetPort)
					_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Create(context.Background(), ep, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					if servicePort.Name == "http" {
						httpSlices = append(httpSlices, ep)
					}
				}
			}
		}
		Eventually(func() int {
			slices, err := watcher.GetServiceEndpointSlices(testNamespace, testService, types.DefaultNetworkName)
			Expect(err).NotTo(HaveOccurred())
			return len(slices)
		}).Should(Equal(8))
		Expect(npw.AddEndpointSlice(httpSlices[0])).To(Succeed())
		hostFlows := make(map[string][]string)
		for key, flows := range npw.ofm.defaultBridge.flowCache {
			hostFlows[key] = flows
		}
		hostGroups := make(map[string][]string)
		for key, groups := range npw.ofm.defaultBridge.groupCache {
			hostGroups[key] = groups
		}
		Expect(hostGroups).NotTo(BeEmpty())
		for _, ep := range httpSlices {
			if deleteSlices {
				Expect(fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Delete(context.Background(), ep.Name, metav1.DeleteOptions{})).To(Succeed())
			} else {
				next := ep.DeepCopy()
				next.Endpoints = nil
				_, err = fakeClient.KubeClient.DiscoveryV1().EndpointSlices(testNamespace).Update(context.Background(), next, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			Eventually(func() bool {
				current, err := watcher.GetEndpointSlice(testNamespace, ep.Name)
				return apierrors.IsNotFound(err) || err == nil && len(current.Endpoints) == 0
			}).Should(BeTrue())
			if deleteSlices {
				Expect(npw.DeleteEndpointSlice(ep)).To(Succeed())
			} else {
				next := ep.DeepCopy()
				next.Endpoints = nil
				Expect(npw.UpdateEndpointSlice(ep, next)).To(Succeed())
			}
		}
		// Restore the complete normal HTTP entries (including ARP/NDP bypass)
		// while leaving the metrics direct-host forwarding unchanged.
		for key, flows := range hostFlows {
			if strings.HasSuffix(key, "_32001") || strings.HasSuffix(key, "_80") {
				Expect(npw.ofm.getFlowsByKey(key)).To(ConsistOf(normalFlows[key]), key)
			} else {
				Expect(npw.ofm.getFlowsByKey(key)).To(ConsistOf(flows), key)
			}
		}
		for key, groups := range hostGroups {
			if strings.HasSuffix(key, "_32001") || strings.HasSuffix(key, "_80") {
				Expect(npw.ofm.getGroupsByKey(key)).To(BeEmpty(), key)
			} else {
				Expect(npw.ofm.getGroupsByKey(key)).To(Equal(groups), key)
			}
		}
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	},
		Entry("shared delete with NodePorts", config.GatewayModeShared, true, true),
		Entry("shared update with NodePorts", config.GatewayModeShared, true, false),
		Entry("shared delete without NodePorts", config.GatewayModeShared, false, true),
		Entry("shared update without NodePorts", config.GatewayModeShared, false, false),
		Entry("local delete with NodePorts", config.GatewayModeLocal, true, true),
		Entry("local update with NodePorts", config.GatewayModeLocal, true, false),
		Entry("local delete without NodePorts", config.GatewayModeLocal, false, true),
		Entry("local update without NodePorts", config.GatewayModeLocal, false, false),
	)

	Context("when UDN is deleted before processing endpoint slice", func() {
		It("should execute delServiceRules and gracefully skip addServiceRules", func() {
			// Setup service and endpoint slice with nftables rules
			// Add UDN annotation to simulate a mirrored UDN EndpointSlice
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace, "10.96.0.2", "10.244.0.2", 80, 30081,
				map[string]string{types.UserDefinedNetworkEndpointSliceAnnotation: "test-udn"})

			// Replace network manager with one that returns InvalidPrimaryNetworkError
			// This simulates UDN deletion scenario
			npw.networkManager = &mockNetworkManagerWithInvalidPrimaryNetworkError{}

			// Call DeleteEndpointSlice - should not return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should gracefully handle UDN deletion (no error)
			Expect(err).NotTo(HaveOccurred())

			// nftables rules should be deleted even when UDN is deleted
			verifyNFTablesRule(nft, "10.96.0.2", 80, 30081, false, "nftables rule should be deleted even when UDN is deleted")
		})
	})

	Context("when network lookup returns other errors", func() {
		It("should execute delServiceRules but return error from network lookup", func() {
			// Setup service and endpoint slice with nftables rules
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace, "10.96.0.3", "10.244.0.3", 80, 30082, nil)

			// Replace network manager with one that returns a generic error
			npw.networkManager = &mockNetworkManagerWithError{}

			// Call DeleteEndpointSlice - should return error
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return error for other types of failures
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("error getting active network"))
			Expect(err.Error()).To(ContainSubstring(testNamespace))
			Expect(err.Error()).To(ContainSubstring(testService))

			// nftables rules should still be deleted even when error is returned
			verifyNFTablesRule(nft, "10.96.0.3", 80, 30082, false, "nftables rule should be deleted even when error occurs")
		})
	})

	Context("when service does not exist in cache", func() {
		It("should return nil without error", func() {
			// Create endpoint slice (but no service in cache)
			epSlice := newEndpointSlice(testService, testNamespace, nil, nil)

			// Call DeleteEndpointSlice when service not in cache
			err := npw.DeleteEndpointSlice(epSlice)

			// Should return nil (no-op when not in cache)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("when namespace is deleted before processing endpoint slice", func() {
		It("should clean up old rules even when namespace is gone", func() {
			// Setup service and endpoint slice with nftables rules
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace, "10.96.0.10", "10.244.0.5", 80, 30090, nil)

			// Simulate namespace not found error
			npw.networkManager = &mockNetworkManagerWithNamespaceNotFoundError{}
			err := npw.DeleteEndpointSlice(epSlice)
			// Verify no error (graceful handling)
			Expect(err).NotTo(HaveOccurred())

			// nftables rules should be deleted even though namespace lookup failed
			verifyNFTablesRule(nft, "10.96.0.10", 80, 30090, false, "nftables rule should be deleted even when namespace lookup fails")
		})
	})
})

var _ = Describe("SyncServices", func() {
	var (
		fakeClient *util.OVNNodeClientset
		watcher    *factory.WatchFactory
		npw        *nodePortWatcher
		nft        knftables.Interface
	)

	const (
		nodeName      = "test-node"
		testNamespace = "test-namespace"
		testService   = "test-service"
	)

	BeforeEach(func() {
		var err error
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.Gateway.Mode = config.GatewayModeLocal
		config.IPv4Mode = true
		config.IPv6Mode = false

		fakeClient = &util.OVNNodeClientset{
			KubeClient: fake.NewSimpleClientset(),
		}
		fakeClient.AdminPolicyRouteClient = adminpolicybasedrouteclient.NewSimpleClientset()
		fakeClient.NetworkAttchDefClient = nadfake.NewSimpleClientset()
		fakeClient.UserDefinedNetworkClient = udnfakeclient.NewSimpleClientset()

		watcher, err = factory.NewNodeWatchFactory(fakeClient, nodeName)
		Expect(err).NotTo(HaveOccurred())
		err = watcher.Start()
		Expect(err).NotTo(HaveOccurred())

		nft = nodenft.SetFakeNFTablesHelper()
		npw = initFakeNodePortWatcher()
		npw.watchFactory = watcher
		npw.networkManager = networkmanager.Default().Interface()

		k := &kube.Kube{KClient: fakeClient.KubeClient}
		npw.nodeIPManager = newAddressManagerInternal(nodeName, k, nil, watcher, nil, nil, false)

		// Since the tests here never call startNodePortWatcher(), we have to call
		// initGatewayNFTables() ourselves to set up the sets, maps, etc.
		err = initGatewayNFTables()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		watcher.Shutdown()
	})

	Context("when namespace has invalid primary network", func() {
		It("should skip service sync without failing startup", func() {
			service := newService(testService, testNamespace, "10.96.0.20",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30091,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.networkManager = &mockNetworkManagerWithInvalidPrimaryNetworkSkip{}

			err := npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyNFTablesRule(nft, "10.96.0.20", 80, 30091, false,
				"nftables rule should not be created when primary network is invalid")
		})
	})

	Context("when namespace is absent from informer cache", func() {
		It("should skip service sync without failing startup", func() {
			service := newService(testService, testNamespace, "10.96.0.21",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30094,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.networkManager = &mockNetworkManagerWithNamespaceNotFoundError{}

			err := npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyNFTablesRule(nft, "10.96.0.21", 80, 30094, false,
				"nftables rule should not be created when namespace lookup returns NotFound")
		})
	})

	Context("when UDN is inactive on this node", func() {
		It("should skip service sync without installing rules", func() {
			service := newService(testService, testNamespace, "10.96.0.30",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30092,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.networkManager = &mockNetworkManagerWithInactiveNode{}

			err := npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyNFTablesRule(nft, "10.96.0.30", 80, 30092, false,
				"nftables rule should not be created when UDN is inactive on this node")
		})
	})

	Context("when UDN is active on this node", func() {
		It("should install nodeport rules", func() {
			// Avoid openflow dependency in this test.
			config.Gateway.AllowNoUplink = true
			npw.ofportPhys = ""

			service := newService(testService, testNamespace, "10.96.0.40",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30093,
				}},
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			nad := ovntest.GenerateNAD("test-udn", "test-nad", testNamespace, types.Layer3Topology, "10.1.0.0/16", types.NetworkRolePrimary)
			netInfo, err := util.ParseNADInfo(nad)
			Expect(err).NotTo(HaveOccurred())
			npw.networkManager = &mockNetworkManagerWithActiveUDN{netInfo: netInfo}

			nodeName := npw.nodeIPManager.nodeName
			epPortName := "http"
			epPortValue := int32(8080)
			epPortProtocol := corev1.ProtocolTCP
			epSlice := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testService + "ab23",
					Namespace: testNamespace,
					Labels: map[string]string{
						types.LabelUserDefinedServiceName: testService,
					},
					Annotations: map[string]string{
						types.UserDefinedNetworkEndpointSliceAnnotation: netInfo.GetNetworkName(),
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{{
					Addresses: []string{"10.244.0.9"},
					NodeName:  &nodeName,
				}},
				Ports: []discovery.EndpointPort{{
					Name:     &epPortName,
					Protocol: &epPortProtocol,
					Port:     &epPortValue,
				}},
			}
			Expect(watcher.EndpointSliceInformer().GetStore().Add(epSlice)).To(Succeed())

			err = npw.SyncServices([]interface{}{service})
			Expect(err).NotTo(HaveOccurred())

			verifyNFTablesRule(nft, "10.96.0.40", 80, 30093, true,
				"nftables rule should be created when UDN is active on this node")
		})
	})
})

var _ = Describe("masqueradeReconciler", func() {
	var (
		netlinkMock *utilMocks.NetLinkOps
		rm          *routemanager.Controller
		wf          factory.NodeWatchFactory
	)

	BeforeEach(func() {
		netlinkMock = new(utilMocks.NetLinkOps)
		util.SetNetLinkOpMockInst(netlinkMock)
		rm = routemanager.NewController()
		fakeClient := fake.NewSimpleClientset()
		var err error
		wf, err = factory.NewNodeWatchFactory(&util.OVNNodeClientset{KubeClient: fakeClient}, "node1")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		util.ResetNetLinkOpMockInst()
		wf.Shutdown()
	})

	It("skips reconciliation when interface name is empty", func() {
		config.Gateway.Interface = ""
		r := &masqueradeReconciler{nodeName: "node1", routeManager: rm, watchFactory: wf}
		err := r.ensure()
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns error when interface does not exist", func() {
		config.Gateway.Interface = "nonexistent0"
		r := &masqueradeReconciler{nodeName: "node1", routeManager: rm, watchFactory: wf}
		netlinkMock.On("LinkByName", "nonexistent0").Return(nil, fmt.Errorf("no such network interface"))
		err := r.ensure()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("interface nonexistent0 not found"))
	})

	It("skips reconciliation when mutex is already held", func() {
		config.Gateway.Interface = "breth0"
		r := &masqueradeReconciler{nodeName: "node1", routeManager: rm, watchFactory: wf}
		r.mu.Lock()
		defer r.mu.Unlock()

		err := r.ensure()
		Expect(err).NotTo(HaveOccurred())
		netlinkMock.AssertNotCalled(GinkgoT(), "LinkByName")
	})

	It("reads config.Gateway.Interface at call time, not at construction", func() {
		config.Gateway.Interface = "placeholder"
		r := &masqueradeReconciler{nodeName: "node1", routeManager: rm, watchFactory: wf}

		netlinkMock.On("LinkByName", "placeholder").Return(nil, fmt.Errorf("no such device"))
		netlinkMock.On("LinkByName", "resolved0").Return(nil, fmt.Errorf("no such device"))

		err := r.ensure()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("interface placeholder not found"))

		config.Gateway.Interface = "resolved0"
		err = r.ensure()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("interface resolved0 not found"))
	})

	It("takes fast path when link index matches and masquerade IP is present", func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.Gateway.Interface = "breth0"

		linkMock := new(netlink_mocks.Link)
		linkMock.On("Attrs").Return(&netlink.LinkAttrs{Index: 10, Name: "breth0"})

		_, masqSubnet, _ := net.ParseCIDR(config.Gateway.V4MasqueradeSubnet)
		masqSubnet.IP = config.Gateway.MasqueradeIPs.V4HostMasqueradeIP
		netlinkMock.On("LinkByName", "breth0").Return(linkMock, nil)
		netlinkMock.On("AddrList", linkMock, netlink.FAMILY_V4).Return([]netlink.Addr{
			{IPNet: masqSubnet},
		}, nil)

		r := &masqueradeReconciler{nodeName: "node1", routeManager: rm, watchFactory: wf, lastLinkIndex: 10}
		err := r.ensure()
		Expect(err).NotTo(HaveOccurred())
		Expect(r.lastLinkIndex).To(Equal(10))
	})
})
