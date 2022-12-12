package addressset

import (
	"net"

	"github.com/urfave/cli/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/libovsdb/ovsdb"
)

type testAddressSetName struct {
	namespace string
	//each suffix in turn
	suffix []string
	remove bool
}

const (
	addrsetName = "foobar"
	ipAddress1  = "1.2.3.4"
	ipAddress2  = "5.6.7.8"
	ipAddress3  = "fd00:10:244::"
	ipAddress4  = "fc00:f853:ccd:e793::4"
	fakeUUID    = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
	fakeUUIDv6  = "8a86f6d8-7972-4253-b0bd-ddbef66e9304"
)

func (asn *testAddressSetName) makeNames() string {
	output := asn.namespace
	for _, suffix := range asn.suffix {
		output = output + "." + suffix
	}
	return output

}

var _ = ginkgo.Describe("OVN Address Set operations", func() {
	var (
		app             *cli.App
		asFactory       AddressSetFactory
		libovsdbCleanup *libovsdbtest.Cleanup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		libovsdbCleanup = nil
	})

	ginkgo.AfterEach(func() {
		libovsdbCleanup.Cleanup()
	})

	ginkgo.Context("when iterating address sets", func() {
		ginkgo.It("calls the iterator function for each address set", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							Name:        "1",
							ExternalIDs: map[string]string{"name": "foo.bar"},
						},
						&nbdb.AddressSet{
							Name:        "2",
							ExternalIDs: map[string]string{"name": "test.test2"},
						},

						&nbdb.AddressSet{
							Name:        "3",
							ExternalIDs: map[string]string{"name": "test3"},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedAddressSets := map[string]bool{
					"foo.bar":    true,
					"test.test2": true,
					"test3":      true,
				}
				err = asFactory.ProcessEachAddressSet(func(hashedName, addrSetName string) error {
					gomega.Expect(expectedAddressSets[addrSetName]).To(gomega.BeTrue())
					delete(expectedAddressSets, addrSetName)
					return nil
				})
				gomega.Expect(len(expectedAddressSets)).To(gomega.Equal(0))
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
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        "",
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{"10.10.10.10"},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{ipAddress1, ipAddress2},
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clears an existing address set of IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        "",
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{"10.10.10.10"},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				_, err = asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   nil,
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates a new address set and sets IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{ipAddress1, ipAddress2},
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}

				_, err = asFactory.NewAddressSet(addrsetName, []net.IP{net.ParseIP(ipAddress1), net.ParseIP(ipAddress2)})
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and returns it", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        fakeUUID,
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{ipAddress1},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				as, err := asFactory.EnsureAddressSet("foobar")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						UUID:        fakeUUID,
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{ipAddress1},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, _ := MakeAddressSetHashNames("foobar")
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(""))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and returns it, both ip4 and ipv6", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        fakeUUID,
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{ipAddress1, ipAddress2},
						},
						&nbdb.AddressSet{
							UUID:        fakeUUIDv6,
							Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
							Addresses:   []string{ipAddress3, ipAddress4},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true
				config.IPv4Mode = true

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						UUID:        fakeUUID,
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{ipAddress1, ipAddress2},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						UUID:        fakeUUIDv6,
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   []string{ipAddress3, ipAddress4},
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}

				as, err := asFactory.EnsureAddressSet("foobar")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, expected_v6 := MakeAddressSetHashNames("foobar")
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(expected_v6))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an empty address set exists and returns it", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        fakeUUID,
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				as, err := asFactory.EnsureAddressSet("foobar")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						UUID:        fakeUUID,
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
				}

				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveData(expectedDatabaseState))
				original_v4, original_v6 := as.GetASHashNames()
				expected_v4, _ := MakeAddressSetHashNames("foobar")
				gomega.Expect(original_v4).To(gomega.Equal(expected_v4))
				gomega.Expect(original_v6).To(gomega.Equal(""))
				ipsv4, ipsv6 := as.GetIPs()
				gomega.Expect(ipsv4).To(gomega.BeNil())
				gomega.Expect(ipsv6).To(gomega.BeNil())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ensures an address set exists and if not creates a new one", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{},
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}

				as, err := asFactory.EnsureAddressSet("foobar")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				v4ips, v6ips := as.GetIPs()
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
			dbSetup := libovsdbtest.TestSetup{}
			var libovsdbOvnNBClient libovsdbclient.Client
			var err error
			libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

			_, err = config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			as, err := asFactory.NewAddressSet(addrsetName, []net.IP{net.ParseIP(ipAddress1), net.ParseIP(ipAddress2)})

			err = as.Destroy()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState := []libovsdbtest.TestData{}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.Context("when manipulating IPs in an address set object", func() {
		ginkgo.It("adds an IP to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.AddIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{addr1},
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("returns ops to add an IP to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.AddIPsReturnOps([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{}, // nothing added to address set yet since transact isn't called
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("returns ops to add duplicate IPs to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"
				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.AddIPsReturnOps([]net.IP{net.ParseIP(addr1), net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{}, // nothing added to address set yet since transact isn't called
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("gets all IPs from an address set", func() {
			app.Action = func(ctx *cli.Context) error {
				dbSetup := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.AddressSet{
							UUID:        fakeUUID,
							Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
							ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
							Addresses:   []string{ipAddress1, ipAddress2},
						},
					},
				}
				var libovsdbOvnNBClient libovsdbclient.Client
				var err error
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				_, err = config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv4Mode = true

				as, err := asFactory.EnsureAddressSet("foobar")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ipsv4, ipsv6 := as.GetIPs()

				gomega.Expect(ipsv4).To(gomega.Equal([]string{ipAddress1, ipAddress2}))
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Deleting a non-existent address is a no-op
				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   nil,
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ops, err := as.DeleteIPsReturnOps([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				var addr1Interface interface{} = addr1
				expectedOps, err := ovsdb.NewOvsSet(addr1Interface)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(ops[0].Mutations[0].Value).To(gomega.Equal(expectedOps))
				expectedDatabaseState := &nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{addr1}, // nothing is deleted from address set yet since transact isn't called,
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.SetIPs([]net.IP{net.ParseIP(addr2), net.ParseIP(addr3)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{addr2, addr3},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
				}

				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
					net.ParseIP(addr3), net.ParseIP(addr4)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{addr1, addr2},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   []string{addr3, addr4},
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
					net.ParseIP(addr3), net.ParseIP(addr4)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = asFactory.NewAddressSet("foobar", nil)
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
					net.ParseIP(addr3), net.ParseIP(addr4)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{addr1, addr2},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   []string{addr3, addr4},
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

			dbSetup := libovsdbtest.TestSetup{}
			var libovsdbOvnNBClient libovsdbclient.Client
			libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

			as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
				net.ParseIP(addr3), net.ParseIP(addr4)})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			expectedDatabaseState := []libovsdbtest.TestData{
				&nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
					Addresses:   []string{addr1, addr2},
					ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
				},
				&nbdb.AddressSet{
					Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
					Addresses:   []string{addr3, addr4},
					ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
				},
			}
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

			err = as.Destroy()
			expectedDatabaseState = []libovsdbtest.TestData{}
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				err = as.AddIPs([]net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{addr1},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   []string{addr2},
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Re-adding is a no-op
				err = as.AddIPs([]net.IP{net.ParseIP(addr1)})
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

				dbSetup := libovsdbtest.TestSetup{}
				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)
				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState := []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   []string{addr1},
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   []string{addr2},
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedDatabaseState = []libovsdbtest.TestData{
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv4AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv4AddressSetSuffix},
					},
					&nbdb.AddressSet{
						Name:        hashedAddressSet(addrsetName + ipv6AddressSetSuffix),
						Addresses:   nil,
						ExternalIDs: map[string]string{"name": addrsetName + ipv6AddressSetSuffix},
					},
				}
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))

				// Deleting a non-existent address is a no-op
				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Dual Stack : when cleaning up old address sets", func() {
		ginkgo.BeforeEach(func() {
		})

		ginkgo.It("destroys address sets in old non dual stack format", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaces := []testAddressSetName{
					{
						// to be removed as v4 address exists
						namespace: "as1",
						suffix:    []string{""},
						remove:    true,
					},
					{
						// to be removed as v6 address exists
						namespace: "as2",
						suffix:    []string{""},
						remove:    true,
					},
					{
						// to be removed as both v4 & v6 address exists
						namespace: "as3",
						suffix:    []string{""},
						remove:    true,
					},
					{
						// not to be removed, no v4 or v6 address exists
						namespace: "as4",
						suffix:    []string{""},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as1",
						suffix:    []string{ipv4AddressSetSuffix},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as2",
						suffix:    []string{ipv6AddressSetSuffix},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as3",
						suffix:    []string{ipv4AddressSetSuffix},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as3",
						suffix:    []string{ipv6AddressSetSuffix},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as5",
						suffix:    []string{ipv4AddressSetSuffix},
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as5",
						suffix:    []string{ipv6AddressSetSuffix},
					},
				}
				expectedDatabaseState := []libovsdbtest.TestData{}

				dbSetup := libovsdbtest.TestSetup{}
				for _, n := range namespaces {
					dbSetup.NBData = append(dbSetup.NBData, &nbdb.AddressSet{
						Name:        hashedAddressSet(n.namespace + n.suffix[0]),
						ExternalIDs: map[string]string{"name": n.namespace + n.suffix[0]},
					})
					if !n.remove {
						expectedDatabaseState = append(expectedDatabaseState, &nbdb.AddressSet{
							Name:        hashedAddressSet(n.namespace + n.suffix[0]),
							ExternalIDs: map[string]string{"name": n.namespace + n.suffix[0]},
						})
					}
				}

				_, err := config.InitConfig(ctx, nil, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				var libovsdbOvnNBClient libovsdbclient.Client
				libovsdbOvnNBClient, _, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				asFactory = NewOvnAddressSetFactory(libovsdbOvnNBClient)

				err = NonDualStackAddressSetCleanup(libovsdbOvnNBClient)
				gomega.Eventually(libovsdbOvnNBClient).Should(libovsdbtest.HaveDataIgnoringUUIDs(expectedDatabaseState))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
