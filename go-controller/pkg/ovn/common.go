package ovn

import (
	"encoding/json"
	"fmt"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbtransaction"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/helpers"
	"github.com/sirupsen/logrus"
	"hash/fnv"
	"strconv"
	"strings"
)

// hash the provided input to make it a valid addressSet or portGroup name.
func hashForOVN(s string) string {
	h := fnv.New64a()
	_, err := h.Write([]byte(s))
	if err != nil {
		logrus.Errorf("failed to hash %s", s)
	}
	hashString := strconv.FormatUint(h.Sum64(), 10)
	return fmt.Sprintf("a%s", hashString)
}

// hash the provided input to make it a valid addressSet name.
func hashedAddressSet(s string) string {
	return hashForOVN(s)
}

// hash the provided input to make it a valid portGroup name.
func hashedPortGroup(s string) string {
	return hashForOVN(s)
}

// forEachAddressSetUnhashedName will pass the unhashedName, namespaceName and
// the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Each unhashed name for an addressSet can be of the form
// namespaceName.suffix1.suffix2. .suffixN)
func (oc *Controller) forEachAddressSetUnhashedName(iteratorFn func(
	string, string, string)) error {
	for _, addrSet := range oc.ovnNbCache.GetMap("Address_Set", "uuid") {
		externalIds := addrSet.(map[string]interface{})["external_ids"]
		if externalIds == nil {
			continue
		}
		addrSetName := externalIds.(map[string]interface{})["name"]
		if addrSetName == nil {
			continue
		}
		names := strings.Split(addrSetName.(string), ".")
		addrSetNamespace := names[0]
		nameSuffix := ""
		if len(names) >= 2 {
			nameSuffix = names[1]
		}
		iteratorFn(addrSetName.(string), addrSetNamespace, nameSuffix)
	}

	return nil
}

func (oc *Controller) setAddressSet(hashName string, addresses []string) {
	logrus.Debugf("setAddressSet for %s with %s", hashName, addresses)
	if len(addresses) == 0 {
		var err error
		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			txn.Update(dbtransaction.Update{
				Table: "Address_Set",
				Where: [][]interface{}{{"name", "==", hashName}},
				Row: map[string]interface{}{
					"addresses": dbtransaction.GetNil(),
				},
			})
			_, err, retry = txn.Commit()
		}
		if err != nil {
			logrus.Errorf("failed to clear address_set (%v)", err)
		}
		return
	}

	ips := strings.Join(addresses, " ")
	var err error
	retry := true
	for retry {
		txn := oc.ovnNBDB.Transaction("OVN_Northbound")
		txn.Update(dbtransaction.Update{
			Table: "Address_Set",
			Where: [][]interface{}{{"name", "==", hashName}},
			Row: map[string]interface{}{
				"addresses": ips,
			},
		})
		_, err, retry = txn.Commit()
	}
	if err != nil {
		logrus.Errorf("failed to set address_set (%v)", err)
	}
}

func (oc *Controller) createAddressSet(name string, hashName string,
	addresses []string) {
	logrus.Debugf("createAddressSet with %s and %s", name, addresses)
	addressSet := oc.ovnNbCache.GetMap("Address_Set", "name", hashName)["uuid"]

	// addressSet has already been created in the database and nothing to set.
	if addressSet != nil && addressSet.(string) != "" && len(addresses) == 0 {
		var err error
		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			txn.Update(dbtransaction.Update{
				Table: "Address_Set",
				Where: [][]interface{}{{"name", "==", hashName}},
				Row: map[string]interface{}{
					"addresses": dbtransaction.GetNil(),
				},
			})
			_, err, retry = txn.Commit()
		}
		if err != nil {
			logrus.Errorf("failed to clear address_set (%v)", err)
		}
		return
	}

	ips := strings.Join(addresses, " ")

	// An addressSet has already been created. Just set addresses.
	if addressSet != nil && addressSet.(string) != "" {
		// Set the addresses
		var err error
		retry := true
		for retry {
			txn := oc.ovnNBDB.Transaction("OVN_Northbound")
			txn.Update(dbtransaction.Update{
				Table: "Address_Set",
				Where: [][]interface{}{{"name", "==", hashName}},
				Row: map[string]interface{}{
					"addresses": ips,
				},
			})
			_, err, retry = txn.Commit()
		}
		if err != nil {
			logrus.Errorf("failed to set address_set (%v)", err)
		}
		return
	}

	// addressSet has not been created yet. Create it.
	var err error
	row := map[string]interface{}{
		"name": hashName,
		"external_ids": helpers.MakeOVSDBMap(map[string]interface{}{
			"name": name,
		}),
	}
	if len(addresses) != 0 {
		row["addresses"] = ips
	}
	retry := true
	for retry {
		txn := oc.ovnNBDB.Transaction("OVN_Northbound")
		txn.Insert(dbtransaction.Insert{
			Table: "Address_Set",
			Row:   row,
		})
		_, err, retry = txn.Commit()
	}
	if err != nil {
		logrus.Errorf("failed to create address_set %s (%v)", name, err)
	}
}

