package adminnetworkpolicy

import (
	"fmt"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

func (c *Controller) processNextANPPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	anpPodKey, quit := c.anpPodQueue.Get()
	if quit {
		return false
	}
	defer c.anpPodQueue.Done(anpPodKey)
	err := c.syncAdminNetworkPolicyPod(anpPodKey.(string))
	if err == nil {
		c.anpPodQueue.Forget(anpPodKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", anpPodKey, err))

	if c.anpPodQueue.NumRequeues(anpPodKey) < maxRetries {
		c.anpPodQueue.AddRateLimited(anpPodKey)
		return true
	}

	c.anpPodQueue.Forget(anpPodKey)
	return true
}

func (c *Controller) syncAdminNetworkPolicyPod(key string) error {
	return nil
}
