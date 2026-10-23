// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package webhook_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/cmd/ovnkube-identity/webhook"
	ovntesting "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = BeforeSuite(func() {
	Expect(configv1.Install(k8sscheme.Scheme)).To(Succeed())
})

var _ = Describe("Run", func() {
	var config webhook.Config

	BeforeEach(func() {
		// Pick a free ephemeral port for each test
		config = newWebhookConfig(getFreePort())
	})

	JustBeforeEach(func() {
		ctx, cancel := context.WithCancel(context.Background())

		doneCh := make(chan struct{})

		DeferCleanup(func() {
			cancel()
			Eventually(doneCh).Within(10 * time.Second).Should(BeClosed())
		})

		go func() {
			defer GinkgoRecover()

			err := webhook.Run(ctx, &rest.Config{
				Host: "https://localhost:6443",
			}, config)

			close(doneCh)
			Expect(err).NotTo(HaveOccurred())
		}()
	})

	It("should always register the \"/node\" webhook endpoint", func() {
		assertWebhookRequestSuccess(config.Host, config.Port, "node", createNodeAdmissionReviewJSON())
	})

	It("should support SO_REUSEPORT by allowing multiple servers on the same port", func() {
		// Wait for the first server to be ready
		waitForServerReady(config.Host, config.Port)

		// Use the same port as the first server
		config2 := newWebhookConfig(config.Port)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		doneCh := make(chan struct{})
		errCh := make(chan error, 1)

		go func() {
			if err := webhook.Run(ctx, &rest.Config{}, config2); err != nil {
				fmt.Printf("Run() returned err: %v\n", err)
				select {
				case errCh <- err:
				default:
				}
			}

			close(doneCh)
		}()

		Consistently(errCh).Within(500 * time.Millisecond).ShouldNot(Receive())

		// Verify both servers are working
		waitForServerReady(config.Host, config.Port)

		cancel()
		Eventually(doneCh).Within(10 * time.Second).Should(BeClosed())
	})

	It("should reload certificates when they change", func() {
		// Wait for server to start and get the initial certificate
		var originalCert *tls.Certificate
		Eventually(func(g Gomega) {
			cert, err := getServerCertificate(config.Host, config.Port)
			g.Expect(err).NotTo(HaveOccurred())
			originalCert = cert
		}).Within(5 * time.Second).Should(Succeed())

		// Generate new certificates
		newCertPEM, newKeyPEM, err := ovntesting.GenerateTestCertificate()
		Expect(err).NotTo(HaveOccurred())

		// Update the certificate files
		certPath := filepath.Join(config.CertDir, "tls.crt")
		keyPath := filepath.Join(config.CertDir, "tls.key")

		Expect(os.WriteFile(certPath, newCertPEM, 0600)).To(Succeed())
		Expect(os.WriteFile(keyPath, newKeyPEM, 0600)).To(Succeed())

		// Verify the server picked up the new certificate
		Eventually(func(g Gomega) {
			newCert, err := getServerCertificate(config.Host, config.Port)
			g.Expect(err).NotTo(HaveOccurred())

			// Compare certificate bytes to verify it changed
			g.Expect(newCert.Certificate[0]).NotTo(Equal(originalCert.Certificate[0]),
				"Server should be using new certificate after rotation")
		}).Within(3 * time.Second).ProbeEvery(200 * time.Millisecond).Should(Succeed())
	})

	Context("with applied TLS config options", func() {
		BeforeEach(func() {
			config.ApplyTLSOptions = func(tlsConfig *tls.Config) {
				tlsConfig.MinVersion = tls.VersionTLS12
				tlsConfig.CipherSuites = []uint16{
					tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				}
				tlsConfig.CurvePreferences = []tls.CurveID{tls.CurveP384}
			}
		})

		Specify("the HTTP server should offer the applied TLS options", func() {
			Eventually(func(g Gomega) {
				conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", config.Host, config.Port), &tls.Config{
					InsecureSkipVerify: true,
					MaxVersion:         tls.VersionTLS12, // Force TLS 1.2 to test minimum version
					NextProtos:         []string{"h2"},   // Request HTTP/2 via ALPN
				})

				g.Expect(err).NotTo(HaveOccurred())
				defer conn.Close()

				err = conn.Handshake()
				g.Expect(err).NotTo(HaveOccurred())

				state := conn.ConnectionState()

				// For Intermediate profile with MaxVersion TLS 1.2, should negotiate exactly TLS 1.2
				g.Expect(int(state.Version)).To(Equal(tls.VersionTLS12))
				g.Expect(state.CipherSuite).To(Equal(tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256))
				g.Expect(state.NegotiatedProtocol).To(Equal("h2"))
				g.Expect(state.CurveID).To(Equal(tls.CurveP384))
			}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
		})

		Specify("the HTTP server should reject TLS versions below the minimum", func() {
			Eventually(func(g Gomega) {
				// Attempt to connect with TLS 1.1 (below the server's minimum of TLS 1.2)
				conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", config.Host, config.Port), &tls.Config{
					InsecureSkipVerify: true,
					MinVersion:         tls.VersionTLS10,
					MaxVersion:         tls.VersionTLS11, // Restrict client to TLS 1.1 or below
				})

				if err == nil {
					err = conn.Handshake()
					conn.Close()
				}

				// The connection or handshake should fail because the server requires TLS 1.2+
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring("protocol version"))
			}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
		})
	})
})

