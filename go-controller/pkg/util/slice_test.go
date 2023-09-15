package util

import (
	"reflect"
	"sort"
	"testing"
)

func TestRemoveIndexFromSliceUnstableFunc(t *testing.T) {
	var tcs = []struct {
		input    []int
		index    int
		expected []int
	}{

		{
			[]int{1, 2, 3},
			0,
			[]int{2, 3},
		},
		{
			[]int{1, 2, 3},
			1,
			[]int{1, 3},
		},
		{
			[]int{1, 2, 3},
			2,
			[]int{1, 2},
		},
	}

	for _, tc := range tcs {
		inputCopy := make([]int, len(tc.input))
		copy(inputCopy, tc.input)
		result := RemoveIndexFromSliceUnstable(inputCopy, tc.index)
		if !equalIntSliceUnstable(result, tc.expected) {
			t.Errorf("RemoveIndexFromSliceUnstable(%v, %d) = %v, want %v", tc.input, tc.index, result, tc.expected)
		}
	}
}

func TestRemoveItemFromSliceUnstableFunc(t *testing.T) {
	var tcs = []struct {
		input    []int
		remove   int
		expected []int
	}{
		{
			[]int{},
			0,
			[]int{},
		},
		{
			[]int{1, 2, 3},
			0,
			[]int{1, 2, 3},
		},
		{
			[]int{1, 2, 3},
			2,
			[]int{1, 3},
		},
		{
			[]int{1, 2, 2},
			2,
			[]int{1},
		},
	}

	for _, tc := range tcs {
		inputCopy := make([]int, len(tc.input))
		copy(inputCopy, tc.input)
		result := RemoveItemFromSliceUnstable(inputCopy, tc.remove)
		if !equalIntSliceUnstable(result, tc.expected) {
			t.Errorf("RemoveItemFromSliceUnstable(%v, %d) = %v, want %v", tc.input, tc.remove, result, tc.expected)
		}
	}
}

func equalIntSliceUnstable(sliceA, sliceB []int) bool {
	if len(sliceA) != len(sliceB) {
		return false
	}
	sliceACopy := make([]int, len(sliceA))
	sliceBCopy := make([]int, len(sliceB))
	copy(sliceACopy, sliceA)
	copy(sliceBCopy, sliceB)
	sort.Ints(sliceACopy)
	sort.Ints(sliceBCopy)
	return reflect.DeepEqual(sliceACopy, sliceBCopy)
}
