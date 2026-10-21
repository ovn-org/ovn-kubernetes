// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime/debug"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovntls "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/tls"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// MetricServerOptions defines the configuration options for the new MetricServer
type MetricServerOptions struct {
	// Server configuration
	BindAddress string

	// CollectionInterval controls OVS/OVN metric extraction. Positive values use
	// periodic collection, zero collects on demand, and negative values use the
	// default interval.
	CollectionInterval time.Duration

	// TLS configuration
	CertFile        string
	KeyFile         string
	ApplyTLSOptions ovntls.ApplyConfigOptions

	// Feature flags
	EnableOVSMetrics           bool
	EnableOVNDBMetrics         bool
	EnableOVNControllerMetrics bool
	EnableOVNNorthdMetrics     bool
	EnablePprof                bool

	OVSDBClient libovsdbclient.Client

	// OnFatalError is called when an unrecoverable error occurs (e.g., failed to bind to address).
	// If set, it allows the caller to trigger a graceful shutdown.
	OnFatalError func()

	// Prometheus plumbing
	Registerer prometheus.Registerer

	dbFoundViaPath bool
}

// MetricServer represents the new unified metrics server
type MetricServer struct {
	// Configuration
	opts MetricServerOptions

	ovsDbProperties []*util.OvsDbProperties

	// HTTP server
	server *http.Server
	mux    *http.ServeMux

	// Prometheus registry
	registerer prometheus.Registerer
}

// NewMetricServer creates a new MetricServer instance
func NewMetricServer(opts MetricServerOptions) *MetricServer {
	registerer := opts.Registerer
	if registerer == nil {
		registerer = prometheus.NewRegistry()
	}

	server := &MetricServer{
		opts:       opts,
		registerer: registerer,
	}

	server.mux = http.NewServeMux()
	tg := prometheus.ToTransactionalGatherer(server.registerer.(prometheus.Gatherer))
	metricsHandler := promhttp.HandlerForTransactional(tg, promhttp.HandlerOpts{})

	if opts.CollectionInterval == 0 {
		next := metricsHandler
		metricsHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			klog.V(5).Infof("Metrics scrape requested for collecting metrics on-demand")
			server.collect()
			next.ServeHTTP(w, r)
		})
	}

	server.mux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		server.registerer,
		metricsHandler,
	))

	if opts.EnablePprof {
		server.mux.HandleFunc("/debug/pprof/", pprof.Index)
		server.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		server.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		server.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		server.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		// Allow changes to log level at runtime
		server.mux.HandleFunc("/debug/flags/v", stringFlagPutHandler(klogSetter))
	}

	return server
}

// registerMetrics registers the metrics to the OVN registry
func (s *MetricServer) registerMetrics() {
	if s.opts.EnableOVSMetrics {
		klog.Infof("MetricServer registers OVS metrics")
		registerOvsMetrics(s.opts.OVSDBClient, s.registerer)
	}
	if s.opts.EnableOVNDBMetrics {
		klog.Infof("MetricServer registers OVN DB metrics")
		s.ovsDbProperties, s.opts.dbFoundViaPath = RegisterOvnDBMetrics(s.registerer)
	}
	if s.opts.EnableOVNControllerMetrics {
		klog.Infof("MetricServer registers OVN Controller metrics")
		RegisterOvnControllerMetrics(s.registerer)
	}
	if s.opts.EnableOVNNorthdMetrics {
		klog.Infof("MetricServer registers OVN Northd metrics")
		RegisterOvnNorthdMetrics(s.registerer)
	}
}

// updateOvsMetrics updates the OVS metrics
func (s *MetricServer) updateOvsMetrics() {
	ovsDatapathMetricsUpdate()
	if err := updateOvsBridgeMetrics(s.opts.OVSDBClient, util.RunOVSOfctl); err != nil {
		klog.Errorf("Updating ovs bridge metrics failed: %s", err.Error())
	}
	if err := updateOvsInterfaceMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Updating ovs interface metrics failed: %s", err.Error())
	}
	if err := setOvsMemoryMetrics(func(args ...string) (string, string, error) {
		return util.RunOvsVswitchdAppCtlWithTimeout(metricsAppctlTimeout, args...)
	}); err != nil {
		klog.Errorf("Updating ovs memory metrics failed: %s", err.Error())
	}
	if err := setOvsHwOffloadMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Updating ovs hardware offload metrics failed: %s", err.Error())
	}
	coverageShowMetricsUpdate(ovsVswitchd)
}

// updateOvnControllerMetrics updates the OVN Controller metrics
func (s *MetricServer) updateOvnControllerMetrics() {
	if err := setOvnControllerConfigurationMetrics(s.opts.OVSDBClient); err != nil {
		klog.Errorf("Setting ovn controller config metrics failed: %s", err.Error())
	}

	updateOvnControllerIntegrationBridgeMetrics(s.opts.OVSDBClient)
	coverageShowMetricsUpdate(ovnController)
	stopwatchShowMetricsUpdate(ovnController)
	updateSBDBConnectionMetric(func(args ...string) (string, string, error) {
		return util.RunOVNControllerAppCtlWithTimeout(metricsAppctlTimeout, args...)
	})

}

