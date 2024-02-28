package persistentips

import (
	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Allocator acts on IPAMClaim events handed off by the cluster network
// controller and allocates or releases IPs for IPAMClaims.
type Allocator struct {
}

// NewPersistentIPsAllocator builds a new PersistentIPsAllocator
func NewPersistentIPsAllocator(kube kube.InterfaceOVN, ipAllocator subnet.NamedAllocator) *Allocator {
	pipsAllocator := &Allocator{}
	return pipsAllocator
}

// Reconcile updates an IPAMClaim with the IP addresses allocated to the pod's
// interface
func (a *Allocator) Reconcile(ipamClaim *ipamclaimsapi.IPAMClaim, ips []string) error {
	return nil
}

type IPAMClaimFetcher struct {
	netInfo util.NetInfo // we need this to know if the network supports IPAM
	lister  ipamclaimslister.IPAMClaimLister
}

func NewIPAMClaimsFetcher(netInfo util.NetInfo, lister ipamclaimslister.IPAMClaimLister) *IPAMClaimFetcher {
	return &IPAMClaimFetcher{
		netInfo: netInfo,
		lister:  lister,
	}
}

func (icf *IPAMClaimFetcher) FindIPAMClaim(network *nadapi.NetworkSelectionElement) (*ipamclaimsapi.IPAMClaim, error) {
	return nil, nil
}
