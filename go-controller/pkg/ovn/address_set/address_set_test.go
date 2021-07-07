package addressset

import (
	"fmt"
	"net"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

type testAddressSetName struct {
	namespace string
	suffix1   string
	suffix2   string
}

const (
	fakeUUID   = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
	fakeUUIDv6 = "8a86f6d8-7972-4253-b0bd-ddbef66e9304"
)

func (asn *testAddressSetName) makeName() string {
	return fmt.Sprintf("%s.%s.%s", asn.namespace, asn.suffix1, asn.suffix2)
}

var _ = ginkgo.Describe("OVN Address Set operations", func() {
	var (
		app       *cli.App
		fexec     *ovntest.FakeExec
		asFactory AddressSetFactory
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fexec = ovntest.NewFakeExec()

		asFactory = NewOvnAddressSetFactory()
	})

	ginkgo.JustBeforeEach(func() {
		err := util.SetExec(fexec)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.Context("when iterating address sets", func() {
		ginkgo.It("calls the iterator function for each address set with the given prefix", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				namespaces := []testAddressSetName{
					{
						namespace: "ns1",
						suffix1:   "foo",
						suffix2:   "bar",
					},
					{
						namespace: "ns2",
						suffix1:   "test",
						suffix2:   "test2",
					},
					{
						namespace: "ns3",
					},
				}

				var namespacesRes string
				for i, n := range namespaces {
					name := n.makeName()
					if i%2 == 0 {
						namespacesRes += fmt.Sprintf("keyA=valA,name=%s\n", name)
					} else {
						namespacesRes += fmt.Sprintf("name=%s,keyB=valB\n", name)
					}
				}
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=external_ids find address_set",
					Output: namespacesRes,
				})

				err = asFactory.ProcessEachAddressSet(func(addrSetName, namespaceName, nameSuffix string) {
					found := false
					for _, n := range namespaces {
						name := n.makeName()
						if addrSetName == name {
							found = true
							gomega.Expect(namespaceName).To(gomega.Equal(n.namespace))
							if n.suffix1 != "" {
								gomega.Expect(nameSuffix).To(gomega.Equal(n.suffix1))
							} else {
								gomega.Expect(nameSuffix).To(gomega.Equal(""))
							}
						}
					}
					gomega.Expect(found).To(gomega.BeTrue())
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 set address_set ` + fakeUUID + ` addresses="` + addr1 + `" "` + addr2 + `"`,
				})

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clears an existing address set of IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 clear address_set " + fakeUUID + " addresses",
				})

				_, err = asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("creates a new address set and sets IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				const (
					addr1 string = "1.2.3.4"
					addr2 string = "5.6.7.8"
				)

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4 addresses="` + addr1 + `" "` + addr2 + `"`,
					Output: fakeUUID,
				})

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.It("destroys an address set", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				Output: fakeUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 clear address_set " + fakeUUID + " addresses",
				"ovn-nbctl --timeout=15 --if-exists destroy address_set " + fakeUUID,
			})

			as, err := asFactory.NewAddressSet("foobar", nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = as.Destroy()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.Context("when manipulating IPs in an address set object", func() {
		ginkgo.It("adds an IP to an empty address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 add address_set ` + fakeUUID + ` addresses "` + addr1 + `"`,
				})

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Re-adding is a no-op
				err = as.AddIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes an IP from an address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4 addresses="` + addr1 + `"`,
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 remove address_set ` + fakeUUID + ` addresses "` + addr1 + `"`,
				})

				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Deleting a non-existent address is a no-op
				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4 addresses="` + addr1 + `"`,
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 set address_set ` + fakeUUID + ` addresses="` + addr2 + `" ` + `"` + addr3 + `"`,
				})

				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.SetIPs([]net.IP{net.ParseIP(addr2), net.ParseIP(addr3)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 set address_set ` + fakeUUID + ` addresses="` + addr1 + `" "` + addr2 + `"`,
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
					Output: fakeUUIDv6,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 set address_set ` + fakeUUIDv6 + ` addresses="` + addr3 + `" "` + addr4 + `"`,
				})

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
					net.ParseIP(addr3), net.ParseIP(addr4)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("clears an existing address set of dual stack IPs", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 clear address_set " + fakeUUID + " addresses",
				})

				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
					Output: fakeUUIDv6,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 clear address_set ` + fakeUUIDv6 + " addresses",
				})

				_, err = asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4 addresses="` + addr1 + `" "` + addr2 + `"`,
					Output: fakeUUID,
				})

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990493521189787229 external-ids:name=foobar_v6 addresses="` + addr3 + `" "` + addr4 + `"`,
					Output: fakeUUIDv6,
				})

				_, err = asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2),
					net.ParseIP(addr3), net.ParseIP(addr4)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.It("destroys an dual stack address set", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := config.InitConfig(ctx, fexec, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.IPv6Mode = true

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				Output: fakeUUID,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 clear address_set " + fakeUUID + " addresses",
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
				Output: fakeUUIDv6,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 clear address_set " + fakeUUIDv6 + " addresses",
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --timeout=15 --if-exists destroy address_set " + fakeUUID,
				"ovn-nbctl --timeout=15 --if-exists destroy address_set " + fakeUUIDv6,
			})

			as, err := asFactory.NewAddressSet("foobar", nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = as.Destroy()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4",
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 create address_set name=a16990493521189787229 external-ids:name=foobar_v6",
					Output: fakeUUIDv6,
				})

				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 add address_set ` + fakeUUIDv6 + ` addresses "` + addr2 + `"`,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 add address_set ` + fakeUUID + ` addresses "` + addr1 + `"`,
				})

				as, err := asFactory.NewAddressSet("foobar", nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.AddIPs([]net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Re-adding is a no-op
				err = as.AddIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deletes an IP from an dual stack address set", func() {
			app.Action = func(ctx *cli.Context) error {
				const addr1 string = "1.2.3.4"
				const addr2 string = "2001:db8::1"

				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				config.IPv6Mode = true

				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990491322166530807",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990491322166530807 external-ids:name=foobar_v4 addresses="` + addr1 + `"`,
					Output: fakeUUID,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=a16990493521189787229",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    `ovn-nbctl --timeout=15 create address_set name=a16990493521189787229 external-ids:name=foobar_v6 addresses="` + addr2 + `"`,
					Output: fakeUUIDv6,
				})

				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 remove address_set ` + fakeUUIDv6 + ` addresses "` + addr2 + `"`,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					`ovn-nbctl --timeout=15 remove address_set ` + fakeUUID + ` addresses "` + addr1 + `"`,
				})

				as, err := asFactory.NewAddressSet("foobar", []net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1), net.ParseIP(addr2)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Deleting a non-existent address is a no-op
				err = as.DeleteIPs([]net.IP{net.ParseIP(addr1)})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("Dual Stack : when cleaning up old address sets", func() {
		ginkgo.BeforeEach(func() {
			fexec = ovntest.NewLooseCompareFakeExec()
		})

		ginkgo.It("destroys address sets in old non dual stack format", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := config.InitConfig(ctx, fexec, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				namespaces := []testAddressSetName{
					{
						// to be removed as v4 address exists
						namespace: "as1",
					},
					{
						// to be removed as v6 address exists
						namespace: "as2",
					},
					{
						// to be removed as both v4 & v6 address exists
						namespace: "as3",
					},
					{
						// not to be removed, no v4 or v6 address exists
						namespace: "as4",
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as1",
						suffix2:   ipv4AddressSetSuffix,
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as2",
						suffix2:   ipv6AddressSetSuffix,
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as3",
						suffix2:   ipv4AddressSetSuffix,
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as3",
						suffix2:   ipv6AddressSetSuffix,
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as5",
						suffix2:   ipv4AddressSetSuffix,
					},
					{
						// not to be removed, address in new dual stack format
						namespace: "as5",
						suffix2:   ipv6AddressSetSuffix,
					},
				}

				var namespacesRes string
				for _, n := range namespaces {
					namespacesRes += fmt.Sprintf("name=%s\n", n.makeName())
				}
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --format=csv --data=bare --no-heading --columns=external_ids find address_set",
					Output: namespacesRes,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists destroy address_set " + hashedAddressSet(namespaces[0].makeName()),
					"ovn-nbctl --timeout=15 --if-exists destroy address_set " + hashedAddressSet(namespaces[1].makeName()),
					"ovn-nbctl --timeout=15 --if-exists destroy address_set " + hashedAddressSet(namespaces[2].makeName()),
				})

				err = NonDualStackAddressSetCleanup()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
