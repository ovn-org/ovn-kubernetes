//go:build linux
// +build linux

package nftables

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// MatchNFTRules checks that the expected nftables rules match the actual ones, ignoring
// order.
func MatchNFTRules(expected, actual string) error {
	expectedSet := sets.New(strings.Split(expected, "\n")...)
	actualSet := sets.New(strings.Split(actual, "\n")...)

	// ignore blank lines
	expectedSet.Delete("")
	actualSet.Delete("")

	missing := expectedSet.Difference(actualSet)
	extra := actualSet.Difference(expectedSet)

	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	msg := "nftables rule mismatch:"
	if len(missing) > 0 {
		msg += fmt.Sprintf("\nMissing rules: %v\n", missing.UnsortedList())
	}
	if len(extra) > 0 {
		msg += fmt.Sprintf("\nExtra rules: %v\n", extra.UnsortedList())
	}
	return fmt.Errorf("%s", msg)
}
