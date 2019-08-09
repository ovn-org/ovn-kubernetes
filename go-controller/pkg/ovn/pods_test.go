package ovn

import (
	"fmt"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newObjectMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(namespace, name, node string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: newObjectMeta(namespace, name),
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
		},
	}
}

func newNamespace(name string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newObjectMeta(name, name),
		Status: v1.NamespaceStatus{
			Phase: v1.NamespaceActive,
		},
	}
}

func portName(namespace, name string) string {
	return namespace + "_" + name
}

var _ = Describe("OVN Pod Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	Context("on startup", func() {
		const (
			nodeName   string = "node1"
			nodeSubnet string = "10.128.1.0/24"
			nodeMgtIP  string = "10.128.1.2"
			nodeGWIP   string = "10.128.1.1"
			nodeGWCIDR string = nodeGWIP + "/24"
			podName    string = "myPod"
			podIP      string = "10.128.1.4"
			podMAC     string = "11:22:33:44:55:66"
			namespace  string = "namespace"
		)

		It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {
				portName := portName(namespace, podName)

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: portName + "\n",
				})

				// node setup
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + nodeName + " other-config",
					Output: `{exclude_ips="10.128.1.2", subnet="` + nodeSubnet + `"}`,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + nodeName + " other-config:subnet",
					Output: fmt.Sprintf("%q", nodeSubnet),
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --may-exist acl-add " + nodeName + " to-lport 1001 ip4.src==" + nodeMgtIP + " allow-related",
				})
				// pod setup
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --wait=sb -- --may-exist lsp-add " + nodeName + " " + portName + " -- lsp-set-addresses " + portName + " dynamic -- set logical_switch_port " + portName + " external-ids:namespace=" + namespace + " external-ids:logical_switch=" + nodeName + " external-ids:pod=true",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch " + nodeName + " external_ids:gateway_ip",
					Output: fmt.Sprintf("%q", nodeGWCIDR),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + portName + " dynamic_addresses",
					Output: `"` + podMAC + " " + podIP + `"`,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 lsp-set-port-security " + portName + " " + podMAC + " " + podIP + "/24",
				})

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeClient := fake.NewSimpleClientset(&v1.PodList{
					Items: []v1.Pod{*newPod(namespace, podName, nodeName)},
				})

				stopChan := make(chan struct{})
				f, err := factory.NewWatchFactory(fakeClient, stopChan)
				Expect(err).NotTo(HaveOccurred())
				defer f.Shutdown()

				oc := NewOvnController(fakeClient, f)
				oc.portGroupSupport = true

				oc.WatchPods()

				Expect(fexec.CalledMatchesExpected()).To(BeTrue())

				node, err := fakeClient.CoreV1().Pods(namespace).Get(podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				podAnnotation, ok := node.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + podIP + `/24", "mac_address":"` + podMAC + `", "gateway_ip": "` + nodeGWIP + `"}`))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {
				portName := portName(namespace, podName)

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: portName + "\n",
				})

				// pod's logical switch port is removed
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --if-exists lsp-del " + portName,
				})

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeClient := fake.NewSimpleClientset()

				stopChan := make(chan struct{})
				f, err := factory.NewWatchFactory(fakeClient, stopChan)
				Expect(err).NotTo(HaveOccurred())
				defer f.Shutdown()

				oc := NewOvnController(fakeClient, f)
				oc.portGroupSupport = true

				oc.WatchPods()

				Expect(fexec.CalledMatchesExpected()).To(BeTrue())
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {
				portName := portName(namespace, podName)

				fexec := ovntest.NewFakeExec()
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_switch_port external_ids:pod=true",
					Output: "\n",
				})

				// node setup
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + nodeName + " other-config",
					Output: `{exclude_ips="10.128.1.2", subnet="` + nodeSubnet + `"}`,
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch " + nodeName + " other-config:subnet",
					Output: fmt.Sprintf("%q", nodeSubnet),
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --may-exist acl-add " + nodeName + " to-lport 1001 ip4.src==" + nodeMgtIP + " allow-related",
				})
				// pod setup
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 --wait=sb -- --may-exist lsp-add " + nodeName + " " + portName + " -- lsp-set-addresses " + portName + " dynamic -- set logical_switch_port " + portName + " external-ids:namespace=" + namespace + " external-ids:logical_switch=" + nodeName + " external-ids:pod=true",
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --if-exists get logical_switch " + nodeName + " external_ids:gateway_ip",
					Output: fmt.Sprintf("%q", nodeGWCIDR),
				})
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 get logical_switch_port " + portName + " dynamic_addresses",
					Output: `"` + podMAC + " " + podIP + `"`,
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					"ovn-nbctl --timeout=15 lsp-set-port-security " + portName + " " + podMAC + " " + podIP + "/24",
				})

				err := util.SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeClient := fake.NewSimpleClientset(&v1.PodList{
					Items: []v1.Pod{*newPod(namespace, podName, nodeName)},
				})

				stopChan := make(chan struct{})
				f, err := factory.NewWatchFactory(fakeClient, stopChan)
				Expect(err).NotTo(HaveOccurred())
				defer f.Shutdown()

				oc := NewOvnController(fakeClient, f)
				oc.portGroupSupport = true

				oc.WatchPods()

				Expect(fexec.CalledMatchesExpected()).To(BeTrue())

				node, err := fakeClient.CoreV1().Pods(namespace).Get(podName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				podAnnotation, ok := node.Annotations["ovn"]
				Expect(ok).To(BeTrue())
				Expect(podAnnotation).To(MatchJSON(`{"ip_address":"` + podIP + `/24", "mac_address":"` + podMAC + `", "gateway_ip": "` + nodeGWIP + `"}`))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
