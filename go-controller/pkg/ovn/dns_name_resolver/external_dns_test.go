package dnsnameresolver

import (
	"context"
	"net"
	"time"

	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

func newDNSNameResolverObject(name, namespace, dnsName string, addresses []string) *ocpnetworkapiv1alpha1.DNSNameResolver {
	resolvedAddresses := []ocpnetworkapiv1alpha1.DNSNameResolverResolvedAddress{}
	for _, address := range addresses {
		resolvedAddresses = append(resolvedAddresses, ocpnetworkapiv1alpha1.DNSNameResolverResolvedAddress{
			IP: address,
		})
	}

	return &ocpnetworkapiv1alpha1.DNSNameResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
			Name: ocpnetworkapiv1alpha1.DNSName(dnsName),
		},
		Status: ocpnetworkapiv1alpha1.DNSNameResolverStatus{
			ResolvedNames: []ocpnetworkapiv1alpha1.DNSNameResolverResolvedName{
				{
					DNSName:           ocpnetworkapiv1alpha1.DNSName(dnsName),
					ResolvedAddresses: resolvedAddresses,
				},
			},
		},
	}
}

func expectDNSNameWithAddresses(extEgDNS *ExternalEgressDNS, dnsName string, expectedAddresses []string) {
	var resolvedName *dnsResolvedName
	err := wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
		var exists bool
		resolvedName, exists = extEgDNS.getResolvedName(dnsName)
		if !exists {
			return false, nil
		}

		return true, nil
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	v4, v6 := resolvedName.dnsAddressSet.GetAddresses()
	ipStrings := append(v4, v6...)
	gomega.Expect(ipStrings).To(gomega.ConsistOf(expectedAddresses))
}

