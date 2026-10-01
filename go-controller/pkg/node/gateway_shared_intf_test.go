// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package node

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/stretchr/testify/mock"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/knftables"

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
	verifyNFTablesRuleInMap(nft, "nodeports-v4", serviceIP, servicePort, nodePort, shouldExist, message)
}

func verifyNFTablesRuleInMap(nft knftables.Interface, mapName, serviceIP string, servicePort, nodePort int32, shouldExist bool, message string) {
	elements, err := nft.ListElements(context.Background(), "map", mapName)
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

type blockingServiceLookupWatchFactory struct {
	factory.NodeWatchFactory
	service            *corev1.Service
	serviceErr         error
	endpointSlices     []*discovery.EndpointSlice
	endpointSlicesErr  error
	serviceLookupStart chan struct{}
	releaseServiceRead chan struct{}
}

type updateBlockingNetworkManager struct {
	networkmanager.Interface
	blockNextLookup bool
	lookupStarted   chan struct{}
	releaseLookup   chan struct{}
	lookupCalls     chan struct{}
}

func (m *updateBlockingNetworkManager) GetActiveNetworkForNamespace(string) (util.NetInfo, error) {
	select {
	case m.lookupCalls <- struct{}{}:
	default:
	}
	if m.blockNextLookup {
		m.blockNextLookup = false
		close(m.lookupStarted)
		<-m.releaseLookup
	}
	return &util.DefaultNetInfo{}, nil
}

type serviceEndpointSnapshotWatchFactory struct {
	factory.NodeWatchFactory
	service        *corev1.Service
	endpointSlices []*discovery.EndpointSlice
}

func (wf *serviceEndpointSnapshotWatchFactory) GetService(string, string) (*corev1.Service, error) {
	return wf.service, nil
}

func (wf *serviceEndpointSnapshotWatchFactory) GetServiceEndpointSlices(string, string, string) ([]*discovery.EndpointSlice, error) {
	return wf.endpointSlices, nil
}

func (wf *blockingServiceLookupWatchFactory) GetService(_, _ string) (*corev1.Service, error) {
	close(wf.serviceLookupStart)
	select {
	case <-wf.releaseServiceRead:
		return wf.service, wf.serviceErr
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timed out waiting to release the blocked Service lookup")
	}
}

func (wf *blockingServiceLookupWatchFactory) GetServiceEndpointSlices(_, _, _ string) ([]*discovery.EndpointSlice, error) {
	return wf.endpointSlices, wf.endpointSlicesErr
}

func waitForTestWorker(done <-chan struct{}, description string) {
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		GinkgoT().Errorf("timed out waiting for %s worker during cleanup", description)
	}
}

