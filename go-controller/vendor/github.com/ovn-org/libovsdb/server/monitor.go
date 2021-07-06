package server

import (
	"fmt"
	"log"
	"sync"

	"github.com/cenkalti/rpc2"
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
	request map[string]*ovsdb.MonitorRequest
	client  *rpc2.Client
	updates chan ovsdb.TableUpdates
	stopCh  chan struct{}
}

func newMonitor(id string, request map[string]*ovsdb.MonitorRequest, client *rpc2.Client) *monitor {
	m := &monitor{
		id:      id,
		request: request,
		client:  client,
		updates: make(chan ovsdb.TableUpdates),
		stopCh:  make(chan struct{}, 1),
	}
	go m.sendUpdates()
	return m
}

func (m *monitor) sendUpdates() {
	for {
		select {
		case update := <-m.updates:
			args := []interface{}{m.id, update}
			var reply interface{}
			err := m.client.Call("update", args, &reply)
			if err != nil {
				log.Printf("client error handling update rpc: %v", err)
			}
		case <-m.stopCh:
			return
		}
	}
}

// Enqueue will enqueue an update if it matches the tables and monitor select arguments
// we take the update by value (not reference) so we can mutate it in place before
// queuing it for dispatch
func (m *monitor) Enqueue(update ovsdb.TableUpdates) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		m.filter(update)
	}
	if len(update) == 0 {
		return
	}
	m.updates <- update
}

func (m *monitor) filter(update ovsdb.TableUpdates) {
	// remove updates for tables that we aren't watching
	if len(m.request) != 0 {
		for table, u := range update {
			if _, ok := m.request[table]; !ok {
				fmt.Println("dropping table update")
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
							update[table][uuid].New = &new
						}
						if row.Old != nil {
							old := *row.Old
							for k := range old {
								if _, ok := cols[k]; !ok {
									delete(old, k)
								}
							}
							update[table][uuid].Old = &old
						}
					}
				default:
					delete(u, uuid)
				}
			}
		}
	}
}
