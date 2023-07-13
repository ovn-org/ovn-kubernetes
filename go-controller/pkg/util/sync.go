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

func WaitForNamedCacheSyncWithTimeout(controllerName string, stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) bool {
	return cache.WaitForNamedCacheSync(controllerName, GetChildStopChanWithTimeout(stopCh, types.InformerSyncTimeout), cacheSyncs...)
}
