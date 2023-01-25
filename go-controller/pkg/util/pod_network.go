package util

import (
	"errors"
	"fmt"
	"net"

	podnetworkapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/podnetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	// OvnPodAnnotationName is the constant string representing the POD annotation key
	// deprecated
	OvnPodAnnotationName = "k8s.ovn.org/pod-networks"
	// OvnPodNetworkVersionAnnotation makes sure pod Update event will be triggered for every PodNetwork object update
	OvnPodNetworkVersionAnnotation = "k8s.ovn.org/pod-network-version"
)

var ErrNoPodIPFound = errors.New("no pod IPs found")

// podnetworkapi.OVNNetwork object is used to pass information about networking from
// the master to the nodes. podnetworkapi.OVNNetwork api object holds multiple pod networks
// and is parsed based on the required network name into SinglePodNetwork, which is used
// in internal functions. (The util.SinglePodNetwork struct is also embedded in the
// cni.PodInterfaceInfo type that is passed from the cniserver to the CNI shim.)
//
// PodNetwork object is created for every pod that has logical switch port in ovn.
// PodNetwork.Namespace = pod.Namespace, PodNetwork.Name = pod.UID, that ensures stateful set pods
// won't have conflicting PodNetworks.
//
//
// If a pod has an additional network attachment that claims the default route, then the "default" network
// will have explicit routes to the cluster and service subnets.)

// SinglePodNetwork describes the assigned network details for a single pod network. (The
// actual annotation may include the equivalent of multiple PodAnnotations.)
type SinglePodNetwork struct {
	// IPs are the pod's assigned IP addresses/prefixes
	IPs []*net.IPNet
	// MAC is the pod's assigned MAC address
	MAC net.HardwareAddr
	// Gateways are the pod's gateway IP addresses; note that there may be
	// fewer Gateways than IPs.
	Gateways []net.IP
	// Routes are additional routes to add to the pod's network namespace
	Routes []PodRoute
}

// PodRoute describes any routes to be added to the pod's network namespace
type PodRoute struct {
	// Dest is the route destination
	Dest *net.IPNet
	// NextHop is the IP address of the next hop for traffic destined for Dest
	NextHop net.IP
}

// GetOVNNetwork converts SinglePodNetwork into *podnetworkapi.OVNNetwork
func GetOVNNetwork(podInfo *SinglePodNetwork) (*podnetworkapi.OVNNetwork, error) {
	ovnNetwork := podnetworkapi.OVNNetwork{
		MAC: podInfo.MAC.String(),
	}

	if len(podInfo.IPs) == 1 && len(podInfo.Gateways) > 1 {
		return nil, fmt.Errorf("bad podNetwork data: single-stack network can only have a single gateway")
	}
	for _, ip := range podInfo.IPs {
		ovnNetwork.IPs = append(ovnNetwork.IPs, ip.String())
	}
	for _, gw := range podInfo.Gateways {
		ovnNetwork.Gateways = append(ovnNetwork.Gateways, gw.String())
	}

	for _, r := range podInfo.Routes {
		if r.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork data: default route %v should be specified as gateway", r)
		}
		var nh string
		if r.NextHop != nil {
			nh = r.NextHop.String()
		}
		ovnNetwork.Routes = append(ovnNetwork.Routes, podnetworkapi.PodRoute{
			Dest:    r.Dest.String(),
			NextHop: nh,
		})
	}
	return &ovnNetwork, nil
}

// ParsePodNetwork returns error is podNetwork is nil, if netName wasn't found in podNetwork, or for
// any parsing error.
func ParsePodNetwork(podNetwork *podnetworkapi.PodNetwork, netName string) (*SinglePodNetwork, error) {
	if podNetwork == nil {
		return nil, fmt.Errorf("podNetwork is nil")
	}
	ovnNetwork, ok := podNetwork.Spec.Networks[netName]
	if !ok {
		return nil, fmt.Errorf("network %s not found in podNetwork: %q",
			netName, podNetwork.Spec.Networks)
	}

	var err error
	podAnnotation := &SinglePodNetwork{}
	podAnnotation.MAC, err = net.ParseMAC(ovnNetwork.MAC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod MAC %q: %v", ovnNetwork.MAC, err)
	}

	if len(ovnNetwork.IPs) == 0 {
		return nil, fmt.Errorf("bad podNetwork %s data ip_addresses are not set", netName)
	}
	for _, ipstr := range ovnNetwork.IPs {
		ip, ipnet, err := net.ParseCIDR(ipstr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod IP %q: %v", ipstr, err)
		}
		ipnet.IP = ip
		podAnnotation.IPs = append(podAnnotation.IPs, ipnet)
	}

	for _, gwstr := range ovnNetwork.Gateways {
		gw := net.ParseIP(gwstr)
		if gw == nil {
			return nil, fmt.Errorf("failed to parse pod gateway %q", gwstr)
		}
		podAnnotation.Gateways = append(podAnnotation.Gateways, gw)
	}

	for _, r := range ovnNetwork.Routes {
		route := PodRoute{}
		_, route.Dest, err = net.ParseCIDR(r.Dest)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pod route dest %q: %v", r.Dest, err)
		}
		if route.Dest.IP.IsUnspecified() {
			return nil, fmt.Errorf("bad podNetwork %s data: default route %v should be specified as gateway", netName, route)
		}
		if r.NextHop != "" {
			route.NextHop = net.ParseIP(r.NextHop)
			if route.NextHop == nil {
				return nil, fmt.Errorf("failed to parse pod route next hop %q", r.NextHop)
			} else if utilnet.IsIPv6(route.NextHop) != utilnet.IsIPv6CIDR(route.Dest) {
				return nil, fmt.Errorf("pod route %s has next hop %s of different family", r.Dest, r.NextHop)
			}
		}
		podAnnotation.Routes = append(podAnnotation.Routes, route)
	}

	return podAnnotation, nil
}

