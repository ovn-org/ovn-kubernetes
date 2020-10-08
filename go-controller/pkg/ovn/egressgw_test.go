package ovn

import (
	"context"
	"encoding/json"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("OVN Egress Gateway Operations", func() {
	const (
		namespaceName    = "namespace1"
		v4AddressSetName = namespaceName + ipv4AddressSetSuffix
		v6AddressSetName = namespaceName + ipv6AddressSetSuffix
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		fExec   *ovntest.FakeExec
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(fExec)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on setting namespace gateway annotations", func() {

		It("reconciles an new pod with namespace single exgw annotation already set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an new pod with namespace double exgw annotation already set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles deleting a pod with namespace double exgw annotation already set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --if-exists --policy=src-ip lr-route-del GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --if-exists --policy=src-ip lr-route-del GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})
				err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles deleting a exgw namespace with active pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				// delete the namespace
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --if-exists --policy=src-ip lr-route-del GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --if-exists --policy=src-ip lr-route-del GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})

				err := fakeOvn.fakeClient.CoreV1().Namespaces().Delete(context.TODO(), t.namespace, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
	Context("on setting pod gateway annotations", func() {
		It("reconciles a host networked pod acting as a exgw for another namespace for new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceX := *newNamespace("namespace2")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "9.0.0.1")
				gwPod.Annotations = map[string]string{"k8s.ovn.org/routing-namespaces": namespaceT.Name}
				gwPod.Spec.HostNetwork = true
				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT, namespaceX,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							gwPod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a host networked pod acting as a exgw for another namespace for existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceX := *newNamespace("namespace2")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "9.0.0.1")
				gwPod.Annotations = map[string]string{"k8s.ovn.org/routing-namespaces": namespaceT.Name}
				gwPod.Spec.HostNetwork = true
				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT, namespaceX,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				_, err := fakeOvn.fakeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a multus networked pod acting as a exgw for another namespace for new pod", func() {
			app.Action = func(ctx *cli.Context) error {
				ns := nettypes.NetworkStatus{Name: "dummy", IPs: []string{"11.0.0.1"}}
				networkStatuses := []nettypes.NetworkStatus{ns}
				nsEncoded, err := json.Marshal(networkStatuses)
				Expect(err).NotTo(HaveOccurred())

				namespaceT := *newNamespace("namespace1")
				namespaceX := *newNamespace("namespace2")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "9.0.0.1")
				gwPod.Annotations = map[string]string{
					"k8s.ovn.org/routing-namespaces":    namespaceT.Name,
					"k8s.ovn.org/routing-network":       "dummy",
					"k8s.v1.cni.cncf.io/network-status": string(nsEncoded),
				}
				gwPod.Spec.HostNetwork = true
				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT, namespaceX,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							gwPod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				_, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 11.0.0.1",
					Output: "\n",
				})
				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles deleting a host networked pod acting as a exgw for another namespace for existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceX := *newNamespace("namespace2")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "9.0.0.1")
				gwPod.Annotations = map[string]string{"k8s.ovn.org/routing-namespaces": namespaceT.Name}
				gwPod.Spec.HostNetwork = true
				t.baseCmds(fExec)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT, namespaceX,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				_, err := fakeOvn.fakeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --may-exist --policy=src-ip --ecmp-symmetric-reply lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				// delete the GW
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 -- --if-exists --policy=src-ip lr-route-del GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				err = fakeOvn.fakeClient.CoreV1().Pods(namespaceX.Name).Delete(context.TODO(), gwPod.Name, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
