package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
)

var _ = ginkgo.Describe("OVN for APB External Route Operations", func() {
	const (
		namespaceName = "namespace1"
		policyName    = "policy"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN

		bfd1NamedUUID     = "bfd-1-UUID"
		bfd2NamedUUID     = "bfd-2-UUID"
		logicalRouterPort = "rtoe-GR_node1"
	)

	getStaticPolicy := func(bfd bool) adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
		return newPolicy(
			policyName,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceName}},
			sets.NewString("9.0.0.1"),
			bfd, nil, nil, false, "")
	}

	getStaticPolicy2IPs := func(bfd bool, ipv6 bool) adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
		ips := sets.NewString("9.0.0.1", "9.0.0.2")
		if ipv6 {
			ips = sets.NewString("fd2e:6f44:5dd8::89", "fd2e:6f44:5dd8::76")
		}
		return newPolicy(
			policyName,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceName}},
			ips,
			bfd, nil, nil, false, "")
	}

	getDynamicPolicy := func(bfd bool, targetNamespace, targetPod string) adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
		return newPolicy(policyName,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceName}},
			nil,
			false,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": targetNamespace}},
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": targetPod}},
			bfd,
			"")
	}

	getStaticDynamicPolicy := func(bfdStatic, bfdDynamic bool, targetNamespace, targetPod string) adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
		return newPolicy(policyName,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceName}},
			sets.NewString("9.0.0.1"),
			bfdStatic,
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": targetNamespace}},
			&metav1.LabelSelector{MatchLabels: map[string]string{"name": targetPod}},
			bfdDynamic,
			"")
	}

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(true)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on setting namespace gateway static hop", func() {

		ginkgo.DescribeTable("reconciles an new pod with namespace single exgw static GW already set", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)

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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticPolicy(bfd),
						},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "9.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles an new pod with namespace single exgw static GW after policy is created", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)

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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticPolicy(bfd),
						},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()
				ginkgo.By("Waiting for the policy to be processed")
				gomega.Eventually(func() bool {
					p, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), policyName, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return !p.Status.LastTransitionTime.IsZero()
				}).Should(gomega.BeTrue())
				ginkgo.By("Adding the target pod")
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.Background(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("Validating the north bound DB has been updated with the new static route to the target pod")
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "9.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles an new pod with namespace single exgw static gateway already set with pod event first", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)

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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticPolicy(bfd),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), &namespaceT, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "9.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles an new pod with namespace double exgw static gateways already set", func(bfd bool, finalNB []libovsdbtest.TestData) {

			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)

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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticPolicy2IPs(bfd, false),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
			ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-2-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.2",
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
				},
			}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.BFD{
					UUID:        bfd2NamedUUID,
					DstIP:       "9.0.0.2",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-2-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.2",
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					BFD:        &bfd2NamedUUID,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
				},
			}),
		)

		ginkgo.DescribeTable("reconciles deleting a pod with namespace double exgw static gateway already set",
			func(bfd bool,
				initNB []libovsdbtest.TestData,
				syncNB []libovsdbtest.TestData,
				finalNB []libovsdbtest.TestData,
			) {
				app.Action = func(ctx *cli.Context) error {

					namespaceT := *newNamespace(namespaceName)

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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: initNB,
						},
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

					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.RunAPBExternalPolicyController()

					gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(syncNB))

					gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
					p := getStaticPolicy2IPs(bfd, false)
					_, err = fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Create(context.Background(), &p, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func() bool {
						p, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), policyName, metav1.GetOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						return !p.Status.LastTransitionTime.IsZero()
					}).Should(gomega.BeTrue())
					deletePod(t.namespace, t.podName, fakeOvn.fakeClient.KubeClient)

					gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
					checkAPBRouteStatus(fakeOvn, policyName, false)
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("No BFD", false,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.2",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			),
			ginkgo.Entry("BFD", true,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.BFD{
						UUID:        bfd1NamedUUID,
						DstIP:       "9.0.0.1",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.BFD{
						UUID:        bfd2NamedUUID,
						DstIP:       "9.0.0.2",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						BFD:        &bfd1NamedUUID,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.2",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						BFD:        &bfd2NamedUUID,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			),
		)

		ginkgo.DescribeTable("reconciles deleting a pod with namespace double exgw static gateway already set IPV6",
			func(bfd bool,
				initNB, syncNB, finalNB []libovsdbtest.TestData) {
				app.Action = func(ctx *cli.Context) error {
					namespaceT := *newNamespace(namespaceName)

					t := newTPod(
						"node1",
						"fd00:10:244:2::0/64",
						"fd00:10:244:2::2",
						"fd00:10:244:2::1",
						"myPod",
						"fd00:10:244:2::3",
						"0a:58:49:a1:93:cb",
						namespaceT.Name,
					)

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: initNB,
						},
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
					config.IPv6Mode = true
					t.populateLogicalSwitchCache(fakeOvn)
					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.RunAPBExternalPolicyController()

					gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(syncNB))

					gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
					p := getStaticPolicy2IPs(bfd, true)
					_, err = fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Create(context.Background(), &p, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func() bool {
						p, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), policyName, metav1.GetOptions{})
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						return !p.Status.LastTransitionTime.IsZero()
					}).Should(gomega.BeTrue())
					deletePod(t.namespace, t.podName, fakeOvn.fakeClient.KubeClient)
					gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
					checkAPBRouteStatus(fakeOvn, policyName, false)
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("BFD IPV6", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: "node1",
					Name: "node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "fd00:10:244:2::3/128",
					BFD:        &bfd1NamedUUID,
					OutputPort: &logicalRouterPort,
					Nexthop:    "fd2e:6f44:5dd8::89",
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-2-UUID",
					IPPrefix:   "fd00:10:244:2::3/128",
					BFD:        &bfd1NamedUUID,
					OutputPort: &logicalRouterPort,
					Nexthop:    "fd2e:6f44:5dd8::76",
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.BFD{
					UUID:        bfd2NamedUUID,
					DstIP:       "fd2e:6f44:5dd8::76",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "fd2e:6f44:5dd8::89",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
				},
			},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:49:a1:93:cb fd00:10:244:2::3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:49:a1:93:cb fd00:10:244:2::3"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouter{
						UUID: "GR_node1-UUID",
						Name: "GR_node1",
					},
				},
			),
		)

		ginkgo.DescribeTable("reconciles deleting a exgw namespace with active pod",
			func(bfd bool,
				initNB []libovsdbtest.TestData,
				finalNB []libovsdbtest.TestData,
			) {
				app.Action = func(ctx *cli.Context) error {

					namespaceT := *newNamespace(namespaceName)

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

					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: initNB,
						},
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
						&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
							Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
								getStaticPolicy2IPs(bfd, false),
							},
						},
					)
					t.populateLogicalSwitchCache(fakeOvn)

					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.RunAPBExternalPolicyController()

					gomega.Eventually(func() string {
						return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
					}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

					deleteNamespace(t.namespace, fakeOvn.fakeClient.KubeClient)
					gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
					checkAPBRouteStatus(fakeOvn, policyName, false)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("No BFD", false,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.2",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			),
			ginkgo.Entry("BFD", true,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.BFD{
						UUID:        "bfd1-UUID",
						DstIP:       "9.0.0.1",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.BFD{
						UUID:        "bfd2-UUID",
						DstIP:       "9.0.0.2",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						BFD:        &bfd1NamedUUID,
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						BFD:        &bfd2NamedUUID,
						Nexthop:    "9.0.0.2",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			))
	})

	ginkgo.Context("on setting pod dynamic gateways", func() {
		ginkgo.DescribeTable("reconciles a host networked pod acting as a exgw for another namespace for new pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)
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
				gwPod.Spec.HostNetwork = true

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getDynamicPolicy(bfd, namespaceX.Name, gwPod.Name),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()
				gomega.Eventually(func() bool {
					p, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), policyName, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return !p.Status.LastTransitionTime.IsZero()
				}).Should(gomega.BeTrue())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "9.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles a host networked pod acting as a exgw for another namespace for existing pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)
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
				gwPod.Spec.HostNetwork = true
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getDynamicPolicy(bfd, namespaceX.Name, gwPod.Name),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "9.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles a multus networked pod acting as a exgw for another namespace for new pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {
				ns := nettypes.NetworkStatus{Name: "dummy", IPs: []string{"11.0.0.1"}}
				networkStatuses := []nettypes.NetworkStatus{ns}
				nsEncoded, err := json.Marshal(networkStatuses)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				namespaceT := *newNamespace(namespaceName)
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
					"k8s.v1.cni.cncf.io/network-status": string(nsEncoded),
				}
				gwPod.Spec.HostNetwork = true
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							newPolicy(policyName,
								&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceName}},
								nil,
								bfd,
								&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceX.Name}},
								&metav1.LabelSelector{MatchLabels: map[string]string{"name": gwPod.Name}},
								bfd,
								"dummy"),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)
				injectNode(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()
				gomega.Eventually(func() bool {
					p, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), policyName, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return !p.Status.LastTransitionTime.IsZero()
				}).Should(gomega.BeTrue())
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, ginkgo.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": namespaceName,
				},
				Name: "namespace1_myPod",
				Options: map[string]string{
					"iface-id-ver":      "myPod",
					"requested-chassis": "node1",
				},
				PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
			},
			&nbdb.LogicalSwitch{
				UUID:  "node1",
				Name:  "node1",
				Ports: []string{"lsp1"},
			},
			&nbdb.LogicalRouterStaticRoute{
				UUID:       "static-route-1-UUID",
				IPPrefix:   "10.128.1.3/32",
				Nexthop:    "11.0.0.1",
				Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
				OutputPort: &logicalRouterPort,
				Options: map[string]string{
					"ecmp_symmetric_reply": "true",
				},
			},
			&nbdb.LogicalRouter{
				UUID:         "GR_node1-UUID",
				Name:         "GR_node1",
				StaticRoutes: []string{"static-route-1-UUID"},
			},
		}),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "11.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "11.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			}))

		ginkgo.DescribeTable("reconciles deleting a host networked pod acting as a exgw for another namespace for existing pod",
			func(bfd bool,
				beforeDeleteNB []libovsdbtest.TestData,
				afterDeleteNB []libovsdbtest.TestData) {
				app.Action = func(ctx *cli.Context) error {

					namespaceT := *newNamespace(namespaceName)
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
					gwPod.Spec.HostNetwork = true
					fakeOvn.startWithDBSetup(
						libovsdbtest.TestSetup{
							NBData: []libovsdbtest.TestData{
								&nbdb.LogicalSwitch{
									UUID: "node1",
									Name: "node1",
								},
								&nbdb.LogicalRouter{
									UUID: "GR_node1-UUID",
									Name: "GR_node1",
								},
							},
						},
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
						&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
							Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
								getDynamicPolicy(bfd, namespaceX.Name, gwPod.Name),
							},
						},
					)
					t.populateLogicalSwitchCache(fakeOvn)
					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					err = fakeOvn.controller.WatchPods()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.RunAPBExternalPolicyController()

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(beforeDeleteNB))

					deletePod(gwPod.Namespace, gwPod.Name, fakeOvn.fakeClient.KubeClient)

					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(afterDeleteNB))
					gomega.Eventually(func() string {
						return getNamespaceAnnotations(fakeOvn.fakeClient.KubeClient, namespaceT.Name)[util.ExternalGatewayPodIPsAnnotation]
					}, 5).Should(gomega.Equal(""))
					checkAPBRouteStatus(fakeOvn, policyName, false)
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			ginkgo.Entry("No BFD", false,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID"},
					},
				},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			),
			ginkgo.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": namespaceName,
					},
					Name: "namespace1_myPod",
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				},
				&nbdb.LogicalSwitch{
					UUID:  "node1",
					Name:  "node1",
					Ports: []string{"lsp1"},
				},
				&nbdb.BFD{
					UUID:        bfd1NamedUUID,
					DstIP:       "9.0.0.1",
					LogicalPort: "rtoe-GR_node1",
				},
				&nbdb.LogicalRouterStaticRoute{
					UUID:       "static-route-1-UUID",
					IPPrefix:   "10.128.1.3/32",
					Nexthop:    "9.0.0.1",
					BFD:        &bfd1NamedUUID,
					Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					OutputPort: &logicalRouterPort,
					Options: map[string]string{
						"ecmp_symmetric_reply": "true",
					},
				},
				&nbdb.LogicalRouter{
					UUID:         "GR_node1-UUID",
					Name:         "GR_node1",
					StaticRoutes: []string{"static-route-1-UUID"},
				},
			},
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				},
			),
		)
	})
	ginkgo.Context("on using bfd", func() {
		ginkgo.It("should enable bfd only on the namespace gw when set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)
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
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "10.0.0.1")
				gwPod.Spec.HostNetwork = true

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticDynamicPolicy(true, false, namespaceX.Name, gwPod.Name),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.BFD{
						UUID:        bfd1NamedUUID,
						DstIP:       "9.0.0.1",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						BFD:        &bfd1NamedUUID,
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "10.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should enable bfd only on the gw pod when set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace(namespaceName)
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
				gwPod := *newPod(namespaceX.Name, "gwPod", "node2", "10.0.0.1")
				gwPod.Spec.HostNetwork = true

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
							},
						},
					},
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
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticDynamicPolicy(false, true, namespaceX.Name, gwPod.Name),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.BFD{
						UUID:        bfd1NamedUUID,
						DstIP:       "10.0.0.1",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-2-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "10.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						BFD:        &bfd1NamedUUID,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID", "static-route-2-UUID"},
					},
				}

				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should disable bfd when removing the static hop from the namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace(namespaceName)

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
				initNB := libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{
							UUID: "node1",
							Name: "node1",
						},
						&nbdb.BFD{
							UUID:        bfd1NamedUUID,
							DstIP:       "9.0.0.1",
							LogicalPort: "rtoe-GR_node1",
						},
						&nbdb.LogicalRouterStaticRoute{
							UUID:       "static-route-1-UUID",
							IPPrefix:   "10.128.1.3/32",
							Nexthop:    "9.0.0.1",
							Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
							BFD:        &bfd1NamedUUID,
							OutputPort: &logicalRouterPort,
							Options: map[string]string{
								"ecmp_symmetric_reply": "true",
							},
						},
						&nbdb.LogicalRouter{
							UUID:         "GR_node1-UUID",
							Name:         "GR_node1",
							StaticRoutes: []string{"static-route-1-UUID"},
						},
					},
				}
				fakeOvn.startWithDBSetup(
					initNB,
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

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData([]libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{},
					},
				}))

				p := getStaticPolicy(true)
				_, err = fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Create(context.Background(), &p, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("Waiting for the policy to be processed")
				gomega.Eventually(func() bool {
					policy, err := fakeOvn.fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.Background(), p.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					return !policy.Status.LastTransitionTime.IsZero()
				}).Should(gomega.BeTrue())

				tempNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.BFD{
						UUID:        bfd1NamedUUID,
						DstIP:       "9.0.0.1",
						LogicalPort: "rtoe-GR_node1",
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						BFD:        &bfd1NamedUUID,
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
				}
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(tempNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)

				updatePolicy(policyName,
					&metav1.LabelSelector{MatchLabels: map[string]string{"name": namespaceT.Name}},
					sets.NewString("9.0.0.1"),
					false,
					nil,
					nil,
					false,
					"",
					fakeOvn.fakeClient.AdminPolicyRouteClient,
				)

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID"},
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": namespaceName,
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"iface-id-ver":      "myPod",
							"requested-chassis": "node1",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:       "static-route-1-UUID",
						IPPrefix:   "10.128.1.3/32",
						Nexthop:    "9.0.0.1",
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
						OutputPort: &logicalRouterPort,
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
					},
				}

				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("hybrid route policy operations in lgw mode", func() {
		ginkgo.It("add hybrid route policy for pods", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
							&nbdb.LogicalRouter{
								Name:  ovntypes.GWRouterPrefix + "node1",
								UUID:  ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
				)
				fakeOvn.RunAPBExternalPolicyController()

				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]",
						Priority: ovntypes.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.4"},
						Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]"},
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + "node1",
						UUID:  ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.apbExternalRouteController.AddHybridRoutePolicyForPod(net.ParseIP("10.128.1.3"), "node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				// check if the address-set was created with the podIP
				dbIDs := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				fakeOvn.asf.ExpectAddressSetWithAddresses(dbIDs, []string{"10.128.1.3"})
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should reconcile a pod and create/delete the hybridRoutePolicy accordingly", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal

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

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
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
					&v1.NodeList{
						Items: []v1.Node{
							*newNode("node1", "192.168.126.202/24"),
						},
					},
					&adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteList{
						Items: []adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
							getStaticPolicy(false),
						},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn)

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.RunAPBExternalPolicyController()

				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				nbWithLRP := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "lrp1",
						Action:   "reroute",
						Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
						Nexthops: []string{"100.64.0.4"},
						Priority: ovntypes.HybridOverlayReroutePriority,
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalRouterStaticRoute{
						UUID:     "static-route-1-UUID",
						IPPrefix: "10.128.1.3/32",
						Nexthop:  "9.0.0.1",
						Options: map[string]string{
							"ecmp_symmetric_reply": "true",
						},
						OutputPort: &logicalRouterPort,
						Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
					},
					&nbdb.LogicalSwitch{
						UUID:  "node1",
						Name:  "node1",
						Ports: []string{"lsp1"},
					},
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": "namespace1",
						},
						Name: "namespace1_myPod",
						Options: map[string]string{
							"requested-chassis": "node1",
							"iface-id-ver":      "myPod",
						},
						PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					},
					&nbdb.LogicalRouter{
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Name:     ovntypes.OVNClusterRouter,
						Policies: []string{"lrp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "GR_node1-UUID",
						Name:         "GR_node1",
						Ports:        []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
						StaticRoutes: []string{"static-route-1-UUID"},
					},
				}

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(nbWithLRP))
				checkAPBRouteStatus(fakeOvn, policyName, false)

				deletePod(t.namespace, t.podName, fakeOvn.fakeClient.KubeClient)

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
					&nbdb.LogicalRouter{
						UUID: ovntypes.OVNClusterRouter + "-UUID",
						Name: ovntypes.OVNClusterRouter,
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
				}
				gomega.Eventually(fakeOvn.nbClient, 5).Should(libovsdbtest.HaveData(finalNB))
				checkAPBRouteStatus(fakeOvn, policyName, false)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should create a single policy for concurrent addHybridRoutePolicy for the same node", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
							&nbdb.LogicalRouter{
								Name:  ovntypes.GWRouterPrefix + "node1",
								UUID:  ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouter{
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
				)

				fakeOvn.RunAPBExternalPolicyController()
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "lrp1",
						Priority: ovntypes.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.4"},
						Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"lrp1"},
					},
					&nbdb.LogicalRouter{
						Name:  ovntypes.GWRouterPrefix + "node1",
						UUID:  ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				wg := &sync.WaitGroup{}
				c := make(chan int)
				for i := 1; i <= 5; i++ {
					podIndex := i
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-c
						fakeOvn.controller.apbExternalRouteController.AddHybridRoutePolicyForPod(net.ParseIP(fmt.Sprintf("10.128.1.%d", podIndex)), "node1")
					}()
				}
				close(c)
				wg.Wait()
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

				err := fakeOvn.controller.apbExternalRouteController.AddHybridRoutePolicyForPod(net.ParseIP(fmt.Sprintf("10.128.1.%d", 6)), "node1")
				// adding another pod after the initial burst should not trigger an error or change db
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete hybrid route policy for pods", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				injectNode(fakeOvn)
				fakeOvn.RunAPBExternalPolicyController()
				err := fakeOvn.controller.apbExternalRouteController.DelHybridRoutePolicyForPod(
					net.ParseIP("10.128.1.3"), "node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				dbIDs := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete hybrid route policy for pods with force", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				asIndex1 := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				as1v4, _ := addressset.GetHashNamesForAS(asIndex1)
				asIndex2 := apbroute.GetHybridRouteAddrSetDbIDs("node2", DefaultNetworkControllerName)
				as2v4, _ := addressset.GetHashNamesForAS(asIndex2)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + as1v4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $" + as2v4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)

				fakeOvn.RunAPBExternalPolicyController()

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.apbExternalRouteController.DelAllHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				dbIDs := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete legacy hybrid route policies", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == 1.3.3.7 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == 1.3.3.8 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-new-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID", "501-new-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:  "GR_node1-UUID",
								Name:  "GR_node1",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
				)

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "501-new-UUID",
						Priority: ovntypes.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.1.4"},
						Match:    "inport == \"rtos-node2\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"501-new-UUID"},
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.apbExternalRouteController.DelAllLegacyHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete stale addresses from apb hybrid route policies on startup", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				asIndex := apbroute.GetHybridRouteAddrSetDbIDs("node1", DefaultNetworkControllerName)
				asv4, _ := addressset.GetHashNamesForAS(asIndex)

				node1 := tNode{
					Name:                 "node1",
					NodeIP:               "1.2.3.4",
					NodeLRPMAC:           "0a:58:0a:01:01:01",
					LrpIP:                "100.64.0.2",
					LrpIPv6:              "fd98::2",
					DrLrpIP:              "100.64.0.1",
					PhysicalBridgeMAC:    "11:22:33:44:55:66",
					SystemID:             "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
					NodeSubnet:           "10.1.1.0/24",
					GWRouter:             ovntypes.GWRouterPrefix + "node1",
					GatewayRouterIPMask:  "172.16.16.2/24",
					GatewayRouterIP:      "172.16.16.2",
					GatewayRouterNextHop: "172.16.16.1",
					PhysicalBridgeName:   "br-eth0",
					NodeGWIP:             "10.1.1.1/24",
					NodeMgmtPortIP:       "10.1.1.2",
					NodeMgmtPortMAC:      "0a:58:0a:01:01:02",
					DnatSnatIP:           "169.254.0.1",
				}
				// create a test node and annotate it with host subnet
				testNode := node1.k8sNode("2")

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterStaticRoute{
								UUID:     "static-route-1-UUID",
								IPPrefix: "10.128.1.3/32",
								Nexthop:  "9.0.0.1",
								Options: map[string]string{
									"ecmp_symmetric_reply": "true",
								},
								OutputPort: &logicalRouterPort,
								Policy:     &nbdb.LogicalRouterStaticRoutePolicySrcIP,
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-new-UUID",
								Priority: ovntypes.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $" + asv4 + " && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-new-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID:         "GR_node1-UUID",
								Name:         "GR_node1",
								Ports:        []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
								StaticRoutes: []string{"static-route-1-UUID"},
							},
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
								Networks: []string{"100.64.0.4/32"},
							},
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							testNode,
						},
					},
				)

				nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{KClient: fakeOvn.fakeClient.KubeClient},
					testNode.Name)

				vlanID := uint(1024)
				l3Config := node1.gatewayConfig(config.GatewayModeLocal, vlanID)
				err := util.SetL3GatewayConfig(nodeAnnotator, l3Config)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = nodeAnnotator.Run()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// add address set with one legit IP that exists in a ecmp route, and one that doesn't
				_, err = fakeOvn.asf.NewAddressSet(asIndex, []string{"10.128.1.3", "1.1.1.1"})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: ovntypes.OVNClusterRouter,
						UUID: ovntypes.OVNClusterRouter + "-UUID",
					},
					&nbdb.LogicalRouter{
						UUID:  "GR_node1-UUID",
						Name:  "GR_node1",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err = fakeOvn.controller.apbExternalRouteController.Repair()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(asIndex)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
	ginkgo.Context("SNAT on gateway router operations", func() {
		ginkgo.It("add/delete SNAT per pod on gateway router", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				config.Gateway.DisableSNATMultipleGWs = true

				nodeName := "node1"
				namespaceT := *newNamespace(namespaceName)
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

				pod := []v1.Pod{
					*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
				}

				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPort{
								UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
								Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
								Networks: []string{"100.64.0.4/32"},
							},
							&nbdb.LogicalRouter{
								Name:  types.GWRouterPrefix + nodeName,
								UUID:  types.GWRouterPrefix + nodeName + "-UUID",
								Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
							},
							&nbdb.LogicalSwitch{
								UUID: "node1",
								Name: "node1",
							},
						},
					},
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: pod,
					},
				)

				finalNB := []libovsdbtest.TestData{
					&nbdb.NAT{
						UUID:       "nat-UUID",
						ExternalIP: "169.254.33.2",
						LogicalIP:  "10.128.1.3",
						Options:    map[string]string{"stateless": "false"},
						Type:       nbdb.NATTypeSNAT,
					},
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + nodeName,
						UUID:  types.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
						Nat:   []string{"nat-UUID"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
				}
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.RunAPBExternalPolicyController()

				extIPs, err := getExternalIPsGR(fakeOvn.controller.watchFactory, pod[0].Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, fullMaskPodNet, _ := net.ParseCIDR("10.128.1.3/32")
				gomega.Expect(
					addOrUpdatePodSNAT(fakeOvn.controller.nbClient, util.GetGatewayRouterFromNode(pod[0].Spec.NodeName), extIPs, []*net.IPNet{fullMaskPodNet}),
				).To(gomega.Succeed())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				finalNB = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name:  types.GWRouterPrefix + nodeName,
						UUID:  types.GWRouterPrefix + nodeName + "-UUID",
						Ports: []string{ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID"},
						Nat:   []string{},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + nodeName,
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "node1",
						Name: "node1",
					},
				}
				err = fakeOvn.controller.apbExternalRouteController.DeletePodSNAT(nodeName, extIPs, []*net.IPNet{fullMaskPodNet})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

func newPolicy(policyName string, fromNSSelector *metav1.LabelSelector, staticHopsGWIPs sets.String, bfdStatic bool,
	dynamicHopsNSSelector *metav1.LabelSelector, dynamicHopsPodSelector *metav1.LabelSelector, bfdDynamic bool, networkAttachementName string) adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
	p := adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
		ObjectMeta: metav1.ObjectMeta{Name: policyName},
		Spec: adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{
			From: adminpolicybasedrouteapi.ExternalNetworkSource{
				NamespaceSelector: *fromNSSelector,
			},
			NextHops: adminpolicybasedrouteapi.ExternalNextHops{},
		},
	}

	if staticHopsGWIPs.Len() > 0 {
		p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{}
		for ip := range staticHopsGWIPs {
			p.Spec.NextHops.StaticHops = append(p.Spec.NextHops.StaticHops, &adminpolicybasedrouteapi.StaticHop{IP: ip, BFDEnabled: bfdStatic})
		}
	}
	if dynamicHopsNSSelector != nil && dynamicHopsPodSelector != nil {
		p.Spec.NextHops.DynamicHops = []*adminpolicybasedrouteapi.DynamicHop{
			{NamespaceSelector: *dynamicHopsNSSelector,
				PodSelector:           *dynamicHopsPodSelector,
				NetworkAttachmentName: networkAttachementName,
				BFDEnabled:            bfdDynamic},
		}
	}
	return p
}