var _ = Describe("mergePortToLBEndpoints", func() {
	It("unions endpoint IPs by service port and target port", func() {
		oldEndpoints := util.PortToLBEndpoints{
			"TCP/http": {
				{Port: 8080, V4IPs: []string{"10.0.0.1"}, V6IPs: []string{"2001:db8::1"}},
				{Port: 9090, V4IPs: []string{"10.0.0.2"}},
			},
		}
		newEndpoints := util.PortToLBEndpoints{
			"TCP/http": {
				{Port: 8080, V4IPs: []string{"10.0.0.1", "10.0.0.3"}, V6IPs: []string{"2001:db8::2"}},
				{Port: 9090, V6IPs: []string{"2001:db8::3"}},
			},
			"UDP/dns": {
				{Port: 5353, V4IPs: []string{"10.0.0.4"}},
			},
		}

		merged := mergePortToLBEndpoints(oldEndpoints, newEndpoints)

		Expect(merged).To(Equal(util.PortToLBEndpoints{
			"TCP/http": {
				{Port: 8080, V4IPs: []string{"10.0.0.1", "10.0.0.3"}, V6IPs: []string{"2001:db8::1", "2001:db8::2"}},
				{Port: 9090, V4IPs: []string{"10.0.0.2"}, V6IPs: []string{"2001:db8::3"}},
			},
			"UDP/dns": {
				{Port: 5353, V4IPs: []string{"10.0.0.4"}},
			},
		}), "merge should preserve both old and current endpoints, without duplicates")
	})
})

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

	Context("when a Service is deleted during endpoint slice add", func() {
		It("does not cache endpoint rules after service deletion", func() {
			// Seed service rules and the service cache, then pause a subsequent
			// endpoint update at the informer Service lookup.
			setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace,
				"10.96.0.20", "10.244.0.20", 80, 30091, nil)
			name := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
			npw.serviceInfoLock.Lock()
			npw.serviceInfo[name].hasLocalHostNetworkEp = true
			npw.serviceInfoLock.Unlock()
			service := newService(testService, testNamespace, "10.96.0.20",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30091,
				}}, corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			portName := "http"
			port := int32(8080)
			protocol := corev1.ProtocolTCP
			updatedEndpointSlice := newEndpointSlice(testService, testNamespace,
				[]discovery.Endpoint{{Addresses: []string{"10.244.0.21"}}},
				[]discovery.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port}})
			blockingWatchFactory := &blockingServiceLookupWatchFactory{
				NodeWatchFactory:   watcher,
				service:            service,
				endpointSlices:     []*discovery.EndpointSlice{updatedEndpointSlice},
				serviceLookupStart: make(chan struct{}),
				releaseServiceRead: make(chan struct{}),
			}
			npw.watchFactory = blockingWatchFactory
			var addWorkerStarted, deleteWorkerStarted bool
			addWorkerFinished := make(chan struct{})
			deleteWorkerFinished := make(chan struct{})
			releaseServiceRead := func() {
				select {
				case <-blockingWatchFactory.releaseServiceRead:
				default:
					close(blockingWatchFactory.releaseServiceRead)
				}
			}
			defer func() {
				releaseServiceRead()
				if addWorkerStarted {
					waitForTestWorker(addWorkerFinished, "AddEndpointSlice")
				}
				if deleteWorkerStarted {
					waitForTestWorker(deleteWorkerFinished, "DeleteService")
				}
			}()

			addDone := make(chan error, 1)
			addWorkerStarted = true
			go func() {
				defer close(addWorkerFinished)
				addDone <- npw.AddEndpointSlice(updatedEndpointSlice)
			}()
			Eventually(blockingWatchFactory.serviceLookupStart).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(BeClosed(), "AddEndpointSlice should reach the blocked Service lookup")
			addHoldsCacheLock := !npw.serviceInfoLock.TryLock()
			if !addHoldsCacheLock {
				npw.serviceInfoLock.Unlock()
			}
			Expect(addHoldsCacheLock).To(BeTrue(), "AddEndpointSlice should hold the cache lock during Service lookup")

			deleteService := service.DeepCopy()
			deleteService.Spec.Ports = nil // Avoid conntrack operations; rule teardown uses the cached Service.
			deleteDone := make(chan error, 1)
			deleteWorkerStarted = true
			go func() {
				defer close(deleteWorkerFinished)
				deleteDone <- npw.DeleteService(deleteService)
			}()
			releaseServiceRead()
			var addErr, deleteErr error
			Eventually(addDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&addErr), "AddEndpointSlice should finish after the Service lookup is released")
			Eventually(deleteDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&deleteErr), "DeleteService should finish after AddEndpointSlice releases the cache lock")
			Expect(addErr).NotTo(HaveOccurred(), "AddEndpointSlice should successfully finish before Service deletion")
			Expect(deleteErr).NotTo(HaveOccurred(), "DeleteService should successfully remove the Service rules")

			_, exists := npw.getServiceInfo(name)
			Expect(exists).To(BeFalse(), "deleted Service should not remain in the cache")
			verifyNFTablesRule(nft, "10.96.0.20", 80, 30091, false, "nftables rule should be deleted with the Service")
		})

		It("serializes endpoint slice deletion with Service deletion", func() {
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace,
				"10.96.0.23", "10.244.0.24", 80, 30094, nil)
			service := newService(testService, testNamespace, "10.96.0.23",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30094,
				}}, corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)
			blockingWatchFactory := &blockingServiceLookupWatchFactory{
				NodeWatchFactory:   watcher,
				service:            service,
				endpointSlices:     nil,
				serviceLookupStart: make(chan struct{}),
				releaseServiceRead: make(chan struct{}),
			}
			npw.watchFactory = blockingWatchFactory
			var endpointDeleteWorkerStarted, serviceDeleteWorkerStarted bool
			endpointDeleteWorkerFinished := make(chan struct{})
			serviceDeleteWorkerFinished := make(chan struct{})
			releaseServiceRead := func() {
				select {
				case <-blockingWatchFactory.releaseServiceRead:
				default:
					close(blockingWatchFactory.releaseServiceRead)
				}
			}
			defer func() {
				releaseServiceRead()
				if endpointDeleteWorkerStarted {
					waitForTestWorker(endpointDeleteWorkerFinished, "DeleteEndpointSlice")
				}
				if serviceDeleteWorkerStarted {
					waitForTestWorker(serviceDeleteWorkerFinished, "DeleteService")
				}
			}()

			endpointDeleteDone := make(chan error, 1)
			endpointDeleteWorkerStarted = true
			go func() {
				defer close(endpointDeleteWorkerFinished)
				endpointDeleteDone <- npw.DeleteEndpointSlice(epSlice)
			}()
			Eventually(blockingWatchFactory.serviceLookupStart).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(BeClosed(), "DeleteEndpointSlice should reach the blocked Service lookup")
			deleteHoldsCacheLock := !npw.serviceInfoLock.TryLock()
			if !deleteHoldsCacheLock {
				npw.serviceInfoLock.Unlock()
			}
			Expect(deleteHoldsCacheLock).To(BeTrue(), "DeleteEndpointSlice should hold the cache lock during Service lookup")

			deleteService := service.DeepCopy()
			deleteService.Spec.Ports = nil // Avoid conntrack operations; rule teardown uses the cached Service.
			serviceDeleteDone := make(chan error, 1)
			serviceDeleteWorkerStarted = true
			go func() {
				defer close(serviceDeleteWorkerFinished)
				serviceDeleteDone <- npw.DeleteService(deleteService)
			}()
			releaseServiceRead()
			var endpointDeleteErr, serviceDeleteErr error
			Eventually(endpointDeleteDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&endpointDeleteErr), "DeleteEndpointSlice should finish after the Service lookup is released")
			Eventually(serviceDeleteDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&serviceDeleteErr), "DeleteService should finish after DeleteEndpointSlice releases the cache lock")
			Expect(endpointDeleteErr).NotTo(HaveOccurred(), "DeleteEndpointSlice should finish before Service deletion")
			Expect(serviceDeleteErr).NotTo(HaveOccurred(), "DeleteService should successfully remove the Service rules")

			name := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
			_, exists := npw.getServiceInfo(name)
			Expect(exists).To(BeFalse(), "deleted Service should not remain in the cache")
			verifyNFTablesRule(nft, "10.96.0.23", 80, 30094, false, "Service deletion should remove rules after endpoint slice deletion")
		})

		It("holds the cache lock until service rules are deleted", func() {
			setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace,
				"10.96.0.21", "10.244.0.22", 80, 30092, nil)
			service := newService(testService, testNamespace, "10.96.0.21", nil,
				corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

			npw.gatewayIPLock.Lock()
			gatewayIPLockReleased := false
			deleteWorkerStarted := false
			deleteWorkerFinished := make(chan struct{})
			defer func() {
				if !gatewayIPLockReleased {
					npw.gatewayIPLock.Unlock()
				}
				if deleteWorkerStarted {
					waitForTestWorker(deleteWorkerFinished, "DeleteService")
				}
			}()

			deleteDone := make(chan error, 1)
			deleteWorkerStarted = true
			go func() {
				defer close(deleteWorkerFinished)
				deleteDone <- npw.DeleteService(service)
			}()

			cacheLockAvailable := func() bool {
				if !npw.serviceInfoLock.TryLock() {
					return false
				}
				npw.serviceInfoLock.Unlock()
				return true
			}
			Eventually(cacheLockAvailable).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(BeFalse(), "DeleteService should acquire the cache lock")
			Consistently(cacheLockAvailable).WithTimeout(100*time.Millisecond).WithPolling(10*time.Millisecond).Should(BeFalse(), "DeleteService should retain the cache lock while deleting rules")

			npw.gatewayIPLock.Unlock()
			gatewayIPLockReleased = true
			var deleteErr error
			Eventually(deleteDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&deleteErr), "DeleteService should finish after rule teardown is unblocked")
			Expect(deleteErr).NotTo(HaveOccurred())
			_, exists := npw.getServiceInfo(ktypes.NamespacedName{Namespace: testNamespace, Name: testService})
			Expect(exists).To(BeFalse(), "service rules should be removed from the cache after deletion")
		})

		DescribeTable("holds the cache lock until conntrack cleanup completes",
			func(clusterIPs []string, ipFamilies []corev1.IPFamily, ipv4Enabled, ipv6Enabled bool) {
				config.IPv4Mode = ipv4Enabled
				config.IPv6Mode = ipv6Enabled
				nft = nodenft.SetFakeNFTablesHelper()
				Expect(initGatewayNFTables()).To(Succeed())

				service := newService(testService, testNamespace, clusterIPs[0],
					[]corev1.ServicePort{{
						Name:       "http",
						Protocol:   corev1.ProtocolTCP,
						Port:       80,
						TargetPort: intstr.FromInt(8080),
						NodePort:   30093,
					}}, corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)
				service.Spec.ClusterIPs = clusterIPs
				service.Spec.IPFamilies = ipFamilies
				Expect(npw.AddService(service)).To(Succeed())

				mapNames := make([]string, 0, len(clusterIPs))
				originalNetlinkOps := util.GetNetLinkOps()
				netlinkMock := new(utilMocks.NetLinkOps)
				util.SetNetLinkOpMockInst(netlinkMock)
				DeferCleanup(func() { util.SetNetLinkOpMockInst(originalNetlinkOps) })

				conntrackStarted := make(chan struct{}, len(clusterIPs))
				releaseConntrack := make(chan struct{})
				deleteWorkerStarted := false
				deleteWorkerFinished := make(chan struct{})
				defer func() {
					select {
					case <-releaseConntrack:
					default:
						close(releaseConntrack)
					}
					if deleteWorkerStarted {
						waitForTestWorker(deleteWorkerFinished, "DeleteService")
					}
				}()

				for _, serviceIP := range clusterIPs {
					family := netlink.FAMILY_V4
					mapName := nftablesNodePortsV4
					if net.ParseIP(serviceIP).To4() == nil {
						family = netlink.FAMILY_V6
						mapName = nftablesNodePortsV6
					}
					mapNames = append(mapNames, mapName)
					verifyNFTablesRuleInMap(nft, mapName, serviceIP, 80, 30093, true,
						"Service rule should exist before deletion")
					netlinkMock.On("ConntrackDeleteFilters",
						netlink.ConntrackTableType(netlink.ConntrackTable),
						netlink.InetFamily(family),
						makeConntrackFilter(serviceIP, 80, corev1.ProtocolTCP, netlink.ConntrackOrigDstIP)).
						Return(uint(0), nil).
						Run(func(mock.Arguments) {
							conntrackStarted <- struct{}{}
							<-releaseConntrack
						}).Once()
				}

				deleteDone := make(chan error, 1)
				deleteWorkerStarted = true
				go func() {
					defer close(deleteWorkerFinished)
					deleteDone <- npw.DeleteService(service)
				}()
				Eventually(conntrackStarted).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(), "DeleteService should reach conntrack cleanup")

				cacheLockAvailable := npw.serviceInfoLock.TryLock()
				if cacheLockAvailable {
					npw.serviceInfoLock.Unlock()
				}
				Expect(cacheLockAvailable).To(BeFalse(), "DeleteService should retain the cache lock during conntrack cleanup")
				for i, serviceIP := range clusterIPs {
					verifyNFTablesRuleInMap(nft, mapNames[i], serviceIP, 80, 30093, false,
						"Service rule should be removed before conntrack cleanup finishes")
				}

				close(releaseConntrack)
				var deleteErr error
				Eventually(deleteDone).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).Should(Receive(&deleteErr), "DeleteService should finish after conntrack cleanup is unblocked")
				Expect(deleteErr).NotTo(HaveOccurred())
				Expect(npw.serviceInfoLock.TryLock()).To(BeTrue(), "DeleteService should release the cache lock after conntrack cleanup")
				npw.serviceInfoLock.Unlock()
				netlinkMock.AssertExpectations(GinkgoT())
			},
			Entry("IPv6-only service", []string{"fd00:10:96::22"}, []corev1.IPFamily{corev1.IPv6Protocol}, false, true),
			Entry("dual-stack service", []string{"10.96.0.22", "fd00:10:96::22"}, []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}, true, true),
		)
	})

	Context("when network lookup returns other errors", func() {
		It("should execute delServiceRules but return error from network lookup", func() {
			// Setup service and endpoint slice with nftables rules
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace, "10.96.0.3", "10.244.0.3", 80, 30082, nil)
			service := newService(testService, testNamespace, "10.96.0.3",
				[]corev1.ServicePort{{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
					NodePort:   30082,
				}}, corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)
			blockingWatchFactory := &blockingServiceLookupWatchFactory{
				NodeWatchFactory:   watcher,
				service:            service,
				serviceLookupStart: make(chan struct{}),
				releaseServiceRead: make(chan struct{}),
			}
			close(blockingWatchFactory.releaseServiceRead)
			npw.watchFactory = blockingWatchFactory

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

	Context("when the Service is missing during endpoint slice deletion", func() {
		It("cleans up existing rules without restoring them", func() {
			epSlice := setupServiceAndEndpointSliceWithRules(npw, nft, testService, testNamespace,
				"10.96.0.30", "10.244.0.30", 80, 30100, nil)
			blockingWatchFactory := &blockingServiceLookupWatchFactory{
				NodeWatchFactory: watcher,
				serviceErr: apierrors.NewNotFound(
					schema.GroupResource{Resource: "services"}, testService),
				endpointSlicesErr: apierrors.NewNotFound(
					schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}, epSlice.Name),
				serviceLookupStart: make(chan struct{}),
				releaseServiceRead: make(chan struct{}),
			}
			close(blockingWatchFactory.releaseServiceRead)
			npw.watchFactory = blockingWatchFactory

			err := npw.DeleteEndpointSlice(epSlice)
			Expect(err).NotTo(HaveOccurred(), "missing Service should still allow endpoint rule cleanup")
			verifyNFTablesRule(nft, "10.96.0.30", 80, 30100, false,
				"endpoint deletion should remove rules and not recreate them for a missing Service")
			_, exists := npw.getServiceInfo(ktypes.NamespacedName{Namespace: testNamespace, Name: testService})
			Expect(exists).To(BeFalse(), "successfully cleaned rules should remove the missing Service cache entry")
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

	It("serializes service updates with endpoint-slice rule reconciliation", func() {
		const (
			serviceIP = "10.0.0.41"
			lbIP      = "192.0.2.41"
			oldEPIP   = "10.244.0.41"
			newEPIP   = "10.244.0.42"
		)

		portName := "dns"
		protocol := corev1.ProtocolUDP
		endpointPort := int32(8080)
		endpoint := func(ip string) *discovery.EndpointSlice {
			nodeName := "test-node"
			return newEndpointSlice(testService, testNamespace,
				[]discovery.Endpoint{{Addresses: []string{ip}, NodeName: &nodeName}},
				[]discovery.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &endpointPort}})
		}
		oldSlice := endpoint(oldEPIP)
		newSlice := endpoint(newEPIP)
		oldService := newServiceWithoutNodePortAllocation(testService, testNamespace, serviceIP,
			[]corev1.ServicePort{{Name: portName, Protocol: protocol, Port: 80, TargetPort: intstr.FromInt(8080)}},
			corev1.ServiceTypeLoadBalancer, nil,
			corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: lbIP}}}},
			true, false)
		newService := oldService.DeepCopy()
		newService.Spec.Ports[0].Port = 81
		noSNATSetTx := nft.NewTransaction()
		noSNATSetTx.Add(&knftables.Set{Name: types.NFTMgmtPortNoSNATServicesV4, Type: "ipv4_addr . inet_proto . inet_service"})
		Expect(nft.Run(context.Background(), noSNATSetTx)).To(Succeed())

		watchFactory := &serviceEndpointSnapshotWatchFactory{
			NodeWatchFactory: watcher,
			service:          newService,
			endpointSlices:   []*discovery.EndpointSlice{oldSlice},
		}
		npw.watchFactory = watchFactory
		blockingNetworkManager := &updateBlockingNetworkManager{
			lookupStarted: make(chan struct{}),
			releaseLookup: make(chan struct{}),
			lookupCalls:   make(chan struct{}, 3),
		}
		npw.networkManager = blockingNetworkManager
		originalExecRunner := util.RunCmdExecRunner
		fakeExec := ovntest.NewFakeExec()
		Expect(util.SetExec(fakeExec)).To(Succeed())
		fakeExec.AddRepeatedFakeCmd(&ovntest.ExpectedCmd{Cmd: "ovs-ofctl show breth0"}, 3)
		DeferCleanup(func() {
			util.RunCmdExecRunner = originalExecRunner
			util.ResetRunner()
		})

		originalNetlinkOps := util.GetNetLinkOps()
		netlinkMock := new(utilMocks.NetLinkOps)
		util.SetNetLinkOpMockInst(netlinkMock)
		DeferCleanup(func() { util.SetNetLinkOpMockInst(originalNetlinkOps) })
		netlinkMock.On("ConntrackDeleteFilters", mock.Anything, mock.Anything, mock.Anything).
			Return(uint(0), nil)

		Expect(npw.AddService(oldService)).To(Succeed())
		// Model an endpoint update whose old rule deletion failed: the cache
		// should keep the current endpoints separate from endpoints pending cleanup.
		name := ktypes.NamespacedName{Namespace: testNamespace, Name: testService}
		currentEndpoints := npw.GetLocalEligibleEndpointAddresses([]*discovery.EndpointSlice{newSlice}, oldService)
		staleEndpoints := npw.GetLocalEligibleEndpointAddresses([]*discovery.EndpointSlice{oldSlice}, oldService)
		npw.serviceInfoLock.Lock()
		npw.serviceInfo[name].localEndpoints = currentEndpoints
		npw.serviceInfo[name].pendingLocalEndpoints = staleEndpoints
		npw.serviceInfoLock.Unlock()
		select {
		case <-blockingNetworkManager.lookupCalls:
		default:
		}
		watchFactory.endpointSlices = []*discovery.EndpointSlice{newSlice}
		blockingNetworkManager.blockNextLookup = true

		updateDone := make(chan error, 1)
		updateFinished := make(chan struct{})
		updateStarted := false
		addFinished := make(chan struct{})
		addStarted := false
		DeferCleanup(func() {
			select {
			case <-blockingNetworkManager.releaseLookup:
			default:
				close(blockingNetworkManager.releaseLookup)
			}
			if updateStarted {
				waitForTestWorker(updateFinished, "UpdateService")
			}
			if addStarted {
				waitForTestWorker(addFinished, "AddEndpointSlice")
			}
		})
		updateStarted = true
		go func() {
			defer close(updateFinished)
			updateDone <- npw.UpdateService(oldService, newService)
		}()
		Eventually(blockingNetworkManager.lookupStarted).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).
			Should(BeClosed(), "UpdateService should pause after deleting old rules and before adding updated rules")
		Eventually(blockingNetworkManager.lookupCalls).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).
			Should(Receive(), "UpdateService should reach network lookup while holding the lifecycle lock")

		addDone := make(chan error, 1)
		addStarted = true
		go func() {
			defer close(addFinished)
			addDone <- npw.AddEndpointSlice(newSlice)
		}()
		Eventually(blockingNetworkManager.lookupCalls).WithTimeout(5*time.Second).WithPolling(10*time.Millisecond).
			Should(Receive(), "AddEndpointSlice should reach network lookup while UpdateService holds the service lifecycle lock")
		Expect(npw.serviceInfoLock.TryLock()).To(BeFalse(), "UpdateService must keep the lifecycle lock through rule installation")
		close(blockingNetworkManager.releaseLookup)
		var updateErr error
		Eventually(updateDone).WithTimeout(5 * time.Second).WithPolling(10 * time.Millisecond).Should(Receive(&updateErr))
		Expect(updateErr).NotTo(HaveOccurred())
		var addErr error
		Eventually(addDone).WithTimeout(5 * time.Second).WithPolling(10 * time.Millisecond).Should(Receive(&addErr))
		Expect(addErr).NotTo(HaveOccurred())

		noSNATSetElements, err := nft.ListElements(context.Background(), "set", types.NFTMgmtPortNoSNATServicesV4)
		Expect(err).NotTo(HaveOccurred())
		containsEndpoint := func(ip string) bool {
			for _, element := range noSNATSetElements {
				if len(element.Key) == 3 && element.Key[0] == ip && element.Key[1] == "udp" && element.Key[2] == "8080" {
					return true
				}
			}
			return false
		}
		Expect(containsEndpoint(newEPIP)).To(BeTrue(), "the replacement endpoint rule should be installed")
		Expect(containsEndpoint(oldEPIP)).To(BeFalse(), "the stale endpoint rule should be removed by serialized reconciliation")

		Expect(npw.DeleteService(newService)).To(Succeed())
		noSNATSetElements, err = nft.ListElements(context.Background(), "set", types.NFTMgmtPortNoSNATServicesV4)
		Expect(err).NotTo(HaveOccurred())
		Expect(containsEndpoint(newEPIP)).To(BeFalse(), "DeleteService should remove the endpoint present in its cache")
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

	It("recreates missing nftables rules when an existing service is retried", func() {
		// Avoid openflow dependency in this test.
		config.Gateway.AllowNoUplink = true
		npw.ofportPhys = ""

		service := newService(testService, testNamespace, "10.96.0.50",
			[]corev1.ServicePort{{
				Name:       "http",
				Protocol:   corev1.ProtocolTCP,
				Port:       80,
				TargetPort: intstr.FromInt(8080),
				NodePort:   30095,
			}},
			corev1.ServiceTypeNodePort, nil, corev1.ServiceStatus{}, false, false)

		Expect(npw.AddService(service)).To(Succeed())
		Expect(nodenft.DeleteObjects(getGatewayNFTRules(service, nil, false))).To(Succeed())
		verifyNFTablesRule(nft, "10.96.0.50", 80, 30095, false,
			"test setup should remove the Service nftables rules")

		Expect(npw.AddService(service)).To(Succeed())
		verifyNFTablesRule(nft, "10.96.0.50", 80, 30095, true,
			"retrying the Service add should recreate missing nftables rules")
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
