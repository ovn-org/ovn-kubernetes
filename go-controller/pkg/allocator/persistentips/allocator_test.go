package persistentips

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	ovnkclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
)

func TestPersistenIPAllocator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Persistent IP allocator Suite")
}

var _ = Describe("Persistent IP allocator operations", func() {
	const (
		claimName  = "claim1"
		namespace  = "ns1"
		subnetName = "dummy-net"
	)
	var (
		persistentIPAllocator *Allocator
		ovnkapiclient         *ovnkclient.KubeOVN
	)

	Context("an existing, but empty IPAMClaim", func() {
		BeforeEach(func() {
			ipAllocator := subnet.NewAllocator()
			ovnkapiclient = &ovnkclient.KubeOVN{
				Kube: ovnkclient.Kube{},
				IPAMClaimsClient: fakeipamclaimclient.NewSimpleClientset(
					emptyDummyIPAMClaim(namespace, claimName),
				),
			}
			Expect(ipAllocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets("192.168.200.0/24", "fd10::/64"))).To(Succeed())
			persistentIPAllocator = NewAllocator(ovnkapiclient, ipAllocator.ForSubnet(subnetName))
		})

		table.DescribeTable("reconciling IPAMClaims is successful when provided with", func(ipamClaim *ipamclaimsapi.IPAMClaim, ips ...string) {
			Expect(persistentIPAllocator.Reconcile(ipamClaim, ips)).To(Succeed())
			updatedIPAMClaim, err := ovnkapiclient.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(namespace).Get(context.Background(), claimName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedIPAMClaim.Status.IPs).To(ConsistOf(ips))
		},
			table.Entry("no IP addresses to persist", emptyDummyIPAMClaim(namespace, claimName)),
			table.Entry("an IP addresses to persist", emptyDummyIPAMClaim(namespace, claimName), "192.168.200.20/24"),
		)
	})

	When("reconciling an IPAMClaim already featuring IPs", func() {
		const originalIPAMClaimIP = "192.168.200.2/24"

		BeforeEach(func() {
			ipAllocator := subnet.NewAllocator()
			ovnkapiclient = &ovnkclient.KubeOVN{
				Kube: ovnkclient.Kube{},
				IPAMClaimsClient: fakeipamclaimclient.NewSimpleClientset(
					ipamClaimWithIPs(namespace, claimName, originalIPAMClaimIP),
				),
			}
			Expect(ipAllocator.AddOrUpdateSubnet(subnetName, ovntest.MustParseIPNets("192.168.200.0/24", "fd10::/64"))).To(Succeed())
			persistentIPAllocator = NewAllocator(ovnkapiclient, ipAllocator.ForSubnet(subnetName))
		})

		It("the IPAMClaim is *not* updated", func() {
			Expect(persistentIPAllocator.Reconcile(
				ipamClaimWithIPs(namespace, claimName, originalIPAMClaimIP),
				[]string{"192.168.200.0/24", "fd10::/64"},
			)).To(Succeed())

			updatedIPAMClaim, err := ovnkapiclient.IPAMClaimsClient.K8sV1alpha1().IPAMClaims(namespace).Get(
				context.Background(),
				claimName,
				metav1.GetOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedIPAMClaim.Status.IPs).To(ConsistOf(originalIPAMClaimIP))
		})
	})
})

func emptyDummyIPAMClaim(namespace string, claimName string) *ipamclaimsapi.IPAMClaim {
	return &ipamclaimsapi.IPAMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: namespace,
		},
		Spec: ipamclaimsapi.IPAMClaimSpec{},
	}
}

func ipamClaimWithIPs(namespace string, claimName string, ips ...string) *ipamclaimsapi.IPAMClaim {
	return &ipamclaimsapi.IPAMClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: namespace,
		},
		Spec: ipamclaimsapi.IPAMClaimSpec{},
		Status: ipamclaimsapi.IPAMClaimStatus{
			IPs: ips,
		},
	}
}
