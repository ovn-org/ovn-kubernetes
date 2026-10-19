// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package crdintegration contains the CRD integration tests for the
// OVN-Kubernetes CRDs.  Tests run against a real kube-apiserver + etcd started
// by envtest so that admission defaulting, validation, and (future) CEL rules
// are exercised exactly as they would be in a live cluster.
//
// This is a single Ginkgo suite: envtest is started once in BeforeSuite (with
// every CRD under helm/ovn-kubernetes/crds installed), and each feature/CRD
// gets its own file (egressip_test.go, ...).  To cover a new CRD, register its
// types in the scheme below and add a <feature>_test.go.
//
// Run with:
//
//	make test-crd   (from the test/ directory)
package crdintegration

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       = context.Background()
)

func TestCRDIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OVN-Kubernetes CRD Integration Suite")
}

var _ = BeforeSuite(func() {
	// Register every CRD's types the suite exercises.  Add new CRDs here.
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(egressipv1.AddToScheme(scheme))
	utilruntime.Must(observabilityconfigv1alpha1.AddToScheme(scheme))
	utilruntime.Must(uplinkv1alpha1.AddToScheme(scheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{crdDir()},
		// Fail fast if the CRD YAML is absent rather than producing a
		// confusing "resource not found" error later.
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	Expect(testEnv.Stop()).To(Succeed())
})

// crdDir returns the absolute path to the committed CRD manifests directory.
// It is computed relative to this source file so the tests work regardless of
// the working directory at invocation time.
func crdDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	// thisFile: .../test/crd-integration/suite_test.go
	// CRDs:     .../helm/ovn-kubernetes/crds/
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "helm", "ovn-kubernetes", "crds")
}
