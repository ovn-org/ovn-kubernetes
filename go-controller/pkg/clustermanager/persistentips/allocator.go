package persistentips

import (
	"fmt"
	"net"
	"strings"

	"k8s.io/klog/v2"

	persistentipsapi "github.com/maiqueb/persistentips/pkg/crd/persistentip/v1alpha1"
	ipam "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// PersistentIPsAllocator acts on persistentips events handed off by the cluster network controller
// and allocates or releases resources (IPs and tunnel IDs at the time of this
// writing) to persistentips on behalf of cluster manager.
type PersistentIPsAllocator struct {
	netInfo util.NetInfo

	kube kube.InterfaceOVN

	// ipAllocator of IPs within subnets
	ipAllocator subnet.NamedAllocator
}

// NewPersistentIPsAllocator builds a new PersistentIPsAllocator
func NewPersistentIPsAllocator(netInfo util.NetInfo, kube kube.InterfaceOVN, ipAllocator subnet.NamedAllocator) *PersistentIPsAllocator {
	pipsAllocator := &PersistentIPsAllocator{
		netInfo:     netInfo,
		kube:        kube,
		ipAllocator: ipAllocator,
	}

	return pipsAllocator
}

// Reconcile allocates or releases IPs for persistentips updating the persistentip annotation
// as necessary with all the additional information derived from those IPs
func (a *PersistentIPsAllocator) Delete(pips *persistentipsapi.IPAMClaim) error {
	if len(pips.Status.IPs) == 0 {
		return nil
	}
	ips := []*net.IPNet{}
	ips, err := util.ParseIPNets(pips.Status.IPs)
	if err != nil {
		return fmt.Errorf("failed parsing ipnets releasing persistent IPs: %v", err)
	}
	if err := a.ipAllocator.ReleaseIPs(ips); err != nil {
		return fmt.Errorf("failed releasing persistent IPs: %v", err)
	}
	fmt.Printf("deleteme, pips, Delete, ReleaseIPs: %+v\n", ips)
	return nil
}

func (a *PersistentIPsAllocator) Reconcile(ipamClaim *persistentipsapi.IPAMClaim) error {
	klog.V(5).Infof("reconciling IPAMClaim %q", ipamClaim.Name)
	if len(ipamClaim.Status.IPs) > 0 {
		klog.V(5).Infof("already have neat lookin' IPs for: %q. Bail out !", ipamClaim.Name)
		return nil
	}
	ips, err := a.ipAllocator.AllocateNextIPs()
	if err != nil {
		return fmt.Errorf("error allocating persistent IPs for IPAM Claim %q: %v", ipamClaim.Name, err)
	}
	var ipsString []string
	for _, ip := range ips {
		ipsString = append(ipsString, ip.String())
	}

	if err := a.kube.UpdateIPAMClaimIPs(ipamClaim, ipsString); err != nil {
		return fmt.Errorf(
			"failed to update the allocation %q with allocations %q: %v",
			ipamClaim.Name,
			strings.Join(ipsString, ","),
			err,
		)
	}

	return nil
}

// Sync initializes the allocator with persistentips that already exist on the cluster
func (a *PersistentIPsAllocator) Sync(objs []interface{}) error {
	ips := []*net.IPNet{}
	for _, obj := range objs {
		pips, ok := obj.(*persistentipsapi.IPAMClaim)
		if !ok {
			klog.Errorf("Could not cast %T object to *persistentipsapi.IPAMClaim", obj)
			continue
		}
		if len(pips.Status.IPs) == 0 {
			continue
		}
		ipnets, err := util.ParseIPNets(pips.Status.IPs)
		if err != nil {
			return fmt.Errorf("failed at parsing IP when allocating persistent IPs: %v", err)
		}
		ips = append(ips, ipnets...)
	}
	if len(ips) > 0 {
		if err := a.ipAllocator.AllocateIPs(ips); !ipam.IsErrAllocated(err) {
			return fmt.Errorf("failed allocating persistent ips: %v", err)
		}
		fmt.Printf("deleteme, pips, Sync, AllocateIPs: %+v\n", ips)
	}
	return nil
}
