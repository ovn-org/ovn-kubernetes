package pod

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/nad"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	ipamclaimsfactory "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/informers/externalversions"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"

	kubemocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"
	v1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
)

type testPod struct {
	scheduled   bool
	hostNetwork bool
	completed   bool
	network     *nadapi.NetworkSelectionElement
}

func (p testPod) getPod(t *testing.T) *corev1.Pod {

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod",
			UID:         apitypes.UID("pod"),
			Namespace:   "namespace",
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			HostNetwork: p.hostNetwork,
		},
		Status: corev1.PodStatus{},
	}
	if p.scheduled {
		pod.Spec.NodeName = "node"
	}
	if p.completed {
		pod.Status.Phase = corev1.PodSucceeded
	}

	if p.network != nil {
		bytes, err := json.Marshal([]*nadapi.NetworkSelectionElement{p.network})
		if err != nil {
			t.Fatalf("Invalid network selection")
		}
		pod.ObjectMeta.Annotations[nadapi.NetworkAttachmentAnnot] = string(bytes)
	}

	return pod
}

type ipAllocatorStub struct {
	released bool
}

func (a *ipAllocatorStub) AddOrUpdateSubnet(name string, subnets []*net.IPNet, excludeSubnets ...*net.IPNet) error {
	panic("not implemented") // TODO: Implement
}

func (a ipAllocatorStub) DeleteSubnet(name string) {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) GetSubnets(name string) ([]*net.IPNet, error) {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) AllocateUntilFull(name string) error {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) AllocateIPs(name string, ips []*net.IPNet) error {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) AllocateNextIPs(name string) ([]*net.IPNet, error) {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) ReleaseIPs(name string, ips []*net.IPNet) error {
	a.released = true
	return nil
}

func (a *ipAllocatorStub) ConditionalIPRelease(name string, ips []*net.IPNet, predicate func() (bool, error)) (bool, error) {
	panic("not implemented") // TODO: Implement
}

func (a *ipAllocatorStub) ForSubnet(name string) subnet.NamedAllocator {
	return &namedAllocatorStub{}
}

func (a *ipAllocatorStub) GetSubnetName([]*net.IPNet) (string, bool) {
	panic("not implemented") // TODO: Implement
}

type idAllocatorStub struct {
	released bool
}

func (a *idAllocatorStub) AllocateID(name string) (int, error) {
	panic("not implemented") // TODO: Implement
}

func (a *idAllocatorStub) ReserveID(name string, id int) error {
	panic("not implemented") // TODO: Implement
}

func (a *idAllocatorStub) ReleaseID(name string) {
	a.released = true
}

func (a *idAllocatorStub) ForName(name string) id.NamedAllocator {
	panic("not implemented") // TODO: Implement
}

func (a *idAllocatorStub) GetSubnetName([]*net.IPNet) (string, bool) {
	panic("not implemented") // TODO: Implement
}

type namedAllocatorStub struct {
}

func (nas *namedAllocatorStub) AllocateIPs(ips []*net.IPNet) error {
	return nil
}

func (nas *namedAllocatorStub) AllocateNextIPs() ([]*net.IPNet, error) {
	return nil, nil
}

func (nas *namedAllocatorStub) ReleaseIPs(ips []*net.IPNet) error {
	return nil
}

