package pod

import (
	"errors"
	"fmt"
	"net"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/onsi/gomega"
)

type ipAllocatorStub struct {
	netxtIPs         []*net.IPNet
	allocateIPsError error
	releasedIPs      []*net.IPNet
}

func (a *ipAllocatorStub) AllocateIPs(ips []*net.IPNet) error {
	return a.allocateIPsError
}

func (a *ipAllocatorStub) AllocateNextIPs() ([]*net.IPNet, error) {
	return a.netxtIPs, nil
}

func (a *ipAllocatorStub) ReleaseIPs(ips []*net.IPNet) error {
	a.releasedIPs = ips
	return nil
}

func (a *ipAllocatorStub) IsErrAllocated(err error) bool {
	return errors.Is(err, ipam.ErrAllocated)
}

type idAllocatorStub struct {
	nextID         int
	reserveIDError error
	releasedID     bool
}

func (a *idAllocatorStub) AllocateID() (int, error) {
	return a.nextID, nil
}

func (a *idAllocatorStub) ReserveID(id int) error {
	return a.reserveIDError
}

func (a *idAllocatorStub) ReleaseID() {
	a.releasedID = true
}

type persistentIPsStub struct {
	datastore map[string]ipamclaimsapi.IPAMClaim
}

func (c *persistentIPsStub) Reconcile(_ *ipamclaimsapi.IPAMClaim, newIPAMClaim *ipamclaimsapi.IPAMClaim, _ persistentips.IPReleaser) error {
	c.datastore[ipamClaimKey(newIPAMClaim.Namespace, newIPAMClaim.Name)] = *newIPAMClaim
	return nil
}

func (c *persistentIPsStub) FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error) {
	ipamClaimKey := fmt.Sprintf("%s/%s", namespace, claimName)
	ipamClaim, wasFound := c.datastore[ipamClaimKey]
	if !wasFound {
		return nil, fmt.Errorf("not found")
	}
	return &ipamClaim, nil
}

func ipamClaimKey(namespace string, claimName string) string {
	return fmt.Sprintf("%s/%s", namespace, claimName)
}

