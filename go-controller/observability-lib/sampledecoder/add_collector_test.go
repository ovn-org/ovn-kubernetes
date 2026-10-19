// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package sampledecoder

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/observability-lib/ovsdb"
)

// TestValidateCollectorReuse covers the collector registration collision semantics:
// re-registration by the same owner with the same group is idempotent (so a consumer
// can recover its own collector after a restart), while a different owner or a
// different group for the same collector ID is rejected as a conflict.
func TestValidateCollectorReuse(t *testing.T) {
	group10 := 10
	existing := func(owner string, group *int) *ovsdb.FlowSampleCollectorSet {
		return &ovsdb.FlowSampleCollectorSet{
			ID:           1,
			LocalGroupID: group,
			ExternalIDs:  map[string]string{"owner": owner},
		}
	}

	tests := []struct {
		name       string
		existing   *ovsdb.FlowSampleCollectorSet
		groupID    int
		ownerName  string
		wantReuse  bool
		wantErrSub string
	}{
		{
			name:      "same owner and group is reused (restart case)",
			existing:  existing("netobserv", &group10),
			groupID:   10,
			ownerName: "netobserv",
			wantReuse: true,
		},
		{
			name:       "different owner is a conflict",
			existing:   existing("netobserv", &group10),
			groupID:    10,
			ownerName:  "other",
			wantErrSub: "already in use",
		},
		{
			name:       "same owner different group is a conflict",
			existing:   existing("netobserv", &group10),
			groupID:    20,
			ownerName:  "netobserv",
			wantErrSub: "already in use",
		},
		{
			name:       "existing collector without a group is a conflict",
			existing:   existing("netobserv", nil),
			groupID:    10,
			ownerName:  "netobserv",
			wantErrSub: "already in use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCollectorReuse(tt.existing, tt.groupID, tt.ownerName)
			if tt.wantReuse {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErrSub)
		})
	}
}

// TestAddCollectorRejectsReservedID covers the collector-ID validation, which mirrors the
// ObservabilityConfig CRD bounds (1..math.MaxUint32): IDs below 1 (0 is the "no collector"
// sentinel used by Shutdown, negatives are meaningless) and IDs above the uint32 max are
// rejected before any OVSDB access. A valid ID passes the range check and only then hits the
// uninitialized-client guard.
func TestAddCollectorRejectsReservedID(t *testing.T) {
	tests := []struct {
		name        string
		collectorID int
		wantErrSub  string
	}{
		{
			name:        "zero is reserved",
			collectorID: 0,
			wantErrSub:  "collector ID must be between 1 and",
		},
		{
			name:        "negative is rejected",
			collectorID: -1,
			wantErrSub:  "collector ID must be between 1 and",
		},
		{
			name:        "above uint32 max is rejected",
			collectorID: math.MaxUint32 + 1,
			wantErrSub:  "collector ID must be between 1 and",
		},
		{
			name:        "valid ID passes the range check",
			collectorID: 1,
			wantErrSub:  "OVSDB client is not initialized",
		},
		{
			name:        "uint32 max is valid",
			collectorID: math.MaxUint32,
			wantErrSub:  "OVSDB client is not initialized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// ovsdbClient is nil: a valid ID reaches (and trips) the client guard, while an
			// invalid ID is rejected before the client is ever touched.
			d := &SampleDecoder{}
			err := d.AddCollector(tt.collectorID, 10, "ovnk-debug")
			require.ErrorContains(t, err, tt.wantErrSub)
		})
	}
}
