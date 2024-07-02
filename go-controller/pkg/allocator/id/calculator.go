package id

import (
	"fmt"
)

// CalculateID will return the ID calculated by 'base' + 'index' + 1 and validated
// by 'limit'
func CalculateID(idName string, index, base, limit uint) (uint, error) {
	base, err := CalculateIDBase(idName, index, base, limit, 1)
	if err != nil {
		return 0, err
	}
	return base + 1, err
}

// CalculateIDBase will return the ID base number to generate a number of
// 'cardinality' IDs for the position at `index`. It will fail if `limit` for the number of `cardinality`
// ids.
func CalculateIDBase(idName string, index, base, limit, cardinality uint) (uint, error) {
	if cardinality < 1 {
		return 0, fmt.Errorf("invalid arguments, cardinality '%d' has to be bigger than '1'", cardinality)
	}
	if limit <= base {
		return 0, fmt.Errorf("invalid arguments, limit '%d' has to be bigger than base '%d'", limit, base)
	}
	if index < 1 {
		return 0, fmt.Errorf("invalid arguments, index has to be bigger than '%d'", index)
	}

	maxID := base + index*cardinality
	if maxID >= limit {
		return 0, fmt.Errorf("out of bounds: calculated max ID '%d' is bigger than limit '%d' for '%s'", maxID, limit, idName)
	}
	// The base will be the last ID from the previous index.
	previousIndex := index - 1
	offset := cardinality * previousIndex
	return base + offset, nil
}
