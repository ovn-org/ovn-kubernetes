package clustermanager

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressserviceapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var _ = ginkgo.Describe("Cluster manager egress service operations", func() {
	var (
		app    *cli.App
		fakeCM *FakeClusterManager
	)
	const (
		node1Name            string = "node1"
		node1IPv4            string = "100.100.100.0"
		node1IPv6            string = "fc00:f853:ccd:e793::1"
		node1IPv4Subnet      string = "10.128.1.0/24"
		node1IPv6Subnet      string = "fe00:10:128:1::/64"
		node2Name            string = "node2"
		node2IPv4            string = "200.200.200.0"
		node2IPv6            string = "fc00:f853:ccd:e793::2"
		node2IPv4Subnet      string = "10.128.2.0/24"
		node2IPv6Subnet      string = "fe00:10:128:2::/64"
		egressSVCLabelPrefix string = "egress-service.k8s.ovn.org"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		// disabling EgressIP to be sure we're creating the no reroute policies ourselves
		config.OVNKubernetesFeature.EnableEgressIP = false
		config.OVNKubernetesFeature.EnableEgressService = true
		_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
		_, cidr6, _ := net.ParseCIDR("fe00::/16")
		config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr4, 24}, {cidr6, 64}}

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeCM = NewFakeClusterManagerOVN()
	})

	ginkgo.AfterEach(func() {
		fakeCM.shutdown()
	})

	ginkgo.Context("on startup repair", func() {

		ginkgo.It("should delete stale labels from nodes", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet)

				node1.Labels = map[string]string{
					"unrelated-label": "",
					fmt.Sprintf("%s/deleted-service1", egressSVCLabelPrefix): "",
					fmt.Sprintf("%s/deleted-service2", egressSVCLabelPrefix): "",
					fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix):      "",
					fmt.Sprintf("%s/testns-svc2", egressSVCLabelPrefix):      "",
				}

				node2.Labels = map[string]string{
					"unrelated-label": "",
					fmt.Sprintf("%s/deleted-service3", egressSVCLabelPrefix): "",
					fmt.Sprintf("%s/deleted-service4", egressSVCLabelPrefix): "",
					fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix):      "",
					fmt.Sprintf("%s/testns-svc2", egressSVCLabelPrefix):      "",
				}

				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1Name,
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				esvc2 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node2Name,
					},
				}
				svc2 := lbSvcFor("testns", "svc2")

				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc2EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
							svc2,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
							svc2EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
							esvc2,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					expectedLabels := map[string]string{
						"unrelated-label": "",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, expectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, expectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					expectedLabels := map[string]string{
						"unrelated-label": "",
						fmt.Sprintf("%s/testns-svc2", egressSVCLabelPrefix): "",
					}

					node2, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, expectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, expectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should delete stale status from EgressServices", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)

				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Status: egressserviceapi.EgressServiceStatus{
						Host: node1Name,
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1's host value %s to be empty", es.Status.Host)
					}

					return nil
					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on services changes", func() {
		ginkgo.It("should create/update/delete EgressService host", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)
				node1.Labels = map[string]string{"firstName": "Albus"}
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet)
				node2.Labels = map[string]string{"firstName": "Severus"}

				ginkgo.By("creating a service that will be allocated on the first node")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"firstName": "Albus",
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")
				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc2EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
							svc2EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), svc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1.Name {
						return fmt.Errorf("expected svc1's host value %s to be node1", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("creating a second service without any EgressService")
				s2 := lbSvcFor("testns", "svc2")
				svc2 := &s2

				svc2, err := fakeCM.fakeClient.KubeClient.CoreV1().Services("testns").Create(context.TODO(), svc2, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				ginkgo.By("creating an EgressService for the second service with a config that matches the second node its status will be updated")
				esvc2 := &egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"firstName": "Severus",
							},
						},
					},
				}
				_, err = fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Create(context.TODO(), esvc2, metav1.CreateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc2.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node2.Name {
						return fmt.Errorf("expected svc2's host value %s to be node2", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("updating the second service's config to match the first node instead of the second its status will be updated")
				esvc2.Spec.NodeSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"firstName": "Albus",
					},
				}
				esvc2.ResourceVersion = "2"
				_, err = fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Update(context.TODO(), esvc2, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc2.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1.Name {
						return fmt.Errorf("expected svc2's host value %s to be node1", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should create/update/delete node labels", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)
				node1.Labels = map[string]string{"animal": "FlyingBison"}
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet)
				node2.Labels = map[string]string{"animal": "Lemur"}

				ginkgo.By("creating an egress service that will be allocated on the first node it will be labeled")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"animal": "FlyingBison",
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")
				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					node1ExpectedLabels := map[string]string{
						"animal": "FlyingBison",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					node2ExpectedLabels := map[string]string{
						"animal": "Lemur",
					}

					node2, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, node2ExpectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, node2ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("updating the service to be allocated on the second node its label will move to the second node")
				esvc1.Spec.NodeSelector = metav1.LabelSelector{
					MatchLabels: map[string]string{
						"animal": "Lemur",
					},
				}
				esvc1.ResourceVersion = "2"
				_, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Update(context.TODO(), &esvc1, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					node1ExpectedLabels := map[string]string{
						"animal": "FlyingBison",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					node2ExpectedLabels := map[string]string{
						"animal": "Lemur",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node2, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, node2ExpectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, node2ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("deleting the EgressService both nodes will not have the label")
				err = fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Delete(context.TODO(), esvc1.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					node1ExpectedLabels := map[string]string{
						"animal": "FlyingBison",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					node2ExpectedLabels := map[string]string{
						"animal": "Lemur",
					}

					node2, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, node2ExpectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, node2ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should do nothing when an invalid nodeSelector is specified", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)

				// ":", "&" not allowed in labels
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"a:b": "c&",
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")
				svc1EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Consistently(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), svc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1 to not have a host, got a value of %v", es.Status.Host)
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					_, ok := node1.Labels[fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix)]

					if ok {
						return fmt.Errorf("expected node1 to not have the egress service label, got %v", node1.Labels)
					}

					return nil
				}, 1*time.Second).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on endpointslices changes", func() {

		ginkgo.It("should create/update/delete status for ETP=Local Service", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)
				node1.Labels["square"] = "pants"
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet)

				ginkgo.By("creating a service with a selector matching a node without local eps lrps should not be created")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"square": "pants",
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")
				svc1.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

				v4EpSlice := &discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
						ResourceVersion: "1",
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							*v4EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				fakeCM.start(objs...)

				// node1 should not be selected since it doesn't host an endpoint
				gomega.Consistently(func() error {
					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname": "node1",
						"square":                 "pants",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1 host value %s to be empty", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("updating the endpointslice to have an endpoint on node1 corresponding status will be created")
				v4EpSlice.Endpoints = append(v4EpSlice.Endpoints, discovery.Endpoint{
					Addresses: []string{"10.128.1.5"},
					NodeName:  &node1.Name,
				})
				v4EpSlice, err := fakeCM.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Update(context.TODO(), v4EpSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname": "node1",
						"square":                 "pants",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1Name {
						return fmt.Errorf("expected svc1 host value %s to be node1", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("removing node1's local ep should delete the status from node1")
				v4EpSlice.Endpoints = append(v4EpSlice.Endpoints, discovery.Endpoint{Addresses: []string{"10.128.1.7"}})
				v4EpSlice.ResourceVersion = "2"
				v4EpSlice, err = fakeCM.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Update(context.TODO(), v4EpSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				v4EpSlice.Endpoints = []discovery.Endpoint{
					{
						Addresses: []string{"10.128.2.5"},
						NodeName:  &node2.Name,
					},
				}
				v4EpSlice.ResourceVersion = "3"
				_, err = fakeCM.fakeClient.KubeClient.DiscoveryV1().EndpointSlices("testns").Update(context.TODO(), v4EpSlice, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname": "node1",
						"square":                 "pants",
					}

					node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1 host value %s to be empty", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on nodes changes", func() {
		ginkgo.It("should create/update/delete labels and status", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)
				node1.Labels = map[string]string{"home": "pineapple"}
				node2 := nodeFor(node2Name, node2IPv4, node2IPv6, node2IPv4Subnet, node2IPv6Subnet)
				node2.Labels = map[string]string{"home": "rock"}

				ginkgo.By("creating two services with different selectors")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"home": "pineapple",
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				esvc2 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"home": "moai",
							},
						},
					},
				}
				svc2 := lbSvcFor("testns", "svc2")

				svc1V4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc1V6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc2V4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.2.5"},
							NodeName:  &node2.Name,
						},
					},
				}

				svc2V6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc2-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc2",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:2::5"},
							NodeName:  &node2.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
							*node2,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
							svc2,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1V4EpSlice,
							svc1V6EpSlice,
							svc2V4EpSlice,
							svc2V6EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
							esvc2,
						},
					},
				}

				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1.Name {
						return fmt.Errorf("expected svc1's host value %s to be node1", es.Status.Host)
					}

					node1ExpectedLabels := map[string]string{
						"home": "pineapple",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					es, err = fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc2.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc2's host value %s to be empty", es.Status.Host)
					}

					node2ExpectedLabels := map[string]string{
						"home": "rock",
					}

					node2, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, node2ExpectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, node2ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("updating the first node's to be not ready the resources of first service will be deleted")
				node1.Status.Conditions = []v1.NodeCondition{
					{
						Type:   v1.NodeReady,
						Status: v1.ConditionFalse,
					},
				}
				node1.ResourceVersion = "2"
				node1, err := fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node1, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				ginkgo.By("updating the second node's labels to match the second service will allocate it")
				node2.Labels["home"] = "moai"
				node2.ResourceVersion = "2"
				node2, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node2, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1's host value %s to be empty", es.Status.Host)
					}

					node1ExpectedLabels := map[string]string{
						"home": "pineapple",
					}

					node1, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					es, err = fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc2.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node2.Name {
						return fmt.Errorf("expected svc1's host value %s to be node2", es.Status.Host)
					}

					node2ExpectedLabels := map[string]string{
						"home": "moai",
						fmt.Sprintf("%s/testns-svc2", egressSVCLabelPrefix): "",
					}

					node2, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node2Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node2.Labels, node2ExpectedLabels) {
						return fmt.Errorf("expected node2's labels %v to be equal %v", node2.Labels, node2ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("deleting the second node the second service's status will be deleted")
				err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), node2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), esvc2.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc2's host value %s to be empty", es.Status.Host)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		ginkgo.It("should update labels and status on reachability failure", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("testns")
				config.IPv6Mode = true
				node1 := nodeFor(node1Name, node1IPv4, node1IPv6, node1IPv4Subnet, node1IPv6Subnet)

				ginkgo.By("creating a service selecting the node")
				esvc1 := egressserviceapi.EgressService{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1",
						Namespace: "testns",
					},
					Spec: egressserviceapi.EgressServiceSpec{
						NodeSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/hostname": node1.Name,
							},
						},
					},
				}
				svc1 := lbSvcFor("testns", "svc1")

				svc1V4EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv4-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv4,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"10.128.1.5"},
							NodeName:  &node1.Name,
						},
					},
				}

				svc1V6EpSlice := discovery.EndpointSlice{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "svc1-ipv6-epslice",
						Namespace: "testns",
						Labels: map[string]string{
							discovery.LabelServiceName: "svc1",
						},
					},
					AddressType: discovery.AddressTypeIPv6,
					Endpoints: []discovery.Endpoint{
						{
							Addresses: []string{"fe00:10:128:1::5"},
							NodeName:  &node1.Name,
						},
					},
				}

				objs := []runtime.Object{
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							*node1,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							svc1,
						},
					},
					&discovery.EndpointSliceList{
						Items: []discovery.EndpointSlice{
							svc1V4EpSlice,
							svc1V6EpSlice,
						},
					},
					&egressserviceapi.EgressServiceList{
						Items: []egressserviceapi.EgressService{
							esvc1,
						},
					},
				}

				ginkgo.By("modifying the controller's IsReachable func to return false on the first call and true for the second")
				count := 0

				isReachable = func(nodeName string, _ []net.IP, _ healthcheck.EgressIPHealthClient) bool {
					count++
					return count == 2
				}
				fakeCM.start(objs...)

				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), svc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1.Name {
						return fmt.Errorf("expected svc1's host value %s to be node1", es.Status.Host)
					}

					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname":                            "node1",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("calling the reachability check which will return that the node is unreachable it will be drained")
				fakeCM.esvc.CheckNodesReachabilityIterate()
				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), svc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != "" {
						return fmt.Errorf("expected svc1's host value %s to be empty", es.Status.Host)
					}

					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname": "node1",
					}

					node1, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				ginkgo.By("calling the reachability check which will return that the node is reachable the service will be reallocated")
				fakeCM.esvc.CheckNodesReachabilityIterate()
				gomega.Eventually(func() error {
					es, err := fakeCM.fakeClient.EgressServiceClient.K8sV1().EgressServices("testns").Get(context.TODO(), svc1.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if es.Status.Host != node1.Name {
						return fmt.Errorf("expected svc1's host value %s to be node1", es.Status.Host)
					}

					node1ExpectedLabels := map[string]string{
						"kubernetes.io/hostname":                            "node1",
						fmt.Sprintf("%s/testns-svc1", egressSVCLabelPrefix): "",
					}

					node1, err = fakeCM.fakeClient.KubeClient.CoreV1().Nodes().Get(context.TODO(), node1Name, metav1.GetOptions{})
					if err != nil {
						return err
					}

					if !reflect.DeepEqual(node1.Labels, node1ExpectedLabels) {
						return fmt.Errorf("expected node1's labels %v to be equal %v", node1.Labels, node1ExpectedLabels)
					}

					return nil
				}).ShouldNot(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

})

func lbSvcFor(namespace, name string) v1.Service {
	return v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: "1",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
		},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{
						IP: "1.1.1.1", // arbitrary ip, we don't care about it for the lrps as long as it's there
					},
				},
			},
		},
	}
}

func nodeFor(name, ipv4, ipv6, v4subnet, v6subnet string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"k8s.ovn.org/node-primary-ifaddr": fmt.Sprintf("{\"ipv4\": \"%s\", \"ipv6\": \"%s\"}", ipv4, ipv6),
				"k8s.ovn.org/node-subnets":        fmt.Sprintf("{\"default\":[\"%s\",\"%s\"]}", v4subnet, v6subnet),
			},
			Labels: map[string]string{
				"kubernetes.io/hostname": name,
			},
			ResourceVersion: "1",
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: ipv4,
				},
				{
					Type:    v1.NodeInternalIP,
					Address: ipv6,
				},
			},
		},
	}
}
