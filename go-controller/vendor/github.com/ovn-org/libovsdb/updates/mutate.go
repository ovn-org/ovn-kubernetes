package updates

import (
	"reflect"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func removeFromSlice(a, b reflect.Value) (reflect.Value, bool) {
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Interface() == b.Interface() {
			v := reflect.AppendSlice(a.Slice(0, i), a.Slice(i+1, a.Len()))
			return v, true
		}
	}
	return a, false
}

func insertToSlice(a, b reflect.Value) (reflect.Value, bool) {
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Interface() == b.Interface() {
			return a, false
		}
	}
	return reflect.Append(a, b), true
}

func mutate(current interface{}, mutator ovsdb.Mutator, value interface{}) (interface{}, interface{}) {
	switch current.(type) {
	case bool, string:
		return current, value
	}
	switch mutator {
	case ovsdb.MutateOperationInsert:
		// for insert, the delta will be the new value added
		return mutateInsert(current, value)
	case ovsdb.MutateOperationDelete:
		return mutateDelete(current, value)
	case ovsdb.MutateOperationAdd:
		// for add, the delta is the new value
		new := mutateAdd(current, value)
		return new, new
	case ovsdb.MutateOperationSubtract:
		// for subtract, the delta is the new value
		new := mutateSubtract(current, value)
		return new, new
	case ovsdb.MutateOperationMultiply:
		new := mutateMultiply(current, value)
		return new, new
	case ovsdb.MutateOperationDivide:
		new := mutateDivide(current, value)
		return new, new
	case ovsdb.MutateOperationModulo:
		new := mutateModulo(current, value)
		return new, new
	}
	return current, value
}

func mutateInsert(current, value interface{}) (interface{}, interface{}) {
	switch current.(type) {
	case int, float64:
		return current, current
	}
	vc := reflect.ValueOf(current)
	vv := reflect.ValueOf(value)
	if vc.Kind() == reflect.Slice && vc.Type() == reflect.SliceOf(vv.Type()) {
		v, ok := insertToSlice(vc, vv)
		var diff interface{}
		if ok {
			diff = value
		}
		return v.Interface(), diff
	}
	if !vc.IsValid() {
		if vv.IsValid() {
			return vv.Interface(), vv.Interface()
		}
		return nil, nil
	}
	if vc.Kind() == reflect.Slice && vv.Kind() == reflect.Slice {
		v := vc
		diff := reflect.Indirect(reflect.New(vv.Type()))
		for i := 0; i < vv.Len(); i++ {
			var ok bool
			v, ok = insertToSlice(v, vv.Index(i))
			if ok {
				diff = reflect.Append(diff, vv.Index(i))
			}
		}
		if diff.Len() > 0 {
			return v.Interface(), diff.Interface()
		}
		return v.Interface(), nil
	}
	if vc.Kind() == reflect.Map && vv.Kind() == reflect.Map {
		if vc.IsNil() && vv.Len() > 0 {
			return value, value
		}
		diff := reflect.MakeMap(vc.Type())
		iter := vv.MapRange()
		for iter.Next() {
			k := iter.Key()
			if !vc.MapIndex(k).IsValid() {
				vc.SetMapIndex(k, iter.Value())
				diff.SetMapIndex(k, iter.Value())
			}
		}
		if diff.Len() > 0 {
			return current, diff.Interface()
		}
		return current, nil
	}
	return current, nil
}

