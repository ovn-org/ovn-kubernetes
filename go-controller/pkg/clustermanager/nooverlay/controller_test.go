// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package nooverlay

import (
	"context"
	"time"

	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// Test helper types
type testRA struct {
	Name             string
	SelectsDefault   bool
	AdvertisePods    bool
	AcceptedStatus   *metav1.ConditionStatus
	NetworkSelectors []apitypes.NetworkSelector
	Advertisements   []ratypes.AdvertisementType
}

func (tra testRA) RouteAdvertisements() *ratypes.RouteAdvertisements {
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: tra.Name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			NetworkSelectors: tra.NetworkSelectors,
			Advertisements:   tra.Advertisements,
		},
	}

	// Handle simple case flags
	if tra.SelectsDefault && len(tra.NetworkSelectors) == 0 {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.DefaultNetwork,
		})
	}
	if tra.AdvertisePods && len(tra.Advertisements) == 0 {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.PodNetwork)
	}

	if tra.AcceptedStatus != nil {
		ra.Status.Conditions = []metav1.Condition{
			{
				Type:   "Accepted",
				Status: *tra.AcceptedStatus,
			},
		}
	}

	return ra
}

func startRouteAdvertisementsInformer(wf *factory.WatchFactory) chan struct{} {
	stopCh := make(chan struct{})
	go wf.RouteAdvertisementsInformer().Informer().Run(stopCh)
	gomega.Eventually(func() bool {
		return wf.RouteAdvertisementsInformer().Informer().HasSynced()
	}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())
	return stopCh
}

