package libovsdbops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
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
)

type OperationModelTestCase struct {
	name                     string
	generateCreateOrUpdateOp func() []OperationModel
	initialDB                []libovsdbtest.TestData
	expectedDB               []libovsdbtest.TestData
}

func runTestCase(tCase OperationModelTestCase, shouldDelete bool) error {
	stopChan := make(chan struct{})
	dbSetup := libovsdbtest.TestSetup{
		NBData: tCase.initialDB,
	}

	nbClient, _ := libovsdbtest.NewNBTestHarness(dbSetup, stopChan)
	modelClient := NewModelClient(nbClient)

	opModel := tCase.generateCreateOrUpdateOp()

	if shouldDelete {
		err := modelClient.Delete(opModel...)
		if err != nil {
			return fmt.Errorf("test: \"%s\" couldn't generate the Delete operations, err: %v", tCase.name, err)
		}
	} else {
		_, err := modelClient.CreateOrUpdate(opModel...)
		if err != nil {
			return fmt.Errorf("test: \"%s\" couldn't generate the CreateOrUpdate operations, err: %v", tCase.name, err)
		}
	}

	matcher := libovsdbtest.HaveData(tCase.expectedDB)
	success, err := matcher.Match(nbClient)
	if !success {
		return fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %s", tCase.name, matcher.FailureMessage(nbClient))
	}
	if err != nil {
		return fmt.Errorf("test: \"%s\" encountered error: %v", tCase.name, err)
	}

	close(stopChan)
	return nil
}

