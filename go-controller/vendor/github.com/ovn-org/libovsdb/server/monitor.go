package server

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/cenkalti/rpc2"
	"github.com/google/uuid"
	"github.com/ovn-org/libovsdb/database"
	"github.com/ovn-org/libovsdb/ovsdb"
)

// connectionMonitors maps a connection to a map or monitors
type connectionMonitors struct {
	monitors map[string]*monitor
	mu       sync.RWMutex
}

func newConnectionMonitors() *connectionMonitors {
	return &connectionMonitors{
		monitors: make(map[string]*monitor),
		mu:       sync.RWMutex{},
	}
}

// monitor represents a connection to a client where db changes
// will be reflected
type monitor struct {
	id      string
	kind    monitorKind
	request map[string]*ovsdb.MonitorRequest
	client  *rpc2.Client
}

type monitorKind int

const (
	monitorKindOriginal monitorKind = iota
	monitorKindConditional
	monitorKindConditionalSince
)

func newMonitor(id string, request map[string]*ovsdb.MonitorRequest, client *rpc2.Client) *monitor {
	m := &monitor{
		id:      id,
		kind:    monitorKindOriginal,
		request: request,
		client:  client,
	}
	return m
}

func newConditionalMonitor(id string, request map[string]*ovsdb.MonitorRequest, client *rpc2.Client) *monitor {
	m := &monitor{
		id:      id,
		kind:    monitorKindConditional,
		request: request,
		client:  client,
	}
	return m
}

func newConditionalSinceMonitor(id string, request map[string]*ovsdb.MonitorRequest, client *rpc2.Client) *monitor {
	m := &monitor{
		id:      id,
		kind:    monitorKindConditional,
		request: request,
		client:  client,
	}
	return m
}

// Send will send an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Send(update database.Update) {
	// remove updates for tables that we aren't watching
	tu := m.filter(update)
	if len(tu) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), tu}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update rpc: %v", err)
	}
}

// Send2 will send an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Send2(update database.Update) {
	// remove updates for tables that we aren't watching
	tu := m.filter2(update)
	if len(tu) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), tu}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update2 rpc: %v", err)
	}
}

// Send3 will send an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Send3(id uuid.UUID, update database.Update) {
	// remove updates for tables that we aren't watching
	tu := m.filter2(update)
	if len(tu) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), id.String(), tu}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update3 rpc: %v", err)
	}
}

func filterColumns(row *ovsdb.Row, columns map[string]bool) *ovsdb.Row {
	if row == nil {
		return nil
	}
	new := make(ovsdb.Row, len(*row))
	for k, v := range *row {
		if _, ok := columns[k]; ok {
			new[k] = v
		}
	}
	return &new
}

func (m *monitor) filter(update database.Update) ovsdb.TableUpdates {
	// remove updates for tables that we aren't watching
	tables := update.GetUpdatedTables()
	tus := make(ovsdb.TableUpdates, len(tables))
	for _, table := range tables {
		if _, ok := m.request[table]; len(m.request) > 0 && !ok {
			// only remove updates for tables that were not requested if other
			// tables were requested, otherwise all tables are watched.
			continue
		}
		tu := ovsdb.TableUpdate{}
		cols := make(map[string]bool)
		cols["_uuid"] = true
		for _, c := range m.request[table].Columns {
			cols[c] = true
		}
		_ = update.ForEachRowUpdate(table, func(uuid string, ru2 ovsdb.RowUpdate2) error {
			ru := &ovsdb.RowUpdate{}
			ru.FromRowUpdate2(ru2)
			switch {
			case ru.Insert() && m.request[table].Select.Insert():
				fallthrough
			case ru.Modify() && m.request[table].Select.Modify():
				fallthrough
			case ru.Delete() && m.request[table].Select.Delete():
				if len(cols) == 0 {
					return nil
				}
				ru.New = filterColumns(ru.New, cols)
				ru.Old = filterColumns(ru.Old, cols)
				tu[uuid] = ru
			}
			return nil
		})
		tus[table] = tu
	}
	return tus
}

func (m *monitor) filter2(update database.Update) ovsdb.TableUpdates2 {
	// remove updates for tables that we aren't watching
	tables := update.GetUpdatedTables()
	tus2 := make(ovsdb.TableUpdates2, len(tables))
	for _, table := range tables {
		if _, ok := m.request[table]; len(m.request) > 0 && !ok {
			// only remove updates for tables that were not requested if other
			// tables were requested, otherwise all tables are watched.
			continue
		}
		tu2 := ovsdb.TableUpdate2{}
		cols := make(map[string]bool)
		cols["_uuid"] = true
		for _, c := range m.request[table].Columns {
			cols[c] = true
		}
		_ = update.ForEachRowUpdate(table, func(uuid string, ru2 ovsdb.RowUpdate2) error {
			switch {
			case ru2.Insert != nil && m.request[table].Select.Insert():
				fallthrough
			case ru2.Modify != nil && m.request[table].Select.Modify():
				fallthrough
			case ru2.Delete != nil && m.request[table].Select.Delete():
				if len(cols) == 0 {
					return nil
				}
				ru2.Insert = filterColumns(ru2.Insert, cols)
				ru2.Modify = filterColumns(ru2.Modify, cols)
				ru2.Delete = filterColumns(ru2.Delete, cols)
				tu2[uuid] = &ru2
			}
			return nil
		})
		tus2[table] = tu2
	}
	return tus2
}
