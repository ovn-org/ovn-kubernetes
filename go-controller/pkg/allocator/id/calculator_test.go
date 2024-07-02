package id

import (
	"testing"

	. "github.com/onsi/gomega"

	"k8s.io/utils/ptr"
)

func TestCalculateIDBase(t *testing.T) {
	var testCases = []struct {
		description                     string
		index, base, limit, cardinality uint
		expectedError                   *string
		expectedIDBase                  uint
	}{
		{
			description:    "with index within range return proper ID",
			index:          1,
			base:           2,
			limit:          5,
			cardinality:    1,
			expectedIDBase: 2,
		},
		{
			description:    "with index within range and cardinality 2 return proper IDs",
			index:          1,
			base:           5,
			limit:          10,
			cardinality:    2,
			expectedIDBase: 5,
		},

		{
			description:    "with index within range and cardinality 3 return proper IDs",
			index:          6,
			base:           2,
			limit:          33,
			cardinality:    3,
			expectedIDBase: 17,
		},
		{
			description:   "with index less than 1 should throw error",
			index:         0,
			base:          2,
			limit:         4,
			cardinality:   1,
			expectedError: ptr.To("invalid arguments, index has to be bigger than '0'"),
		},
		{
			description:   "with cardinality less than 1 should throw error",
			index:         3,
			base:          2,
			limit:         2,
			cardinality:   0,
			expectedError: ptr.To("invalid arguments, cardinality '0' has to be bigger than '1'"),
		},

		{
			description:   "with limit equals to base throw an error",
			index:         3,
			base:          2,
			limit:         2,
			cardinality:   1,
			expectedError: ptr.To("invalid arguments, limit '2' has to be bigger than base '2'"),
		},
		{
			description:   "with limit less than base throw an error",
			index:         3,
			base:          2,
			limit:         1,
			cardinality:   1,
			expectedError: ptr.To("invalid arguments, limit '1' has to be bigger than base '2'"),
		},
		{
			description:   "with index with cardinality equals to limit throw error",
			index:         3,
			base:          2,
			limit:         7,
			cardinality:   2,
			expectedError: ptr.To("out of bounds: calculated max ID '8' is bigger than limit '7' for 'test-id'"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			g := NewWithT(t)
			obtainedIDBase, err := CalculateIDBase("test-id", tc.index, tc.base, tc.limit, tc.cardinality)
			if tc.expectedError != nil {
				g.Expect(err).To(MatchError(*tc.expectedError))
			} else {
				g.Expect(obtainedIDBase).To(Equal(tc.expectedIDBase))
			}
		})
	}
}

func TestCalculateID(t *testing.T) {
	var testCases = []struct {
		description        string
		index, base, limit uint
		expectedID         uint
	}{
		{
			description: "with index '1' within range return proper ID",
			index:       1,
			base:        2,
			limit:       5,
			expectedID:  3,
		},
		{
			description: "with index '2' within range return proper ID",
			index:       2,
			base:        2,
			limit:       5,
			expectedID:  4,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			g := NewWithT(t)
			obtainedID, err := CalculateID("test-id", tc.index, tc.base, tc.limit)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(obtainedID).To(Equal(tc.expectedID))
		})
	}
}
