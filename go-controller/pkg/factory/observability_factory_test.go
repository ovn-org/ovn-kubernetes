// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	observabilityconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	observabilityconfigfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Watch Factory ObservabilityConfig informer", func() {
	var (
		wf  *WatchFactory
		err error
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableObservability = true
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
	})

	// Broad test for ObservabilityConfig informers, including its startup sequence.
	// It would break if watchFactory.Start happened before ControllerManager.Start.
	// The informer must be instantiated in the watch factory constructor.
	It("creates and starts the informer so its cache syncs on Start", func() {
		obsConfig := &observabilityconfigapi.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: observabilityconfigapi.ObservabilitySpec{
				CollectorID: 1,
				Features: []observabilityconfigapi.FeatureConfig{
					{Feature: observabilityconfigapi.NetworkPolicy, Probability: 100},
				},
			},
		}
		ovnClientset := &util.OVNKubeControllerClientset{
			KubeClient:                k8sfake.NewSimpleClientset(),
			ObservabilityConfigClient: observabilityconfigfake.NewSimpleClientset(obsConfig),
		}

		wf, err = NewOVNKubeControllerWatchFactory(ovnClientset, "node1")
		Expect(err).NotTo(HaveOccurred())
		Expect(wf.Start()).To(Succeed())

		informer := wf.ObservabilityConfigInformer().Informer()
		Eventually(informer.HasSynced, 5*time.Second, 50*time.Millisecond).Should(BeTrue(),
			"ObservabilityConfig informer must be started by watchFactory.Start()")
		Eventually(func() []interface{} {
			return informer.GetStore().List()
		}, 5*time.Second, 50*time.Millisecond).Should(HaveLen(1),
			"seeded ObservabilityConfig must land in the informer cache")
	})
})
