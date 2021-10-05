package server

import (
	"reflect"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func removeFromSlice(a, b reflect.Value) reflect.Value {
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Interface() == b.Interface() {
			v := reflect.AppendSlice(a.Slice(0, i), a.Slice(i+1, a.Len()))
			return v
		}
	}
	return a
}

func insertToSlice(a, b reflect.Value) reflect.Value {
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Interface() == b.Interface() {
			return a
		}
	}
	return reflect.Append(a, b)
}

func mutate(current interface{}, mutator ovsdb.Mutator, value interface{}) (interface{}, interface{}) {
	switch current.(type) {
	case bool, string:
		return current, value
	}
	switch mutator {
	case ovsdb.MutateOperationInsert:
		// for insert, the delta will be the new value added
		return mutateInsert(current, value), value
	case ovsdb.MutateOperationDelete:
		// for delete, the delta will be the value removed
		return mutateDelete(current, value), value
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

func mutateInsert(current, value interface{}) interface{} {
	switch current.(type) {
	case int, float64:
		return current
	}
	vc := reflect.ValueOf(current)
	vv := reflect.ValueOf(value)
	if vc.Kind() == reflect.Slice && vc.Type() == reflect.SliceOf(vv.Type()) {
		v := insertToSlice(vc, vv)
		return v.Interface()
	}
	if vc.Kind() == reflect.Slice && vv.Kind() == reflect.Slice {
		v := vc
		for i := 0; i < vv.Len(); i++ {
			v = insertToSlice(v, vv.Index(i))
		}
		return v.Interface()
	}
	if vc.Kind() == reflect.Map && vv.Kind() == reflect.Map {
		iter := vv.MapRange()
		for iter.Next() {
			k := iter.Key()
			if !vc.MapIndex(k).IsValid() {
				vc.SetMapIndex(k, iter.Value())
			}
		}
	}
	return current
}

func mutateDelete(current, value interface{}) interface{} {
	switch current.(type) {
	case int, float64:
		return current
	}
	vc := reflect.ValueOf(current)
	vv := reflect.ValueOf(value)
	if vc.Kind() == reflect.Slice && vc.Type() == reflect.SliceOf(vv.Type()) {
		v := removeFromSlice(vc, vv)
		return v.Interface()
	}
	if vc.Kind() == reflect.Slice && vv.Kind() == reflect.Slice {
		v := vc
		for i := 0; i < vv.Len(); i++ {
			v = removeFromSlice(v, vv.Index(i))
		}
		return v.Interface()
	}
	if vc.Kind() == reflect.Map && vv.Type() == reflect.SliceOf(vc.Type().Key()) {
		for i := 0; i < vv.Len(); i++ {
			vc.SetMapIndex(vv.Index(i), reflect.Value{})
		}
	}
	if vc.Kind() == reflect.Map && vv.Kind() == reflect.Map {
		iter := vv.MapRange()
		for iter.Next() {
			vvk := iter.Key()
			vvv := iter.Value()
			vcv := vc.MapIndex(vvk)
			if reflect.DeepEqual(vcv.Interface(), vvv.Interface()) {
				vc.SetMapIndex(vvk, reflect.Value{})
			}
		}
	}
	return current
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