func (oc *Controller) deleteAddressSet(hashName string) {
	logrus.Debugf("deleteAddressSet %s", hashName)
	var err error
	retry := true
	for retry {
		txn := oc.ovnNBDB.Transaction("OVN_Northbound")
		txn.Delete(dbtransaction.Delete{
			Table: "Address_Set",
			Where: [][]interface{}{{"name", "==", hashName}},
		})
		_, err, retry = txn.Commit()
	}
	if err != nil {
		logrus.Errorf("failed to destroy address set %s (%v)", hashName, err)
		return
	}
}

func (oc *Controller) createPortGroup(name string,
	hashName string) (string, error) {
	logrus.Debugf("createPortGroup with %s", name)
	portGroup := oc.ovnNbCache.GetMap("Port_Group", "name", hashName)["uuid"]
	if portGroup != nil {
		return portGroup.(string), nil
	}

	var err error
	var resp dbtransaction.Transact
	retry := true
	for retry {
		txn := oc.ovnNBDB.Transaction("OVN_Northbound")
		txn.Insert(dbtransaction.Insert{
			Table: "Port_Group",
			Row: map[string]interface{}{
				"name": hashName,
				"external_ids": helpers.MakeOVSDBMap(map[string]interface{}{
					"name": name,
				}),
			},
		})
		resp, err, retry = txn.Commit()
	}
	if err != nil {
		logrus.Errorf("failed to create port_group %s (%v)", name, err)
	}

	return resp[0].UUID[1], nil
}

func (oc *Controller) deletePortGroup(hashName string) {
	logrus.Debugf("deletePortGroup %s", hashName)
	var err error
	retry := true
	for retry {
		txn := oc.ovnNBDB.Transaction("OVN_Northbound")
		txn.Delete(dbtransaction.Delete{
			Table: "Port_Group",
			Where: [][]interface{}{{"name", "==", hashName}},
		})
		_, err, retry = txn.Commit()
	}
	if err != nil {
		logrus.Errorf("failed to destroy port_group %s (%v)", hashName, err)
		return
	}
}

func (oc *Controller) getIPFromOvnAnnotation(ovnAnnotation string) string {
	if ovnAnnotation == "" {
		return ""
	}

	var ovnAnnotationMap map[string]string
	err := json.Unmarshal([]byte(ovnAnnotation), &ovnAnnotationMap)
	if err != nil {
		logrus.Errorf("Error in json unmarshaling ovn annotation "+
			"(%v)", err)
		return ""
	}

	ipAddressMask := strings.Split(ovnAnnotationMap["ip_address"], "/")
	if len(ipAddressMask) != 2 {
		logrus.Errorf("Error in splitting ip address")
		return ""
	}

	return ipAddressMask[0]
}

func (oc *Controller) getMacFromOvnAnnotation(ovnAnnotation string) string {
	if ovnAnnotation == "" {
		return ""
	}

	var ovnAnnotationMap map[string]string
	err := json.Unmarshal([]byte(ovnAnnotation), &ovnAnnotationMap)
	if err != nil {
		logrus.Errorf("Error in json unmarshaling ovn annotation "+
			"(%v)", err)
		return ""
	}

	return ovnAnnotationMap["mac_address"]
}

func stringSliceMembership(slice []string, key string) bool {
	for _, val := range slice {
		if val == key {
			return true
		}
	}
	return false
}
