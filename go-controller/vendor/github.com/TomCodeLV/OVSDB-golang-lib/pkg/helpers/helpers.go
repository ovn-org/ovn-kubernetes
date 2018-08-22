package helpers

// =======
// HELPERS
// =======

// Helper function for retrieving data from ovsdb set column.
func GetIdListFromOVSDBSet(data []interface{}) []string {
	// if there is multiple entries data are returned as set
	if data[0] == "set" {
		ret := []string{}
		for _, val := range data[1].([]interface{}) {
			ret = append(ret, val.([]interface{})[1].(string))
		}
		return ret
	} else { // if there is one entry it is returned as single value
		return []string{data[1].(string)}
	}
}

// Helper function to create ovsdb set
func MakeOVSDBSet(data map[string]interface{}) []interface{} {
	list := []interface{}{}
	for key, l := range data {
		for _, v := range l.([]string) {
			list = append(list, []string{key, v})
		}
	}
	return []interface{}{
		"set",
		list,
	}
}

func RemoveFromIdList(list []string, idsList []string) []string {
	idMap := make(map[string]bool, len(idsList))
	for _, val := range idsList {
		idMap[val] = true
	}

	ret := make([]string, 0)
	for _, uuid := range list {
		if _, ok := idMap[uuid]; !ok {
			ret = append(ret, uuid)
		}
	}

	return ret
}

func AppendNamedUUIDToUUIDList(UUIDList []interface{}, idsList []string) []interface{} {
	ret := UUIDList
	for _, val := range idsList {
		ret = append(ret, []interface{}{"named-uuid", val})
	}
	return ret
}

//func MapToList(data interface{}) []interface{} {
//	switch data.(type) {
//	case map[string]interface{}:
//		d := data.(map[string]interface{})
//		l := make([]interface{}, len(d))
//		c := 0
//		for _, val := range d {
//			l[c] = val
//			c++
//		}
//		return l
//	default:
//		return nil
//	}
//}