package dbcache

import (
	"encoding/json"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbmonitor"
	"errors"
	"fmt"
	"sync"
)

type iOVSDB interface {
	Monitor(schema string) *dbmonitor.Monitor
	Call(string, interface{}, *uint64) (json.RawMessage, error)
	GetCounter() uint64
}

type Cache struct {
	ID string
	sync.RWMutex
	OVSDB iOVSDB
	Schema string
	Indexes map[string][]string
	Data map[string]interface{} // Data[table][index_type][index_val][column]
}

func (cache *Cache) StartMonitor(schema string, tables map[string][]string) error {
	monitor := cache.OVSDB.Monitor(schema)

	for table, columns := range tables {
		monitor.Register(table, dbmonitor.Table{
			Columns: columns,
			Select: dbmonitor.Select{Initial:true, Insert: true, Delete: true, Modify: true,},
		})
	}

	res, err := monitor.Start(func(response json.RawMessage) {
		cache.Lock()
		cache.update(response)
		cache.Unlock()
	})
	if err != nil {
		return err
	}

	cache.Lock()
	cache.Data = make(map[string]interface{})
	err2 := cache.update(res)
	if err2 != nil {
		cache.Unlock()
		return err2
	}
	cache.Unlock()

	return nil
}

func normalize (data interface{}) interface{} {
	switch data.(type) {
	case []interface{}:
		a := data.([]interface{})
		if a[0] == "set" {
			if len(a[1].([]interface{})) == 0 {
				// empty set is returned both for empty set and unset optional value, so we convert to nil both
				return nil
			} else {
				// we convert all list type sets to real sets (maps in go)
				m := map[string]interface{}{}
				switch a[1].([]interface{})[0].(type) {
				case []interface{}: // we have a list of uuid pairs [["uuid", "some-value"]]
					for _, val := range a[1].([]interface{}) {
						m[val.([]interface{})[1].(string)] = normalize(val)
					}
				default: // we have a list of strings, numbers or booleans
					for _, val := range a[1].([]interface{}) {
						m[val.(string)] = val
					}
				}
				return m
			}
		} else if a[0] == "map" {
			if len(a[1].([]interface{})) == 0 {
				// like empty sets, we convert empty maps to nil for consistency
				return nil
			} else {
				m := map[string]interface{}{}
				// we convert list type map to real map, items always are pairs
				for _, val := range a[1].([]interface{}) {
					m[val.([]interface{})[0].(string)] = normalize(val)
				}
				return m
			}
		} else if a[0] == "uuid" {
			// single uuid is returned when set has single entry
			// in case we have single uuid, we convert it to single element set
			m := map[string]interface{}{}
			m[a[1].(string)] = a[1]
			return m
		} else {
			// we have a pair, key is stored previously in map
			return a[1]
		}
	default:
		return data
	}

	return nil // never invoked
}

func normalizeMap (data map[string]interface{}) map[string]interface{} {
	ret := map[string]interface{}{}
	for key, val := range data {
		ret[key] = normalize(val)
	}
	return ret
}

func (cache *Cache) update(response json.RawMessage) error {
	var update map[string]map[string]dbmonitor.RowUpdate
	json.Unmarshal(response, &update)

	for table, data := range update {
		for uuid, rowUpdate := range data {
			// make structures on initial update
			if _, ok := cache.Data[table]; !ok {
				cache.Data[table] = make(map[string]interface{})
				cache.Data[table].(map[string]interface{})["uuid"] = make(map[string]interface{})

				for _, index := range cache.Indexes[table] {
					if _, ok := rowUpdate.New[index]; !ok { // for initial update there will be "New"
						return errors.New(fmt.Sprintf("wrong index (%s) provided for table: %s", index, table))
					}

					cache.Data[table].(map[string]interface{})[index] = make(map[string]interface{})
				}
			}

			// update cache depending on activity type
			if rowUpdate.New != nil && rowUpdate.Old == nil { // initial or insert
				cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid] = normalizeMap(rowUpdate.New)
				cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})["uuid"] = uuid

				for _, index := range cache.Indexes[table] {
					indexValue := cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})[index].(string)
					cache.Data[table].(map[string]interface{})[index].(map[string]interface{})[indexValue] = cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid]
				}
			} else if rowUpdate.Old != nil && rowUpdate.New == nil { // delete
				// remove from custom index
				for _, index := range cache.Indexes[table] {
					indexValue := cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})[index].(string)
					delete(cache.Data[table].(map[string]interface{})[index].(map[string]interface{}), indexValue)
				}

				delete(cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{}), uuid)
			} else { // modify
				for column, _ := range rowUpdate.Old { // old contains only changed
					cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})[column] = normalize(rowUpdate.New[column])

					for _, index := range cache.Indexes[table] {
						indexValue := cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})[index].(string)
						cache.Data[table].(map[string]interface{})[index].(map[string]interface{})[indexValue].(map[string]interface{})[column] = cache.Data[table].(map[string]interface{})["uuid"].(map[string]interface{})[uuid].(map[string]interface{})[column]
					}
				}
			}
		}
	}

	update = nil

	return nil
}

