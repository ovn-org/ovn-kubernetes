// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Startup waiter", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	AfterEach(func() {
		// Restore the global config so the timeout set here does not
		// leak into other specs in this package.
		Expect(config.PrepareTestConfig()).To(Succeed(), "failed to restore test config")
	})

	It("uses the configured startup readiness timeout", func() {
		config.OvnKubeNode.StartupReadinessTimeout = 1
		waiter := newStartupWaiter()
		Expect(waiter.timeout).To(Equal(time.Second), "startup-readiness-timeout=1 should give a 1s waiter timeout")

		waiter.AddWait(func() (bool, error) { return false, nil }, nil)
		start := time.Now()
		err := waiter.Wait()
		elapsed := time.Since(start)
		Expect(err).To(MatchError(context.DeadlineExceeded), "a check that never succeeds should hit the deadline")
		Expect(elapsed).To(BeNumerically(">=", 900*time.Millisecond), "wait returned before the 1s timeout")
		Expect(elapsed).To(BeNumerically("<", 5*time.Second), "wait ran well past the 1s timeout")
	})

	It("defaults to a 300 second timeout", func() {
		Expect(newStartupWaiter().timeout).To(Equal(300*time.Second), "default waiter timeout should be 300s")
	})

	It("runs the post wait function once the wait succeeds", func() {
		postRan := false
		waiter := newStartupWaiter()
		waiter.AddWait(func() (bool, error) { return true, nil }, func() error {
			postRan = true
			return nil
		})
		Expect(waiter.Wait()).To(Succeed(), "wait should succeed when the check returns true")
		Expect(postRan).To(BeTrue(), "post wait function should run after a successful wait")
	})
})
