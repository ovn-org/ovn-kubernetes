package persistentips

import (
	"net"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
)

type IPReleaser interface {
	ReleaseIPs(ips []*net.IPNet) error
}

type PersistentAllocations interface {
	FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error)

	Reconcile(oldIPAMClaim *ipamclaimsapi.IPAMClaim, newIPAMClaim *ipamclaimsapi.IPAMClaim, ipReleaser IPReleaser) error
}

// IPAMClaimReconciler acts on IPAMClaim events handed off by the cluster network
// controller and allocates or releases IPs for IPAMClaims.
type IPAMClaimReconciler struct {
}

// NewIPAMClaimReconciler builds a new PersistentIPsAllocator
func NewIPAMClaimReconciler() *IPAMClaimReconciler {
	pipsAllocator := &IPAMClaimReconciler{}
	return pipsAllocator
}

// Reconcile updates an IPAMClaim with the IP addresses allocated to the pod's
// interface
func (icr *IPAMClaimReconciler) Reconcile(
	oldIPAMClaim *ipamclaimsapi.IPAMClaim,
	newIPAMClaim *ipamclaimsapi.IPAMClaim,
	ipReleaser IPReleaser,
) error {
	return nil
}

func (icr *IPAMClaimReconciler) FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error) {
	return nil, nil
}
