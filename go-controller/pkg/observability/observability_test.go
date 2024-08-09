package observability

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

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

	initialDB := []libovsdbtest.TestData{
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
		},
	}

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
					Metadata:   int(libovsdbops.GetSampleID(acl)),
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
})
