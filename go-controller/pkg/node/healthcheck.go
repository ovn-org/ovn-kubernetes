package node

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// initLoadBalancerHealthChecker initializes the health check server for
// ServiceTypeLoadBalancer services

type loadBalancerHealthChecker struct {
	sync.Mutex
	nodeName     string
	server       healthcheck.Server
	services     map[ktypes.NamespacedName]uint16
	endpoints    map[ktypes.NamespacedName]int
	watchFactory factory.NodeWatchFactory
}

func newLoadBalancerHealthChecker(nodeName string, watchFactory factory.NodeWatchFactory) *loadBalancerHealthChecker {
	return &loadBalancerHealthChecker{
		nodeName:     nodeName,
		server:       healthcheck.NewServer(nodeName, nil, nil, nil),
		services:     make(map[ktypes.NamespacedName]uint16),
		endpoints:    make(map[ktypes.NamespacedName]int),
		watchFactory: watchFactory,
	}
}

func (l *loadBalancerHealthChecker) AddService(svc *kapi.Service) error {
	if svc.Spec.HealthCheckNodePort != 0 {
		l.Lock()
		defer l.Unlock()
		name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		l.services[name] = uint16(svc.Spec.HealthCheckNodePort)
		return l.server.SyncServices(l.services)
	}
	return nil
}

func (l *loadBalancerHealthChecker) UpdateService(old, new *kapi.Service) error {
	// if the ETP values have changed between local and cluster,
	// we need to update health checks accordingly
	// HealthCheckNodePort is used only in ETP=local mode
	var err error
	var errors []error
	if old.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeCluster &&
		new.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeLocal {
		if err = l.AddService(new); err != nil {
			errors = append(errors, err)
		}
		epSlices, err := l.watchFactory.GetEndpointSlices(new.Namespace, new.Name)
		if err != nil {
			errors = append(errors, fmt.Errorf("could not fetch endpointslices "+
				"for service %s/%s during health check update service: %v",
				new.Namespace, new.Name, err))
			return apierrors.NewAggregate(errors)
		}
		namespacedName := ktypes.NamespacedName{Namespace: new.Namespace, Name: new.Name}
		l.Lock()
		l.endpoints[namespacedName] = l.CountLocalEndpointAddresses(epSlices)
		if err = l.server.SyncEndpoints(l.endpoints); err != nil {
			errors = append(errors, err)
		}
		l.Unlock()
	}
	if old.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeLocal &&
		new.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeCluster {
		if err = l.DeleteService(old); err != nil {
			errors = append(errors, err)
		}
	}
	return apierrors.NewAggregate(errors)
}

func (l *loadBalancerHealthChecker) DeleteService(svc *kapi.Service) error {
	if svc.Spec.HealthCheckNodePort != 0 {
		l.Lock()
		defer l.Unlock()
		name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
		delete(l.services, name)
		delete(l.endpoints, name)
		return l.server.SyncServices(l.services)
	}
	return nil
}

func (l *loadBalancerHealthChecker) SyncServices(svcs []interface{}) error {
	return nil
}

func (l *loadBalancerHealthChecker) SyncEndPointSlices(epSlice *discovery.EndpointSlice) error {
	namespacedName, err := namespacedNameFromEPSlice(epSlice)
	if err != nil {
		return fmt.Errorf("skipping %s/%s: %v", epSlice.Namespace, epSlice.Name, err)
	}
	epSlices, err := l.watchFactory.GetEndpointSlices(epSlice.Namespace, epSlice.Labels[discovery.LabelServiceName])
	if err != nil {
		// should be a rare occurence
		return fmt.Errorf("could not fetch all endpointslices for service %s during health check", namespacedName.String())
	}

	localEndpointAddressCount := l.CountLocalEndpointAddresses(epSlices)
	if len(epSlices) == 0 {
		// let's delete it from cache and wait for the next update;
		// this will show as 0 endpoints for health checks
		delete(l.endpoints, namespacedName)
	} else {
		l.endpoints[namespacedName] = localEndpointAddressCount
	}
	return l.server.SyncEndpoints(l.endpoints)
}

