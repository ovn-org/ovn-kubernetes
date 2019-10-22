package ovn

import (
	"fmt"
	"net"

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
	fakeOvn.controller.lsMutex.Lock()
	defer fakeOvn.controller.lsMutex.Unlock()
	fakeOvn.controller.logicalSwitchCache[p.nodeName] = []*net.IPNet{ovntest.MustParseIPNet(p.nodeSubnet)}
}

func (p pod) addCmds(fexec *ovntest.FakeExec, fail bool) {
	// pod setup
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --may-exist lsp-add " + p.nodeName + " " + p.portName + " -- lsp-set-addresses " + p.portName + " dynamic -- set logical_switch_port " + p.portName + " external-ids:namespace=" + p.namespace + " external-ids:pod=true",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + p.portName + " dynamic_addresses addresses",
		Output: `"` + p.podMAC + " " + p.podIP + `"` + "\n" + "[]",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + p.portName + " _uuid",
		Output: fakeUUID + "\n",
	})
	if fail {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP,
			Err: fmt.Errorf("adsfadsfasfdasfd"),
		})
	} else {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP,
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID + " -- add port_group mcastPortGroupDeny ports " + fakeUUID,
		})
	}
}

func (p pod) addCmdsForNonExistingPod(fexec *ovntest.FakeExec) {
	p.addCmds(fexec, false)
}

func (p pod) addCmdsForExistingPod(fexec *ovntest.FakeExec, addPort bool) {
	// pod setup
	var addPortCmd string
	if addPort {
		addPortCmd = fmt.Sprintf("lsp-add %s %s ", p.nodeName, p.portName)
	}
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 %s-- lsp-set-addresses %s %s %s -- --if-exists clear logical_switch_port %s dynamic_addresses -- set logical_switch_port %s external-ids:namespace=namespace external-ids:pod=true", addPortCmd, p.portName, p.podMAC, p.podIP, p.portName, p.portName),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + p.portName + " _uuid",
		Output: fakeUUID + "\n",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 lsp-set-port-security " + p.portName + " " + p.podMAC + " " + p.podIP,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID + " -- add port_group mcastPortGroupDeny ports " + fakeUUID,
	})
}

func (p pod) addCmdsForNonExistingFailedPod(fexec *ovntest.FakeExec) {
	p.addCmds(fexec, true)
}

func (p pod) delCmds(fexec *ovntest.FakeExec) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove port_group mcastPortGroupDeny ports " + fakeUUID,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists lsp-del " + p.portName,
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

				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.portName = t.namespace + "_" + t.podName
				t.populateLogicalSwitchCache(fakeOvn)
				t.addCmdsForNonExistingPod(fExec)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Update(newPod(t.namespace, t.podName, t.nodeName, t.podIP))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				t.baseCmds(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(pod).To(BeNil())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				t.addCmdsForNonExistingPod(fExec)

				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Create(newPod(t.namespace, t.podName, t.nodeName, t.podIP))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				// Setup an assigned pod
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addCmdsForNonExistingPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				// Delete it
				t.delCmds(fExec)

				err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Delete(t.podName, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(pod).To(BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("retries a failed pod Add on Update", func() {
			app.Action = func(ctx *cli.Context) error {

				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addCmdsForNonExistingFailedPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				// Pod creation should be retried on Update event
				t.addCmdsForNonExistingPod(fExec)
				_, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Update(newPod(t.namespace, t.podName, t.nodeName, t.podIP))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on startup", func() {

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists lsp-del " + t.portName,
				})
				t.addCmdsForNonExistingPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeFalse())

				fakeOvn.controller.WatchPods()

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: t.portName + "\n",
				})
				fExec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists lsp-del " + t.portName,
				})

				fakeOvn.start(ctx)
				fakeOvn.controller.WatchPods()
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addCmdsForNonExistingPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on ovn restart", func() {

		It("reconciles an existing pod without an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addCmdsForNonExistingPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				// Simulate an OVN restart with a new IP assignment and verify that the pod annotation is updated.
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch_port namespace_myPod _uuid",
					Output: "\n",
				})
				t.addCmdsForExistingPod(fExec, true)

				fakeOvn.restart()
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				// Check that pod annotations have been re-written to correct values
				podAnnotation, ok = pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing pod with an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {

				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					"namespace",
				)

				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.addCmdsForNonExistingPod(fExec)

				fakeOvn.start(ctx, &v1.PodList{
					Items: []v1.Pod{
						*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
					},
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				podAnnotation, ok := pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				// Simulate an OVN restart with a new IP assignment and verify that the pod annotation is updated.
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})
				t.populateLogicalSwitchCache(fakeOvn)
				fExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch_port namespace_myPod _uuid",
					Output: fakeUUID + "\n",
				})
				t.addCmdsForExistingPod(fExec, false)

				fakeOvn.restart()
				t.populateLogicalSwitchCache(fakeOvn)
				fakeOvn.controller.WatchPods()

				pod, err = fakeOvn.fakeClient.CoreV1().Pods(t.namespace).Get(t.podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				// Check that pod annotations have been re-written to correct values
				podAnnotation, ok = pod.Annotations[util.OvnPodAnnotationName]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
