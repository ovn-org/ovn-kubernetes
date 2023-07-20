package updates

import "reflect"

// difference between value 'a' and value 'b'.
// This difference is calculated as described in
// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
// The result is calculated in 'a' in-place and returned unless the
// difference is 'b' in which case 'b' is returned unmodified. Also returns a
// boolean indicating if there is an actual difference.
func difference(a, b interface{}) (interface{}, bool) {
	return mergeDifference(nil, a, b)
}

// applyDifference returns the result of applying difference 'd' to value 'v'
// along with a boolean indicating if 'v' was changed.
func applyDifference(v, d interface{}) (interface{}, bool) {
	if d == nil {
		return v, false
	}
	// difference can be applied with the same algorithm used to calculate it
	// f(x,f(x,y))=y
	result, changed := difference(v, d)
	dv := reflect.ValueOf(d)
	switch dv.Kind() {
	case reflect.Slice:
		fallthrough
	case reflect.Map:
		// but we need to tweak the interpretation of change for map and slices:
		// when there is no difference between the value and non-empty delta, it
		// actually means the value needs to be emptied so there is actually a
		// change
		if !changed && dv.Len() > 0 {
			return result, true
		}
		// there are no changes when delta is empty
		return result, changed && dv.Len() > 0
	}
	return result, changed
}

// mergeDifference, given an original value 'o' and two differences 'a' and 'b',
// returns a new equivalent difference that when applied on 'o' it would have
// the same result as applying 'a' and 'b' consecutively.
// If 'o' is nil, returns the difference between 'a' and 'b'.
// This difference is calculated as described in
// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
// The result is calculated in 'a' in-place and returned unless the result is
// 'b' in which case 'b' is returned unmodified. Also returns a boolean
// indicating if there is an actual difference.
func mergeDifference(o, a, b interface{}) (interface{}, bool) {
	kind := reflect.ValueOf(b).Kind()
	if kind == reflect.Invalid {
		kind = reflect.ValueOf(a).Kind()
	}
	switch kind {
	case reflect.Invalid:
		return nil, false
	case reflect.Slice:
		// set differences are transitive
		return setDifference(a, b)
	case reflect.Map:
		return mergeMapDifference(o, a, b)
	case reflect.Array:
		panic("Not implemented")
	default:
		return mergeAtomicDifference(o, a, b)
	}
}

// setDifference calculates the difference between set 'a' and set 'b'.
// This difference is calculated as described in
// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
// The result is calculated in 'a' in-place and returned unless the difference
// is 'b' in which case 'b' is returned unmodified. Also returns a boolean
// indicating if there is an actual difference.
func setDifference(a, b interface{}) (interface{}, bool) {
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)

	if !av.IsValid() && !bv.IsValid() {
		return nil, false
	} else if (!av.IsValid() || av.Len() == 0) && bv.IsValid() {
		return b, bv.Len() != 0
	} else if (!bv.IsValid() || bv.Len() == 0) && av.IsValid() {
		return a, av.Len() != 0
	}

	// From https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
	// The difference between two sets are all elements that only belong to one
	// of the sets.
	difference := make(map[interface{}]struct{}, bv.Len())
	for i := 0; i < bv.Len(); i++ {
		// supossedly we are working with comparable atomic types with no
		// pointers so we can use the values as map key
		difference[bv.Index(i).Interface()] = struct{}{}
	}
	j := av.Len()
	for i := 0; i < j; {
		vv := av.Index(i)
		vi := vv.Interface()
		if _, ok := difference[vi]; ok {
			// this value of 'a' is in 'b', so remove it from 'a'; to do that,
			// overwrite it with the last value and re-evaluate
			vv.Set(av.Index(j - 1))
			// decrease where the last 'a' value is at
			j--
			// remove from 'b' values
			delete(difference, vi)
		} else {
			// this value of 'a' is not in 'b', evaluate the next value
			i++
		}
	}
	// trim the slice to the actual values held
	av = av.Slice(0, j)
	for item := range difference {
		// this value of 'b' is not in 'a', so add it
		av = reflect.Append(av, reflect.ValueOf(item))
	}

	if av.Len() == 0 {
		return reflect.Zero(av.Type()).Interface(), false
	}

	return av.Interface(), true
}

// mergeMapDifference, given an original map 'o' and two differences 'a' and
// 'b', returns a new equivalent difference that when applied on 'o' it would
// have the same result as applying 'a' and 'b' consecutively.
// If 'o' is nil, returns the difference between 'a' and 'b'.
// This difference is calculated as described in
// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
// The result is calculated in 'a' in-place and returned unless the result is
// 'b' in which case 'b' is returned unmodified.
// Returns a boolean indicating if there is an actual difference.
func mergeMapDifference(o, a, b interface{}) (interface{}, bool) {
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)

	if !av.IsValid() && !bv.IsValid() {
		return nil, false
	} else if (!av.IsValid() || av.Len() == 0) && bv.IsValid() {
		return b, bv.Len() != 0
	} else if (!bv.IsValid() || bv.Len() == 0) && av.IsValid() {
		return a, av.Len() != 0
	}

	ov := reflect.ValueOf(o)
	if !ov.IsValid() {
		ov = reflect.Zero(av.Type())
	}

	// From
	// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
	// The difference between two maps are all key-value pairs whose keys
	// appears in only one of the maps, plus the key-value pairs whose keys
	// appear in both maps but with different values. For the latter elements,
	// <row> includes the value from the new column.

	// We can assume that difference is a transitive operation so we calculate
	// the difference between 'a' and 'b' but we need to handle exceptions when
	// the same key is present in all values.
	for i := bv.MapRange(); i.Next(); {
		kv := i.Key()
		bvv := i.Value()
		avv := av.MapIndex(kv)
		ovv := ov.MapIndex(kv)
		// supossedly we are working with comparable types with no pointers so
		// we can compare directly here
		switch {
		case ovv.IsValid() && avv.IsValid() && ovv.Interface() == bvv.Interface():
			// key is present in the three values
			// final result would restore key to the original value, delete from 'a'
			av.SetMapIndex(kv, reflect.Value{})
		case ovv.IsValid() && avv.IsValid() && avv.Interface() == bvv.Interface():
			// key is present in the three values
			// final result would remove key, set in 'a' with 'o' value
			av.SetMapIndex(kv, ovv)
		case avv.IsValid() && avv.Interface() == bvv.Interface():
			// key/value is in 'a' and 'b', delete from 'a'
			av.SetMapIndex(kv, reflect.Value{})
		default:
			// key/value in 'b' is not in 'a', set in 'a' with 'b' value
			av.SetMapIndex(kv, bvv)
		}
	}

	if av.Len() == 0 {
		return reflect.Zero(av.Type()).Interface(), false
	}

	return av.Interface(), true
}

// mergeAtomicDifference, given an original atomic value 'o' and two differences
// 'a' and 'b', returns a new equivalent difference that when applied on 'o' it
// would have the same result as applying 'a' and 'b' consecutively.
// If 'o' is nil, returns the difference between 'a' and 'b'.
// This difference is calculated as described in
// https://docs.openvswitch.org/en/latest/ref/ovsdb-server.7/#update2-notification
// Returns a boolean indicating if there is an actual difference.
func mergeAtomicDifference(o, a, b interface{}) (interface{}, bool) {
	if o != nil {
		return b, !reflect.DeepEqual(o, b)
	}
	return b, !reflect.DeepEqual(a, b)
}
