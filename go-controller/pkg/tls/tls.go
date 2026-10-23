// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package tls

import (
	"crypto/tls"
	"fmt"
	"math"

	cliflag "k8s.io/component-base/cli/flag"
)

type ApplyConfigOptions func(*tls.Config)

func NewApplyConfigOptions(minVersion string, cipherSuites []string, curvePreferences []int) (ApplyConfigOptions, error) {
	minVersionID, err := cliflag.TLSVersion(minVersion)
	if err != nil {
		return nil, err
	}

	cipherSuiteIDs, err := cliflag.TLSCipherSuites(cipherSuites)
	if err != nil {
		return nil, err
	}

	curvePrefs32 := make([]int32, len(curvePreferences))
	for i, v := range curvePreferences {
		if v < 0 || v > math.MaxUint16 {
			return nil, fmt.Errorf("invalid TLS curve preference %d: out of range", v)
		}
		curvePrefs32[i] = int32(v)
	}

	curveIDs, err := cliflag.TLSCurvePreferences(curvePrefs32)
	if err != nil {
		return nil, fmt.Errorf("error parsing TLS curve preferences %v: %w", curvePreferences, err)
	}

	return func(cfg *tls.Config) {
		cfg.CipherSuites = cipherSuiteIDs
		cfg.MinVersion = minVersionID
		cfg.CurvePreferences = curveIDs
	}, nil
}
