// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// resyncCall records one ResyncSampling invocation.
type resyncCall struct {
	feature    libovsdbops.SampleFeature
	namespaces sets.Set[string]
}

// recordingHandler is a SamplingResyncHandler that records the calls it receives.
type recordingHandler struct {
	calls []resyncCall
}

func (h *recordingHandler) ResyncSampling(feature libovsdbops.SampleFeature, namespaces sets.Set[string]) error {
	h.calls = append(h.calls, resyncCall{feature: feature, namespaces: namespaces})
	return nil
}

// fr builds a featureResolution from a cluster-wide collector list and a per-namespace map.
func fr(cluster []string, perNS map[string][]string) *featureResolution {
	f := &featureResolution{cluster: sets.New(cluster...), perNS: map[string]sets.Set[string]{}}
	for ns, cs := range perNS {
		f.perNS[ns] = sets.New(cs...)
	}
	return f
}

var _ = Describe("Observability resync diff", func() {
	const npF = libovsdbops.NetworkPolicySample
	const efF = libovsdbops.EgressFirewallSample

	DescribeTable("diffResolutions",
		func(old, new map[libovsdbops.SampleFeature]*featureResolution, expected map[libovsdbops.SampleFeature]resyncScope) {
			Expect(diffResolutions(old, new)).To(Equal(expected))
		},
		Entry("no change yields empty delta",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]resyncScope{},
		),
		Entry("feature added at cluster scope resyncs all",
			map[libovsdbops.SampleFeature]*featureResolution{},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]resyncScope{npF: {all: true}},
		),
		Entry("feature fully removed resyncs all",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]*featureResolution{},
			map[libovsdbops.SampleFeature]resyncScope{npF: {all: true}},
		),
		Entry("cluster collectors changed resyncs all",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1", "c2"}, nil)},
			map[libovsdbops.SampleFeature]resyncScope{npF: {all: true}},
		),
		Entry("namespace added, cluster unchanged, resyncs that namespace",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, map[string][]string{"foo": {"c2"}})},
			map[libovsdbops.SampleFeature]resyncScope{npF: {namespaces: sets.New("foo")}},
		),
		Entry("namespace collectors changed resyncs that namespace",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, map[string][]string{"foo": {"c2"}})},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, map[string][]string{"foo": {"c3"}})},
			map[libovsdbops.SampleFeature]resyncScope{npF: {namespaces: sets.New("foo")}},
		),
		Entry("namespace removed resyncs that namespace",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, map[string][]string{"foo": {"c2"}})},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil)},
			map[libovsdbops.SampleFeature]resyncScope{npF: {namespaces: sets.New("foo")}},
		),
		Entry("only the changed feature is included",
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1"}, nil), efF: fr([]string{"c9"}, nil)},
			map[libovsdbops.SampleFeature]*featureResolution{npF: fr([]string{"c1", "c2"}, nil), efF: fr([]string{"c9"}, nil)},
			map[libovsdbops.SampleFeature]resyncScope{npF: {all: true}},
		),
	)
})

