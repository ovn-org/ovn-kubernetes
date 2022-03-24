package ovn

import (
	"fmt"
	"math/rand"
	"net"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

// iterateRetryPods checks if any outstanding pods have been waiting for 60 seconds of last known failure
// then tries to re-add them if so
// updateAll forces all pods to be attempted to be retried regardless of the 1 minute delay
func (oc *Controller) iterateRetryPods(updateAll bool) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	now := time.Now()
	for key, podEntry := range oc.retryPods {
		if podEntry.ignore {
			// neither addition nor deletion is being retried
			continue
		}

		pod := podEntry.pod
		podDesc := fmt.Sprintf("[%s/%s/%s]", pod.UID, pod.Namespace, pod.Name)
		// it could be that the Pod got deleted, but Pod's DeleteFunc has not been called yet, so don't retry creation
		if podEntry.needsAdd {
			kPod, err := oc.watchFactory.GetPod(pod.Namespace, pod.Name)
			if err != nil && errors.IsNotFound(err) {
				klog.Infof("%s pod not found in the informers cache, not going to retry pod setup", podDesc)
				podEntry.needsAdd = false
			} else {
				pod = kPod
			}
		}

		if !util.PodScheduled(pod) {
			klog.V(5).Infof("Retry: %s not scheduled", podDesc)
			continue
		}

		podEntry.backoffSec = (podEntry.backoffSec * 2)
		if podEntry.backoffSec > 60 {
			podEntry.backoffSec = 60
		}
		backoff := (podEntry.backoffSec * time.Second) + (time.Duration(rand.Intn(500)) * time.Millisecond)
		podTimer := podEntry.timeStamp.Add(backoff)
		if updateAll || now.After(podTimer) {
			// check if we need to retry delete first
			if podEntry.needsDel != nil {
				klog.Infof("%s retry pod teardown", podDesc)
				if err := oc.removePod(pod, podEntry.needsDel); err != nil {
					klog.Infof("%s teardown retry failed; will try again later", podDesc)
					podEntry.timeStamp = time.Now()
					continue // if deletion failed we will not retry add
				}
				klog.Infof("%s pod teardown successful", podDesc)
				podEntry.needsDel = nil
				if !podEntry.needsAdd {
					delete(oc.retryPods, key) // this means retryDelete worked, we can remove entry safely
				}
			}
			// check if we need to retry add
			if podEntry.needsAdd {
				klog.Infof("%s retry pod setup", podDesc)
				if err := oc.ensurePod(nil, pod, true); err != nil {
					klog.Infof("%s setup retry failed; will try again later", podDesc)
					podEntry.timeStamp = time.Now()
				} else {
					klog.Infof("%s pod setup successful", podDesc)
					delete(oc.retryPods, key) // this means retryDelete and retryAdd both worked, we can remove entry safely
				}
			}
		} else {
			klog.V(5).Infof("%s retry pod not after timer yet, time: %s", podDesc, podTimer)
		}
	}
}

// checkAndDeleteRetryPod deletes a specific entry from the map, if it existed, returns true
func (oc *Controller) checkAndDeleteRetryPod(pod *kapi.Pod) bool {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if _, ok := oc.retryPods[key]; ok {
		delete(oc.retryPods, key)
		return true
	}
	return false
}

// checkAndSkipRetryPod sets a specific entry from the map to be ignored for subsequent retries
// if it existed, returns true
func (oc *Controller) checkAndSkipRetryPod(pod *kapi.Pod) bool {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entry.ignore = true
		return true
	}
	return false
}

// unSkipRetryPod ensures a pod is no longer ignored for retry loop
func (oc *Controller) unSkipRetryPod(pod *kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entry.ignore = false
	}
}

// initRetryAddPod tracks a pod that failed to be created to potentially retry later (needsAdd = true)
// initially it is marked as skipped for retry loop (ignore = true)
func (oc *Controller) initRetryAddPod(pod *kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entry.timeStamp = time.Now()
		entry.pod = pod
		entry.needsAdd = true
	} else {
		oc.retryPods[key] = &retryEntry{pod, time.Now(), 1, true, true, nil}
	}
}

// initRetryDelPod tracks a pod that failed to be deleted to potentially retry later (needsDel != nil)
// initially it is marked as skipped for retry loop (ignore = true)
func (oc *Controller) initRetryDelPod(pod *kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	var portInfo *lpInfo
	if !util.PodWantsNetwork(pod) {
		// create dummy logicalPortInfo for host-networked pods
		mac, _ := net.ParseMAC("00:00:00:00:00:00")
		portInfo = &lpInfo{
			logicalSwitch: "host-networked",
			name:          getPodNamespacedName(pod),
			uuid:          "host-networked",
			ips:           []*net.IPNet{},
			mac:           mac,
		}
	} else {
		portInfo, _ = oc.logicalPortCache.get(key)
	}
	if entry, ok := oc.retryPods[key]; ok {
		entry.timeStamp = time.Now()
		entry.needsDel = portInfo
	} else {
		oc.retryPods[key] = &retryEntry{pod, time.Now(), 1, true, false, portInfo}
	}
}

func (oc *Controller) removeAddRetry(pod *kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entry.needsAdd = false
	}
}

func (oc *Controller) removeDeleteRetry(pod *kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entry.needsDel = nil
	}
}

func (oc *Controller) getPodRetryEntry(pod *kapi.Pod) *retryEntry {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	key := getPodNamespacedName(pod)
	if entry, ok := oc.retryPods[key]; ok {
		entryCopy := *entry
		return &entryCopy
	}
	return nil
}

// addRetryPods adds multiple pods to be retried later for their add events
func (oc *Controller) addRetryPods(pods []kapi.Pod) {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	for _, pod := range pods {
		pod := pod
		key := getPodNamespacedName(&pod)
		if entry, ok := oc.retryPods[key]; ok {
			entry.timeStamp = time.Now()
			entry.pod = &pod
		} else {
			oc.retryPods[key] = &retryEntry{&pod, time.Now(), 1, false, true, nil}
		}
	}
}

func (oc *Controller) requestRetryPods() {
	select {
	case oc.retryPodsChan <- struct{}{}:
		klog.V(5).Infof("Iterate retry pods requested")
	default:
		klog.V(5).Infof("Iterate retry pods already requested")
	}
}

// unit testing only
func (oc *Controller) hasPodRetryEntry(pod *kapi.Pod) bool {
	oc.retryPodsLock.Lock()
	defer oc.retryPodsLock.Unlock()
	_, ok := oc.retryPods[getPodNamespacedName(pod)]
	return ok
}

// getPodNamespacedName returns <namespace>_<podname> for the provided pod
// this is to maintain the same key format as used by logicalPortCache
func getPodNamespacedName(pod *kapi.Pod) string {
	return fmt.Sprintf("%s_%s", pod.Namespace, pod.Name)
}