var _ = ginkgo.Describe("Egress Firewall External DNS Operations", func() {
	var (
		extEgDNS              *ExternalEgressDNS
		_, clusterSubnet, _   = net.ParseCIDR("10.128.0.0/14")
		wf                    *factory.WatchFactory
		fakeClient            *util.OVNMasterClientset
		fakeAddressSetFactory addressset.AddressSetFactory
		nbClient              libovsdbclient.Client
		testdbCtx             *libovsdbtest.Context
	)

	const (
		dnsName   = "www.example.com."
		namespace = "namespace1"
	)

	start := func(objects ...runtime.Object) {
		var err error

		fakeClient = util.GetOVNClientset(objects...).GetMasterClientset()
		wf, err = factory.NewMasterWatchFactory(fakeClient)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		extEgDNS, err = NewExternalEgressDNS(fakeAddressSetFactory, DefaultNetworkControllerName, true,
			wf.DNSNameResolverInformer().Informer(), wf.EgressFirewallInformer().Lister())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = extEgDNS.Run()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDNSNameResolver = true
		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

		fakeAddressSetFactory = addressset.NewFakeAddressSetFactory(DefaultNetworkControllerName)
	})

	ginkgo.AfterEach(func() {
		wf.Shutdown()
		extEgDNS.Shutdown()
	})

	ginkgo.Context("on cleanup", func() {
		ginkgo.It("correctly deletes stale address set", func() {
			var err error
			nbClient, testdbCtx, err = libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			defer testdbCtx.Cleanup()

			fakeAddressSetFactory = addressset.NewOvnAddressSetFactory(nbClient, true, false)

			dnsName := "www.example.com."
			asIndex := GetEgressFirewallDNSAddrSetDbIDs(dnsName, DefaultNetworkControllerName)
			_, err = fakeAddressSetFactory.NewAddressSet(asIndex, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			start()

			checkAddrSets := func() int {
				predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetEgressFirewallDNS, extEgDNS.dnsTracker.controllerName, nil)
				predicate := libovsdbops.GetPredicate[*nbdb.AddressSet](predicateIDs, nil)

				addrSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, predicate)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return len(addrSets)
			}

			gomega.Eventually(checkAddrSets).ShouldNot(gomega.BeZero())

			err = extEgDNS.DeleteStaleAddrSets(nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(checkAddrSets).Should(gomega.BeZero())
		})

	})

	ginkgo.Context("on dns name resolver resource creation", func() {
		type ipMode string
		const (
			ipV4Mode      ipMode = "ipv4"
			ipV6Mode      ipMode = "ipv6"
			dualStackMode ipMode = "dual-stack"
		)
		ginkgo.DescribeTable("Should add addresses for different ip modes with cluster subnet/without cluster subnet", func(mode ipMode, addresses []string, ignoreClusterSubnet bool, expectedAddresses []string) {
			start()

			switch mode {
			case ipV4Mode:
				config.IPv4Mode = true
				config.IPv6Mode = false
			case ipV6Mode:
				config.IPv4Mode = false
				config.IPv6Mode = true
			case dualStackMode:
				config.IPv4Mode = true
				config.IPv6Mode = true
			}

			extEgDNS.dnsTracker.ignoreClusterSubnet = ignoreClusterSubnet

			dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsName, addresses)

			_, err := fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
				Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectDNSNameWithAddresses(extEgDNS, dnsName, expectedAddresses)
		},
			ginkgo.Entry("Should add IPv4 addresses", ipV4Mode, []string{"1.1.1.1", "2.2.2.2"}, true, []string{"1.1.1.1", "2.2.2.2"}),
			ginkgo.Entry("Should add IPv6 address", ipV6Mode, []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}, true, []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}),
			ginkgo.Entry("Should support dual stack", dualStackMode, []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}, true, []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}),
			ginkgo.Entry("Should only add supported ipV4 addresses", ipV4Mode, []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}, true, []string{"1.1.1.1", "2.2.2.2"}),
			ginkgo.Entry("Should only add supported ipV6 addresses", ipV6Mode, []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}, true, []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}),
			ginkgo.Entry("Should not add IP addresses matching cluster subnet", ipV4Mode, []string{"10.128.0.1"}, true, []string{}),
			ginkgo.Entry("Should add IP addresses matching cluster subnet", ipV4Mode, []string{"10.128.0.1"}, false, []string{"10.128.0.1"}),
			ginkgo.Entry("Should not add IP addresses matching cluster subnet, should add other IPs", ipV4Mode, []string{"10.128.0.1", "1.1.1.1", "2.2.2.2"}, true, []string{"1.1.1.1", "2.2.2.2"}),
			ginkgo.Entry("Should add IP addresses matching cluster subnet, should add other IPs also", ipV4Mode, []string{"10.128.0.1", "1.1.1.1", "2.2.2.2"}, false, []string{"10.128.0.1", "1.1.1.1", "2.2.2.2"}),
		)
	})

	ginkgo.Context("on dns name resolver resource deletion", func() {
		ginkgo.It("Should delete added addresses", func() {
			start()

			config.IPv4Mode = true
			config.IPv6Mode = true

			addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
			dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsName, addresses)

			_, err := fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
				Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectDNSNameWithAddresses(extEgDNS, dnsName, addresses)

			err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
				Delete(context.TODO(), dnsNameResolver.Name, metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				_, exists := extEgDNS.getResolvedName(dnsName)
				if exists {
					return false, nil
				}

				return true, nil
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.It("Should not delete added addresses if DNS name is still used in a namespace", func() {
		start()

		config.IPv4Mode = true
		config.IPv6Mode = true

		addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
		dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsName, addresses)

		_, err := fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
			Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectDNSNameWithAddresses(extEgDNS, dnsName, addresses)

		_, err = extEgDNS.Add(namespace, dnsName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
			Delete(context.TODO(), dnsNameResolver.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Consistently(func() bool {
			_, exists := extEgDNS.getResolvedName(dnsName)
			return exists
		}).Should(gomega.BeTrue())
	})

	ginkgo.It("Should delete added addresses if DNS name is not used in a namespace", func() {
		start()

		config.IPv4Mode = true
		config.IPv6Mode = true

		addresses := []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"}
		dnsNameResolver := newDNSNameResolverObject("dns-default", config.Kubernetes.OVNConfigNamespace, dnsName, addresses)

		_, err := fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
			Create(context.TODO(), dnsNameResolver, metav1.CreateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		expectDNSNameWithAddresses(extEgDNS, dnsName, addresses)

		_, err = extEgDNS.Add(namespace, dnsName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = fakeClient.OCPNetworkClient.NetworkV1alpha1().DNSNameResolvers(dnsNameResolver.Namespace).
			Delete(context.TODO(), dnsNameResolver.Name, metav1.DeleteOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = extEgDNS.Delete(namespace)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 5*time.Minute, true, func(ctx context.Context) (done bool, err error) {
			_, exists := extEgDNS.getResolvedName(dnsName)
			if exists {
				return false, nil
			}

			return true, nil
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})

func (extEgDNS *ExternalEgressDNS) getResolvedName(dnsName string) (*dnsResolvedName, bool) {
	extEgDNS.dnsTracker.dnsLock.Lock()
	defer extEgDNS.dnsTracker.dnsLock.Unlock()

	resolvedName, exists := extEgDNS.dnsTracker.dnsNames[dnsName]
	return resolvedName, exists
}
