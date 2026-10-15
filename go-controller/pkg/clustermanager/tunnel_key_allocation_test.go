// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestInitTunnelKeysAllocatorOutOfRange(t *testing.T) {
	for _, invalid := range []int{-2, 1 << 24, math.MaxInt} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			if err := config.PrepareTestConfig(); err != nil {
				t.Fatal(err)
			}
			nad := ovntesting.GenerateNAD("test", "test", "test", types.Layer2Topology, "10.0.0.0/24", types.NetworkRolePrimary)
			// Keep the derived transit key valid so the dynamic key reaches the bitmap.
			nad.Annotations = map[string]string{types.OvnNetworkTunnelKeysAnnotation: fmt.Sprintf("[%d,%d]", 16711683+1, invalid)}
			client := util.GetOVNClientset(nad)
			allocator, err := initTunnelKeysAllocator(client.NetworkAttchDefClient, client.NetworkConnectClient)
			if err == nil || !strings.Contains(err.Error(), "outside allocation range") {
				t.Fatalf("expected startup bounds error, got %v", err)
			}
			if allocator != nil {
				t.Fatal("returned allocator after failed initialization")
			}
		})
	}
}
