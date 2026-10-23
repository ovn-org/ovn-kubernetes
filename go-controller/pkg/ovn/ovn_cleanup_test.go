// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunPodCleanupStepsAttemptsEveryStepAndAggregatesErrors(t *testing.T) {
	zoneErr := errors.New("zone teardown failed")
	failedTargetErr := errors.New("failed target cleanup failed")
	liveMigratableErr := errors.New("live migratable cleanup failed")

	var attempted []string
	err := runPodCleanupSteps(
		podCleanupStep{
			operation: "remove local-node pod namespace/pod",
			cleanup: func() error {
				attempted = append(attempted, "zone")
				return zoneErr
			},
		},
		podCleanupStep{
			operation: "delete network policy membership for pod namespace/pod",
			cleanup: func() error {
				attempted = append(attempted, "network-policy")
				return nil
			},
		},
		podCleanupStep{
			operation: "clean up failed live migration target pod namespace/pod",
			cleanup: func() error {
				attempted = append(attempted, "failed-target")
				return failedTargetErr
			},
		},
		podCleanupStep{
			operation: "clean up live migratable pod namespace/pod",
			cleanup: func() error {
				attempted = append(attempted, "live-migratable")
				return liveMigratableErr
			},
		},
	)
	if err == nil {
		t.Fatal("expected cleanup failures to be aggregated")
	}

	wantAttempted := []string{"zone", "network-policy", "failed-target", "live-migratable"}
	if !reflect.DeepEqual(attempted, wantAttempted) {
		t.Fatalf("unexpected cleanup attempts: got %v, want %v", attempted, wantAttempted)
	}
	for _, wantErr := range []error{zoneErr, failedTargetErr, liveMigratableErr} {
		if !errors.Is(err, wantErr) {
			t.Errorf("aggregated error %v does not wrap %v", err, wantErr)
		}
	}
	for _, context := range []string{
		"failed to remove local-node pod namespace/pod",
		"failed to clean up failed live migration target pod namespace/pod",
		"failed to clean up live migratable pod namespace/pod",
	} {
		if !strings.Contains(err.Error(), context) {
			t.Errorf("aggregated error %q does not contain context %q", err, context)
		}
	}
}
