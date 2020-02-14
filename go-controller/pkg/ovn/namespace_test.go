package ovn

import (
	"fmt"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

type namespace struct{}

func newNamespaceMeta(namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:  types.UID(namespace),
		Name: namespace,
		Labels: map[string]string{
			"name": namespace,
		},
		Annotations: map[string]string{},
	}
}

func newNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

func (n namespace) baseCmds(fexec *ovntest.FakeExec, namespaces ...v1.Namespace) {
	namespacesRes := ""
	for _, n := range namespaces {
		namespacesRes += fmt.Sprintf("name=%s\n", n.Name)
	}
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_ids find address_set",
		Output: namespacesRes,
	})
}

func (n namespace) addCmds(fexec *ovntest.FakeExec, namespaces ...v1.Namespace) {
	for _, n := range namespaces {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=" + hashedAddressSet(n.Name),
			Output: fmt.Sprintf("name=%s\n", n.Name),
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedAddressSet(n.Name)),
		})
	}
}

func (n namespace) delCmds(fexec *ovntest.FakeExec, namespace v1.Namespace) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists destroy address_set %s", hashedAddressSet(namespace.Name)),
	})
}

func (n namespace) addPodCmds(fexec *ovntest.FakeExec, tP pod, namespace v1.Namespace) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf(`ovn-nbctl --timeout=15 set address_set %s addresses="%s"`, hashedAddressSet(namespace.Name), tP.podIP),
	})
}

func (n namespace) delPodCmds(fexec *ovntest.FakeExec, tP pod, namespace v1.Namespace) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 clear address_set %s addresses", hashedAddressSet(namespace.Name)),
	})
}

func (n namespace) addCmdsWithPods(fexec *ovntest.FakeExec, tP pod, namespace v1.Namespace) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find address_set name=" + hashedAddressSet(namespace.Name),
		Output: fmt.Sprintf("name=%s\n", namespace.Name),
	})
	n.addPodCmds(fexec, tP, namespace)
}

var _ = Describe("OVN Namespace Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		fExec   *ovntest.FakeExec
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewFakeExec()
		fakeOvn = NewFakeOVN(fExec, true)
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on startup", func() {

		It("reconciles an existing namespace with pods", func() {
			app.Action = func(ctx *cli.Context) error {

				test := namespace{}
				namespaceT := *newNamespace("namespace1")
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.4",
					"11:22:33:44:55:66",
					namespaceT.Name,
				)

				test.baseCmds(fExec, namespaceT)
				test.addCmdsWithPods(fExec, tP, namespaceT)

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
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles an existing namespace without pods", func() {
			app.Action = func(ctx *cli.Context) error {

				test := namespace{}
				namespaceT := *newNamespace("namespace1")

				test.baseCmds(fExec, namespaceT)
				test.addCmds(fExec, namespaceT)

				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				})
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})

	Context("during execution", func() {

		It("reconciles a deleted namespace without pods", func() {
			app.Action = func(ctx *cli.Context) error {

				test := namespace{}
				namespaceT := *newNamespace("namespace1")

				test.baseCmds(fExec, namespaceT)
				test.addCmds(fExec, namespaceT)

				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						namespaceT,
					},
				})
				fakeOvn.controller.WatchNamespaces()

				namespace, err := fakeOvn.fakeClient.CoreV1().Namespaces().Get(namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(namespace).NotTo(BeNil())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				test.delCmds(fExec, namespaceT)

				err = fakeOvn.fakeClient.CoreV1().Namespaces().Delete(namespaceT.Name, metav1.NewDeleteOptions(1))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})
})
