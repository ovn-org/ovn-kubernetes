// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"fmt"
	"testing"
)

func TestEgressIPMarkAllocatorInclusiveRange(t *testing.T) {
	allocator := getEgressIPMarkAllocator()
	maxOffset := eipMarkMax - eipMarkMin
	for offset := 0; offset <= maxOffset; offset++ {
		got, err := allocator.AllocateID(fmt.Sprintf("eip-%d", offset))
		if err != nil || got != offset {
			t.Fatalf("allocating mark %d: got offset %d, error %v", eipMarkMin+offset, got, err)
		}
	}
	if _, err := allocator.AllocateID("exhausted"); err == nil {
		t.Fatal("allocation succeeded after exhausting the inclusive mark range")
	}
	allocator.ReleaseID(fmt.Sprintf("eip-%d", maxOffset))
	if err := allocator.ReserveID("restored", maxOffset); err != nil {
		t.Fatalf("could not restore the maximum valid mark: %v", err)
	}
	if _, err := allocator.AllocateID("still-exhausted"); err == nil {
		t.Fatal("restoring the maximum valid mark did not consume the last slot")
	}
}