func (l *loadBalancerHealthChecker) AddEndpointSlice(epSlice *discovery.EndpointSlice) error {
	namespacedName, err := namespacedNameFromEPSlice(epSlice)
	if err != nil {
		return fmt.Errorf("cannot add %s/%s to loadBalancerHealthChecker: %v", epSlice.Namespace, epSlice.Name, err)
	}
	l.Lock()
	defer l.Unlock()
	if _, exists := l.services[namespacedName]; exists {
		return l.SyncEndPointSlices(epSlice)
	}
	return nil
}

func (l *loadBalancerHealthChecker) UpdateEndpointSlice(oldEpSlice, newEpSlice *discovery.EndpointSlice) error {
	namespacedName, err := namespacedNameFromEPSlice(newEpSlice)
	if err != nil {
		return fmt.Errorf("cannot update %s/%s in loadBalancerHealthChecker: %v",
			newEpSlice.Namespace, newEpSlice.Name, err)
	}
	l.Lock()
	defer l.Unlock()
	if _, exists := l.services[namespacedName]; exists {
		return l.SyncEndPointSlices(newEpSlice)
	}
	return nil
}

func (l *loadBalancerHealthChecker) DeleteEndpointSlice(epSlice *discovery.EndpointSlice) error {
	l.Lock()
	defer l.Unlock()
	return l.SyncEndPointSlices(epSlice)
}

// CountLocalEndpointAddresses returns the number of IP addresses from ready endpoints that are local
// to the node for a service
func (l *loadBalancerHealthChecker) CountLocalEndpointAddresses(endpointSlices []*discovery.EndpointSlice) int {
	localEndpointAddresses := sets.NewString()
	for _, endpointSlice := range endpointSlices {
		for _, endpoint := range endpointSlice.Endpoints {
			isLocal := endpoint.NodeName != nil && *endpoint.NodeName == l.nodeName
			if isEndpointReady(endpoint) && isLocal {
				localEndpointAddresses.Insert(endpoint.Addresses...)
			}
		}
	}
	return len(localEndpointAddresses)
}

// hasLocalHostNetworkEndpoints returns true if there is at least one host-networked endpoint
// in the provided list that is local to this node.
// It returns false if none of the endpoints are local host-networked endpoints or if ep.Subsets is nil.
func hasLocalHostNetworkEndpoints(epSlices []*discovery.EndpointSlice, nodeAddresses []net.IP) bool {
	for _, epSlice := range epSlices {
		for _, endpoint := range epSlice.Endpoints {
			if !isEndpointReady(endpoint) {
				continue
			}
			for _, ip := range endpoint.Addresses {
				for _, nodeIP := range nodeAddresses {
					if nodeIP.String() == utilnet.ParseIPSloppy(ip).String() {
						return true
					}
				}
			}
		}
	}
	return false
}

// isHostEndpoint determines if the given endpoint ip belongs to a host networked pod
func isHostEndpoint(endpointIP string) bool {
	for _, clusterNet := range config.Default.ClusterSubnets {
		if clusterNet.CIDR.Contains(net.ParseIP(endpointIP)) {
			return false
		}
	}
	return true
}

// discovery.EndpointSlice reports in the same slice all endpoints along with their status.
// isEndpointReady takes an endpoint from an endpoint slice and returns true if the endpoint is
// to be considered ready.
func isEndpointReady(endpoint discovery.Endpoint) bool {
	return endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
}

type openflowManager struct {
	defaultBridge         *bridgeConfiguration
	externalGatewayBridge *bridgeConfiguration
	// flow cache, use map instead of array for readability when debugging
	flowCache     map[string][]string
	flowMutex     sync.Mutex
	exGWFlowCache map[string][]string
	exGWFlowMutex sync.Mutex
	// channel to indicate we need to update flows immediately
	flowChan chan struct{}
}

func (c *openflowManager) updateFlowCacheEntry(key string, flows []string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	c.flowCache[key] = flows
}

func (c *openflowManager) deleteFlowsByKey(key string) {
	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()
	delete(c.flowCache, key)
}

func (c *openflowManager) updateExBridgeFlowCacheEntry(key string, flows []string) {
	c.exGWFlowMutex.Lock()
	defer c.exGWFlowMutex.Unlock()
	c.exGWFlowCache[key] = flows
}

func (c *openflowManager) requestFlowSync() {
	select {
	case c.flowChan <- struct{}{}:
		klog.V(5).Infof("Gateway OpenFlow sync requested")
	default:
		klog.V(5).Infof("Gateway OpenFlow sync already requested")
	}
}

