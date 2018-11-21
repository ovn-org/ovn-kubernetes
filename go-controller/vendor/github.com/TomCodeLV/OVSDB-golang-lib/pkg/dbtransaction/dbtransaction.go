package dbtransaction

import (
	"encoding/json"
	"errors"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbcache"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/helpers"
	"strconv"
)

type iOVSDB interface {
	Call(string, interface{}, *uint64) (json.RawMessage, error)
	Notify(string, interface{}) error
}

type UUID []string

type ActionResponse struct {
	Rows    []interface{} `json:"rows"`
	UUID    UUID
	Error   string
	Details string
}

type Transact []ActionResponse

// Transaction handle structure
type Transaction struct {
	OVSDB      iOVSDB
	Schema     string
	Actions    []interface{}
	Tables     map[string]string
	References map[string][]interface{}
	Counter    int
	id         uint64
}

func (txn *Transaction) Cancel() {
	args := []interface{}{txn.id}

	txn.OVSDB.Notify("cancel", args)
}

type Select struct {
	Table   string
	Columns []string
	Where   [][]interface{}
}

func (txn *Transaction) Select(s Select) {
	action := map[string]interface{}{}

	action["op"] = "select"
	action["table"] = s.Table
	if s.Where == nil {
		action["where"] = [][]interface{}{}
	} else {
		action["where"] = s.Where
	}
	if s.Columns == nil {
		action["columns"] = []string{}
	} else {
		action["columns"] = s.Columns
	}

	txn.Actions = append(txn.Actions, action)
}

type Insert struct {
	Table string
	Row   interface{}
}

func (txn *Transaction) Insert(i Insert) string {
	action := map[string]interface{}{}

	tempId := "row" + strconv.Itoa(txn.Counter)
	txn.Counter++

	action["uuid-name"] = tempId
	action["row"] = i.Row
	action["op"] = "insert"
	action["table"] = i.Table

	txn.Actions = append(txn.Actions, action)

	return tempId
}

type Update struct {
	Table    string
	Where    [][]interface{}
	Row      map[string]interface{}
	WaitRows []interface{}
}

func (txn *Transaction) Update(u Update) {
	if u.WaitRows != nil {
		columns := make([]string, len(u.Row))
		c := 0
		for column, _ := range u.Row {
			columns[c] = column
			c++
		}

		txn.Wait(Wait{
			Table:   u.Table,
			Where:   u.Where,
			Columns: columns,
			Until:   "==",
			Rows:    u.WaitRows,
		})
	}

	action := map[string]interface{}{}

	action["op"] = "update"
	action["table"] = u.Table
	if u.Where == nil {
		action["where"] = [][]interface{}{}
	} else {
		action["where"] = u.Where
	}
	action["row"] = u.Row

	txn.Actions = append(txn.Actions, action)
}

type Mutate struct {
	Table     string
	Where     [][]interface{}
	Mutations [][]interface{}
}

func (txn *Transaction) Mutate(m Mutate) {
	action := map[string]interface{}{}

	action["op"] = "mutate"
	action["table"] = m.Table
	if m.Where == nil {
		action["where"] = [][]interface{}{}
	} else {
		action["where"] = m.Where
	}
	action["mutations"] = m.Mutations

	txn.Actions = append(txn.Actions, action)
}

type Delete struct {
	Table string
	Where [][]interface{}
}

func (txn *Transaction) Delete(d Delete) {
	action := map[string]interface{}{}

	action["op"] = "delete"
	action["table"] = d.Table
	action["where"] = d.Where

	txn.Actions = append(txn.Actions, action)
}

type Wait struct {
	Timeout uint64
	Table   string
	Where   [][]interface{}
	Columns []string
	Until   string
	Rows    []interface{}
}

func (txn *Transaction) Wait(w Wait) {
	action := map[string]interface{}{}

	action["op"] = "wait"
	action["timeout"] = w.Timeout
	action["table"] = w.Table
	action["where"] = w.Where
	action["columns"] = w.Columns
	action["until"] = w.Until
	action["rows"] = w.Rows

	txn.Actions = append(txn.Actions, action)
}

