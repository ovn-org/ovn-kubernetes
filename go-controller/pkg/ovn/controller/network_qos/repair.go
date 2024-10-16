package networkqos

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// repairNetworkQoSes is called at startup and as the name suggests
// aims to repair the NBDB logical objects
// that are created for the network qoses in the cluster
func (c *Controller) repairNetworkQoSes() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing network qos took %v", time.Since(start))
	}()
	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("unable to list NetworkQoSes from the lister: %v", err)
	}
	nqosMap := map[string]*networkqosapi.NetworkQoS{}
	for _, nqos := range nqoses {
		nqosMap[joinMetaNamespaceAndName(nqos.Namespace, nqos.Name, ":")] = nqos
	}

	// delete stale ovn qos objects owned by NetworkQoS
	if err := libovsdbops.DeleteQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		if qos.ExternalIDs[libovsdbops.OwnerControllerKey.String()] != c.controllerName ||
			qos.ExternalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType) {
			return false
		}
		objName := qos.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		// doesn't have corresponding k8s name
		if objName == "" {
			klog.Warningf("OVN QoS %s doesn't have expected key %s", qos.UUID, libovsdbops.ObjectNameKey.String())
			return true
		}
		// clean up qoses whose k8s object has gone
		if _, exists := nqosMap[objName]; !exists {
			klog.Warningf("OVN QoS %s doesn't have expected NetworkQoS object %s", qos.UUID, objName)
			return true
		}
		return false
	}); err != nil {
		klog.Errorf("Failed to get ops to clean up stale QoSes: %v", err)
	}

	// delete address sets whose networkqos object has gone in k8s
	if err := libovsdbops.DeleteAddressSetsWithPredicate(c.nbClient, func(addrset *nbdb.AddressSet) bool {
		if addrset.ExternalIDs[libovsdbops.OwnerControllerKey.String()] != c.controllerName ||
			addrset.ExternalIDs[libovsdbops.OwnerTypeKey.String()] != string(libovsdbops.NetworkQoSOwnerType) {
			return false
		}
		objName := addrset.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		// doesn't have corresponding k8s name
		if objName == "" {
			klog.Warningf("AddressSet %s doesn't have expected key %s", addrset.UUID, libovsdbops.ObjectNameKey.String())
			return true
		}
		// clean up qoses whose k8s object has gone
		if _, exists := nqosMap[objName]; !exists {
			klog.Warningf("AddressSet %s doesn't have expected NetworkQoS object %s", addrset.UUID, objName)
			return true
		}
		return false
	}); err != nil {
		klog.Errorf("Failed to get ops clean up stale address sets: %v", err)
	}

	return nil
}
