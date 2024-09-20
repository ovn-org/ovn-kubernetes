package services

import (
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// repair handles pre-sync and post-sync service cleanup.
// It has two phases:
//
// Pre-Sync: Delete any ovn Load_Balancer rows that are "owned" by a Kubernetes Service
//
//	that doesn't exist anymore
//
// Post-sync: After every service has been synced at least once, delete any legacy load-balancers
//
// We need to execute in two phases so that we don't disrupt any vips being handled by
// legacy load balancers before we've had a change to migrate those vips.
type repair struct {
	sync.Mutex
	serviceLister corelisters.ServiceLister

	// We want to run some functions after every service is successfully synced, so populate this
	// list with every service that should be in the informer queue before we start the ServiceController
	// workers.
	unsyncedServices sets.Set[string]

	nbClient libovsdbclient.Client
}

// NewRepair creates a controller that periodically ensures that there is no stale data in OVN
func newRepair(serviceLister corelisters.ServiceLister, nbClient libovsdbclient.Client) *repair {
	return &repair{
		serviceLister:    serviceLister,
		unsyncedServices: sets.Set[string]{},
		nbClient:         nbClient,
	}
}

// runBeforeSync performs some cleanup of stale LBs and other miscellaneous setup.
func (r *repair) runBeforeSync(useTemplates bool, netInfo util.NetInfo, nodes map[string]nodeInfo) {
	// no need to lock, single-threaded.

	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for services")
	defer func() {
		klog.V(4).Infof("Finished repairing loop for services: %v", time.Since(startTime))
	}()

	// Build a list of every service existing
	// After every service has been synced, then we'll execute runAfterSync
	services, _ := r.serviceLister.List(labels.Everything())
	for _, service := range services {
		key, _ := cache.MetaNamespaceKeyFunc(service)
		r.unsyncedServices.Insert(key)
	}

	// Find all templates.
	allTemplates := TemplateMap{}

	if useTemplates {
		var err error

		allTemplates, err = listSvcTemplates(r.nbClient)
		if err != nil {
			klog.Errorf("Unable to get templates for repair: %v", err)
		}
	}

	// Find all load-balancers associated with Services
	existingLBs, err := getAllLBs(r.nbClient, allTemplates)
	if err != nil {
		klog.Errorf("Unable to get service lbs for repair: %v", err)
	}

	// Look for any load balancers whose Service no longer exists in the apiserver
	// and for any chassis template vars whose Service no longer exists.
	staleTemplateNames := sets.Set[string]{}
	for templateName := range allTemplates {
		// NodeIP templates are always valid, skip those.
		if isLBNodeIPTemplateName(templateName) {
			continue
		}

		// All others are potentially stale.
		staleTemplateNames.Insert(templateName)
	}
	staleLBs := []string{}
	for _, lb := range existingLBs {
		// Extract namespace + name, look to see if it exists
		owner := lb.ExternalIDs[types.LoadBalancerOwnerExternalID]
		namespace, name, err := cache.SplitMetaNamespaceKey(owner)
		if err != nil || namespace == "" {
			klog.Warningf("Service LB %#v has unreadable owner, deleting", lb)
			staleLBs = append(staleLBs, lb.UUID)
			continue
		}

		_, err = r.serviceLister.Services(namespace).Get(name)
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("Found stale service LB %#v", lb)
			staleLBs = append(staleLBs, lb.UUID)
			continue
		}

		// All of the LB's template vars are still useful.
		for _, t := range lb.Templates {
			staleTemplateNames.Delete(t.Name)
		}
	}

	// Delete those stale load balancers
	if err := DeleteLBs(r.nbClient, staleLBs); err != nil {
		klog.Errorf("Failed to delete stale LBs: %v", err)
	}
	klog.V(2).Infof("Deleted %d stale service LBs", len(staleLBs))

	// Delete those stale template vars
	if err := libovsdbops.DeleteAllChassisTemplateVarVariables(r.nbClient, staleTemplateNames.UnsortedList()); err != nil {
		klog.Errorf("Failed to delete stale Chassis Template Vars: %v", err)
	}
	klog.V(2).Infof("Deleted %d stale Chassis Template Vars", len(staleTemplateNames))

	// Remove existing reject rules. They are not used anymore
	// given the introduction of idling loadbalancers
	p := func(item *nbdb.ACL) bool {
		return item.Action == nbdb.ACLActionReject
	}
	acls, err := libovsdbops.FindACLsWithPredicate(r.nbClient, p)
	if err != nil {
		klog.Errorf("Error while finding reject ACLs error: %v", err)
	}

	if len(acls) > 0 {
		p := func(item *nbdb.LogicalSwitch) bool { return true }
		err = libovsdbops.RemoveACLsFromLogicalSwitchesWithPredicate(r.nbClient, p, acls...)
		if err != nil {
			klog.Errorf("Failed to purge existing reject rules: %v", err)
		}
	}

	// remove static routes for UDN enabled services that are no longer valid
	udnDelPredicate := func(route *nbdb.LogicalRouterStaticRoute) bool {
		if route.ExternalIDs[types.NetworkExternalID] == netInfo.GetNetworkName() &&
			route.ExternalIDs[types.TopologyExternalID] == netInfo.TopologyType() {
			if serviceKey, exists := route.ExternalIDs[types.UDNEnabledServiceExternalID]; exists {
				if !r.unsyncedServices.Has(serviceKey) {
					// the service doesn't exist
					return true
				}
				if !util.IsUDNEnabledService(serviceKey) {
					// the service is not a part of UDNAllowedDefaultServices anymore
					return true
				}
			}
		}
		return false
	}

	if netInfo.IsPrimaryNetwork() {
		var ops []libovsdb.Operation
		if netInfo.TopologyType() == types.Layer2Topology {
			for _, node := range nodes {
				if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(r.nbClient, ops, netInfo.GetNetworkScopedGWRouterName(node.name), udnDelPredicate); err != nil {
					klog.Errorf("Failed to create a delete logical router static route op: %v", err)
				}
			}
		} else {
			if ops, err = libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicateOps(r.nbClient, ops, netInfo.GetNetworkScopedClusterRouterName(), udnDelPredicate); err != nil {
				klog.Errorf("Failed to create a delete logical router static route op: %v", err)
			}
		}
		if _, err = libovsdbops.TransactAndCheck(r.nbClient, ops); err != nil {
			klog.Errorf("Failed to delete logical router static routes: %v", err)
		}
	}
}

// serviceSynced is called by a ServiceController worker when it has successfully
// applied a service.
// If all services have successfully synced at least once, kick off
// runAfterSync()
func (r *repair) serviceSynced(key string) {
	r.Lock()
	defer r.Unlock()
	if len(r.unsyncedServices) == 0 {
		return
	}
	delete(r.unsyncedServices, key)
}
