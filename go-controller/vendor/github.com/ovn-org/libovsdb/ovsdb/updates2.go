package ovsdb

// TableUpdates2 is an object that maps from a table name to a
// TableUpdate2
type TableUpdates2 map[string]TableUpdate2

// TableUpdate2 is an object that maps from the row's UUID to a
// RowUpdate2
type TableUpdate2 map[string]*RowUpdate2

// RowUpdate2 represents a row update according to ovsdb-server.7
type RowUpdate2 struct {
	Initial *Row `json:"initial,omitempty"`
	Insert  *Row `json:"insert,omitempty"`
	Modify  *Row `json:"modify,omitempty"`
	Delete  *Row `json:"delete,omitempty"`
	Old     *Row `json:"-"`
	New     *Row `json:"-"`
}

// AddTableUpdate adds a new TableUpdate to a TableUpdates
func (t TableUpdates2) AddTableUpdate(table string, update TableUpdate2) {
	if _, ok := t[table]; !ok {
		t[table] = update
	} else {
		for uuid, row := range update {
			t[table].AddRowUpdate(uuid, row)
		}
	}
}

func (t TableUpdates2) Merge(update TableUpdates2) {
	for k, v := range update {
		t.AddTableUpdate(k, v)
	}
}

func (t TableUpdate2) AddRowUpdate(uuid string, update *RowUpdate2) {
	if _, ok := t[uuid]; !ok {
		t[uuid] = update
	} else {
		t[uuid].Merge(update)
	}
}

func (r *RowUpdate2) Merge(new *RowUpdate2) {
	// old.Delete() cannot be true, as we can't modify a row after it was deleted
	if r.Delete != nil {
		return
	}
	// we should never get two insert events
	if r.Insert != nil && new.Insert != nil {
		return
	}
	if r.Insert != nil && new.Modify != nil {
		r.Insert = new.Modify
		return
	}
	if r.Insert != nil && new.Delete != nil {
		r.Insert = nil
		r.Delete = new.Delete
		return
	}
	if r.Modify != nil && new.Modify != nil {
		currentRowData := *r.Modify
		newRowData := *new.Modify
		for k, v := range newRowData {
			if _, ok := currentRowData[k]; !ok {
				currentRowData[k] = v
			} else {
				switch v.(type) {
				case OvsSet:
					oSet := currentRowData[k].(OvsSet)
					newSet := v.(OvsSet)
					oSet.GoSet = append(oSet.GoSet, newSet.GoSet...)
					// copy new appended set back to currentRowData
					currentRowData[k] = oSet
				case OvsMap:
					oMap := currentRowData[k].(OvsMap)
					newMap := v.(OvsMap)
					for newK, newV := range newMap.GoMap {
						if _, ok := oMap.GoMap[newK]; !ok {
							oMap.GoMap[newK] = newV
						}
					}
					// note: unlike OvsSet, currentRowData for
					// goMap does not need to be copied back.
				}
			}
		}
		r.Modify = &currentRowData
		r.New = new.New
		return
	}
	if r.Modify != nil && new.Delete != nil {
		r.Modify = nil
		r.Delete = new.Delete
		return
	}
}