func (cache *Cache) getData(args ...string) interface{} {
	var ret interface{}
	ret = cache.Data
	if ret == nil {
		return map[string]interface{}{}
	}
	for _, val := range args {
		ret = ret.(map[string]interface{})[val]
		if ret == nil {
			return map[string]interface{}{}
		}
	}
	return ret
}

func (cache *Cache) GetKeys(args ...string) []string {
	cache.RLock()
	data := cache.getData(args...).(map[string]interface{})
	keys := make([]string, len(data))
	c := 0
	for key, _ := range data {
		keys[c] = key
		c++
	}
	cache.RUnlock()

	return keys
}

func deepCopy(data interface{}) interface{} {
	switch data.(type) {
	case map[string]interface{}:
		ret := map[string]interface{}{}
		for key, val := range data.(map[string]interface{}) {
			ret[key] = deepCopy(val)
		}
		return ret
	default:
		return data
	}
}

// GetList is used to retrieve data from cache data structure and deep copy it.
// Cache structure: Data[table][index_type][index_val][column]
//
// Arguments is used as keys in order as in structure:
// first - table name of interest
// second - index type of interest, for example "uuid", "name", ...
// third - index value, for example particular item name
// fourth - column name, for example "external_ids"
//
// Any amount of arguments can be provided
func (cache *Cache) GetList(args ...string) []interface{} {
	cache.RLock()
	data := cache.getData(args...)
	d := data.(map[string]interface{})
	list := make([]interface{}, len(d))
	c := 0
	for _, val := range d {
		list[c] = deepCopy(val)
		c++
	}
	cache.RUnlock()

	return list
}

// GetMap is used to retrieve data from cache data structure and deep copy it.
// Cache structure: Data[table][index_type][index_val][column]
//
// Arguments is used as keys in order as in structure:
// first - table name of interest
// second - index type of interest, for example "uuid", "name", ...
// third - index value, for example particular item name
// fourth - column name, for example "external_ids"
//
// Any amount of arguments can be provided
func (cache *Cache) GetMap(args ...string) map[string]interface{} {
	cache.RLock()
	data := cache.getData(args...)
	m := make(map[string]interface{})
	for key, val := range data.(map[string]interface{}) {
		m[key] = deepCopy(val)
	}
	cache.RUnlock()

	return m
}

//func (cache *Cache) Get(args ...string) interface{} {
//	cache.RLock()
//	data := cache.getData(args...)
//	ret := deepCopy(data)
//	cache.RUnlock()
//
//	return ret
//}

//func (cache *Cache) MapToList(data map[string]interface{}) []interface{} {
//	l := make([]interface{}, len(data))
//	for _, val := range data {
//		l = append(l, val)
//	}
//	return l
//}
//
//func (cache *Cache) GetReferenceWait(schema string, reference string, list []interface{}) (uint64, string,  [][]interface{}, []string, string, []interface{}) {
//	row := []interface{}{map[string]interface{}{}}
//	row[0].(map[string]interface{})[reference] = helpers.MakeSet(list)
//	return 0, schema, [][]interface{}{}, []string{reference}, "==", row
//}