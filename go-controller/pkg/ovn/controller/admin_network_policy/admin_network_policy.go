package adminnetworkpolicy

import (
	"fmt"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func (c *Controller) processNextANPWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpKey, quit := c.anpQueue.Get()
	if quit {
		return false
	}
	defer c.anpQueue.Done(anpKey)

	err := c.syncAdminNetworkPolicy(anpKey.(string))
	if err == nil {
		c.anpQueue.Forget(anpKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpKey, err))

	if c.anpQueue.NumRequeues(anpKey) < maxRetries {
		c.anpQueue.AddRateLimited(anpKey)
		return true
	}

	c.anpQueue.Forget(anpKey)
	return true
}

func (c *Controller) syncAdminNetworkPolicy(key string) error {
	return nil
}