func updatePolicy(policyName string, fromNSSelector *metav1.LabelSelector, staticHopsGWIPs sets.String, bfdStatic bool, dynamicHopsNSSelector *metav1.LabelSelector, dynamicHopsPodSelector *metav1.LabelSelector, bfdDynamic bool, networkAttachementName string, fakeRouteClient adminpolicybasedrouteclientset.Interface) {

	p, err := fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Get(context.TODO(), policyName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	p.Generation++
	p.Spec.From.NamespaceSelector = *fromNSSelector

	p.Spec.NextHops.StaticHops = []*adminpolicybasedrouteapi.StaticHop{}
	if staticHopsGWIPs.Len() > 0 {
		for ip := range staticHopsGWIPs {
			p.Spec.NextHops.StaticHops = append(p.Spec.NextHops.StaticHops, &adminpolicybasedrouteapi.StaticHop{IP: ip, BFDEnabled: bfdStatic})
		}
	}
	p.Spec.NextHops.DynamicHops = []*adminpolicybasedrouteapi.DynamicHop{}
	if dynamicHopsNSSelector != nil && dynamicHopsPodSelector != nil {
		p.Spec.NextHops.DynamicHops = append(p.Spec.NextHops.DynamicHops,
			&adminpolicybasedrouteapi.DynamicHop{
				NamespaceSelector:     *dynamicHopsNSSelector,
				PodSelector:           *dynamicHopsPodSelector,
				NetworkAttachmentName: networkAttachementName,
				BFDEnabled:            bfdDynamic},
		)
	}
	_, err = fakeRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().Update(context.Background(), p, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func deletePod(namespace, name string, fakeClient kubernetes.Interface) {

	p, err := fakeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	p.Generation++
	p.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	_, err = fakeClient.CoreV1().Pods(namespace).Update(context.Background(), p, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = fakeClient.CoreV1().Pods(namespace).Delete(context.Background(), p.Name, metav1.DeleteOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func deleteNamespace(namespaceName string, fakeClient kubernetes.Interface) {

	ns, err := fakeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ns.Generation++
	ns.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	_, err = fakeClient.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = fakeClient.CoreV1().Namespaces().Delete(context.Background(), namespaceName, metav1.DeleteOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

func (o *FakeOVN) RunAPBExternalPolicyController() {
	err := o.controller.apbExternalRouteController.Repair()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = o.controller.apbExternalRouteController.Run(o.controller.wg, 5)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
}

func checkAPBRouteStatus(fakeOVN *FakeOVN, policyName string, expectFailure bool) {
	var status *adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus
	var err error
	// first fetch status, if it is empty policy may not be handled yet
	gomega.Eventually(func() bool {
		status, err = fakeOVN.controller.apbExternalRouteController.GetAPBRoutePolicyStatus(policyName)
		if err != nil {
			return false
		}
		return len(status.Messages) == 1
	}).Should(gomega.BeTrue())

	// as soon as status is set, it should match expected status, without extra retries
	if expectFailure {
		gomega.Expect(status.Messages[0]).To(gomega.ContainSubstring(types.APBRouteErrorMsg))
	} else {
		gomega.Expect(status.Messages[0]).ToNot(gomega.ContainSubstring(types.APBRouteErrorMsg))
	}
}
