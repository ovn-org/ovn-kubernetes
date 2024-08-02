package ops

import (
	"context"
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

// GetACLName returns the ACL name if it has one otherwise returns
// an empty string.
func GetACLName(acl *nbdb.ACL) string {
	if acl.Name != nil {
		return *acl.Name
	}
	return ""
}

func getACLMutableFields(acl *nbdb.ACL) []interface{} {
	return []interface{}{&acl.Action, &acl.Direction, &acl.ExternalIDs, &acl.Log, &acl.Match, &acl.Meter,
		&acl.Name, &acl.Options, &acl.Priority, &acl.Severity, &acl.Tier, &acl.SampleNew, &acl.SampleEst}
}

type aclPredicate func(*nbdb.ACL) bool

// FindACLsWithPredicate looks up ACLs from the cache based on a given predicate
func FindACLsWithPredicate(nbClient libovsdbclient.Client, p aclPredicate) ([]*nbdb.ACL, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	acls := []*nbdb.ACL{}
	err := nbClient.WhereCache(p).List(ctx, &acls)
	return acls, err
}

func FindACLs(nbClient libovsdbclient.Client, acls []*nbdb.ACL) ([]*nbdb.ACL, error) {
	opModels := make([]operationModel, 0, len(acls))
	foundACLs := make([]*nbdb.ACL, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		found := []*nbdb.ACL{}
		opModel := operationModel{
			Model:          acl,
			ExistingResult: &found,
			ErrNotFound:    false,
			BulkOp:         false,
			DoAfter: func() {
				if len(found) > 0 {
					foundACLs = append(foundACLs, found[0])
				}
			},
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	err := modelClient.Lookup(opModels...)
	return foundACLs, err
}

// BuildACL builds an ACL with empty optional properties unset
func BuildACL(name string, direction nbdb.ACLDirection, priority int, match string, action nbdb.ACLAction, meter string,
	severity nbdb.ACLSeverity, log bool, externalIds map[string]string, options map[string]string, tier int) *nbdb.ACL {
	name = fmt.Sprintf("%.63s", name)

	var realName *string
	var realMeter *string
	var realSeverity *string
	if len(name) != 0 {
		realName = &name
	}
	if len(meter) != 0 {
		realMeter = &meter
	}
	if len(severity) != 0 {
		realSeverity = &severity
	}
	acl := &nbdb.ACL{
		Name:        realName,
		Direction:   direction,
		Match:       match,
		Action:      action,
		Priority:    priority,
		Severity:    realSeverity,
		Log:         log,
		Meter:       realMeter,
		ExternalIDs: externalIds,
		Options:     options,
		Tier:        tier,
	}

	return acl
}

func SetACLLogging(acl *nbdb.ACL, severity nbdb.ACLSeverity, log bool) {
	var realSeverity *string
	if len(severity) != 0 {
		realSeverity = &severity
	}
	acl.Severity = realSeverity
	acl.Log = log
}

// CreateOrUpdateACLsOps creates or updates the provided ACLs returning the
// corresponding ops
func CreateOrUpdateACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, samplingConfig *SamplingConfig, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		// ensure names are truncated (let's cover our bases from snippets that don't call BuildACL and call this directly)
		if acl.Name != nil {
			// node ACLs won't have names set
			*acl.Name = fmt.Sprintf("%.63s", *acl.Name)
		}
		opModels = addSample(samplingConfig, opModels, acl)
		opModel := operationModel{
			Model:          acl,
			OnModelUpdates: getACLMutableFields(acl),
			ErrNotFound:    false,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

func UpdateACLsOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		opModel := operationModel{
			Model:          acl,
			OnModelUpdates: getACLMutableFields(acl),
			ErrNotFound:    true,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}

// CreateOrUpdateACLs creates or updates the provided ACLs
func CreateOrUpdateACLs(nbClient libovsdbclient.Client, samplingConfig *SamplingConfig, acls ...*nbdb.ACL) error {
	ops, err := CreateOrUpdateACLsOps(nbClient, nil, samplingConfig, acls...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheckAndSetUUIDs(nbClient, acls, ops)
	return err
}

// UpdateACLsLoggingOps updates the log and severity on the provided ACLs and
// returns the corresponding ops
func UpdateACLsLoggingOps(nbClient libovsdbclient.Client, ops []libovsdb.Operation, acls ...*nbdb.ACL) ([]libovsdb.Operation, error) {
	opModels := make([]operationModel, 0, len(acls))
	for i := range acls {
		// can't use i in the predicate, for loop replaces it in-memory
		acl := acls[i]
		opModel := operationModel{
			Model:          acl,
			OnModelUpdates: []interface{}{&acl.Severity, &acl.Log},
			ErrNotFound:    true,
			BulkOp:         false,
		}
		opModels = append(opModels, opModel)
	}

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, opModels...)
}
