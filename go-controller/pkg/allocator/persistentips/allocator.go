package persistentips

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovnktypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Allocator acts on IPAMClaim events handed off by the cluster network
// controller and allocates or releases IPs for IPAMClaims.
type Allocator struct {
	kube kube.InterfaceOVN

	// ipAllocator of IPs within subnets
	ipAllocator subnet.NamedAllocator

	// networkName is used to filter relevant IPAMClaim events when syncing
	// i.e. each NetworkController has a PersistentIPs.Allocator, which syncs
	// / deletes IPAM claims for a *single* network
	networkName string
}

// NewPersistentIPsAllocator builds a new PersistentIPsAllocator
func NewPersistentIPsAllocator(kube kube.InterfaceOVN, ipAllocator subnet.NamedAllocator, networkName string) *Allocator {
	return &Allocator{
		kube:        kube,
		ipAllocator: ipAllocator,
		networkName: networkName,
	}
}

// Reconcile updates an IPAMClaim with the IP addresses allocated to the pod's
// interface
func (a *Allocator) Reconcile(ipamClaim *ipamclaimsapi.IPAMClaim, ips []string) error {
	klog.V(5).Infof("Reconciling IPAMLease %q", ipamClaim.Name)
	if len(ipamClaim.Status.IPs) > 0 {
		klog.V(5).Infof("IPAMClaim %q already features IPs - bailing out !", ipamClaim.Name)
		return nil
	}

	if err := a.kube.UpdateIPAMLeaseIPs(ipamClaim, ips); err != nil {
		return fmt.Errorf(
			"failed to update the allocation %q with allocations %q: %v",
			ipamClaim.Name,
			strings.Join(ips, ","),
			err,
		)
	}

	return nil
}

// Sync initializes the IPs allocator with the IPAMClaims already existing on
// the cluster. For live pods, therse are already allocated, so no error will
// be thrown (e.g. we ignore the `ipam.IsErrAllocated` error
func (a *Allocator) Sync(objs []interface{}) error {
	var ips []*net.IPNet
	for _, obj := range objs {
		ipamClaim, ok := obj.(*ipamclaimsapi.IPAMClaim)
		if !ok {
			klog.Errorf("Could not cast %T object to *ipamclaimsapi.IPAMClaim", obj)
			continue
		}
		if ipamClaim.Spec.Network != a.networkName {
			klog.V(5).Infof(
				"Ignoring IPAMClaim for network %q in controller: %s",
				ipamClaim.Spec.Network,
				a.networkName,
			)
			continue
		}
		ipnets, err := util.ParseIPNets(ipamClaim.Status.IPs)
		if err != nil {
			return fmt.Errorf("failed at parsing IP when allocating persistent IPs: %w", err)
		}
		ips = append(ips, ipnets...)
	}
	if len(ips) > 0 {
		if err := a.ipAllocator.AllocateIPs(ips); err != nil && !ipam.IsErrAllocated(err) {
			return fmt.Errorf("failed allocating persistent ips: %w", err)
		}
	}
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
	if icf.lister == nil ||
		!util.DoesNetworkRequireIPAM(icf.netInfo) ||
		icf.netInfo.TopologyType() == ovnktypes.Layer3Topology ||
		network.IPAMClaimReference == "" {

		return nil, nil
	}

	ipamClaimKey := network.IPAMClaimReference
	klog.V(5).Infof("IPAMClaim key: %s", ipamClaimKey)
	claim, err := icf.lister.IPAMClaims(network.Namespace).Get(ipamClaimKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get IPAMClaim %q", ipamClaimKey)
	}
	return claim, nil
}
