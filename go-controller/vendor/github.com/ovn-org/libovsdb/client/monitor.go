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
	Fields []interface{}
}

func WithTable(m model.Model, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		tableName := o.primaryDB().model.FindTable(reflect.TypeOf(m))
		if tableName == "" {
			return fmt.Errorf("object of type %s is not part of the ClientDBModel", reflect.TypeOf(m))
		}
		tableMonitor := TableMonitor{
			Table:  tableName,
			Fields: fields,
		}
		monitor.Tables = append(monitor.Tables, tableMonitor)
		return nil
	}
}

func WithConditionalTable(m model.Model, condition model.Condition, fields ...interface{}) MonitorOption {
	return func(o *ovsdbClient, monitor *Monitor) error {
		tableName := o.primaryDB().model.FindTable(reflect.TypeOf(m))
		if tableName == "" {
			return fmt.Errorf("object of type %s is not part of the ClientDBModel", reflect.TypeOf(m))
		}
		tableMonitor := TableMonitor{
			Table:     tableName,
			Condition: condition,
			Fields:    fields,
		}
		monitor.Tables = append(monitor.Tables, tableMonitor)
		return nil
	}
}
