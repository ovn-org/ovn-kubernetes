package updates

import (
	"fmt"
	"reflect"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func merge(ts *ovsdb.TableSchema, a, b modelUpdate) (modelUpdate, error) {
	// handle model update
	switch {
	case b.old == nil && b.new == nil:
		// noop
	case a.old == nil && a.new == nil:
		// first op
		a.old = b.old
		a.new = b.new
	case a.new != nil && b.old != nil && b.new != nil:
		// update after an insert or an update
		a.new = b.new
	case b.old != nil && b.new == nil:
		// a final delete
		a.new = nil
	default:
		return modelUpdate{}, fmt.Errorf("sequence of updates not supported")
	}

	// handle row update
	ru2, err := mergeRowUpdate(ts, a.rowUpdate2, b.rowUpdate2)
	if err != nil {
		return modelUpdate{}, err
	}
	if ru2 == nil {
		return modelUpdate{}, nil
	}
	a.rowUpdate2 = ru2

	return a, nil
}

func mergeRowUpdate(ts *ovsdb.TableSchema, a, b *rowUpdate2) (*rowUpdate2, error) {
	switch {
	case b == nil:
		// noop
	case a == nil:
		// first op
		a = b
	case a.Insert != nil && b.Modify != nil:
		// update after an insert
		a.New = b.New
		a.Insert = b.New
	case a.Modify != nil && b.Modify != nil:
		// update after update
		a.New = b.New
		a.Modify = mergeModifyRow(ts, a.Old, a.Modify, b.Modify)
		if a.Modify == nil {
			// we merged two modifications that brought back the row to its
			// original value which is a no op
			a = nil
		}
	case a.Insert != nil && b.Delete != nil:
		// delete after insert
		a = nil
	case b.Delete != nil:
		// a final delete
		a.Initial = nil
		a.Insert = nil
		a.Modify = nil
		a.New = nil
		a.Delete = b.Delete
	default:
		return &rowUpdate2{}, fmt.Errorf("sequence of updates not supported")
	}
	return a, nil
}

// mergeModifyRow merges two modification rows 'a' and 'b' with respect an
// original row 'o'. Two modifications that restore the original value cancel
// each other and won't be included in the result. Returns nil if there are no
// resulting modifications.
func mergeModifyRow(ts *ovsdb.TableSchema, o, a, b *ovsdb.Row) *ovsdb.Row {
	original := *o
	aMod := *a
	bMod := *b
	for k, v := range bMod {
		if _, ok := aMod[k]; !ok {
			aMod[k] = v
			continue
		}

		var result interface{}
		var changed bool

		// handle maps or sets first
		switch v.(type) {
		// difference only supports set or map values that are comparable with
		// no pointers. This should be currently fine because the set or map
		// values should only be non pointer atomic types or the UUID struct.
		case ovsdb.OvsSet:
			aSet := aMod[k].(ovsdb.OvsSet)
			bSet := v.(ovsdb.OvsSet)
			// handle sets of multiple values, single value sets are handled as
			// atomic values
			if ts.Column(k).TypeObj.Max() != 1 {
				// set difference is a fully transitive operation so we dont
				// need to do anything special to merge two differences
				result, changed = setDifference(aSet.GoSet, bSet.GoSet)
				result = ovsdb.OvsSet{GoSet: result.([]interface{})}
			}
		case ovsdb.OvsMap:
			aMap := aMod[k].(ovsdb.OvsMap)
			bMap := v.(ovsdb.OvsMap)
			var originalMap ovsdb.OvsMap
			if v, ok := original[k]; ok {
				originalMap = v.(ovsdb.OvsMap)
			}
			// map difference is not transitive with respect to the original
			// value so we have to take the original value into account when
			// merging
			result, changed = mergeMapDifference(originalMap.GoMap, aMap.GoMap, bMap.GoMap)
			result = ovsdb.OvsMap{GoMap: result.(map[interface{}]interface{})}
		}

		// was neither a map nor a set
		if result == nil {
			// atomic difference is not transitive with respect to the original
			// value so we have to take the original value into account when
			// merging
			o := original[k]
			if o == nil {
				// assume zero value if original does not have the column
				o = reflect.Zero(reflect.TypeOf(v)).Interface()
			}
			if set, ok := o.(ovsdb.OvsSet); ok {
				// atomic optional values are cleared out with an empty set
				// if the original value was also cleared out, use an empty set
				// instead of a nil set so that mergeAtomicDifference notices
				// that we are returning to the original value
				if set.GoSet == nil {
					set.GoSet = []interface{}{}
				}
				o = set
			}
			result, changed = mergeAtomicDifference(o, aMod[k], v)
		}

		if !changed {
			delete(aMod, k)
			continue
		}
		aMod[k] = result
	}

	if len(aMod) == 0 {
		return nil
	}

	return a
}
