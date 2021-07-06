package ovn

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// hash the provided input to make it a valid portGroup name.
func hashedPortGroup(s string) string {
	return util.HashForOVN(s)
}

func createPortGroup(nbClient libovsdbclient.Client, name string, hashName string) (string, error) {
	klog.V(5).Infof("createPortGroup with %s", name)
	externalIds := map[string]string{"name": name}

	pg := nbdb.PortGroup{
		Name:        hashName,
		ExternalIDs: externalIds,
	}
	ops, err := nbClient.Create(&pg)
	if err != nil {
		return "", fmt.Errorf("Error creating port group %+v: %v", pg, err)
	}

	results, err := nbClient.Transact(ops...)
	if err != nil {
		return "", fmt.Errorf("Error creating port group with ops %+v: %v", ops, err)
	}

	opErrors, err := libovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return "", fmt.Errorf("Error creating port group with results %+v and errors %+v: %v", results, opErrors, err)
	}

	uuid := results[0].UUID.GoUUID
	return uuid, nil
}

func deletePortGroup(nbClient libovsdbclient.Client, hashName string) error {
	klog.V(5).Infof("deletePortGroup %s", hashName)

	pg := nbdb.PortGroup{
		Name: hashName,
	}
	ops, err := nbClient.Where(&pg).Delete()
	if err != nil {
		return fmt.Errorf("Error deleting port group %+v: %v", pg, err)
	}

	results, err := nbClient.Transact(ops...)
	if err != nil {
		return fmt.Errorf("Error deleting port group with ops %+v: %v", ops, err)
	}

	opErrors, err := libovsdb.CheckOperationResults(results, ops)
	if err != nil {
		return fmt.Errorf("Error deleting port group with results %+v and errors %+v: %v", results, opErrors, err)
	}

	return nil
}
