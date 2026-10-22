// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
)

// Queue deleted policies for cleanup and record legacy address sets before workers start.
// Queued deletions retry normally. Objects without a policy name have no queue key, so
// delete them here. Only NetworkQoS-owned objects are considered.
func (c *Controller) repairNetworkQoSes() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing NetworkQoS took %v", time.Since(start))
	}()

	nqoses, err := c.nqosLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list NetworkQoS: %w", err)
	}
	existing := sets.New[string]()
	for _, nqos := range nqoses {
		existing.Insert(ovnObjectName(nqos.Namespace, nqos.Name))
	}
	isOwnedByNetworkQoS := func(externalIDs map[string]string) bool {
		return externalIDs[libovsdbops.OwnerTypeKey.String()] == string(libovsdbops.NetworkQoSOwnerType)
	}

	staleQoSes, err := libovsdbops.FindQoSesWithPredicate(c.nbClient, func(qos *nbdb.QoS) bool {
		return isOwnedByNetworkQoS(qos.ExternalIDs) && !existing.Has(qos.ExternalIDs[libovsdbops.ObjectNameKey.String()])
	})
	if err != nil {
		return fmt.Errorf("failed to look up stale QoSes: %w", err)
	}
	legacyAddrSets, err := libovsdbops.FindAddressSetsWithPredicate(c.nbClient, func(addrSet *nbdb.AddressSet) bool {
		return isOwnedByNetworkQoS(addrSet.ExternalIDs)
	})
	if err != nil {
		return fmt.Errorf("failed to look up the address sets the per-network controllers owned: %w", err)
	}

	staleKeys := sets.New[string]()
	var unnamedQoSes []*nbdb.QoS
	for _, qos := range staleQoSes {
		if key, ok := nqosKeyOf(qos.ExternalIDs[libovsdbops.ObjectNameKey.String()]); ok {
			staleKeys.Insert(key)
		} else {
			unnamedQoSes = append(unnamedQoSes, qos)
		}
	}
	var unnamedAddrSets []*nbdb.AddressSet
	for _, addrSet := range legacyAddrSets {
		objectName := addrSet.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		key, ok := nqosKeyOf(objectName)
		if !ok {
			unnamedAddrSets = append(unnamedAddrSets, addrSet)
			continue
		}
		c.legacyAddressSets.add(objectName, addrSet)
		if !existing.Has(objectName) {
			staleKeys.Insert(key)
		}
	}
	for key := range staleKeys {
		klog.V(4).Infof("Removing what was programmed for NetworkQoS %s, which no longer exists", key)
		c.nqosController.Reconcile(key)
	}
	if len(unnamedQoSes) > 0 || len(unnamedAddrSets) > 0 {
		// Without a policy name, failed cleanup has to wait until the next restart.
		if err := c.deleteUnnamed(unnamedQoSes, unnamedAddrSets); err != nil {
			klog.Errorf("Failed to delete the QoS rules and address sets owned by no NetworkQoS: %v", err)
		}
	}
	return nil
}

func nqosKeyOf(objectName string) (string, bool) {
	namespace, name, ok := strings.Cut(objectName, ovnObjectNameSeparator)
	if !ok || namespace == "" || name == "" {
		return "", false
	}
	return nqosKey(namespace, name), true
}

func (c *Controller) deleteUnnamed(qoses []*nbdb.QoS, addrSets []*nbdb.AddressSet) error {
	if err := c.deleteOvnQoSes(qoses); err != nil {
		return err
	}
	ops, err := libovsdbops.DeleteAddressSetsOps(c.nbClient, nil, addrSets...)
	if err != nil {
		return fmt.Errorf("failed to get ops to delete address sets: %w", err)
	}
	if _, err := libovsdbops.TransactAndCheck(c.nbClient, ops); err != nil {
		return fmt.Errorf("failed to delete address sets: %w", err)
	}
	return nil
}