// GetPodCIDRsWithFullMask returns the pod's IP addresses in a CIDR with FullMask format
// Internally it calls GetPodIPsOfNetwork for DefaultNetwork
// PodNetwork may be nil
func GetPodCIDRsWithFullMask(podNetwork *podnetworkapi.PodNetwork, pod *kapi.Pod) ([]*net.IPNet, error) {
	podIPs, err := GetPodIPsOfNetwork(podNetwork, pod, nil)
	if err != nil {
		return nil, err
	}
	ips := make([]*net.IPNet, 0, len(podIPs))
	for _, podIP := range podIPs {
		podIPStr := podIP.String()
		mask := GetIPFullMask(podIPStr)
		_, ipnet, err := net.ParseCIDR(podIPStr + mask)
		if err != nil {
			// this should not happen;
			klog.Warningf("Failed to parse pod IP %v err: %v", podIP, err)
			continue
		}
		ips = append(ips, ipnet)
	}
	return ips, nil
}

// GetPodIPsOfNetwork returns the pod's IP addresses, first from the PodNetwork object
// and then falling back to the Pod Status IPs. This function is intended to
// also return IPs for HostNetwork and other non-OVN-IPAM-ed pods.
// PodNetwork may be nil
func GetPodIPsOfNetwork(podNetworks *podnetworkapi.PodNetwork, pod *kapi.Pod, nInfo NetInfo) ([]net.IP, error) {
	// if netInfo is nil, this is called fro default network
	nadName := types.DefaultNetworkName

	if nInfo != nil && nInfo.IsSecondary() {
		on, network, err := PodWantsMultiNetwork(pod, nInfo)
		if err != nil {
			return nil, err
		} else if !on {
			// the pod is not attached to this specific network, don't return error
			return []net.IP{}, nil
		}
		nadName = GetNADName(network.Namespace, network.Name)
	}

	if podNetworks != nil {
		podNetwork, err := ParsePodNetwork(podNetworks, nadName)
		if err == nil && podNetwork != nil {
			// Use the podNetwork if valid
			ips := make([]net.IP, 0, len(podNetwork.IPs))
			for _, ip := range podNetwork.IPs {
				ips = append(ips, ip.IP)
			}
			// podNetwork should never have empty IPs, but just in case
			if len(ips) > 0 {
				return ips, nil
			}
			klog.Warningf("No IPs found in existing OVN PodNetwork! Pod Name: %s, PodNetwork: %#v",
				pod.Name, podNetwork)
		}
	}

	// Otherwise if the podNetwork is not valid try to use Kube API pod IPs
	ips := make([]net.IP, 0, len(pod.Status.PodIPs))
	for _, podIP := range pod.Status.PodIPs {
		ip := utilnet.ParseIPSloppy(podIP.IP)
		if ip == nil {
			klog.Warningf("Failed to parse pod IP %q", podIP)
			continue
		}
		ips = append(ips, ip)
	}

	if len(ips) > 0 {
		return ips, nil
	}

	// Fallback check pod.Status.PodIP
	// Kubelet < 1.16 only set podIP
	ip := utilnet.ParseIPSloppy(pod.Status.PodIP)
	if ip == nil {
		return nil, fmt.Errorf("pod %s/%s: %w ", pod.Namespace, pod.Name, ErrNoPodIPFound)
	}

	return []net.IP{ip}, nil
}

func BuildPodNetwork(pod *kapi.Pod, podInfo *SinglePodNetwork, netName string) (*podnetworkapi.PodNetwork, error) {
	ovnNet, err := GetOVNNetwork(podInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to get ovnNetwork CRD: %v", err)
	}
	return CreatePodNetworkFromOvnNet(pod, ovnNet, netName), nil
}

func GetPodNetworkName(pod *kapi.Pod) string {
	// Since podNetworks are deleted with 60 sec delay, e.g. stateful set pod may be re-created, and
	// old, not-yet-deleted podNetwork will be overwritten with the newly created pod.
	// Therefore, we use pod.UID as podNetwork Name, to avoid collision
	return string(pod.UID)
}

// CreatePodNetworkFromOvnNet creates a PodNetwork object with a single ovnNet network in the spec.
// PodNetwork.Namespace = pod.Namespace, PodNetwork.Name = pod.UID
func CreatePodNetworkFromOvnNet(pod *kapi.Pod, ovnNet *podnetworkapi.OVNNetwork, netName string) *podnetworkapi.PodNetwork {
	podNet := &podnetworkapi.PodNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GetPodNetworkName(pod),
			Namespace: pod.Namespace,
		},
		Spec: podnetworkapi.PodNetworkSpec{
			Networks: map[string]podnetworkapi.OVNNetwork{
				netName: *ovnNet,
			},
			PodName:  pod.Name,
			NodeName: pod.Spec.NodeName,
		},
	}
	ownerRef := &metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       pod.GetName(),
		UID:        pod.GetUID(),
	}
	podNet.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})
	return podNet
}
