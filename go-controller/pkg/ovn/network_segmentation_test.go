package ovn

import (
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
)

var _ = ginkgo.Describe("OVN Pod Operations with network segmentation", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	const (
		node1Name = "node1"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		fakeOvn = NewFakeOVN(true)
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}

	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("reconciles an existing pod with missing 'role=primary' ovn annotation field", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				// use 2 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				// Remove network role as this is the expected initial
				t1.networkRole = ""

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								"requested-chassis": t1.nodeName,
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{t1.portUUID},
						},
					},
				}

				pod1 := newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*newNode(node1Name, "192.168.126.202/24"),
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod1,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn)
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check db values are updated to correlate with test pods settings
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getDefaultNetExpectedPodsAndSwitches([]testPod{t1}, []string{"node1"})))
				// check annotations are updated with role=primary
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)

				// Expect ovn pod annotated with role=primary
				t1.networkRole = ovntypes.NetworkRolePrimary
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
