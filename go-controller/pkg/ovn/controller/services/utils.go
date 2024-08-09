package services

import (
	"net"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
)

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod
func hasHostEndpoints(endpointIPs []string) bool {
	for _, endpointIP := range endpointIPs {
		if IsHostEndpoint(endpointIP) {
			return true
		}
	}
	return false
}

// IsHostEndpoint determines if the given endpoint ip belongs to a host networked pod
func IsHostEndpoint(endpointIP string) bool {
	for _, clusterNet := range globalconfig.Default.ClusterSubnets {
		if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
			return false
		}
	}
	return true
}

func getExternalIDsForLoadBalancer(service *v1.Service, netInfo util.NetInfo) map[string]string {
	nsn := ktypes.NamespacedName{Namespace: service.Namespace, Name: service.Name}

	gk := service.GroupVersionKind().GroupKind()
	if gk.String() == "" {
		kinds, _, err := scheme.Scheme.ObjectKinds(service)
		if err != nil || len(kinds) == 0 || len(kinds) > 1 {
			klog.Warningf("Object %v either has no GroupVersionKind or has an ambiguous GroupVersionKind: %#v, err", service, err)
		}
		gk = kinds[0].GroupKind()
	}

	role := types.NetworkRoleDefault
	if !netInfo.IsDefault() {
		role = types.NetworkRolePrimary
	}

	return map[string]string{
		types.LoadBalancerOwnerExternalID: nsn.String(),
		types.LoadBalancerKindExternalID:  gk.String(),
		types.NetworkExternalID:           netInfo.GetNetworkName(),
		types.NetworkRoleExternalID:       role,
	}
}