var _ = ginkgo.Describe("No-Overlay Controller", func() {
	var (
		recorder *record.FakeRecorder
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		recorder = record.NewFakeRecorder(100)
		// Enable multi-network and route advertisements features
		// These are required for the no-overlay controller to function
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		// Set transport to no-overlay for all tests in this suite
		config.Default.Transport = types.NetworkTransportNoOverlay
	})

	ginkgo.Context("Controller creation", func() {
		ginkgo.It("should create controller when transport is no-overlay", func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller := NewController(wf, recorder)
			gomega.Expect(controller).NotTo(gomega.BeNil())
			gomega.Expect(controller.wf).To(gomega.Equal(wf))
			gomega.Expect(controller.recorder).To(gomega.Equal(recorder))
			gomega.Expect(controller.raController).NotTo(gomega.BeNil())
		})

		ginkgo.It("should have empty last validation state on creation", func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller := NewController(wf, recorder)
			gomega.Expect(controller.lastValidationState).To(gomega.Equal(validationState{}))
		})
	})

	ginkgo.DescribeTable("Validation logic",
		func(ras []*testRA, expectError bool, expectErrorSubstring string) {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			// Create RAs with both spec and status
			for _, tra := range ras {
				ra := tra.RouteAdvertisements()
				_, err := fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(
					context.Background(), ra, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Update status separately if it exists
				if tra.AcceptedStatus != nil {
					ra.Status.Conditions = []metav1.Condition{
						{
							Type:   "Accepted",
							Status: *tra.AcceptedStatus,
						},
					}
					_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(
						context.Background(), ra, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)

			controller := NewController(wf, recorder)

			_, err = controller.validate()
			if expectError {
				gomega.Expect(err).To(gomega.HaveOccurred())
				if expectErrorSubstring != "" {
					gomega.Expect(err.Error()).To(gomega.ContainSubstring(expectErrorSubstring))
				}
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			}
		},
		ginkgo.Entry("should fail when no RouteAdvertisements CR exists",
			[]*testRA{},
			true,
			"no RouteAdvertisements CR is advertising the default network",
		),
		ginkgo.Entry("should pass when valid RouteAdvertisements CR exists",
			[]*testRA{
				{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
			},
			false,
			"",
		),
		ginkgo.Entry("should fail when RouteAdvertisements CR exists but not Accepted",
			[]*testRA{
				{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionFalse),
				},
			},
			true,
			"but none have status Accepted=True",
		),
		ginkgo.Entry("should fail when RouteAdvertisements CR doesn't advertise PodNetwork",
			[]*testRA{
				{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  false,
					Advertisements: []ratypes.AdvertisementType{ratypes.EgressIP},
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
			},
			true,
			"no RouteAdvertisements CR is advertising the default network",
		),
		ginkgo.Entry("should pass when multiple RouteAdvertisements exist and one is valid",
			[]*testRA{
				{
					Name: "test-ra-1",
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork")},
					},
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				{
					Name:           "test-ra-2",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
			},
			false,
			"",
		),
	)

	ginkgo.Context("unmanaged no-overlay validation without RouteAdvertisements", func() {
		ginkgo.BeforeEach(func() {
			config.NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			config.NoOverlay.Routing = config.NoOverlayRoutingUnmanaged
		})

		ginkgo.It("should pass when no RouteAdvertisements CR exists", func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)
			controller := NewController(wf, recorder)

			mode, err := controller.validate()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(mode).To(gomega.Equal(validationModeNoRouteAdvertisements))
		})

		ginkgo.It("should fail when a matching RouteAdvertisements CR exists but is not accepted", func() {
			ra := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionFalse),
			}
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)
			controller := NewController(wf, recorder)

			raObj := ra.RouteAdvertisements()
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), raObj, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), raObj, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(func() bool {
				raObj, getErr := wf.RouteAdvertisementsInformer().Lister().Get("test-ra")
				if getErr != nil || raObj == nil {
					return false
				}
				condition := meta.FindStatusCondition(raObj.Status.Conditions, ratypes.RouteAdvertisementsAccepted)
				return condition != nil && condition.Status == metav1.ConditionFalse
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			_, err = controller.validate()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("but none have status Accepted=True"))
		})

		ginkgo.It("should pass and emit a ready event when RouteAdvertisements are disabled", func() {
			config.OVNKubernetesFeature.EnableRouteAdvertisements = false

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller := NewController(wf, recorder)
			gomega.Expect(controller.raController).To(gomega.BeNil())

			mode, err := controller.validate()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(mode).To(gomega.Equal(validationModeNoRouteAdvertisements))

			gomega.Expect(controller.Start()).To(gomega.Succeed())
			defer controller.Stop()
			var event string
			gomega.Eventually(recorder.Events, 2*time.Second).Should(gomega.Receive(&event))
			gomega.Expect(event).To(gomega.ContainSubstring("NoOverlayConfigurationReady"))
			gomega.Expect(event).To(gomega.ContainSubstring("unmanaged routing and no RouteAdvertisements"))
		})

		ginkgo.It("should fail when RouteAdvertisements are disabled and routing is managed", func() {
			config.OVNKubernetesFeature.EnableRouteAdvertisements = false
			config.NoOverlay.Routing = config.NoOverlayRoutingManaged

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			controller := NewController(wf, recorder)

			_, err = controller.validate()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("RouteAdvertisements are disabled"))
		})
	})

	ginkgo.Context("RA needsValidation logic", func() {
		var controller *Controller
		var wf *factory.WatchFactory
		var err error

		ginkgo.BeforeEach(func() {
			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}

			wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			controller = NewController(wf, recorder)
		})

		ginkgo.AfterEach(func() {
			if wf != nil {
				wf.Shutdown()
			}
		})

		ginkgo.DescribeTable("raNeedsValidation",
			func(oldRA *testRA, newRA *testRA, expectValidate bool) {
				var oldRAObj, newRAObj *ratypes.RouteAdvertisements
				if oldRA != nil {
					oldRAObj = oldRA.RouteAdvertisements()
				}
				if newRA != nil {
					newRAObj = newRA.RouteAdvertisements()
				}
				result := controller.raNeedsValidation(oldRAObj, newRAObj)
				gomega.Expect(result).To(gomega.Equal(expectValidate))
			},
			ginkgo.Entry("should return true when NetworkSelectors change",
				&testRA{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.DefaultNetwork},
					},
				},
				&testRA{
					NetworkSelectors: []apitypes.NetworkSelector{
						{NetworkSelectionType: apitypes.DefaultNetwork},
						{NetworkSelectionType: apitypes.NetworkSelectionType("CustomNetwork")},
					},
				},
				true,
			),
			ginkgo.Entry("should return true when Advertisements change",
				&testRA{
					SelectsDefault: true,
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
				},
				&testRA{
					SelectsDefault: true,
					Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork, ratypes.EgressIP},
				},
				true,
			),
			ginkgo.Entry("should return true when Accepted condition changes",
				&testRA{
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionFalse),
				},
				&testRA{
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				true,
			),
			ginkgo.Entry("should return false when nothing relevant changes",
				&testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				&testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				false,
			),
			ginkgo.Entry("should return true when old RA is nil (addition)",
				nil,
				&testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				true,
			),
			ginkgo.Entry("should return true when new RA is nil (deletion)",
				&testRA{
					Name:           "test-ra",
					SelectsDefault: true,
					AdvertisePods:  true,
					AcceptedStatus: ptr.To(metav1.ConditionTrue),
				},
				nil,
				true,
			),
		)
	})

	ginkgo.Context("Event emission", func() {
		ginkgo.It("should emit event on validation failure", func() {
			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)

			controller := NewController(wf, localRecorder)

			// Start controller which will run initial validation
			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer controller.Stop()

			// Wait a bit for the event to be emitted
			gomega.Eventually(func() int {
				return len(localRecorder.Events)
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeNumerically(">", 0))

			// Check event was emitted
			event := <-localRecorder.Events
			gomega.Expect(event).To(gomega.ContainSubstring("Warning"))
			gomega.Expect(event).To(gomega.ContainSubstring("NoRouteAdvertisements"))
			gomega.Expect(event).To(gomega.ContainSubstring("RouteAdvertisements"))
		})

		ginkgo.It("should not spam events when validation stays failed", func() {
			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)

			controller := NewController(wf, localRecorder)

			// Run validation multiple times with the same error
			controller.runValidation()
			controller.runValidation()
			controller.runValidation()

			// Should only have one event (first validation)
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
		})

		ginkgo.It("should emit Ready event when validation passes after being failed", func() {
			localRecorder := record.NewFakeRecorder(100)

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)

			controller := NewController(wf, localRecorder)

			// First validation will fail (no RouteAdvertisements)
			controller.runValidation()
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
			event1 := <-localRecorder.Events
			gomega.Expect(event1).To(gomega.ContainSubstring("Warning"))

			// Now, create a valid RA to fix the configuration
			validRA := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionTrue),
			}
			ra := validRA.RouteAdvertisements()
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Wait for informer to see the new RA with Accepted=True status
			gomega.Eventually(func() bool {
				raObj, getErr := wf.RouteAdvertisementsInformer().Lister().Get("test-ra")
				if getErr != nil || raObj == nil {
					return false
				}
				for _, c := range raObj.Status.Conditions {
					if c.Type == "Accepted" && c.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(gomega.BeTrue())

			// Second validation will pass
			controller.runValidation()
			gomega.Expect(localRecorder.Events).To(gomega.HaveLen(1))
			event2 := <-localRecorder.Events
			gomega.Expect(event2).To(gomega.ContainSubstring("Normal"))
			gomega.Expect(event2).To(gomega.ContainSubstring("NoOverlayConfigurationReady"))
		})
	})

	ginkgo.Context("Controller lifecycle", func() {
		ginkgo.It("should start and stop without errors", func() {
			ra := testRA{
				Name:           "test-ra",
				SelectsDefault: true,
				AdvertisePods:  true,
				AcceptedStatus: ptr.To(metav1.ConditionTrue),
			}

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:                fake.NewSimpleClientset(),
				NetworkAttchDefClient:     nadfake.NewSimpleClientset(),
				RouteAdvertisementsClient: rafake.NewSimpleClientset(ra.RouteAdvertisements()),
				FRRClient:                 frrfake.NewSimpleClientset(),
			}
			wf, err := factory.NewClusterManagerWatchFactory(fakeClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer wf.Shutdown()

			stopCh := startRouteAdvertisementsInformer(wf)
			defer close(stopCh)

			controller := NewController(wf, recorder)

			err = controller.Start()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Stop should not panic
			gomega.Expect(func() { controller.Stop() }).NotTo(gomega.Panic())
		})
	})
})
