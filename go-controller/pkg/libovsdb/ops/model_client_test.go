package ops

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/onsi/gomega/types"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

var (
	adressSetTestName         = "test"
	adressSetTestUUID         = "test-uuid"
	adressSetTestAdress       = "test-adress"
	portGroupTestName         = "test-port-group"
	portGroupTestUUID         = "test-port-group-uuid"
	aclTestUUID               = "test-acl-uuid"
	logicalSwitchTestName     = "test-switch"
	logicalSwitchTestUUID     = "test-switch-uuid"
	logicalSwitchPortTestName = "test-switch-port"
	logicalSwitchPortTestUUID = "test-switch-port-uuid"
	logicalSwitchPortAddress  = "test-switch-port-address"
	logicalSwitchPortAddress2 = "test-switch-port-address2"
)

type OperationModelTestCase struct {
	name           string
	op             string
	generateOp     func() []operationModel
	interleaveOp   bool
	initialDB      []libovsdbtest.TestData
	expectedDB     []libovsdbtest.TestData
	expectedRes    [][]libovsdbtest.TestData
	expectedOpsErr error
	expectedTxnErr bool
}

func runTestCase(t *testing.T, tCase OperationModelTestCase) error {
	dbSetup := libovsdbtest.TestSetup{
		NBData: tCase.initialDB,
	}

	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	if err != nil {
		return err
	}
	t.Cleanup(cleanup.Cleanup)

	modelClient := newModelClient(nbClient)

	opModels := tCase.generateOp()

	ops := []ovsdb.Operation{}
	switch tCase.op {
	case "Lookup":
		err = modelClient.Lookup(opModels...)
	case "CreateOrUpdate":
		ops, err = modelClient.CreateOrUpdateOps(ops, opModels...)
	case "Delete":
		ops, err = modelClient.DeleteOps(ops, opModels...)
	default:
		return fmt.Errorf("test \"%s\": unknown op %s", tCase.name, tCase.op)
	}

	if err != tCase.expectedOpsErr {
		return fmt.Errorf("test \"%s\": unexpected error generating %s operations, got %v, expected %v", tCase.name, tCase.op, err, tCase.expectedOpsErr)
	}

	if tCase.interleaveOp {
		_, err = modelClient.CreateOrUpdate(opModels...)
		if err != nil {
			return fmt.Errorf("test \"%s\": unexpected error executing interleave operations: %v", tCase.name, err)
		}
	}

	_, err = TransactAndCheck(nbClient, ops)
	if err != nil && !tCase.expectedTxnErr {
		return fmt.Errorf("test \"%s\": unexpected error transacting operations: %v", tCase.name, err)
	}

	var matcher types.GomegaMatcher
	if tCase.expectedDB != nil {
		matcher = libovsdbtest.HaveData(tCase.expectedDB)
	} else {
		matcher = libovsdbtest.HaveData(tCase.initialDB)
	}
	success, err := matcher.Match(nbClient)
	if err != nil {
		return fmt.Errorf("test \"%s\": DB state did not match: %v", tCase.name, err)
	}
	if !success {
		return fmt.Errorf("test \"%s\": DB state did not match: %s", tCase.name, matcher.FailureMessage(nbClient))
	}

	var i int
	for _, opModel := range opModels {
		if opModel.ExistingResult != nil {
			if len(tCase.expectedRes) == i {
				break
			}
			actual := reflect.ValueOf(opModel.ExistingResult).Elem().Interface()
			matcher = libovsdbtest.ConsistOfIgnoringUUIDs(tCase.expectedRes[i]...)
			success, err := matcher.Match(actual)
			if err != nil {
				return fmt.Errorf("test \"%s\": existing result did not match: %v", tCase.name, err)
			}
			if !success {
				return fmt.Errorf("test \"%s\": existing result did not match: %s", tCase.name, matcher.FailureMessage(actual))
			}
			i++
		}
	}

	return nil
}