func Test_allocatePodAnnotationWithRollback(t *testing.T) {
	randomMac, err := util.GenerateRandMAC()
	if err != nil {
		t.Fatalf("failed to generate random mac")
	}

	requestedMAC := "01:02:03:04:05:06"
	requestedMACParsed, err := net.ParseMAC(requestedMAC)
	if err != nil {
		t.Fatalf("failed to generate random mac")
	}

	type args struct {
		ipAllocator subnet.NamedAllocator
		idAllocator id.NamedAllocator
		network     *nadapi.NetworkSelectionElement
		ipamClaim   *ipamclaimsapi.IPAMClaim
		reallocate  bool
	}
	tests := []struct {
		name                      string
		args                      args
		ipam                      bool
		idAllocation              bool
		persistentIPAllocation    bool
		podAnnotation             *util.PodAnnotation
		invalidNetworkAnnotation  bool
		wantUpdatedPod            bool
		wantGeneratedMac          bool
		wantPodAnnotation         *util.PodAnnotation
		wantReleasedIPs           []*net.IPNet
		wantReleasedIPsOnRollback []*net.IPNet
		wantReleaseID             bool
		wantRelasedIDOnRollback   bool
		wantErr                   bool
	}{
		{
			// on secondary L2 networks with no IPAM, we expect to generate a
			// random mac
			name:             "expect generated mac, no IPAM",
			wantUpdatedPod:   true,
			wantGeneratedMac: true,
		},
		{
			// on secondary L2 networks with no IPAM, if the pod is already
			// annotated with a random MAC, we expect no further changes
			name: "expect no updates, has mac, no IPAM",
			podAnnotation: &util.PodAnnotation{
				MAC: randomMac,
			},
			wantPodAnnotation: &util.PodAnnotation{
				MAC: randomMac,
			},
		},
		{
			// on secondary L2 network with no IPAM, honor static IP requests
			// present in the network selection annotation
			name: "expect requested static IP, no gateway, no IPAM",
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.4/24"},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
			},
		},
		{
			// on secondary L2 network with no IPAM, honor static IP and gateway
			// requests present in the network selection annotation
			name: "expect requested static IP, with gateway, no IPAM",
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest:      []string{"192.168.0.4/24"},
					GatewayRequest: ovntest.MustParseIPs("192.168.0.1"),
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				Gateways: ovntest.MustParseIPs("192.168.0.1"),
			},
		},
		{
			// on networks with IPAM, expect error if static IP request present
			// in the network selection annotation
			name: "expect error, static ip request, IPAM",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.3/24"},
				},
			},
			wantUpdatedPod: true,
			wantErr:        true,
		},
		{
			// on networks with IPAM, expect a normal IP, MAC and gateway
			// allocation
			name: "expect new IP",
			ipam: true,
			args: args{
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, if pod is already annotated, expect no
			// further updates but do allocate the IP
			name: "expect no updates, annotated, IPAM",
			ipam: true,
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			args: args{
				ipAllocator: &ipAllocatorStub{},
			},
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, if pod is already annotated, expect no
			// further updates and no error if the IP is already allocated
			name: "expect no updates, annotated, already allocated, IPAM",
			ipam: true,
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			args: args{
				ipAllocator: &ipAllocatorStub{
					allocateIPsError: ipam.ErrAllocated,
				},
			},
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
		},
		{
			// on networks with IPAM, if pod is already annotated, expect error
			// if allocation fails
			name: "expect error, annotated, allocation fails, IPAM",
			ipam: true,
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			args: args{
				ipAllocator: &ipAllocatorStub{
					allocateIPsError: errors.New("Allocate IPs failed"),
				},
			},
			wantErr: true,
		},
		{
			// on networks with IPAM, try to honor IP request allowing to
			// re-allocater on error
			name: "expect requested non-static IP, IPAM",
			ipam: true,
			args: args{
				reallocate: true,
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.4/24"},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.4/24"),
		},
		{
			// on networks with IPAM, try to honor IP request that is already
			// allocated
			name: "expect requested non-static IP, already allocated, IPAM",
			ipam: true,
			args: args{
				reallocate: true,
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.4/24"},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs:         ovntest.MustParseIPNets("192.168.0.3/24"),
					allocateIPsError: ipam.ErrAllocated,
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
			},
		},
		{
			// on networks with IPAM, trying to honor IP request but
			// re-allocating on error
			name: "expect reallocate to new IP, error on requested non-static IP, IPAM",
			ipam: true,
			args: args{
				reallocate: true,
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.4/24"},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs:         ovntest.MustParseIPNets("192.168.0.3/24"),
					allocateIPsError: errors.New("Allocate IPs failed"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, expect error on an invalid IP request
			name: "expect error, invalid requested IP, no IPAM",
			ipam: false,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"ivalid"},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantErr: true,
		},
		{
			// on networks with IPAM, expect error on an invalid MAC request
			name: "expect error, invalid requested MAC, IPAM",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: "ivalid",
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantErr:         true,
			wantReleasedIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, honor a MAC request through the network
			// selection element
			name: "expect requested MAC",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: requestedMAC,
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      requestedMACParsed,
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, expect error on an invalid network
			// selection element
			name: "expect error, invalid network annotation, IPAM",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: "invalid",
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			invalidNetworkAnnotation: true,
			wantErr:                  true,
			wantReleasedIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, and persistent IPs, expect to reuse the
			// already allocated IPAM claim
			name:                   "IPAM persistent IPs, IP address re-use",
			ipam:                   true,
			persistentIPAllocation: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPAMClaimReference: "my-ipam-claim",
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "my-ipam-claim",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{"192.168.0.200/24"},
					},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.200/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.200/24")[0].IP),
			},
		},
		{
			// on networks with IPAM, with persistent IPs *not* allowed, but
			// the pod requests a claim, new IPs are allocated, and rolled back
			// on failures.
			name: "IPAM, persistent IPs *not* allowed, requested by pod; new IP address allocated, and rolled back on failures",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPAMClaimReference: "my-ipam-claim",
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "my-ipam-claim",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{"192.168.0.200/24"},
					},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod:            true,
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
		},
		{
			// on networks with IPAM, and persistent IPs, expect to allocate a
			// new IP address if the IPAMClaim provided is empty
			name:                   "IPAM persistent IPs, empty IPAMClaim",
			ipam:                   true,
			persistentIPAllocation: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPAMClaimReference: "my-ipam-claim",
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "my-ipam-claim",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{},
				},
				ipAllocator: &ipAllocatorStub{
					netxtIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with ID allocation, expect allocated ID
			name:         "expect ID allocation",
			idAllocation: true,
			args: args{
				idAllocator: &idAllocatorStub{
					nextID: 100,
				},
			},
			podAnnotation: &util.PodAnnotation{
				MAC: randomMac,
			},
			wantPodAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 100,
			},
			wantUpdatedPod:          true,
			wantRelasedIDOnRollback: true,
		},
		{
			// on networks with ID allocation, already allocated, expect
			// allocated ID
			name:         "expect already allocated ID",
			idAllocation: true,
			args: args{
				idAllocator: &idAllocatorStub{},
			},
			podAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 200,
			},
			wantPodAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 200,
			},
			wantRelasedIDOnRollback: true,
		},
		{
			// ID allocation error
			name:         "expect ID allocation error",
			idAllocation: true,
			args: args{
				idAllocator: &idAllocatorStub{
					reserveIDError: errors.New("ID allocation error"),
				},
			},
			podAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 200,
			},
			wantErr: true,
		},
		{
			// ID allocation error
			name:         "expect ID allocation error",
			idAllocation: true,
			args: args{
				idAllocator: &idAllocatorStub{
					reserveIDError: errors.New("ID allocation error"),
				},
			},
			podAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 200,
			},
			wantErr: true,
		},
		{
			// expect ID release on error
			name:         "expect error, release ID",
			idAllocation: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: "invalid",
				},
				idAllocator: &idAllocatorStub{
					nextID: 300,
				},
			},
			wantErr:       true,
			wantReleaseID: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error

			g := gomega.NewWithT(t)

			network := tt.args.network
			if network == nil {
				network = &nadapi.NetworkSelectionElement{}
			}
			network.Name = "network"
			network.Namespace = "namespace"

			var netInfo util.NetInfo
			netInfo = &util.DefaultNetInfo{}
			nadName := types.DefaultNetworkName
			if !tt.ipam || tt.idAllocation || tt.persistentIPAllocation || tt.args.ipamClaim != nil {
				nadName = util.GetNADName(network.Namespace, network.Name)
				var subnets string
				if tt.ipam {
					subnets = "192.168.0.0/24"
				}
				netInfo, err = util.NewNetInfo(&ovncnitypes.NetConf{
					Topology: types.Layer2Topology,
					NetConf: cnitypes.NetConf{
						Name: network.Name,
					},
					NADName:            nadName,
					Subnets:            subnets,
					AllowPersistentIPs: tt.persistentIPAllocation,
				})
				if err != nil {
					t.Fatalf("failed to create NetInfo: %v", err)
				}
			}

			config.OVNKubernetesFeature.EnableInterconnect = tt.idAllocation

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "namespace",
				},
			}
			if tt.podAnnotation != nil {
				pod.Annotations, err = util.MarshalPodAnnotation(nil, tt.podAnnotation, nadName)
				if err != nil {
					t.Fatalf("failed to set pod annotations: %v", err)
				}
			}

			if tt.invalidNetworkAnnotation {
				pod.ObjectMeta.Annotations = map[string]string{
					nadapi.NetworkAttachmentAnnot: "",
				}
			}

			var claimsReconciler persistentips.PersistentAllocations
			dummyDatastore := map[string]ipamclaimsapi.IPAMClaim{}
			if tt.args.ipamClaim != nil {
				tt.args.ipamClaim.Namespace = network.Namespace
				dummyDatastore[fmt.Sprintf("%s/%s", tt.args.ipamClaim.Namespace, tt.args.ipamClaim.Name)] = *tt.args.ipamClaim
			}

			claimsReconciler = &persistentIPsStub{
				datastore: dummyDatastore,
			}

			pod, podAnnotation, rollback, err := allocatePodAnnotationWithRollback(
				tt.args.ipAllocator,
				tt.args.idAllocator,
				netInfo,
				pod,
				network,
				claimsReconciler,
				tt.args.reallocate,
			)

			if tt.args.ipAllocator != nil {
				releasedIPs := tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs
				g.Expect(releasedIPs).To(gomega.Equal(tt.wantReleasedIPs), "Release IP on error behaved unexpectedly")
				tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs = nil
			}

			if tt.args.idAllocator != nil {
				releasedID := tt.args.idAllocator.(*idAllocatorStub).releasedID
				g.Expect(releasedID).To(gomega.Equal(tt.wantReleaseID), "Release ID on error behaved unexpectedly")
				tt.args.idAllocator.(*idAllocatorStub).releasedID = false
			}

			rollback()

			if tt.args.ipAllocator != nil {
				releasedIPs := tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs
				g.Expect(releasedIPs).To(gomega.Equal(tt.wantReleasedIPsOnRollback), "Release IP on rollback behaved unexpectedly")
			}

			if tt.args.idAllocator != nil {
				releasedID := tt.args.idAllocator.(*idAllocatorStub).releasedID
				g.Expect(releasedID).To(gomega.Equal(tt.wantRelasedIDOnRollback), "Release ID on rollback behaved unexpectedly")
			}

			if tt.wantErr {
				// check the expected error after we have checked above that the
				// rollback has behaved as expected
				g.Expect(err).To(gomega.HaveOccurred(), "Expected error")
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred(), "Did not expect error")

			if tt.wantGeneratedMac {
				g.Expect(podAnnotation).NotTo(gomega.BeNil(), "Expected updated pod annotation")
				g.Expect(podAnnotation.IPs).To(gomega.BeNil(), "Did not expect IPs")
				g.Expect(podAnnotation.MAC[0]&2).To(gomega.BeEquivalentTo(2), "Expected local MAC")
				return
			}

			g.Expect(podAnnotation).To(gomega.Equal(tt.wantPodAnnotation))

			if tt.wantUpdatedPod {
				g.Expect(pod).NotTo(gomega.BeNil(), "Expected an updated pod")
			}
		})
	}
}
