package util

// RemoveIndexFromSliceUnstable attempts to remove slice index specified by parameter i. Slice order is not preserved.
func RemoveIndexFromSliceUnstable[T comparable](slice []T, i int) []T {
	var t T
	sliceLen := len(slice)
	slice[i] = slice[sliceLen-1]
	slice[sliceLen-1] = t // zero out the copied last element to have it garbage collected
	return slice[:sliceLen-1]
}

// RemoveItemFromSliceUnstable attempts to remove an item from a slice specified by parameter candidate. Slice order is not preserved.
func RemoveItemFromSliceUnstable[T comparable](slice []T, candidate T) []T {
	for i := 0; i < len(slice); {
		if slice[i] == candidate {
			slice = RemoveIndexFromSliceUnstable(slice, i)
			continue
		}
		i++
	}
	return slice
}
