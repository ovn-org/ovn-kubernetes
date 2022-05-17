package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("OVN Egress Gateway Operations", func() {
	const (
		namespaceName = "namespace1"
	)
	var (
		app     *cli.App
		fakeOvn *FakeOVN

		bfd1NamedUUID     = "bfd-1-UUID"
		bfd2NamedUUID     = "bfd-2-UUID"
		logicalRouterPort = "rtoe-GR_node1"
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN()
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on setting namespace gateway annotations", func() {

		table.DescribeTable("reconciles an new pod with namespace single exgw annotation already set", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				if bfd {
					namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
				}
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
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, table.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles an new pod with namespace single exgw annotation already set with pod event first", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				if bfd {
					namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
				}
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
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Create(context.TODO(), &namespaceT, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, table.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles an new pod with namespace double exgw annotation already set", func(bfd bool, finalNB []libovsdbtest.TestData) {

			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
				if bfd {
					namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
				}
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
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
			table.Entry("No BFD", false, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles deleting a pod with namespace double exgw annotation already set",
			func(bfd bool,
				initNB []libovsdbtest.TestData,
				finalNB []libovsdbtest.TestData,
			) {
				app.Action = func(ctx *cli.Context) error {

					namespaceT := *newNamespace("namespace1")
					namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
					if bfd {
						namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
					}
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
					t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()

					gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			table.Entry("No BFD", false,
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
			table.Entry("BFD", true,
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

		table.DescribeTable("reconciles deleting a pod with namespace double exgw annotation already set IPV6",
			func(bfd bool,
				initNB []libovsdbtest.TestData,
				finalNB []libovsdbtest.TestData) {
				app.Action = func(ctx *cli.Context) error {
					namespaceT := *newNamespace("namespace1")
					namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "fd2e:6f44:5dd8::89,fd2e:6f44:5dd8::76"}
					if bfd {
						namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
					}
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
					t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()

					gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/64"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/64", "gateway_ip": "` + t.nodeGWIP + `"}}`))

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB...))
					return nil
				}
				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			table.Entry("BFD IPV6", true, []libovsdbtest.TestData{
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

		table.DescribeTable("reconciles deleting a exgw namespace with active pod",
			func(bfd bool,
				initNB []libovsdbtest.TestData,
				finalNB []libovsdbtest.TestData,
			) {
				app.Action = func(ctx *cli.Context) error {

					namespaceT := *newNamespace("namespace1")
					namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1,9.0.0.2"}
					if bfd {
						namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
					}
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
					t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()

					gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Delete(context.TODO(), t.namespace, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			table.Entry("No BFD", false,
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
							"namespace": "namespace1",
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
			table.Entry("BFD", true,
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
							"namespace": "namespace1",
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

	ginkgo.Context("on setting pod gateway annotations", func() {
		table.DescribeTable("reconciles a host networked pod acting as a exgw for another namespace for new pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
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
				if bfd {
					gwPod.Annotations["k8s.ovn.org/bfd-enabled"] = ""
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
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, table.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles a host networked pod acting as a exgw for another namespace for existing pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
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
				if bfd {
					gwPod.Annotations["k8s.ovn.org/bfd-enabled"] = ""
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
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, table.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles a multus networked pod acting as a exgw for another namespace for new pod", func(bfd bool, finalNB []libovsdbtest.TestData) {
			app.Action = func(ctx *cli.Context) error {
				ns := nettypes.NetworkStatus{Name: "dummy", IPs: []string{"11.0.0.1"}}
				networkStatuses := []nettypes.NetworkStatus{ns}
				nsEncoded, err := json.Marshal(networkStatuses)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
				if bfd {
					gwPod.Annotations["k8s.ovn.org/bfd-enabled"] = ""
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
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				injectNode(fakeOvn)
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}, table.Entry("No BFD", false, []libovsdbtest.TestData{
			&nbdb.LogicalSwitchPort{
				UUID:      "lsp1",
				Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				ExternalIDs: map[string]string{
					"pod":       "true",
					"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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

		table.DescribeTable("reconciles deleting a host networked pod acting as a exgw for another namespace for existing pod",
			func(bfd bool,
				beforeDeleteNB []libovsdbtest.TestData,
				afterDeleteNB []libovsdbtest.TestData) {
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
					if bfd {
						gwPod.Annotations["k8s.ovn.org/bfd-enabled"] = ""
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
								*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
							},
						},
					)
					t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
					injectNode(fakeOvn)
					err := fakeOvn.controller.WatchNamespaces()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					fakeOvn.controller.WatchPods()

					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(beforeDeleteNB))

					err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Delete(context.TODO(), gwPod.Name, *metav1.NewDeleteOptions(0))
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(afterDeleteNB))
					return nil
				}

				err := app.Run([]string{app.Name})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
			},
			table.Entry("No BFD", false,
				[]libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": "namespace1",
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
							"namespace": "namespace1",
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
			table.Entry("BFD Enabled", true, []libovsdbtest.TestData{
				&nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": "namespace1",
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
							"namespace": "namespace1",
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

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""
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
				gwPod.Annotations = map[string]string{"k8s.ovn.org/routing-namespaces": namespaceT.Name}
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
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": "namespace1",
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
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should enable bfd only on the gw pod when set", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
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
				gwPod.Annotations = map[string]string{"k8s.ovn.org/routing-namespaces": namespaceT.Name}
				gwPod.Annotations["k8s.ovn.org/bfd-enabled"] = ""

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
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespaceX.Name).Create(context.TODO(), &gwPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": "namespace1",
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
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should disable bfd when removing the annotation from the namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				namespaceT.Annotations["k8s.ovn.org/bfd-enabled"] = ""

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
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				namespaceT.Annotations = map[string]string{"k8s.ovn.org/routing-external-gws": "9.0.0.1"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.Background(), &namespaceT, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalSwitchPort{
						UUID:      "lsp1",
						Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
						ExternalIDs: map[string]string{
							"pod":       "true",
							"namespace": "namespace1",
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
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
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
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]",
						Priority: types.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.4"},
						Match:    "inport == \"rtos-node1\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]"},
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.addHybridRoutePolicyForPod(net.ParseIP("10.128.1.3"), "node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				// check if the address-set was created with the podIP
				fakeOvn.asf.ExpectAddressSetWithIPs("hybrid-route-pods-node1", []string{"10.128.1.3"})
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("should reconcile a pod and create/delete the hybridRoutePolicy accordingly", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal

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
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
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
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				injectNode(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				nbWithLRP := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "lrp1",
						Action:   "reroute",
						Match:    "inport == \"rtos-node1\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
						Nexthops: []string{"100.64.0.4"},
						Priority: types.HybridOverlayReroutePriority,
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
						UUID:  "493c61b4-2f97-446d-a1f0-1f713b510bbf",
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
						UUID:     "e496b76e-18a1-461e-a919-6dcf0b3c35db",
						Name:     "ovn_cluster_router",
						Policies: []string{"lrp1"},
					},
					&nbdb.LogicalRouter{
						UUID:         "8945d2c1-bf8a-43ab-aa9f-6130eb525682",
						Name:         "GR_node1",
						StaticRoutes: []string{"static-route-1-UUID"},
					},
				}

				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(`{"default": {"ip_addresses":["` + t.podIP + `/24"], "mac_address":"` + t.podMAC + `", "gateway_ips": ["` + t.nodeGWIP + `"], "ip_address":"` + t.podIP + `/24", "gateway_ip": "` + t.nodeGWIP + `"}}`))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(nbWithLRP))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
					&nbdb.LogicalSwitch{
						UUID: "493c61b4-2f97-446d-a1f0-1f713b510bbf",
						Name: "node1",
					},
					&nbdb.LogicalRouter{
						UUID: "e496b76e-18a1-461e-a919-6dcf0b3c35db",
						Name: "ovn_cluster_router",
					},
					&nbdb.LogicalRouter{
						UUID: "8945d2c1-bf8a-43ab-aa9f-6130eb525682",
						Name: "GR_node1",
					},
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

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
								Name: ovntypes.OVNClusterRouter,
								UUID: ovntypes.OVNClusterRouter + "-UUID",
							},
						},
					},
				)
				finalNB := []libovsdbtest.TestData{
					&nbdb.LogicalRouterPolicy{
						UUID:     "lrp1",
						Priority: types.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.0.4"},
						Match:    "inport == \"rtos-node1\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"lrp1"},
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
						fakeOvn.controller.addHybridRoutePolicyForPod(net.ParseIP(fmt.Sprintf("10.128.1.%d", podIndex)), "node1")
					}()
				}
				close(c)
				wg.Wait()
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))

				err := fakeOvn.controller.addHybridRoutePolicyForPod(net.ParseIP(fmt.Sprintf("10.128.1.%d", 6)), "node1")
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
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"2a7a61cb-fb13-4266-a3f0-9ac5c4471123 [u2596996164]"},
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
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
						UUID: "GR_node1-UUID",
						Name: "GR_node1",
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				injectNode(fakeOvn)
				err := fakeOvn.controller.delHybridRoutePolicyForPod(net.ParseIP("10.128.1.3"), "node1")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				fakeOvn.asf.ExpectEmptyAddressSet("hybrid-route-pods-node1")
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete hybrid route policy for pods with force", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == $a17568862106095406050 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
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
						UUID: "GR_node1-UUID",
						Name: "GR_node1",
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.delAllHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				fakeOvn.asf.ExpectEmptyAddressSet("hybrid-route-pods-node1")
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("delete legacy hybrid route policies", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				fakeOvn.startWithDBSetup(
					libovsdbtest.TestSetup{
						NBData: []libovsdbtest.TestData{
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-1st-UUID",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.0.4"},
								Match:    "inport == \"rtos-node1\" && ip4.src == 1.3.3.7 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-2nd-UUID",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == 1.3.3.8 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouterPolicy{
								UUID:     "501-new-UUID",
								Priority: types.HybridOverlayReroutePriority,
								Action:   nbdb.LogicalRouterPolicyActionReroute,
								Nexthops: []string{"100.64.1.4"},
								Match:    "inport == \"rtos-node2\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
							},
							&nbdb.LogicalRouter{
								Name:     ovntypes.OVNClusterRouter,
								UUID:     ovntypes.OVNClusterRouter + "-UUID",
								Policies: []string{"501-1st-UUID", "501-2nd-UUID", "501-new-UUID"},
							},
							&nbdb.LogicalRouter{
								UUID: "GR_node1-UUID",
								Name: "GR_node1",
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
						Priority: types.HybridOverlayReroutePriority,
						Action:   nbdb.LogicalRouterPolicyActionReroute,
						Nexthops: []string{"100.64.1.4"},
						Match:    "inport == \"rtos-node2\" && ip4.src == $a17568862106095406051 && ip4.dst != 10.128.0.0/14",
					},
					&nbdb.LogicalRouter{
						Name:     ovntypes.OVNClusterRouter,
						UUID:     ovntypes.OVNClusterRouter + "-UUID",
						Policies: []string{"501-new-UUID"},
					},
					&nbdb.LogicalRouter{
						UUID: "GR_node1-UUID",
						Name: "GR_node1",
					},
					&nbdb.LogicalRouterPort{
						UUID:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1" + "-UUID",
						Name:     ovntypes.GWRouterToJoinSwitchPrefix + ovntypes.GWRouterPrefix + "node1",
						Networks: []string{"100.64.0.4/32"},
					},
				}

				err := fakeOvn.controller.delAllLegacyHybridRoutePolicies()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
								Name: types.GWRouterPrefix + nodeName,
								UUID: types.GWRouterPrefix + nodeName + "-UUID",
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
						Name: types.GWRouterPrefix + nodeName,
						UUID: types.GWRouterPrefix + nodeName + "-UUID",
						Nat:  []string{"nat-UUID"},
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
				fakeOvn.controller.WatchPods()
				extIPs, err := getExternalIPsGRSNAT(fakeOvn.controller.watchFactory, pod[0].Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, fullMaskPodNet, _ := net.ParseCIDR("10.128.1.3/32")
				addOrUpdatePerPodGRSNAT(fakeOvn.controller.nbClient, pod[0].Spec.NodeName, extIPs, []*net.IPNet{fullMaskPodNet})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				finalNB = []libovsdbtest.TestData{
					&nbdb.LogicalRouter{
						Name: types.GWRouterPrefix + nodeName,
						UUID: types.GWRouterPrefix + nodeName + "-UUID",
						Nat:  []string{},
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
				err = deletePerPodGRSNAT(fakeOvn.controller.nbClient, nodeName, extIPs, []*net.IPNet{fullMaskPodNet})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalNB))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

// injectNode adds a valid node to the nodeinformer so the get
// to understand if there are two bridged won't fail
func injectNode(fakeOvn *FakeOVN) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Annotations: map[string]string{"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"local","mac-address":"7e:57:f8:f0:3c:49", "ip-address":"169.254.33.2/24", "next-hop":"169.254.33.1"}}`,
				"k8s.ovn.org/node-chassis-id": "79fdcfc4-6fe6-4cd3-8242-c0f85a4668ec",
				"k8s.ovn.org/node-subnets":    `{"default":"10.128.1.0/24"}`,
			},
		},
	}
	fakeOvn.controller.watchFactory.NodeInformer().GetStore().Add(node)
}
