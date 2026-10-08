// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/mock"

	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = BeforeSuite(func() {
	// Register OVN-Kube controller base metrics into the default registry once for all tests
	RegisterOVNKubeControllerBase()
})

var _ = Describe("MetricServer", func() {
	t := newMetricsTestDriver()

	DescribeTableSubtree("",
		func(desc string, tc metricsTestCase, registerer prometheus.Registerer) {
			BeforeEach(func() {
				t.opts.Registerer = registerer
				t.opts.EnableOVSMetrics = tc.enableOVS
				t.opts.EnableOVNDBMetrics = tc.enableOVNDB
				t.opts.EnableOVNControllerMetrics = tc.enableOVNController
				t.opts.EnableOVNNorthdMetrics = tc.enableOVNNorthd

				savedEnableScaleMetrics := config.Metrics.EnableScaleMetrics
				config.Metrics.EnableScaleMetrics = tc.enableScaleMetrics
				DeferCleanup(func() {
					config.Metrics.EnableScaleMetrics = savedEnableScaleMetrics
				})

				// Mock the exec runner for RunOvsVswitchdAppCtl calls
				mockCmd := new(mock_k8s_io_utils_exec.Cmd)
				mockExecRunner := new(mocks.ExecRunner)
				util.RunCmdExecRunner = mockExecRunner
				ovntest.ProcessMockFnList(&mockExecRunner.Mock, tc.mockRunCommands)

				mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
				mockKexecIface.Mock.On("Command", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Maybe().Return(mockCmd)
				mockKexecIface.Mock.On("Command", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Maybe().Return(mockCmd)
				mockKexecIface.Mock.On("Command", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything,
					mock.Anything).Maybe().Return(mockCmd)
				mockKexecIface.Mock.On("LookPath", "ip").Return("ip", nil).Once()
				mockKexecIface.Mock.On("LookPath", "sysctl").Return("sysctl", nil).Once()
				Expect(util.SetExecWithoutOVS(mockKexecIface)).To(Succeed())

				DeferCleanup(func() {
					// Reset exec interface to avoid test pollution
					util.ResetRunner()
				})
			})

			It(fmt.Sprintf("should handle %s metrics", desc), func() {
				// Extraction runs out of band, so some GaugeVec series only appear
				// after the background loop's first collect() has Set() them. Retry
				// the whole fetch+compare until the registry has converged.
				Eventually(func(g Gomega) {
					// Make actual HTTP request to the /metrics endpoint
					metricsURL := fmt.Sprintf("http://%s/metrics", t.opts.BindAddress)
					resp, err := http.Get(metricsURL)
					g.Expect(err).NotTo(HaveOccurred())
					defer resp.Body.Close()

					g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

					body, err := io.ReadAll(resp.Body)
					g.Expect(err).NotTo(HaveOccurred())

					response := string(body)
					g.Expect(response).NotTo(BeEmpty())

					gotMetrics := []string{}
					for _, line := range strings.Split(response, "\n") {
						if strings.HasPrefix(line, "# TYPE ") {
							m := strings.Split(line, " ")[2]
							gotMetrics = append(gotMetrics, m)
						}
					}

					diff := cmp.Diff(gotMetrics, tc.expectedMetrics, cmpopts.SortSlices(func(x, y string) bool {
						return x < y
					}))
					g.Expect(diff).To(BeEmpty(), "metrics mismatch (-got +want):\n%s", diff)
				}).Within(5 * time.Second).Should(Succeed())
			})
		},

		Entry("", "OVS", OVSMetricsTestCase, nil),
		Entry("", "OVN DB", OVNDBMetricsTestCase, nil),
		Entry("", "OVN Controller", OVNControllerMetricsTestCase, nil),
		Entry("", "OVN Northd", OVNNorthdMetricsTestCase, nil),
		Entry("", "default registry", DefaultMetricsTestCase, prometheus.DefaultRegisterer),
	)

	When("EnablePprof", func() {
		BeforeEach(func() {
			t.opts.EnablePprof = true
		})

		It("should serve pprof endpoints", func() {
			baseURL := fmt.Sprintf("http://%s", t.opts.BindAddress)

			// Test the pprof index endpoint
			Eventually(func(g Gomega) {
				resp, err := http.Get(baseURL + "/debug/pprof/")
				g.Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
			}).Within(5 * time.Second).Should(Succeed())

			// Test the log level endpoint
			Eventually(func(g Gomega) {
				req, err := http.NewRequest("PUT", baseURL+"/debug/flags/v", strings.NewReader("2"))
				g.Expect(err).NotTo(HaveOccurred())

				resp, err := http.DefaultClient.Do(req)
				g.Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				g.Expect(resp.StatusCode).To(Equal(http.StatusOK))

				body, err := io.ReadAll(resp.Body)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(body)).To(ContainSubstring("successfully set klog.logging.verbosity to 2"))
			}).Within(5 * time.Second).Should(Succeed())
		})
	})

	Context("", func() {
		var onFatalInvoked <-chan struct{}

		BeforeEach(func() {
			t.opts.BindAddress = "127.0.0.5:9410"

			// Occupy the port first so that the metrics server will fail with "address already in use"
			listener, err := net.Listen("tcp", t.opts.BindAddress)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(listener.Close)

			var ctx context.Context

			ctx, t.opts.OnFatalError = context.WithCancel(context.Background())
			onFatalInvoked = ctx.Done()
		})

		It("should call OnFatalError when failing to bind", func() {
			// Wait for OnFatalError to be called (context cancelled) or timeout
			Eventually(onFatalInvoked).Within(5 * time.Second).Should(BeClosed())
		})
	})

	When("TLS is enabled", func() {
		BeforeEach(func() {
			// Create temporary cert and key files
			var err error
			t.opts.CertFile, t.opts.KeyFile, err = ovntest.CreateTestCertificateFiles()
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = os.Remove(t.opts.CertFile)
				_ = os.Remove(t.opts.KeyFile)
			})
		})

		Context("with no applied TLS config options", func() {
			It("should default to TLS 1.2", func() {
				t.testTLSHandshake(tls.VersionTLS12, nil, func(g Gomega, state tls.ConnectionState, err error) {
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(state.Version).To(BeNumerically("==", tls.VersionTLS12))
				})
			})

			It("should reject TLS 1.1 and earlier", func() {
				t.testTLSHandshake(tls.VersionTLS11, nil, func(g Gomega, _ tls.ConnectionState, err error) {
					g.Expect(err).To(HaveOccurred())
					g.Expect(err.Error()).To(Or(
						ContainSubstring("protocol version"),
						ContainSubstring("handshake failure"),
						ContainSubstring("tls: no supported versions"),
					))
				})
			})
		})

		Context("with applied TLS ciphers", func() {
			BeforeEach(func() {
				t.opts.ApplyTLSOptions = func(tlsConfig *tls.Config) {
					tlsConfig.MinVersion = tls.VersionTLS12
					// Configure server to explicitly NOT accept ChaCha20-Poly1305
					// Only accept AES-based cipher suites
					tlsConfig.CipherSuites = []uint16{
						tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
						tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, // Required for HTTP/2
					}
				}
			})

			It("should accept allowed cipher suites", func() {
				// Client offers an AES cipher suite that the server explicitly allows
				t.testTLSHandshake(tls.VersionTLS12, []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				}, func(g Gomega, state tls.ConnectionState, err error) {
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(state.CipherSuite).To(Equal(tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256))
				})
			})

			It("should reject disallowed cipher suites", func() {
				// Client offers ONLY ChaCha20-Poly1305, which server explicitly excludes so handshake should fail.
				t.testTLSHandshake(tls.VersionTLS12, []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
				}, func(g Gomega, _ tls.ConnectionState, err error) {
					g.Expect(err).To(HaveOccurred())
					g.Expect(err.Error()).To(ContainSubstring("handshake failure"))
				})
			})
		})

		Context("with applied TLS minimum version", func() {
			BeforeEach(func() {
				t.opts.ApplyTLSOptions = func(tlsConfig *tls.Config) {
					tlsConfig.MinVersion = tls.VersionTLS13
				}
			})

			It("should reject versions below the minimum", func() {
				for clientVersion := tls.VersionTLS11; clientVersion <= tls.VersionTLS12; clientVersion++ {
					t.testTLSHandshake(clientVersion, nil, func(g Gomega, _ tls.ConnectionState, err error) {
						g.Expect(err).To(HaveOccurred())
						g.Expect(err.Error()).To(Or(
							ContainSubstring("protocol version"),
							ContainSubstring("handshake failure"),
							ContainSubstring("tls: no supported versions"),
						))
					})
				}
			})
		})
	})
})

