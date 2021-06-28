package services

import (
	"net"
	"regexp"
	"strings"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// deleteVIPsFromNonIdlingOVNBalancers removes the given vips for all the loadbalancers but
// the idling ones. This includes the cluster loadbalancer, the gateway routers loadbalancers
// and the node switch ones.
func deleteServiceFromLegacyLBs(service *v1.Service) error {
	vipPortsPerProtocol := map[v1.Protocol]sets.String{}
	lbsPerProtocol := map[v1.Protocol]sets.String{}

	// This misses cleaning up NodePort services, but we'll
	// catch those soon enough once all services are synced

	// Generate a list of proto:vip:port so we can remove them from the legacy LBs
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

	legacyLBs, err := findLegacyLBs()
	if err != nil {
		return err
	}
	if len(legacyLBs) == 0 {
		return nil
	}

	// Find any legacy LBs with these vips
	for _, lb := range legacyLBs {
		proto := v1.Protocol(strings.ToUpper(lb.Protocol))
		for _, vip := range vipPortsPerProtocol[proto].List() {
			if _, ok := lb.VIPs[vip]; ok {
				if _, ok := lbsPerProtocol[proto]; !ok {
					lbsPerProtocol[proto] = sets.NewString(lb.UUID)
				} else {
					lbsPerProtocol[proto].Insert(lb.UUID)
				}
			}
		}
	}

	txn := util.NewNBTxn()
	for proto := range lbsPerProtocol {
		vips := vipPortsPerProtocol[proto].List()
		lbs := lbsPerProtocol[proto].List()
		klog.V(5).Infof("Deleting service %s/%s vips %#v from %d legacy load balancers", service.Namespace, service.Name, vips, len(lbs))
		if err := ovnlb.DeleteLoadBalancerVIPs(txn, lbs, vips); err != nil {
			klog.Errorf("Error deleting VIP %v on OVN LoadBalancer %v", vipPortsPerProtocol[proto].List(), lbsPerProtocol[proto].List())
			return err
		}
	}
	if _, _, err := txn.Commit(); err != nil {
		klog.Errorf("Error deleting service %s/%s from legacy load balancers: %v", service.Namespace, service.Name)
		return err
	}
	return nil
}

// getLegacyLBs returns all of the "legacy" non-per-Service load balancers
// This is any load balancer with one of the following external ID keys
// - k8s-worker-lb-<proto>
// - k8s-cluster-lb-<proto>
// - <PROTO>_lb_gateway_router
func findLegacyLBs() ([]ovnlb.LBRow, error) {
	lbs, err := ovnlb.FindLBs(nil)
	if err != nil {
		return nil, err
	}

	legacyLBPattern := regexp.MustCompile(`(k8s-(worker|cluster)-lb-(tcp|udp|sctp)|(TCP|UDP|SCTP)_lb_gateway_router)`)

	out := []ovnlb.LBRow{}
	for _, lb := range lbs {
		// legacy LBs had no name
		if lb.Name != "" {
			continue
		}
		for key := range lb.ExternalIDs {
			if legacyLBPattern.MatchString(key) {
				out = append(out, lb)
				continue
			}
		}
	}
	return out, nil
}

// hasHostEndpoints determines if a slice of endpoints contains a host networked pod
func hasHostEndpoints(endpointIPs []string) bool {
	for _, endpointIP := range endpointIPs {
		found := false
		for _, clusterNet := range globalconfig.Default.ClusterSubnets {
			if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
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
