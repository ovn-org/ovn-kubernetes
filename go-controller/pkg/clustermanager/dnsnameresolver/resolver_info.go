package dnsnameresolver

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"sync"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	hashutil "k8s.io/kubernetes/pkg/util/hash"

	ocpnetworkclientset "github.com/openshift/client-go/network/clientset/versioned"
)

// resolverInfo maintains consistent information about the DNS names, the
// corresponding DNSNameResolver objects and the namespaces in which the
// DNS names are used.
type resolverInfo struct {
	lock             sync.Mutex
	ocpNetworkClient ocpnetworkclientset.Interface

	// maps to hold the details of the DNS names and the corresponding
	// DNSNameResolver objects.
	dnsNameToResolverDetails map[string]*resolverDetails
	resolverNameToDNSName    map[string]string

	// map to hold the current DNS names used in a namespace.
	namespaceDNSNames map[string]sets.Set[string]
}

// resolverDetails holds the information regarding the DNSNameResolver
// object corresponding to a DNS name.
type resolverDetails struct {
	// objName gives the name of the DNSNameResolver object corresponding
	// to the DNS name. If objName is empty string then the DNSNameResolver
	// object was attepmted to be created. The corresponding entry for the
	// DNSNameResolver object will be created in dnsNameToResolverDetails
	// only when the object with the name is successfully created.
	objName string
	// namespaces stores those namespaces where the DNS name is being used.
	namespaces sets.Set[string]
	// collisionCount is used during the creation of the DNSNameResolver
	// object to ensure that its name does not have a collision with any
	// existing DNSNameResolver objects.
	collisionCount int
}

// newResolverInfo returns a new instance of the resolverInfo.
func newResolverInfo(ocpNetworkClient ocpnetworkclientset.Interface) *resolverInfo {
	return &resolverInfo{
		dnsNameToResolverDetails: make(map[string]*resolverDetails),
		resolverNameToDNSName:    make(map[string]string),
		namespaceDNSNames:        make(map[string]sets.Set[string]),
		ocpNetworkClient:         ocpNetworkClient,
	}
}

// SyncResolverInfo syncs the existing DNSNameResolver objects and the DNS names
// used in each namespace after a restart and updates the dnsNameToResolverDetails
// and resolverNameToDNSName maps. After the sync these two maps will only contain
// the details of the DNS names which are used in at least one namespace. The
// namespaceDNSNames map is also synced with the DNS names used in each namespace.
func (resInfo *resolverInfo) SyncResolverInfo(dnsNameToResolver map[string]string, namespaceToDNSNames map[string][]string) {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Iterate through the existing DNSNameResolver object names and the corresponding
	// DNS names, and add the corresponding details to the dnsNameToResolverDetails and
	// resolverNameToDNSName maps.
	for dnsName, dnsNameResolverName := range dnsNameToResolver {
		if _, exists := resInfo.dnsNameToResolverDetails[dnsName]; !exists {
			resInfo.dnsNameToResolverDetails[dnsName] = &resolverDetails{
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
	for namespace, existingDNSNames := range namespaceToDNSNames {
		dnsNames := sets.New[string]()
		for _, dnsName := range existingDNSNames {
			// Check if the corresponding DNSNameResolver object details are available
			// for the DNS name and add the namespace to the list of namespaces where
			// the DNS name is used. If the object details are not currently available,
			// then those objects will be added the with Add event on controller startup.
			if objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]; exists {
				objDetails.namespaces.Insert(namespace)
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

// ModifyDNSNamesForNamespace obtains the newly added and deleted
// DNS names from the current and existing DNS names. For the newly
// added DNS names, the addDNSName function is called and for the
// deleted DNS names, the deleteDNSName function is called.
func (resInfo *resolverInfo) ModifyDNSNamesForNamespace(dnsNames []string, namespace string) error {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Get the current DNS names which are in use in the namespace.
	requestedDNSNames := sets.New[string](dnsNames...)

	// Check if the namespace has some existing DNS names.
	existingDNSNames, exists := resInfo.namespaceDNSNames[namespace]
	if !exists {
		existingDNSNames = sets.New[string]()
	}

	// Get the newly added DNS names.
	addedDNSNames := requestedDNSNames.Difference(existingDNSNames)
	// Get the deleted DNS names.
	deletedDNSNames := existingDNSNames.Difference(requestedDNSNames)

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

// DeleteDNSNamesForNamespace obtains the deleted DNS names for the namespace
// calls the deleteDNSName function for those DNS names.
func (resInfo *resolverInfo) DeleteDNSNamesForNamespace(namespace string) error {
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

	return nil
}

// addDNSName creates the corresponding DNSNameResolver object for the DNS name
// if not already created. The namespace is added to the list of namespaces where
// the DNS name is used.
func (resInfo *resolverInfo) addDNSName(dnsName, namespace string) error {
	objDetails, exists := resInfo.dnsNameToResolverDetails[dnsName]
	if !exists {
		objDetails = &resolverDetails{
			namespaces: sets.New[string](),
		}
		resInfo.dnsNameToResolverDetails[dnsName] = objDetails
	}
	// Create the DNSNameResolver object if its details are not available.
	if objDetails.objName == "" {
		var err error
		objDetails.objName, objDetails.collisionCount, err = resInfo.generateObjName(objDetails.collisionCount, dnsName)
		if err != nil {
			return err
		}

		if err := createDNSNameResolver(resInfo.ocpNetworkClient, objDetails.objName, dnsName); err != nil {
			return err
		}
		resInfo.resolverNameToDNSName[objDetails.objName] = dnsName
	}

	objDetails.namespaces.Insert(namespace)

	// Add the DNS name to the set of DNS names belonging
	// to the namespace.
	dnsNames, exists := resInfo.namespaceDNSNames[namespace]
	if !exists {
		dnsNames = sets.New[string]()
		resInfo.namespaceDNSNames[namespace] = dnsNames
	}
	dnsNames.Insert(dnsName)

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

// computeHash returns a hash value calculated from dns name and a collisionCount
// to avoid hash collision. The hash will be safe encoded to avoid bad words.
// Inspired by the ComputeHash function used for creating hash value from pod templates in
// https://github.com/openshift/kubernetes/blob/master/pkg/controller/controller_utils.go
func computeHash(dnsName string, collisionCount int) string {
	dnsNameHasher := fnv.New32a()
	hashutil.DeepHashObject(dnsNameHasher, dnsName)

	// Add collisionCount in the hash if it is not zero.
	if collisionCount != 0 {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(collisionCount))
		dnsNameHasher.Write(collisionCountBytes)
	}
	return rand.SafeEncodeString(fmt.Sprint(dnsNameHasher.Sum32()))
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
			err := deleteDNSNameResolver(resInfo.ocpNetworkClient, objDetails.objName)
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
		}
	}

	return nil
}

// GetDNSNameForResolver returns the DNS name corresponding to the DNSNameResolver
// object name.
func (resInfo *resolverInfo) GetDNSNameForResolver(resolverName string) (string, bool) {
	resInfo.lock.Lock()
	defer resInfo.lock.Unlock()

	// Check if the details of the object and the corresponding DNS
	// name exists or not.
	dnsName, found := resInfo.resolverNameToDNSName[resolverName]
	if !found {
		return "", false
	}

	return dnsName, true
}

// IsDNSNameMatchingResolverName checks if the DNS name and the DNSNameResolver object
// name matches the existing information.
func (resInfo *resolverInfo) IsDNSNameMatchingResolverName(dnsName, resolverName string) bool {
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
