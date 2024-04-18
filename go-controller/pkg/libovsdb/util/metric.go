package util

import (
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"k8s.io/klog/v2"
)

// GetACLCount returns the number of ACLs owned by idsType/controllerName
func GetACLCount(nbClient libovsdbclient.Client, idsType *ops.ObjectIDsType, controllerName string) int {
	predicateIDs := ops.NewDbObjectIDs(idsType, controllerName, nil)
	p := ops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	ACLs, err := ops.FindACLsWithPredicate(nbClient, p)
	if err != nil {
		klog.Warningf("Cannot find ACLs: %v; Resetting metrics...", err)
		return 0
	}
	return len(ACLs)
}

// GetAddressSetCount returns the number of AddressSets owned by idsType/controllerName
func GetAddressSetCount(nbClient libovsdbclient.Client, idsType *ops.ObjectIDsType, controllerName string) int {
	predicateIDs := ops.NewDbObjectIDs(idsType, controllerName, nil)
	p := ops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)
	ASes, err := ops.FindAddressSetsWithPredicate(nbClient, p)
	if err != nil {
		klog.Warningf("Cannot find AddressSets: %v; Resetting metrics...", err)
		return 0
	}
	return len(ASes)
}
