package clustermanager

import (
	"context"
	"time"

	"github.com/miekg/dns"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/urfave/cli/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = ginkgo.Describe("Cluster manager DNS Name Resolver operations", func() {
	var (
		app    *cli.App
		fakeCM *FakeClusterManager
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase.
		config.PrepareTestConfig()
		// Enable egress firewall and dns name resolver.
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDNSNameResolver = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeCM = NewFakeClusterManagerOVN()
	})

	ginkgo.AfterEach(func() {
		fakeCM.shutdown()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("correctly sync existing egress firewall and dns name resolver objects", func() {
			var err error
			dnsName := "www.example.com"
			namesapce := "namespace1"
			egressFirewall := &egressfirewallapi.EgressFirewall{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: namesapce,
				},
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: dnsName,
							},
						},
					},
				},
			}
			dnsNameResolver := &ocpnetworkapiv1alpha1.DNSNameResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dns-example",
					Namespace: config.Kubernetes.OVNConfigNamespace,
				},
				Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
					Name: ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName)),
				},
			}
			ginkgo.By("starting the cluster manager with the objects")
			fakeCM.start(egressFirewall, dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is still available")
			_, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly sync existing objects to remove dns name resolver object not matching any egress firewall object", func() {
			var err error
			dnsName := "www.example.com"
			dnsNameResolver := &ocpnetworkapiv1alpha1.DNSNameResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dns-example",
					Namespace: config.Kubernetes.OVNConfigNamespace,
				},
				Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
					Name: ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName)),
				},
			}
			ginkgo.By("starting the cluster manager with the dns name resolver object")
			fakeCM.start(dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
				if err == nil || !errors.IsNotFound(err) {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly sync existing objects to create dns name resolver object matching the existing egress firewall object", func() {
			var err error
			dnsName := "www.example.com"
			namesapce := "namespace1"
			egressFirewall := &egressfirewallapi.EgressFirewall{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "default",
					Namespace: namesapce,
				},
				Spec: egressfirewallapi.EgressFirewallSpec{
					Egress: []egressfirewallapi.EgressFirewallRule{
						{
							Type: egressfirewallapi.EgressFirewallRuleAllow,
							To: egressfirewallapi.EgressFirewallDestination{
								DNSName: dnsName,
							},
						},
					},
				},
			}
			ginkgo.By("starting the cluster manager with the egress firewall object")
			fakeCM.start(egressFirewall)

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object is correctly created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
				if err != nil {
					return false, err
				}

				if len(dnsNameResolvers) != 1 {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the spec.name field of the dns name resolver object matches the dns name of the egress firewall rule")
			gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName))))
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly create a dns name resolver", func() {
			app.Action = func(ctx *cli.Context) error {
				var err error
				dnsName := "www.example.com"
				namesapce := "namespace1"
				egressFirewall := &egressfirewallapi.EgressFirewall{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default",
						Namespace: namesapce,
					},
					Spec: egressfirewallapi.EgressFirewallSpec{
						Egress: []egressfirewallapi.EgressFirewallRule{
							{
								Type: egressfirewallapi.EgressFirewallRuleAllow,
								To: egressfirewallapi.EgressFirewallDestination{
									DNSName: dnsName,
								},
							},
						},
					},
				}
				ginkgo.By("starting the cluster manager")
				fakeCM.start()

				ginkgo.By("creating the egress firewall object")
				_, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
				ginkgo.By("checking if the corresponding dns name resolver object got created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
					if err != nil {
						return false, err
					}

					if len(dnsNameResolvers) != 1 {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the spec.name field of the dns name resolver object matches the dns name of the egress firewall rule")
				gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName))))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly delete a dns name resolver", func() {
			app.Action = func(ctx *cli.Context) error {
				var err error
				dnsName := "www.example.com"
				namesapce := "namespace1"
				egressFirewall := &egressfirewallapi.EgressFirewall{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default",
						Namespace: namesapce,
					},
					Spec: egressfirewallapi.EgressFirewallSpec{
						Egress: []egressfirewallapi.EgressFirewallRule{
							{
								Type: egressfirewallapi.EgressFirewallRuleAllow,
								To: egressfirewallapi.EgressFirewallDestination{
									DNSName: dnsName,
								},
							},
						},
					},
				}
				ginkgo.By("starting the cluster manager")
				fakeCM.start()

				ginkgo.By("creating the egress firewall object")
				_, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
				ginkgo.By("checking if the corresponding dns name resolver object got created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
					if err != nil {
						return false, err
					}

					if len(dnsNameResolvers) != 1 {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the spec.name field of the dns name resolver object matches the dns name of the egress firewall rule")
				gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName))))

				ginkgo.By("deleting the egress firewall object")
				err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Delete(context.Background(), egressFirewall.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the dns name resolver object is correctly deleted")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					_, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolvers[0].Name)
					if err == nil || !errors.IsNotFound(err) {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("on update of egress firewall correctly create and delete dns name resolvers", func() {
			app.Action = func(ctx *cli.Context) error {
				var err error
				dnsName1 := "www.example.com"
				dnsName2 := "www.test.com"
				namesapce := "namespace1"
				egressFirewall := &egressfirewallapi.EgressFirewall{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default",
						Namespace: namesapce,
					},
					Spec: egressfirewallapi.EgressFirewallSpec{
						Egress: []egressfirewallapi.EgressFirewallRule{
							{
								Type: egressfirewallapi.EgressFirewallRuleAllow,
								To: egressfirewallapi.EgressFirewallDestination{
									DNSName: dnsName1,
								},
							},
						},
					},
				}
				ginkgo.By("starting the cluster manager")
				fakeCM.start()

				ginkgo.By("creating the egress firewall object")
				_, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
				ginkgo.By("checking if the corresponding dns name resolver object got created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
					if err != nil {
						return false, err
					}

					if len(dnsNameResolvers) != 1 {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the spec.name field of the dns name resolver object matches the dns name of the egress firewall rule")
				gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName1))))

				ginkgo.By("fetching the egress firewall object")
				egressFirewall, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("changing the dns name and updating the egress firewall object")
				egressFirewall.Spec.Egress[0].To.DNSName = dnsName2
				_, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Update(context.Background(), egressFirewall, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the dns name resolver object corresponding to the old dns name is correctly deleted")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					_, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolvers[0].Name)
					if err == nil || !errors.IsNotFound(err) {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if a dns name resolver object is created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
					if err != nil {
						return false, err
					}

					if len(dnsNameResolvers) != 1 {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the spec.name field of the dns name resolver object matches the new dns name of the egress firewall rule")
				gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName2))))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly recreate a dns name resolver when inadvertently deleted", func() {
			app.Action = func(ctx *cli.Context) error {
				var err error
				dnsName := "www.example.com"
				namesapce := "namespace1"
				egressFirewall := &egressfirewallapi.EgressFirewall{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "default",
						Namespace: namesapce,
					},
					Spec: egressfirewallapi.EgressFirewallSpec{
						Egress: []egressfirewallapi.EgressFirewallRule{
							{
								Type: egressfirewallapi.EgressFirewallRuleAllow,
								To: egressfirewallapi.EgressFirewallDestination{
									DNSName: dnsName,
								},
							},
						},
					},
				}
				ginkgo.By("starting the cluster manager")
				fakeCM.start()

				ginkgo.By("creating the egress firewall object")
				_, err = fakeCM.fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
					Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
				ginkgo.By("checking if the corresponding dns name resolver object got created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					dnsNameResolvers, err = fakeCM.watcher.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
					if err != nil {
						return false, err
					}

					if len(dnsNameResolvers) != 1 {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if the spec.name field of the dns name resolver object matches the dns name of the egress firewall rule")
				gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName))))

				existingDNSNameResolver := dnsNameResolvers[0]
				ginkgo.By("deleting the dns name resolver object")
				err = fakeCM.fakeClient.NetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
					Delete(context.TODO(), existingDNSNameResolver.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("sleeping for 1 second for the dns name resolver object to be removed by the internal cache")
				time.Sleep(1 * time.Second)

				var newDNSNameResolver *ocpnetworkapiv1alpha1.DNSNameResolver
				ginkgo.By("checking if the corresponding dns name resolver object got re-created")
				err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
					newDNSNameResolver, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, existingDNSNameResolver.Name)
					if err != nil {
						return false, nil
					}

					return true, nil
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("checking if re-created dns name resolver object matches the deleted object")
				gomega.Expect(newDNSNameResolver).To(gomega.Equal(existingDNSNameResolver))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly remove additinal dns name resolver object not matching any egress firewall object", func() {
			var err error
			dnsName := "www.example.com"
			dnsNameResolver := &ocpnetworkapiv1alpha1.DNSNameResolver{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dns-example",
					Namespace: config.Kubernetes.OVNConfigNamespace,
				},
				Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
					Name: ocpnetworkapiv1alpha1.DNSName(dns.Fqdn(dnsName)),
				},
			}
			ginkgo.By("starting the cluster manager with the dns name resolver object")
			fakeCM.start()

			ginkgo.By("creating the dns name resolver object")
			_, err = fakeCM.fakeClient.NetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
				Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("sleeping for 1 second for the dns name resolver object to be added by the internal cache")
			time.Sleep(1 * time.Second)

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = fakeCM.watcher.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
				if err == nil || !errors.IsNotFound(err) {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
