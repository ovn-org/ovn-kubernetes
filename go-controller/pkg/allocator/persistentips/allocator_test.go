package persistentips

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ipamclaimsapi "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1"
	fakeipamclaimclient "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned/fake"
	ipamclaimsfactory "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/informers/externalversions"
	ipamclaimslister "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/listers/ipamclaims/v1alpha1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/ip/subnet"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	ovnkclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovnktypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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

	Context("retrieving IPAMClaims", func() {
		table.DescribeTable(
			"succeeds",
			func(
				netConf *ovncnitypes.NetConf,
				network *nadapi.NetworkSelectionElement,
				inputClaims *ipamclaimsapi.IPAMClaim,
				expectedClaim *ipamclaimsapi.IPAMClaim,
			) {
				ctx, cancel := context.WithCancel(context.Background())
				lister, listerTeardown := generateIPAMClaimsListerAndTeardownFunc(ctx.Done(), inputClaims)
				defer func() {
					cancel()
					listerTeardown()
				}()

				netInfo, err := util.NewNetInfo(netConf)
				Expect(err).NotTo(HaveOccurred())
				Expect(NewIPAMClaimsFetcher(netInfo, lister).FindIPAMClaim(network)).To(Equal(expectedClaim))
			},
			table.Entry(
				"when an empty claim is passed in layer2 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer2Topology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: "", Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when an empty claim is passed in localnet topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.LocalnetTopology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: "", Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when an empty claim is passed in layer3 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer3Topology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: "", Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when an empty datastore is passed in layer2 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer2Topology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when an empty datastore is passed in localnet topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.LocalnetTopology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when an empty datastore is passed in localnet topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer3Topology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				nil,
				nil,
			),
			table.Entry(
				"when the claim we're looking for is actually passed in layer2 topology for a network without subnets",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer2Topology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				nil,
			),
			table.Entry(
				"when the claim we're looking for is actually passed in localnet topology for a network without subnets",
				&ovncnitypes.NetConf{Topology: ovnktypes.LocalnetTopology},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				nil,
			),
			table.Entry(
				"when the claim we're looking for is actually passed in layer3 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer3Topology, Subnets: "192.10.10.0/16/24"},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				nil,
			),
			table.Entry(
				"when the claim we're looking for is actually passed in layer2 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer2Topology, Subnets: "192.10.10.0/24"},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
			),
			table.Entry(
				"when the claim we're looking for is actually passed in localnet topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.LocalnetTopology, Subnets: "192.10.10.0/24"},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
			),
			table.Entry(
				"when the claim we're looking for is actually passed in layer3 topology",
				&ovncnitypes.NetConf{Topology: ovnktypes.Layer3Topology, Subnets: "192.10.10.0/16/24"},
				&nadapi.NetworkSelectionElement{IPAMClaimReference: claimName, Namespace: namespace},
				ipamClaimWithIPs(namespace, claimName, "192.10.10.10/24"),
				nil,
			),
		)
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

func generateIPAMClaimsListerAndTeardownFunc(stopChannel <-chan struct{}, ipamClaims ...*ipamclaimsapi.IPAMClaim) (ipamclaimslister.IPAMClaimLister, func()) {
	ipamClaimClient := fakeipamclaimclient.NewSimpleClientset(toRuntimeObj(ipamClaims)...)
	informerFactory := ipamclaimsfactory.NewSharedInformerFactory(ipamClaimClient, 0)
	lister := informerFactory.K8s().V1alpha1().IPAMClaims().Lister()
	informerFactory.Start(stopChannel)
	informerFactory.WaitForCacheSync(stopChannel)
	return lister, func() {
		informerFactory.Shutdown()
	}
}

func toRuntimeObj(ipamClaims []*ipamclaimsapi.IPAMClaim) []runtime.Object {
	var castIPAMClaims []runtime.Object
	for i := range ipamClaims {
		if ipamClaims[i] == nil {
			continue
		}
		castIPAMClaims = append(castIPAMClaims, ipamClaims[i])
	}
	return castIPAMClaims
}
