package ovn

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
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
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=external_ids", "find", "address_set")
	if err != nil {
		logrus.Errorf("Error in obtaining list of address sets from OVN: "+
			"stdout: %q, stderr: %q err: %v", output, stderr, err)
		return err
	}
	for _, addrSet := range strings.Fields(output) {
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
		_, stderr, err := util.RunOVNNbctl("clear", "address_set",
			hashName, "addresses")
		if err != nil {
			logrus.Errorf("failed to clear address_set, stderr: %q (%v)",
				stderr, err)
		}
		return
	}

	ips := strings.Join(addresses, " ")
	_, stderr, err := util.RunOVNNbctl("set", "address_set",
		hashName, fmt.Sprintf("addresses=%s", ips))
	if err != nil {
		logrus.Errorf("failed to set address_set, stderr: %q (%v)",
			stderr, err)
	}
}

func (oc *Controller) createAddressSet(name string, hashName string,
	addresses []string) {
	logrus.Debugf("createAddressSet with %s and %s", name, addresses)
	addressSet, stderr, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "address_set",
		fmt.Sprintf("name=%s", hashName))
	if err != nil {
		logrus.Errorf("find failed to get address set, stderr: %q (%v)",
			stderr, err)
		return
	}

	// addressSet has already been created in the database and nothing to set.
	if addressSet != "" && len(addresses) == 0 {
		_, stderr, err = util.RunOVNNbctl("clear", "address_set",
			hashName, "addresses")
		if err != nil {
			logrus.Errorf("failed to clear address_set, stderr: %q (%v)",
				stderr, err)
		}
		return
	}

	ips := strings.Join(addresses, " ")

	// An addressSet has already been created. Just set addresses.
	if addressSet != "" {
		// Set the addresses
		_, stderr, err = util.RunOVNNbctl("set", "address_set",
			hashName, fmt.Sprintf("addresses=%s", ips))
		if err != nil {
			logrus.Errorf("failed to set address_set, stderr: %q (%v)",
				stderr, err)
		}
		return
	}

	// addressSet has not been created yet. Create it.
	if len(addresses) == 0 {
		_, stderr, err = util.RunOVNNbctl("create", "address_set",
			fmt.Sprintf("name=%s", hashName),
			fmt.Sprintf("external-ids:name=%s", name))
	} else {
		_, stderr, err = util.RunOVNNbctl("create", "address_set",
			fmt.Sprintf("name=%s", hashName),
			fmt.Sprintf("external-ids:name=%s", name),
			fmt.Sprintf("addresses=%s", ips))
	}
	if err != nil {
		logrus.Errorf("failed to create address_set %s, stderr: %q (%v)",
			name, stderr, err)
	}
}

func (oc *Controller) deleteAddressSet(hashName string) {
	logrus.Debugf("deleteAddressSet %s", hashName)

	_, stderr, err := util.RunOVNNbctl("--if-exists", "destroy",
		"address_set", hashName)
	if err != nil {
		logrus.Errorf("failed to destroy address set %s, stderr: %q, (%v)",
			hashName, stderr, err)
		return
	}
}

func (oc *Controller) createPortGroup(name string,
	hashName string) (string, error) {
	logrus.Debugf("createPortGroup with %s", name)
	portGroup, stderr, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "port_group",
		fmt.Sprintf("name=%s", hashName))
	if err != nil {
		return "", fmt.Errorf("find failed to get port_group, stderr: %q (%v)",
			stderr, err)
	}

	if portGroup != "" {
		return portGroup, nil
	}

	portGroup, stderr, err = util.RunOVNNbctl("create", "port_group",
		fmt.Sprintf("name=%s", hashName),
		fmt.Sprintf("external-ids:name=%s", name))
	if err != nil {
		return "", fmt.Errorf("failed to create port_group %s, "+
			"stderr: %q (%v)", name, stderr, err)
	}

	return portGroup, nil
}

func (oc *Controller) deletePortGroup(hashName string) {
	logrus.Debugf("deletePortGroup %s", hashName)

	portGroup, stderr, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "port_group",
		fmt.Sprintf("name=%s", hashName))
	if err != nil {
		logrus.Errorf("find failed to get port_group, stderr: %q (%v)",
			stderr, err)
		return
	}

	if portGroup == "" {
		return
	}

	_, stderr, err = util.RunOVNNbctl("--if-exists", "destroy",
		"port_group", portGroup)
	if err != nil {
		logrus.Errorf("failed to destroy port_group %s, stderr: %q, (%v)",
			hashName, stderr, err)
		return
	}
}

func stringSliceMembership(slice []string, key string) bool {
	for _, val := range slice {
		if val == key {
			return true
		}
	}
	return false
}
