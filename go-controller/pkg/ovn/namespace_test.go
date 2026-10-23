// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sretry "k8s.io/client-go/util/retry"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	dnsnameresolver "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newUDNNamespaceWithLabels(namespace string, additionalLabels map[string]string) *corev1.Namespace {
	n := &corev1.Namespace{
		ObjectMeta: ovntest.NewNamespaceMeta(namespace, additionalLabels),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
	n.Labels[ovntypes.RequiredUDNNamespaceLabel] = ""
	return n
}

func newUDNNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: ovntest.NewNamespaceMeta(namespace, map[string]string{ovntypes.RequiredUDNNamespaceLabel: ""}),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}

func getStaleNamespaceAddrSetDbIDs(namespaceName, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, controller, map[libovsdbops.ExternalIDKey]string{
		// namespace has only 1 address set, no additional ids are required
		libovsdbops.ObjectNameKey: namespaceName,
	})
}

func buildStaleNamespaceAddressSets(namespace string, ips []string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	return addressset.GetTestDbAddrSets(getStaleNamespaceAddrSetDbIDs(namespace, "default-network-controller"), ips)
}

// namespacePortGroupLookupClient lets tests advance namespace setup after the
// first pod lookup has already observed a missing group, without timed sleeps.
type namespacePortGroupLookupClient struct {
	libovsdbclient.Client
	lookups          int
	afterFirstLookup func()
	lookupErr        error
}

// Where intercepts only port-group lookups; mutations and transactions use the real client.
func (c *namespacePortGroupLookupClient) Where(models ...model.Model) libovsdbclient.ConditionalAPI {
	api := c.Client.Where(models...)
	if len(models) == 1 {
		if _, ok := models[0].(*nbdb.PortGroup); ok {
			return &namespacePortGroupLookupAPI{ConditionalAPI: api, client: c}
		}
	}
	return api
}

type namespacePortGroupLookupAPI struct {
	libovsdbclient.ConditionalAPI
	client *namespacePortGroupLookupClient
}

// List preserves the first lookup result even when its callback creates the group.
func (a *namespacePortGroupLookupAPI) List(ctx context.Context, result any) error {
	a.client.lookups++
	if a.client.lookupErr != nil {
		return a.client.lookupErr
	}
	err := a.ConditionalAPI.List(ctx, result)
	if a.client.lookups == 1 && a.client.afterFirstLookup != nil {
		a.client.afterFirstLookup()
	}
	return err
}

func TestNamespacePortGroupRetry(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressFirewall = true
	lookupErr := errors.New("injected port-group lookup failure")
	for _, tc := range []struct {
		name           string
		initiallyReady bool
		becomesReady   bool
		lookupErr      error
		wantErr        error
		wantLookups    int
	}{
		{name: "ready group needs one lookup", initiallyReady: true, wantLookups: 1},
		{name: "namespace catches up within pod setup", becomesReady: true, wantLookups: 2},
		{name: "missing group falls back to outer retry", wantErr: libovsdbclient.ErrNotFound, wantLookups: k8sretry.DefaultRetry.Steps},
		{name: "other errors are not retried", lookupErr: lookupErr, wantErr: lookupErr, wantLookups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			nbClient, cleanup, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{}, nil)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			t.Cleanup(cleanup.Cleanup)
			bnc := &BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(&util.DefaultNetInfo{}),
				controllerName:      ovntypes.DefaultNetworkControllerName,
			}
			pg := &nbdb.PortGroup{Name: bnc.getNamespacePortGroupName("namespace")}
			createGroup := func() {
				g.Expect(libovsdbops.CreatePortGroup(nbClient, pg)).To(gomega.Succeed())
				g.Eventually(func() error {
					_, err := libovsdbops.GetPortGroup(nbClient, &nbdb.PortGroup{Name: pg.Name})
					return err
				}).WithTimeout(5 * time.Second).Should(gomega.Succeed())
			}
			if tc.initiallyReady {
				createGroup()
			}
			lookupClient := &namespacePortGroupLookupClient{Client: nbClient, lookupErr: tc.lookupErr}
			if tc.becomesReady {
				lookupClient.afterFirstLookup = createGroup
			}
			bnc.nbClient = lookupClient

			port := &nbdb.LogicalSwitchPort{Name: "pod", UUID: "new-pod-port"}
			sw := &nbdb.LogicalSwitch{Name: "node", Ports: []string{port.UUID}}
			ops, err := nbClient.Create(port)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			switchOps, err := nbClient.Create(sw)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			ops = append(ops, switchOps...)
			membershipOps, err := bnc.addPodToNamespacePortGroupOps(ops, "namespace", port.UUID)
			g.Expect(lookupClient.lookups).To(gomega.Equal(tc.wantLookups))
			if tc.wantErr != nil {
				g.Expect(errors.Is(err, tc.wantErr)).To(gomega.BeTrue(), "lookup errors must survive the bounded retry")
				g.Expect(membershipOps).To(gomega.BeNil())
				g.Eventually(nbClient).WithTimeout(5 * time.Second).Should(libovsdb.HaveData())
				return
			}
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(membershipOps[:len(ops)]).To(gomega.Equal(ops), "retry must preserve the pod's pending operations")
			results, err := libovsdbops.TransactAndCheck(nbClient, membershipOps)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			portUUID := results[0].UUID.GoUUID
			g.Eventually(func() (*nbdb.PortGroup, error) {
				return libovsdbops.GetPortGroup(nbClient, &nbdb.PortGroup{Name: pg.Name})
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Ports", gomega.ConsistOf(portUUID)))
		})
	}
}

