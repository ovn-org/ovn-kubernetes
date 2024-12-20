//go:build linux
// +build linux

package nftables

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// MatchNFTRules checks that the expected nftables rules match the actual ones, ignoring
// order and extra whitespace.
func MatchNFTRules(expected, actual string) error {
	missing, extra := diffNFTRules(expected, actual)
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	msg := "nftables rule mismatch:"
	if len(missing) > 0 {
		msg += fmt.Sprintf("\nRules missing from `nft dump ruleset`:\n%s\n", strings.Join(missing, "\n"))
	}
	if len(extra) > 0 {
		msg += fmt.Sprintf("\nUnexpected extra rules in `nft dump ruleset`:\n%s\n", strings.Join(extra, "\n"))
	}
	return fmt.Errorf("%s", msg)
}

// helper function, for ease of unit testing
func diffNFTRules(expected, actual string) (missing, extra []string) {
	expectedSet := sets.New[string]()
	for _, line := range strings.Split(expected, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			expectedSet.Insert(line)
		}
	}

	actualSet := sets.New[string]()
	for _, line := range strings.Split(actual, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			actualSet.Insert(line)
		}
	}

	missingSet := expectedSet.Difference(actualSet)
	extraSet := actualSet.Difference(expectedSet)

	// While we ignore order for purposes of the comparison, it's confusing to output
	// the missing/extra rules in essentially random order (and makes it harder to see
	// what the problem is in cases like "the rules are basically correct, except that
	// they have the wrong IP"). So we sort the `missing` rules back into the same
	// order as they appeared in `expected`, and the `extra` rules into the same order
	// as they appeared in `actual`.
	missingSorted := missingSet.UnsortedList()
	sort.Slice(missingSorted, func(i, j int) bool {
		return strings.Index(expected, missingSorted[i]) < strings.Index(expected, missingSorted[j])
	})
	extraSorted := extraSet.UnsortedList()
	sort.Slice(extraSorted, func(i, j int) bool {
		return strings.Index(actual, extraSorted[i]) < strings.Index(actual, extraSorted[j])
	})

	return missingSorted, extraSorted
}
