// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package pod

import (
	"errors"
	"fmt"
	"net"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/google/go-cmp/cmp"
	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	ipam "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/mac"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/persistentips"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type ipAllocatorStub struct {
	nextIPs          []*net.IPNet
	allocateIPsError error
	releasedIPs      []*net.IPNet
}

func (a *ipAllocatorStub) AllocateIPs([]*net.IPNet) error {
	return a.allocateIPsError
}

func (a *ipAllocatorStub) AllocateNextIPs() ([]*net.IPNet, error) {
	return a.nextIPs, nil
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

func (a *idAllocatorStub) ReserveID(int) error {
	return a.reserveIDError
}

func (a *idAllocatorStub) ReleaseID() int {
	a.releasedID = true
	return a.nextID
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

func (c *persistentIPsStub) UpdateIPAMClaimStatus(ipamClaim *ipamclaimsapi.IPAMClaim, podAnnotation *util.PodAnnotation, podName string, allocationErr error) *ipamclaimsapi.IPAMClaim {
	updatedClaim := ipamClaim.DeepCopy()
	updatedClaim.Status.OwnerPod = &ipamclaimsapi.OwnerPod{Name: podName}
	if allocationErr != nil {
		updatedClaim.Status.IPs = []string{}
	} else if podAnnotation != nil && len(podAnnotation.IPs) > 0 {
		updatedClaim.Status.IPs = util.StringSlice(podAnnotation.IPs)
	}
	return updatedClaim
}

func ipamClaimKey(namespace string, claimName string) string {
	return fmt.Sprintf("%s/%s", namespace, claimName)
}

type macRegistryStub struct {
	reserveErr  error
	releaseMAC  net.HardwareAddr
	reservedMAC net.HardwareAddr
}

func (m *macRegistryStub) Reserve(_ string, mac net.HardwareAddr) error {
	m.reservedMAC = mac
	return m.reserveErr
}

func (m *macRegistryStub) Release(_ string, mac net.HardwareAddr) error {
	m.releaseMAC = mac
	return nil
}

func Test_allocatePodAnnotationReturnsUpdatedPod(t *testing.T) {
	g := gomega.NewWithT(t)

	origFeatureConfig := config.OVNKubernetesFeature
	origDefaultConfig := config.Default
	origIPv4Mode := config.IPv4Mode
	origIPv6Mode := config.IPv6Mode
	t.Cleanup(func() {
		config.OVNKubernetesFeature = origFeatureConfig
		config.Default = origDefaultConfig
		config.IPv4Mode = origIPv4Mode
		config.IPv6Mode = origIPv6Mode
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.IPv4Mode = true
	config.IPv6Mode = false
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{CIDR: ovntest.MustParseIPNet("10.128.0.0/14"), HostSubnetLength: 24},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod",
			Namespace: "namespace",
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	g.Expect(podIndexer.Add(pod)).To(gomega.Succeed())

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"k8s.ovn.org/node-subnets": `{"default":["10.128.0.0/24"]}`,
			},
		},
	}

	updatedPod, podAnnotation, err := allocatePodAnnotation(
		corelisters.NewPodLister(podIndexer),
		&kube.Kube{KClient: fake.NewSimpleClientset(pod.DeepCopy())},
		&ipAllocatorStub{nextIPs: ovntest.MustParseIPNets("10.128.0.3/24")},
		&util.DefaultNetInfo{},
		node,
		pod,
		types.DefaultNetworkName,
		nil,
		nil,
		nil,
		false,
		false,
		types.NetworkRolePrimary,
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(updatedPod).NotTo(gomega.BeNil())
	g.Expect(podAnnotation.MAC).NotTo(gomega.BeNil())
	g.Expect(pod.Annotations).To(gomega.BeNil())

	updatedPodAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, types.DefaultNetworkName)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(util.StringSlice(updatedPodAnnotation.IPs)).To(gomega.Equal(util.StringSlice(podAnnotation.IPs)))
	g.Expect(updatedPodAnnotation.MAC.String()).To(gomega.Equal(podAnnotation.MAC.String()))
	g.Expect(updatedPodAnnotation.Role).To(gomega.Equal(podAnnotation.Role))
}

func Test_allocatePodAnnotationWithTunnelIDReturnsUpdatedPod(t *testing.T) {
	g := gomega.NewWithT(t)

	origFeatureConfig := config.OVNKubernetesFeature
	t.Cleanup(func() {
		config.OVNKubernetesFeature = origFeatureConfig
	})
	config.OVNKubernetesFeature.EnableMultiNetwork = true

	network := &nadapi.NetworkSelectionElement{
		Name:      "network",
		Namespace: "namespace",
	}
	nadKey := util.GetNADName(network.Namespace, network.Name)
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: network.Name,
		},
		NADName: nadKey,
		Role:    types.NetworkRolePrimary,
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	initialMAC := net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0x00, 0x02}
	podAnnotations, err := util.MarshalPodAnnotation(nil, &util.PodAnnotation{
		MAC: initialMAC,
	}, nadKey)
	g.Expect(err).NotTo(gomega.HaveOccurred())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod",
			Namespace:   "namespace",
			Annotations: podAnnotations,
		},
		Spec: corev1.PodSpec{
			NodeName: "node",
		},
	}
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	g.Expect(podIndexer.Add(pod)).To(gomega.Succeed())

	updatedPod, podAnnotation, err := allocatePodAnnotationWithTunnelID(
		corelisters.NewPodLister(podIndexer),
		&kube.Kube{KClient: fake.NewSimpleClientset(pod.DeepCopy())},
		nil,
		&idAllocatorStub{nextID: 100},
		netInfo,
		&corev1.Node{},
		pod,
		nadKey,
		network,
		nil,
		nil,
		false,
		types.NetworkRolePrimary,
	)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(updatedPod).NotTo(gomega.BeNil())
	g.Expect(podAnnotation.TunnelID).To(gomega.Equal(100))

	updatedPodAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(updatedPodAnnotation).To(gomega.Equal(podAnnotation))
	g.Expect(updatedPodAnnotation.TunnelID).To(gomega.Equal(100))
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

	localnetNetInfo := func(ipamType string) util.NetInfo {
		netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			Topology: types.LocalnetTopology,
			NetConf: cnitypes.NetConf{
				Name: "network",
				IPAM: cnitypes.IPAM{Type: ipamType},
			},
			NADName: util.GetNADName("namespace", "network"),
		})
		if err != nil {
			t.Fatalf("failed to create NetInfo: %v", err)
		}
		return netInfo
	}

	type args struct {
		ipAllocator            subnet.NamedAllocator
		idAllocator            id.NamedAllocator
		macRegistry            *macRegistryStub
		network                *nadapi.NetworkSelectionElement
		ipamClaim              *ipamclaimsapi.IPAMClaim
		reallocate             bool
		requireIPAMReservation bool
	}
	tests := []struct {
		name                            string
		args                            args
		netInfo                         util.NetInfo
		nadName                         string
		ipam                            bool
		tunnelIDAllocation              bool
		persistentIPAllocation          bool
		enablePreconfiguredUDNAddresses bool
		role                            string
		podAnnotation                   *util.PodAnnotation
		invalidNetworkAnnotation        bool
		wantUpdatedPod                  bool
		wantGeneratedMac                bool
		wantPodAnnotation               *util.PodAnnotation
		wantReleasedIPs                 []*net.IPNet
		wantReleasedIPsOnRollback       []*net.IPNet
		wantReservedMAC                 net.HardwareAddr
		wantReleaseMACOnRollback        net.HardwareAddr
		wantReleaseID                   bool
		wantRelasedIDOnRollback         bool
		wantErr                         bool
		isSingleStackIPv4               bool
		isSingleStackIPv6               bool
		multiNetworkDisabled            bool
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
			// annotated with a random MAC, we expect to add a tunnel ID
			name: "expect tunnel ID update, has mac, no IPAM",
			podAnnotation: &util.PodAnnotation{
				MAC:  randomMac,
				Role: types.NetworkRolePrimary,
			},
			wantPodAnnotation: &util.PodAnnotation{
				MAC:      randomMac,
				TunnelID: 100,
			},
			wantUpdatedPod: true,
		},
		{
			// with multiNetwork disabled, on secondary L2 network with no IPAM, honor static IP requests
			// present in the network selection annotation
			name: "expect requested static IP, no gateway, no IPAM",
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.4/24"},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			role:                 types.NetworkRolePrimary,
			multiNetworkDisabled: true,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				Gateways: ovntest.MustParseIPs("192.168.0.1"),
				TunnelID: 100,
				Role:     types.NetworkRoleSecondary,
			},
			role: types.NetworkRoleSecondary,
		},
		{
			// on networks with IPAM, expect error if static IP request present
			// in the network selection annotation
			name:    "expect error, static ip request, IPAM, non layer2",
			netInfo: &util.DefaultNetInfo{},
			nadName: types.DefaultNetworkName,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("169.254.169.5"),
							Mask: net.CIDRMask(32, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role: types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			role:                      types.NetworkRolePrimary,
		},
		{
			name:                   "expect new IP and ipv6 gateway LLA for primary udn layer2 with dual stack",
			ipam:                   true,
			tunnelIDAllocation:     true,
			persistentIPAllocation: true,
			args: args{
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24", "2010:100:200::3/60"),
				},
				idAllocator: &idAllocatorStub{
					nextID: 100,
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:            ovntest.MustParseIPNets("192.168.0.3/24", "2010:100:200::3/60"),
				MAC:            util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				Gateways:       []net.IP{ovntest.MustParseIP("192.168.0.1").To4(), ovntest.MustParseIP("2010:100:200::1")},
				GatewayIPv6LLA: util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(ovntest.MustParseIP("100.65.0.4"))),
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("100.65.0.0").To4(),
							Mask: net.CIDRMask(16, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("fd99::/64"),
						NextHop: ovntest.MustParseIP("2010:100:200::1"),
					},
				},
				Role:     types.NetworkRolePrimary,
				TunnelID: 100,
			},
			wantRelasedIDOnRollback:   true,
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24", "2010:100:200::3/60"),
			role:                      types.NetworkRolePrimary,
		},
		{
			name:                   "expect new IP and ipv6 gateway LLA for primary udn layer2 with single stack IPv6",
			isSingleStackIPv6:      true,
			ipam:                   true,
			tunnelIDAllocation:     true,
			persistentIPAllocation: true,
			args: args{
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("2010:100:200::3/60"),
				},
				idAllocator: &idAllocatorStub{
					nextID: 100,
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:            ovntest.MustParseIPNets("2010:100:200::3/60"),
				MAC:            util.IPAddrToHWAddr(ovntest.MustParseIPNets("2010:100:200::3/60")[0].IP),
				Gateways:       []net.IP{ovntest.MustParseIP("2010:100:200::1")},
				GatewayIPv6LLA: util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(ovntest.MustParseIP("fd99::4"))),
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("fd99::/64"),
						NextHop: ovntest.MustParseIP("2010:100:200::1"),
					},
				},
				Role:     types.NetworkRolePrimary,
				TunnelID: 100,
			},
			wantRelasedIDOnRollback:   true,
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("2010:100:200::3/60"),
			role:                      types.NetworkRolePrimary,
		},
		{
			name:                   "expect new IP but no ipv6 gateway LLA for primary udn layer2 with single stack IPv4",
			isSingleStackIPv4:      true,
			ipam:                   true,
			tunnelIDAllocation:     true,
			persistentIPAllocation: true,
			args: args{
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
				idAllocator: &idAllocatorStub{
					nextID: 100,
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("100.65.0.0").To4(),
							Mask: net.CIDRMask(16, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role:     types.NetworkRolePrimary,
				TunnelID: 100,
			},
			wantRelasedIDOnRollback:   true,
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			role:                      types.NetworkRolePrimary,
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
			// Once the caller has released this attachment's IPs, ErrAllocated
			// may identify another owner and must no longer be ignored merely
			// because the stale annotation is still present.
			name: "expect error reacquiring a released annotated IP that is already allocated",
			ipam: true,
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
			},
			args: args{
				ipAllocator: &ipAllocatorStub{
					allocateIPsError: ipam.ErrAllocated,
				},
				requireIPAMReservation: true,
			},
			wantErr: true,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.4/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.4/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("169.254.169.5"),
							Mask: net.CIDRMask(32, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role: types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.4/24"),
			role:                      types.NetworkRolePrimary,
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
					nextIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
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
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("169.254.169.5"),
							Mask: net.CIDRMask(32, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role: types.NetworkRolePrimary,
			},
			role: types.NetworkRolePrimary,
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
					nextIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
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
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("169.254.169.5"),
							Mask: net.CIDRMask(32, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role: types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			role:                      types.NetworkRolePrimary,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantErr:         true,
			wantReleasedIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with IPAM, honor a IP and MAC request through the network
			// selection element
			name: "expect requested MAC, IPAM",
			ipam: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: requestedMAC,
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      requestedMACParsed,
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("169.254.169.5"),
							Mask: net.CIDRMask(32, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("100.64.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				Role: types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			role:                      types.NetworkRolePrimary, // has to be primary network for default routes to be set
		},
		{
			// on primary networks with IPAM and layer2 topology, expect success when EnablePreconfiguredUDNAddresses is enabled
			name:                            "expect success, static IP and MAC with IPAM on primary network when EnablePreconfiguredUDNAddresses is enabled",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			role:                            types.NetworkRolePrimary, // has to be primary network for default routes to be set
			persistentIPAllocation:          true,
			isSingleStackIPv4:               true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					MacRequest: requestedMAC,
					IPRequest:  []string{"192.168.0.101/24"},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.101/24"),
				MAC:      requestedMACParsed,
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.65.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.101/24"),
		},
		{
			// on primary networks with IPAM and layer2 topology, expect success when EnablePreconfiguredUDNAddresses is enabled
			name:                            "expect success, just static IP with IPAM on primary network when EnablePreconfiguredUDNAddresses is enabled",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			isSingleStackIPv4:               true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24"},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.101/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.101/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.101/24")[0].IP),
				Gateways: []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("100.65.0.0").To4(),
							Mask: net.CIDRMask(16, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.101/24"),
		},

		{
			// on networks with IPAM and layer2 topology, expect error when EnablePreconfiguredUDNAddresses is false
			name:                   "expect error, static IP with IPAM on layer2 when EnablePreconfiguredUDNAddresses is false",
			ipam:                   true,
			role:                   types.NetworkRolePrimary,
			persistentIPAllocation: true,
			// enablePreconfiguredUDNAddresses defaults to false
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24"},
				},
			},
			wantErr: true,
		},
		{
			// on networks with IPAM and layer2 topology, expect error when IPAMClaims status IPs do not match requested IPs
			name:                            "expect error, static IP with IPAM on layer2 when IPAMClaims status IPs do not match requested IPs",
			ipam:                            true,
			role:                            types.NetworkRolePrimary,
			persistentIPAllocation:          true,
			enablePreconfiguredUDNAddresses: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest:          []string{"192.168.0.101/24"},
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
			},
			wantErr: true,
		},
		{
			// with preconfigured UDN address feature enabled still continue failing with secondary layer2 with ipam + static IPs
			name:                            "expect error, static IP with IPAM on secondary network when EnablePreconfiguredUDNAddresses is enabled",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24"},
				},
			},
			role:    types.NetworkRoleSecondary,
			wantErr: true,
		},
		{
			// IP family validation: dual-stack network with correct IPs (1 IPv4 + 1 IPv6)
			name:                            "expect success, dual-stack layer2 primary with correct IPs (1 IPv4 + 1 IPv6)",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24", "2001:db8::101/64"},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs: ovntest.MustParseIPNets("192.168.0.101/24", "2001:db8::101/64"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:            ovntest.MustParseIPNets("192.168.0.101/24", "2001:db8::101/64"),
				MAC:            util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.101/24")[0].IP),
				Gateways:       []net.IP{ovntest.MustParseIP("192.168.0.1").To4(), ovntest.MustParseIP("2001:db8::1")},
				GatewayIPv6LLA: util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(ovntest.MustParseIP("100.65.0.4"))),
				Routes: []util.PodRoute{
					{
						Dest:    ovntest.MustParseIPNet("100.65.0.0/16"),
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
					{
						Dest:    ovntest.MustParseIPNet("fd99::/64"),
						NextHop: ovntest.MustParseIP("2001:db8::1"),
					},
				},
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.101/24", "2001:db8::101/64"),
		},
		{
			// IP family validation: dual-stack network with only IPv4 (should fail)
			name:                            "expect error, dual-stack layer2 primary with only IPv4",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24"},
				},
			},
			wantErr: true,
		},
		{
			// IP family validation: dual-stack network with only IPv6 (should fail)
			name:                            "expect error, dual-stack layer2 primary with only IPv6",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"2001:db8::101/64"},
				},
			},
			wantErr: true,
		},
		{
			// IP family validation: single-stack IPv4 network with IPv6 (should fail)
			name:                            "expect error, single-stack IPv4 layer2 primary with IPv6 IP",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			isSingleStackIPv4:               true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"2001:db8::101/64"},
				},
			},
			wantErr: true,
		},
		{
			// IP family validation: single-stack IPv6 network with IPv4 (should fail)
			name:                            "expect error, single-stack IPv6 layer2 primary with IPv4 IP",
			ipam:                            true,
			enablePreconfiguredUDNAddresses: true,
			persistentIPAllocation:          true,
			role:                            types.NetworkRolePrimary,
			isSingleStackIPv6:               true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPRequest: []string{"192.168.0.101/24"},
				},
			},
			wantErr: true,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
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
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.200/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.200/24")[0].IP),
			},
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.200/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.200/24")[0].IP),
				TunnelID: 100,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod:            true,
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				TunnelID: 100,
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
					nextIPs: ovntest.MustParseIPNets("192.168.0.3/24"),
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.3/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.3/24")[0].IP),
				TunnelID: 100,
			},
			wantReleasedIPsOnRollback: ovntest.MustParseIPNets("192.168.0.3/24"),
		},
		{
			// on networks with ID allocation, expect allocated ID
			name:               "expect ID allocation",
			tunnelIDAllocation: true,
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
				Role:     types.NetworkRolePrimary,
			},
			wantUpdatedPod:          true,
			wantRelasedIDOnRollback: true,
			role:                    types.NetworkRolePrimary,
		},
		{
			// on networks with ID allocation, already allocated, expect
			// allocated ID
			name:               "expect already allocated ID",
			tunnelIDAllocation: true,
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
			name:               "expect ID allocation error",
			tunnelIDAllocation: true,
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
			name:               "expect error, release ID",
			tunnelIDAllocation: true,
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
		{
			// Test ErrAllocated is always skipped with EnablePreconfiguredUDNAddresses disabled (legacy behavior)
			name:                            "ErrAllocated should be skipped when EnablePreconfiguredUDNAddresses disabled",
			ipam:                            true,
			persistentIPAllocation:          true,
			enablePreconfiguredUDNAddresses: false,
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
					nextIPs:          ovntest.MustParseIPNets("192.168.0.200/24"),
					allocateIPsError: ipam.ErrAllocated,
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:            ovntest.MustParseIPNets("192.168.0.200/24"),
				MAC:            util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.200/24")[0].IP),
				Gateways:       []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				GatewayIPv6LLA: util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(ovntest.MustParseIP("100.65.0.4"))),
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("100.65.0.0").To4(),
							Mask: net.CIDRMask(16, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			// With legacy behavior (feature flag disabled), IPs should NOT be tracked for rollback when hasIPAMClaim is true
			role: types.NetworkRolePrimary,
		},
		{
			// Test ErrAllocated with EnablePreconfiguredUDNAddresses enabled and network annotation persisted - should not fail with ErrAllocated
			name:                            "Pod with persisted annotation should skip ErrAllocated",
			ipam:                            true,
			persistentIPAllocation:          true,
			enablePreconfiguredUDNAddresses: true,
			podAnnotation: &util.PodAnnotation{
				IPs: ovntest.MustParseIPNets("192.168.0.150/24"),
				MAC: util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.150/24")[0].IP),
			},
			args: args{
				ipAllocator: &ipAllocatorStub{
					nextIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
					allocateIPsError: ipam.ErrAllocated, // Should be skipped because network already allocated
				},
			},
			wantPodAnnotation: &util.PodAnnotation{
				IPs:      ovntest.MustParseIPNets("192.168.0.150/24"),
				MAC:      util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.150/24")[0].IP),
				TunnelID: 100,
			},
			// No wantUpdatedPod because annotation already exists and no changes needed
		},
		{
			// Test VM restart/migration case: new pod spawned with no network annotation but IPAMClaim has IPs
			name:                            "VM restart/migration new pod with IPAMClaim IPs should skip ErrAllocated",
			ipam:                            true,
			persistentIPAllocation:          true,
			enablePreconfiguredUDNAddresses: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPAMClaimReference: "vm-ipam-claim",
				},
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vm-ipam-claim",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{"192.168.0.250/24"}, // IPAMClaim has IPs from previous pod
					},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
					allocateIPsError: ipam.ErrAllocated, // Should be skipped because IPAMClaim has IPs
				},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				IPs:            ovntest.MustParseIPNets("192.168.0.250/24"),
				MAC:            util.IPAddrToHWAddr(ovntest.MustParseIPNets("192.168.0.250/24")[0].IP),
				Gateways:       []net.IP{ovntest.MustParseIP("192.168.0.1").To4()},
				GatewayIPv6LLA: util.HWAddrToIPv6LLA(util.IPAddrToHWAddr(ovntest.MustParseIP("100.65.0.4"))),
				Routes: []util.PodRoute{
					{
						Dest: &net.IPNet{
							IP:   ovntest.MustParseIP("100.65.0.0").To4(),
							Mask: net.CIDRMask(16, 32),
						},
						NextHop: ovntest.MustParseIP("192.168.0.1").To4(),
					},
				},
				TunnelID: 100,
				Role:     types.NetworkRolePrimary,
			},
			role: types.NetworkRolePrimary,
		},
		{
			// Test ErrAllocated when pod with no annotation and IPAMClaim has no IPs allocated yet - should fail on ErrAllocated
			name:                            "New pod with IPAMClaim but no IPs yet should fail on ErrAllocated",
			ipam:                            true,
			persistentIPAllocation:          true,
			enablePreconfiguredUDNAddresses: true,
			args: args{
				network: &nadapi.NetworkSelectionElement{
					IPAMClaimReference: "empty-ipam-claim",
					IPRequest:          []string{"192.168.0.100/24"}, // Request specific IP to trigger AllocateIPs call
				},
				reallocate: false, // Don't reallocate on error
				ipamClaim: &ipamclaimsapi.IPAMClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "empty-ipam-claim",
					},
					Status: ipamclaimsapi.IPAMClaimStatus{
						IPs: []string{}, // No IPs allocated yet
					},
				},
				ipAllocator: &ipAllocatorStub{
					nextIPs:          ovntest.MustParseIPNets("192.168.0.3/24"),
					allocateIPsError: ipam.ErrAllocated, // Should NOT be skipped, should cause failure
				},
			},
			wantErr: true, // Should fail because ErrAllocated is not skipped
		},
		{
			// In a scenario of VM migration multiple pods using the same network configuration including the MAC address.
			// When the migration destination pod is created, the pod-allocator should relax ErrMACReserved error
			// to allow the migration destination pod use the same MAC as the migration source pod, for the migration to succeed.
			name: "macRegistry should not release already reserved MAC on rollback",
			args: args{
				network:     &nadapi.NetworkSelectionElement{MacRequest: requestedMAC},
				macRegistry: &macRegistryStub{reserveErr: mac.ErrMACReserved},
			},
			wantPodAnnotation: &util.PodAnnotation{
				MAC:      requestedMACParsed,
				TunnelID: 100,
			},
			wantReservedMAC:          requestedMACParsed,
			wantReleaseMACOnRollback: nil,
		},
		{
			// on localnet networks configured with ipam.type "dhcp", the IPAM
			// mode is surfaced in the pod annotation so whoever reads the
			// annotation knows the IP is externally assigned. The persisted
			// MAC keeps the DHCP identity stable, so the server hands the
			// same lease back across sandbox recreations and guest renewals
			name:    "expect IPAM mode dhcp, localnet network with dhcp ipam.type",
			netInfo: localnetNetInfo(types.IPAMTypeDHCP),
			nadName: util.GetNADName("namespace", "network"),
			args: args{
				network: &nadapi.NetworkSelectionElement{MacRequest: requestedMAC},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				MAC:      requestedMACParsed,
				IPAMMode: types.IPAMTypeDHCP,
			},
		},
		{
			// on localnet networks without ipam.type, no IPAM mode is set in
			// the pod annotation
			name:    "expect empty IPAM mode, localnet network without ipam.type",
			netInfo: localnetNetInfo(""),
			nadName: util.GetNADName("namespace", "network"),
			args: args{
				network: &nadapi.NetworkSelectionElement{MacRequest: requestedMAC},
			},
			wantUpdatedPod: true,
			wantPodAnnotation: &util.PodAnnotation{
				MAC: requestedMACParsed,
			},
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

			config.OVNKubernetesFeature.EnableMultiNetwork = !tt.multiNetworkDisabled
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnablePreconfiguredUDNAddresses = tt.enablePreconfiguredUDNAddresses

			var macRegistry mac.Register
			macRegistry = mac.NewManager()
			if tt.args.macRegistry != nil {
				macRegistry = tt.args.macRegistry
			}

			config.IPv4Mode = true
			if tt.isSingleStackIPv6 {
				config.IPv4Mode = false
			}
			config.IPv6Mode = true
			if tt.isSingleStackIPv4 {
				config.IPv6Mode = false
			}
			if tt.netInfo == nil {
				tt.netInfo = &util.DefaultNetInfo{}
				tt.nadName = types.DefaultNetworkName
				if !tt.ipam || tt.tunnelIDAllocation || tt.persistentIPAllocation || tt.args.ipamClaim != nil {
					tt.nadName = util.GetNADName(network.Namespace, network.Name)
					var subnets string
					if tt.ipam {
						subnets = "192.168.0.0/24,2001:db8::/64"
						if tt.isSingleStackIPv4 {
							subnets = "192.168.0.0/24"
						} else if tt.isSingleStackIPv6 {
							subnets = "2001:db8::/64"
						}
					}
					tt.netInfo, err = util.NewNetInfo(&ovncnitypes.NetConf{
						Topology: types.Layer2Topology,
						NetConf: cnitypes.NetConf{
							Name: network.Name,
						},
						NADName:            tt.nadName,
						Subnets:            subnets,
						AllowPersistentIPs: tt.persistentIPAllocation,
						Role:               tt.role,
					})
					if err != nil {
						t.Fatalf("failed to create NetInfo: %v", err)
					}
				}
			}
			if tt.args.idAllocator == nil && util.DoesNetworkRequireTunnelIDs(tt.netInfo) {
				tt.args.idAllocator = &idAllocatorStub{
					nextID: 100,
				}
			}

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-id": "4",
					},
				},
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "namespace",
				},
			}
			if tt.podAnnotation != nil {
				pod.Annotations, err = util.MarshalPodAnnotation(nil, tt.podAnnotation, tt.nadName)
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
				tt.netInfo,
				node,
				pod,
				fmt.Sprintf("%s/%s", network.Namespace, network.Name),
				network,
				claimsReconciler,
				macRegistry,
				tt.args.reallocate,
				tt.args.requireIPAMReservation,
				tt.role,
			)

			if tt.args.ipAllocator != nil {
				releasedIPs := tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs
				g.Expect(releasedIPs).To(gomega.Equal(tt.wantReleasedIPs), "Release IP on error behaved unexpectedly")
				tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs = nil
			}

			if tt.args.idAllocator != nil {
				wantReleaseID := tt.wantReleaseID
				if tt.wantErr && util.DoesNetworkRequireTunnelIDs(tt.netInfo) && tt.args.idAllocator.(*idAllocatorStub).reserveIDError == nil {
					wantReleaseID = true
				}
				releasedID := tt.args.idAllocator.(*idAllocatorStub).releasedID
				g.Expect(releasedID).To(gomega.Equal(wantReleaseID), "Release ID on error behaved unexpectedly")
				tt.args.idAllocator.(*idAllocatorStub).releasedID = false
			}

			if tt.args.macRegistry != nil {
				reservedMAC := tt.args.macRegistry.reservedMAC
				g.Expect(reservedMAC).To(gomega.Equal(tt.wantReservedMAC), "Reserve MAC on error behaved unexpectedly")
				tt.args.macRegistry.reservedMAC = nil
			}

			rollback()

			if tt.args.ipAllocator != nil {
				releasedIPs := tt.args.ipAllocator.(*ipAllocatorStub).releasedIPs
				g.Expect(releasedIPs).To(gomega.Equal(tt.wantReleasedIPsOnRollback), "Release IP on rollback behaved unexpectedly: %s", tt.netInfo.TopologyType())
			}

			if tt.args.idAllocator != nil {
				wantRelasedIDOnRollback := tt.wantRelasedIDOnRollback
				if !tt.wantErr && util.DoesNetworkRequireTunnelIDs(tt.netInfo) {
					wantRelasedIDOnRollback = true
				}
				releasedID := tt.args.idAllocator.(*idAllocatorStub).releasedID
				g.Expect(releasedID).To(gomega.Equal(wantRelasedIDOnRollback), "Release ID on rollback behaved unexpectedly")
			}

			if tt.args.macRegistry != nil {
				releaseMAC := tt.args.macRegistry.releaseMAC
				g.Expect(releaseMAC).To(gomega.Equal(tt.wantReleaseMACOnRollback), "Release MAC on rollback behaved unexpectedly")
				tt.args.macRegistry.releaseMAC = nil
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
				if util.DoesNetworkRequireTunnelIDs(tt.netInfo) {
					g.Expect(podAnnotation.TunnelID).To(gomega.Equal(100), "Expected tunnel ID")
				}
				return
			}
			g.Expect(podAnnotation).To(gomega.Equal(tt.wantPodAnnotation), "diff: %s", cmp.Diff(tt.wantPodAnnotation, podAnnotation))

			if tt.wantUpdatedPod {
				g.Expect(pod).NotTo(gomega.BeNil(), "Expected an updated pod")
			}
		})
	}
}