func TestPodAllocator_reconcileForNAD(t *testing.T) {
	type args struct {
		old       *testPod
		new       *testPod
		ipamClaim *ipamclaimsapi.IPAMClaim
		nads      []*nadapi.NetworkAttachmentDefinition
		release   bool
	}
	tests := []struct {
		name            string
		args            args
		ipam            bool
		idAllocation    bool
		tracked         bool
		role            string
		expectAllocate  bool
		expectIPRelease bool
		expectIDRelease bool
		expectTracked   bool
		expectEvents    []string
		expectError     string
	}{
		{
			name: "Pod not scheduled",
			args: args{
				new: &testPod{},
			},
		},
		{
			name: "Pod on host network",
			args: args{
				new: &testPod{
					hostNetwork: true,
				},
			},
		},
		{
			name: "Pod not on network",
			args: args{
				new: &testPod{
					scheduled: true,
				},
			},
		},
		{
			name: "Pod on network",
			args: args{
				new: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
			},
			expectAllocate: true,
		},
		{
			name: "Pod completed, release inactive, IP allocation",
			ipam: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
			},
			expectTracked: true,
		},
		{
			name:         "Pod completed, release inactive, ID allocation",
			idAllocation: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
			},
			expectTracked: true,
		},
		{
			name: "Pod completed, release inactive, no allocation",
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
			},
		},
		{
			name: "Pod completed, release active, not previously released, IP allocation",
			ipam: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			expectIPRelease: true,
			expectTracked:   true,
		},
		{
			name:         "Pod completed, release active, not previously released, ID allocation",
			idAllocation: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			expectTracked:   true,
			expectIDRelease: true,
		},
		{
			name: "Pod completed, release active, not previously released, no allocation",
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
		},
		{
			name: "Pod completed, release active, previously released, IP allocation",
			ipam: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			tracked:       true,
			expectTracked: true,
		},
		{
			name:         "Pod completed, release active, previously released, ID allocation",
			idAllocation: true,
			args: args{
				new: &testPod{
					scheduled: true,
					completed: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			tracked:       true,
			expectTracked: true,
		},
		{
			name: "Pod deleted, not scheduled",
			args: args{
				old: &testPod{},
			},
		},
		{
			name: "Pod deleted, on host network",
			args: args{
				old: &testPod{
					hostNetwork: true,
				},
			},
		},
		{
			name: "Pod deleted, not on network",
			args: args{
				old: &testPod{
					scheduled: true,
				},
			},
		},
		{
			name: "Pod deleted, not previously released, IP allocation",
			ipam: true,
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			expectIPRelease: true,
		},
		{
			name:         "Pod deleted, not previously released, ID allocation",
			idAllocation: true,
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			expectIDRelease: true,
		},
		{
			name: "Pod deleted, not previously released, no allocation",
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
		},
		{
			name: "Pod deleted, previously released, IP allocation",
			ipam: true,
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			tracked: true,
		},
		{
			name:         "Pod deleted, previously released, ID allocation",
			idAllocation: true,
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name: "nad",
					},
				},
				release: true,
			},
			tracked: true,
		},
		{
			name: "Pod on network, persistent IP requested, IPAMClaim features IPs",
			args: args{
				new: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name:               "nad",
						IPAMClaimReference: "claim",
					},
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim",
						Namespace: "namespace",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{"192.168.200.80/24"},
					},
				},
			},
			expectAllocate: true,
			ipam:           true,
		},
		{
			name: "Pod deleted, persistent IPs, IP not released",
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name:               "nad",
						IPAMClaimReference: "claim",
					},
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim",
						Namespace: "namespace",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{"192.168.200.80/24"},
					},
				},
				release: true,
			},
			ipam: true,
		},
		{
			name: "Pod deleted, persistent IPs requested *but* not found, IP released",
			args: args{
				old: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Name:               "nad",
						IPAMClaimReference: "claim",
					},
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "claim",
						Namespace: "namespace",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{},
				},
				release: true,
			},
			ipam:            true,
			expectIPRelease: true,
		},
		{
			name: "Pod with primary network NSE, expect event and error",
			args: args{
				new: &testPod{
					scheduled: true,
					network: &nadapi.NetworkSelectionElement{
						Namespace: "namespace",
						Name:      "nad",
					},
				},
				nads: []*nadapi.NetworkAttachmentDefinition{
					ovntest.GenerateNAD("surya", "nad", "namespace",
						types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary),
				},
			},
			role:         types.NetworkRolePrimary,
			expectError:  "failed to get NAD to network mapping: unexpected primary network \"\" specified with a NetworkSelectionElement &{Name:nad Namespace:namespace IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}",
			expectEvents: []string{"Warning ErrorAllocatingPod unexpected primary network \"\" specified with a NetworkSelectionElement &{Name:nad Namespace:namespace IPRequest:[] MacRequest: InfinibandGUIDRequest: InterfaceRequest: PortMappingsRequest:[] BandwidthRequest:<nil> CNIArgs:<nil> GatewayRequest:[] IPAMClaimReference:}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			ipallocator := &ipAllocatorStub{}
			idallocator := &idAllocatorStub{}

			podListerMock := &v1mocks.PodLister{}
			kubeMock := &kubemocks.InterfaceOVN{}
			podNamespaceLister := &v1mocks.PodNamespaceLister{}

			podListerMock.On("Pods", mock.AnythingOfType("string")).Return(podNamespaceLister)

			var allocated bool
			kubeMock.On("UpdatePodStatus", mock.AnythingOfType(fmt.Sprintf("%T", &corev1.Pod{}))).Run(
				func(args mock.Arguments) {
					allocated = true
				},
			).Return(nil)

			kubeMock.On(
				"UpdateIPAMClaimIPs",
				mock.AnythingOfType(fmt.Sprintf("%T", &ipamclaimsapi.IPAMClaim{})),
			).Return(nil)

			netConf := &ovncnitypes.NetConf{
				Topology:           types.Layer2Topology,
				AllowPersistentIPs: tt.ipam && tt.args.ipamClaim != nil,
			}

			if tt.role != "" {
				netConf.Role = tt.role
			}

			if tt.ipam {
				netConf.Subnets = "10.1.130.0/24"
			}

			config.OVNKubernetesFeature.EnableInterconnect = tt.idAllocation

			// config.IPv4Mode needs to be set so that the ipv4 of the userdefined primary networks can match the running cluster
			config.IPv4Mode = true
			netInfo, err := util.NewNetInfo(netConf)
			if err != nil {
				t.Fatalf("Invalid netConf")
			}
			netInfo.AddNADs("namespace/nad")

			var ipamClaimsReconciler persistentips.PersistentAllocations
			if tt.ipam && tt.args.ipamClaim != nil {
				ctx, cancel := context.WithCancel(context.Background())
				ipamClaimsLister, teardownFn := generateIPAMClaimsListerAndTeardownFunc(ctx.Done(), tt.args.ipamClaim)
				ipamClaimsReconciler = persistentips.NewIPAMClaimReconciler(kubeMock, netInfo, ipamClaimsLister)

				t.Cleanup(func() {
					cancel()
					teardownFn()
				})
			}

			podAnnotationAllocator := pod.NewPodAnnotationAllocator(
				netInfo,
				podListerMock,
				kubeMock,
				ipamClaimsReconciler,
			)

			testNs := "namespace"
			nadNetworks := map[string]util.NetInfo{}
			for _, nad := range tt.args.nads {
				if nad.Namespace == testNs {
					nadNetwork, _ := util.ParseNADInfo(nad)
					if nadNetwork.IsPrimaryNetwork() {
						if _, ok := nadNetworks[testNs]; !ok {
							nadNetworks[testNs] = nadNetwork
						}
					}
				}
			}

			nadController := &nad.FakeNADController{PrimaryNetworks: nadNetworks}

			fakeRecorder := record.NewFakeRecorder(10)

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true

			a := &PodAllocator{
				netInfo:                netInfo,
				ipAllocator:            ipallocator,
				idAllocator:            idallocator,
				podAnnotationAllocator: podAnnotationAllocator,
				releasedPods:           map[string]sets.Set[string]{},
				releasedPodsMutex:      sync.Mutex{},
				ipamClaimsReconciler:   ipamClaimsReconciler,
				recorder:               fakeRecorder,
				nadController:          nadController,
			}

			var old, new *corev1.Pod
			if tt.args.old != nil {
				old = tt.args.old.getPod(t)
			}
			if tt.args.new != nil {
				new = tt.args.new.getPod(t)
				podNamespaceLister.On("Get", mock.AnythingOfType("string")).Return(new, nil)
			}

			if tt.tracked {
				a.releasedPods["namespace/nad"] = sets.New("pod")
			}

			err = a.reconcile(old, new, tt.args.release)
			if len(tt.expectError) > 0 {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.expectError)))
			} else if err != nil {
				t.Errorf("reconcile unexpected failure: %v", err)
			}

			if tt.expectAllocate != allocated {
				t.Errorf("expected pod ips allocated to be %v but it was %v", tt.expectAllocate, allocated)
			}

			if tt.expectIPRelease != ipallocator.released {
				t.Errorf("expected pod ips released to be %v but it was %v", tt.expectIPRelease, ipallocator.released)
			}

			if tt.expectIDRelease != idallocator.released {
				t.Errorf("expected pod ID released to be %v but it was %v", tt.expectIPRelease, ipallocator.released)
			}

			if tt.expectTracked != a.releasedPods["namespace/nad"].Has("pod") {
				t.Errorf("expected pod tracked to be %v but it was %v", tt.expectTracked, a.releasedPods["namespace/nad"].Has("pod"))
			}

			var obtainedEvents []string
			for {
				if len(fakeRecorder.Events) == 0 {
					break
				}
				obtainedEvents = append(obtainedEvents, <-fakeRecorder.Events)
			}
			g.Expect(tt.expectEvents).To(gomega.Equal(obtainedEvents))
		})
	}
}

func generateIPAMClaimsListerAndTeardownFunc(stopChannel <-chan struct{}, ipamClaims ...runtime.Object) (ipamclaimslister.IPAMClaimLister, func()) {
	ipamClaimClient := fakeipamclaimclient.NewSimpleClientset(ipamClaims...)
	informerFactory := ipamclaimsfactory.NewSharedInformerFactory(ipamClaimClient, 0)
	lister := informerFactory.K8s().V1alpha1().IPAMClaims().Lister()
	informerFactory.Start(stopChannel)
	informerFactory.WaitForCacheSync(stopChannel)
	return lister, func() {
		informerFactory.Shutdown()
	}
}
