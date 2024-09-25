package dnsnameresolver

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	ocpnetworklisterv1alpha1 "github.com/openshift/client-go/network/listers/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = ginkgo.Describe("Cluster manager DNS Name Resolver Controller operations", func() {
	var (
		dnsController *Controller
		wf            *factory.WatchFactory
		fakeClient    *util.OVNClusterManagerClientset
		dnsLister     ocpnetworklisterv1alpha1.DNSNameResolverLister
	)

	start := func(objects ...runtime.Object) {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDNSNameResolver = true
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		dnsController = NewController(fakeClient, wf)

		dnsSharedIndexInformer := wf.DNSNameResolverInformer().Informer()
		dnsLister = ocpnetworklisterv1alpha1.NewDNSNameResolverLister(dnsSharedIndexInformer.GetIndexer())

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = dnsController.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	buildEgressFirewall := func(name, namespace, dnsName string) *egressfirewallapi.EgressFirewall {
		return &egressfirewallapi.EgressFirewall{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
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
	}

	buildDNSNameResolver := func(name, namespace, dnsName string) *ocpnetworkapiv1alpha1.DNSNameResolver {
		return &ocpnetworkapiv1alpha1.DNSNameResolver{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
				Name: ocpnetworkapiv1alpha1.DNSName(util.LowerCaseFQDN(dnsName)),
			},
		}
	}

	checkDNSNameResolverExists := func(dnsName string) *ocpnetworkapiv1alpha1.DNSNameResolver {
		var dnsNameResolvers []*ocpnetworkapiv1alpha1.DNSNameResolver
		err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {
			dnsNameResolvers, err = dnsLister.DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).List(labels.Everything())
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
		gomega.Expect(dnsNameResolvers[0].Spec.Name).To(gomega.Equal(ocpnetworkapiv1alpha1.DNSName(util.LowerCaseFQDN(dnsName))))
		return dnsNameResolvers[0]
	}

	checkDNSNameResolverRemoved := func(resolverName string) {
		err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Second, true, func(ctx context.Context) (done bool, err error) {
			_, err = dnsLister.DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).Get(resolverName)
			if err == nil || !errors.IsNotFound(err) {
				return false, nil
			}

			return true, nil
		})
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
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName)
			dnsNameResolver := buildDNSNameResolver("dns-example", config.Kubernetes.OVNConfigNamespace, dnsName)
			ginkgo.By("starting the cluster manager with the objects")
			start(egressFirewall, dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is still available")
			_, err = dnsLister.DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).Get(dnsNameResolver.Name)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly sync existing objects to remove dns name resolver object not matching any egress firewall object", func() {
			dnsName := "www.example.com"
			dnsNameResolver := buildDNSNameResolver("dns-example", config.Kubernetes.OVNConfigNamespace, dnsName)
			ginkgo.By("starting the cluster manager with the dns name resolver object")
			start(dnsNameResolver)

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			checkDNSNameResolverRemoved(dnsNameResolver.Name)
		})
		ginkgo.It("correctly sync existing objects to create dns name resolver object matching the existing egress firewall object", func() {
			dnsName := "www.example.com"
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName)
			ginkgo.By("starting the cluster manager with the egress firewall object")
			start(egressFirewall)

			ginkgo.By("checking if the corresponding dns name resolver object is correctly created")
			checkDNSNameResolverExists(dnsName)
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly create a dns name resolver", func() {
			var err error
			dnsName := "www.example.com"
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName)
			ginkgo.By("starting the cluster manager")
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got created")
			checkDNSNameResolverExists(dnsName)
		})
		ginkgo.It("correctly delete a dns name resolver", func() {
			var err error
			dnsName := "www.example.com"
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName)
			ginkgo.By("starting the cluster manager")
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got created")
			dnsNameResolver := checkDNSNameResolverExists(dnsName)

			ginkgo.By("deleting the egress firewall object")
			err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Delete(context.Background(), egressFirewall.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			checkDNSNameResolverRemoved(dnsNameResolver.Name)
		})
		ginkgo.It("on update of egress firewall correctly create and delete dns name resolvers", func() {
			var err error
			dnsName1 := "www.example.com"
			dnsName2 := "www.test.com"
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName1)
			ginkgo.By("starting the cluster manager")
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got created")
			dnsNameResolver := checkDNSNameResolverExists(dnsName1)

			ginkgo.By("fetching the egress firewall object")
			egressFirewall, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Get(context.Background(), egressFirewall.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("changing the dns name and updating the egress firewall object")
			egressFirewall.Spec.Egress[0].To.DNSName = dnsName2
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Update(context.Background(), egressFirewall, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object corresponding to the old dns name is correctly deleted")
			checkDNSNameResolverRemoved(dnsNameResolver.Name)

			ginkgo.By("checking if a dns name resolver object is created")
			checkDNSNameResolverExists(dnsName2)
		})
		ginkgo.It("correctly recreate a dns name resolver when inadvertently deleted", func() {
			var err error
			dnsName := "www.example.com"
			namespace := "namespace1"
			egressFirewall := buildEgressFirewall("default", namespace, dnsName)
			ginkgo.By("starting the cluster manager")
			start()

			ginkgo.By("creating the egress firewall object")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace).
				Create(context.TODO(), egressFirewall, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got created")
			existingDNSNameResolver := checkDNSNameResolverExists(dnsName)
			ginkgo.By("deleting the dns name resolver object")
			err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
				Delete(context.TODO(), existingDNSNameResolver.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got re-created")
			checkDNSNameResolverExists(string(existingDNSNameResolver.Spec.Name))
		})
		ginkgo.It("correctly remove additional dns name resolver object not matching any egress firewall object", func() {
			var err error
			dnsName := "www.example.com"
			dnsNameResolver := buildDNSNameResolver("dns-example", config.Kubernetes.OVNConfigNamespace, dnsName)
			ginkgo.By("starting the cluster manager with the dns name resolver object")
			start()

			ginkgo.By("creating the dns name resolver object")
			_, err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).
				Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			checkDNSNameResolverRemoved(dnsNameResolver.Name)
		})
		ginkgo.It("correctly create dns name resolver and don't delete it until all egress firewall referencing it are deleted", func() {
			var err error
			dnsName := "www.example.com"
			namespace1 := "namespace1"
			egressFirewall1 := buildEgressFirewall("default", namespace1, dnsName)
			namespace2 := "namespace2"
			egressFirewall2 := buildEgressFirewall("default", namespace2, dnsName)
			ginkgo.By("starting the cluster manager")
			start()

			ginkgo.By("creating the egress firewall objects")
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace1).
				Create(context.TODO(), egressFirewall1, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace2).
				Create(context.TODO(), egressFirewall2, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object got created")
			checkDNSNameResolverExists(dnsName)

			ginkgo.By("deleting the first egress firewall object")
			err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace1).
				Delete(context.Background(), egressFirewall1.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the corresponding dns name resolver object is not deleted")
			var dnsNameResolver *ocpnetworkapiv1alpha1.DNSNameResolver
			gomega.Consistently(func() bool {
				dnsNameResolvers, err := dnsLister.DNSNameResolvers(config.Kubernetes.OVNConfigNamespace).List(labels.Everything())
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(len(dnsNameResolvers)).To(gomega.Equal(1))

				dnsNameResolver = checkDNSNameResolverExists(dnsName)

				return dnsNameResolver.Spec.Name == ocpnetworkapiv1alpha1.DNSName(util.LowerCaseFQDN(dnsName))
			}).Should(gomega.BeTrue())

			ginkgo.By("deleting the second egress firewall object")
			err = fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace2).
				Delete(context.Background(), egressFirewall2.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("checking if the dns name resolver object is correctly deleted")
			checkDNSNameResolverRemoved(dnsNameResolver.Name)
		})
	})
})
