package ovn

import (
	"fmt"
	knet "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"math/rand"
	"time"

	"k8s.io/klog/v2"
)

// iterateRetryNetworkPolicies checks if any outstanding NetworkPolicies exist
// then tries to re-add them if so
// updateAll forces all policies to be attempted to be retried regardless
func (oc *Controller) iterateRetryNetworkPolicies(updateAll bool) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	now := time.Now()
	for namespacedName, npEntry := range oc.retryNetPolices {
		if npEntry.ignore {
			continue
		}

		// check if we need to create
		var policyToCreate *knet.NetworkPolicy
		if npEntry.newPolicy != nil {
			// get the latest version of the new policy from the informer, if it doesn't exist we are not going to
			// create the new policy
			np, err := oc.watchFactory.GetNetworkPolicy(npEntry.newPolicy.Namespace, npEntry.newPolicy.Name)
			if err != nil && errors.IsNotFound(err) {
				klog.Infof("%s policy not found in the informers cache, not going to retry policy create", namespacedName)
				npEntry.newPolicy = nil
			} else {
				policyToCreate = np
			}
		}

		npEntry.backoffSec = npEntry.backoffSec * 2
		if npEntry.backoffSec > 60 {
			npEntry.backoffSec = 60
		}
		backoff := (npEntry.backoffSec * time.Second) + (time.Duration(rand.Intn(500)) * time.Millisecond)
		npTimer := npEntry.timeStamp.Add(backoff)
		if updateAll || now.After(npTimer) {
			klog.Infof("Network Policy Retry: %s retry network policy setup", namespacedName)

			// check if we need to delete anything
			if npEntry.oldPolicy != nil {
				klog.Infof("Network Policy Retry: Removing old policy for %s", namespacedName)
				if err := oc.deleteNetworkPolicy(npEntry.oldPolicy, npEntry.np); err != nil {
					klog.Infof("Network Policy Retry delete failed for %s, will try again later: %v",
						namespacedName, err)
					npEntry.timeStamp = time.Now()
					continue
				}
				// successfully cleaned up old policy, remove it from the retry cache
				npEntry.oldPolicy = nil
			}

			// create new policy if needed
			if policyToCreate != nil {
				klog.Infof("Network Policy Retry: Creating new policy for %s", namespacedName)
				if err := oc.addNetworkPolicy(policyToCreate); err != nil {
					klog.Infof("Network Policy Retry create failed for %s, will try again later: %v",
						namespacedName, err)
					npEntry.timeStamp = time.Now()
					continue
				}
				// successfully cleaned up old policy, remove it from the retry cache
				npEntry.newPolicy = nil
			}

			klog.Infof("Network Policy Retry successful for %s", namespacedName)
			delete(oc.retryNetPolices, namespacedName)
		} else {
			klog.V(5).Infof("%s retry network policy not after timer yet, time: %s", namespacedName, npTimer)
		}
	}
}

// initRetryPolicy tracks a failed policy to potentially retry later
// initially it is marked as skipped for retry loop (ignore = true)
func (oc *Controller) initRetryPolicy(policy *knet.NetworkPolicy) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		entry.timeStamp = time.Now()
		entry.newPolicy = policy
	} else {
		oc.retryNetPolices[key] = &retryNetPolEntry{policy, nil, nil, time.Now(), 1, true}
	}
}

// initRetryPolicyWithDelete tracks a failed policy to potentially retry later
// initially it is marked as skipped for retry loop (ignore = true)
// includes the np object as well in case the namespace is removed and the policy is orphaned from
// the namespace
func (oc *Controller) initRetryPolicyWithDelete(policy *knet.NetworkPolicy, np *networkPolicy) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		entry.timeStamp = time.Now()
		entry.oldPolicy = policy
		if entry.np == nil {
			entry.np = np
		}
	} else {
		oc.retryNetPolices[key] = &retryNetPolEntry{nil, policy, np, time.Now(), 1, true}
	}
}

// addDeleteToRetryPolicy adds an old policy that needs to be cleaned up to a retry object
// includes the np object as well in case the namespace is removed and the policy is orphaned from
// the namespace
func (oc *Controller) addDeleteToRetryPolicy(policy *knet.NetworkPolicy, np *networkPolicy) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		entry.oldPolicy = policy
		entry.np = np
	}
}

// removeDeleteFromRetryPolicy removes any old policy from a retry entry
func (oc *Controller) removeDeleteFromRetryPolicy(policy *knet.NetworkPolicy) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		entry.oldPolicy = nil
		entry.np = nil
	}
}

// unSkipRetryPolicy ensures a policy is no longer ignored for retry loop
func (oc *Controller) unSkipRetryPolicy(policy *knet.NetworkPolicy) {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	if entry, ok := oc.retryNetPolices[getPolicyNamespacedName(policy)]; ok {
		entry.ignore = false
	}
}

// checkAndDeleteRetryPolicy deletes a specific entry from the map, if it existed, returns true
func (oc *Controller) checkAndDeleteRetryPolicy(policy *knet.NetworkPolicy) bool {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if _, ok := oc.retryNetPolices[key]; ok {
		delete(oc.retryNetPolices, key)
		return true
	}
	return false
}

// checkAndSkipRetryPolicy sets a specific entry from the map to be ignored for subsequent retries
// if it existed, returns true
func (oc *Controller) checkAndSkipRetryPolicy(policy *knet.NetworkPolicy) bool {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		entry.ignore = true
		return true
	}
	return false
}

// RequestRetryPolicy allows a caller to immediately request to iterate through all policies that
// are in the retry cache. This will ignore any outstanding time wait/backoff state
// Used for Unit Tests only
func (oc *Controller) RequestRetryPolicy() {
	select {
	case oc.retryPolicyChan <- struct{}{}:
		klog.V(5).Infof("Iterate retry policy requested")
	default:
		klog.V(5).Infof("Iterate retry policy already requested")
	}
}

// Used for Unit Tests only
func (oc *Controller) getRetryEntry(policy *knet.NetworkPolicy) *retryNetPolEntry {
	oc.retryNetPolLock.Lock()
	defer oc.retryNetPolLock.Unlock()
	key := getPolicyNamespacedName(policy)
	if entry, ok := oc.retryNetPolices[key]; ok {
		x := *entry
		return &x
	}
	return nil
}

func getPolicyNamespacedName(policy *knet.NetworkPolicy) string {
	return fmt.Sprintf("%v/%v", policy.Namespace, policy.Name)
}
