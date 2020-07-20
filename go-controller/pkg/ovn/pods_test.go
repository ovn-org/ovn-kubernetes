package ovn

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func getPodAnnotations(fakeClient *fake.Clientset, namespace, name string) string {
	pod, err := fakeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return pod.Annotations[util.OvnPodAnnotationName]
}

func newPodMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(namespace, name, node, podIP string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: newPodMeta(namespace, name),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: podIP,
			PodIPs: []v1.PodIP{
				{IP: podIP},
			},
		},
	}
}

type pod struct {
	nodeName   string
	nodeSubnet string
	nodeMgtIP  string
	nodeGWIP   string
	podName    string
	podIP      string
	podMAC     string
	namespace  string
	portName   string
}

func newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIP, podMAC, namespace string) (to pod) {
	to = pod{
		nodeName:   nodeName,
		nodeSubnet: nodeSubnet,
		nodeMgtIP:  nodeMgtIP,
		nodeGWIP:   nodeGWIP,
		podName:    podName,
		podIP:      podIP,
		podMAC:     podMAC,
		namespace:  namespace,
		portName:   namespace + "_" + podName,
	}
	return
}

func (p pod) baseCmds(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
		Output: "\n",
	})
}

func (p pod) populateLogicalSwitchCache(fakeOvn *FakeOVN) {
	Expect(p.nodeName).NotTo(Equal(""))
	fakeOvn.controller.lsManager.AddNode(p.nodeName, []*net.IPNet{ovntest.MustParseIPNet(p.nodeSubnet)})
}

func (p pod) addCmds(fexec *ovntest.FakeExec, fail bool) {
	// pod setup
	if !fail {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists get logical_switch_port" + " " + p.portName + " dynamic_addresses addresses",
		})

		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --may-exist lsp-add " + p.nodeName + " " + p.portName + " -- lsp-set-addresses " + p.portName + " " + p.podMAC + " " + p.podIP + " -- set logical_switch_port " + p.portName + " external-ids:namespace=" + p.namespace + " external-ids:pod=true -- lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP,
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + p.portName + " _uuid",
			Output: fakeUUID + "\n",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID + " -- add port_group mcastPortGroupDeny ports " + fakeUUID,
		})

	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists get logical_switch_port" + " " + p.portName + " dynamic_addresses addresses",
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: strings.Join([]string{
				"ovn-nbctl --timeout=15 --may-exist lsp-add " + p.nodeName + " " + p.portName + " -- lsp-set-addresses " + p.portName + " " + p.podMAC + " " + p.podIP + " -- set logical_switch_port " + p.portName + " external-ids:namespace=" + p.namespace + " external-ids:pod=true -- lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP,
			}, " "),
			Err: fmt.Errorf("adsfadsfasfdasfd"),
		})
	}

}

func (p pod) addCmdsForNonExistingPod(fexec *ovntest.FakeExec) {
	p.addCmds(fexec, false)
}

func (p pod) delCmds(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists lsp-del " + p.portName,
	})
}

func (p pod) addPodDenyMcast(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID + " -- add port_group mcastPortGroupDeny ports " + fakeUUID,
	})
}

