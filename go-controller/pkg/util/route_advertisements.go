// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"

	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
)

// RouteAdvertisementsReadyToken identifies a successfully generated node set.
// The count prevents a missing configuration from making a partial set usable.
func RouteAdvertisementsReadyToken(ra *ratypes.RouteAdvertisements, count int) string {
	return fmt.Sprintf("%s/%d/%d", ra.UID, ra.Generation, count)
}

// RouteAdvertisementsNodeLabel returns a label value identifying a node.
// The underscore keeps hashed names distinct from all valid DNS node names.
func RouteAdvertisementsNodeLabel(nodeName string) string {
	if len(nodeName) <= validation.LabelValueMaxLength {
		return nodeName
	}
	digest := sha256.Sum256([]byte(nodeName))
	return "node_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
}
