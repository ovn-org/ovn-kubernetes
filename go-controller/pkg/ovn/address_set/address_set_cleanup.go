package addressset

import (
	"k8s.io/klog/v2"
)

// NonDualStackAddressSetCleanup cleans addresses in old non dual stack spec.
// Assumes that for every address set <name>, if there exists an address set
// of <name_[v4|v6]>, address set <name> is no longer used and removes it.
// This method should only be called after ensuring address sets in old spec
// are no longer being referenced from any other object.
func NonDualStackAddressSetCleanup() error {
	// For each address set, track if it is in old non dual stack
	// spec and in new dual stack spec
	const old = 0
	const new = 1
	addressSets := map[string][2]bool{}
	err := forEachAddressSet(func(name string) {
		shortName := truncateSuffixFromAddressSet(name)
		spec, found := addressSets[shortName]
		if !found {
			spec = [2]bool{false, false}
		}
		if shortName == name {
			// This address set is in old non dual stack spec
			spec[old] = true
		} else {
			// This address set is in new dual stack spec
			spec[new] = true
		}
		addressSets[shortName] = spec
	})

	if err != nil {
		return err
	}

	for name, spec := range addressSets {
		// If we have an address set in both old and new spec,
		// we can safely remove the old spec.
		if spec[old] {
			if spec[new] {
				klog.Infof("Removing old spec address set %s", name)
				err := destroyAddressSet(name)
				if err != nil {
					return err
				}
				continue
			}
			klog.Warningf("Found an orphan old spec address set %s, ignoring", name)
		}
	}

	return nil
}
