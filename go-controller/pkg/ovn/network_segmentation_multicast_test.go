package ovn

import (
	"github.com/urfave/cli/v2"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/onsi/gomega/format"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("[Network Segmentation] OVN Multicast with IP Address Family", func() {
	const (
		namespaceName1 = "namespace1"
		nodeName       = "node1"
	)
	var (
		app                   *cli.App
		fakeOvn               *FakeOVN
		gomegaFormatMaxLength int
		nad                   = ovntest.GenerateNAD("bluenet", "rednad", namespaceName1,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netInfo util.NetInfo
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		config.IPv4Mode = true
		config.IPv6Mode = false
		config.EnableMulticast = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableMultiNetworkPolicy = true

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
				testDefaultMulticastACLCreation(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("updates stale default Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				testUpdateStaleDefaultMulticastACLs(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("cleans up Multicast resources when multicast is disabled", func() {
			app.Action = func(ctx *cli.Context) error {
				testCleanupMulticastResourcesOnDisable(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("creates namespace Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				testCreateNamespaceMulticastACLs(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("updates stale namespace Multicast ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				testUpdateStaleNamespaceMulticastACLs(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
		It("cleans up namespace Multicast ACLs when multicast is disabled for namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				testCleanupNamespaceMulticastACLs(fakeOvn, getNetworkControllerName(netInfo.GetNetworkName()), netInfo, nad)
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
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
