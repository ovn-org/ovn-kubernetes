// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"math"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	observabilityconfigv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	observabilityconfigfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/clientset/versioned/fake"
	observabilityconfiginformerfactory "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/informers/externalversions"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	defaultObservabilityCollectorSetID = 1
)

var _ = Describe("Observability Manager", func() {
	var (
		nbClient        libovsdbclient.Client
		libovsdbCleanup *libovsdbtest.Context
		manager         *Manager
		initialDB       []libovsdbtest.TestData
		samplingApps    []libovsdbtest.TestData
	)

	const collectorUUID = "collector-uuid"

	// sampleFeatureToObservabilityFeature maps SampleFeature (ops) to CRD ObservabilityFeature (Multicast -> MulticastIsolation).
	sampleFeatureToObservabilityFeature := func(sf libovsdbops.SampleFeature) observabilityconfigv1alpha1.ObservabilityFeature {
		switch sf {
		case libovsdbops.MulticastSample:
			return observabilityconfigv1alpha1.MulticastIsolation
		default:
			return observabilityconfigv1alpha1.ObservabilityFeature(sf)
		}
	}
	// crFromCollectorConfig builds an ObservabilityConfig CR from the internal collectorConfig for tests.
	crFromCollectorConfig := func(name string, c *collectorConfig, namespaces []string) *observabilityconfigv1alpha1.ObservabilityConfig {
		features := make([]observabilityconfigv1alpha1.FeatureConfig, 0, len(c.featuresProbability))
		for sf, p := range c.featuresProbability {
			features = append(features, observabilityconfigv1alpha1.FeatureConfig{
				Feature:     sampleFeatureToObservabilityFeature(sf),
				Probability: int32(p),
			})
		}
		return &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: int64(c.collectorSetID),
				Features:    features,
				Filter:      &observabilityconfigv1alpha1.Filter{Namespaces: namespaces},
			},
		}
	}

	defaultTestConfig := &collectorConfig{
		collectorSetID: defaultObservabilityCollectorSetID,
		featuresProbability: map[libovsdbops.SampleFeature]int{
			libovsdbops.EgressFirewallSample:     100,
			libovsdbops.NetworkPolicySample:      100,
			libovsdbops.AdminNetworkPolicySample: 100,
			libovsdbops.MulticastSample:          100,
			libovsdbops.UDNIsolationSample:       100,
		},
	}

	startManager := func(data []libovsdbtest.TestData) {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
			NBData: data})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{
			crFromCollectorConfig("default", defaultTestConfig, nil),
		})
		Expect(err).NotTo(HaveOccurred())
	}

	createACLWithPortGroup := func(acl *nbdb.ACL) *nbdb.PortGroup {
		ops, err := libovsdbops.CreateOrUpdateACLsOps(
			nbClient,
			nil,
			manager.SamplingConfigForContext("default-ns", libovsdbops.NetworkPolicySample),
			acl,
		)
		Expect(err).NotTo(HaveOccurred())
		pg := &nbdb.PortGroup{
			UUID: "pg-uuid",
			ACLs: []string{acl.UUID},
		}
		ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, pg)
		Expect(err).NotTo(HaveOccurred())
		_, err = libovsdbops.TransactAndCheck(nbClient, ops)
		Expect(err).NotTo(HaveOccurred())
		return pg
	}

	// createOrUpdateACLPreserveUUID calls CreateOrUpdateACLs and sets the acl.UUID back.
	// that is required as setting real UUID breaks libovsdb matching
	createOrUpdateACLPreserveUUID := func(nbClient libovsdbclient.Client, samplingConfig *libovsdbops.SamplingConfig, acl *nbdb.ACL) error {
		namedUUID := acl.UUID
		err := libovsdbops.CreateOrUpdateACLs(nbClient, samplingConfig, acl)
		acl.UUID = namedUUID
		return err
	}

	BeforeEach(func() {
		initialDB = []libovsdbtest.TestData{
			&nbdb.SamplingApp{
				UUID: "drop-sampling-uuid",
				ID:   DropSamplingID,
				Type: nbdb.SamplingAppTypeDrop,
			},
			&nbdb.SamplingApp{
				UUID: "acl-new-traffic-sampling-uuid",
				ID:   ACLNewTrafficSamplingID,
				Type: nbdb.SamplingAppTypeACLNew,
			},
			&nbdb.SamplingApp{
				UUID: "acl-est-traffic-sampling-uuid",
				ID:   ACLEstTrafficSamplingID,
				Type: nbdb.SamplingAppTypeACLEst,
			},
			&nbdb.SampleCollector{
				UUID:        collectorUUID,
				ID:          1,
				SetID:       defaultObservabilityCollectorSetID,
				Probability: 65535,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: strings.Join([]string{libovsdbops.AdminNetworkPolicySample, libovsdbops.EgressFirewallSample,
						libovsdbops.MulticastSample, libovsdbops.NetworkPolicySample, libovsdbops.UDNIsolationSample}, ","),
				},
			},
		}

		samplingApps = initialDB[:3]
	})

	AfterEach(func() {
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	It("should have only sampling apps in NBDB when feature is enabled but no config applied", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		// No applyConfigs: no collectors, no extra NBDB objects
		Eventually(nbClient).Should(libovsdbtest.HaveData(samplingApps))
	})

	It("should apply an ObservabilityConfig delivered through a started informer (StartWatching)", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		Expect(manager.Init()).To(Succeed())

		cr := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: defaultObservabilityCollectorSetID,
				Features: []observabilityconfigv1alpha1.FeatureConfig{
					{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100},
				},
			},
		}
		fakeClient := observabilityconfigfake.NewSimpleClientset(cr)
		informerFactory := observabilityconfiginformerfactory.NewSharedInformerFactory(fakeClient, 0)
		informer := informerFactory.K8s().V1alpha1().ObservabilityConfigs()
		// Instantiate the shared informer before starting the factory: a SharedInformerFactory only Run()s informers that exist at Start() time.
		sharedInformer := informer.Informer()

		stopCh := make(chan struct{})
		defer close(stopCh)

		// Nothing applied before the informer is wired in.
		Expect(manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)).To(BeNil())

		// Mirror production ordering: start the factory (runs the informer and syncs its cache) before handing the informer to StartWatching.
		informerFactory.Start(stopCh)
		Expect(cache.WaitForCacheSync(stopCh, sharedInformer.HasSynced)).To(BeTrue())

		// The config has no Filter.NodeSelector so it applies regardless of node labels; a minimal
		// node watcher just satisfies the non-nil contract and resolves node1.
		manager.StartWatching(informer, newTestNodeWatcher("node1"), "node1", stopCh)

		Eventually(func() *libovsdbops.SamplingConfig {
			return manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)
		}).ShouldNot(BeNil())
	})

	// Unlike the test above (which pre-seeds the CR and exercises the initial store apply),
	// this one starts with an empty store and mutates the CR after StartWatching, so it
	// exercises the informer's Add/Update/Delete event handlers wired in configReconciler.start.
	It("should react to ObservabilityConfig add/update/delete after StartWatching", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		Expect(manager.Init()).To(Succeed())

		// Start with no CRs so the config only ever arrives through informer events.
		fakeClient := observabilityconfigfake.NewSimpleClientset()
		informerFactory := observabilityconfiginformerfactory.NewSharedInformerFactory(fakeClient, 0)
		informer := informerFactory.K8s().V1alpha1().ObservabilityConfigs()
		sharedInformer := informer.Informer()

		stopCh := make(chan struct{})
		defer close(stopCh)

		informerFactory.Start(stopCh)
		Expect(cache.WaitForCacheSync(stopCh, sharedInformer.HasSynced)).To(BeTrue())
		// The configs below have no Filter.NodeSelector so they apply regardless of node labels; a
		// minimal node watcher just satisfies the non-nil contract and resolves node1.
		manager.StartWatching(informer, newTestNodeWatcher("node1"), "node1", stopCh)

		// Nothing applies before any CR exists.
		Expect(manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)).To(BeNil())

		// AddFunc: creating a CR makes NetworkPolicy sampling apply.
		cr := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: defaultObservabilityCollectorSetID,
				Features: []observabilityconfigv1alpha1.FeatureConfig{
					{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100},
				},
			},
		}
		cr, err = fakeClient.K8sV1alpha1().ObservabilityConfigs().Create(context.Background(), cr, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() *libovsdbops.SamplingConfig {
			return manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)
		}).ShouldNot(BeNil())

		// UpdateFunc: switching the CR to a different feature drops NetworkPolicy sampling and
		// enables EgressFirewall sampling instead.
		cr.Spec.Features = []observabilityconfigv1alpha1.FeatureConfig{
			{Feature: observabilityconfigv1alpha1.EgressFirewall, Probability: 100},
		}
		_, err = fakeClient.K8sV1alpha1().ObservabilityConfigs().Update(context.Background(), cr, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() *libovsdbops.SamplingConfig {
			return manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)
		}).Should(BeNil())
		Eventually(func() *libovsdbops.SamplingConfig {
			return manager.SamplingConfigForContext("ns", libovsdbops.EgressFirewallSample)
		}).ShouldNot(BeNil())

		// DeleteFunc: removing the CR clears all sampling.
		err = fakeClient.K8sV1alpha1().ObservabilityConfigs().Delete(context.Background(), "default", metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() *libovsdbops.SamplingConfig {
			return manager.SamplingConfigForContext("ns", libovsdbops.EgressFirewallSample)
		}).Should(BeNil())
	})

	// Invalid configs are non-convergent (only the user editing the CR can fix them), so
	// applyConfigs skips them with a warning and does NOT fail the reconcile. The error
	// message is asserted directly against the validation function.
	It("should skip ObservabilityConfig with collectorID < 1", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		cr := crFromCollectorConfig("bad", defaultTestConfig, nil)
		cr.Spec.CollectorID = 0
		Expect(validateObservabilityConfig(cr)).To(MatchError(ContainSubstring("collectorID (set_id) must be between 1 and")))
		// applyConfigs skips the invalid config without failing.
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cr})).To(Succeed())
		Expect(manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)).To(BeNil())
	})

	// The consumer side (AddCollector) and producer side (this validation) share the same
	// 1..math.MaxUint32 range, matching the ObservabilityConfig CRD's Maximum (OVS set_id is uint32).
	It("should skip ObservabilityConfig with collectorID above uint32 max", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		cr := crFromCollectorConfig("bad", defaultTestConfig, nil)
		cr.Spec.CollectorID = math.MaxUint32 + 1
		Expect(validateObservabilityConfig(cr)).To(MatchError(ContainSubstring("collectorID (set_id) must be between 1 and")))
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cr})).To(Succeed())
		Expect(manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)).To(BeNil())
	})

	It("should skip ObservabilityConfig with probability > 100", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		cr := crFromCollectorConfig("bad", defaultTestConfig, nil)
		cr.Spec.Features[0].Probability = 101
		Expect(validateObservabilityConfig(cr)).To(MatchError(ContainSubstring("probability must be 0..100")))
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cr})).To(Succeed())
		Expect(manager.SamplingConfigForContext("ns", libovsdbops.NetworkPolicySample)).To(BeNil())
	})

	It("should skip ObservabilityConfig with Filter.Namespaces and cluster-scoped feature", func() {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
		ns := []string{"foo"}
		cr := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "bad"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: 1,
				Features: []observabilityconfigv1alpha1.FeatureConfig{
					{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100},
					{Feature: observabilityconfigv1alpha1.AdminNetworkPolicy, Probability: 100},
				},
				Filter: &observabilityconfigv1alpha1.Filter{Namespaces: ns},
			},
		}
		err = validateObservabilityConfig(cr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("Filter.Namespaces can only be used with namespaced features"))
		Expect(err.Error()).To(ContainSubstring("AdminNetworkPolicy"))
		Expect(manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cr})).To(Succeed())
		Expect(manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample)).To(BeNil())
	})

	for _, dbSetup := range [][]libovsdbtest.TestData{
		nil, initialDB,
	} {
		msg := "db is empty"
		if dbSetup != nil {
			msg = "db is not empty"
		}
		When(msg, func() {

			It("should initialize database", func() {
				startManager(dbSetup)
				Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(initialDB...))
			})

			It("should cleanup database", func() {
				startManager(dbSetup)
				Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(initialDB...))
				err := Cleanup(nbClient)
				Expect(err).NotTo(HaveOccurred())
				Eventually(nbClient).Should(libovsdbtest.HaveEmptyData())
			})

			It("should return correct collectors for an ACL, when feature is enabled", func() {
				startManager(dbSetup)
				collectors, err := libovsdbops.ListSampleCollectors(nbClient)
				Expect(err).NotTo(HaveOccurred())
				Expect(collectors).To(HaveLen(1))
				actualCollectorUUID := collectors[0].UUID

				acl := &nbdb.ACL{
					UUID: "acl-uuid",
					ExternalIDs: map[string]string{
						libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
					},
				}
				pg := createACLWithPortGroup(acl)

				sample := &nbdb.Sample{
					UUID:       "sample-uuid",
					Metadata:   int(libovsdbops.GetACLSampleID(acl)),
					Collectors: []string{actualCollectorUUID},
				}
				acl.SampleNew = &sample.UUID
				acl.SampleEst = &sample.UUID

				Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(append(initialDB, sample, pg, acl)...))
			})
			It("should return correct collectors for an ACL, when feature is disabled", func() {
				startManager(dbSetup)
				acl := &nbdb.ACL{
					UUID: "acl-uuid",
					ExternalIDs: map[string]string{
						libovsdbops.OwnerTypeKey.String(): "disabled-feature",
					},
				}
				pg := createACLWithPortGroup(acl)

				Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(append(initialDB, pg, acl)...))
			})
		})
	}

	It("should update existing ACL, when feature is enabled", func() {
		acl := &nbdb.ACL{
			UUID: "acl-uuid",
			ExternalIDs: map[string]string{
				libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
			},
		}
		pg := &nbdb.PortGroup{
			UUID: "pg-uuid",
			ACLs: []string{acl.UUID},
		}
		startManager(append(initialDB, acl, pg))
		collectors, err := libovsdbops.ListSampleCollectors(nbClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(collectors).To(HaveLen(1))
		actualCollectorUUID := collectors[0].UUID

		err = createOrUpdateACLPreserveUUID(
			nbClient,
			manager.SamplingConfigForContext("", libovsdbops.NetworkPolicySample),
			acl,
		)
		Expect(err).NotTo(HaveOccurred())
		sample := &nbdb.Sample{
			UUID:       "sample-uuid",
			Metadata:   int(libovsdbops.GetACLSampleID(acl)),
			Collectors: []string{actualCollectorUUID},
		}
		acl.SampleNew = &sample.UUID
		acl.SampleEst = &sample.UUID
		Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(append(initialDB, sample, pg, acl)...))
	})

	It("should update existing ACL, when feature is disabled", func() {
		acl := &nbdb.ACL{
			UUID: "acl-uuid",
			ExternalIDs: map[string]string{
				libovsdbops.OwnerTypeKey.String(): "disabled-feature",
			},
		}
		pg := &nbdb.PortGroup{
			UUID: "pg-uuid",
			ACLs: []string{acl.UUID},
		}
		sample := &nbdb.Sample{
			UUID:       "sample-uuid",
			Metadata:   int(libovsdbops.GetACLSampleID(acl)),
			Collectors: []string{collectorUUID},
		}
		acl.SampleNew = &sample.UUID
		acl.SampleEst = &sample.UUID
		startManager(append(initialDB, sample, acl, pg))

		err := createOrUpdateACLPreserveUUID(
			nbClient,
			manager.SamplingConfigForContext("", libovsdbops.NetworkPolicySample),
			acl,
		)
		Expect(err).NotTo(HaveOccurred())
		acl.SampleNew = nil
		acl.SampleEst = nil

		Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(append(initialDB, pg, acl)...))
	})

	It("should generate new sampleID on ACL action change", func() {
		startManager(initialDB)
		acl := &nbdb.ACL{
			UUID:   "acl-uuid",
			Action: nbdb.ACLActionAllowRelated,
			ExternalIDs: map[string]string{
				// NetworkPolicy is enabled by default
				libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
			},
		}
		createACLWithPortGroup(acl)

		// find sample by ACL and save sampleID
		acls, err := libovsdbops.FindACLs(nbClient, []*nbdb.ACL{acl})
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(1))
		sample, err := libovsdbops.GetSample(nbClient, &nbdb.Sample{
			UUID: *acls[0].SampleNew,
		})
		Expect(err).NotTo(HaveOccurred())
		sampleID := sample.Metadata

		// update acl Action
		acl.Action = nbdb.ACLActionDrop
		err = createOrUpdateACLPreserveUUID(
			nbClient,
			manager.SamplingConfigForContext("", libovsdbops.NetworkPolicySample),
			acl,
		)
		Expect(err).NotTo(HaveOccurred())

		// find new sampleID
		acls, err = libovsdbops.FindACLs(nbClient, []*nbdb.ACL{acl})
		Expect(err).NotTo(HaveOccurred())
		Expect(acls).To(HaveLen(1))
		sample, err = libovsdbops.GetSample(nbClient, &nbdb.Sample{
			UUID: *acls[0].SampleNew,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(sample.Metadata).NotTo(Equal(sampleID))
	})

	When("non-default config is used", func() {
		startManagerWithConfig := func(data []libovsdbtest.TestData, config *collectorConfig) {
			var err error
			nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
				NBData: data})
			Expect(err).NotTo(HaveOccurred())
			manager = NewManager(nbClient)
			manager.unusedCollectorsRetryInterval = time.Second
			err = manager.Init()
			Expect(err).NotTo(HaveOccurred())
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{
				crFromCollectorConfig("config", config, nil),
			})
			Expect(err).NotTo(HaveOccurred())
		}

		It("should update stale collectors", func() {
			tweakedConfig := &collectorConfig{
				collectorSetID: defaultObservabilityCollectorSetID,
				featuresProbability: map[libovsdbops.SampleFeature]int{
					libovsdbops.NetworkPolicySample:      50,
					libovsdbops.AdminNetworkPolicySample: 100,
					libovsdbops.MulticastSample:          100,
					libovsdbops.UDNIsolationSample:       100,
				},
			}
			startManagerWithConfig(initialDB, tweakedConfig)
			expectedDB := append(samplingApps,
				&nbdb.SampleCollector{
					UUID:        collectorUUID,
					ID:          1,
					SetID:       defaultObservabilityCollectorSetID,
					Probability: 65535,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: strings.Join([]string{libovsdbops.AdminNetworkPolicySample,
							libovsdbops.MulticastSample, libovsdbops.UDNIsolationSample}, ","),
					},
				},
				&nbdb.SampleCollector{
					UUID:        collectorUUID + "-2",
					ID:          2,
					SetID:       defaultObservabilityCollectorSetID,
					Probability: 32767,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: libovsdbops.NetworkPolicySample,
					},
				},
			)
			Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDB...))
		})
		It("should cleanup stale collectors", func() {
			tweakedConfig := &collectorConfig{
				collectorSetID: defaultObservabilityCollectorSetID,
				featuresProbability: map[libovsdbops.SampleFeature]int{
					libovsdbops.NetworkPolicySample: 50,
				},
			}

			startManagerWithConfig(initialDB, tweakedConfig)
			expectedDB := append(samplingApps,
				&nbdb.SampleCollector{
					UUID:        collectorUUID + "-2",
					ID:          2,
					SetID:       defaultObservabilityCollectorSetID,
					Probability: 32767,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: libovsdbops.NetworkPolicySample,
					},
				},
			)
			Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDB...))
		})
		It("should cleanup stale collectors after samples are removed", func() {
			// tweakedConfig doesn't have probability used by existing collector
			tweakedConfig := &collectorConfig{
				collectorSetID: defaultObservabilityCollectorSetID,
				featuresProbability: map[libovsdbops.SampleFeature]int{
					libovsdbops.EgressFirewallSample: 50,
				},
			}
			acl := &nbdb.ACL{
				UUID: "acl-uuid",
				ExternalIDs: map[string]string{
					// NetworkPolicy is enabled by default
					libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
				},
			}
			pg := &nbdb.PortGroup{
				UUID: "pg-uuid",
				ACLs: []string{acl.UUID},
			}
			sample := &nbdb.Sample{
				UUID:       "sample-uuid",
				Metadata:   int(libovsdbops.GetACLSampleID(acl)),
				Collectors: []string{collectorUUID},
			}
			acl.SampleNew = &sample.UUID
			acl.SampleEst = &sample.UUID
			testInitialDB := append(initialDB, sample, pg, acl)

			startManagerWithConfig(testInitialDB, tweakedConfig)
			newCollector := &nbdb.SampleCollector{
				UUID:        collectorUUID + "-2",
				ID:          2,
				SetID:       defaultObservabilityCollectorSetID,
				Probability: 32767,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: libovsdbops.EgressFirewallSample,
				},
			}
			// initial collector will fail to be cleaned up, since acl sample still references that collector
			expectedDB := append(testInitialDB, newCollector)
			Consistently(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDB...))
			// now imitate netpol handler initialization by updating acl sample.
			err := createOrUpdateACLPreserveUUID(
				nbClient,
				manager.SamplingConfigForContext("", libovsdbops.NetworkPolicySample),
				acl,
			)
			Expect(err).NotTo(HaveOccurred())
			expectedDB = append(samplingApps, pg, acl, newCollector)
			Eventually(nbClient, 2*manager.unusedCollectorsRetryInterval).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDB...))
		})
	})

	When("two configs apply (cluster-wide 42 and namespace foo 24)", func() {
		config42 := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: 42,
				Features: []observabilityconfigv1alpha1.FeatureConfig{
					{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100},
					{Feature: observabilityconfigv1alpha1.AdminNetworkPolicy, Probability: 100},
					{Feature: observabilityconfigv1alpha1.EgressFirewall, Probability: 100},
					{Feature: observabilityconfigv1alpha1.UDNIsolation, Probability: 100},
					{Feature: observabilityconfigv1alpha1.MulticastIsolation, Probability: 100},
				},
			},
		}
		config24 := &observabilityconfigv1alpha1.ObservabilityConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-foo"},
			Spec: observabilityconfigv1alpha1.ObservabilitySpec{
				CollectorID: 24,
				Features:    []observabilityconfigv1alpha1.FeatureConfig{{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100}},
				Filter:      &observabilityconfigv1alpha1.Filter{Namespaces: []string{"foo"}},
			},
		}

		It("should apply both collectors to ACL in namespace foo and only cluster collector to ACL in bar", func() {
			var err error
			nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
			Expect(err).NotTo(HaveOccurred())
			manager = NewManager(nbClient)
			err = manager.Init()
			Expect(err).NotTo(HaveOccurred())
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{config42, config24})
			Expect(err).NotTo(HaveOccurred())

			collectors, err := libovsdbops.ListSampleCollectors(nbClient)
			Expect(err).NotTo(HaveOccurred())
			var uuid42, uuid24 string
			for _, c := range collectors {
				if c.SetID == 42 {
					uuid42 = c.UUID
				}
				if c.SetID == 24 {
					uuid24 = c.UUID
				}
			}
			Expect(uuid42).NotTo(BeEmpty())
			Expect(uuid24).NotTo(BeEmpty())

			// ACL in namespace foo: should get both collectors
			aclFoo := &nbdb.ACL{
				UUID: "acl-foo-uuid",
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
				},
			}
			sampFoo := manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample)
			Expect(sampFoo).NotTo(BeNil())
			ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, sampFoo, aclFoo)
			Expect(err).NotTo(HaveOccurred())
			pgFoo := &nbdb.PortGroup{UUID: "pg-foo-uuid", ACLs: []string{aclFoo.UUID}}
			ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, pgFoo)
			Expect(err).NotTo(HaveOccurred())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			Expect(err).NotTo(HaveOccurred())

			acls, err := libovsdbops.FindACLs(nbClient, []*nbdb.ACL{aclFoo})
			Expect(err).NotTo(HaveOccurred())
			Expect(acls).To(HaveLen(1))
			Expect(acls[0].SampleNew).NotTo(BeNil())
			sampleFoo, err := libovsdbops.GetSample(nbClient, &nbdb.Sample{UUID: *acls[0].SampleNew})
			Expect(err).NotTo(HaveOccurred())
			Expect(sampleFoo.Collectors).To(ContainElement(uuid42))
			Expect(sampleFoo.Collectors).To(ContainElement(uuid24))
			Expect(sampleFoo.Collectors).To(HaveLen(2))

			// ACL in namespace bar: should get only cluster collector 42
			aclBar := &nbdb.ACL{
				UUID: "acl-bar-uuid",
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
				},
			}
			sampBar := manager.SamplingConfigForContext("bar", libovsdbops.NetworkPolicySample)
			Expect(sampBar).NotTo(BeNil())
			ops, err = libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, sampBar, aclBar)
			Expect(err).NotTo(HaveOccurred())
			pgBar := &nbdb.PortGroup{UUID: "pg-bar-uuid", ACLs: []string{aclBar.UUID}}
			ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, pgBar)
			Expect(err).NotTo(HaveOccurred())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			Expect(err).NotTo(HaveOccurred())

			acls, err = libovsdbops.FindACLs(nbClient, []*nbdb.ACL{aclBar})
			Expect(err).NotTo(HaveOccurred())
			Expect(acls).To(HaveLen(1))
			Expect(acls[0].SampleNew).NotTo(BeNil())
			sampleBar, err := libovsdbops.GetSample(nbClient, &nbdb.Sample{UUID: *acls[0].SampleNew})
			Expect(err).NotTo(HaveOccurred())
			Expect(sampleBar.Collectors).To(ContainElement(uuid42))
			Expect(sampleBar.Collectors).To(HaveLen(1))
		})

		It("should deduplicate collector UUIDs when two configs share collectorID and probability", func() {
			// Two cluster-wide configs declare the same collectorID (42) and the same
			// probability (100) for NetworkPolicy: both resolve to the same collector UUID.
			// The merged list must be deduplicated, otherwise the duplicate lands in the
			// Sample.Collectors OVSDB set column and the transaction fails.
			dupA := &observabilityconfigv1alpha1.ObservabilityConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "dup-a"},
				Spec: observabilityconfigv1alpha1.ObservabilitySpec{
					CollectorID: 42,
					Features:    []observabilityconfigv1alpha1.FeatureConfig{{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100}},
				},
			}
			dupB := &observabilityconfigv1alpha1.ObservabilityConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "dup-b"},
				Spec: observabilityconfigv1alpha1.ObservabilitySpec{
					CollectorID: 42,
					Features:    []observabilityconfigv1alpha1.FeatureConfig{{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100}},
				},
			}

			var err error
			nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
			Expect(err).NotTo(HaveOccurred())
			manager = NewManager(nbClient)
			err = manager.Init()
			Expect(err).NotTo(HaveOccurred())
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{dupA, dupB})
			Expect(err).NotTo(HaveOccurred())

			samp := manager.SamplingConfigForContext("any", libovsdbops.NetworkPolicySample)
			Expect(samp).NotTo(BeNil())

			// The resolved collector set for the ACL must contain a single (deduplicated) UUID.
			acl := &nbdb.ACL{
				UUID: "acl-dup-uuid",
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
				},
			}
			ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, samp, acl)
			Expect(err).NotTo(HaveOccurred())
			pg := &nbdb.PortGroup{UUID: "pg-dup-uuid", ACLs: []string{acl.UUID}}
			ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, pg)
			Expect(err).NotTo(HaveOccurred())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			Expect(err).NotTo(HaveOccurred())

			acls, err := libovsdbops.FindACLs(nbClient, []*nbdb.ACL{acl})
			Expect(err).NotTo(HaveOccurred())
			Expect(acls).To(HaveLen(1))
			Expect(acls[0].SampleNew).NotTo(BeNil())
			sample, err := libovsdbops.GetSample(nbClient, &nbdb.Sample{UUID: *acls[0].SampleNew})
			Expect(err).NotTo(HaveOccurred())
			Expect(sample.Collectors).To(HaveLen(1))
		})

		It("should cleanup collector 24 and update samples when config 24 is deleted", func() {
			var err error
			nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{NBData: samplingApps})
			Expect(err).NotTo(HaveOccurred())
			manager = NewManager(nbClient)
			manager.unusedCollectorsRetryInterval = time.Second
			err = manager.Init()
			Expect(err).NotTo(HaveOccurred())
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{config42, config24})
			Expect(err).NotTo(HaveOccurred())

			// Create ACL in foo (references both 42 and 24)
			aclFoo := &nbdb.ACL{
				UUID: "acl-foo-uuid",
				ExternalIDs: map[string]string{
					libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
				},
			}
			ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample), aclFoo)
			Expect(err).NotTo(HaveOccurred())
			pgFoo := &nbdb.PortGroup{UUID: "pg-foo-uuid", ACLs: []string{aclFoo.UUID}}
			ops, err = libovsdbops.CreateOrUpdatePortGroupsOps(nbClient, ops, pgFoo)
			Expect(err).NotTo(HaveOccurred())
			_, err = libovsdbops.TransactAndCheck(nbClient, ops)
			Expect(err).NotTo(HaveOccurred())

			collectors, err := libovsdbops.ListSampleCollectors(nbClient)
			Expect(err).NotTo(HaveOccurred())
			var uuid24 string
			for _, c := range collectors {
				if c.SetID == 24 {
					uuid24 = c.UUID
					break
				}
			}
			Expect(uuid24).NotTo(BeEmpty())

			// Remove config 24; only config 42 remains
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{config42})
			Expect(err).NotTo(HaveOccurred())

			// Re-apply ACL for foo so its sample no longer references collector 24
			err = createOrUpdateACLPreserveUUID(nbClient, manager.SamplingConfigForContext("foo", libovsdbops.NetworkPolicySample), aclFoo)
			Expect(err).NotTo(HaveOccurred())

			// Collector 24 should be removed from NBDB after stale cleanup
			Eventually(func() []*nbdb.SampleCollector {
				list, err := libovsdbops.ListSampleCollectors(nbClient)
				Expect(err).NotTo(HaveOccurred())
				return list
			}, 3*manager.unusedCollectorsRetryInterval).Should(WithTransform(
				func(list []*nbdb.SampleCollector) []int {
					ids := make([]int, 0, len(list))
					for _, c := range list {
						ids = append(ids, c.SetID)
					}
					return ids
				},
				Not(ContainElement(24)),
			))
		})
	})

	When("a collector owned by another component exists in the DB", func() {
		It("should not mark it unused nor delete it", func() {
			// Foreign collector: no collectorFeaturesExternalID external ID, so it is not
			// observability-owned and must be left untouched by retrieval and cleanup.
			foreign := &nbdb.SampleCollector{
				UUID:        "foreign-collector-uuid",
				ID:          200,
				SetID:       200,
				Probability: 65535,
				ExternalIDs: map[string]string{"owner": "some-other-component"},
			}

			var err error
			nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
				NBData: append([]libovsdbtest.TestData{foreign}, samplingApps...),
			})
			Expect(err).NotTo(HaveOccurred())
			manager = NewManager(nbClient)
			err = manager.Init()
			Expect(err).NotTo(HaveOccurred())

			cfg := &observabilityconfigv1alpha1.ObservabilityConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec: observabilityconfigv1alpha1.ObservabilitySpec{
					CollectorID: 42,
					Features:    []observabilityconfigv1alpha1.FeatureConfig{{Feature: observabilityconfigv1alpha1.NetworkPolicy, Probability: 100}},
				},
			}
			err = manager.applyConfigs([]*observabilityconfigv1alpha1.ObservabilityConfig{cfg})
			Expect(err).NotTo(HaveOccurred())

			// The foreign collector is never tracked in manager state.
			Expect(manager.unusedCollectors).NotTo(HaveKey(getCollectorKey(foreign.SetID, foreign.Probability)))
			Expect(manager.dbCollectors).NotTo(HaveKey(getCollectorKey(foreign.SetID, foreign.Probability)))

			// Clearing all configs must sweep observability-owned collectors but leave the foreign one.
			manager.clearConfig()
			Eventually(func() []int {
				list, err := libovsdbops.ListSampleCollectors(nbClient)
				Expect(err).NotTo(HaveOccurred())
				ids := make([]int, 0, len(list))
				for _, c := range list {
					ids = append(ids, c.SetID)
				}
				return ids
			}).Should(And(ContainElement(200), Not(ContainElement(42))))
		})
	})
})