var _ = Describe("Observability resync dispatch", func() {
	var (
		nbClient        libovsdbclient.Client
		libovsdbCleanup *libovsdbtest.Context
		manager         *Manager
		samplingApps    []libovsdbtest.TestData
	)

	BeforeEach(func() {
		samplingApps = []libovsdbtest.TestData{
			&nbdb.SamplingApp{UUID: "drop-sampling-uuid", ID: DropSamplingID, Type: nbdb.SamplingAppTypeDrop},
			&nbdb.SamplingApp{UUID: "acl-new-traffic-sampling-uuid", ID: ACLNewTrafficSamplingID, Type: nbdb.SamplingAppTypeACLNew},
			&nbdb.SamplingApp{UUID: "acl-est-traffic-sampling-uuid", ID: ACLEstTrafficSamplingID, Type: nbdb.SamplingAppTypeACLEst},
		}
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		Expect(manager.Init()).To(Succeed())
	})

	AfterEach(func() {
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	clusterNPConfig := func(name string, collectorID int64) *observabilityconfigv1alpha1.ObservabilityConfig {
		return &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: collectorID,
				Features:    []observabilityconfigv1alpha1.FeatureConfig{{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100}},
			},
		}
	}
	nsNPConfig := func(name string, collectorID int64, namespaces []string) *observabilityconfigv1alpha1.ObservabilityConfig {
		c := clusterNPConfig(name, collectorID)
		c.Spec.Filter = &observabilityconfigv1alpha1.Filter{Namespaces: namespaces}
		return c
	}

	It("dispatches an all-namespaces resync when a cluster feature is added, and again (empty) on clear", func() {
		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)

		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{clusterNPConfig("cluster", 42)})).To(Succeed())
		Expect(h.calls).To(HaveLen(1))
		Expect(h.calls[0].feature).To(Equal(libovsdbops.NetworkPolicySample))
		Expect(h.calls[0].namespaces).To(BeNil()) // nil == all namespaces

		manager.clearConfig()
		Expect(h.calls).To(HaveLen(2))
		Expect(h.calls[1].namespaces).To(BeNil())
	})

	It("does not dispatch when re-applying an unchanged config", func() {
		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)

		cfg := clusterNPConfig("cluster", 42)
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cfg})).To(Succeed())
		Expect(h.calls).To(HaveLen(1))

		// Re-apply the identical config: resolution is unchanged, so no resync.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cfg})).To(Succeed())
		Expect(h.calls).To(HaveLen(1))
	})

	It("dispatches a namespace-scoped resync when only a namespaced config changes", func() {
		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)

		cluster := clusterNPConfig("cluster", 42)
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cluster})).To(Succeed())
		Expect(h.calls).To(HaveLen(1))

		// Add a namespace-scoped config: the cluster collectors are unchanged, so only "foo"
		// must be resynced, not every namespace.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cluster, nsNPConfig("ns-foo", 24, []string{"foo"})})).To(Succeed())
		Expect(h.calls).To(HaveLen(2))
		Expect(h.calls[1].namespaces).To(Equal(sets.New("foo")))
	})

	It("does not dispatch to a handler registered for a different feature", func() {
		h := &recordingHandler{}
		// Handler registered for EgressFirewall, but the config only touches NetworkPolicy.
		manager.RegisterResyncHandler(libovsdbops.EgressFirewallSample, h)
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{clusterNPConfig("cluster", 42)})).To(Succeed())
		Expect(h.calls).To(BeEmpty())
	})

	It("buffers a resync dispatched before the handler registers and replays it on registration", func() {
		// Config change lands during the startup window, before the NetworkPolicy handler exists.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{clusterNPConfig("cluster", 42)})).To(Succeed())

		// Registering the handler must replay the buffered (all-namespaces) scope exactly once.
		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)
		Expect(h.calls).To(HaveLen(1))
		Expect(h.calls[0].feature).To(Equal(libovsdbops.NetworkPolicySample))
		Expect(h.calls[0].namespaces).To(BeNil()) // nil == all namespaces

		// The buffer is drained: a second registration replays nothing.
		h2 := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h2)
		Expect(h2.calls).To(BeEmpty())
	})

	It("merges multiple buffered namespace-scoped resyncs and replays their union", func() {
		nsFoo := nsNPConfig("ns-foo", 24, []string{"foo"})
		// Two namespace-scoped changes dispatched before the handler registers, each adding a
		// different namespace. No cluster-scoped config exists, so each delta stays namespace-scoped.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{nsFoo})).To(Succeed())
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{nsFoo, nsNPConfig("ns-bar", 25, []string{"bar"})})).To(Succeed())

		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)
		Expect(h.calls).To(HaveLen(1))
		Expect(h.calls[0].namespaces).To(Equal(sets.New("foo", "bar")))
	})

	It("collapses a buffered all-namespaces scope over buffered namespace scopes", func() {
		nsFoo := nsNPConfig("ns-foo", 24, []string{"foo"})
		// A namespace-scoped change, then a cluster-scoped feature add: "all" must dominate the merge.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{nsFoo})).To(Succeed())
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{nsFoo, clusterNPConfig("cluster", 42)})).To(Succeed())

		h := &recordingHandler{}
		manager.RegisterResyncHandler(libovsdbops.NetworkPolicySample, h)
		Expect(h.calls).To(HaveLen(1))
		Expect(h.calls[0].namespaces).To(BeNil()) // nil == all namespaces
	})
})