var _ = Describe("OVN Pod Operations", func() {
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

		fExec = ovntest.NewFakeExec()
		fakeOvn = NewFakeOVN(fExec)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("during execution", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

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
				fakeOvn.controller.WatchPods()
				fakeOvn.controller.WatchNamespaces()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.portName = t.namespace + "_" + t.podName
				t.populateLogicalSwitchCache(fakeOvn)
				t.addPodDenyMcast(fExec)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Update(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
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
						Items: []v1.Pod{},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				Expect(pod).To(BeNil())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				t.addPodDenyMcast(fExec)

				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an assigned pod
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addPodDenyMcast(fExec)

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
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				// Delete it
				t.delCmds(fExec)

				err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(pod).To(BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("retries a failed pod Add on Update", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

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
				mockAddNBDBError(ovntest.LogicalSwitchPortType, t.portName,
					ovntest.LogicalSwitchPortPortSecurity,
					fmt.Errorf("injected dummy port security set error"),
					fakeOvn.ovnNBClient)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)
				mockDelNBDBError(ovntest.LogicalSwitchPortType, t.portName,
					ovntest.LogicalSwitchPortPortSecurity,
					fakeOvn.ovnNBClient)

				// Pod creation should be retried on Update event
				t.addPodDenyMcast(fExec)
				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Update(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)
				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on startup", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists lsp-del " + t.portName,
				})
				t.addPodDenyMcast(fExec)

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

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists lsp-del " + t.portName,
				})

				fakeOvn.start(ctx)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addPodDenyMcast(fExec)

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
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on ovn restart", func() {

		It("reconciles an existing pod without an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addPodDenyMcast(fExec)

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
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)
				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				// Simulate an OVN restart with a new IP assignment and verify that the pod annotation is updated.
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.populateLogicalSwitchCache(fakeOvn)
				t.addPodDenyMcast(fExec)

				fakeOvn.restart()
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing pod with an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
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

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addPodDenyMcast(fExec)

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
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				// Simulate an OVN restart with a new IP assignment and verify that the pod annotation is updated.
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.populateLogicalSwitchCache(fakeOvn)
				t.addPodDenyMcast(fExec)

				fakeOvn.restart()
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient, t.namespace, t.podName) }, 2).Should(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with hybrid overlay gw mode", func() {
		It("resets the hybrid annotations on update", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations[hotypes.HybridOverlayVTEP] = "1.1.1.1"
				namespaceT.Annotations[hotypes.HybridOverlayExternalGw] = "2.2.2.2"
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.3",
					"myPod",
					"10.128.1.4",
					"0a:58:0a:80:01:04",
					namespaceT.Name,
				)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP),
						},
					},
				)
				podMAC := ovntest.MustParseMAC(tP.podMAC)
				fakeOvn.controller.logicalPortCache.add(tP.nodeName, tP.portName, fakeUUID, podMAC, []*net.IPNet{ovntest.MustParseIPNet(tP.nodeSubnet)})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				tP.addPodDenyMcast(fExec)
				tP.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				_, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				pod, err := fakeOvn.fakeClient.CoreV1().Pods(tP.namespace).Get(context.TODO(), tP.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(ContainSubstring(`"gateway_ip":"10.128.1.3"`))
				// Update namespace to remove annotation
				namespaceT.Annotations = nil
				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Update(context.TODO(), &namespaceT, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(func() map[string]string {
					updatedNs, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
					if err != nil {
						return map[string]string{"ns": "error"}
					}
					return updatedNs.Annotations
				}).Should(BeEmpty())
				time.Sleep(time.Second)
				// Create new pod
				tP = newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod2",
					"10.128.1.5",
					"0a:58:0a:80:01:05",
					namespaceT.Name,
				)
				tP.addPodDenyMcast(fExec)
				pod, err = fakeOvn.fakeClient.CoreV1().Pods(tP.namespace).Create(context.TODO(), newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP), metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				var annotation string
				Eventually(func() string {
					pod, err = fakeOvn.fakeClient.CoreV1().Pods(tP.namespace).Get(context.TODO(), tP.podName, metav1.GetOptions{})
					annotation = pod.Annotations[util.OvnPodAnnotationName]
					return pod.Annotations[util.OvnPodAnnotationName]
				}).ShouldNot(BeEmpty())
				Expect(annotation).To(ContainSubstring(`"gateway_ip":"10.128.1.1"`))
				return nil
			}

			err := app.Run([]string{
				app.Name,
				"-enable-hybrid-overlay",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("with routing external gws annotation", func() {
		It("adds LR routes on pod created in namespace with annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations[routingExternalGWsAnnotation] = "9.0.0.1,9.0.0.2"
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.3",
					"myPod",
					"10.128.1.4",
					"0a:58:0a:80:01:04",
					namespaceT.Name,
				)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP),
						},
					},
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				tP.addPodDenyMcast(fExec)
				tP.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp lr-route-add GR_node1 10.128.1.3/32 9.0.0.1",
					Output: "\n",
				})
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --may-exist --policy=src-ip --ecmp lr-route-add GR_node1 10.128.1.3/32 9.0.0.2",
					Output: "\n",
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{
				app.Name,
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
