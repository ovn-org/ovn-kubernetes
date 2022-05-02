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
	// Condition is the condition under which the table should be monitored
	Condition model.Condition
	// Fields are the fields in the model to monitor
	// If none are supplied, all fields will be used
	Fields []string
}

func fieldsAsStrings(dbModel model.DatabaseModel, m model.Model, fields []interface{}) ([]string, error) {
	var columns []string

	if len(fields) == 0 {
		return columns, nil
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
	return columns, nil
}

func WithTable(m model.Model, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		dbModel := o.primaryDB().model
		tableName := dbModel.FindTable(reflect.TypeOf(m))
		if tableName == "" {
			return fmt.Errorf("object of type %s is not part of the ClientDBModel", reflect.TypeOf(m))
		}
		fieldsStr, err := fieldsAsStrings(dbModel, m, fields)
		if err != nil {
			return err
		}
		tableMonitor := TableMonitor{
			Table:  tableName,
			Fields: fieldsStr,
		}
		monitor.Tables = append(monitor.Tables, tableMonitor)
		return nil
	}
}

func WithConditionalTable(m model.Model, condition model.Condition, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		dbModel := o.primaryDB().model
		tableName := dbModel.FindTable(reflect.TypeOf(m))
		if tableName == "" {
			return fmt.Errorf("object of type %s is not part of the ClientDBModel", reflect.TypeOf(m))
		}
		fieldsStr, err := fieldsAsStrings(dbModel, m, fields)
		if err != nil {
			return err
		}
		tableMonitor := TableMonitor{
			Table:     tableName,
			Condition: condition,
			Fields:    fieldsStr,
		}
		monitor.Tables = append(monitor.Tables, tableMonitor)
		return nil
	}
}