// This test uses an AddressSet for its assertion, mainly because AddressSet
// is specified as root and indexed by name in the OVN NB schema, which can
// evaluate all test cases correctly without having to specify a UUID.
func TestCreateOrUpdateForRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			name: "Test create non-existing item by model predicate specification",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
					},
				}
			},
			initialDB: []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
		},
		{
			name: "Test create non-existing item by model",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
		},
		{
			name: "Test update existing item by model predicate specification",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []operationModel{
					{
						Model:          &model,
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
		{
			name: "Test update existing item by model",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []operationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
		{
			name: "Test create/update of non-existing item by model",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []operationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
		{
			name: "Test setting of uuid of existing item to model when using model predicate",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				notTheUUIDWanted := buildNamedUUID()
				model := nbdb.AddressSet{
					UUID: notTheUUIDWanted,
					Name: adressSetTestName,
				}
				return []operationModel{
					{
						Model:          &model,
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						BulkOp:         false,
						ErrNotFound:    true,
						DoAfter: func() {
							if model.UUID == notTheUUIDWanted {
								t.Fatalf("Test setting of uuid of existing item to model: should have UUID %s modified to match %s",
									notTheUUIDWanted, adressSetTestUUID)
							}
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteForRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			name: "Test delete non-existing item by model predicate specification",
			op:   "Delete",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
					},
				}
			},
			initialDB:  []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{},
		},
		{
			name: "Test delete non-existing item by model specification",
			op:   "Delete",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
					},
				}
			},
			initialDB:  []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{},
		},
		{
			name: "Test delete existing item by model predicate specification",
			op:   "Delete",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
				},
			},
			expectedDB: []libovsdbtest.TestData{},
		},
		{
			name: "Test delete existing item by model specification",
			op:   "Delete",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
				},
			},
			expectedDB: []libovsdbtest.TestData{},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}
}

