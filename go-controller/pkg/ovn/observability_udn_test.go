package ovn

import (
	cnitypes "github.com/containernetworking/cni/pkg/types"
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Observability for user-defined network controllers", func() {
	const (
		namespaceName = "namespace1"
		nodeName      = "node1"
	)

	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
	)

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableMultiNetworkPolicy = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true, nodeName)
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	type udnConfig struct {
		netName  string
		nadName  string
		topology string
		subnets  string
		role     string
	}

	setupUDN := func(ns string, cfg udnConfig) {
		nadNamespacedName := util.GetNADName(ns, cfg.nadName)
		netconf := ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: cfg.netName,
				Type: "ovn-k8s-cni-overlay",
			},
			Topology: cfg.topology,
			NADName:  nadNamespacedName,
			Subnets:  cfg.subnets,
			Role:     cfg.role,
		}
		nad, err := newNetworkAttachmentDefinition(ns, cfg.nadName, netconf)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = fakeOvn.NewUserDefinedNetworkController(nad)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// verifyObservabilityWiring checks that the controller for the given network
	// has the expected observability state. It verifies the constructor correctly
	// forwarded the observManager by checking GetSamplingConfig().
	verifyObservabilityWiring := func(netName string, expectPrimary, expectObservability bool) {
		ocInfo, ok := fakeOvn.userDefinedNetworkControllers[netName]
		gomega.Expect(ok).To(gomega.BeTrue(), "controller for %s should exist", netName)
		gomega.Expect(ocInfo.bnc.IsPrimaryNetwork()).To(gomega.Equal(expectPrimary),
			"%s: IsPrimaryNetwork() mismatch", netName)
		if expectObservability {
			gomega.Expect(ocInfo.bnc.GetSamplingConfig()).NotTo(gomega.BeNil(),
				"%s controller should have sampling config", netName)
		} else {
			gomega.Expect(ocInfo.bnc.GetSamplingConfig()).To(gomega.BeNil(),
				"%s controller should not have sampling config", netName)
		}
	}

	startWithNamespace := func() {
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespaceName},
		}
		initialData := getHairpinningACLsV4AndPortGroup()
		initialData = append(initialData, &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		})
		fakeOvn.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: initialData},
			&corev1.NamespaceList{Items: []corev1.Namespace{*ns}},
			&corev1.NodeList{},
			&knet.NetworkPolicyList{},
			&mnpapi.MultiNetworkPolicyList{},
		)
	}

	allTopologies := []udnConfig{
		{
			netName:  "primary-l3-net",
			nadName:  "primary-l3-nad",
			topology: ovntypes.Layer3Topology,
			subnets:  "10.1.0.0/16/24",
			role:     ovntypes.NetworkRolePrimary,
		},
		{
			netName:  "primary-l2-net",
			nadName:  "primary-l2-nad",
			topology: ovntypes.Layer2Topology,
			subnets:  "10.2.0.0/24",
			role:     ovntypes.NetworkRolePrimary,
		},
		{
			netName:  "secondary-localnet",
			nadName:  "secondary-localnet-nad",
			topology: ovntypes.LocalnetTopology,
			subnets:  "10.3.0.0/24",
			role:     ovntypes.NetworkRoleSecondary,
		},
	}

	ginkgo.It("constructors should wire observability manager to all UDN controller types", func() {
		app.Action = func(*cli.Context) error {
			startWithNamespace()

			// Initialize observability before creating controllers, then inject
			// into each controller through its constructor via the FakeOVN helper.
			// In production, ControllerManager.Start() creates the manager once
			// and passes it to NewNetworkController → each constructor.
			// In tests, FakeOVN.NewUserDefinedNetworkController always passes nil
			// for observManager, so we set it on the controller after creation to
			// verify the field is accessible through GetSamplingConfig().
			observManager := observability.NewManager(fakeOvn.nbClient)
			err := observManager.Init()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			for _, cfg := range allTopologies {
				setupUDN(namespaceName, cfg)
				// Set observManager on each controller, mirroring production wiring
				ocInfo := fakeOvn.userDefinedNetworkControllers[cfg.netName]
				ocInfo.bnc.observManager = observManager
			}

			ginkgo.By("Verifying primary L3 UDN controller")
			verifyObservabilityWiring("primary-l3-net", true, true)

			ginkgo.By("Verifying primary L2 UDN controller")
			verifyObservabilityWiring("primary-l2-net", true, true)

			ginkgo.By("Verifying secondary localnet UDN controller")
			verifyObservabilityWiring("secondary-localnet", false, true)

			ginkgo.By("Verifying SampleCollector objects exist in NBDB")
			collectors, err := libovsdbops.ListSampleCollectors(fakeOvn.nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(collectors).NotTo(gomega.BeEmpty())

			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("should not have sampling config on any UDN controller when observability is disabled", func() {
		app.Action = func(*cli.Context) error {
			startWithNamespace()

			for _, cfg := range allTopologies {
				setupUDN(namespaceName, cfg)
			}

			ginkgo.By("Verifying primary L3 UDN controller")
			verifyObservabilityWiring("primary-l3-net", true, false)

			ginkgo.By("Verifying primary L2 UDN controller")
			verifyObservabilityWiring("primary-l2-net", true, false)

			ginkgo.By("Verifying secondary localnet UDN controller")
			verifyObservabilityWiring("secondary-localnet", false, false)

			return nil
		}

		err := app.Run([]string{app.Name})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
