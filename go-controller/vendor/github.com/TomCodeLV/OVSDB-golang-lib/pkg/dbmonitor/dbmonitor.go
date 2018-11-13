package dbmonitor

import (
	"encoding/json"
	"errors"
	"strconv"
)

type iOVSDB interface {
	Call(string, interface{}, *uint64) (json.RawMessage, error)
	AddCallBack(string, Callback)
	GetCounter() uint64
}

type RowUpdate struct {
	New map[string]interface{}	`json:"new"`
	Old map[string]interface{}	`json:"old"`
}

type Select struct {
	Initial bool	`json:"initial"`
	Insert bool		`json:"insert"`
	Delete bool		`json:"delete"`
	Modify bool		`json:"modify"`
}

type Table struct {
	Columns []string 	`json:"columns,omitempty"`
	Select Select 		`json:"select"`
}

type Callback func(json.RawMessage)

type Monitor struct {
	OVSDB iOVSDB
	Schema string
	MonitorRequests map[string]interface{}
	id string
}

func (monitor *Monitor) Register(tableName string, monitorTable interface{}) {
	monitor.MonitorRequests[tableName] = monitorTable
}

func (monitor *Monitor) Start (callback Callback) (json.RawMessage, error) {
	monitor.id = "monitor-" + strconv.FormatUint(monitor.OVSDB.GetCounter(), 10)
	args := []interface {}{
		monitor.Schema,
		monitor.id,
		monitor.MonitorRequests,
	}

	response, err := monitor.OVSDB.Call("monitor", args, nil)

	if err == nil {
		monitor.OVSDB.AddCallBack(monitor.id, callback)
	}

	return response, err
}

func (monitor *Monitor) Cancel() (interface{}, error) {
	response, err := monitor.OVSDB.Call("monitor_cancel", []string{ monitor.id }, nil)
	if err == nil {
		return response, nil
	} else {
		return nil, errors.New("cancel failed")
	}
}

