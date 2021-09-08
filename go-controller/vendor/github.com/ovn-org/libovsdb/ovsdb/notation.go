package ovsdb

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const (
	// OperationInsert is an insert operation
	OperationInsert = "insert"
	// OperationSelect is a select operation
	OperationSelect = "select"
	// OperationUpdate is an update operation
	OperationUpdate = "update"
	// OperationMutate is a mutate operation
	OperationMutate = "mutate"
	// OperationDelete is a delete operation
	OperationDelete = "delete"
	// OperationWait is a wait operation
	OperationWait = "wait"
	// OperationCommit is a commit operation
	OperationCommit = "commit"
	// OperationAbort is an abort operation
	OperationAbort = "abort"
	// OperationComment is a comment operation
	OperationComment = "comment"
	// OperationAssert is an assert operation
	OperationAssert = "assert"
)

// Operation represents an operation according to RFC7047 section 5.2
type Operation struct {
	Op        string      `json:"op"`
	Table     string      `json:"table"`
	Row       Row         `json:"row,omitempty"`
	Rows      []Row       `json:"rows,omitempty"`
	Columns   []string    `json:"columns,omitempty"`
	Mutations []Mutation  `json:"mutations,omitempty"`
	Timeout   int         `json:"timeout,omitempty"`
	Where     []Condition `json:"where,omitempty"`
	Until     string      `json:"until,omitempty"`
	Durable   *bool       `json:"durable,omitempty"`
	Comment   *string     `json:"comment,omitempty"`
	Lock      *string     `json:"lock,omitempty"`
	UUIDName  string      `json:"uuid-name,omitempty"`
}

// MarshalJSON marshalls 'Operation' to a byte array
// For 'select' operations, we don't omit the 'Where' field
// to allow selecting all rows of a table
func (o Operation) MarshalJSON() ([]byte, error) {
	type OpAlias Operation
	switch o.Op {
	case "select":
		where := o.Where
		if where == nil {
			where = make([]Condition, 0)
		}
		return json.Marshal(&struct {
			Where []Condition `json:"where"`
			OpAlias
		}{
			Where:   where,
			OpAlias: (OpAlias)(o),
		})
	default:
		return json.Marshal(&struct {
			OpAlias
		}{
			OpAlias: (OpAlias)(o),
		})
	}
}

// MonitorRequests represents a group of monitor requests according to RFC7047
// We cannot use MonitorRequests by inlining the MonitorRequest Map structure till GoLang issue #6213 makes it.
// The only option is to go with raw map[string]interface{} option :-( that sucks !
// Refer to client.go : MonitorAll() function for more details
type MonitorRequests struct {
	Requests map[string]MonitorRequest `json:"requests"`
}

// MonitorRequest represents a monitor request according to RFC7047
type MonitorRequest struct {
	Columns []string       `json:"columns,omitempty"`
	Select  *MonitorSelect `json:"select,omitempty"`
}

// TransactResponse represents the response to a Transact Operation
type TransactResponse struct {
	Result []OperationResult `json:"result"`
	Error  string            `json:"error"`
}

// OperationResult is the result of an Operation
type OperationResult struct {
	Count   int    `json:"count,omitempty"`
	Error   string `json:"error,omitempty"`
	Details string `json:"details,omitempty"`
	UUID    UUID   `json:"uuid,omitempty"`
	Rows    []Row  `json:"rows,omitempty"`
}

func ovsSliceToGoNotation(val interface{}) (interface{}, error) {
	switch sl := val.(type) {
	case []interface{}:
		bsliced, err := json.Marshal(sl)
		if err != nil {
			return nil, err
		}
		switch sl[0] {
		case "uuid", "named-uuid":
			var uuid UUID
			err = json.Unmarshal(bsliced, &uuid)
			return uuid, err
		case "set":
			var oSet OvsSet
			err = json.Unmarshal(bsliced, &oSet)
			return oSet, err
		case "map":
			var oMap OvsMap
			err = json.Unmarshal(bsliced, &oMap)
			return oMap, err
		}
		return val, nil
	}
	return val, nil
}

// interfaceToSetMapOrUUIDInterface takes a reflect.Value and converts it to
// the correct OVSDB Notation (Set, Map, UUID) using reflection
func interfaceToOVSDBNotationInterface(v reflect.Value) (interface{}, error) {
	// if value is a scalar value, it will be an interface that can
	// be type asserted back to string, float64, int etc...
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return v.Interface(), nil
	}
	// if its a set, map or uuid here we need to convert it to the correct type, not []interface{}
	s := v.Slice(0, v.Len())
	first := s.Index(0)
	// assert that our first element is a string value
	if first.Elem().Kind() != reflect.String {
		return nil, fmt.Errorf("first element of array/slice is not a string: %v %s", first, first.Kind().String())
	}
	switch first.Elem().String() {
	case "uuid", "named-uuid":
		uuid := s.Index(1).Elem().String()
		return UUID{GoUUID: uuid}, nil
	case "set":
		// second is the second element of the slice
		second := s.Index(1).Elem()
		// in a set, it must be a slice
		if second.Kind() != reflect.Slice && second.Kind() != reflect.Array {
			return nil, fmt.Errorf("second element of set is not a slice")
		}
		ss := second.Slice(0, second.Len())

		// check first index of second element
		// if it's not a slice or array this is a set of scalar values
		if ss.Index(0).Elem().Kind() != reflect.Slice && ss.Index(0).Elem().Kind() != reflect.Array {
			si := second.Interface()
			set := OvsSet{GoSet: si.([]interface{})}
			return set, nil
		}
		innerSet := []interface{}{}
		// iterate over the slice and extract the uuid, adding a UUID object to our innerSet
		for i := 0; i < ss.Len(); i++ {
			uuid := ss.Index(i).Elem().Index(1).Elem().String()
			innerSet = append(innerSet, UUID{GoUUID: uuid})
		}
		return OvsSet{GoSet: innerSet}, nil
	case "map":
		ovsMap := OvsMap{GoMap: make(map[interface{}]interface{})}
		second := s.Index(1).Elem()
		for i := 0; i < second.Len(); i++ {
			pair := second.Index(i).Elem().Slice(0, 2)
			var key interface{}
			// check if key is slice or array, in which case we can infer that it's a UUUID
			if pair.Index(0).Elem().Kind() == reflect.Slice || pair.Index(0).Elem().Kind() == reflect.Array {
				uuid := pair.Index(0).Elem().Index(1).Elem().String()
				key = UUID{GoUUID: uuid}
			} else {
				key = pair.Index(0).Interface()
			}
			// check if value is slice or array, in which case we can infer that it's a UUUID
			var value interface{}
			if pair.Index(1).Elem().Kind() == reflect.Slice || pair.Index(1).Elem().Kind() == reflect.Array {
				uuid := pair.Index(1).Elem().Index(1).Elem().String()
				value = UUID{GoUUID: uuid}
			} else {
				value = pair.Index(1).Elem().Interface()
			}
			ovsMap.GoMap[key] = value
		}
		return ovsMap, nil
	default:
		return nil, fmt.Errorf("unsupported notation. expected <uuid>,<named-uuid>,<set> or <map>. got %v", v)
	}
}
