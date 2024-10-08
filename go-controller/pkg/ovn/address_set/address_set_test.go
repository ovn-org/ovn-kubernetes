package addressset

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func getDbAddrSets(dbIDs *libovsdbops.DbObjectIDs, ipFamily string, ips []string) *nbdb.AddressSet {
	asv4, asv6 := GetTestDbAddrSets(dbIDs, ips)
	if ipFamily == ipv4InternalID {
		asv4.UUID = asv4.Name + "-UUID"
		return asv4
	} else {
		asv6.UUID = asv6.Name + "-UUID"
		return asv6
	}
}

func getDbAsV4(dbIDs *libovsdbops.DbObjectIDs, ips []string) *nbdb.AddressSet {
	return getDbAddrSets(dbIDs, ipv4InternalID, ips)
}

func getDbAsWithUUID(dbIDs *libovsdbops.DbObjectIDs, ips []string, UUID string, ipFamily string) *nbdb.AddressSet {
	as := getDbAddrSets(dbIDs, ipFamily, ips)
	as.UUID = UUID
	return as
}

func getNamespaceAddrSetDbIDs(namespaceName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controller, map[libovsdbops.ExternalIDKey]string{
		// namespace has only 1 address set, no additional ids are required
		libovsdbops.ObjectNameKey: namespaceName,
	})
}

