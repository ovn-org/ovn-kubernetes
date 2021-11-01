package server

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/cenkalti/rpc2"
	"github.com/google/uuid"
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
func (m *monitor) Send(update ovsdb.TableUpdates) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		m.filter(update)
	}
	if len(update) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), update}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update rpc: %v", err)
	}
}

// Send2 will send an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Send2(update ovsdb.TableUpdates2) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		m.filter2(update)
	}
	if len(update) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), update}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update2 rpc: %v", err)
	}
}

// Send3 will send an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Send3(id uuid.UUID, update ovsdb.TableUpdates2) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		m.filter2(update)
	}
	if len(update) == 0 {
		return
	}
	args := []interface{}{json.RawMessage([]byte(m.id)), id.String(), update}
	var reply interface{}
	err := m.client.Call("update2", args, &reply)
	if err != nil {
		log.Printf("client error handling update3 rpc: %v", err)
	}
}

func (m *monitor) filter(update ovsdb.TableUpdates) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		for table, u := range update {
			if _, ok := m.request[table]; !ok {
				delete(update, table)
				continue
			}
			for uuid, row := range u {
				switch {
				case row.Insert() && m.request[table].Select.Insert():
					fallthrough
				case row.Modify() && m.request[table].Select.Modify():
					fallthrough
				case row.Delete() && m.request[table].Select.Delete():
					if len(m.request[table].Columns) > 0 {
						cols := make(map[string]bool)
						for _, c := range m.request[table].Columns {
							cols[c] = true
						}
						if row.New != nil {
							new := *row.New
							for k := range new {
								if _, ok := cols[k]; !ok {
									delete(new, k)
								}
							}
							row.New = &new
							update[table][uuid] = row
						}
						if row.Old != nil {
							old := *row.Old
							for k := range old {
								if _, ok := cols[k]; !ok {
									delete(old, k)
								}
							}
							row.Old = &old
							update[table][uuid] = row
						}
					}
				default:
					delete(u, uuid)
				}
			}
		}
	}
}

func (m *monitor) filter2(update ovsdb.TableUpdates2) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		for table, u := range update {
			if _, ok := m.request[table]; !ok {
				delete(update, table)
				continue
			}
			for uuid, row := range u {
				switch {
				case row.Insert != nil && m.request[table].Select.Insert():
					fallthrough
				case row.Modify != nil && m.request[table].Select.Modify():
					fallthrough
				case row.Delete != nil && m.request[table].Select.Delete():
					if len(m.request[table].Columns) > 0 {
						cols := make(map[string]bool)
						for _, c := range m.request[table].Columns {
							cols[c] = true
						}
						if row.Insert != nil {
							new := *row.Insert
							for k := range new {
								if _, ok := cols[k]; !ok {
									delete(new, k)
								}
							}
							row.Insert = &new
							update[table][uuid] = row
						}
						if row.Modify != nil {
							new := *row.Modify
							for k := range new {
								if _, ok := cols[k]; !ok {
									delete(new, k)
								}
							}
							row.Modify = &new
							update[table][uuid] = row
						}
						if row.Delete != nil {
							old := *row.Delete
							for k := range old {
								if _, ok := cols[k]; !ok {
									delete(old, k)
								}
							}
							row.Delete = &old
							update[table][uuid] = row
						}
					}
				default:
					delete(u, uuid)
				}
			}
		}
	}
}