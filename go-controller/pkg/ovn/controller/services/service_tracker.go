package services

import (
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// serviceTrackerKey returns a string used for the tracker index.
func serviceTrackerKey(name, namespace string) string { return name + "/" + namespace }

// virtualIPKey returns a string used for the virtual IPs index.
func virtualIPKey(virtualIP string, protocol v1.Protocol) string {
	return virtualIP + "/" + string(protocol)
}

// splitVirtualIPKey splits the VirtualIPKey from the service tracker in virtual ip and protocol
func splitVirtualIPKey(key string) (string, v1.Protocol) {
	parts := strings.Split(key, "/")
	return parts[0], v1.Protocol(parts[1])
}

// serviceTracker tracks the services VIPs using the service name and namespace as key
// one service can have multiple VIPs, they are stored in the format IP:Port/Protocol
// The services allows to map Kubernetes Services and OVN LoadBalancer
type serviceTracker struct {
	sync.Mutex
	virtualIPByService map[string]sets.String
	hadEndpoints       map[string]bool
}

// newServiceTracker creates and initializes a new serviceTracker.
func newServiceTracker() *serviceTracker {
	return &serviceTracker{
		virtualIPByService: map[string]sets.String{},
		hadEndpoints:       map[string]bool{},
	}
}

// updateService adds or updates the virtualIPs and endpoints of the Service
func (st *serviceTracker) updateService(name, namespace, virtualIP string, proto v1.Protocol) {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	key := virtualIPKey(virtualIP, proto)

	// check if the service already exists and create a new entry if it does not
	vips, ok := st.virtualIPByService[serviceNN]
	if !ok || vips == nil {
		klog.V(5).Infof("Created service %s VIP %s %s on Service Tracker", serviceNN, virtualIP, proto)
		st.virtualIPByService[serviceNN] = sets.NewString(key)
		return
	}
	// Update the service VIP with the new endpoints
	vips.Insert(key)
	klog.V(5).Infof("Updated service %s VIP %s %s on Service Tracker", serviceNN, virtualIP, proto)
}

// deleteService removes the set of virtual IPs tracked for the Service.
func (st *serviceTracker) deleteService(name, namespace string) {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	delete(st.virtualIPByService, serviceNN)
	delete(st.hadEndpoints, serviceNN)
	klog.V(5).Infof("Deleted service %s from Service Tracker", serviceNN)
}

// deleteServiceVIP removes the virtual IP tracked for the Service.
func (st *serviceTracker) deleteServiceVIP(name, namespace, virtualIP string, proto v1.Protocol) {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	key := virtualIPKey(virtualIP, proto)
	vips, ok := st.virtualIPByService[serviceNN]
	if ok {
		vips.Delete(key)
		klog.V(5).Infof("Deleted service %s VIP %s %s from Service Tracker", serviceNN, virtualIP, proto)
	}
}

// hasService return true if the service is being tracked
func (st *serviceTracker) hasService(name, namespace string) bool {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	_, ok := st.virtualIPByService[serviceNN]
	return ok
}

// setHasEndpoints indicates that the service has had endpoints before
func (st *serviceTracker) setHasEndpoints(name, namespace string) {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	st.hadEndpoints[serviceNN] = true
}

// everHadEndpoints return true if the service has ever had any endpoints
func (st *serviceTracker) everHadEndpoints(name, namespace string) bool {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	_, ok := st.hadEndpoints[serviceNN]
	return ok
}

// hasServiceVIP return true if the VIP is being tracked for that service
func (st *serviceTracker) hasServiceVIP(name, namespace, virtualIP string, proto v1.Protocol) bool {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	key := virtualIPKey(virtualIP, proto)

	// check if the service already exists
	vips, ok := st.virtualIPByService[serviceNN]
	if !ok {
		return false
	}
	return vips.Has(key)
}

// getService return the service VIPs associated to the service
func (st *serviceTracker) getService(name, namespace string) sets.String {
	st.Lock()
	defer st.Unlock()

	serviceNN := serviceTrackerKey(name, namespace)
	if vips, ok := st.virtualIPByService[serviceNN]; ok {
		klog.V(5).Infof("Obtained service %s on Service Tracker: %v", serviceNN, vips)
		return vips
	}
	return sets.NewString()
}

// updateKubernetesService adds or updates the tracker from a Kubernetes service
// added for testing purposes
func (st *serviceTracker) updateKubernetesService(service *v1.Service) {
	for _, ip := range util.GetClusterIPs(service) {
		for _, svcPort := range service.Spec.Ports {
			vip := util.JoinHostPortInt32(ip, svcPort.Port)
			st.updateService(service.Name, service.Namespace, vip, svcPort.Protocol)
		}
	}
}
