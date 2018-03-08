package ovn

import (
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"hash/fnv"
	"os/exec"
	"strconv"
	"strings"
)

// hash the provided input to make it a valid addressSet name.
func hashedAddressSet(s string) string {
	h := fnv.New64a()
	_, err := h.Write([]byte(s))
	if err != nil {
		logrus.Errorf("failed to hash %s", s)
	}
	hashString := strconv.FormatUint(h.Sum64(), 10)
	return fmt.Sprintf("a%s", hashString)
}

// forEachAddressSetUnhashedName will pass the unhashedName, namespaceName and
// the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Each unhashed name for an addressSet can be of the form
// namespaceName.suffix1.suffix2. .suffixN)
func (oc *Controller) forEachAddressSetUnhashedName(iteratorFn func(
	string, string, string)) error {
	output, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=external_ids", "find", "address_set").Output()
	if err != nil {
		logrus.Errorf("Error in obtaining list of address sets from OVN: %v",
			err)
		return err
	}
	for _, addrSet := range strings.Fields(string(output)) {
		if !strings.HasPrefix(addrSet, "name=") {
			continue
		}
		addrSetName := addrSet[5:]
		names := strings.Split(addrSetName, ".")
		addrSetNamespace := names[0]
		nameSuffix := ""
		if len(names) >= 2 {
			nameSuffix = names[1]
		}
		iteratorFn(addrSetName, addrSetNamespace, nameSuffix)
	}
	return nil
}

func (oc *Controller) setAddressSet(hashName string, addresses []string) {
	logrus.Debugf("setAddressSet for %s with %s", hashName, addresses)
	if len(addresses) == 0 {
		_, err := exec.Command(OvnNbctl, "clear", "address_set",
			hashName, "addresses").Output()
		if err != nil {
			logrus.Errorf("failed to clear address_set (%v)", err)
		}
		return
	}

	ips := strings.Join(addresses, " ")
	_, err := exec.Command(OvnNbctl, "set", "address_set",
		hashName, fmt.Sprintf("addresses=%s", ips)).Output()
	if err != nil {
		logrus.Errorf("failed to set address_set (%v)", err)
	}
}

func (oc *Controller) createAddressSet(name string, hashName string,
	addresses []string) {
	logrus.Debugf("createAddressSet with %s and %s", name, addresses)
	addressSet, err := exec.Command(OvnNbctl, "--data=bare", "--no-heading",
		"--columns=_uuid", "find", "address_set",
		fmt.Sprintf("name=%s", hashName)).Output()
	if err != nil {
		logrus.Errorf("find failed to get address set (%v)", err)
		return
	}

	// addressSet has already been created in the database and nothing to set.
	if string(addressSet) != "" && len(addresses) == 0 {
		_, err = exec.Command(OvnNbctl, "clear", "address_set",
			hashName, "addresses").Output()
		if err != nil {
			logrus.Errorf("failed to clear address_set (%v)", err)
		}
		return
	}

	ips := strings.Join(addresses, " ")

	// An addressSet has already been created. Just set addresses.
	if string(addressSet) != "" {
		// Set the addresses
		_, err = exec.Command(OvnNbctl, "set", "address_set",
			hashName, fmt.Sprintf("addresses=%s", ips)).Output()
		if err != nil {
			logrus.Errorf("failed to set address_set (%v)", err)
		}
		return
	}

	// addressSet has not been created yet. Create it.
	if len(addresses) == 0 {
		_, err = exec.Command(OvnNbctl, "create", "address_set",
			fmt.Sprintf("name=%s", hashName),
			fmt.Sprintf("external-ids:name=%s", name)).Output()
	} else {
		_, err = exec.Command(OvnNbctl, "create", "address_set",
			fmt.Sprintf("name=%s", hashName),
			fmt.Sprintf("external-ids:name=%s", name),
			fmt.Sprintf("addresses=%s", ips)).Output()
	}
	if err != nil {
		logrus.Errorf("failed to create address_set %s (%v)", name, err)
	}
}

func (oc *Controller) deleteAddressSet(hashName string) {
	logrus.Debugf("deleteAddressSet %s", hashName)

	_, err := exec.Command(OvnNbctl, "--if-exists", "destroy",
		"address_set", hashName).Output()
	if err != nil {
		logrus.Errorf("failed to destroy address set %s (%v)", hashName, err)
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
