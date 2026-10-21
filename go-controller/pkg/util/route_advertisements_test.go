// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestRouteAdvertisementsNodeLabel(t *testing.T) {
	for _, name := range []string{
		"worker.example.org", strings.Repeat("a", 63),
		strings.Repeat("a", 63) + ".example.org",
		strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61),
	} {
		t.Run(name, func(t *testing.T) {
			value := RouteAdvertisementsNodeLabel(name)
			if errs := validation.IsValidLabelValue(value); len(errs) != 0 {
				t.Fatalf("invalid node label %q: %v", value, errs)
			}
			if value != RouteAdvertisementsNodeLabel(name) {
				t.Fatal("node label is not deterministic")
			}
			if len(name) <= validation.LabelValueMaxLength {
				if value != name {
					t.Fatalf("short name changed: %q", value)
				}
			} else {
				if len(validation.IsDNS1123Subdomain(value)) == 0 {
					t.Fatal("hashed label could collide with an unencoded node name")
				}
				other := name[:len(name)-1] + "c"
				if value == RouteAdvertisementsNodeLabel(other) {
					t.Fatal("different long names have the same label")
				}
			}
		})
	}
}
