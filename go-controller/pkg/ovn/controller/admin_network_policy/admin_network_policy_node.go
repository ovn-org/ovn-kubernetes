package adminnetworkpolicy

import (
	"fmt"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func (c *Controller) processNextANPNodeWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpNodeKey, quit := c.anpNodeQueue.Get()
	if quit {
		return false
	}
	defer c.anpNodeQueue.Done(anpNodeKey)
	err := c.syncAdminNetworkPolicyNode(anpNodeKey.(string))
	if err == nil {
		c.anpNodeQueue.Forget(anpNodeKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpNodeKey, err))

	if c.anpNodeQueue.NumRequeues(anpNodeKey) < maxRetries {
		c.anpNodeQueue.AddRateLimited(anpNodeKey)
		return true
	}

	c.anpNodeQueue.Forget(anpNodeKey)
	return true
}

// syncAdminNetworkPolicyNode decides the main logic everytime
// we dequeue a key from the anpNodeQueue cache
func (c *Controller) syncAdminNetworkPolicyNode(key string) error {
	return nil
}
