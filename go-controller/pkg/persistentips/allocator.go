package persistentips

import (
	"errors"
	"fmt"
	"net"

	"k8s.io/klog/v2"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"

	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovnktypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var (
	ErrIgnoredIPAMClaim                   = errors.New("ignored IPAMClaim: it belongs to other network")
	ErrPersistentIPsNotAvailableOnNetwork = errors.New("ipam claims not supported on this network")
)

type IPReleaser interface {
	ReleaseIPs(ips []*net.IPNet) error
}

type IPAllocator interface {
	AllocateIPs(ips []*net.IPNet) error
}

type PersistentAllocations interface {
	FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error)

	Reconcile(oldIPAMClaim *ipamclaimsapi.IPAMClaim, newIPAMClaim *ipamclaimsapi.IPAMClaim, ipReleaser IPReleaser) error
}

// IPAMClaimReconciler acts on IPAMClaim events handed off by the cluster network
// controller and allocates or releases IPs for IPAMClaims.
type IPAMClaimReconciler struct {
	kube kube.InterfaceOVN

	// netInfo is used to filter relevant IPAMClaim events when syncing
	// i.e. each NetworkController has a PersistentIPs.IPAMClaimReconciler, which syncs
	// and deletes IPAM claims for a *single* network
	// we need this to know if the network supports IPAM
	netInfo util.NetInfo

	lister ipamclaimslister.IPAMClaimLister
}

// NewIPAMClaimReconciler builds a new PersistentIPsAllocator
func NewIPAMClaimReconciler(kube kube.InterfaceOVN, netConfig util.NetInfo, lister ipamclaimslister.IPAMClaimLister) *IPAMClaimReconciler {
	pipsAllocator := &IPAMClaimReconciler{
		kube:    kube,
		netInfo: netConfig,
		lister:  lister,
	}
	return pipsAllocator
}

// Reconcile updates an IPAMClaim with the IP addresses allocated to the pod's
// interface
func (icr *IPAMClaimReconciler) Reconcile(
	oldIPAMClaim *ipamclaimsapi.IPAMClaim,
	newIPAMClaim *ipamclaimsapi.IPAMClaim,
	ipReleaser IPReleaser,
) error {
	var ipamClaim *ipamclaimsapi.IPAMClaim
	if oldIPAMClaim != nil {
		ipamClaim = oldIPAMClaim
	}
	if newIPAMClaim != nil {
		ipamClaim = newIPAMClaim
	}

	if ipamClaim == nil {
		return nil
	}

	if newIPAMClaim == nil {
		if err := icr.releaseIPs(oldIPAMClaim, ipReleaser); err != nil {
			return fmt.Errorf("error releasing IPs %q from IPAM claim: %w", oldIPAMClaim.Status.IPs, err)
		}
		return nil
	}

	mustUpdateIPAMClaim := (oldIPAMClaim == nil ||
		len(oldIPAMClaim.Status.IPs) == 0) &&
		newIPAMClaim != nil

	if mustUpdateIPAMClaim {
		if err := icr.kube.UpdateIPAMClaimIPs(newIPAMClaim); err != nil {
			return fmt.Errorf(
				"failed to update the allocation %q with allocations %q: %w",
				newIPAMClaim.Name,
				newIPAMClaim.Status.IPs,
				err,
			)
		}
		return nil
	}

	var originalIPs []string
	if len(oldIPAMClaim.Status.IPs) > 0 {
		originalIPs = oldIPAMClaim.Status.IPs
	}

	var newIPs []string
	if newIPAMClaim != nil && len(newIPAMClaim.Status.IPs) > 0 {
		newIPs = newIPAMClaim.Status.IPs
	}

	areClaimsEqual := cmp.Equal(
		originalIPs,
		newIPs,
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
	)

	if !areClaimsEqual {
		ipamClaimKey := fmt.Sprintf("%s/%s", ipamClaim.Namespace, ipamClaim.Name)
		return fmt.Errorf(
			"failed to update IPAMClaim %q - overwriting existing IPs %q with newer IPs %q",
			ipamClaimKey,
			originalIPs,
			newIPs,
		)
	}

	return nil
}

func (icr *IPAMClaimReconciler) FindIPAMClaim(claimName string, namespace string) (*ipamclaimsapi.IPAMClaim, error) {
	if icr.lister == nil ||
		!util.DoesNetworkRequireIPAM(icr.netInfo) ||
		icr.netInfo.TopologyType() == ovnktypes.Layer3Topology ||
		claimName == "" {
		return nil, ErrPersistentIPsNotAvailableOnNetwork
	}
	claim, err := icr.lister.IPAMClaims(namespace).Get(claimName)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPAMClaim %q: %w", claimName, err)
	}
	return claim, nil
}

// Sync initializes the IPs allocator with the IPAMClaims already existing on
// the cluster. For live pods, therse are already allocated, so no error will
// be thrown (e.g. we ignore the `ipam.IsErrAllocated` error
func (icr *IPAMClaimReconciler) Sync(objs []interface{}, ipAllocator IPAllocator) error {
	for _, obj := range objs {
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			klog.Errorf("Could not cast %T object to *ipamclaimsapi.IPAMClaim", obj)
			continue
		}
		if ipamClaim.Spec.Network != icr.netInfo.GetNetworkName() {
			klog.V(5).Infof(
				"Ignoring IPAMClaim for network %q in controller: %s",
				ipamClaim.Spec.Network,
				icr.netInfo.GetNetworkName(),
			)
			continue
		}
		ipnets, err := util.ParseIPNets(ipamClaim.Status.IPs)
		if err != nil {
			return fmt.Errorf("failed at parsing IP when allocating persistent IPs: %w", err)
		}

		if len(ipnets) != 0 {
			if err := ipAllocator.AllocateIPs(ipnets); err != nil && !ipam.IsErrAllocated(err) {
				return fmt.Errorf("failed syncing persistent ips: %w", err)
			}
		}
	}
	return nil
}

func (icr *IPAMClaimReconciler) releaseIPs(ipamClaim *ipamclaimsapi.IPAMClaim, ipReleaser IPReleaser) error {
	if ipamClaim.Spec.Network != icr.netInfo.GetNetworkName() {
		return ErrIgnoredIPAMClaim
	}
	ips, err := util.ParseIPNets(ipamClaim.Status.IPs)
	if err != nil {
		klog.Errorf(
			"Failed parsing ipnets while trying to release persistent IPs %q: %v",
			ipamClaim.Status.IPs,
			err,
		)
		return nil
	}
	if err := ipReleaser.ReleaseIPs(ips); err != nil {
		return fmt.Errorf("failed releasing persistent IPs: %v", err)
	}
	klog.V(5).Infof("Released IPs: %+v", ips)
	return nil
}
