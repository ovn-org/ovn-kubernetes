// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"strings"
)

const (
	APBRouteErrorMsg       = "failed to apply policy"
	EgressFirewallErrorMsg = "EgressFirewall Rules not correctly applied"
	EgressQoSErrorMsg      = "EgressQoS Rules not correctly applied"
	NetworkQoSErrorMsg     = "NetworkQoS Destinations not correctly applied"
	// NetworkQoSNoErrorsMsg means no zone reported an error.
	NetworkQoSNoErrorsMsg = "No NetworkQoS errors reported"
)

func GetZoneStatus(zoneID, message string) string {
	return fmt.Sprintf("%s: %s", zoneID, message)
}

func GetZoneFromStatus(status string) string {
	return strings.Split(status, ":")[0]
}