func newWebhookConfig(port int) webhook.Config {
	tempDir, err := os.MkdirTemp("", "webhook-test-*")
	Expect(err).NotTo(HaveOccurred())

	DeferCleanup(func() {
		_ = os.RemoveAll(tempDir)
	})

	// Generate valid test certificates
	certPEM, keyPEM, err := ovntesting.GenerateTestCertificate()
	Expect(err).NotTo(HaveOccurred())

	// Write test certificates
	certPath := filepath.Join(tempDir, "tls.crt")
	Expect(os.WriteFile(certPath, certPEM, 0600)).To(Succeed())

	keyPath := filepath.Join(tempDir, "tls.key")
	Expect(os.WriteFile(keyPath, keyPEM, 0600)).To(Succeed())

	return webhook.Config{
		CertDir:         tempDir,
		Host:            "localhost",
		Port:            port,
		ApplyTLSOptions: func(*tls.Config) {},
		NewKubernetesClient: func(_ *rest.Config) (kubernetes.Interface, error) {
			return k8sfake.NewClientset(), nil
		},
	}
}

func getFreePort() int {
	listener, err := net.Listen("tcp", "localhost:0")
	Expect(err).NotTo(HaveOccurred())

	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	return port
}

func waitForServerReady(host string, port int) {
	Eventually(func(g Gomega) {
		conn, err := tls.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
			InsecureSkipVerify: true,
		})
		g.Expect(err).NotTo(HaveOccurred())
		conn.Close()
	}).Within(5 * time.Second).Should(Succeed())
}

func getServerCertificate(host string, port int) (*tls.Certificate, error) {
	conn, err := tls.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
		InsecureSkipVerify: true,
	})

	if err != nil {
		return nil, err
	}

	defer conn.Close()

	// Get the peer certificate presented by the server
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no peer certificates received")
	}

	// Convert to tls.Certificate format for comparison
	cert := &tls.Certificate{
		Certificate: [][]byte{state.PeerCertificates[0].Raw},
	}

	return cert, nil
}

func assertWebhookRequestSuccess(host string, port int, endpoint string, payload []byte) {
	httpClient := createWebhookClient()

	Eventually(func(g Gomega) {
		resp, err := httpClient.Post(
			fmt.Sprintf("https://%s/%s", net.JoinHostPort(host, strconv.Itoa(port)), endpoint),
			"application/json",
			bytes.NewBuffer(payload),
		)

		g.Expect(err).NotTo(HaveOccurred())
		defer resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	}).Within(5 * time.Second).ProbeEvery(100 * time.Millisecond).Should(Succeed())
}

func createNodeAdmissionReviewJSON() []byte {
	return createAdmissionReviewJSON(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	})
}

func createAdmissionReviewJSON(obj any) []byte {
	raw, err := json.Marshal(obj)
	Expect(err).NotTo(HaveOccurred())

	objMeta, err := meta.Accessor(obj)
	Expect(err).NotTo(HaveOccurred())

	admissionReview := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Name:      objMeta.GetName(),
			Namespace: objMeta.GetNamespace(),
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: raw,
			},
		},
	}

	admJSON, err := json.Marshal(admissionReview)
	Expect(err).NotTo(HaveOccurred())

	return admJSON
}

func createWebhookClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				// For testing, skip certificate verification
				// In production, you'd load the CA cert and verify
				InsecureSkipVerify: true,
			},
			// Force HTTP/2
			ForceAttemptHTTP2: true,
		},
	}
}

func TestWebhook(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Webhook Suite")
}
