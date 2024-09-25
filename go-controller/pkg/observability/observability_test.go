package observability

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"strings"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
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

	startManager := func(data []libovsdbtest.TestData) {
		var err error
		nbClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{
			NBData: data})
		Expect(err).NotTo(HaveOccurred())
		manager = NewManager(nbClient)
		err = manager.Init()
		Expect(err).NotTo(HaveOccurred())
	}

	createACLWithPortGroup := func(acl *nbdb.ACL) *nbdb.PortGroup {
		ops, err := libovsdbops.CreateOrUpdateACLsOps(nbClient, nil, manager.SamplingConfig(), acl)
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
				SetID:       DefaultObservabilityCollectorSetID,
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
				Eventually(nbClient).Should(libovsdbtest.HaveData(initialDB))
			})

			It("should cleanup database", func() {
				startManager(dbSetup)
				Eventually(nbClient).Should(libovsdbtest.HaveData(initialDB))
				err := Cleanup(nbClient)
				Expect(err).NotTo(HaveOccurred())
				Eventually(nbClient).Should(libovsdbtest.HaveEmptyData())
			})

			It("should return correct collectors for an ACL, when feature is enabled", func() {
				startManager(dbSetup)

				acl := &nbdb.ACL{
					UUID: "acl-uuid",
					ExternalIDs: map[string]string{
						// NetworkPolicy is enabled by default
						libovsdbops.OwnerTypeKey.String(): libovsdbops.NetworkPolicyOwnerType,
					},
				}
				pg := createACLWithPortGroup(acl)

				sample := &nbdb.Sample{
					UUID:       "sample-uuid",
					Metadata:   int(libovsdbops.GetACLSampleID(acl)),
					Collectors: []string{collectorUUID},
				}
				acl.SampleNew = &sample.UUID
				acl.SampleEst = &sample.UUID

				Eventually(nbClient).Should(libovsdbtest.HaveData(append(initialDB, sample, pg, acl)))
			})
			It("should return correct collectors for an ACL, when feature is disabled", func() {
				startManager(dbSetup)
				acl := &nbdb.ACL{
					UUID: "acl-uuid",
					ExternalIDs: map[string]string{
						// disabled-feature doesn't exist => not enabled
						libovsdbops.OwnerTypeKey.String(): "disabled-feature",
					},
				}
				pg := createACLWithPortGroup(acl)

				Eventually(nbClient).Should(libovsdbtest.HaveData(append(initialDB, pg, acl)))
			})
		})
	}

	It("should update existing ACL, when feature is enabled", func() {
		// start with ACL that doesn't have samples
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
		startManager(append(initialDB, acl, pg))

		err := createOrUpdateACLPreserveUUID(nbClient, manager.SamplingConfig(), acl)
		Expect(err).NotTo(HaveOccurred())
		// expect sample to be added to the existing acl
		sample := &nbdb.Sample{
			UUID:       "sample-uuid",
			Metadata:   int(libovsdbops.GetACLSampleID(acl)),
			Collectors: []string{collectorUUID},
		}
		acl.SampleNew = &sample.UUID
		acl.SampleEst = &sample.UUID
		Eventually(nbClient).Should(libovsdbtest.HaveData(append(initialDB, sample, pg, acl)))
	})

	It("should update existing ACL, when feature is disabled", func() {
		// start with ACL that has samples
		acl := &nbdb.ACL{
			UUID: "acl-uuid",
			ExternalIDs: map[string]string{
				// disabled-feature doesn't exist => not enabled
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

		err := createOrUpdateACLPreserveUUID(nbClient, manager.SamplingConfig(), acl)
		Expect(err).NotTo(HaveOccurred())
		// expect sample to be removed from the existing acl
		acl.SampleNew = nil
		acl.SampleEst = nil

		Eventually(nbClient).Should(libovsdbtest.HaveData(append(initialDB, pg, acl)))
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
		err = createOrUpdateACLPreserveUUID(nbClient, manager.SamplingConfig(), acl)
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
			// tweak retry interval for testing
			manager.unusedCollectorsRetryInterval = time.Second
			err = manager.initWithConfig(config)
			Expect(err).NotTo(HaveOccurred())
		}

		It("should update stale collectors", func() {
			// tweakedConfig doesn't have EgressFirewall enabled, and sets different probability for NetworkPolicy
			tweakedConfig := &collectorConfig{
				collectorSetID: DefaultObservabilityCollectorSetID,
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
					SetID:       DefaultObservabilityCollectorSetID,
					Probability: 65535,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: strings.Join([]string{libovsdbops.AdminNetworkPolicySample,
							libovsdbops.MulticastSample, libovsdbops.UDNIsolationSample}, ","),
					},
				},
				&nbdb.SampleCollector{
					UUID:        collectorUUID + "-2",
					ID:          2,
					SetID:       DefaultObservabilityCollectorSetID,
					Probability: 32767,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: libovsdbops.NetworkPolicySample,
					},
				},
			)
			Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDB))
		})
		It("should cleanup stale collectors", func() {
			// tweakedConfig doesn't have probability used by existing collector
			tweakedConfig := &collectorConfig{
				collectorSetID: DefaultObservabilityCollectorSetID,
				featuresProbability: map[libovsdbops.SampleFeature]int{
					libovsdbops.NetworkPolicySample: 50,
				},
			}

			startManagerWithConfig(initialDB, tweakedConfig)
			expectedDB := append(samplingApps,
				&nbdb.SampleCollector{
					UUID:        collectorUUID + "-2",
					ID:          2,
					SetID:       DefaultObservabilityCollectorSetID,
					Probability: 32767,
					ExternalIDs: map[string]string{
						collectorFeaturesExternalID: libovsdbops.NetworkPolicySample,
					},
				},
			)
			Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDB))
		})
		It("should cleanup stale collectors after samples are removed", func() {
			// tweakedConfig doesn't have probability used by existing collector
			tweakedConfig := &collectorConfig{
				collectorSetID: DefaultObservabilityCollectorSetID,
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
				SetID:       DefaultObservabilityCollectorSetID,
				Probability: 32767,
				ExternalIDs: map[string]string{
					collectorFeaturesExternalID: libovsdbops.EgressFirewallSample,
				},
			}
			// initial collector will fail to be cleaned up, since acl sample still references that collector
			expectedDB := append(testInitialDB, newCollector)
			Consistently(nbClient).Should(libovsdbtest.HaveData(expectedDB))
			// now imitate netpol handler initialization by updating acl sample.
			err := createOrUpdateACLPreserveUUID(nbClient, manager.SamplingConfig(), acl)
			Expect(err).NotTo(HaveOccurred())
			// sample is removed, collector should be cleaned up now
			expectedDB = append(samplingApps, pg, acl, newCollector)
			Eventually(nbClient, 2*manager.unusedCollectorsRetryInterval).Should(libovsdbtest.HaveData(expectedDB))
		})
	})
})
