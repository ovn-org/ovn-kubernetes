package ops

import (
	"context"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

type chassisTemplateVarPredicate func(*nbdb.ChassisTemplateVar) bool

// ListTemplateVar looks up all chassis template variables.
func ListTemplateVar(nbClient libovsdbclient.Client) ([]*nbdb.ChassisTemplateVar, error) {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()

	templatesList := []*nbdb.ChassisTemplateVar{}
	err := nbClient.List(ctx, &templatesList)
	return templatesList, err
}

// CreateOrUpdateChassisTemplateVarOps creates or updates the provided
// 'template' variable and returns the corresponding ops.
func CreateOrUpdateChassisTemplateVarOps(nbClient libovsdbclient.Client,
	ops []libovsdb.Operation, template *nbdb.ChassisTemplateVar) ([]libovsdb.Operation, error) {

	modelClient := newModelClient(nbClient)
	return modelClient.CreateOrUpdateOps(ops, operationModel{
		Model:            template,
		OnModelMutations: []interface{}{&template.Variables},
		ErrNotFound:      false,
		BulkOp:           false,
	})
}

// deleteChassisTemplateVarVariablesOps removes the variables listed as
// keys of 'template.Variables' and returns the corresponding ops.
// It applies the mutation to all records that are selected by 'predicate'.
func deleteChassisTemplateVarVariablesOps(nbClient libovsdbclient.Client,
	ops []libovsdb.Operation, template *nbdb.ChassisTemplateVar,
	predicate chassisTemplateVarPredicate) ([]libovsdb.Operation, error) {

	deleteTemplate := &nbdb.ChassisTemplateVar{
		Chassis:   template.Chassis,
		Variables: map[string]string{},
	}
	for name := range template.Variables {
		deleteTemplate.Variables[name] = ""
	}
	modelClient := newModelClient(nbClient)
	return modelClient.DeleteOps(ops, operationModel{
		Model:            deleteTemplate,
		ModelPredicate:   predicate,
		OnModelMutations: []interface{}{&deleteTemplate.Variables},
		ErrNotFound:      false,
		BulkOp:           true,
	})
}

// DeleteChassisTemplateVarVariablesOps removes all variables listed as
// keys of 'template.Variables' from the record matching the same chassis
// as 'template'. It returns the corresponding ops.
func DeleteChassisTemplateVarVariablesOps(nbClient libovsdbclient.Client,
	ops []libovsdb.Operation, template *nbdb.ChassisTemplateVar) ([]libovsdb.Operation, error) {

	return deleteChassisTemplateVarVariablesOps(nbClient, ops, template, nil)
}

// DeleteAllChassisTemplateVarVariables removes the variables listed as
// in 'varNames' and commits the transaction to the database. It applies
// the mutation to all records that contain these variable names.
func DeleteAllChassisTemplateVarVariables(nbClient libovsdbclient.Client, varNames []string) error {
	deleteTemplateVar := &nbdb.ChassisTemplateVar{
		Variables: make(map[string]string, len(varNames)),
	}
	for _, name := range varNames {
		deleteTemplateVar.Variables[name] = ""
	}
	ops, err := deleteChassisTemplateVarVariablesOps(nbClient, nil, deleteTemplateVar,
		func(item *nbdb.ChassisTemplateVar) bool {
			for _, name := range varNames {
				if _, found := item.Variables[name]; found {
					return true
				}
			}
			return false
		})
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// DeleteChassisTemplateVar deletes all complete Chassis_Template_Var
// records matching 'templates'.
func DeleteChassisTemplateVar(nbClient libovsdbclient.Client, templates ...*nbdb.ChassisTemplateVar) error {
	opModels := make([]operationModel, 0, len(templates))
	for i := range templates {
		template := templates[i]
		opModels = append(opModels, operationModel{
			Model:       template,
			ErrNotFound: false,
			BulkOp:      false,
		})
	}
	m := newModelClient(nbClient)
	return m.Delete(opModels...)
}
