package batching

import (
	"fmt"

	"github.com/onsi/gomega"

	"strings"
	"testing"
)

type batchTestData struct {
	name      string
	batchSize int
	data      []int
	expectErr string
}

func TestBatch(t *testing.T) {
	tt := []batchTestData{
		{
			name:      "batch size should be > 0",
			batchSize: 0,
			data:      []int{1, 2, 3},
			expectErr: "batchSize should be > 0",
		},
		{
			name:      "batchSize = 1",
			batchSize: 1,
			data:      []int{1, 2, 3},
		},
		{
			name:      "batchSize > 1",
			batchSize: 2,
			data:      []int{1, 2, 3},
		},
		{
			name:      "number of batches = 0",
			batchSize: 2,
			data:      nil,
		},
		{
			name:      "number of batches = 1",
			batchSize: 2,
			data:      []int{1, 2},
		},
		{
			name:      "number of batches > 1",
			batchSize: 2,
			data:      []int{1, 2, 3, 4},
		},
		{
			name:      "number of batches not int",
			batchSize: 2,
			data:      []int{1, 2, 3, 4, 5},
		},
	}

	for _, tCase := range tt {
		g := gomega.NewGomegaWithT(t)
		var result []int
		batchNum := 0
		err := Batch[int](tCase.batchSize, tCase.data, func(l []int) error {
			batchNum += 1
			result = append(result, l...)
			return nil
		})
		if err != nil {
			if tCase.expectErr != "" && strings.Contains(err.Error(), tCase.expectErr) {
				continue
			}
			t.Fatal(fmt.Sprintf("test %s failed: %v", tCase.name, err))
		}
		// tCase.data/tCase.batchSize round up
		expectedBatchNum := (len(tCase.data) + tCase.batchSize - 1) / tCase.batchSize
		g.Expect(batchNum).To(gomega.Equal(expectedBatchNum))
		g.Expect(result).To(gomega.Equal(tCase.data))
	}
}

type batchMapTestData struct {
	name               string
	batchSize          int
	data               map[string][]int
	expectErr          string
	expectedBatchesNum int
}

func TestBatchMap(t *testing.T) {
	tt := []batchMapTestData{
		{
			name:      "batch size should be > 0",
			batchSize: 0,
			data:      map[string][]int{"a": {1, 2, 3}},
			expectErr: "batchSize should be > 0",
		},
		{
			name:      "batchSize = 1",
			batchSize: 1,
			data: map[string][]int{
				"a": {1},
				"b": {2},
				"c": {3},
			},
			expectedBatchesNum: 3,
		},
		{
			name:      "batchSize = 1, nil value",
			batchSize: 1,
			data: map[string][]int{
				"a": nil,
				"b": nil,
			},
			expectedBatchesNum: 1,
		},
		{
			name:      "batchSize = 1, empty value",
			batchSize: 1,
			data: map[string][]int{
				"a": {},
				"b": {},
			},
			expectedBatchesNum: 1,
		},
		{
			name:      "batchSize = 1, len(value) > 1",
			batchSize: 1,
			data: map[string][]int{
				"a": {1, 2, 3},
				"b": {4},
			},
			expectedBatchesNum: 4,
		},
		{
			name:      "batchSize > 1",
			batchSize: 2,
			data: map[string][]int{
				"a": {1},
				"b": {2},
				"c": {3},
			},
			expectedBatchesNum: 2,
		},
		{
			name:               "number of batches = 0",
			batchSize:          2,
			data:               map[string][]int{},
			expectedBatchesNum: 0,
		},
		{
			name:      "number of batches = 1, same key",
			batchSize: 2,
			data: map[string][]int{
				"a": {1, 2},
			},
			expectedBatchesNum: 1,
		},
		{
			name:      "number of batches = 1, different keys",
			batchSize: 2,
			data: map[string][]int{
				"a": {1},
				"b": {2},
			},
			expectedBatchesNum: 1,
		},
		{
			name:      "number of batches > 1, different keys",
			batchSize: 2,
			data: map[string][]int{
				"a": {1, 2},
				"b": {3},
				"c": {4},
			},
			expectedBatchesNum: 2,
		},
		{
			name:      "number of batches > 2, different keys",
			batchSize: 3,
			data: map[string][]int{
				"a": {1, 2},
				"b": {3, 4, 5},
				"c": {6, 7},
			},
			expectedBatchesNum: 3,
		},
		{
			name:      "number of batches = 2, split values",
			batchSize: 3,
			data: map[string][]int{
				"a": {1, 2},
				"b": {3, 4, 5},
				"c": {6},
			},
			expectedBatchesNum: 2,
		},
	}

	for _, tCase := range tt {
		g := gomega.NewGomegaWithT(t)
		result := map[string][]int{}
		batchNum := 0
		err := BatchMap[int](tCase.batchSize, tCase.data, func(l map[string][]int) error {
			batchNum += 1
			for key, value := range l {
				// this is needed to handle empty list vs nil, since appending empty list to a nil returns nil.
				if len(value) == 0 {
					result[key] = value
				} else {
					result[key] = append(result[key], value...)
				}
			}
			return nil
		})
		if err != nil {
			if tCase.expectErr != "" && strings.Contains(err.Error(), tCase.expectErr) {
				continue
			}
			t.Fatal(fmt.Sprintf("test %s failed: %v", tCase.name, err))
		}
		g.Expect(batchNum).To(gomega.Equal(tCase.expectedBatchesNum))
		g.Expect(result).To(gomega.Equal(tCase.data))
	}
}
