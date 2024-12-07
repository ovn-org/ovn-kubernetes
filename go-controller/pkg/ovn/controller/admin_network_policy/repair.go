package adminnetworkpolicy

import (
	"errors"
	"fmt"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// repairAdminNetworkPolicies is called at startup and as the name suggests
// aims to repair the NBDB logical objects -> port groups, acls and address-sets
// that are created for the admin network policies in the cluster
// Logic:
// We fetch all the ANPs present in the cluster from the Lister
// We fetch PGs and AddressSets that are owned by ANP objectIDs based on
// externalIDs match from the NBDB. Using predicate search we check if
// the relevant ANP still exists for these objects and if not we delete them
func (c *Controller) repairAdminNetworkPolicies() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing admin network policies took %v", time.Since(start))
	}()
	c.Lock()
	defer c.Unlock()
	anps, err := c.anpLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("unable to list ANPs from the lister, err: %v", err)
	}
	existingANPs := map[string]*anpapi.AdminNetworkPolicy{}
	for _, anp := range anps {
		existingANPs[anp.Name] = anp
	}

	// Deal with PortGroup Repairs first - this will auto cleanup ACLs so no need to specifically delete ACLs
	// We grab all the port groups that belong to ANP controller using externalIDs
	// and compare the value with the name of existing ANPs. If no match is found
	// we delete that port group along with all the acls in it.
	// This is done for all networks in the cluster
	predicateIDs := libovsdbops.NewDbObjectIDsAcrossAllContollers(libovsdbops.PortGroupAdminNetworkPolicy, nil)
	p := libovsdbops.GetPredicateAcrossAllControllers[*nbdb.PortGroup](predicateIDs, func(pg *nbdb.PortGroup) bool {
		_, ok := existingANPs[pg.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
		return !ok // return if it doesn't exist in the cache
	})

	stalePGs, err := libovsdbops.FindPortGroupsWithPredicate(c.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to fetch port groups by predicate, err: %v", err)
	}
	if len(stalePGs) > 0 {
		klog.Infof("Deleting Stale PortGroups +%v", stalePGs)
		err = libovsdbops.DeletePortGroupsWithPredicate(c.nbClient, p)
		if err != nil {
			return fmt.Errorf("unable to delete stale port groups, err: %v", err)
		}
	}
	// Deal with Address-Sets Repairs
	// We grab all the AddressSets that belong to ANP controller using externalIDs
	// across all networks and compare with the existing ANPs. The ones that don't match
	// will be deleted from the DB.
	// NOTE: When we call syncAdminNetworkPolicy function after this for every ANP on startup,
	// the right Address-sets will be recreated.
	asPredicateIDs := libovsdbops.NewDbObjectIDsAcrossAllContollers(libovsdbops.AddressSetAdminNetworkPolicy, nil)
	asPredicateFunc := func(as *nbdb.AddressSet) bool {
		_, ok := existingANPs[as.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
		return !ok // if not present in cache then its stale
	}
	asPredicate := libovsdbops.GetPredicateAcrossAllControllers[*nbdb.AddressSet](asPredicateIDs, asPredicateFunc)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(c.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to remove stale ANP address sets, err: %v", err)
	}
	return nil
}

// repairBaselineAdminNetworkPolicies is called at startup and as the name suggests
// aims to repair the NBDB logical objects -> port groups, acls and address-sets
// that are created for the baseline admin network policy in the cluster
// Logic:
// We fetch the singleton BANPs present in the cluster from the Lister
// We fetch PGs and AddressSets that are owned by BANP objectIDs based on
// externalIDs match from the NBDB. Using predicate search we check if
// the relevant BANP still exists for these objects and if not we delete them
func (c *Controller) repairBaselineAdminNetworkPolicy() error {
	start := time.Now()
	defer func() {
		klog.Infof("Repairing baseline admin network policies took %v", time.Since(start))
	}()
	c.Lock()
	defer c.Unlock()

	banps, err := c.banpLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("unable to list BANPs from the lister, err: %v", err)
	}
	existingBANPs := map[string]*anpapi.BaselineAdminNetworkPolicy{}
	for _, banp := range banps {
		existingBANPs[banp.Name] = banp
	}

	// Deal with PortGroup Repairs first - this will auto cleanup ACLs so no need to specifically delete ACLs
	// We grab all the port groups that belong to BANP controller using externalIDs
	// and compare the value with the name of existing BANPs. If no match is found
	// we delete that port group along with all the acls in it.
	// This is done for all networks in the cluster
	predicateIDs := libovsdbops.NewDbObjectIDsAcrossAllContollers(libovsdbops.PortGroupBaselineAdminNetworkPolicy, nil)
	p := libovsdbops.GetPredicateAcrossAllControllers[*nbdb.PortGroup](predicateIDs, func(pg *nbdb.PortGroup) bool {
		_, ok := existingBANPs[pg.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
		return !ok // return if it doesn't exist in the cache
	})
	stalePGs, err := libovsdbops.FindPortGroupsWithPredicate(c.nbClient, p)
	if err != nil {
		return fmt.Errorf("unable to fetch port groups by predicate, err: %v", err)
	}
	if len(stalePGs) > 0 {
		klog.Infof("Deleting Stale PortGroups +%v", stalePGs)
		err = libovsdbops.DeletePortGroupsWithPredicate(c.nbClient, p)
		if err != nil && !errors.Is(err, libovsdbclient.ErrNotFound) {
			// if the ACL or PG is already gone, then nothing to do; unreferences ACLs should be autoremoved
			return fmt.Errorf("unable to delete stale port groups, err: %v", err)
		}
	}
	// Deal with Address-Sets Repairs
	// We grab all the AddressSets that belong to BANP controller using externalIDs
	// across all networks and compare with the existing ANPs. The ones that don't match
	// will be deleted from the DB.
	// NOTE: When we call syncBaselineAdminNetworkPolicy function after this for every BANP on startup,
	// the right Address-sets will be recreated.
	// Since we clean ACLs before Address-sets we should not run into any referential ingegrity violation
	asPredicateIDs := libovsdbops.NewDbObjectIDsAcrossAllContollers(libovsdbops.AddressSetBaselineAdminNetworkPolicy, nil)
	asPredicateFunc := func(as *nbdb.AddressSet) bool {
		_, ok := existingBANPs[as.ExternalIDs[libovsdbops.ObjectNameKey.String()]]
		return !ok // if not present in cache then its stale
	}
	asPredicate := libovsdbops.GetPredicateAcrossAllControllers[*nbdb.AddressSet](asPredicateIDs, asPredicateFunc)
	if err := libovsdbops.DeleteAddressSetsWithPredicate(c.nbClient, asPredicate); err != nil {
		return fmt.Errorf("failed to remove stale BANP address sets, err: %v", err)
	}
	return nil
}
