package ops

import (
	"fmt"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type dbObjType int
type ownerType = string
type ExternalIDKey string

func (key ExternalIDKey) String() string {
	return string(key)
}

// ObjectIDsType defines which ExternalIDs are used to indentify db objects.
// ExternalIDs are defined based on dbObjType and ownerType, e.g. default network controller creates address
// sets for namespaces and network policies, and needs to use different sets of ids for them. So it will
// create ObjectIDsType with the same dbObjType=addressSet, but different ownerTypes NamespaceOwnerType and
// NetworkPolicyOwnerType. Then it can define a set of ExternalIDs that will be used for each type.
// From the db perspective, dbObjType is identified based on the db table, and ownerType is used directly
// in the ExternalIDs with OwnerTypeKey key.
type ObjectIDsType struct {
	dbTable         dbObjType
	ownerObjectType ownerType
	// externalIDKeys is a slice, because primary id for given ObjectIDsType will be built from the
	// ExternalIDKey values in given order
	externalIDKeys []ExternalIDKey
	externalIDsMap map[ExternalIDKey]bool
}

func (it ObjectIDsType) GetExternalIDKeys() []ExternalIDKey {
	return it.externalIDKeys
}

func (it ObjectIDsType) HasKey(key ExternalIDKey) bool {
	return it.externalIDsMap[key]
}

func (it ObjectIDsType) IsSameType(it2 *ObjectIDsType) bool {
	return it.ownerObjectType == it2.ownerObjectType && it.dbTable == it2.dbTable
}

const (
	// ExternalIDs keys that will be a part of a client index.
	// OwnerControllerKey and OwnerTypeKey define managing entity (a.k.a. owner) for given db object.
	// All the other ids are object-related.
	// PrimaryIDKey will be used a primary client index.
	// A combination of OwnerControllerKey, OwnerTypeKey, and ObjectNameKey will be used a secondary client index.
	// While owner-related keys together with PrimaryIDKey will always be present in the ExternalIDs,
	// ObjectNameKey may or may not be used, based on ObjectIDsType.
	OwnerControllerKey ExternalIDKey = types.OvnK8sPrefix + "/owner-controller"
	OwnerTypeKey       ExternalIDKey = types.OvnK8sPrefix + "/owner-type"
	// ObjectNameKey is a part of a secondary index, together with OwnerControllerKey and OwnerTypeKey
	// May be used by controllers to store e.g. namespace+name of the object.
	ObjectNameKey ExternalIDKey = types.OvnK8sPrefix + "/name"
	// PrimaryIDKey will be used as a primary index, that is unique for every db object,
	// and can be built based on the combination of all the other ids.
	PrimaryIDKey ExternalIDKey = types.PrimaryIDKey
)

// ObjectNameKey may be used as a secondary ID in the future. To ensure easy filtering for namespaced
// objects, you can combine namespace and name in that key. To unify this process (and potential parsing of the key)
// the following 2 functions exist:
// - BuildNamespaceNameKey to combine namespace and name into one key
// - ParseNamespaceNameKey to split the key back into namespace and name

func BuildNamespaceNameKey(namespace, name string) string {
	return namespace + ":" + name
}

func ParseNamespaceNameKey(key string) (namespace, name string, err error) {
	s := strings.Split(key, ":")
	if len(s) != 2 {
		err = fmt.Errorf("failed to parse namespaced name key %v, expected format <namespace>:<name>", key)
		return
	}
	return s[0], s[1], nil
}

// dbIDsMap is used to make sure the same ownerType is not defined twice for the same dbObjType to avoid conflicts.
// It is filled in newObjectIDsType when registering new ObjectIDsType
var dbIDsMap = map[dbObjType]map[ownerType]bool{}

func newObjectIDsType(dbTable dbObjType, ownerObjectType ownerType, keys []ExternalIDKey) *ObjectIDsType {
	if dbIDsMap[dbTable][ownerObjectType] {
		panic(fmt.Sprintf("ObjectIDsType for params %v %v is already registered", dbTable, ownerObjectType))
	}
	if dbIDsMap[dbTable] == nil {
		dbIDsMap[dbTable] = map[ownerType]bool{}
	}
	dbIDsMap[dbTable][ownerObjectType] = true
	keysMap := map[ExternalIDKey]bool{}
	for _, key := range keys {
		keysMap[key] = true
	}
	return &ObjectIDsType{dbTable, ownerObjectType, keys, keysMap}
}

// DbObjectIDs is a structure representing a set of db object ExternalIDs, used to identify
// an object in the db (as a primary/secondary index) or for a predicate search.
// DbObjectIDs consists of 3 parts:
// - idsType defines which IDs are used for a given object, as an ObjectIDsType,
// idsType.ownerObjectType will be written to ExternalIDs[OwnerTypeKey]
// - ownerControllerName defines who manages given object. It is required in case there are more than 1 controllers
// using the same idsType to make sure every controller only updates objects it owns.
// - objectIDs provide values for keys that are used by given idsType. To create a new object, all fields should be set.
// For predicate search, only some values that need to be matched may be set.
//
//	 dbIndex := NewDbObjectIDs(AddressSetEgressFirewallDNS, "DefaultController",
//			map[ExternalIDKey]string{
//				ObjectNameKey: "dns.name",
//				IPFamilyKey:   "ipv4"
//		})
//
//	uses AddressSetEgressFirewallDNS = newObjectIDsType(addressSet, EgressFirewallDNSOwnerType, []ExternalIDKey{
//			// dnsName
//			ObjectNameKey,
//			IPFamilyKey,
//	 })
//
// its dbIndex will be mapped to the following ExternalIDs
//
//	 {
//			"k8s.ovn.org/owner-controller": "DefaultController"
//			"k8s.ovn.org/owner-type": "EgressFirewallDNS" (value of EgressFirewallDNSOwnerType)
//			"k8s.ovn.org/name": "dns.name"
//			"k8s.ovn.org/ipFamily": "ipv4"
//			"k8s.ovn.org/id": "DefaultController:EgressFirewallDNS:dns.name:ipv4"
//	 }
type DbObjectIDs struct {
	idsType *ObjectIDsType
	// ownerControllerName specifies which controller owns the object.
	// Controller should only change objects it owns, make sure to always set this field.
	ownerControllerName string
	// objectIDs store values for keys required by given ObjectIDsType, and may be different for different ObjectIDsType.
	// These ids should uniquely identify db object with the same ownerControllerName and OwnerTypeKey.
	objectIDs map[ExternalIDKey]string
}

// NewDbObjectIDs is used to construct DbObjectIDs, idsType and controller are always required,
// objectIds may be empty, or half-filled for predicate search.
// objectIds keys that are not used by given idsType will cause panic.
func NewDbObjectIDs(idsType *ObjectIDsType, controller string, objectIds map[ExternalIDKey]string) *DbObjectIDs {
	if controller == "" {
		panic("NewDbObjectIDs failed: controller should not be empty")
	}
	externalIDKeys := idsType.GetExternalIDKeys()
	if externalIDKeys == nil {
		// can only happen if ObjectIDsType{} is passed
		panic(fmt.Sprintf("NewDbObjectIDs failed: ObjectIDsType %v should not be empty", idsType))
	}
	// only use values for keys from idsType
	for key := range objectIds {
		if !idsType.HasKey(key) {
			panic(fmt.Sprintf("NewDbObjectIDs failed: key %v is unknown", key))
		}
	}
	if objectIds == nil {
		objectIds = map[ExternalIDKey]string{}
	}
	objectIDs := &DbObjectIDs{
		idsType:             idsType,
		ownerControllerName: controller,
		objectIDs:           objectIds,
	}
	return objectIDs
}

// AddIDs creates new DbObjectIDs with the additional extraObjectIds.
// If at least one of extraObjectIds keys is not used by the objectIDs.idsType it will cause panic.
func (objectIDs *DbObjectIDs) AddIDs(extraObjectIds map[ExternalIDKey]string) *DbObjectIDs {
	ids := deepcopyMap(objectIDs.objectIDs)
	for key, value := range extraObjectIds {
		ids[key] = value
	}
	return &DbObjectIDs{objectIDs.idsType, objectIDs.ownerControllerName, ids}
}

func (objectIDs *DbObjectIDs) RemoveIDs(idsToDelete ...ExternalIDKey) *DbObjectIDs {
	ids := deepcopyMap(objectIDs.objectIDs)
	for _, keyToDel := range idsToDelete {
		delete(ids, keyToDel)
	}
	return &DbObjectIDs{objectIDs.idsType, objectIDs.ownerControllerName, ids}
}

func (objectIDs *DbObjectIDs) HasSameOwner(ownerController string, objectIDsType *ObjectIDsType) bool {
	return objectIDs.ownerControllerName == ownerController && objectIDs.idsType.IsSameType(objectIDsType)
}

func (objectIDs *DbObjectIDs) GetUnsetKeys() []ExternalIDKey {
	unsetKeys := []ExternalIDKey{}
	for _, key := range objectIDs.idsType.GetExternalIDKeys() {
		if _, ok := objectIDs.objectIDs[key]; !ok {
			unsetKeys = append(unsetKeys, key)
		}
	}
	return unsetKeys
}

// GetObjectID returns value from objectIDs.objectIDs map, and empty string for not found values.
// Usually objectIDs.objectIDs doesn't include PrimaryIDKey, OwnerTypeKey, and OwnerControllerKey.
func (objectIDs *DbObjectIDs) GetObjectID(key ExternalIDKey) string {
	return objectIDs.objectIDs[key]
}

// GetExternalIDs should only be used to build ids before creating the new db object.
// If at least one of required by DbObjectIDs.idsType keys is not present in the DbObjectIDs.objectIDs it will panic.
// GetExternalIDs returns a map of ids, that always includes keys
// - OwnerControllerKey
// - OwnerTypeKey
// - PrimaryIDKey
// and also all keys that are preset in objectIDs.objectIDs.
// PrimaryIDKey value consists of the following values joined with ":"
// - objectIDs.ownerControllerName
// - objectIDs.idsType.ownerObjectType
// - values from DbObjectIDs.objectIDs are added in order set in ObjectIDsType.externalIDKeys
func (objectIDs *DbObjectIDs) GetExternalIDs() map[string]string {
	return objectIDs.getExternalIDs(false)
}

func (objectIDs *DbObjectIDs) getExternalIDs(allowEmptyKeys bool) map[string]string {
	externalIDs := map[string]string{
		OwnerControllerKey.String(): objectIDs.ownerControllerName,
		OwnerTypeKey.String():       objectIDs.idsType.ownerObjectType,
	}
	for key, value := range objectIDs.objectIDs {
		externalIDs[key.String()] = value
	}
	primaryID, err := objectIDs.getUniqueID()
	if err == nil {
		// err == nil => primary id was properly built
		externalIDs[PrimaryIDKey.String()] = primaryID
	} else if !allowEmptyKeys {
		panic(fmt.Sprintf("Failed to build Primary ID for %+v: %v", objectIDs, err))
	}
	return externalIDs
}

// String returns a string that is similar to PrimaryIDKey value, but if some required keys are not present
// in the DbObjectIDs.objectIDs, they will be replaced with empty strings.
// String returns the representation of all the information set in DbObjectIDs.
func (objectIDs *DbObjectIDs) String() string {
	id := objectIDs.ownerControllerName + ":" + objectIDs.idsType.ownerObjectType
	for _, key := range objectIDs.idsType.GetExternalIDKeys() {
		id += ":" + objectIDs.objectIDs[key]
	}
	return id
}

func (objectIDs *DbObjectIDs) GetIDsType() *ObjectIDsType {
	return objectIDs.idsType
}

// getUniqueID returns primary id that is build based on objectIDs values.
// If at least one required key is missing, an error will be returned.
func (objectIDs *DbObjectIDs) getUniqueID() (string, error) {
	id := objectIDs.ownerControllerName + ":" + objectIDs.idsType.ownerObjectType
	for _, key := range objectIDs.idsType.GetExternalIDKeys() {
		value, ok := objectIDs.objectIDs[key]
		if !ok {
			return "", fmt.Errorf("key %v is required but not present", key)
		}
		id += ":" + value
	}
	return id, nil
}

// NewDbObjectIDsFromExternalIDs is used to parse object ExternalIDs, it sets DbObjectIDs.ownerControllerName based
// on OwnerControllerKey key, and verifies OwnerControllerKey value matches given objectIDsType.
// All the other ids from objectIDsType will be set to DbObjectIDs.objectIDs.
func NewDbObjectIDsFromExternalIDs(objectIDsType *ObjectIDsType, externalIDs map[string]string) (*DbObjectIDs, error) {
	if externalIDs[OwnerTypeKey.String()] != objectIDsType.ownerObjectType {
		return nil, fmt.Errorf("expected ExternalID %s to equal %s, got %s",
			OwnerTypeKey, objectIDsType.ownerObjectType, externalIDs[OwnerTypeKey.String()])
	}
	if externalIDs[OwnerControllerKey.String()] == "" {
		return nil, fmt.Errorf("required ExternalID %s is empty", OwnerControllerKey)
	}
	objIDs := map[ExternalIDKey]string{}
	for key, value := range externalIDs {
		if objectIDsType.HasKey(ExternalIDKey(key)) {
			objIDs[ExternalIDKey(key)] = value
		}
	}
	return NewDbObjectIDs(objectIDsType, externalIDs[OwnerControllerKey.String()], objIDs), nil
}

// hasExternalIDs interface should only include types that use new ExternalIDs from DbObjectIDs.
type hasExternalIDs interface {
	GetExternalIDs() map[string]string
}

// GetNoOwnerPredicate should only be used on initial sync when switching to new ExternalIDs.
// Otherwise, use GetPredicate with the specific OwnerControllerKey id.
func GetNoOwnerPredicate[T hasExternalIDs]() func(item T) bool {
	return func(item T) bool {
		return item.GetExternalIDs()[OwnerControllerKey.String()] == ""
	}
}

// GetPredicate returns a predicate to search for db obj of type nbdbT.
// Only non-empty ids will be matched (that always includes DbObjectIDs.OwnerTypeKey and DbObjectIDs.ownerControllerName),
// but the other IDs may be empty and will be ignored in the filtering, additional filter function f may be passed, or set
// to nil.
func GetPredicate[nbdbT hasExternalIDs](objectIDs *DbObjectIDs, f func(item nbdbT) bool) func(item nbdbT) bool {
	predicateIDs := objectIDs.getExternalIDs(true)
	if primaryID, ok := predicateIDs[PrimaryIDKey.String()]; ok {
		// when primary id is set, other ids are not required
		predicateIDs = map[string]string{PrimaryIDKey.String(): primaryID}
	}
	return func(item nbdbT) bool {
		dbExternalIDs := item.GetExternalIDs()
		for predKey, predValue := range predicateIDs {
			if dbExternalIDs[predKey] != predValue {
				return false
			}
		}
		return f == nil || f(item)
	}
}

func deepcopyMap(m map[ExternalIDKey]string) map[ExternalIDKey]string {
	result := map[ExternalIDKey]string{}
	for key, value := range m {
		result[key] = value
	}
	return result
}
