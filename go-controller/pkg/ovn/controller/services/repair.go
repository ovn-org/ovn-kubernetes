package services

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/acl"
	ovnlb "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/loadbalancer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// repair handles pre-sync and post-sync service cleanup.
// It checks the OVN Load Balancers and delete the ones that doesn't exist in
// the Kubernetes Service Informer cache.
// It also deletes the legacy load balancers once a full sync run has completed.
// Based on:
// https://raw.githubusercontent.com/kubernetes/kubernetes/release-1.19/pkg/registry/core/service/ipallocator/controller/repair.go
type repair struct {
	sync.Mutex
	serviceLister corelisters.ServiceLister

	// We want to run some functions after every service is successfully synced, so tpoplate this
	// list with every service that should be in the informer queue before we start the ServiceController
	// workers.
	unsyncedServices sets.String

	// Really a boolean, but an int32 for atomicitiy purposes
	semLegacyLBsDeleted uint32
}

// NewRepair creates a controller that periodically ensures that there is no stale data in OVN
func newRepair(serviceLister corelisters.ServiceLister) *repair {
	return &repair{
		serviceLister:    serviceLister,
		unsyncedServices: sets.String{},
	}
}

// runBeforeSync performs some cleanup of stale LBs and other miscellaneous setup.
func (r *repair) runBeforeSync(clusterPortGroupUUID string) error {
	// no need to lock, single-threaded.

	startTime := time.Now()
	klog.V(4).Infof("Starting repairing loop for services")
	defer func() {
		klog.V(4).Infof("Finished repairing loop for services: %v", time.Since(startTime))
	}()

	// Ensure unidling is enabled
	if globalconfig.Kubernetes.OVNEmptyLbEvents {
		_, _, err := util.RunOVNNbctl("set", "nb_global", ".", "options:controller_event=true")
		if err != nil {
			klog.Error("Unable to enable controller events. Unidling not possible")
			return err
		}
	}

	// Build a list of every service existing
	// After every service has been synced, then we'll execute runAfterSync
	services, _ := r.serviceLister.List(labels.Everything())
	for _, service := range services {
		key, _ := cache.MetaNamespaceKeyFunc(service)
		r.unsyncedServices.Insert(key)
	}

	// Find all load-balancers associated with Services
	existingLBs, err := ovnlb.FindLBs(map[string]string{"k8s.ovn.org/kind": "Service"})
	if err != nil {
		klog.Errorf("Failed to list existing load balancers: %v", err)
		return err
	}

	// Look for any load balancers whose Service no longer exists in the apiserver
	staleLBs := []string{}
	for _, lb := range existingLBs {
		// Extract namespace + name, look to see if it exists
		owner := lb.ExternalIDs["k8s.ovn.org/owner"]
		namespace, name, err := cache.SplitMetaNamespaceKey(owner)
		if err != nil || namespace == "" {
			klog.Warningf("Service LB %#v has unreadable owner, deleting", lb)
			staleLBs = append(staleLBs, lb.UUID)
		}

		_, err = r.serviceLister.Services(namespace).Get(name)
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("Found stale service LB %#v", lb)
			staleLBs = append(staleLBs, lb.UUID)
		}
	}

	// Delete those stale load balancers
	if err := ovnlb.DeleteLBs(nil, staleLBs); err != nil {
		klog.Errorf("Failed to delete stale LBs: %v", err)
		return fmt.Errorf("failed to delete stale LBs: %w", err)
	}
	klog.V(2).Infof("Deleted %d stale service LBs", len(staleLBs))

	// Remove existing reject rules. They are not used anymore
	// given the introduction of idling loadbalancers
	err = acl.PurgeRejectRules(clusterPortGroupUUID)
	if err != nil {
		klog.Errorf("Failed to purge existing reject rules: %v", err)
		return err
	}
	return nil
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
	if len(r.unsyncedServices) == 0 {
		go r.runAfterSync() // run in a goroutine so we don't block the ServiceController
	}
}

// runAfterSync is called sometime after every existing service is successfully synced at least once
// It deletes all legacy load balancers.
func (r *repair) runAfterSync() {
	_ = utilwait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
		klog.Infof("Running Service post-sync cleanup")
		err := r.deleteLegacyLBs()
		if err != nil {
			klog.Warningf("Failed to delete legacy LBs: %v", err)
			return false, nil
		}
		return true, nil
	})
}

func (r *repair) deleteLegacyLBs() error {
	// Find all load-balancers associated with Services
	legacyLBs, err := findLegacyLBs()
	if err != nil {
		klog.Errorf("Failed to list existing load balancers: %v", err)
		return err
	}

	klog.V(2).Infof("Deleting %d legacy LBs", len(legacyLBs))
	toDelete := make([]string, 0, len(legacyLBs))
	for _, lb := range legacyLBs {
		toDelete = append(toDelete, lb.UUID)
	}
	if err := ovnlb.DeleteLBs(nil, toDelete); err != nil {
		return fmt.Errorf("failed to delete LBs: %w", err)
	}
	atomic.StoreUint32(&r.semLegacyLBsDeleted, 1)
	return nil
}

// legacyLBsDeleted returns true if we've run the post-sync repair
// and there are no more legacy LBs, so we can stop searching
// for them in the services handler.
func (r *repair) legacyLBsDeleted() bool {
	return atomic.LoadUint32(&r.semLegacyLBsDeleted) > 0
}
