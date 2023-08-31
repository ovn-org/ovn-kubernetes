package csrapprover

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509/pkix"
	"fmt"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate/csr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const csrName = "testCSR"

func TestOVNKubeCSRController(t *testing.T) {
	tests := []struct {
		name                     string
		expectedCommonNamePrefix string
		expectedOrganization     []string
		expectedGroups           sets.Set[string]
		expectedUsersPrefixes    sets.Set[string]
		expectedUsages           sets.Set[certificatesv1.KeyUsage]
		expectedMaxDuration      time.Duration
		expectedCondition        certificatesv1.CertificateSigningRequestCondition
		expectedEvent            string

		csrUserName      string
		signerName       string
		commonNamePrefix string
		nodeName         string
		groups           []string
		organization     []string
		usages           sets.Set[certificatesv1.KeyUsage]
		duration         time.Duration
		shouldIgnore     bool
	}{
		{
			name:                     "CSR with a CommonName that does not start with commonNamePrefix is ignored",
			expectedCommonNamePrefix: "test:group",
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			commonNamePrefix:         "different:group",
			shouldIgnore:             true,
		},
		{
			name:                     "CSR with .Spec.SignerName not equal to kubernetes.io/kube-apiserver-client is ignored",
			expectedCommonNamePrefix: "test:group",
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			commonNamePrefix:         "test:group",
			signerName:               certificatesv1.KubeletServingSignerName,
			shouldIgnore:             true,
		},
		{
			name:                  "CSR created by unexpected user is denied",
			expectedUsersPrefixes: sets.New[string]("system:node"),
			expectedUsages:        sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("CSR %q was created by an unexpected user: %q", csrName, "invalid:prefix:test.node"),
			},
			expectedEvent: fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: CSR %q was created by an unexpected user: %q", csrName, csrName, "invalid:prefix:test.node"),
			csrUserName:   "invalid:prefix:test.node",
			signerName:    certificatesv1.KubeAPIServerClientSignerName,
			nodeName:      "test.node",
		},
		{
			name:                  "CSR created by system:node:<nodeName> user with invalid nodeName is denied",
			expectedUsersPrefixes: sets.New[string]("system:node"),
			expectedUsages:        sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:   certificatesv1.CertificateDenied,
				Status: corev1.ConditionTrue,
				Reason: "CSRDenied",
				Message: fmt.Sprintf("extracted node name %q is not a valid DNS subdomain "+
					"[a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', "+
					"and must start and end with an alphanumeric character "+
					"(e.g. 'example.com', regex used for validation is "+
					"'[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')]", "iNvAlId#N@dE"),
			},
			expectedEvent: fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: extracted node name %q is not a valid DNS subdomain "+
				"[a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', "+
				"and must start and end with an alphanumeric character "+
				"(e.g. 'example.com', regex used for validation is "+
				"'[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')]", csrName, "iNvAlId#N@dE"),
			csrUserName: "system:node:iNvAlId#N@dE",
			signerName:  certificatesv1.KubeAPIServerClientSignerName,
			nodeName:    "test.node",
		},
		{
			name:                     "CSR with unexpected groups is denied",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("CSR %q was created by a user with unexpected groups: %v", csrName, []string{"test:nodes", "test:authenticated", "unexpected:group"}),
			},
			expectedEvent:    fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: CSR %q was created by a user with unexpected groups: %v", csrName, csrName, []string{"test:nodes", "test:authenticated", "unexpected:group"}),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "test.node",
			groups:           []string{"test:nodes", "test:authenticated", "unexpected:group"},
		},
		{
			name:                     "CSR with unexpected common name is denied",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("expected the CSR's commonName to be %q, but it is %q", "test:group:test.node", "test:group:different.node"),
			},
			expectedEvent:    fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: expected the CSR's commonName to be %q, but it is %q", csrName, "test:group:test.node", "test:group:different.node"),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "different.node",
			groups:           []string{"test:nodes", "test:authenticated"},
		},
		{
			name:                     "CSR with unexpected usages is denied",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("CSR %q was created with unexpected usages: [%s]", csrName, certificatesv1.UsageCertSign),
			},
			expectedEvent:    fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: CSR %q was created with unexpected usages: [%s]", csrName, csrName, certificatesv1.UsageCertSign),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "test.node",
			usages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageCertSign),
			groups:           []string{"test:nodes", "test:authenticated"},
		},
		{
			name:                     "CSR without expirationSeconds is denied",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedMaxDuration:      time.Hour,
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("CSR %q was created without specyfying the expirationSeconds", csrName),
			},
			expectedEvent:    fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: CSR %q was created without specyfying the expirationSeconds", csrName, csrName),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "test.node",
			groups:           []string{"test:nodes", "test:authenticated"},
		},
		{
			name:                     "CSR with invalid expirationSeconds is denied",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedMaxDuration:      time.Hour,
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateDenied,
				Status:  corev1.ConditionTrue,
				Reason:  "CSRDenied",
				Message: fmt.Sprintf("CSR %q was created with invalid expirationSeconds value: %d", csrName, 3600000),
			},
			expectedEvent:    fmt.Sprintf("Warning CSRDenied The CSR %q has been denied: CSR %q was created with invalid expirationSeconds value: %d", csrName, csrName, 3600000),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "test.node",
			groups:           []string{"test:nodes", "test:authenticated"},
			duration:         time.Hour * 1000,
		},
		{
			name:                     "Valid CSR is approved",
			expectedCommonNamePrefix: "test:group",
			expectedUsages:           sets.New[certificatesv1.KeyUsage](certificatesv1.UsageClientAuth),
			expectedUsersPrefixes:    sets.New[string]("system:node"),
			expectedGroups:           sets.New[string]("test:nodes", "test:authenticated"),
			expectedMaxDuration:      time.Hour,
			expectedCondition: certificatesv1.CertificateSigningRequestCondition{
				Type:    certificatesv1.CertificateApproved,
				Status:  corev1.ConditionTrue,
				Reason:  "AutoApproved",
				Message: fmt.Sprintf("Auto-approved CSR %q", csrName),
			},
			expectedEvent:    fmt.Sprintf("Normal CSRApproved CSR %q has been approved", csrName),
			csrUserName:      "system:node:test.node",
			signerName:       certificatesv1.KubeAPIServerClientSignerName,
			commonNamePrefix: "test:group",
			nodeName:         "test.node",
			groups:           []string{"test:nodes", "test:authenticated"},
			duration:         time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			csrPEM, err := cert.MakeCSR(privateKey, &pkix.Name{
				CommonName:   fmt.Sprintf("%s:%s", tt.commonNamePrefix, tt.nodeName),
				Organization: tt.organization,
			}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			csrObj := &certificatesv1.CertificateSigningRequest{
				TypeMeta: metav1.TypeMeta{Kind: "CertificateSigningRequest"},
				ObjectMeta: metav1.ObjectMeta{
					Name: csrName,
				},
				Spec: certificatesv1.CertificateSigningRequestSpec{
					Request:    csrPEM,
					Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageClientAuth},
					SignerName: tt.signerName,
					Username:   tt.csrUserName,
					Groups:     tt.groups,
				},
			}
			if len(tt.usages) != 0 {
				csrObj.Spec.Usages = tt.usages.UnsortedList()
			}
			if tt.duration != 0 {
				csrObj.Spec.ExpirationSeconds = csr.DurationToExpirationSeconds(tt.duration)
			}

			client := fake.NewClientBuilder().WithRuntimeObjects(csrObj).Build()
			recorder := record.NewFakeRecorder(10)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			csrCtrl := NewController(client,
				tt.expectedCommonNamePrefix,
				tt.expectedOrganization,
				tt.expectedGroups,
				tt.expectedUsersPrefixes,
				tt.expectedUsages,
				tt.expectedMaxDuration,
				recorder)

			if err != nil {
				t.Fatal(err)
			}

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: csrName,
				},
			}
			_, err = csrCtrl.Reconcile(ctx, req)
			if err != nil {
				t.Fatal(err)
			}

			csrObj = &certificatesv1.CertificateSigningRequest{}
			err = client.Get(context.TODO(), req.NamespacedName, csrObj)
			if err != nil {
				t.Fatal(err)
			}
			if len(csrObj.Status.Conditions) != 1 {
				if !tt.shouldIgnore {
					t.Fatal(fmt.Errorf("invalid conditions: %v", csrObj.Status.Conditions))
				}
			} else {
				if csrObj.Status.Conditions[0] != tt.expectedCondition {
					t.Fatal(fmt.Errorf("expected:\n%v\ngot:\n%v", tt.expectedCondition, csrObj.Status.Conditions[0]))
				}
			}

			if err != nil {
				if err != context.DeadlineExceeded || !tt.shouldIgnore {
					t.Fatal(fmt.Errorf("csr verification failed, err: %v", err))
				}

			}

			if tt.expectedEvent != "" {
				if len(recorder.Events) != 1 {
					t.Fatal(fmt.Errorf("invalid number of events recorded: %d", len(recorder.Events)))
				}
				event := <-recorder.Events
				if event != tt.expectedEvent {
					t.Fatal(fmt.Errorf("expected event:\n%s\ngot\n%s", tt.expectedEvent, event))
				}
			}
		})
	}
}