// This test uses an AddressSet for its assertion, mainly because AddressSet
// is specified as root and indexed by name in the OVN NB schema, which can
// evaluate all test cases correctly without having to specify a UUID.
func TestCreateOrUpdateForRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			"Test create non-existing item by model predicate specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
		},
		{
			"Test create non-existing item by model",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
		},
		{
			"Test update existing item by model predicate specification",
			func() []OperationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []OperationModel{
					{
						Model:          &model,
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
		{
			"Test update existing item by model",
			func() []OperationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []OperationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
					UUID: adressSetTestUUID,
				},
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
		{
			"Test create/update of non-existing item by model",
			func() []OperationModel {
				model := nbdb.AddressSet{
					Name:      adressSetTestName,
					Addresses: []string{adressSetTestAdress},
				}
				return []OperationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:      adressSetTestName,
					UUID:      adressSetTestUUID,
					Addresses: []string{adressSetTestAdress},
				},
			},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(tCase, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteForRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			"Test delete non-existing item by model predicate specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{},
		},
		{
			"Test delete non-existing item by model specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{},
		},
		{
			"Test delete existing item by model predicate specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ModelPredicate: func(a *nbdb.AddressSet) bool { return a.Name == adressSetTestName },
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
				},
			},
			[]libovsdbtest.TestData{},
		},
		{
			"Test delete existing item by model specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &nbdb.AddressSet{
							Name: adressSetTestName,
						},
						ExistingResult: &[]nbdb.AddressSet{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name: adressSetTestName,
				},
			},
			[]libovsdbtest.TestData{},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(tCase, true); err != nil {
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
			"Test create non-existing no-root by model predicate specification and parent model mutation",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model:          &m,
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationInsert, m)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
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
			"Test create non-existing no-root by model predicate specification and parent model update",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					Ports: []string{logicalSwitchPortTestUUID},
				}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						ExistingResult: &[]nbdb.LogicalSwitchPort{},
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
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
			"Test create non-existing no-root by model and parent model mutate",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model:          &m,
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationInsert, m)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
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
			"Test create non-existing no-root by model and parent model update",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					Ports: []string{logicalSwitchPortTestUUID},
				}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ExistingResult: &[]nbdb.LogicalSwitchPort{},
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
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
			"Test update existing no-root by model update and parent model update",
			func() []OperationModel {
				model := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					Ports: []string{logicalSwitchPortTestUUID},
				}
				return []OperationModel{
					{
						Model: &model,
						OnModelUpdates: []interface{}{
							&model.Addresses,
						},
						ExistingResult: &[]nbdb.LogicalSwitchPort{},
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelUpdates: []interface{}{
							&parentModel.Ports,
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
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
			"Test update existing no-root by model mutation and parent model update",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name: logicalSwitchPortTestName,
				}
				pm := nbdb.LogicalSwitch{
					Name:  logicalSwitchTestName,
					Ports: []string{logicalSwitchPortTestUUID},
				}
				return []OperationModel{
					{
						Model: &m,
						OnModelMutations: func() []model.Mutation {
							return []model.Mutation{
								{
									Field:   &m.Addresses,
									Mutator: ovsdb.MutateOperationInsert,
									Value:   []string{logicalSwitchPortAddress},
								},
							}
						},
						ExistingResult: &[]nbdb.LogicalSwitchPort{},
					},
					{
						Model:          &pm,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelUpdates: []interface{}{
							&pm.Ports,
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
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
			"Test update non-existing no-root by model mutation and parent model mutation",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				pm := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model: &m,
						OnModelMutations: func() []model.Mutation {
							return []model.Mutation{
								{
									Field:   &m.Addresses,
									Mutator: ovsdb.MutateOperationInsert,
									Value:   []string{logicalSwitchPortAddress},
								},
							}
						},
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &pm,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&pm.Ports, ovsdb.MutateOperationInsert, m)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
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
			"Test update existing no-root by model specification and parent model mutation without specifying direct ID",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model: &m,
						OnModelUpdates: []interface{}{
							&m.Addresses,
						},
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationInsert, logicalSwitchPortRes)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
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
			"Test no update of existing non-root object by model specification and parent model mutation without specifying direct ID",
			func() []OperationModel {
				m := nbdb.LogicalSwitchPort{
					Name:      logicalSwitchPortTestName,
					Addresses: []string{logicalSwitchPortAddress},
				}
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model:          &m,
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationInsert, logicalSwitchPortRes)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
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
		if err := runTestCase(tCase, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteForNonRootObjects(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			"Test delete non-existing no-root by model predicate specification and parent model mutation",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						ExistingResult: &[]nbdb.LogicalSwitchPort{},
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return []model.Mutation{
								{
									Field:   &parentModel.Ports,
									Mutator: ovsdb.MutateOperationDelete,
									Value:   []string{logicalSwitchPortTestUUID},
								},
							}
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			"Test delete existing no-root by model predicate specification and parent model mutation",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ModelPredicate: func(lsp *nbdb.LogicalSwitchPort) bool { return lsp.Name == logicalSwitchPortTestName },
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationDelete, logicalSwitchPortRes)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			"Test delete existing no-root by model specification and parent model mutation without specifying direct ID",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationDelete, logicalSwitchPortRes)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
					UUID: logicalSwitchTestUUID,
				},
			},
		},
		{
			"Test delete existing non-root by model specification and parent model mutation without predicate",
			func() []OperationModel {
				parentModel := nbdb.PortGroup{
					Name: portGroupTestName,
				}
				aclRes := []nbdb.ACL{}
				return []OperationModel{
					{
						Model: &nbdb.ACL{
							Action: nbdb.ACLActionAllow,
						},
						ModelPredicate: func(acl *nbdb.ACL) bool { return acl.Action == nbdb.ACLActionAllow },
						ExistingResult: &aclRes,
					},
					{
						Model: &parentModel,
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.ACLs, ovsdb.MutateOperationDelete, aclRes)
						},
						ExistingResult: &[]nbdb.PortGroup{},
					},
				}
			},
			[]libovsdbtest.TestData{
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
			[]libovsdbtest.TestData{
				&nbdb.PortGroup{
					Name: portGroupTestName,
					UUID: portGroupTestUUID,
				},
			},
		},
		{
			"Test delete existing no-root by model specification and parent model mutation with empty ID slice",
			func() []OperationModel {
				parentModel := nbdb.LogicalSwitch{
					Name: logicalSwitchTestName,
				}
				logicalSwitchPortRes := []nbdb.LogicalSwitchPort{}
				return []OperationModel{
					{
						Model: &nbdb.LogicalSwitchPort{
							Name: logicalSwitchPortTestName,
						},
						ExistingResult: &logicalSwitchPortRes,
					},
					{
						Model:          &parentModel,
						ModelPredicate: func(ls *nbdb.LogicalSwitch) bool { return ls.Name == logicalSwitchTestName },
						OnModelMutations: func() []model.Mutation {
							return OnReferentialModelMutation(&parentModel.Ports, ovsdb.MutateOperationDelete, logicalSwitchPortRes)
						},
						ExistingResult: &[]nbdb.LogicalSwitch{},
					},
				}
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: logicalSwitchTestUUID,
					Name: logicalSwitchTestName,
				},
			},
			[]libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: logicalSwitchTestUUID,
					Name: logicalSwitchTestName,
				},
			},
		},
	}

	for _, tCase := range tt {
		if err := runTestCase(tCase, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateWithAdHocClient(t *testing.T) {
	tt := []OperationModelTestCase{
		{
			"Test create non-existing item by model predicate specification",
			func() []OperationModel {
				return []OperationModel{
					{
						Model: &sbdb.Chassis{
							Name: "chassis-name",
						},
						ModelPredicate: func(c *sbdb.Chassis) bool { return c.Name == "chassis-name" },
						ExistingResult: &[]sbdb.Chassis{},
					},
				}
			},
			[]libovsdbtest.TestData{},
			[]libovsdbtest.TestData{
				&sbdb.Chassis{
					Name: "chassis-name",
					UUID: "chassis-uuid",
				},
			},
		},
	}

	for _, tCase := range tt {
		stopChan := make(chan struct{})
		dbSetup := libovsdbtest.TestSetup{
			SBData: tCase.initialDB,
		}

		nbClient, sbClient, _ := libovsdbtest.NewNBSBTestHarness(dbSetup, stopChan)
		modelClient := NewModelClient(nbClient)

		opModel := tCase.generateCreateOrUpdateOp()
		modelClient.WithClient(sbClient).CreateOrUpdate(opModel...)

		matcher := libovsdbtest.HaveData(tCase.expectedDB)
		success, err := matcher.Match(sbClient)
		if !success {
			t.Fatalf("test: \"%s\" didn't match expected with actual, err: %s", tCase.name, matcher.FailureMessage(sbClient))
		}
		if err != nil {
			t.Fatalf("test: \"%s\" encountered error: %v", tCase.name, err)
		}

		close(stopChan)
	}
}