// blockingExecRunner blocks the collection commands (coverage/show,
// stopwatch/show) on the release channel to simulate a saturated daemon that
// cannot answer, while returning immediately for every other command. It lets a
// background collection cycle stall so we can prove a scrape still returns
// promptly instead of waiting on extraction.
type blockingExecRunner struct {
	release chan struct{}
}

func (r *blockingExecRunner) RunCmd(_ kexec.Cmd, _ string, _ []string, args ...string) (*bytes.Buffer, *bytes.Buffer, error) {
	for _, a := range args {
		switch a {
		case "coverage/show", "stopwatch/show":
			<-r.release
		}
	}
	return bytes.NewBuffer(nil), bytes.NewBuffer(nil), nil
}

var _ = Describe("scrape decoupled from extraction", func() {
	It("serves the registry promptly while a collection cycle is stalled", func() {
		savedRunner := util.RunCmdExecRunner
		DeferCleanup(func() { util.RunCmdExecRunner = savedRunner })

		setupAppFs()

		// Minimal OVS DB so the ovn-controller registration/config metrics resolve.
		ovsVersion := "2.17.0"
		br := vswitchd.Bridge{Name: "br-int", UUID: buildUUID()}
		testDB := []libovsdbtest.TestData{
			&vswitchd.Bridge{UUID: br.UUID, Name: br.Name},
			&vswitchd.OpenvSwitch{
				UUID:       buildUUID(),
				OVSVersion: &ovsVersion,
				Bridges:    []string{br.UUID},
				ExternalIDs: map[string]string{
					"ovn-remote": "unix:/var/run/ovn/ovnsb_db.sock",
				},
			},
		}
		ovsClient, cleanup, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{OVSData: testDB})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(cleanup.Cleanup)

		// Build a mock kexec interface so the appctl wrappers can construct a Cmd;
		// the actual exec is intercepted by blockingExecRunner below.
		mockCmd := new(mock_k8s_io_utils_exec.Cmd)
		mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
		for _, n := range []int{3, 4, 5, 6} {
			anys := make([]interface{}, n)
			for i := range anys {
				anys[i] = mock.Anything
			}
			mockKexecIface.Mock.On("Command", anys...).Maybe().Return(mockCmd)
		}
		mockKexecIface.Mock.On("LookPath", "ip").Return("ip", nil).Once()
		mockKexecIface.Mock.On("LookPath", "sysctl").Return("sysctl", nil).Once()
		Expect(util.SetExecWithoutOVS(mockKexecIface)).To(Succeed())
		DeferCleanup(util.ResetRunner)

		// The collection path (coverage/show) blocks until released; everything
		// else returns immediately.
		runner := &blockingExecRunner{release: make(chan struct{})}
		util.RunCmdExecRunner = runner

		opts := MetricServerOptions{
			EnableOVNControllerMetrics: true,
			OVSDBClient:                ovsClient,
			Registerer:                 prometheus.NewRegistry(),
		}
		server := NewMetricServer(opts)
		server.registerMetrics()

		// Start the background collection loop; its first cycle stalls on
		// coverage/show and never completes until released.
		stop := make(chan struct{})
		loopDone := make(chan struct{})
		go func() {
			server.runCollectionLoop(stop)
			close(loopDone)
		}()

		ts := httptest.NewServer(server.mux)
		DeferCleanup(ts.Close)

		// Even though extraction is stuck, the scrape only serializes the registry,
		// so it must return promptly rather than waiting on the stalled cycle.
		start := time.Now()
		resp, err := http.Get(ts.URL + "/metrics")
		Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		elapsed := time.Since(start)

		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(body).NotTo(BeEmpty())
		Expect(elapsed).To(BeNumerically("<", time.Second),
			"scrape blocked on stalled extraction instead of serving the registry")

		// Release the stalled cycle and let the loop stop before the DeferCleanups
		// restore the process-global exec runner, otherwise cleanup races the loop.
		close(runner.release)
		close(stop)
		Eventually(loopDone).Within(5 * time.Second).Should(BeClosed())
	})
})

