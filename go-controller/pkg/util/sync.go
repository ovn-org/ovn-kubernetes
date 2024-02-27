package util

import (
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"k8s.io/client-go/tools/cache"
)

func GetChildStopChanWithTimeout(parentStopChan <-chan struct{}, duration time.Duration) chan struct{} {
	childStopChan := make(chan struct{})
	timer := time.NewTicker(duration)
	go func() {
		defer timer.Stop()
		select {
		case <-parentStopChan:
			close(childStopChan)
			return
		case <-childStopChan:
			return
		case <-timer.C:
			close(childStopChan)
			return
		}
	}()
	return childStopChan
}

// WaitForInformerCacheSyncWithTimeout waits for the provided informer caches to be populated with all existing objects
// by their respective informer. This corresponds to a LIST operation on the corresponding resource types.
// WaitForInformerCacheSyncWithTimeout times out and returns false if the provided caches haven't all synchronized within types.InformerSyncTimeout
func WaitForInformerCacheSyncWithTimeout(controllerName string, stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	return cache.WaitForNamedCacheSync(controllerName, GetChildStopChanWithTimeout(stopCh, types.InformerSyncTimeout), cacheSyncs...)
}

// WaitForHandlerSyncWithTimeout waits for the provided handlers to do a sync on all existing objects for the resource types they're
// watching. This corresponds to adding all existing objects. If that doesn't happen before the provided timeout,
// WaitForInformerCacheSyncWithTimeout times out and returns false.
func WaitForHandlerSyncWithTimeout(controllerName string, stopCh <-chan struct{}, timeout time.Duration, handlerSyncs ...cache.InformerSynced) bool {
	return cache.WaitForNamedCacheSync(controllerName, GetChildStopChanWithTimeout(stopCh, timeout), handlerSyncs...)
}
