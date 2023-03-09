package batching

import (
	"fmt"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"strings"
	"testing"
)

type batchTestData struct {
	name      string
	batchSize int
	data      []int
	result    []int
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
		ginkgo.By(tCase.name)
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
