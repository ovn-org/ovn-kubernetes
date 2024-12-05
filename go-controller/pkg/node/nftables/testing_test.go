//go:build linux
// +build linux

package nftables

import (
	"reflect"
	"testing"
)

func Test_diffNFTRules(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		actual   string
		missing  []string
		extra    []string
	}{
		{
			name:     "empty match",
			expected: "",
			actual:   "",
			missing:  []string{},
			extra:    []string{},
		},
		{
			name:     "non-empty match",
			expected: "line one\nline two\nline three\n",
			actual:   "line three\nline one\nline two\n",
			missing:  []string{},
			extra:    []string{},
		},
		{
			name:     "match with extra whitespace",
			expected: "	line one\n	line two\n	line three\n",
			actual:   "\nline three\nline one\nline two\n\n",
			missing:  []string{},
			extra:    []string{},
		},
		{
			name:     "missing lines",
			expected: "line one\nline two\nline three\nline four\n",
			actual:   "line two\nline four\n",
			missing:  []string{"line one", "line three"},
			extra:    []string{},
		},
		{
			name:     "missing lines, alternate order",
			expected: "line one\nline two\nline three\nline four\n",
			actual:   "line four\nline two\n",
			missing:  []string{"line one", "line three"},
			extra:    []string{},
		},
		{
			name:     "extra lines",
			expected: "line two\nline four\n",
			actual:   "line one\nline two\nline three\nline four\n",
			missing:  []string{},
			extra:    []string{"line one", "line three"},
		},
		{
			name:     "extra lines, alternate order",
			expected: "line four\nline two\n",
			actual:   "line one\nline two\nline three\nline four\n",
			missing:  []string{},
			extra:    []string{"line one", "line three"},
		},
		{
			name:     "missing and extra lines, inconsistent whitespace",
			expected: "	line one\n    line two\n	line three\n",
			actual:   " line two\n line two-and-a-half\nline three",
			missing:  []string{"line one"},
			extra:    []string{"line two-and-a-half"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing, extra := diffNFTRules(tc.expected, tc.actual)
			if !reflect.DeepEqual(tc.missing, missing) {
				t.Errorf("expected missing=%#v, got %#v", tc.missing, missing)
			}
			if !reflect.DeepEqual(tc.extra, extra) {
				t.Errorf("expected extra=%#v, got %#v", tc.extra, extra)
			}
		})
	}
}
