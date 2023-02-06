package client

import (
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
)

const emptyUUID = "00000000-0000-0000-0000-000000000000"

// Monitor represents a monitor
type Monitor struct {
	Method            string
	Tables            []TableMonitor
	Errors            []error
	LastTransactionID string
}

// newMonitor creates a new *Monitor with default values
func newMonitor() *Monitor {
	return &Monitor{
		Method:            ovsdb.ConditionalMonitorSinceRPC,
		Errors:            make([]error, 0),
		LastTransactionID: emptyUUID,
	}
}

// NewMonitor creates a new Monitor with the provided options
func (o *ovsdbClient) NewMonitor(opts ...MonitorOption) *Monitor {
	m := newMonitor()
	for _, opt := range opts {
		err := opt(o, m)
		if err != nil {
			m.Errors = append(m.Errors, err)
		}
	}
	return m
}

// MonitorOption adds Tables to a Monitor
type MonitorOption func(o *ovsdbClient, m *Monitor) error

// MonitorCookie is the struct we pass to correlate from updates back to their
// originating Monitor request.
type MonitorCookie struct {
	DatabaseName string `json:"databaseName"`
	ID           string `json:"id"`
}

func newMonitorCookie(dbName string) MonitorCookie {
	return MonitorCookie{
		DatabaseName: dbName,
		ID:           uuid.NewString(),
	}
}

// TableMonitor is a table to be monitored
type TableMonitor struct {
	// Table is the table to be monitored
	Table string
	// Conditions are the conditions under which the table should be monitored
	Conditions []ovsdb.Condition
	// Fields are the fields in the model to monitor
	// If none are supplied, all fields will be used
	Fields []string
}

func newTableMonitor(o *ovsdbClient, m model.Model, conditions []model.Condition, fields []interface{}) (*TableMonitor, error) {
	dbModel := o.primaryDB().model
	tableName := dbModel.FindTable(reflect.TypeOf(m))
	if tableName == "" {
		return nil, fmt.Errorf("object of type %s is not part of the ClientDBModel", reflect.TypeOf(m))
	}

	var columns []string
	var ovsdbConds []ovsdb.Condition

	if len(fields) == 0 && len(conditions) == 0 {
		return &TableMonitor{
			Table:      tableName,
			Conditions: ovsdbConds,
			Fields:     columns,
		}, nil
	}

	data, err := dbModel.NewModelInfo(m)
	if err != nil {
		return nil, fmt.Errorf("unable to obtain info from model %v: %v", m, err)
	}
	for _, f := range fields {
		column, err := data.ColumnByPtr(f)
		if err != nil {
			return nil, fmt.Errorf("unable to obtain column from model %v: %v", data, err)
		}
		columns = append(columns, column)
	}
	db := o.databases[o.primaryDBName]
	mmapper := db.model.Mapper
	for _, modelCond := range conditions {
		ovsdbCond, err := mmapper.NewCondition(data, modelCond.Field, modelCond.Function, modelCond.Value)
		if err != nil {
			return nil, fmt.Errorf("unable to convert condition %v: %v", modelCond, err)
		}
		ovsdbConds = append(ovsdbConds, *ovsdbCond)
	}
	return &TableMonitor{
		Table:      tableName,
		Conditions: ovsdbConds,
		Fields:     columns,
	}, nil
}

func WithTable(m model.Model, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		tableMonitor, err := newTableMonitor(o, m, []model.Condition{}, fields)
		if err != nil {
			return err
		}
		monitor.Tables = append(monitor.Tables, *tableMonitor)
		return nil
	}
}

func WithConditionalTable(m model.Model, conditions []model.Condition, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		tableMonitor, err := newTableMonitor(o, m, conditions, fields)
		if err != nil {
			return err
		}
		monitor.Tables = append(monitor.Tables, *tableMonitor)
		return nil
	}
}
