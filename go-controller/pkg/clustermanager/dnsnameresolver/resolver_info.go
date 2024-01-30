package dnsnameresolver

import (
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
)

// resolverInfo maintains consistent information about the DNS names, the
// corresponding DNSNameResolver objects and the namespaces in which the
// DNS names are used.
type resolverInfo struct {
	lock sync.Mutex
	kube *kube.KubeOVN

	// maps to hold the details of the DNS names and the corresponding
	// DNSNameResolver objects.
	dnsNameToResolverDetails map[string]resolverDetails
	resolverNameToDNSName    map[string]string

	// map to hold the current DNS names used in a namespace.
	namespaceDNSNames map[string]sets.Set[string]
}

// resolverDetails holds the information regarding the DNSNameResolver
// object corresponding to a DNS name.
type resolverDetails struct {
	// objName gives the name of the DNSNameResolver object corresponding
	// to the DNS name.
	objName string
	// namespaces stores those namespaces where the DNS name is being used.
	namespaces sets.Set[string]
	// collisionCount is used during the creation of the DNSNameResolver
	// object to ensure that its name does not have a collision with any
	// existing DNSNameResolver objects.
	collisionCount int
}

// newResolverInfo returns a new instance of the resolverInfo.
func newResolverInfo(kube *kube.KubeOVN) *resolverInfo {
	return &resolverInfo{
		dnsNameToResolverDetails: make(map[string]resolverDetails),
		resolverNameToDNSName:    make(map[string]string),
		namespaceDNSNames:        make(map[string]sets.Set[string]),
		kube:                     kube,
	}
}

// syncResolverInfo syncs the existing DNSNameResolver objects and the DNS names
// used in each namespace after a restart and updates the dnsNameToResolverDetails
// and resolverNameToDNSName maps. After the sync these two maps will only contain
// the details of the DNS names which are used in at least one namespace. The
// namespaceDNSNames map is also synced with the DNS names used in each namespace.
func (resInfo *resolverInfo) syncResolverInfo(dnsNameToResolver map[string]string, namespaceToDNSNames map[string][]string) {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Iterate through the existing DNSNameResolver object names and the corresponding
	// DNS names, and add the corresponding details to the dnsNameToResolverDetails and
	// resolverNameToDNSName maps.
	for dnsName, dnsNameResolverName := range dnsNameToResolver {
		if _, exists := resInfo.dnsNameToResolverDetails[dnsName]; !exists {
			resInfo.dnsNameToResolverDetails[dnsName] = resolverDetails{
				objName:    dnsNameResolverName,
				namespaces: sets.New[string](),
			}
			resInfo.resolverNameToDNSName[dnsNameResolverName] = dnsName
		}
	}

	// Iterate through the existing DNS names used in each namespace.
	// Iterate through the DNS names used in a namespace and add the
	// namespace to the list of namespaces for each of the DNS names.
	// Also add the DNS names to the set of DNS names mapped to each
	// namespace.
	for namespace, dnsNames := range namespaceToDNSNames {
		existingDNSNames := sets.New[string]()
		existingDNSNames.Insert(dnsNames...)
		dnsNames, exists := resInfo.namespaceDNSNames[namespace]
		if !exists {
			dnsNames = sets.New[string]()
		}
		for dnsName := range existingDNSNames {
			if objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]; exists {
				objDetails.namespaces.Insert(namespace)
				resInfo.dnsNameToResolverDetails[dnsName] = objDetails
				dnsNames.Insert(dnsName)
			}
		}
		resInfo.namespaceDNSNames[namespace] = dnsNames
	}

	// Delete the details of the DNS names from the dnsNameToResolverDetails and
	// the resolverNameToDNSName if the DNS name is not used in any namespace.
	for dnsName, objDetails := range resInfo.dnsNameToResolverDetails {
		if objDetails.namespaces.Len() == 0 {
			delete(resInfo.resolverNameToDNSName, objDetails.objName)
			delete(resInfo.dnsNameToResolverDetails, dnsName)
		}
	}
}

