package ovn

import (
	"context"
	"net"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func newNamespaceMeta(namespace string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": namespace,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		UID:         types.UID(namespace),
		Name:        namespace,
		Labels:      labels,
		Annotations: map[string]string{},
	}
}

func newNamespaceWithLabels(namespace string, additionalLabels map[string]string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, additionalLabels),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

func newNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: newNamespaceMeta(namespace, nil),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}

var _ = Describe("OVN Namespace Operations", func() {
	const (
		namespaceName    = "namespace1"
		v4AddressSetName = namespaceName + ipv4AddressSetSuffix
		v6AddressSetName = namespaceName + ipv6AddressSetSuffix
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(ovntest.NewFakeExec())
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on startup", func() {

		It("reconciles an existing namespace with pods", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace(namespaceName)
				tP := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"11:22:33:44:55:66",
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
				podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
				fakeOvn.controller.logicalPortCache.add(tP.nodeName, tP.portName, fakeUUID, podMAC, podIPNets)
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.asf.ExpectAddressSetWithIPs(v4AddressSetName, []string{tP.podIP})
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("creates an empty address set for the namespace without pods", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace("namespace1"),
					},
				})
				fakeOvn.controller.WatchNamespaces()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.asf.ExpectEmptyAddressSet(v4AddressSetName)
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("during execution", func() {
		It("deletes an empty namespace's resources", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvn.start(ctx, &v1.NamespaceList{
					Items: []v1.Namespace{
						*newNamespace(namespaceName),
					},
				})
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.asf.ExpectEmptyAddressSet(v4AddressSetName)
				fakeOvn.asf.ExpectNoAddressSet(v6AddressSetName)

				err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, *metav1.NewDeleteOptions(1))
				Expect(err).NotTo(HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(v4AddressSetName)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
