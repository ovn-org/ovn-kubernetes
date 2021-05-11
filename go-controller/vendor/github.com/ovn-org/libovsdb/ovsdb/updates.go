package ovsdb

// TableUpdates is an object that maps from a table name to a
// TableUpdate
type TableUpdates map[string]TableUpdate

// AddTableUpdate adds a new TableUpdate to a TableUpdates
func (t TableUpdates) AddTableUpdate(table string, update TableUpdate) {
	if _, ok := t[table]; !ok {
		t[table] = update
	} else {
		for uuid, row := range update {
			t[table].AddRowUpdate(uuid, row)
		}
	}
}

func (t TableUpdates) Merge(update TableUpdates) {
	for k, v := range update {
		t.AddTableUpdate(k, v)
	}
}

// TableUpdate is an object that maps from the row's UUID to a
// RowUpdate
type TableUpdate map[string]*RowUpdate

func (t TableUpdate) AddRowUpdate(uuid string, update *RowUpdate) {
	if _, ok := t[uuid]; !ok {
		t[uuid] = update
	} else {
		t[uuid].Merge(update)
	}
}

// RowUpdate represents a row update according to RFC7047
type RowUpdate struct {
	New *Row `json:"new,omitempty"`
	Old *Row `json:"old,omitempty"`
}

// newRowUpdate creates a *RowUpdate using the provided rows
func newRowUpdate(old, new Row) *RowUpdate {
	r := &RowUpdate{}
	if old != nil {
		r.Old = &old
	} else {
		r.Old = nil
	}
	if new != nil {
		r.New = &new
	} else {
		r.New = nil
	}
	return r
}

// Insert returns true if this is an update for an insert operation
func (r RowUpdate) Insert() bool {
	return r.New != nil && r.Old == nil
}

// Modify returns true if this is an update for a modify operation
func (r RowUpdate) Modify() bool {
	return r.New != nil && r.Old != nil
}

// Delete returns true if this is an update for a delete operation
func (r RowUpdate) Delete() bool {
	return r.New == nil && r.Old != nil
}

func (r *RowUpdate) Merge(new *RowUpdate) {
	// old.Delete() cannot be true, as we can't modify a row after it was deleted
	if r.Delete() {
		return
	}
	// we should never get two insert events
	if r.Insert() && new.Insert() {
		return
	}
	if r.Insert() && new.Modify() {
		r.Old = nil
		r.New = new.New
		return
	}
	if r.Insert() && new.Delete() {
		r.Old = new.Old
		r.New = nil
		return
	}
	if r.Modify() && new.Modify() {
		r.New = new.New
		return
	}
	if r.Modify() && new.Delete() {
		r.Old = new.Old
		r.New = nil
		return
	}
}
