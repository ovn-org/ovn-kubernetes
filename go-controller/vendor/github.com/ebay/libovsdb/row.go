package libovsdb

import "encoding/json"

// Row is a table Row according to RFC7047
type Row struct {
	Fields map[string]interface{}
}

// UnmarshalJSON unmarshalls a byte array to an OVSDB Row
func (r *Row) UnmarshalJSON(b []byte) (err error) {
	r.Fields = make(map[string]interface{})
	var raw map[string]interface{}
	err = json.Unmarshal(b, &raw)
	for key, val := range raw {
		val, err = ovsSliceToGoNotation(val)
		if err != nil {
			return err
		}
		r.Fields[key] = val
	}
	return err
}

// ResultRow is an properly unmarshalled row returned by Transact
type ResultRow map[string]interface{}

// UnmarshalJSON unmarshalls a byte array to an OVSDB Row
func (r *ResultRow) UnmarshalJSON(b []byte) (err error) {
	*r = make(map[string]interface{})
	var raw map[string]interface{}
	err = json.Unmarshal(b, &raw)
	for key, val := range raw {
		val, err = ovsSliceToGoNotation(val)
		if err != nil {
			return err
		}
		(*r)[key] = val
	}
	return err
}