var _ = Describe("resolveCollectionInterval", func() {
	It("clamps non-positive intervals to the default", func() {
		Expect(resolveCollectionInterval(0)).To(Equal(defaultCollectionInterval))
		Expect(resolveCollectionInterval(-5 * time.Second)).To(Equal(defaultCollectionInterval))
	})
	It("keeps a positive interval as configured", func() {
		Expect(resolveCollectionInterval(10 * time.Second)).To(Equal(10 * time.Second))
	})
})

type metricsTestDriver struct {
	libovsdbCleanup *libovsdbtest.Context
	opts            MetricServerOptions
}

func newMetricsTestDriver() *metricsTestDriver {
	t := &metricsTestDriver{}

	BeforeEach(func() {
		// disable Process metrics collector to avoid the test flakiness
		savedUnprivileged := config.UnprivilegedMode
		config.UnprivilegedMode = true

		savedRunner := util.RunCmdExecRunner

		t.opts = MetricServerOptions{
			BindAddress: fmt.Sprintf("127.0.0.1:%d", getFreePort()),
		}

		setupAppFs()

		// common OVS DB setup
		ovsVersion := "2.17.0"

		intf1 := vswitchd.Interface{Name: "porta", UUID: buildUUID()}
		port1 := vswitchd.Port{Name: "porta", UUID: buildUUID()}
		br1 := vswitchd.Bridge{Name: "br-int", UUID: buildUUID()}

		testDB := []libovsdbtest.TestData{
			&vswitchd.Interface{
				UUID: intf1.UUID,
				Name: intf1.Name,
				Statistics: map[string]int{
					"rx_packets": 1000,
					"tx_packets": 800,
					"rx_bytes":   100000,
					"tx_bytes":   80000,
				},
			},
			&vswitchd.Port{UUID: port1.UUID, Name: port1.Name, Interfaces: []string{intf1.UUID}},
			&vswitchd.Bridge{UUID: br1.UUID, Name: br1.Name, Ports: []string{port1.UUID}},
			&vswitchd.OpenvSwitch{
				UUID:       buildUUID(),
				OVSVersion: &ovsVersion,
				Bridges:    []string{br1.UUID},
				ExternalIDs: map[string]string{
					"ovn-bridge-remote-probe-interval": "100",
					"ovn-remote-probe-interval":        "200",
					"ovn-monitor-all":                  "false",
					"ovn-encap-ip":                     "192.168.1.1",
					"ovn-encap-type":                   "geneve",
					"ovn-remote":                       "unix:/var/run/ovn/ovnsb_db.sock",
					"ovn-k8s-node-port":                "false",
					"ovn-bridge-mappings":              "physnet:breth0",
				},
			},
		}

		// Setup OVS test harness
		dbSetup := libovsdbtest.TestSetup{
			OVSData: testDB,
		}

		var err error
		t.opts.OVSDBClient, t.libovsdbCleanup, err = libovsdbtest.NewOVSTestHarness(dbSetup)
		Expect(err).NotTo(HaveOccurred())

		// Set a sample metric value to ensure the metric is initialized
		MetricOVNKubeControllerSyncDuration.WithLabelValues("pods").Set(0)

		DeferCleanup(func() {
			config.UnprivilegedMode = savedUnprivileged
			util.RunCmdExecRunner = savedRunner

			if t.libovsdbCleanup != nil {
				t.libovsdbCleanup.Cleanup()
			}
		})
	})

	JustBeforeEach(func() {
		server := NewMetricServer(t.opts)
		server.registerMetrics()

		ctx, cancel := context.WithCancel(context.Background())

		serverDone := make(chan struct{})
		go func() {
			server.Run(ctx.Done())
			close(serverDone)
		}()

		// Extraction runs out of band from scraping (as in production). The loop
		// does one synchronous collect then ticks; the test's Eventually on
		// /metrics waits for that first collect to populate the registry.
		collectionDone := make(chan struct{})
		go func() {
			server.runCollectionLoop(ctx.Done())
			close(collectionDone)
		}()

		DeferCleanup(func() {
			shutdownStart := time.Now()
			cancel()

			// Wait for server to stop with timeout
			Eventually(serverDone, 10*time.Second).Should(BeClosed())

			// Wait for the collection loop to stop before later DeferCleanups
			// restore the process-global exec runner.
			Eventually(collectionDone, 10*time.Second).Should(BeClosed())

			// Validate shutdown was reasonably fast (should be under 6 seconds, allowing for 5s grace period)
			Expect(time.Since(shutdownStart)).To(BeNumerically("<", 6*time.Second))
		})
	})

	return t
}

func (t *metricsTestDriver) testTLSHandshake(clientVersion int, clientCipherSuites []uint16, verify func(Gomega, tls.ConnectionState, error)) {
	// Load the server's certificate to create a trusted cert pool
	certPool := x509.NewCertPool()
	certPEM, err := os.ReadFile(t.opts.CertFile)
	Expect(err).NotTo(HaveOccurred())
	Expect(certPool.AppendCertsFromPEM(certPEM)).To(BeTrue())

	tlsConfig := &tls.Config{
		RootCAs:      certPool,
		ServerName:   "localhost", // matches the cert's CommonName/DNSNames
		MinVersion:   uint16(clientVersion),
		MaxVersion:   uint16(clientVersion),
		CipherSuites: clientCipherSuites,
	}

	Eventually(func(g Gomega) {
		conn, err := tls.Dial("tcp", t.opts.BindAddress, tlsConfig)
		if err != nil {
			verify(g, tls.ConnectionState{}, err)
			return
		}
		defer conn.Close()

		state := conn.ConnectionState()
		g.Expect(state.HandshakeComplete).To(BeTrue())
		verify(g, state, err)
	}).Within(5 * time.Second).Should(Succeed())
}

func setupAppFs() {
	prevFS := util.AppFs
	util.AppFs = afero.NewMemMapFs()
	DeferCleanup(func() {
		util.AppFs = prevFS
	})

	err := util.AppFs.MkdirAll("/var/run/openvswitch/", 0o755)
	Expect(err).NotTo(HaveOccurred())

	files := []ovntest.AferoFileMockHelper{
		{FileName: "/var/run/openvswitch/ovs-vswitchd.pid", Permissions: 0o755, Content: []byte("101")},
		{FileName: "ovn-controller.pid", Permissions: 0o755, Content: []byte("102")},
		{FileName: "ovn-northd.pid", Permissions: 0o755, Content: []byte("103")},
		{FileName: "/var/run/openvswitch/ovnnb_db.sock", Permissions: 0o755, Content: []byte("")},
		{FileName: "/var/run/openvswitch/ovnsb_db.sock", Permissions: 0o755, Content: []byte("")},
		{FileName: "/etc/ovn/ovnnb_db.db", Permissions: 0o755, Content: []byte("abcdefgh")},
		{FileName: "/etc/ovn/ovnsb_db.db", Permissions: 0o755, Content: []byte("xyz")},
	}

	for _, file := range files {
		err := afero.WriteFile(util.AppFs, file.FileName, file.Content, file.Permissions)
		Expect(err).NotTo(HaveOccurred())
	}
}

