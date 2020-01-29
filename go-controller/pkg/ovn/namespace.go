package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
)

const (
	// Annotation used to enable/disable multicast in the namespace
	nsMulticastAnnotation = "k8s.ovn.org/multicast-enabled"
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
			deleteAddressSet(hashedAddressSet(addrSetName))
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

func (oc *Controller) addPodToNamespace(ns string, ip net.IP, logicalPort string) error {
	mutex := oc.getNamespaceLock(ns)
	if mutex == nil {
		return nil
	}
	defer mutex.Unlock()

	if oc.namespacePolicies[ns] == nil {
		return nil
	}

	// If pod has already been added, nothing to do.
	address := ip.String()
	if oc.namespaceAddressSet[ns][address] != "" {
		return nil
	}

	oc.namespaceAddressSet[ns][address] = logicalPort
	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns] {
		addresses = append(addresses, address)
	}

	setAddressSet(hashedAddressSet(ns), addresses)

	// If multicast is allowed and enabled for the namespace, add the port
	// to the allow policy.
	if oc.multicastSupport && oc.multicastEnabled[ns] {
		if err := oc.podAddAllowMulticastPolicy(ns, logicalPort); err != nil {
			return err
		}
	}

	return nil
}

func (oc *Controller) deletePodFromNamespace(ns string, ip net.IP, logicalPort string) error {
	if ip == nil {
		return nil
	}

	mutex := oc.getNamespaceLock(ns)
	if mutex == nil {
		return nil
	}
	defer mutex.Unlock()

	address := ip.String()
	if oc.namespaceAddressSet[ns][address] == "" {
		return nil
	}

	delete(oc.namespaceAddressSet[ns], address)
	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns] {
		addresses = append(addresses, address)
	}

	setAddressSet(hashedAddressSet(ns), addresses)

	// Remove the port from the multicast allow policy.
	if oc.multicastSupport && oc.multicastEnabled[ns] {
		if err := oc.podDeleteAllowMulticastPolicy(ns, logicalPort); err != nil {
			return err
		}
	}

	return nil
}

// Creates an explicit "allow" policy for multicast traffic within the
// namespace if multicast is enabled. Otherwise, removes the "allow" policy.
// Traffic will be dropped by the default multicast deny ACL.
func (oc *Controller) multicastUpdateNamespace(ns *kapi.Namespace) {
	if !oc.multicastSupport {
		return
	}

	enabled := (ns.Annotations[nsMulticastAnnotation] == "true")
	enabledOld := oc.multicastEnabled[ns.Name]

	if enabledOld == enabled {
		return
	}

	var err error
	if enabled {
		err = oc.createMulticastAllowPolicy(ns.Name)
	} else {
		err = deleteMulticastAllowPolicy(ns.Name)
	}
	if err != nil {
		logrus.Errorf(err.Error())
		return
	}

	oc.multicastEnabled[ns.Name] = enabled
}

// Cleans up the multicast policy for this namespace if multicast was
// previously allowed.
func (oc *Controller) multicastDeleteNamespace(ns *kapi.Namespace) {
	if oc.multicastEnabled[ns.Name] {
		if err := deleteMulticastAllowPolicy(ns.Name); err != nil {
			logrus.Errorf(err.Error())
		}
	}
	delete(oc.multicastEnabled, ns.Name)
}

// AddNamespace creates corresponding addressset in ovn db
func (oc *Controller) AddNamespace(ns *kapi.Namespace) {
	logrus.Debugf("Adding namespace: %s", ns.Name)
	oc.namespaceMutexMutex.Lock()
	if oc.namespaceMutex[ns.Name] == nil {
		oc.namespaceMutex[ns.Name] = &sync.Mutex{}
	}

	// A big fat lock per namespace to prevent race conditions
	// with namespace resources like address sets and deny acls.
	oc.namespaceMutex[ns.Name].Lock()
	defer oc.namespaceMutex[ns.Name].Unlock()
	oc.namespaceMutexMutex.Unlock()

	oc.namespaceAddressSet[ns.Name] = make(map[string]string)

	// Get all the pods in the namespace and append their IP to the
	// address_set
	existingPods, err := oc.kube.GetPods(ns.Name)
	if err != nil {
		logrus.Errorf("Failed to get all the pods (%v)", err)
	} else {
		for _, pod := range existingPods.Items {
			if pod.Status.PodIP != "" {
				portName := podLogicalPortName(&pod)
				oc.namespaceAddressSet[ns.Name][pod.Status.PodIP] = portName
			}
		}
	}

	addresses := make([]string, 0)
	for address := range oc.namespaceAddressSet[ns.Name] {
		addresses = append(addresses, address)
	}

	// Create an address_set for the namespace.  All the pods' IP address
	// in the namespace will be added to the address_set
	createAddressSet(ns.Name, hashedAddressSet(ns.Name), addresses)

	oc.namespacePolicies[ns.Name] = make(map[string]*namespacePolicy)
	oc.multicastUpdateNamespace(ns)
}

func (oc *Controller) updateNamespace(old, newer *kapi.Namespace) {
	logrus.Debugf("Updating namespace: old %s new %s", old.Name, newer.Name)

	// A big fat lock per namespace to prevent race conditions
	// with namespace resources like address sets and deny acls.
	oc.namespaceMutex[newer.Name].Lock()
	defer oc.namespaceMutex[newer.Name].Unlock()

	oc.multicastUpdateNamespace(newer)
}

func (oc *Controller) deleteNamespace(ns *kapi.Namespace) {
	logrus.Debugf("Deleting namespace: %s", ns.Name)
	oc.namespaceMutexMutex.Lock()
	defer oc.namespaceMutexMutex.Unlock()

	mutex, ok := oc.namespaceMutex[ns.Name]
	if !ok {
		return
	}
	mutex.Lock()
	defer mutex.Unlock()

	deleteAddressSet(hashedAddressSet(ns.Name))
	oc.multicastDeleteNamespace(ns)
	delete(oc.namespacePolicies, ns.Name)
	delete(oc.namespaceAddressSet, ns.Name)
	delete(oc.namespaceMutex, ns.Name)
}

// getNamespaceLock grabs the lock for a particular namespace. If the
// namespace does not exist, returns nil. Otherwise, returns the held lock.
func (oc *Controller) getNamespaceLock(ns string) *sync.Mutex {
	// lock the list of namespaces, get the mutex
	oc.namespaceMutexMutex.Lock()
	mutex, ok := oc.namespaceMutex[ns]
	oc.namespaceMutexMutex.Unlock()
	if !ok {
		return nil
	}

	// lock the individual namespace
	mutex.Lock()

	// check that the namespace wasn't deleted between getting the two locks
	if _, ok := oc.namespaceMutex[ns]; !ok {
		mutex.Unlock()
		return nil
	}

	return mutex
}