// Commit stores all staged changes in DB. It manages references in main table
// automatically.
func (txn *Transaction) Commit() (Transact, error, bool) {
	args := []interface{}{txn.Schema}
	args = append(args, txn.Actions...)

	var id uint64
	response, err := txn.OVSDB.Call("transact", args, &id)
	// for transaction call can only return network errors, so retry is true
	if err != nil {
		return nil, err, true
	}
	txn.id = id

	var t Transact
	json.Unmarshal(response, &t)

	// handle OVSDB errors
	for _, res := range t {
		if res.Error != "" {
			if res.Error == "timed out" {
				return nil, errors.New(res.Error + ": " + res.Details), true
			} else {
				return nil, errors.New(res.Error + ": " + res.Details), false
			}
		}
	}

	// we have an error
	if len(t) > len(txn.Actions) {
		return nil, errors.New(t[len(t)-1].Error + ": " + t[len(t)-1].Details), false
	}

	return t, nil, false
}

// ==================
// HELPER FUNCTIONS
// ==================

type DeleteReferences struct {
	Table           string
	WhereId         string
	ReferenceColumn string
	DeleteIdsList   []string
	CurrentIdsList  []string // can be passed for performance reasons
	Wait            bool
	Cache           *dbcache.Cache
	LockChannel     chan int // used for locking for testing purposes
}

func (txn *Transaction) DeleteReferences(dr DeleteReferences) *Transaction {
	var bridgeIdList []string
	if dr.CurrentIdsList != nil {
		bridgeIdList = dr.CurrentIdsList
	} else {
		bridgeIdList = dr.Cache.GetKeys(dr.Table, "uuid", dr.WhereId, dr.ReferenceColumn)
	}

	newBridgeIdList := helpers.RemoveFromIdList(bridgeIdList, dr.DeleteIdsList)

	update := Update{
		Table: dr.Table,
		Where: [][]interface{}{{"_uuid", "==", []string{"uuid", dr.WhereId}}},
		Row: map[string]interface{}{
			dr.ReferenceColumn: helpers.MakeOVSDBSet(map[string]interface{}{
				"uuid": newBridgeIdList,
			}),
		},
	}

	if dr.Wait {
		update.WaitRows = []interface{}{map[string]interface{}{
			dr.ReferenceColumn: helpers.MakeOVSDBSet(map[string]interface{}{
				"uuid": bridgeIdList,
			}),
		}}
	}

	txn.Update(update)

	// lock for testing purposes
	if dr.LockChannel != nil {
		<-dr.LockChannel
	}

	return txn
}

type InsertReferences struct {
	Table           		string
	WhereId        			string
	ReferenceColumn			string
	InsertIdsList   		[]string
	InsertExistingIdsList   []string
	CurrentIdsList  		[]string
	Wait            		bool
	Cache           		*dbcache.Cache
}

func (txn *Transaction) InsertReferences(ir InsertReferences) *Transaction {
	var bridgeIdList []string
	if ir.CurrentIdsList != nil {
		bridgeIdList = ir.CurrentIdsList
	} else {
		bridgeIdList = ir.Cache.GetKeys(ir.Table, "uuid", ir.WhereId, ir.ReferenceColumn)
	}

	var newBridgeIdList = bridgeIdList
	if ir.InsertExistingIdsList != nil {
		newBridgeIdList = append(newBridgeIdList, ir.InsertExistingIdsList...)
	}

	update := Update{
		Table: ir.Table,
		Where: [][]interface{}{{"_uuid", "==", []string{"uuid", ir.WhereId}}},
		Row: map[string]interface{}{
			ir.ReferenceColumn: helpers.MakeOVSDBSet(map[string]interface{}{
				"uuid":       newBridgeIdList,
				"named-uuid": ir.InsertIdsList,
			}),
		},
	}

	if ir.Wait {
		update.WaitRows = []interface{}{map[string]interface{}{
			ir.ReferenceColumn: helpers.MakeOVSDBSet(map[string]interface{}{
				"uuid": bridgeIdList,
			}),
		}}
	}

	txn.Update(update)

	return txn
}

// Unset value in OVSDB is represented as empty set
func GetNil() []interface{} {
	return []interface{}{"set", []interface{}{}}
}