func getFreePort() int {
	listener, err := net.Listen("tcp", "localhost:0")
	Expect(err).NotTo(HaveOccurred())

	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	return port
}

type metricsTestCase struct {
	enableOVS           bool
	enableOVNDB         bool
	enableOVNController bool
	enableOVNNorthd     bool
	enableScaleMetrics  bool
	mockRunCommands     []ovntest.TestifyMockHelper
	expectedMetrics     []string
}

var (
	OVSMetricsTestCase = metricsTestCase{
		enableOVS: true,
		mockRunCommands: []ovntest.TestifyMockHelper{
			// dpctl/dump-dps
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "dpctl/dump-dps"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("system@ovs-system")), bytes.NewBuffer([]byte("")), nil},
			},
			// dpctl/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "dpctl/show", "system@ovs-system"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(dpctlShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// memory/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "memory/show"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovsMemoryShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// coverage/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "coverage/show"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(coverageShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// ovs-ofctl dump-aggregate br-int
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "dump-aggregate", "br-int"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovsOfctlDumpAggregateOutput)), bytes.NewBuffer([]byte("")), nil},
			},
		},
		expectedMetrics: []string{
			"ovs_build_info",
			"ovs_vswitchd_bridge_flows_total",
			"ovs_vswitchd_bridge_ports_total",
			"ovs_vswitchd_bridge_reconfigure",
			"ovs_vswitchd_bridge_total",
			"ovs_vswitchd_bridge",
			"ovs_vswitchd_dp_flows_lookup_hit",
			"ovs_vswitchd_dp_flows_lookup_lost",
			"ovs_vswitchd_dp_flows_lookup_missed",
			"ovs_vswitchd_dp_flows_total",
			"ovs_vswitchd_dp_if_total",
			"ovs_vswitchd_dp_masks_hit_ratio",
			"ovs_vswitchd_dp_masks_hit",
			"ovs_vswitchd_dp_masks_total",
			"ovs_vswitchd_dp_packets_total",
			"ovs_vswitchd_dp_total",
			"ovs_vswitchd_dp",
			"ovs_vswitchd_dpif_execute",
			"ovs_vswitchd_dpif_flow_del",
			"ovs_vswitchd_dpif_flow_flush",
			"ovs_vswitchd_dpif_flow_get",
			"ovs_vswitchd_dpif_flow_put",
			"ovs_vswitchd_dpif_port_add",
			"ovs_vswitchd_dpif_port_del",
			"ovs_vswitchd_handlers_total",
			"ovs_vswitchd_hw_offload",
			"ovs_vswitchd_interface_collisions_total",
			"ovs_vswitchd_interface_resets_total",
			"ovs_vswitchd_interface_rx_dropped_total",
			"ovs_vswitchd_interface_rx_errors_total",
			"ovs_vswitchd_interface_tx_dropped_total",
			"ovs_vswitchd_interface_tx_errors_total",
			"ovs_vswitchd_interface_up_wait_seconds_total",
			"ovs_vswitchd_interfaces_total",
			"ovs_vswitchd_netlink_overflow",
			"ovs_vswitchd_netlink_received",
			"ovs_vswitchd_netlink_recv_jumbo",
			"ovs_vswitchd_netlink_sent",
			"ovs_vswitchd_ofproto_dpif_expired",
			"ovs_vswitchd_ofproto_flush",
			"ovs_vswitchd_ofproto_packet_out",
			"ovs_vswitchd_ofproto_recv_openflow",
			"ovs_vswitchd_ofproto_reinit_ports",
			"ovs_vswitchd_packet_in_drop",
			"ovs_vswitchd_packet_in",
			"ovs_vswitchd_pstream_open",
			"ovs_vswitchd_rconn_discarded",
			"ovs_vswitchd_rconn_overflow",
			"ovs_vswitchd_rconn_queued",
			"ovs_vswitchd_rconn_sent",
			"ovs_vswitchd_revalidators_total",
			"ovs_vswitchd_stream_open",
			"ovs_vswitchd_tc_policy",
			"ovs_vswitchd_txn_aborted",
			"ovs_vswitchd_txn_error",
			"ovs_vswitchd_txn_incomplete",
			"ovs_vswitchd_txn_success",
			"ovs_vswitchd_txn_try_again",
			"ovs_vswitchd_txn_unchanged",
			"ovs_vswitchd_txn_uncommitted",
			"ovs_vswitchd_upcall_flow_limit_hit",
			"ovs_vswitchd_upcall_flow_limit_kill",
			"ovs_vswitchd_vconn_open",
			"ovs_vswitchd_vconn_received",
			"ovs_vswitchd_vconn_sent",
			"ovs_vswitchd_xlate_actions_oversize",
			"ovs_vswitchd_xlate_actions_too_many_output",
			"ovs_vswitchd_xlate_actions",
			"promhttp_metric_handler_requests_in_flight",
			"promhttp_metric_handler_requests_total",
		},
	}

	OVNDBMetricsTestCase = metricsTestCase{
		enableOVNDB: true,
		mockRunCommands: []ovntest.TestifyMockHelper{
			// ovs-appctl version
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "version"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("ovsdb-server (Open vSwitch) 3.5.0")), bytes.NewBuffer([]byte("")), nil},
			},
			// ovsdb-client  get-schema-version unix:/var/run/openvswitch/ovnnb_db.sock OVN_Northbound
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "get-schema-version", mock.AnythingOfType("string"), "OVN_Northbound"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("7.11.0")), bytes.NewBuffer([]byte("")), nil},
			},
			// ovsdb-client  get-schema-version unix:/var/run/openvswitch/ovnnb_db.sock OVN_Southbound
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "get-schema-version", mock.AnythingOfType("string"), "OVN_Southbound"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("20.41.0")), bytes.NewBuffer([]byte("")), nil},
			},
			// ovs-appctl  -t /var/run/openvswitch/ovnnb_db.ctl memory/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), mock.AnythingOfType("string"), "memory/show"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnDBMemoryShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// ovs-appctl  -t /var/run/openvswitch/ovnsb_db.ctl memory/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), mock.AnythingOfType("string"), "memory/show"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnDBMemoryShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
		},
		expectedMetrics: []string{
			"ovn_db_build_info",
			"ovn_db_db_size_bytes",
			"ovn_db_jsonrpc_server_sessions",
			"ovn_db_ovsdb_monitors",
			"promhttp_metric_handler_requests_in_flight",
			"promhttp_metric_handler_requests_total",
		},
	}

	OVNControllerMetricsTestCase = metricsTestCase{
		enableOVNController: true,
		enableScaleMetrics:  true,
		mockRunCommands: []ovntest.TestifyMockHelper{
			// ovs-ofctl -t 5 dump-aggregate br-int
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "dump-aggregate", "br-int"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnControllerDumpAggregateOutput)), bytes.NewBuffer([]byte("")), nil},
				CallTimes:  2,
			},
			// ovs-appctl -t /var/run/openvswitch/ovn-controller.113.ctl coverage/show
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "coverage/show"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnControllercoverageShowOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// ovs-appctl -t  /var/run/openvswitch/ovn-controller.113.ctl version
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "version"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnControllerVersionOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// ovs-appctl -t  /var/run/openvswitch/ovn-controller.113.ctl connection-status
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "connection-status"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("connected")), bytes.NewBuffer([]byte("")), nil},
			},
		},
		expectedMetrics: []string{
			"ovn_controller_bfd_run_95th_percentile",
			"ovn_controller_bfd_run_long_term_avg",
			"ovn_controller_bfd_run_maximum",
			"ovn_controller_bfd_run_minimum",
			"ovn_controller_bfd_run_short_term_avg",
			"ovn_controller_bfd_run_total_samples",
			"ovn_controller_bridge_mappings",
			"ovn_controller_build_info",
			"ovn_controller_ct_zone_commit_95th_percentile",
			"ovn_controller_ct_zone_commit_long_term_avg",
			"ovn_controller_ct_zone_commit_maximum",
			"ovn_controller_ct_zone_commit_minimum",
			"ovn_controller_ct_zone_commit_short_term_avg",
			"ovn_controller_ct_zone_commit_total_samples",
			"ovn_controller_encap_ip",
			"ovn_controller_encap_type",
			"ovn_controller_flow_generation_95th_percentile",
			"ovn_controller_flow_generation_long_term_avg",
			"ovn_controller_flow_generation_maximum",
			"ovn_controller_flow_generation_minimum",
			"ovn_controller_flow_generation_short_term_avg",
			"ovn_controller_flow_generation_total_samples",
			"ovn_controller_flow_installation_95th_percentile",
			"ovn_controller_flow_installation_long_term_avg",
			"ovn_controller_flow_installation_maximum",
			"ovn_controller_flow_installation_minimum",
			"ovn_controller_flow_installation_short_term_avg",
			"ovn_controller_flow_installation_total_samples",
			"ovn_controller_if_status_mgr_run_95th_percentile",
			"ovn_controller_if_status_mgr_run_long_term_avg",
			"ovn_controller_if_status_mgr_run_maximum",
			"ovn_controller_if_status_mgr_run_minimum",
			"ovn_controller_if_status_mgr_run_short_term_avg",
			"ovn_controller_if_status_mgr_run_total_samples",
			"ovn_controller_if_status_mgr_update_95th_percentile",
			"ovn_controller_if_status_mgr_update_long_term_avg",
			"ovn_controller_if_status_mgr_update_maximum",
			"ovn_controller_if_status_mgr_update_minimum",
			"ovn_controller_if_status_mgr_update_short_term_avg",
			"ovn_controller_if_status_mgr_update_total_samples",
			"ovn_controller_integration_bridge_geneve_ports",
			"ovn_controller_integration_bridge_openflow_total",
			"ovn_controller_integration_bridge_patch_ports",
			"ovn_controller_lflow_run",
			"ovn_controller_monitor_all",
			"ovn_controller_netlink_overflow",
			"ovn_controller_netlink_received",
			"ovn_controller_netlink_recv_jumbo",
			"ovn_controller_netlink_sent",
			"ovn_controller_ofctrl_seqno_run_95th_percentile",
			"ovn_controller_ofctrl_seqno_run_long_term_avg",
			"ovn_controller_ofctrl_seqno_run_maximum",
			"ovn_controller_ofctrl_seqno_run_minimum",
			"ovn_controller_ofctrl_seqno_run_short_term_avg",
			"ovn_controller_ofctrl_seqno_run_total_samples",
			"ovn_controller_openflow_probe_interval_seconds",
			"ovn_controller_packet_in_drop",
			"ovn_controller_packet_in",
			"ovn_controller_patch_run_95th_percentile",
			"ovn_controller_patch_run_long_term_avg",
			"ovn_controller_patch_run_maximum",
			"ovn_controller_patch_run_minimum",
			"ovn_controller_patch_run_short_term_avg",
			"ovn_controller_patch_run_total_samples",
			"ovn_controller_pinctrl_run_95th_percentile",
			"ovn_controller_pinctrl_run_long_term_avg",
			"ovn_controller_pinctrl_run_maximum",
			"ovn_controller_pinctrl_run_minimum",
			"ovn_controller_pinctrl_run_short_term_avg",
			"ovn_controller_pinctrl_run_total_samples",
			"ovn_controller_rconn_discarded",
			"ovn_controller_rconn_overflow",
			"ovn_controller_rconn_queued",
			"ovn_controller_rconn_sent",
			"ovn_controller_remote_probe_interval_seconds",
			"ovn_controller_sb_connection_method",
			"ovn_controller_southbound_database_connected",
			"ovn_controller_stream_open",
			"ovn_controller_txn_aborted",
			"ovn_controller_txn_error",
			"ovn_controller_txn_incomplete",
			"ovn_controller_txn_success",
			"ovn_controller_txn_try_again",
			"ovn_controller_txn_unchanged",
			"ovn_controller_txn_uncommitted",
			"ovn_controller_vconn_open",
			"ovn_controller_vconn_received",
			"ovn_controller_vconn_sent",
			"promhttp_metric_handler_requests_in_flight",
			"promhttp_metric_handler_requests_total",
		},
	}

	OVNNorthdMetricsTestCase = metricsTestCase{
		enableOVNNorthd: true,
		mockRunCommands: []ovntest.TestifyMockHelper{
			// ovs-appctl -t /var/run/openvswitch/ovn-northd.152.ctl version
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "version"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ovnNorthdVersionOutput)), bytes.NewBuffer([]byte("")), nil},
				CallTimes:  1,
			},
			// ovs-appctl -t /var/run/openvswitch/ovn-northd.152.ctl status
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "status"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("Status: standby")), bytes.NewBuffer([]byte("")), nil},
				CallTimes:  2,
			},
			// ovs-appctl -t /var/run/openvswitch/ovn-northd.152.ctl sb-connection-status
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "sb-connection-status"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("connected")), bytes.NewBuffer([]byte("")), nil},
				CallTimes:  2,
			},
			// ovs-appctl -t /var/run/openvswitch/ovn-northd.152.ctl nb-connection-status
			{
				OnCallMethodName: "RunCmd",
				OnCallMethodArgs: []interface{}{mock.AnythingOfType("*mocks.Cmd"), mock.AnythingOfType("string"),
					mock.AnythingOfType("[]string"), "-t", mock.AnythingOfType("string"), "--timeout=2", "nb-connection-status"},
				RetArgList: []interface{}{bytes.NewBuffer([]byte("connected")), bytes.NewBuffer([]byte("")), nil},
				CallTimes:  2,
			},
		},
		expectedMetrics: []string{
			"ovn_northd_build_flows_ctx_95th_percentile",
			"ovn_northd_build_flows_ctx_long_term_avg",
			"ovn_northd_build_flows_ctx_maximum",
			"ovn_northd_build_flows_ctx_minimum",
			"ovn_northd_build_flows_ctx_short_term_avg",
			"ovn_northd_build_flows_ctx_total_samples",
			"ovn_northd_build_info",
			"ovn_northd_build_lflows_95th_percentile",
			"ovn_northd_build_lflows_long_term_avg",
			"ovn_northd_build_lflows_maximum",
			"ovn_northd_build_lflows_minimum",
			"ovn_northd_build_lflows_short_term_avg",
			"ovn_northd_build_lflows_total_samples",
			"ovn_northd_clear_lflows_ctx_95th_percentile",
			"ovn_northd_clear_lflows_ctx_long_term_avg",
			"ovn_northd_clear_lflows_ctx_maximum",
			"ovn_northd_clear_lflows_ctx_minimum",
			"ovn_northd_clear_lflows_ctx_short_term_avg",
			"ovn_northd_clear_lflows_ctx_total_samples",
			"ovn_northd_lflows_datapaths_95th_percentile",
			"ovn_northd_lflows_datapaths_long_term_avg",
			"ovn_northd_lflows_datapaths_maximum",
			"ovn_northd_lflows_datapaths_minimum",
			"ovn_northd_lflows_datapaths_short_term_avg",
			"ovn_northd_lflows_datapaths_total_samples",
			"ovn_northd_lflows_dp_groups_95th_percentile",
			"ovn_northd_lflows_dp_groups_long_term_avg",
			"ovn_northd_lflows_dp_groups_maximum",
			"ovn_northd_lflows_dp_groups_minimum",
			"ovn_northd_lflows_dp_groups_short_term_avg",
			"ovn_northd_lflows_dp_groups_total_samples",
			"ovn_northd_lflows_igmp_95th_percentile",
			"ovn_northd_lflows_igmp_long_term_avg",
			"ovn_northd_lflows_igmp_maximum",
			"ovn_northd_lflows_igmp_minimum",
			"ovn_northd_lflows_igmp_short_term_avg",
			"ovn_northd_lflows_igmp_total_samples",
			"ovn_northd_lflows_lbs_95th_percentile",
			"ovn_northd_lflows_lbs_long_term_avg",
			"ovn_northd_lflows_lbs_maximum",
			"ovn_northd_lflows_lbs_minimum",
			"ovn_northd_lflows_lbs_short_term_avg",
			"ovn_northd_lflows_lbs_total_samples",
			"ovn_northd_lflows_ports_95th_percentile",
			"ovn_northd_lflows_ports_long_term_avg",
			"ovn_northd_lflows_ports_maximum",
			"ovn_northd_lflows_ports_minimum",
			"ovn_northd_lflows_ports_short_term_avg",
			"ovn_northd_lflows_ports_total_samples",
			"ovn_northd_nb_connection_status",
			"ovn_northd_ovn_northd_loop_95th_percentile",
			"ovn_northd_ovn_northd_loop_long_term_avg",
			"ovn_northd_ovn_northd_loop_maximum",
			"ovn_northd_ovn_northd_loop_minimum",
			"ovn_northd_ovn_northd_loop_short_term_avg",
			"ovn_northd_ovn_northd_loop_total_samples",
			"ovn_northd_ovnnb_db_run_95th_percentile",
			"ovn_northd_ovnnb_db_run_long_term_avg",
			"ovn_northd_ovnnb_db_run_maximum",
			"ovn_northd_ovnnb_db_run_minimum",
			"ovn_northd_ovnnb_db_run_short_term_avg",
			"ovn_northd_ovnnb_db_run_total_samples",
			"ovn_northd_ovnsb_db_run_95th_percentile",
			"ovn_northd_ovnsb_db_run_long_term_avg",
			"ovn_northd_ovnsb_db_run_maximum",
			"ovn_northd_ovnsb_db_run_minimum",
			"ovn_northd_ovnsb_db_run_short_term_avg",
			"ovn_northd_ovnsb_db_run_total_samples",
			"ovn_northd_pstream_open",
			"ovn_northd_sb_connection_status",
			"ovn_northd_status",
			"ovn_northd_stream_open",
			"ovn_northd_txn_aborted",
			"ovn_northd_txn_error",
			"ovn_northd_txn_incomplete",
			"ovn_northd_txn_success",
			"ovn_northd_txn_try_again",
			"ovn_northd_txn_unchanged",
			"ovn_northd_txn_uncommitted",
			"promhttp_metric_handler_requests_in_flight",
			"promhttp_metric_handler_requests_total",
		},
	}

	DefaultMetricsTestCase = metricsTestCase{
		expectedMetrics: []string{
			"ovnkube_controller_ready_duration_seconds",
			"ovnkube_controller_sync_duration_seconds",
			"ovnkube_controller_build_info",
			"go_gc_duration_seconds",
			"go_gc_gogc_percent",
			"go_gc_gomemlimit_bytes",
			"go_goroutines",
			"go_info",
			"go_memstats_alloc_bytes",
			"go_memstats_alloc_bytes_total",
			"go_memstats_buck_hash_sys_bytes",
			"go_memstats_frees_total",
			"go_memstats_gc_sys_bytes",
			"go_memstats_heap_alloc_bytes",
			"go_memstats_heap_idle_bytes",
			"go_memstats_heap_inuse_bytes",
			"go_memstats_heap_objects",
			"go_memstats_heap_released_bytes",
			"go_memstats_heap_sys_bytes",
			"go_memstats_last_gc_time_seconds",
			"go_memstats_mallocs_total",
			"go_memstats_mcache_inuse_bytes",
			"go_memstats_mcache_sys_bytes",
			"go_memstats_mspan_inuse_bytes",
			"go_memstats_mspan_sys_bytes",
			"go_memstats_next_gc_bytes",
			"go_memstats_other_sys_bytes",
			"go_memstats_stack_inuse_bytes",
			"go_memstats_stack_sys_bytes",
			"go_memstats_sys_bytes",
			"go_sched_gomaxprocs_threads",
			"go_threads",
			"process_cpu_seconds_total",
			"process_max_fds",
			"process_network_receive_bytes_total",
			"process_network_transmit_bytes_total",
			"process_open_fds",
			"process_resident_memory_bytes",
			"process_start_time_seconds",
			"process_virtual_memory_bytes",
			"process_virtual_memory_max_bytes",
			"promhttp_metric_handler_requests_in_flight",
			"promhttp_metric_handler_requests_total",
		},
	}

	dpctlShowOutput = `system@ovs-system:
lookups: hit:123456 missed:11 lost:22
flows: 77
masks: hit:23456 total:33 hit/pkt:0.33
cache: hit:34567 hit-rate:99.99%
caches:
	masks-cache: size:256
port 0: ovs-system (internal)
port 1: breth0 (internal)
port 4: eth0
`

	ovsMemoryShowOutput = `handlers:8 idl-cells-Open_vSwitch:2892 ofconns:4 ports:48 revalidators:3 rules:1803 udpif keys:86`

	coverageShowOutput = `Event coverage, avg rate over last: 5 seconds, last minute, last hour,  hash=f5b8301d:
bridge_reconfigure         0.0/sec     0.000/sec        0.0000/sec   total: 194
ofproto_flush              0.0/sec     0.000/sec        0.0000/sec   total: 5
ofproto_packet_out         0.0/sec     0.000/sec        0.0000/sec   total: 26586
ofproto_recv_openflow      0.0/sec     0.750/sec        0.7525/sec   total: 2020864
ofproto_update_port        0.0/sec     0.000/sec        0.0000/sec   total: 174
rev_reconfigure            0.0/sec     0.000/sec        0.0000/sec   total: 50
rev_port_toggled           0.0/sec     0.000/sec        0.0000/sec   total: 20
rev_flow_table             0.0/sec     0.000/sec        0.0000/sec   total: 680
rev_mac_learning           0.0/sec     0.000/sec        0.0011/sec   total: 15447
dumped_duplicate_flow      0.0/sec     0.000/sec        0.0000/sec   total: 1
handler_duplicate_upcall   0.0/sec     0.517/sec        0.6217/sec   total: 1466381
revalidate_missed_dp_flow   0.0/sec     0.000/sec        0.0000/sec   total: 14
revalidate_missing_dp_flow   0.0/sec     0.000/sec        0.0000/sec   total: 479
ukey_replace_contention    0.0/sec     0.000/sec        0.0003/sec   total: 703
ukey_from_dp_flow          0.0/sec     0.000/sec        0.0000/sec   total: 13
upcall_flow_limit_grew     0.0/sec     0.000/sec        0.0000/sec   total: 127
upcall_ukey_contention     0.0/sec     0.000/sec        0.0000/sec   total: 1
upcall_ukey_replace        0.0/sec     0.000/sec        0.0000/sec   total: 77
upcall_flow_del_rev        0.0/sec     0.000/sec        0.0000/sec   total: 139
upcall_flow_del_idle_or_limit   3.8/sec     5.083/sec        5.0364/sec   total: 7281882
xlate_actions              6.4/sec     9.100/sec        9.0861/sec   total: 13299790
connmgr_async_unsent       0.0/sec     0.000/sec        0.0000/sec   total: 2
ccmap_expand               0.0/sec     0.000/sec        0.0000/sec   total: 233
ccmap_shrink               0.0/sec     0.000/sec        0.0000/sec   total: 388
cmap_expand                0.0/sec     0.000/sec        0.0000/sec   total: 3098
cmap_shrink                0.0/sec     0.000/sec        0.0000/sec   total: 1952
dpif_execute               4.2/sec     6.250/sec        6.1442/sec   total: 9164190
dpif_execute_error         0.0/sec     0.000/sec        0.0000/sec   total: 1
dpif_execute_with_help     0.2/sec     0.200/sec        0.1728/sec   total: 215206
dpif_flow_del              3.8/sec     5.083/sec        5.0364/sec   total: 7282525
dpif_flow_flush            0.0/sec     0.000/sec        0.0000/sec   total: 1
dpif_flow_get              0.0/sec     0.000/sec        0.0000/sec   total: 38
dpif_flow_put              3.8/sec     5.217/sec        5.1142/sec   total: 7524160
dpif_flow_put_error        0.0/sec     0.033/sec        0.0758/sec   total: 241346
dpif_meter_del             0.0/sec     0.000/sec        0.0000/sec   total: 55
dpif_meter_set             0.0/sec     0.000/sec        0.0000/sec   total: 58
dpif_port_add              0.0/sec     0.000/sec        0.0000/sec   total: 23
dpif_port_del              0.0/sec     0.000/sec        0.0000/sec   total: 34
flow_extract               3.8/sec     5.733/sec        5.7367/sec   total: 8994358
miniflow_malloc            0.0/sec     0.467/sec        0.4689/sec   total: 1258436
hindex_pathological        0.0/sec     0.000/sec        0.0000/sec   total: 8
hindex_expand              0.0/sec     0.000/sec        0.0000/sec   total: 9
hmap_pathological          0.0/sec     0.017/sec        0.0269/sec   total: 55393
hmap_expand               28.2/sec    41.100/sec       39.8708/sec   total: 85284712
mac_learning_learned       0.0/sec     0.000/sec        0.0006/sec   total: 6175
mac_learning_expired       0.0/sec     0.000/sec        0.0006/sec   total: 6150
mac_learning_moved         0.0/sec     0.000/sec        0.0000/sec   total: 7299
mac_learning_static_none_move   0.2/sec     0.200/sec        0.1728/sec   total: 381251
netdev_get_stats           9.6/sec    10.400/sec       10.4000/sec   total: 20330897
txn_unchanged              0.0/sec     0.050/sec        0.0500/sec   total: 108339
txn_incomplete             0.2/sec     0.200/sec        0.2111/sec   total: 472946
txn_success                0.2/sec     0.200/sec        0.2000/sec   total: 451953
poll_create_node         186.4/sec   333.033/sec      333.6025/sec   total: 719704044
poll_zero_timeout          3.8/sec     6.017/sec        5.8844/sec   total: 8608665
rconn_queued               0.0/sec     1.900/sec        1.9078/sec   total: 4419934
rconn_sent                 0.0/sec     1.900/sec        1.9078/sec   total: 4419934
seq_change               1065.8/sec  1073.883/sec     1077.5122/sec   total: 2422874389
pstream_open               0.0/sec     0.000/sec        0.0000/sec   total: 14
stream_open                0.0/sec     0.000/sec        0.0000/sec   total: 1
unixctl_received           0.0/sec     0.117/sec        0.1167/sec   total: 222403
unixctl_replied            0.0/sec     0.117/sec        0.1167/sec   total: 222403
util_xalloc              1724.0/sec  3564.283/sec     3521.6053/sec   total: 6371878350
vconn_received             0.0/sec     1.067/sec        1.0700/sec   total: 2789645
vconn_sent                 0.0/sec     2.217/sec        2.2253/sec   total: 5188715
netdev_set_policing        0.0/sec     0.000/sec        0.0000/sec   total: 77
netdev_get_ifindex         0.0/sec     0.500/sec        0.5000/sec   total: 341857
netdev_set_hwaddr          0.0/sec     0.000/sec        0.0000/sec   total: 2
netdev_get_ethtool         0.0/sec     0.000/sec        0.0000/sec   total: 104
netdev_set_ethtool         0.0/sec     0.000/sec        0.0000/sec   total: 20
netlink_received          25.6/sec    32.067/sec       32.0092/sec   total: 60257902
netlink_recv_jumbo         3.6/sec     4.683/sec        4.6350/sec   total: 7520804
netlink_sent              24.8/sec    32.550/sec       32.2783/sec   total: 56841452
route_table_dump           0.0/sec     0.000/sec        0.0000/sec   total: 163
nln_changed                0.0/sec     0.000/sec        0.0000/sec   total: 230
119 events never hit
`
	ovsOfctlDumpAggregateOutput = `
NXST_AGGREGATE reply (xid=0x4): packet_count=12345 byte_count=67890 flow_count=18000
`

	ovnDBMemoryShowOutput = `atoms:324341 cells:307671 monitors:2 n-weak-refs:5627 sessions:12 txn-history:100 txn-history-atoms:52811`

	ovnControllerDumpAggregateOutput = `NXST_AGGREGATE reply (xid=0x4): packet_count=9945601440 byte_count=33370900148508 flow_count=12062`
	ovnControllercoverageShowOutput  = `Event coverage, avg rate over last: 5 seconds, last minute, last hour,  hash=7a3e39bf:
lflow_run                  0.0/sec     0.000/sec        0.0000/sec   total: 113
consider_logical_flow      0.0/sec     0.333/sec        0.9019/sec   total: 3874864
lflow_cache_flush          0.0/sec     0.000/sec        0.0000/sec   total: 1
lflow_cache_trim           0.0/sec     0.000/sec        0.0000/sec   total: 2
lflow_conj_alloc           0.0/sec     0.000/sec        0.0097/sec   total: 17158
lflow_conj_free            0.0/sec     0.000/sec        0.0097/sec   total: 14792
pinctrl_notify_main_thread   0.0/sec     0.000/sec        0.0000/sec   total: 118
pinctrl_total_pin_pkts     0.0/sec     0.000/sec        0.0000/sec   total: 147
physical_run               0.0/sec     0.000/sec        0.0000/sec   total: 136
flow_extract               0.0/sec     0.000/sec        0.0000/sec   total: 147
miniflow_malloc            0.0/sec     0.200/sec        3.1769/sec   total: 9010830
hmap_pathological          0.6/sec     1.600/sec        1.4653/sec   total: 2410194
hmap_expand               44.4/sec   116.050/sec      103.9733/sec   total: 169419911
hmap_reserve               3.0/sec     7.667/sec        6.7917/sec   total: 10860162
txn_unchanged              1.2/sec     3.067/sec        2.7164/sec   total: 4343791
txn_incomplete             0.0/sec     0.000/sec        0.0003/sec   total: 556
txn_success                0.0/sec     0.000/sec        0.0003/sec   total: 289
txn_try_again              0.0/sec     0.000/sec        0.0000/sec   total: 1
poll_create_node          14.2/sec    87.233/sec       59.1267/sec   total: 93695892
poll_zero_timeout          0.0/sec     0.017/sec        0.0125/sec   total: 15868
rconn_queued               0.0/sec     1.400/sec        2.0528/sec   total: 3211580
rconn_sent                 0.0/sec     1.400/sec        2.0528/sec   total: 3211580
seq_change                 5.2/sec    62.550/sec       37.2158/sec   total: 58941139
pstream_open               0.0/sec     0.000/sec        0.0000/sec   total: 1
stream_open                0.0/sec     0.000/sec        0.0000/sec   total: 9
unixctl_received           0.2/sec     0.167/sec        0.1794/sec   total: 226274
unixctl_replied            0.2/sec     0.167/sec        0.1794/sec   total: 226274
util_xalloc              1438.0/sec  4125.033/sec     3639.7153/sec   total: 6035801728
vconn_open                 0.0/sec     0.000/sec        0.0000/sec   total: 6
vconn_received             0.0/sec     1.000/sec        0.7628/sec   total: 1295583
vconn_sent                 0.0/sec     1.400/sec        2.0528/sec   total: 3211584
nln_changed                0.0/sec     0.000/sec        0.0000/sec   total: 6
netlink_received           0.0/sec     0.000/sec        0.0000/sec   total: 582
netlink_sent               0.0/sec     0.000/sec        0.0000/sec   total: 576
119 events never hit
`

	ovnControllerVersionOutput = `ovn-controller 20.06.0.86f64fc1
Open vSwitch Library 2.13.0.f945b5c5
`
	ovnNorthdVersionOutput = `ovn-northd 25.03.0.c2144df1.28754012
Open vSwitch Library 3.5.0
`
)