// updateOvnNorthdMetrics updates the OVN Northd metrics
func (s *MetricServer) updateOvnNorthdMetrics() {
	updateOvnNorthdStatusMetrics()
	coverageShowMetricsUpdate(ovnNorthd)
	stopwatchShowMetricsUpdate(ovnNorthd)
}

// updateOvnDBMetrics updates the OVN DB metrics
func (s *MetricServer) updateOvnDBMetrics() {
	for _, dbProperty := range s.ovsDbProperties {
		if s.opts.dbFoundViaPath {
			updateOvnDBSizeMetrics(dbProperty)
		}
		updateOvnDBMemoryMetrics(dbProperty)
	}
}

// collect refreshes all enabled OVS/OVN metric values in the registry. It execs
// ovs-appctl/ovn-appctl subprocesses (each bounded by metricsAppctlTimeout) and
// queries OVSDB. It runs either in runCollectionLoop or during a scrape when
// CollectionInterval is zero. A panic in any metric path only fails the
// current cycle rather than crashing the process.
func (s *MetricServer) collect() {
	defer func() {
		if r := recover(); r != nil {
			klog.Errorf("MetricServer recovered from panic during metric collection: %v\n%s", r, debug.Stack())
		}
	}()

	if s.opts.EnableOVSMetrics {
		s.updateOvsMetrics()
	}
	if s.opts.EnableOVNDBMetrics {
		s.updateOvnDBMetrics()
	}
	if s.opts.EnableOVNControllerMetrics {
		s.updateOvnControllerMetrics()
	}
	if s.opts.EnableOVNNorthdMetrics {
		s.updateOvnNorthdMetrics()
	}
}

// resolveCollectionInterval preserves zero for on-demand collection and falls
// back to defaultCollectionInterval for negative intervals.
func resolveCollectionInterval(d time.Duration) time.Duration {
	if d < 0 {
		return defaultCollectionInterval
	}
	return d
}

// runCollectionLoop refreshes the registry once synchronously (so the first
// scrape has data) and then on every CollectionInterval tick until stopChan is
// closed. A plain ticker loop serializes cycles: an over-running collect just
// delays the next one, so cycles never overlap and no locking is needed.
// Each collect runs in a goroutine so shutdown can proceed without waiting for
// a stalled extraction operation to finish.
func (s *MetricServer) runCollectionLoop(stopChan <-chan struct{}) {
	interval := resolveCollectionInterval(s.opts.CollectionInterval)
	if interval == 0 {
		return
	}
	klog.Infof("MetricServer collection loop running every %s", interval)

	s.collect()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			collectDone := make(chan struct{})
			go func() {
				defer close(collectDone)
				klog.V(5).Infof("Collecting metrics on scheduled interval")
				s.collect()
			}()
			// Wait for this cycle to complete or shutdown to signal. If shutdown
			// signals while collection is in-flight, we exit immediately without
			// waiting; the in-flight goroutine finishes asynchronously.
			select {
			case <-collectDone:
			case <-stopChan:
				return
			}
		case <-stopChan:
			return
		}
	}
}

// Run runs the metrics server and blocks until graceful shutdown
func (s *MetricServer) Run(stopChan <-chan struct{}) {
	utilwait.Until(func() {
		s.server = &http.Server{
			Addr:    s.opts.BindAddress,
			Handler: s.mux,
		}
		listenAndServe := func() error { return s.server.ListenAndServe() }
		if s.opts.CertFile != "" && s.opts.KeyFile != "" {
			s.server.TLSConfig = &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
					cert, err := tls.LoadX509KeyPair(s.opts.CertFile, s.opts.KeyFile)
					if err != nil {
						return nil, fmt.Errorf("error generating x509 certs for metrics TLS endpoint: %v", err)
					}
					return &cert, nil
				},
			}

			if s.opts.ApplyTLSOptions != nil {
				s.opts.ApplyTLSOptions(s.server.TLSConfig)
			}

			listenAndServe = func() error { return s.server.ListenAndServeTLS("", "") }
		}

		errCh := make(chan error)
		go func() {
			klog.Infof("Metric Server starts to listen on %s", s.opts.BindAddress)
			errCh <- listenAndServe()
		}()

		select {
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				utilruntime.HandleError(fmt.Errorf("failed while running metrics server at address %q: %w", s.opts.BindAddress, err))
				if s.opts.OnFatalError != nil {
					s.opts.OnFatalError()
				}
			}
		case <-stopChan:
			klog.Infof("Stopping metrics server at address %q", s.opts.BindAddress)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				klog.Errorf("Error stopping metrics server at address %q: %v", s.opts.BindAddress, err)
			}
		}
	}, 5*time.Second, stopChan)
}
