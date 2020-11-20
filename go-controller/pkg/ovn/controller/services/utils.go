package services

import (
	"net"
	"strconv"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/acl"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/gateway"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/klog/v2"
)

// return the endpoints that belong to the IPFamily as a slice of IP:Port
func getLbEndpoints(slices []*discovery.EndpointSlice, svcPort v1.ServicePort, family v1.IPFamily) []string {
	// return an empty object so the caller don't have to check for nil and can use it as an iterator
	if len(slices) == 0 {
		return []string{}
	}
	// Endpoint Slices are allowed to have duplicate endpoints
	// we use a set to deduplicate endpoints
	lbEndpoints := sets.NewString()
	for _, slice := range slices {
		klog.V(4).Infof("Getting endpoints for slice %s", slice.Name)
		// Only return addresses that belong to the requested IP family
		if slice.AddressType != discovery.AddressType(family) {
			klog.V(4).Infof("Slice %s with different IP Family endpoints, requested: %s received: %s",
				slice.Name, slice.AddressType, family)
			continue
		}

		// build the list of endpoints in the slice
		for _, port := range slice.Ports {
			// If Service port name set it must match the name field in the endpoint
			if svcPort.Name != "" && svcPort.Name != *port.Name {
				klog.V(5).Infof("Slice %s with different Port name, requested: %s received: %s",
					slice.Name, svcPort.Name, *port.Name)
				continue
			}

			// Get the targeted port
			tgtPort := int32(svcPort.TargetPort.IntValue())
			// If this is a string, it will return 0
			// it has to match the port name
			// otherwise, it has to match the port number
			if (tgtPort == 0 && svcPort.TargetPort.String() != *port.Name) ||
				(tgtPort > 0 && tgtPort != *port.Port) {
				continue
			}

			// Skip ports that doesn't match the protocol
			if *port.Protocol != svcPort.Protocol {
				klog.V(5).Infof("Slice %s with different Port protocol, requested: %s received: %s",
					slice.Name, svcPort.Protocol, *port.Protocol)
				continue
			}

			for _, endpoint := range slice.Endpoints {
				// Skip endpoints that are not ready
				if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
					klog.V(4).Infof("Slice endpoints Not Ready")
					continue
				}
				for _, ip := range endpoint.Addresses {
					klog.V(4).Infof("Adding slice %s endpoints: %s %d", slice.Name, ip, *port.Port)
					lbEndpoints.Insert(util.JoinHostPortInt32(ip, *port.Port))
				}
			}
		}
	}

	klog.V(4).Infof("LB Endpoints for %s are: %v", slices[0].Labels[discovery.LabelServiceName], lbEndpoints.List())
	return lbEndpoints.List()
}

func deleteVIPsFromOVN(vips sets.String, st *serviceTracker, name, namespace, clusterPortGroupUUID string) error {
	// Obtain the VIPs associated to the Service from the Service Tracker
	for vipKey := range vips {
		// the VIP is stored with the format IP:Port/Protocol
		vip, proto := splitVirtualIPKey(vipKey)
		// ClusterIP use a global load balancer per protocol
		lbID, err := loadbalancer.GetOVNKubeLoadBalancer(proto)
		if err != nil {
			klog.Errorf("Error getting OVN LoadBalancer for protocol %s", proto)
			return err
		}
		// Delete the Service VIP from OVN
		klog.Infof("Deleting service %s on namespace %s from OVN", name, namespace)
		if err := loadbalancer.DeleteLoadBalancerVIP(lbID, vip); err != nil {
			klog.Errorf("Error deleting VIP %s on OVN LoadBalancer %s", vip, lbID)
			return err
		}
		// handle reject ACLs for services without endpoints
		// eventually we have to remove this because it will
		// be implemented by OVN
		// https://github.com/ovn-org/ovn-kubernetes/pull/1887
		ip, portString, err := net.SplitHostPort(vip)
		if err != nil {
			return err
		}
		port, err := strconv.Atoi(portString)
		if err != nil {
			return err
		}
		rejectACLName := loadbalancer.GenerateACLNameForOVNCommand(lbID, ip, int32(port))
		aclID, err := acl.GetACLByName(rejectACLName)
		if err != nil {
			klog.Errorf("Error trying to get ACL for Service %s/%s: %v", name, namespace, err)
		}
		// delete the ACL if exist
		if len(aclID) > 0 {
			// remove acl
			err = acl.RemoveACLFromPortGroup(aclID, clusterPortGroupUUID)
			if err != nil {
				klog.Errorf("Error trying to remove ACL for Service %s/%s: %v", name, namespace, err)
			}
		}
		// end of reject ACL code
		// NodePort and ExternalIPs use loadbalancers in each node
		gatewayRouters, _, err := gateway.GetOvnGateways()
		if err != nil {
			return errors.Wrapf(err, "failed to retrieve OVN gateway routers")
		}
		// Configure the NodePort in each Node Gateway Router
		for _, gatewayRouter := range gatewayRouters {
			lbID, err := gateway.GetGatewayLoadBalancer(gatewayRouter, proto)
			if err != nil {
				klog.Warningf("Service Sync: Gateway router %s does not have load balancer (%v)",
					gatewayRouter, err)
				// TODO: why continue? should we error and requeue and retry?
				continue
			}
			// Delete the Service VIP from OVN
			klog.Infof("Deleting service %s on namespace %s from OVN", name, namespace)
			if err := loadbalancer.DeleteLoadBalancerVIP(lbID, vip); err != nil {
				klog.Errorf("Error deleting VIP %s on OVN LoadBalancer %s", vip, lbID)
				return err
			}
		}
		// Delete the Service VIP from the Service Tracker
		st.deleteServiceVIP(name, namespace, vip, proto)
	}
	return nil
}