// This test uses a LogicalSwitch and LogicalSwitchPort for its assertion,
// mainly because LogicalSwitchPort is specified as non-root, indexed by name
// and referenced by LogicalSwitch in the OVN NB schema, which can evaluate all
// test cases correctly.
func TestCreateOrUpdateForNonRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			name: "Test create non-existing no-root by model predicate specification and parent model mutation",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model:          &m,
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
		{
			name: "Test create non-existing no-root by model predicate specification and non-existing parent model mutation",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model:          &m,
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
		{
			name: "Test create non-existing no-root by model predicate specification and parent model update",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model:          &m,
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
		{
			name: "Test create non-existing no-root by model and parent model mutate",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
		{
			name: "Test create non-existing no-root by model and parent model update",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
		{
			name: "Test update existing no-root by model update and parent model update",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				model := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
						DoAfter: func() {
							parentModel.Ports = []string{model.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					UUID:      logicalSwitchPortTestUUID,
					Addresses: []string{logicalSwitchPortAddress},
				},
			},
		},
		{
			name: "Test update existing no-root by model mutation and parent model update",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				pm := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						OnModelMutations: []interface{}{
							&m.Addresses,
						},
						DoAfter: func() {
							pm.Ports = []string{m.UUID}
						},
					},
					{
						Model: &pm,
						OnModelUpdates: []interface{}{
							&pm.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					UUID:      logicalSwitchPortTestUUID,
					Addresses: []string{logicalSwitchPortAddress},
				},
			},
		},
		{
			name: "Test update existing no-root by model mutation and update on the same row",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name:        logicalSwitchPortTestName,
					Addresses:   []string{logicalSwitchPortAddress2}, // to replace initial's column
					ExternalIDs: map[string]string{"two": "2"},       // to be inserted to initial's column
				}
				return []operationModel{
					{
						Model: &m,
						OnModelMutations: []interface{}{
							&m.ExternalIDs,
						},
						OnModelUpdates: []interface{}{
							&m.Addresses,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:        logicalSwitchPortTestName,
					UUID:        logicalSwitchPortTestUUID,
					Addresses:   []string{logicalSwitchPortAddress}, // to be replaced
					ExternalIDs: map[string]string{"one": "1"},      // to be insert mutated
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:        logicalSwitchPortTestName,
					UUID:        logicalSwitchPortTestUUID,
					Addresses:   []string{logicalSwitchPortAddress2},
					ExternalIDs: map[string]string{"one": "1", "two": "2"},
				},
			},
		},
		{
			name: "Test update non-existing no-root by model mutation and parent model mutation",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				pm := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						OnModelMutations: []interface{}{
							&m.Addresses,
						},
						DoAfter: func() {
							pm.Ports = []string{m.UUID}
						},
					},
					{
						Model: &pm,
						OnModelMutations: []interface{}{
							&pm.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					UUID:      logicalSwitchPortTestUUID,
					Addresses: []string{logicalSwitchPortAddress},
				},
			},
		},
		{
			name: "Test update existing no-root by model specification and parent model mutation without specifying direct ID",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						OnModelUpdates: []interface{}{
							&m.Addresses,
						},
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					UUID:      logicalSwitchPortTestUUID,
					Addresses: []string{logicalSwitchPortAddress},
				},
			},
		},
		{
			name: "Test no update of existing non-root object by model specification and parent model mutation without specifying direct ID",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
			},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteForNonRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			name: "Test delete non-existing no-root by model predicate specification and parent model mutation",
			op:   "Delete",
			generateOp: func() []operationModel {
				parentModel := nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					Ports: []string{logicalSwitchPortTestUUID},
				}
				return []operationModel{
					{
						Model:          &nbdb.LogicalSwitchPort{},
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			name: "Test delete existing no-root by model predicate specification and parent model mutation",
			op:   "Delete",
			generateOp: func() []operationModel {
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []operationModel{
					{
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						ExistingResult: &logicalSwitchPortRes,
						DoAfter: func() {
							parentModel.Ports = extractUUIDsFromModels(&logicalSwitchPortRes)
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			name: "Test delete existing no-root by model specification and parent model mutation without specifying direct ID",
			op:   "Delete",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
					UUID: logicalSwitchPortTestUUID,
				},
				&nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					UUID:  logicalSwitchTestUUID,
					Ports: []string{logicalSwitchPortTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			name: "Test delete existing non-root by model specification and parent model mutation without predicate",
			op:   "Delete",
			generateOp: func() []operationModel {
				parentModel := nbdb.PortGroup{
					Name: portGroupTestName,
				}
				aclRes := []nbdb.ACL{}
				return []operationModel{
					{
						ModelPredicate: func(acl *nbdb.ACL) bool { return acl.Action == nbdb.ACLActionAllow },
						ExistingResult: &aclRes,
						DoAfter: func() {
							parentModel.ACLs = extractUUIDsFromModels(&aclRes)
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.ACLs,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.ACL{
					Action: nbdb.ACLActionAllow,
					UUID:   aclTestUUID,
				},
				&nbdb.PortGroup{
					Name: portGroupTestName,
					UUID: portGroupTestUUID,
					ACLs: []string{aclTestUUID},
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.PortGroup{
					Name: portGroupTestName,
					UUID: portGroupTestUUID,
				},
			},
		},
		{
			name: "Test delete existing no-root by model specification and parent model mutation with empty ID slice",
			op:   "Delete",
			generateOp: func() []operationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []operationModel{
					{
						Model: &m,
						DoAfter: func() {
							parentModel.Ports = []string{m.UUID}
						},
					},
					{
						Model: &parentModel,
						OnModelMutations: []interface{}{
							&parentModel.Ports,
						},
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: logicalSwitchTestUUID,
					Name: logicalSwitchTestName,
				},
			},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: logicalSwitchTestUUID,
					Name: logicalSwitchTestName,
				},
			},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}

}

func TestLookup(t *testing.T) {
	lookupUUID := "b9998337-2498-4d1e-86e6-fc0417abb2f0"
	tt := []OperationModelTestCase{
		{
			name: "Test lookup by index over predicate",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
				},
			},
		},
		{
			name: "Test lookup by UUID over predicate",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							UUID: lookupUUID,
						},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      lookupUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
				},
			},
		},
		{
			name: "Test lookup by index not found error",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName + "-not-found",
						},
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedOpsErr: client.ErrNotFound,
		},
		{
			name: "Test lookup by index not found no error",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName + "-not-found",
						},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    false,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{{}},
		},
		{
			name: "Test lookup by predicate no indexes",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model:          &nbdb.AddressSet{},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return item.Name == adressSetTestName },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
				},
			},
		},
		{
			name: "Test lookup with index provided doesn't use predicate",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName + "-not-found",
						},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return item.Name == adressSetTestName },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedOpsErr: client.ErrNotFound,
		},
		{
			name: "Test lookup by predicate not found error",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedOpsErr: client.ErrNotFound,
		},
		{
			name: "Test lookup by predicate not found no error",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    false,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{{}},
		},
		{
			name: "Test lookup by index when bulk op",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return false },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
						BulkOp:         true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						UUID:      adressSetTestUUID,
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
				},
			},
		},
		{
			name: "Test lookup by predicate when bulk op",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model:          &nbdb.AddressSet{},
						ModelPredicate: func(item *nbdb.AddressSet) bool { return item.Name == adressSetTestName },
						ExistingResult: &[]*nbdb.AddressSet{},
						ErrNotFound:    true,
						BulkOp:         true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						UUID:      adressSetTestUUID,
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
				},
			},
		},
		{
			name: "Test lookup by predicate bulk op multiple results",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						ModelPredicate: func(item *nbdb.AddressSet) bool { return true },
						ExistingResult: &[]*nbdb.AddressSet{},
						BulkOp:         true,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID + "-2",
					Name:      adressSetTestName + "-2",
					Addresses: []string{adressSetTestAdress + "-2"},
				},
			},
			expectedRes: [][]libovsdbtest.TestData{
				{
					&nbdb.AddressSet{
						Name:      adressSetTestName,
						Addresses: []string{adressSetTestAdress},
					},
					&nbdb.AddressSet{
						UUID:      adressSetTestUUID + "-2",
						Name:      adressSetTestName + "-2",
						Addresses: []string{adressSetTestAdress + "-2"},
					},
				},
			},
		},
		{
			name: "Test lookup by predicate multiple results error no bulk op",
			op:   "Lookup",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						ModelPredicate: func(item *nbdb.AddressSet) bool { return true },
						ExistingResult: &[]*nbdb.AddressSet{},
						BulkOp:         false,
					},
				}
			},
			initialDB: []libovsdbtest.TestData{
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID,
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				},
				&nbdb.AddressSet{
					UUID:      adressSetTestUUID + "-2",
					Name:      adressSetTestName + "-2",
					Addresses: []string{adressSetTestAdress + "-2"},
				},
			},
			expectedOpsErr: errMultipleResults,
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildMutationsFromFields(t *testing.T) {
	invalidField := 5
	mapField := map[string]string{
		"key1": "value1",
		"key2": "",
		"":     "value3",
	}
	emptyMapField := map[string]string{}
	var nilMapField map[string]string
	sliceField := []string{"item1", "item2"}
	emptySliceField := []string{}
	invalidSliceField := []string{"item1", "", "item2"}
	var nilSliceField []string

	tt := []struct {
		name      string
		fields    []interface{}
		mutator   ovsdb.Mutator
		mutations []model.Mutation
		err       bool
	}{
		{
			name:   "build mutation over invalid type",
			fields: []interface{}{&invalidField},
			err:    true,
		},
		{
			name:    "build insert mutation over map",
			fields:  []interface{}{&mapField},
			mutator: ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{
				{
					Field:   &mapField,
					Mutator: ovsdb.MutateOperationDelete,
					Value:   []string{"", "key1", "key2"},
				},
				{
					Field:   &mapField,
					Mutator: ovsdb.MutateOperationInsert,
					Value:   mapField,
				},
			},
		},
		{
			name:    "build delete mutation over map",
			fields:  []interface{}{&mapField},
			mutator: ovsdb.MutateOperationDelete,
			mutations: []model.Mutation{
				{
					Field:   &mapField,
					Mutator: ovsdb.MutateOperationDelete,
					Value:   []string{"key2"},
				},
				{
					Field:   &mapField,
					Mutator: ovsdb.MutateOperationDelete,
					Value: map[string]string{
						"key1": "value1",
						"":     "value3",
					},
				},
			},
		},
		{
			name:      "build mutation over nil map",
			fields:    []interface{}{&nilMapField},
			mutator:   ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{},
		},
		{
			name:      "build mutation over empty map",
			fields:    []interface{}{&emptyMapField},
			mutator:   ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{},
		},
		{
			name:    "build insert mutation over slice",
			fields:  []interface{}{&sliceField},
			mutator: ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{
				{
					Field:   &sliceField,
					Mutator: ovsdb.MutateOperationInsert,
					Value:   sliceField,
				},
			},
		},
		{
			name:      "build mutation over nil slice",
			fields:    []interface{}{&nilSliceField},
			mutator:   ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{},
		},
		{
			name:      "build mutation over empty slice",
			fields:    []interface{}{&emptySliceField},
			mutator:   ovsdb.MutateOperationInsert,
			mutations: []model.Mutation{},
		},
		{
			name:    "build mutation over slice with empty value",
			fields:  []interface{}{&invalidSliceField},
			mutator: ovsdb.MutateOperationInsert,
			err:     true,
		},
	}

	for _, tCase := range tt {
		mutations, err := buildMutationsFromFields(tCase.fields, tCase.mutator)
		if err != nil && !tCase.err {
			t.Fatalf("%s: got unexpected error: %v", tCase.name, err)
		}
		for _, m := range mutations {
			if v, ok := m.Value.([]string); ok {
				sort.Strings(v)
			}
		}
		if !reflect.DeepEqual(mutations, tCase.mutations) {
			t.Fatalf("%s: unexpected mutations, got: %+v expected: %+v", tCase.name, mutations, tCase.mutations)
		}
	}
}

func TestWaitForDuplicates(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			name: "Test non-root model transaction fails when duplicate",
			op:   "CreateOrUpdate",
			generateOp: func() []operationModel {
				return []operationModel{
					{
						Model: &nbdb.LogicalSwitch{
							Name: logicalSwitchTestName,
						},
					},
				}
			},
			interleaveOp: true,
			initialDB:    []libovsdbtest.TestData{},
			expectedDB: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: logicalSwitchTestUUID,
					Name: logicalSwitchTestName,
				},
			},
			expectedTxnErr: true,
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(t, tCase); err != nil {
			t.Fatal(err)
		}
	}

}