// modifyDNSNamesForNamespace obtains the newly added and deleted
// DNS names from the current and existing DNS names. For the newly
// added DNS names, the addDNSName function is called and for the
// deleted DNS names, the deleteDNSName function is called.
func (resInfo *resolverInfo) modifyDNSNamesForNamespace(dnsNames []string, namespace string) error {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Get the current DNS names which are in use in the namespace.
	currentDNSNames := sets.New[string]()
	currentDNSNames.Insert(dnsNames...)

	// Check if the namespace has some existing DNS names.
	oldDNSNames, exists := resInfo.namespaceDNSNames[namespace]
	if !exists {
		oldDNSNames = sets.New[string]()
	}

	// Get the newly added DNS names.
	addedDNSNames := currentDNSNames.Difference(oldDNSNames)
	// Get the deleted DNS names.
	deletedDNSNames := oldDNSNames.Difference(currentDNSNames)

	var errorList []error
	// Iterate through each newly added DNS name and call the addDNSName.
	for addedDNSName := range addedDNSNames {
		if err := resInfo.addDNSName(addedDNSName, namespace); err != nil {
			errorList = append(errorList, err)
		}
	}

	// Iterate through each deleted DNS name and call the deleteDNSName.
	for deletedDNSName := range deletedDNSNames {
		if err := resInfo.deleteDNSName(deletedDNSName, namespace); err != nil {
			errorList = append(errorList, err)
		}
	}

	// Return the errors, if any.
	if len(errorList) != 0 {
		return errors.NewAggregate(errorList)
	}

	return nil
}

// deleteDNSNamesForNamespace obtains the deleted DNS names for the namespace
// calls the deleteDNSName function for those DNS names.
func (resInfo *resolverInfo) deleteDNSNamesForNamespace(namespace string) error {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Check if any DNS names were used in the namespace.
	deletedDNSNames, exists := resInfo.namespaceDNSNames[namespace]
	if !exists {
		return nil
	}

	var errorList []error
	// Iterate through each deleted DNS name of the namespace and call the deleteDNSName.
	for deletedDNSName := range deletedDNSNames {
		if err := resInfo.deleteDNSName(deletedDNSName, namespace); err != nil {
			errorList = append(errorList, err)
		}
	}

	// Return the errors, if any.
	if len(errorList) != 0 {
		return errors.NewAggregate(errorList)
	}

	// Remove the entry for the namespace from efNamespaceDNSNames.
	delete(resInfo.namespaceDNSNames, namespace)

	return nil
}

// addDNSName creates the corresponding DNSNameResolver object for the DNS name
// if not already created. The namespace is added to the list of namespaces where
// the DNS name is used.
func (resInfo *resolverInfo) addDNSName(dnsName, namespace string) error {
	objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]
	// Create the DNSNameResolver object if its details are not available.
	if !exists || objDetails.objName == "" {
		var err error
		objDetails.objName, objDetails.collisionCount, err = resInfo.generateObjName(objDetails.collisionCount, dnsName)
		if err != nil {
			resInfo.dnsNameToResolverDetails[dnsName] = objDetails
			return err
		}

		if err := createDNSNameResolver(resInfo.kube, objDetails.objName, dnsName); err != nil {
			return err
		}
		if objDetails.namespaces == nil {
			objDetails.namespaces = sets.New[string]()
		}
	}

	// Add the DNS name to the set of DNS names belonging
	// to the namespace.
	dnsNames, exists := resInfo.namespaceDNSNames[namespace]
	if !exists {
		dnsNames = sets.New[string]()
	}
	dnsNames.Insert(dnsName)
	resInfo.namespaceDNSNames[namespace] = dnsNames

	objDetails.namespaces.Insert(namespace)

	resInfo.dnsNameToResolverDetails[dnsName] = objDetails
	resInfo.resolverNameToDNSName[objDetails.objName] = dnsName

	return nil
}