var _ = ginkgo.Describe("OVN Address Set operations", func() {
	const (
		addrsetName    = "foobar"
		ipAddress1     = "1.2.3.4"
		ipAddress2     = "5.6.7.8"
		ipAddress3     = "fd00:10:244::"
		ipAddress4     = "fc00:f853:ccd:e793::4"
		fakeUUID       = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
		fakeUUIDv6     = "8a86f6d8-7972-4253-b0bd-ddbef66e9304"
		controllerName = "fake-controller"
	)

	var (
		app          *cli.App
		asFactory    AddressSetFactory
		testdbCtx    *libovsdbtest.Context
		nbClient     libovsdbclient.Client
		addrsetDbIDs = getNamespaceAddrSetDbIDs(addrsetName, controllerName)
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		var err error
		nbClient, testdbCtx, err = libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.AfterEach(func() {
		testdbCtx.Cleanup()
	})

	ginkgo.Context("when iterating address sets", func() {
		ginkgo.It("calls the iterator function for each address set with given controller name", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name: "1",
						ExternalIDs: map[string]string{
							libovsdbops.OwnerControllerKey.String(): controllerName,
							libovsdbops.OwnerTypeKey.String():       string(libovsdbops.NamespaceOwnerType),
						},
					},
					&nbdb.AddressSet{
						Name: "2",
						ExternalIDs: map[string]string{
							libovsdbops.OwnerControllerKey.String(): controllerName,
							libovsdbops.OwnerTypeKey.String():       string(libovsdbops.NamespaceOwnerType),
							libovsdbops.ObjectNameKey.String():      "ns",
						},
					},
					// another controller name, won't be handled
					&nbdb.AddressSet{
						Name:        "3",
						ExternalIDs: map[string]string{libovsdbops.OwnerControllerKey.String(): "other-controller"},
					},
					// no OwnerTypeKey, won't be handled
					&nbdb.AddressSet{
						Name: "4",
						ExternalIDs: map[string]string{
							libovsdbops.OwnerControllerKey.String(): controllerName,
						},
					},
				}
				err := testdbCtx.NBServer.CreateTestData(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				expectedIndexes := map[string]*libovsdbops.DbObjectIDs{
					"":   libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controllerName, nil),
					"ns": getNamespaceAddrSetDbIDs("ns", controllerName),
				}
				handledIndexes := map[string]*libovsdbops.DbObjectIDs{}
				_ = asFactory.ProcessEachAddressSet(controllerName, libovsdbops.AddressSetNamespace,
					func(dbIDs *libovsdbops.DbObjectIDs) error {
						handledIndexes[dbIDs.GetObjectID(libovsdbops.ObjectNameKey)] = dbIDs
						return nil
					})
				gomega.Expect(handledIndexes).To(gomega.BeEquivalentTo(expectedIndexes))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("when creating an address set object", func() {
		ginkgo.It("re-uses an existing address set and replaces IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					addr1 string = "1.2.3.4"
					addr2 string = "5.6.7.8"
				)
				oldAS := getDbAsV4(addrsetDbIDs, []string{"10.10.10.10"})
				err := testdbCtx.NBServer.CreateTestData([]libovsdbtest.TestData{oldAS})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				_, err = asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{ipAddress1, ipAddress2})

				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clears an existing address set of IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				oldAS := getDbAsV4(addrsetDbIDs, []string{"10.10.10.10"})
				err := testdbCtx.NBServer.CreateTestData([]libovsdbtest.TestData{oldAS})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				_, err = asFactory.NewAddressSet(addrsetDbIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, nil)
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates a new address set and sets IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{ipAddress1, ipAddress2})
				_, err = asFactory.NewAddressSet(addrsetDbIDs, []string{ipAddress1, ipAddress2})
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and returns it", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := []libovsdbtest.TestData{getDbAsV4(addrsetDbIDs, []string{ipAddress1})}
				err := testdbCtx.NBServer.CreateTestData(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.EnsureAddressSet(addrsetDbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{getDbAsV4(addrsetDbIDs, []string{ipAddress1})}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, _ := GetHashNamesForAS(addrsetDbIDs)
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(""))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and returns it, both ip4 and ipv6", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := []libovsdbtest.TestData{
					getDbAsWithUUID(addrsetDbIDs, []string{ipAddress1, ipAddress2}, fakeUUID, ipv4InternalID),
					getDbAsWithUUID(addrsetDbIDs, []string{ipAddress1, ipAddress2}, fakeUUIDv6, ipv6InternalID),
				}
				err := testdbCtx.NBServer.CreateTestData(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true
				config.IPv4Mode = true
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAsWithUUID(addrsetDbIDs, []string{ipAddress1, ipAddress2}, fakeUUID, ipv4InternalID),
					getDbAsWithUUID(addrsetDbIDs, []string{ipAddress1, ipAddress2}, fakeUUIDv6, ipv6InternalID),
				}

				as, err := asFactory.EnsureAddressSet(addrsetDbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, expected_v6 := GetHashNamesForAS(addrsetDbIDs)
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(expected_v6))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an empty address set exists and returns it", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := []libovsdbtest.TestData{
					getDbAsWithUUID(addrsetDbIDs, []string{}, fakeUUID, ipv4InternalID),
				}
				err := testdbCtx.NBServer.CreateTestData(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.EnsureAddressSet(addrsetDbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAsWithUUID(addrsetDbIDs, []string{}, fakeUUID, ipv4InternalID),
				}

				gomega.Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, _ := GetHashNamesForAS(addrsetDbIDs)
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(""))
				ipsv4, ipsv6 := as.GetAddresses()
				gomega.Expect(ipsv4).To(gomega.BeNil())
				gomega.Expect(ipsv6).To(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and if not creates a new one", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, nil)

				as, err := asFactory.EnsureAddressSet(addrsetDbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				v4ips, v6ips := as.GetAddresses()
				gomega.Expect(v4ips).To(gomega.BeNil())
				gomega.Expect(v6ips).To(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.It("destroys an address set", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

			as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{ipAddress1, ipAddress2})

			err = as.Destroy()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState := []libovsdbtest.TestData{}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.Context("when manipulating IPs in an address set object", func() {
		ginkgo.It("adds an IP to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.NewAddressSet(addrsetDbIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.AddAddresses([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{addr1})
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("returns ops to add an IP to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.NewAddressSet(addrsetDbIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.AddAddressesReturnOps([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				// nothing added to address set yet since transact isn't called
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{})
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("returns ops to add duplicate IPs to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.NewAddressSet(addrsetDbIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.AddAddressesReturnOps([]string{addr1, addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				// nothing added to address set yet since transact isn't called
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{})
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("gets all IPs from an address set", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := []libovsdbtest.TestData{
					getDbAsWithUUID(addrsetDbIDs, []string{ipAddress1, ipAddress2}, fakeUUID, ipv4InternalID),
				}
				err := testdbCtx.NBServer.CreateTestData(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv4Mode = true
				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.EnsureAddressSet(addrsetDbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipsv4, ipsv6 := as.GetAddresses()

				gomega.Expect(ipsv4).To(gomega.ConsistOf([]string{ipAddress1, ipAddress2}))
				gomega.Expect(ipsv6).To(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes an IP from an address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
				as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.DeleteAddresses([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Deleting a non-existent address is a no-op
				err = as.DeleteAddresses([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, nil)
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("returns ops to delete an IP from an address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
				as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.DeleteAddressesReturnOps([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				// nothing is deleted from address set yet since transact isn't called
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{addr1})
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("sets an already set addressSet", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"
				const addr2 string = "2.3.4.5"
				const addr3 string = "7.8.9.10"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
				as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.SetAddresses([]string{addr2, addr3})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := getDbAsV4(addrsetDbIDs, []string{addr2, addr3})

				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Dual stack : when creating an address set object", func() {
		ginkgo.It("re-uses an existing dual stack address set and replaces IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					addr1 string = "1.2.3.4"
					addr2 string = "5.6.7.8"
					addr3 string = "2001:db8::1"
					addr4 string = "2001:db8::2"
				)

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				_, err = asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2, addr3, addr4})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, []string{addr1, addr2}),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, []string{addr3, addr4}),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clears an existing address set of dual stack IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					addr1 string = "1.2.3.4"
					addr2 string = "5.6.7.8"
					addr3 string = "2001:db8::1"
					addr4 string = "2001:db8::2"
				)
				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				_, err = asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2, addr3, addr4})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = asFactory.NewAddressSet(addrsetDbIDs, nil)
				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, nil),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, nil),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates a new address set and sets dual stack IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					addr1 string = "1.2.3.4"
					addr2 string = "5.6.7.8"
					addr3 string = "2001:db8::1"
					addr4 string = "2001:db8::2"
				)

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				_, err = asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2, addr3, addr4})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, []string{addr1, addr2}),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, []string{addr3, addr4}),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.It("destroys an dual stack address set", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				addr1 string = "1.2.3.4"
				addr2 string = "5.6.7.8"
				addr3 string = "2001:db8::1"
				addr4 string = "2001:db8::2"
			)
			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.IPv6Mode = true

			asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

			as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2, addr3, addr4})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState := []libovsdbtest.TestData{
				getDbAddrSets(addrsetDbIDs, ipv4InternalID, []string{addr1, addr2}),
				getDbAddrSets(addrsetDbIDs, ipv6InternalID, []string{addr3, addr4}),
			}
			gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

			err = as.Destroy()
			expectedDatabaseState = []libovsdbtest.TestData{}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.Context("Dual Stack : when manipulating IPs in an address set object", func() {
		ginkgo.It("adds IP to an empty dual stack address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"
				const addr2 string = "2001:db8::1"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)

				as, err := asFactory.NewAddressSet(addrsetDbIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, nil),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, nil),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				err = as.AddAddresses([]string{addr1, addr2})
				expectedDatabaseState = []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, []string{addr1}),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, []string{addr2}),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Re-adding is a no-op
				err = as.AddAddresses([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes an IP from an dual stack address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"
				const addr2 string = "2001:db8::1"

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				asFactory = NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
				as, err := asFactory.NewAddressSet(addrsetDbIDs, []string{addr1, addr2})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, []string{addr1}),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, []string{addr2}),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				err = as.DeleteAddresses([]string{addr1, addr2})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{
					getDbAddrSets(addrsetDbIDs, ipv4InternalID, nil),
					getDbAddrSets(addrsetDbIDs, ipv6InternalID, nil),
				}
				gomega.Eventually(nbClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Deleting a non-existent address is a no-op
				err = as.DeleteAddresses([]string{addr1})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
