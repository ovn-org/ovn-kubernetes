package services

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

// deleteServiceFrom LegacyLBs removes any of a service's vips from
// the legacy shared load balancers.
// This misses cleaning up NodePort services, but those will be caught
// when the repair PostSync is done.
func deleteServiceFromLegacyLBs(nbClient libovsdbclient.Client, service *v1.Service) error {
	vipPortsPerProtocol := map[v1.Protocol]sets.String{}

	// Generate list of vip:port by proto
	ips := append([]string{}, service.Spec.ClusterIPs...)
	if len(ips) == 0 {
		ips = append(ips, service.Spec.ClusterIP)
	}
	ips = append(ips, service.Spec.ExternalIPs...)
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		ips = append(ips, ingress.IP)
	}
	for _, svcPort := range service.Spec.Ports {
		proto := svcPort.Protocol
		ipPorts := make([]string, 0, len(ips))
		for _, ip := range ips {
			ipPorts = append(ipPorts, util.JoinHostPortInt32(ip, svcPort.Port))
		}

		if _, ok := vipPortsPerProtocol[proto]; !ok {
			vipPortsPerProtocol[proto] = sets.NewString(ipPorts...)
		} else {
			vipPortsPerProtocol[proto].Insert(ipPorts...)
		}
	}

	legacyLBs, err := findLegacyLBs(nbClient)
	if err != nil {
		return err
	}
	if len(legacyLBs) == 0 {
		return nil
	}

	toRemove := []ovnlb.DeleteVIPEntry{}

	// Find any legacy LBs with these vips
	for _, lb := range legacyLBs {
		r := ovnlb.DeleteVIPEntry{
			LBUUID: lb.UUID,
		}

		proto := v1.Protocol(strings.ToUpper(lb.Protocol))
		for _, vip := range vipPortsPerProtocol[proto].List() {
			if _, ok := lb.VIPs[vip]; ok {
				r.VIPs = append(r.VIPs, vip)
			}
		}

		if len(r.VIPs) > 0 {
			toRemove = append(toRemove, r)
		}
	}

	if err := ovnlb.DeleteLoadBalancerVIPs(nbClient, toRemove); err != nil {
		return fmt.Errorf("failed to delete vip(s) from legacy load balancers: %w", err)
	}
	return nil
}

// getLegacyLBs returns all of the "legacy" non-per-Service load balancers
// This is any load balancer with one of the following external ID keys
// - k8s-worker-lb-<proto>
// - k8s-cluster-lb-<proto>
// - k8s-idling-lb-<proto>
// - <PROTO>_lb_gateway_router
func findLegacyLBs(nbClient libovsdbclient.Client) ([]ovnlb.CachedLB, error) {
	lbCache, err := ovnlb.GetLBCache(nbClient)
	if err != nil {
		return nil, err
	}
	lbs := lbCache.Find(nil)

	legacyLBPattern := regexp.MustCompile(`(k8s-(worker|cluster|idling)-lb-(tcp|udp|sctp)|(TCP|UDP|SCTP)_lb_gateway_router)`)

	out := []ovnlb.CachedLB{}
	for _, lb := range lbs {
		// legacy LBs had no name
		if lb.Name != "" {
			continue
		}
		for key := range lb.ExternalIDs {
			if legacyLBPattern.MatchString(key) {
				out = append(out, *lb)
				continue
			}
		}
	}
	return out, nil
}

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

const OvnServiceIdledSuffix = "idled-at"

// When idling or empty LB events are enabled, we want to ensure we receive these packets and not reject them.
func svcNeedsIdling(annotations map[string]string) bool {
	if !globalconfig.Kubernetes.OVNEmptyLbEvents {
		return false
	}

	for annotationKey := range annotations {
		if strings.HasSuffix(annotationKey, OvnServiceIdledSuffix) {
			return true
		}
	}
	return false
}
