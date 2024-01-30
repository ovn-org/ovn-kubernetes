package dnsnameresolver

import (
	"context"
	"time"

	"github.com/miekg/dns"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = ginkgo.Describe("Cluster manager DNS Name Resolver Controller operations", func() {
	var (
		dnsController *Controller
		wf            *factory.WatchFactory
		fakeClient    *util.OVNClusterManagerClientset
	)

	start := func(objects ...runtime.Object) {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDNSNameResolver = true
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dnsController = NewController(fakeClient, wf)

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = dnsController.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		wf = nil
		dnsController = nil
	})

	ginkgo.AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
		}
		if dnsController != nil {
			dnsController.Stop()
		}
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
			start(egressFirewall, dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is still available")
			_, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
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
			start(dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
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
			start(egressFirewall)

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object is correctly created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object got created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
		ginkgo.It("correctly delete a dns name resolver", func() {
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
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object got created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
			err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Delete(context.Background(), egressFirewall.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolvers[0].Name)
				if err == nil || !errors.IsNotFound(err) {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("on update of egress firewall correctly create and delete dns name resolvers", func() {
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
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object got created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
			egressFirewall, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("changing the dns name and updating the egress firewall object")
			egressFirewall.Spec.Egress[0].To.DNSName = dnsName2
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Update(context.Background(), egressFirewall, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object corresponding to the old dns name is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolvers[0].Name)
				if err == nil || !errors.IsNotFound(err) {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if a dns name resolver object is created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
		})
		ginkgo.It("correctly recreate a dns name resolver when inadvertently deleted", func() {
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
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namesapce).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object got created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				dnsNameResolvers, err = wf.GetDNSNameResolvers(config.Kubernetes.OVNConfigNamespace)
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
			err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
				Delete(context.TODO(), existingDNSNameResolver.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("sleeping for 1 second for the dns name resolver object to be removed by the internal cache")
			time.Sleep(1 * time.Second)

			var newDNSNameResolver *ocpnetworkapiv1alpha1.DNSNameResolver
			ginkgo.By("checking if the corresponding dns name resolver object got re-created")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				newDNSNameResolver, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, existingDNSNameResolver.Name)
				if err != nil {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if re-created dns name resolver object matches the deleted object")
			gomega.Expect(newDNSNameResolver).To(gomega.Equal(existingDNSNameResolver))
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
			start()

			ginkgo.By("creating the dns name resolver object")
			_, err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
				Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("sleeping for 1 second for the dns name resolver object to be added by the internal cache")
			time.Sleep(1 * time.Second)

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, err = wf.GetDNSNameResolver(config.Kubernetes.OVNConfigNamespace, dnsNameResolver.Name)
				if err == nil || !errors.IsNotFound(err) {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
