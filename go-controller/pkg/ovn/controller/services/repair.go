package services

import (
	"fmt"
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

// Repair is a controller loop that can work as in one-shot or periodic mode
// it checks the OVN Load Balancers and delete the ones that doesn't exist in
// the Kubernetes Service Informer cache.
// Based on:
// https://raw.githubusercontent.com/kubernetes/kubernetes/release-1.19/pkg/registry/core/service/ipallocator/controller/repair.go
type Repair struct {
	interval      time.Duration
	serviceLister corelisters.ServiceLister

	// We want to run some functions after every service is successfully synced
	unsyncedServices sets.String

	syncedServices chan string

	semLegacyLBsDeleted int32 // for atomic writes
}

// NewRepair creates a controller that periodically ensures that there is no stale data in OVN
func newRepair(interval time.Duration, serviceLister corelisters.ServiceLister) *Repair {
	return &Repair{
		interval:         interval,
		serviceLister:    serviceLister,
		unsyncedServices: sets.String{},
		syncedServices:   make(chan string, 5),
	}
}

// runOnce verifies the state of the cluster OVN LB VIP allocations and returns an error if an unrecoverable problem occurs.
func (r *Repair) runBeforeSync(clusterPortGroupUUID string) error {
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

	if err := ovnlb.DeleteLBs(staleLBs); err != nil {
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

func (r *Repair) legacyLBsDeleted() bool {
	return atomic.LoadInt32(&r.semLegacyLBsDeleted) > 0
}

func (r *Repair) serviceSynced(key string) {
	if r.legacyLBsDeleted() {
		return
	}
	r.syncedServices <- key
}

// runAterSync waits until all services have been synced, then
// executes the afterSync function. It should be run in a goroutine.
func (r *Repair) runAfterSync() {
	if len(r.unsyncedServices) == 0 {
		go r.afterSync()
	}

	for {
		key := <-r.syncedServices

		// If we reach 0 unynced services...
		if len(r.unsyncedServices) != 0 {
			r.unsyncedServices.Delete(key)
			if len(r.unsyncedServices) == 0 {
				go r.afterSync() // so we don't block
			}
		}
	}
}

// afterSync is called sometime after every existing service is successfully synced at least once
// It deletes all legacy load balancers.
func (r *Repair) afterSync() {
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

func (r *Repair) deleteLegacyLBs() error {
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
	if err := ovnlb.DeleteLBs(toDelete); err != nil {
		return fmt.Errorf("failed to delete LBs: %w", err)
	}
	atomic.StoreInt32(&r.semLegacyLBsDeleted, 1)
	return nil
}
