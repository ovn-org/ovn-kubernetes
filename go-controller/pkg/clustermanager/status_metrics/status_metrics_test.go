// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package statusmetrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStatusMetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Status Metrics Suite")
}

var _ = Describe("AggregateFromSamples", func() {
	It("returns incomplete when relevant set is empty", func() {
		outcome, failing := AggregateFromSamples(nil, sets.New[string]())
		Expect(outcome).To(Equal(OutcomeIncomplete))
		Expect(failing).To(BeEmpty())
	})

	It("returns incomplete when a relevant node has no series", func() {
		samples := []Sample{{Labels: map[string]string{"node": "n1"}, Value: 1}}
		outcome, _ := AggregateFromSamples(samples, sets.New("n1", "n2"))
		Expect(outcome).To(Equal(OutcomeIncomplete))
	})

	It("returns success when all relevant nodes succeeded", func() {
		samples := []Sample{
			{Labels: map[string]string{"node": "n1"}, Value: 1},
			{Labels: map[string]string{"node": "n2"}, Value: 1},
		}
		outcome, failing := AggregateFromSamples(samples, sets.New("n1", "n2"))
		Expect(outcome).To(Equal(OutcomeSuccess))
		Expect(failing).To(BeEmpty())
	})

	It("returns failure with sorted failing nodes and ignores stale nodes", func() {
		samples := []Sample{
			{Labels: map[string]string{"node": "n1"}, Value: 1},
			{Labels: map[string]string{"node": "n2"}, Value: 0},
			{Labels: map[string]string{"node": "deleted"}, Value: 0},
		}
		outcome, failing := AggregateFromSamples(samples, sets.New("n1", "n2"))
		Expect(outcome).To(Equal(OutcomeFailure))
		Expect(failing).To(Equal([]string{"n2"}))
	})
})

var _ = Describe("FormatFailingNodesMessage", func() {
	It("caps at five sorted names", func() {
		msg := FormatFailingNodesMessage([]string{"z", "a", "m", "b", "c", "d"})
		Expect(msg).To(Equal("failing nodes: a, b, c, d, m"))
	})
})

var _ = Describe("Availability", func() {
	It("reports unavailable when URL is empty", func() {
		a := NewAvailability(NewClient(""), time.Second)
		Expect(a.Available(context.Background())).To(BeFalse())
	})

	It("probes and caches availability", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]}]}}`))
		}))
		DeferCleanup(srv.Close)

		a := NewAvailability(NewClient(srv.URL), time.Hour)
		Expect(a.Available(context.Background())).To(BeTrue())
		// Second call should use cache (server can be closed)
		srv.Close()
		Expect(a.Available(context.Background())).To(BeTrue())
	})

	It("reports unavailable when probe fails", func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		DeferCleanup(srv.Close)

		a := NewAvailability(NewClient(srv.URL), time.Millisecond)
		Expect(a.Available(context.Background())).To(BeFalse())
	})
})

var _ = Describe("Client.Query", func() {
	It("preserves a path prefix on the configured base URL", func() {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}))
		DeferCleanup(srv.Close)

		client := NewClient(srv.URL + "/prometheus")
		_, err := client.Query(context.Background(), "up")
		Expect(err).NotTo(HaveOccurred())
		Expect(gotPath).To(Equal("/prometheus/api/v1/query"))
	})
})

var _ = Describe("BuildSyncSucceededQuery", func() {
	It("builds namespaced selectors", func() {
		Expect(BuildSyncSucceededQuery("ovnkube_egressfirewall_sync_succeeded", "ns", "ef")).
			To(Equal(`ovnkube_egressfirewall_sync_succeeded{namespace="ns",name="ef"}`))
	})

	It("builds cluster-scoped selectors", func() {
		Expect(BuildSyncSucceededQuery("ovnkube_adminnetworkpolicy_sync_succeeded", "", "anp")).
			To(Equal(`ovnkube_adminnetworkpolicy_sync_succeeded{name="anp"}`))
	})
})
