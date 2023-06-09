package adminnetworkpolicy

import (
	"fmt"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func (c *Controller) processNextANPNamespaceWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpNSKey, quit := c.anpNamespaceQueue.Get()
	if quit {
		return false
	}
	defer c.anpNamespaceQueue.Done(anpNSKey)

	err := c.syncAdminNetworkPolicyNamespace(anpNSKey.(string))
	if err == nil {
		c.anpNamespaceQueue.Forget(anpNSKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpNSKey, err))

	if c.anpNamespaceQueue.NumRequeues(anpNSKey) < maxRetries {
		c.anpNamespaceQueue.AddRateLimited(anpNSKey)
		return true
	}

	c.anpNamespaceQueue.Forget(anpNSKey)
	return true
}

func (c *Controller) syncAdminNetworkPolicyNamespace(key string) error {
	return nil
}
