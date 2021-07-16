package server

import (
	"reflect"

	"github.com/ovn-org/libovsdb/ovsdb"
)

func mutate(current interface{}, mutator ovsdb.Mutator, value interface{}) interface{} {
	switch current.(type) {
	case bool, string:
		return current
	}
	switch mutator {
	case ovsdb.MutateOperationInsert:
		return mutateInsert(current, value)
	case ovsdb.MutateOperationDelete:
		return mutateDelete(current, value)
	case ovsdb.MutateOperationAdd:
		return mutateAdd(current, value)
	case ovsdb.MutateOperationSubtract:
		return mutateSubtract(current, value)
	case ovsdb.MutateOperationMultiply:
		return mutateMultiply(current, value)
	case ovsdb.MutateOperationDivide:
		return mutateDivide(current, value)
	case ovsdb.MutateOperationModulo:
		return mutateModulo(current, value)
	}
	return current
}

func mutateInsert(current, value interface{}) interface{} {
	switch current.(type) {
	case int, float64:
		return current
	}
	vc := reflect.ValueOf(current)
	vv := reflect.ValueOf(value)
	if vc.Kind() == reflect.Slice && vc.Type() == reflect.SliceOf(vv.Type()) {
		v := reflect.Append(vc, vv)
		return v.Interface()
	}
	if vc.Kind() == reflect.Slice && vv.Kind() == reflect.Slice {
		v := reflect.AppendSlice(vc, vv)
		return v.Interface()
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
