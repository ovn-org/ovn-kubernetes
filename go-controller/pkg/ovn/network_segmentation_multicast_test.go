package ovn

import (
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/onsi/gomega/format"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func getDefaultPortGroupsForNetwork(netInfo util.NetInfo) (clusterPortGroup, clusterRtrPortGroup *nbdb.PortGroup) {
	clusterPortGroup = newClusterPortGroupForNetwork(netInfo)
	clusterRtrPortGroup = newRouterPortGroupForNetwork(netInfo)
	return
}

var _ = Describe("[Network Segmentation] OVN Multicast with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
		nodeName       = "node1"
	)
	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
		nad                   = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netInfo util.NetInfo
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.EnableMulticast = true

		app = cli.NewApp()
		app.Name = "test"
		// flags are written to config.EnableMulticast
		// if there is no --enable-multicast flag, it will set to false.
		// alternative approach is to give this flag to app.Run, but that require more changes.
		//app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0

		var err error
		netInfo, err = util.ParseNADInfo(nad)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	Context("on startup", func() {
		It("creates default Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroupsForNetwork(netInfo)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: []libovsdb.TestData{
						clusterPortGroup,
						clusterRtrPortGroup,
					},
				})

				Expect(fakeOvn.NewSecondaryNetworkController(nad)).To(Succeed())
				controller, ok := fakeOvn.secondaryControllers[netInfo.GetNetworkName()]
				Expect(ok).To(BeTrue())

				Expect(controller.bnc.createDefaultDenyMulticastPolicy()).To(Succeed())
				Expect(controller.bnc.createDefaultAllowMulticastPolicy()).To(Succeed())

				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastDefaultExpectedDataForNetwork(netInfo, clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("updates stale default Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				// start with stale ACLs
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroupsForNetwork(netInfo)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{
					NBData: getMulticastDefaultStaleDataForNetwork(netInfo, clusterPortGroup, clusterRtrPortGroup),
				})

				Expect(fakeOvn.NewSecondaryNetworkController(nad)).To(Succeed())
				controller, ok := fakeOvn.secondaryControllers[netInfo.GetNetworkName()]
				Expect(ok).To(BeTrue())

				Expect(controller.bnc.createDefaultDenyMulticastPolicy()).To(Succeed())
				Expect(controller.bnc.createDefaultAllowMulticastPolicy()).To(Succeed())

				// check acls are updated
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(
					getMulticastDefaultExpectedDataForNetwork(netInfo, clusterPortGroup, clusterRtrPortGroup)))
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("cleans up Multicast resources when multicast is disabled", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroupsForNetwork(netInfo)
				initialData := getMulticastDefaultExpectedDataForNetwork(netInfo, clusterPortGroup, clusterRtrPortGroup)

				nsData := getMulticastPolicyExpectedDataForNetwork(netInfo, namespaceName1, nil)
				initialData = append(initialData, nsData...)
				// namespace is still present, but multicast support is disabled
				namespace1 := *newNamespace(namespaceName1)
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)

				Expect(fakeOvn.NewSecondaryNetworkController(nad)).To(Succeed())
				controller, ok := fakeOvn.secondaryControllers[netInfo.GetNetworkName()]
				Expect(ok).To(BeTrue())

				// this "if !oc.multicastSupport" part of SetupMaster
				err := controller.bnc.disableMulticast()
				Expect(err).NotTo(HaveOccurred())

				// check acls are deleted when multicast is disabled
				clusterPortGroup, clusterRtrPortGroup = getDefaultPortGroupsForNetwork(netInfo)
				namespacePortGroup := getNamespacePG(namespaceName1, controller.bnc.controllerName)
				expectedData := []libovsdb.TestData{
					clusterPortGroup,
					clusterRtrPortGroup,
					namespacePortGroup,
				}

				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("creates namespace Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroupsForNetwork(netInfo)
				expectedData := getMulticastDefaultExpectedDataForNetwork(netInfo, clusterPortGroup, clusterRtrPortGroup)
				// namespace exists, but multicast acls do not
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Annotations[util.NsMulticastAnnotation] = "true"
				fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
				)

				Expect(fakeOvn.NewSecondaryNetworkController(nad)).To(Succeed())
				controller, ok := fakeOvn.secondaryControllers[netInfo.GetNetworkName()]
				Expect(ok).To(BeTrue())

				err := controller.bnc.WatchNamespaces()
				Expect(err).NotTo(HaveOccurred())
				expectedData = append(expectedData, getMulticastPolicyExpectedDataForNetwork(netInfo, namespaceName1, nil)...)
				Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		/*
			It("updates stale namespace Multicast ACLs", func() {
				app.Action = func(ctx *cli.Context) error {
					// start with stale ACLs for existing namespace
					clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
					expectedData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
					expectedData = append(expectedData, getMulticastPolicyStaleData(namespaceName1, nil)...)
					namespace1 := *newNamespace(namespaceName1)
					namespace1.Annotations[util.NsMulticastAnnotation] = "true"
					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: expectedData},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)

					err := fakeOvn.controller.WatchNamespaces()
					Expect(err).NotTo(HaveOccurred())

					expectedData = getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
					expectedData = append(expectedData, getMulticastPolicyExpectedData(namespaceName1, nil)...)
					Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
					return nil
				}

				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})
			It("cleans up namespace Multicast ACLs when multicast is disabled for namespace", func() {
				app.Action = func(ctx *cli.Context) error {
					// start with stale ACLs
					clusterPortGroup, clusterRtrPortGroup := getDefaultPortGroups()
					defaultMulticastData := getMulticastDefaultExpectedData(clusterPortGroup, clusterRtrPortGroup)
					namespaceMulticastData := getMulticastPolicyExpectedData(namespaceName1, nil)
					namespace1 := *newNamespace(namespaceName1)

					fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: append(defaultMulticastData, namespaceMulticastData...)},
						&v1.NamespaceList{
							Items: []v1.Namespace{
								namespace1,
							},
						},
					)

					err := fakeOvn.controller.WatchNamespaces()
					Expect(err).NotTo(HaveOccurred())
					// only namespaced acls should be dereferenced, default acls will stay
					namespacePortGroup := getNamespacePG(namespaceName1, fakeOvn.controller.controllerName)
					expectedData := append(defaultMulticastData, namespacePortGroup)
					Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
					return nil
				}
				err := app.Run([]string{app.Name})
				Expect(err).NotTo(HaveOccurred())
			})
		*/
	})

	/*
		Context("during execution", func() {
			for _, m := range getIpModes() {
				m := m
				It("tests enabling/disabling multicast in a namespace "+ipModeStr(m), func() {
					app.Action = func(ctx *cli.Context) error {
						namespace1 := *newNamespace(namespaceName1)

						fakeOvn.startWithDBSetup(libovsdb.TestSetup{},
							&v1.NamespaceList{
								Items: []v1.Namespace{
									namespace1,
								},
							},
						)
						setIpMode(m)

						err := fakeOvn.controller.WatchNamespaces()
						Expect(err).NotTo(HaveOccurred())
						ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns).NotTo(BeNil())

						// Multicast is denied by default.
						_, ok := ns.Annotations[util.NsMulticastAnnotation]
						Expect(ok).To(BeFalse())

						// Enable multicast in the namespace.
						updateMulticast(fakeOvn, ns, true)
						expectedData := getMulticastPolicyExpectedData(namespace1.Name, nil)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

						// Disable multicast in the namespace.
						updateMulticast(fakeOvn, ns, false)

						namespacePortGroup := getNamespacePG(namespaceName1, fakeOvn.controller.controllerName)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(namespacePortGroup))
						return nil
					}

					err := app.Run([]string{app.Name})
					Expect(err).NotTo(HaveOccurred())
				})

				It("tests enabling multicast in a namespace with a pod "+ipModeStr(m), func() {
					app.Action = func(ctx *cli.Context) error {
						namespace1 := *newNamespace(namespaceName1)
						pods, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, m)

						fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
							&v1.NamespaceList{
								Items: []v1.Namespace{
									namespace1,
								},
							},
							&v1.NodeList{
								Items: []v1.Node{
									*newNode("node1", "192.168.126.202/24"),
								},
							},
							&v1.PodList{
								Items: pods,
							},
						)
						setIpMode(m)

						for _, tPod := range tPods {
							tPod.populateLogicalSwitchCache(fakeOvn)
						}

						err := fakeOvn.controller.WatchNamespaces()
						Expect(err).NotTo(HaveOccurred())
						err = fakeOvn.controller.WatchPods()
						Expect(err).NotTo(HaveOccurred())
						ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns).NotTo(BeNil())

						// Enable multicast in the namespace
						updateMulticast(fakeOvn, ns, true)
						// calculate expected data
						ports := []string{}
						for _, tPod := range tPods {
							ports = append(ports, tPod.portUUID)
						}
						expectedData := getMulticastPolicyExpectedData(namespace1.Name, ports)
						expectedData = append(expectedData, getExpectedDataPodsAndSwitches(tPods, []string{nodeName})...)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
						fakeOvn.asf.ExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
						return nil
					}

					err := app.Run([]string{app.Name})
					Expect(err).NotTo(HaveOccurred())
				})

				It("tests enabling multicast in multiple namespaces with a long name > 42 characters "+ipModeStr(m), func() {
					app.Action = func(ctx *cli.Context) error {
						longNameSpace1Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
						namespace1 := *newNamespace(longNameSpace1Name)
						longNameSpace2Name := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijl" // create with 63 characters
						namespace2 := *newNamespace(longNameSpace2Name)

						fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
							&v1.NamespaceList{
								Items: []v1.Namespace{
									namespace1,
									namespace2,
								},
							},
						)
						setIpMode(m)

						err := fakeOvn.controller.WatchNamespaces()
						Expect(err).NotTo(HaveOccurred())
						fakeOvn.controller.WatchPods()
						ns1, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns1).NotTo(BeNil())
						ns2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace2.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns2).NotTo(BeNil())

						portsns1 := []string{}
						expectedData := getMulticastPolicyExpectedData(longNameSpace1Name, portsns1)
						acl := expectedData[0].(*nbdb.ACL)
						// Post ACL indexing work, multicast ACL's don't have names
						// We use externalIDs instead; so we can check if the expected IDs exist for the long namespace so that
						// isEquivalent logic will be correct
						Expect(acl.Name).To(BeNil())
						Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longNameSpace1Name))
						expectedData = append(expectedData, getMulticastPolicyExpectedData(longNameSpace2Name, nil)...)
						acl = expectedData[3].(*nbdb.ACL)
						Expect(acl.Name).To(BeNil())
						Expect(acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]).To(Equal(longNameSpace2Name))
						expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...)
						// Enable multicast in the namespace.
						updateMulticast(fakeOvn, ns1, true)
						updateMulticast(fakeOvn, ns2, true)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
						return nil
					}

					err := app.Run([]string{app.Name})
					Expect(err).NotTo(HaveOccurred())
				})

				It("tests adding a pod to a multicast enabled namespace "+ipModeStr(m), func() {
					app.Action = func(ctx *cli.Context) error {
						namespace1 := *newNamespace(namespaceName1)
						_, tPods, tPodIPs := createTestPods(nodeName, namespaceName1, m)

						ports := []string{}
						for _, pod := range tPods {
							ports = append(ports, pod.portUUID)
						}

						fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: getNodeSwitch(nodeName)},
							&v1.NamespaceList{
								Items: []v1.Namespace{
									namespace1,
								},
							},
							&v1.NodeList{
								Items: []v1.Node{
									*newNode("node1", "192.168.126.202/24"),
								},
							},
						)
						setIpMode(m)

						err := fakeOvn.controller.WatchNamespaces()
						Expect(err).NotTo(HaveOccurred())
						err = fakeOvn.controller.WatchPods()
						Expect(err).NotTo(HaveOccurred())
						ns, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespace1.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns).NotTo(BeNil())

						// Enable multicast in the namespace.
						updateMulticast(fakeOvn, ns, true)
						// Check expected data without pods
						expectedDataWithoutPods := getMulticastPolicyExpectedData(namespace1.Name, nil)
						expectedDataWithoutPods = append(expectedDataWithoutPods, getNodeSwitch(nodeName)...)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

						// Create pods
						for _, tPod := range tPods {
							tPod.populateLogicalSwitchCache(fakeOvn)
							_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Create(context.TODO(), newPod(
								tPod.namespace, tPod.podName, tPod.nodeName, tPod.podIP), metav1.CreateOptions{})
							Expect(err).NotTo(HaveOccurred())
						}
						// Check pods were added
						fakeOvn.asf.EventuallyExpectAddressSetWithAddresses(namespace1.Name, tPodIPs)
						expectedDataWithPods := getMulticastPolicyExpectedData(namespace1.Name, ports)
						expectedDataWithPods = append(expectedDataWithPods, getExpectedDataPodsAndSwitches(tPods, []string{nodeName})...)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPods...))

						// Delete the pod from the namespace.
						for _, tPod := range tPods {
							err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(tPod.namespace).Delete(context.TODO(),
								tPod.podName, *metav1.NewDeleteOptions(0))
							Expect(err).NotTo(HaveOccurred())
						}
						fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespace1.Name)
						Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPods))

						return nil
					}

					err := app.Run([]string{app.Name})
					Expect(err).NotTo(HaveOccurred())
				})
			}
		})
	*/
})