func TestNamespacePortGroupLifecycle(t *testing.T) {
	g := gomega.NewWithT(t)
	g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
	t.Cleanup(func() { _ = config.PrepareTestConfig() })
	config.OVNKubernetesFeature.EnableEgressFirewall = true
	udn, err := util.ParseNADInfo(ovntest.GenerateNAD("blue", "nad", "namespace",
		ovntypes.Layer3Topology, "100.128.0.0/16", ovntypes.NetworkRolePrimary))
	g.Expect(err).NotTo(gomega.HaveOccurred())

	for _, netInfo := range []util.NetInfo{&util.DefaultNetInfo{}, udn} {
		for _, beforeBuild := range []bool{true, false} {
			phase := "delete-before-transaction"
			if beforeBuild {
				phase = "delete-before-operation-build"
			}
			t.Run(netInfo.GetNetworkName()+"/"+phase, func(t *testing.T) {
				g := gomega.NewWithT(t)
				nbClient, cleanup, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{}, nil)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				t.Cleanup(cleanup.Cleanup)
				bnc := &BaseNetworkController{
					ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
					controllerName:      getNetworkControllerName(netInfo.GetNetworkName()),
					namespaces:          map[string]*namespaceInfo{},
				}
				bnc.nbClient = nbClient
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "namespace"}}
				_, unlock, err := bnc.ensureNamespaceLockedCommon(namespace.Name, false, namespace,
					func(context.Context, *namespaceInfo, *corev1.Namespace) error { return nil })
				g.Expect(err).NotTo(gomega.HaveOccurred())
				unlock()

				deleteNamespace := func() {
					nsInfo, err := bnc.deleteNamespaceLocked(namespace.Name)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					g.Expect(nsInfo).NotTo(gomega.BeNil())
					nsInfo.Unlock()
					g.Eventually(func() error {
						_, err := libovsdbops.GetPortGroup(nbClient,
							&nbdb.PortGroup{Name: bnc.getNamespacePortGroupName(namespace.Name)})
						return err
					}).Should(gomega.MatchError(libovsdbclient.ErrNotFound))
				}
				if beforeBuild {
					deleteNamespace()
				}
				port := &nbdb.LogicalSwitchPort{Name: "pod", UUID: "new-pod-port"}
				sw := &nbdb.LogicalSwitch{Name: "node", Ports: []string{port.UUID}}
				ops, err := nbClient.Create(port)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				switchOps, err := nbClient.Create(sw)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				ops = append(ops, switchOps...)
				ops, err = bnc.addPodToNamespacePortGroupOps(ops, namespace.Name, port.UUID)
				if beforeBuild {
					g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(libovsdbclient.ErrNotFound.Error())))
				} else {
					g.Expect(err).NotTo(gomega.HaveOccurred())
					deleteNamespace()
					results, err := libovsdbops.TransactAndCheck(nbClient, ops)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					port.UUID = results[0].UUID.GoUUID
					sw.UUID = results[1].UUID.GoUUID
					sw.Ports = []string{port.UUID}
					// A late pod add may commit, but must not recreate the namespace group.
					g.Eventually(nbClient).Should(libovsdb.HaveData(port, sw))
				}

				// Pod cleanup must tolerate the already-deleted dependency, while
				// neither the pod port nor an orphan namespace group may remain.
				ops, err = bnc.deletePodFromNamespace(namespace.Name, port.UUID)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				var expected []libovsdb.TestData
				if !beforeBuild {
					portOps, err := bnc.delLSPOps(port.Name, sw.Name, port.UUID)
					g.Expect(err).NotTo(gomega.HaveOccurred())
					ops = append(ops, portOps...)
					sw.Ports = nil
					expected = append(expected, sw)
				}
				_, err = libovsdbops.TransactAndCheck(nbClient, ops)
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Eventually(nbClient).Should(libovsdb.HaveData(expected))
			})
		}
	}
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	const (
		namespaceName  = "namespace1"
		controllerName = ovntypes.DefaultNetworkControllerName
	)
	var (
		fakeOvn *FakeOVN
		wg      *sync.WaitGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		err := config.PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		fakeOvn = NewFakeOVN(false, "node1")
		wg = &sync.WaitGroup{}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		wg.Wait()
	})

	ginkgo.Context("on startup", func() {
		ginkgo.It("only cleans up address sets owned by namespace", func() {
			// namespace address sets are now deprecated and should all be removed on startup
			namespace1 := ovntest.NewNamespace(namespaceName)
			// namespace-owned address set for existing namespace, should be deleted
			ns1, _ := buildStaleNamespaceAddressSets(namespaceName, []string{"1.1.1.1"})
			// namespace-owned address set for stale namespace, should be deleted
			ns2, _ := buildStaleNamespaceAddressSets("namespace2", []string{"1.1.1.2"})
			// netpol peer address set will be removed by the addresssetManager as unreferenced
			netpol := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", ovntypes.DefaultNetworkControllerName, false)
			netpolAS, _ := addressset.GetTestDbAddrSets(netpol, []string{"1.1.1.3"})
			// egressQoS-owned address set, should stay
			qos := getEgressQosAddrSetDbIDs("namespace", "0", controllerName)
			qosAS, _ := addressset.GetTestDbAddrSets(qos, []string{"1.1.1.4"})
			// hybridNode-owned address set, should stay
			hybridNode := apbroute.GetHybridRouteAddrSetDbIDs("node", ovntypes.DefaultNetworkControllerName)
			hybridNodeAS, _ := addressset.GetTestDbAddrSets(hybridNode, []string{"1.1.1.5"})
			// egress firewall-owned address set, should stay
			ef := dnsnameresolver.GetEgressFirewallDNSAddrSetDbIDs("dnsname", controllerName)
			efAS, _ := addressset.GetTestDbAddrSets(ef, []string{"1.1.1.6"})

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: []libovsdb.TestData{ns1, ns2, netpolAS, qosAS, hybridNodeAS, efAS}})
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{ns1, ns2, qosAS, hybridNodeAS, efAS}))

			// now namespace address sets will be cleaned up
			err := fakeOvn.controller.syncNamespaces(context.Background(), []interface{}{namespace1})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{qosAS, hybridNodeAS, efAS}))
		})

		ginkgo.It("reconciles an existing namespace with pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			namespaceT := *ovntest.NewNamespace(namespaceName)
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

			tPod := ovntest.NewPod(namespaceT.Name, tP.podName, tP.nodeName, tP.podIP)
			fakeOvn.start(
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
				&corev1.PodList{
					Items: []corev1.Pod{
						*tPod,
					},
				},
			)
			podMAC := ovntest.MustParseMAC(tP.podMAC)
			podIPNets := []*net.IPNet{ovntest.MustParseIPNet(tP.podIP + "/24")}
			fakeOvn.controller.logicalPortCache.add(tPod, tP.nodeName, ovntypes.DefaultNetworkName, fakeUUID, podMAC, podIPNets)
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceT.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// port group is empty, because it will be filled by pod add logic
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("creates an empty address set and port group for the namespace without pods", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			fakeOvn.start(&corev1.NamespaceList{
				Items: []corev1.Namespace{
					*ovntest.NewNamespace(namespaceName),
				},
			})
			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{pg}))
		})

		ginkgo.It("reconciles an existing namespace port group, without updating it", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			namespaceT := *ovntest.NewNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialData))
		})
		ginkgo.It("deletes an existing namespace port group when egress firewall and multicast are disabled", func() {
			namespaceT := *ovntest.NewNamespace(namespaceName)
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NamespaceList{
					Items: []corev1.Namespace{
						namespaceT,
					},
				},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
		ginkgo.It("deletes an existing namespace port group when there are no namespaces", func() {
			// this flag will create namespaced port group
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			pgIDs := getNamespacePortGroupDbIDs(namespaceName, ovntypes.DefaultNetworkControllerName)
			pg := libovsdbutil.BuildPortGroup(pgIDs, nil, nil)
			pg.UUID = pg.Name + "-UUID"
			initialData := []libovsdb.TestData{pg}

			fakeOvn.startWithDBSetup(libovsdb.TestSetup{NBData: initialData},
				&corev1.NodeList{
					Items: []corev1.Node{
						*newNode("node1", "192.168.126.202/24"),
					},
				},
			)

			err := fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData([]libovsdb.TestData{}))
		})
	})
})
