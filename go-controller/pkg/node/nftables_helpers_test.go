// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"

	"sigs.k8s.io/knftables"
)

// nftRunFailer fails the first `failures` calls to Run, then delegates to the wrapped interface.
type nftRunFailer struct {
	knftables.Interface
	failures int
}

func (f *nftRunFailer) Run(ctx context.Context, tx *knftables.Transaction) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("Could not process rule: No such file or directory")
	}
	return f.Interface.Run(ctx, tx)
}
