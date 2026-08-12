// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"sync"
	"testing"

	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// recorder is a sink for the reconcile keys a controller.Reconciler receives, so
// tests can assert which follow-up work the controller enqueued.
type recorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *recorder) record(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// startRecorder builds a real controller.Reconciler whose only job is to record
// the keys it is handed (controller.Reconciler has unexported methods, so it
// cannot be faked outside the package). The caller must controller.Stop it.
func startRecorder(g gomega.Gomega, name string) (*recorder, controller.Reconciler) {
	rec := &recorder{}
	r := controller.NewReconciler(name, &controller.ReconcilerConfig{
		RateLimiter: controller.DefaultRateLimiter[string](),
		Reconcile:   func(key string) error { rec.record(key); return nil },
		Threadiness: 1,
		MaxAttempts: 1,
	})
	g.Expect(controller.Start(r)).To(gomega.Succeed())
	return rec, r
}

// cdnPortFor returns the CDN Gateway Router external port name for a node, the
// same way the controller derives it.
func cdnPortFor(node string) string {
	return types.GWRouterToExtSwitchPrefix + (&util.DefaultNetInfo{}).GetNetworkScopedGWRouterName(node)
}

// fakeUplinkSourceProvider provides the designated uplink sources without a real
// openflow manager. An empty map leaves only the default group (the getter adds
// the CDN source for the "" uplink itself).
type fakeUplinkSourceProvider struct {
	sources map[string]string
}

func (f *fakeUplinkSourceProvider) GetMacBindingSourceForUplinks() map[string]string {
	return f.sources
}

// newTestController builds a controller wired only with the fields the
// method-level tests exercise, bypassing NewMACBindingController (which needs a
// live networkManager).
func newTestController() *MACBindingController {
	return &MACBindingController{
		uplinkSourceProvider: &fakeUplinkSourceProvider{sources: map[string]string{}},
		nodeName:             "node1",
		followers:            map[string]sets.Set[string]{},
	}
}

// TestReconcileUplinkSource verifies that a re-designation notification for a
// network that is not already a source invalidates the cached uplink->source
// designation — making a full reconcile due (hasUnknownSource) — and enqueues
// the network so the group is recomputed.
func TestReconcileUplinkSource(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

	const network = "tenantred"

	netRec, netR := startRecorder(g, "network")
	defer controller.Stop(netR)

	c := newTestController()
	c.cdnGatewayPort = cdnPortFor("node1")
	c.networkReconciler = netR
	c.uplinkSourceProvider = &fakeUplinkSourceProvider{sources: map[string]string{"uplinkB": "rtoe-GR_other_node1"}}
	// prime a valid designation so hasUnknownSource is only true if it gets invalidated.
	c.getMacBindingSourceForUplinks()
	g.Expect(c.hasUnknownSource()).To(gomega.BeFalse())

	c.ReconcileUplinkSource(network)

	// the designation was invalidated, so a full reconcile is now due...
	g.Expect(c.hasUnknownSource()).To(gomega.BeTrue())
	// ...and the network is enqueued for recompute.
	g.Eventually(netRec.got).Should(gomega.ConsistOf(network))
}