func (c *openflowManager) syncFlows() {
	// protect gwBridge config from being updated by gw.nodeIPManager
	c.defaultBridge.Lock()
	defer c.defaultBridge.Unlock()

	c.flowMutex.Lock()
	defer c.flowMutex.Unlock()

	flows := []string{}
	for _, entry := range c.flowCache {
		flows = append(flows, entry...)
	}

	_, stderr, err := util.ReplaceOFFlows(c.defaultBridge.bridgeName, flows)
	if err != nil {
		klog.Errorf("Failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, c.flowCache)
	}

	if c.externalGatewayBridge != nil {
		c.exGWFlowMutex.Lock()
		defer c.exGWFlowMutex.Unlock()

		flows := []string{}
		for _, entry := range c.exGWFlowCache {
			flows = append(flows, entry...)
		}

		_, stderr, err := util.ReplaceOFFlows(c.externalGatewayBridge.bridgeName, flows)
		if err != nil {
			klog.Errorf("Failed to add flows, error: %v, stderr, %s, flows: %s", err, stderr, c.exGWFlowCache)
		}
	}
}

// checkDefaultOpenFlow checks for the existence of default OpenFlow rules and
// exits if the output is not as expected
func (c *openflowManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	doneWg.Add(1)
	go func() {
		defer doneWg.Done()
		syncPeriod := 15 * time.Second
		timer := time.NewTicker(syncPeriod)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				if err := checkPorts(c.defaultBridge.patchPort, c.defaultBridge.ofPortPatch,
					c.defaultBridge.uplinkName, c.defaultBridge.ofPortPhys); err != nil {
					klog.Errorf("Checkports failed %v", err)
					continue
				}
				if c.externalGatewayBridge != nil {
					if err := checkPorts(
						c.externalGatewayBridge.patchPort, c.externalGatewayBridge.ofPortPatch,
						c.externalGatewayBridge.uplinkName, c.externalGatewayBridge.ofPortPhys); err != nil {
						klog.Errorf("Checkports failed %v", err)
						continue
					}
				}
				c.syncFlows()
			case <-c.flowChan:
				c.syncFlows()
				timer.Reset(syncPeriod)
			case <-stopChan:
				return
			}
		}
	}()
}

func checkPorts(patchIntf, ofPortPatch, physIntf, ofPortPhys string) error {
	// it could be that the ovn-controller recreated the patch between the host OVS bridge and
	// the integration bridge, as a result the ofport number changed for that patch interface
	curOfportPatch, stderr, err := util.GetOVSOfPort("--if-exists", "get", "Interface", patchIntf, "ofport")
	if err != nil {
		return errors.Wrapf(err, "Failed to get ofport of %s, stderr: %q", patchIntf, stderr)

	}
	if ofPortPatch != curOfportPatch {
		klog.Errorf("Fatal error: patch port %s ofport changed from %s to %s",
			patchIntf, ofPortPatch, curOfportPatch)
		os.Exit(1)
	}

	// it could be that someone removed the physical interface and added it back on the OVS host
	// bridge, as a result the ofport number changed for that physical interface
	curOfportPhys, stderr, err := util.GetOVSOfPort("--if-exists", "get", "interface", physIntf, "ofport")
	if err != nil {
		return errors.Wrapf(err, "Failed to get ofport of %s, stderr: %q", physIntf, stderr)
	}
	if ofPortPhys != curOfportPhys {
		klog.Errorf("Fatal error: phys port %s ofport changed from %s to %s",
			physIntf, ofPortPhys, curOfportPhys)
		os.Exit(1)
	}
	return nil
}

func namespacedNameFromEPSlice(epSlice *discovery.EndpointSlice) (ktypes.NamespacedName, error) {
	// Return the namespaced name of the corresponding service
	var serviceNamespacedName ktypes.NamespacedName
	svcName := epSlice.Labels[discovery.LabelServiceName]
	if svcName == "" {
		// should not happen, since the informer already filters out endpoint slices with an empty service label
		return serviceNamespacedName,
			fmt.Errorf("endpointslice %s/%s: empty value for label %s",
				epSlice.Namespace, epSlice.Name, discovery.LabelServiceName)
	}
	return ktypes.NamespacedName{Namespace: epSlice.Namespace, Name: svcName}, nil
}
