package util

import (
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"k8s.io/client-go/tools/cache"
)

// GetChildStopChan returns a new channel that doesn't affect parentStopChan, but will be closed when
// parentStopChan is closed. May be used for child goroutines that may need to be stopped with the main goroutine or
// separately.
func GetChildStopChan(parentStopChan <-chan struct{}) chan struct{} {
	childStopChan := make(chan struct{})

	select {
	case <-parentStopChan:
		// parent is already canceled
		close(childStopChan)
		return childStopChan
	default:
	}

	go func() {
		select {
		case <-parentStopChan:
			close(childStopChan)
			return
		case <-childStopChan:
			return
		}
	}()
	return childStopChan
}

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