// generateObjName generates the name for a dns name resolver object corresponding
// to a DNS name. The collisionCount variable for the DNS name is incremented a
// maximum of 10 times for generating a name for the corresponding dns name
// resolver object without any collision.
func (resInfo *resolverInfo) generateObjName(collisionCount int, dnsName string) (string, int, error) {
	maxRetry := collisionCount + 10
	var objName string
	for ; collisionCount < maxRetry; collisionCount++ {
		objName = "dns-" + computeHash(dnsName, collisionCount)
		// Check if the generated object name matches with any of the existing
		// object's name.
		if _, found := resInfo.resolverNameToDNSName[objName]; !found {
			break
		}
	}
	// If the maximum retry is reached, then return an error so that it can be
	// retried again via the controller's retry mechanism.
	if collisionCount == maxRetry {
		return "", collisionCount, fmt.Errorf("encountered collision in name while creating dns name resolver object for %s", dnsName)
	}
	return objName, collisionCount, nil
}

// deleteDNSName removes the namespace from the list of namespaces the
// DNS name is currently used. If the DNS name, is not used in any other
// namespace, then the corresponding DNSNameResolver object is deleted
// and the details of the DNS name is removed.
func (resInfo *resolverInfo) deleteDNSName(dnsName, namespace string) error {
	objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]
	if exists {
		delete(objDetails.namespaces, namespace)
		if objDetails.namespaces.Len() == 0 {
			err := deleteDNSNameResolver(resInfo.kube, objDetails.objName)
			if err != nil {
				// Insert back the namespace when an error is encountered
				// while deleting the DNSNameResolver object.
				objDetails.namespaces.Insert(namespace)
				return err
			}
			delete(resInfo.dnsNameToResolverDetails, dnsName)
			delete(resInfo.resolverNameToDNSName, objDetails.objName)
		}
	}

	// Remove the DNS name from the set of DNS names belonging
	// to the namespace. If the set is empty, then delete the
	// mapping to the namespace.
	dnsNames, exists := resInfo.namespaceDNSNames[namespace]
	if exists {
		dnsNames.Delete(dnsName)
		if dnsNames.Len() == 0 {
			delete(resInfo.namespaceDNSNames, namespace)
		} else {
			resInfo.namespaceDNSNames[namespace] = dnsNames
		}
	}

	resInfo.dnsNameToResolverDetails[dnsName] = objDetails
	resInfo.resolverNameToDNSName[objDetails.objName] = dnsName

	return nil
}

// getDNSNameForResolver returns the DNS name corresponding to the DNSNameResolver
// object name.
func (resInfo *resolverInfo) getDNSNameForResolver(resolverName string) (string, bool) {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Check if the details of the object and the corresponding DNS
	// name exists or not.
	dnsName, found := resInfo.resolverNameToDNSName[resolverName]
	if !found {
		return "", false
	}
	// Get the object details corresponding to the DNS name.
	objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]
	if !exists {
		// If the object details doesn't exists in the dnsNameToResolverDetails map,
		// then remove the corresponding entry from the resolverNameToDNSName map.
		delete(resInfo.resolverNameToDNSName, resolverName)
		return "", false
	}
	// Check if the DNS name is used in any of the namespaces. If not,
	// then remove the details of the DNS name and the object.
	if objDetails.namespaces.Len() == 0 {
		delete(resInfo.dnsNameToResolverDetails, dnsName)
		delete(resInfo.resolverNameToDNSName, objDetails.objName)
		return "", false
	}

	return dnsName, true
}

// isDNSNameMatchingResolverName checks if the DNS name and the DNSNameResolver object
// name matches the existing information.
func (resInfo *resolverInfo) isDNSNameMatchingResolverName(dnsName, resolverName string) bool {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Fetch the DNSNameResolver object details using the DNS name. If the details doesn't
	// exist or if the existing object name doesn't match the object name passed as an
	// argument, then return false.
	objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]
	if !exists || objDetails.objName != resolverName {
		return false
	}
	return true
}
