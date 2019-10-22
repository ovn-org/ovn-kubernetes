package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

func (oc *Controller) syncNamespaces(namespaces []interface{}) {
	expectedNs := make(map[string]bool)
	for _, nsInterface := range namespaces {
		ns, ok := nsInterface.(*kapi.Namespace)
		if !ok {
			logrus.Errorf("Spurious object in syncNamespaces: %v", nsInterface)
			continue
		}
		expectedNs[ns.Name] = true
	}

	err := oc.forEachAddressSetUnhashedName(func(addrSetName,
		namespaceName, nameSuffix string) {
		if nameSuffix == "" && !expectedNs[namespaceName] {
			// delete the address sets for this namespace from OVN
			oc.deleteAddressSet(hashedAddressSet(addrSetName))
		}
	})
	if err != nil {
		logrus.Errorf("Error in syncing namespaces: %v", err)
	}
}

func (oc *Controller) waitForNamespaceEvent(namespace string) error {
	// Wait for 10 seconds to get the namespace event.
	count := 100
	for {
		if oc.namespacePolicies[namespace] != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
		count--
		if count == 0 {
			return fmt.Errorf("timeout waiting for namespace event")
		}
	}
	return nil
}

func (oc *Controller) addPodToNamespaceAddressSet(ns string, ip net.IP) {
	if oc.namespacePolicies[ns] == nil {
		return
	}

	oc.namespaceMutex[ns].Lock()
	defer oc.namespaceMutex[ns].Unlock()

	// If pod has already been added, nothing to do.
	address := ip.String()
	if oc.namespaceAddressSet[ns][address] {
		return
	}

	oc.namespaceAddressSet[ns][address] = true
	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns] {
		addresses = append(addresses, address)
	}

	oc.setAddressSet(hashedAddressSet(ns), addresses)
}

func (oc *Controller) deletePodFromNamespaceAddressSet(ns string, ip net.IP) {
	if ip == nil || oc.namespacePolicies[ns] == nil {
		return
	}

	oc.namespaceMutex[ns].Lock()
	defer oc.namespaceMutex[ns].Unlock()

	address := ip.String()
	if !oc.namespaceAddressSet[ns][address] {
		return
	}

	delete(oc.namespaceAddressSet[ns], address)
	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns] {
		addresses = append(addresses, address)
	}

	oc.setAddressSet(hashedAddressSet(ns), addresses)
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *Controller) AddNamespace(ns *kapi.Namespace) {
	logrus.Debugf("Adding namespace: %s", ns.Name)

	if oc.namespaceMutex[ns.Name] == nil {
		oc.namespaceMutex[ns.Name] = &sync.Mutex{}
	}

	// A big fat lock per namespace to prevent race conditions
	// with namespace resources like address sets and deny acls.
	oc.namespaceMutex[ns.Name].Lock()
	defer oc.namespaceMutex[ns.Name].Unlock()

	oc.namespaceAddressSet[ns.Name] = make(map[string]bool)

	// Get all the pods in the namespace and append their IP to the
	// address_set
	existingPods, err := oc.kube.GetPods(ns.Name)
	if err != nil {
		logrus.Errorf("Failed to get all the pods (%v)", err)
	} else {
		for _, pod := range existingPods.Items {
			if pod.Status.PodIP != "" {
				oc.namespaceAddressSet[ns.Name][pod.Status.PodIP] = true
			}
		}
	}

	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns.Name] {
		addresses = append(addresses, address)
	}

	// Create an address_set for the namespace.  All the pods' IP address
	// in the namespace will be added to the address_set
	oc.createAddressSet(ns.Name, hashedAddressSet(ns.Name),
		addresses)

	oc.namespacePolicies[ns.Name] = make(map[string]*namespacePolicy)
}

func (oc *Controller) deleteNamespace(ns *kapi.Namespace) {
	logrus.Debugf("Deleting namespace: %+v", ns.Name)

	if oc.namespacePolicies[ns.Name] == nil {
		return
	}

	oc.namespaceMutex[ns.Name].Lock()

	oc.deleteAddressSet(hashedAddressSet(ns.Name))
	oc.namespacePolicies[ns.Name] = nil
	oc.namespaceAddressSet[ns.Name] = nil

	oc.namespaceMutex[ns.Name].Unlock()
	oc.namespaceMutex[ns.Name] = nil
}
