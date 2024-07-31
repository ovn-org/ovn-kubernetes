package networkqos

import (
	// "errors"
	"fmt"
	"time"

	// libovsdbclient "github.com/ovn-org/libovsdb/client"
	// libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	// "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
)

// repairNetworkQoSes is called at startup and as the name suggests
// aims to repair the NBDB logical objects
// that are created for the network qoses in the cluster
func (c *Controller) repairNetworkQoSes() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing network qos took %v", time.Since(start))
	}()
	c.Lock()
	defer c.Unlock()
	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("unable to list NQOs from the lister, err: %v", err)
	}
	existingNQOSs := map[string]*networkqosapi.NetworkQoS{}
	for _, nqos := range nqoses {
		existingNQOSs[nqos.Name] = nqos
	}

	// TODO: implement this!

	return nil
}