func mutateDelete(current, value interface{}) (interface{}, interface{}) {
	switch current.(type) {
	case int, float64:
		return current, nil
	}
	vc := reflect.ValueOf(current)
	vv := reflect.ValueOf(value)
	if vc.Kind() == reflect.Slice && vc.Type() == reflect.SliceOf(vv.Type()) {
		v, ok := removeFromSlice(vc, vv)
		diff := value
		if !ok {
			diff = nil
		}
		return v.Interface(), diff
	}
	if vc.Kind() == reflect.Slice && vv.Kind() == reflect.Slice {
		v := vc
		diff := reflect.Indirect(reflect.New(vv.Type()))
		for i := 0; i < vv.Len(); i++ {
			var ok bool
			v, ok = removeFromSlice(v, vv.Index(i))
			if ok {
				diff = reflect.Append(diff, vv.Index(i))
			}
		}
		if diff.Len() > 0 {
			return v.Interface(), diff.Interface()
		}
		return v.Interface(), nil
	}
	if vc.Kind() == reflect.Map && vv.Type() == reflect.SliceOf(vc.Type().Key()) {
		diff := reflect.MakeMap(vc.Type())
		for i := 0; i < vv.Len(); i++ {
			if vc.MapIndex(vv.Index(i)).IsValid() {
				diff.SetMapIndex(vv.Index(i), vc.MapIndex(vv.Index(i)))
				vc.SetMapIndex(vv.Index(i), reflect.Value{})
			}
		}
		if diff.Len() > 0 {
			return current, diff.Interface()
		}
		return current, nil
	}
	if vc.Kind() == reflect.Map && vv.Kind() == reflect.Map {
		diff := reflect.MakeMap(vc.Type())
		iter := vv.MapRange()
		for iter.Next() {
			vvk := iter.Key()
			vvv := iter.Value()
			vcv := vc.MapIndex(vvk)
			if vcv.IsValid() && reflect.DeepEqual(vcv.Interface(), vvv.Interface()) {
				diff.SetMapIndex(vvk, vcv)
				vc.SetMapIndex(vvk, reflect.Value{})
			}
		}
		if diff.Len() > 0 {
			return current, diff.Interface()
		}
		return current, nil
	}
	return current, nil
}

func mutateAdd(current, value interface{}) interface{} {
	if i, ok := current.(int); ok {
		v := value.(int)
		return i + v
	}
	if i, ok := current.(float64); ok {
		v := value.(float64)
		return i + v
	}
	if is, ok := current.([]int); ok {
		v := value.(int)
		for i, j := range is {
			is[i] = j + v
		}
		return is
	}
	if is, ok := current.([]float64); ok {
		v := value.(float64)
		for i, j := range is {
			is[i] = j + v
		}
		return is
	}
	return current
}

func mutateSubtract(current, value interface{}) interface{} {
	if i, ok := current.(int); ok {
		v := value.(int)
		return i - v
	}
	if i, ok := current.(float64); ok {
		v := value.(float64)
		return i - v
	}
	if is, ok := current.([]int); ok {
		v := value.(int)
		for i, j := range is {
			is[i] = j - v
		}
		return is
	}
	if is, ok := current.([]float64); ok {
		v := value.(float64)
		for i, j := range is {
			is[i] = j - v
		}
		return is
	}
	return current
}

func mutateMultiply(current, value interface{}) interface{} {
	if i, ok := current.(int); ok {
		v := value.(int)
		return i * v
	}
	if i, ok := current.(float64); ok {
		v := value.(float64)
		return i * v
	}
	if is, ok := current.([]int); ok {
		v := value.(int)
		for i, j := range is {
			is[i] = j * v
		}
		return is
	}
	if is, ok := current.([]float64); ok {
		v := value.(float64)
		for i, j := range is {
			is[i] = j * v
		}
		return is
	}
	return current
}

func mutateDivide(current, value interface{}) interface{} {
	if i, ok := current.(int); ok {
		v := value.(int)
		return i / v
	}
	if i, ok := current.(float64); ok {
		v := value.(float64)
		return i / v
	}
	if is, ok := current.([]int); ok {
		v := value.(int)
		for i, j := range is {
			is[i] = j / v
		}
		return is
	}
	if is, ok := current.([]float64); ok {
		v := value.(float64)
		for i, j := range is {
			is[i] = j / v
		}
		return is
	}
	return current
}

func mutateModulo(current, value interface{}) interface{} {
	if i, ok := current.(int); ok {
		v := value.(int)
		return i % v
	}
	if is, ok := current.([]int); ok {
		v := value.(int)
		for i, j := range is {
			is[i] = j % v
		}
		return is
	}
	return current
}
