// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ignoreAlreadyRegisteredRegisterer allows ControllerManager.Start to be invoked
// more than once in a single test process. Production metrics registration uses
// prometheus.MustRegister and assumes a single leadership term per process.
type ignoreAlreadyRegisteredRegisterer struct {
	prometheus.Registerer
}

func (r ignoreAlreadyRegisteredRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// udnEgressIPNBTestConfig holds caller-supplied topology and EgressIP object
// identity for prepareUDNEgressIPNBControllerManagerTest. Leave StalePodIP /
// StalePodName empty to omit a stale reroute LRP from NB.
type udnEgressIPNBTestConfig struct {
	NodeName                string
	Namespace               string
	NADName                 string
	NetworkName             string
	NetworkSubnets          string
	UDNNodeSubnet           string
	EgressIPName            string
	EgressIP                string
	EgressIPMark            int
	LivePodName             string
	LivePodIP               string
	StalePodName            string
	StalePodIP              string
	UDNJoinIP               string
	EgressIPHealthCheckPort int
}

type udnEgressIPNBFixture struct {
	cm                        *ControllerManager
	nbClient                  libovsdbclient.Client
	sbClient                  libovsdbclient.Client
	nbsbCleanup               *libovsdbtest.Context
	mutableNetInfo            util.NetInfo
	pod                       *corev1.Pod
	podIPNet                  *net.IPNet
	hwAddr                    net.HardwareAddr
	namespace                 string
	nadKey                    string
	nodeName                  string
	findUDNEgressIPRerouteLRP func(podIP, podName string) ([]*nbdb.LogicalRouterPolicy, error)
	livePodIP                 string
	livePodName               string
	stalePodIP                string
	stalePodName              string
	liveLRPUUID               string
	staleLRPUUID              string
}

func udnEgressIPRerouteLRP(
	uuid, networkName, egressIPName, namespace, podName, podIP, joinIP string,
	eipMark int,
) *nbdb.LogicalRouterPolicy {
	return &nbdb.LogicalRouterPolicy{
		UUID:     uuid,
		Priority: types.EgressIPReroutePriority,
		Match:    fmt.Sprintf("ip4.src == %s", podIP),
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		Nexthops: []string{joinIP},
		ExternalIDs: libovsdbops.NewDbObjectIDs(
			libovsdbops.LogicalRouterPolicyEgressIP,
			types.DefaultNetworkControllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", egressIPName, namespace, podName),
				libovsdbops.PriorityKey:   fmt.Sprintf("%d", types.EgressIPReroutePriority),
				libovsdbops.IPFamilyKey:   "ip4",
				libovsdbops.NetworkKey:    networkName,
			},
		).GetExternalIDs(),
		Options: map[string]string{"pkt_mark": fmt.Sprintf("%d", eipMark)},
	}
}

// prepareUDNEgressIPNBControllerManagerTest builds a controller-manager fixture with
// pre-existing UDN EgressIP reroute policy state in NB from cfg. When cfg.StalePodIP
// is non-empty, an additional stale reroute LRP is seeded.
func prepareUDNEgressIPNBControllerManagerTest(
	wg *sync.WaitGroup,
	wf **factory.WatchFactory,
	nbsbCleanup **libovsdbtest.Context,
	cfg udnEgressIPNBTestConfig,
) *udnEgressIPNBFixture {
	Expect(cfg.NodeName).NotTo(BeEmpty())
	Expect(cfg.Namespace).NotTo(BeEmpty())
	Expect(cfg.NADName).NotTo(BeEmpty())
	Expect(cfg.NetworkName).NotTo(BeEmpty())
	Expect(cfg.LivePodName).NotTo(BeEmpty())
	Expect(cfg.LivePodIP).NotTo(BeEmpty())
	if cfg.StalePodIP != "" {
		Expect(cfg.StalePodName).NotTo(BeEmpty(), "StalePodName is required when StalePodIP is set")
	}

	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Gateway.Mode = config.GatewayModeShared
	config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort = cfg.EgressIPHealthCheckPort

	nadKey := util.GetNADName(cfg.Namespace, cfg.NADName)
	egressLabel := map[string]string{"egress": "needed"}

	netconf := ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: cfg.NetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		Role:     types.NetworkRolePrimary,
		Topology: types.Layer3Topology,
		NADName:  nadKey,
		Subnets:  cfg.NetworkSubnets,
	}
	netconfBytes, err := json.Marshal(netconf)
	Expect(err).NotTo(HaveOccurred())
	nad := &nadv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.NADName,
			Namespace: cfg.Namespace,
			Annotations: map[string]string{
				types.OvnNetworkIDAnnotation: "2",
			},
		},
		Spec: nadv1.NetworkAttachmentDefinitionSpec{Config: string(netconfBytes)},
	}
	netInfo, err := util.ParseNADInfo(nad)
	Expect(err).NotTo(HaveOccurred())
	mutableNetInfo := util.NewMutableNetInfo(netInfo)
	mutableNetInfo.AddNADs(nadKey)

	node := testNode(cfg.NodeName)
	node.Labels = map[string]string{"k8s.ovn.org/egress-assignable": ""}
	node.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf(
		`{"default":["10.128.0.0/23"],"%s":"%s"}`, cfg.NetworkName, cfg.UDNNodeSubnet)
	node.Annotations["k8s.ovn.org/network-ids"] = fmt.Sprintf(
		`{"default":"0","%s":"2"}`, cfg.NetworkName)
	node.Annotations["k8s.ovn.org/node-primary-ifaddr"] = `{"ipv4":"10.1.1.2/24","ipv6":""}`

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.Namespace,
			Labels: map[string]string{
				types.RequiredUDNNamespaceLabel: "",
				"egress":                        "needed",
			},
		},
	}
	pod := ovntest.NewPodWithLabels(cfg.Namespace, cfg.LivePodName, cfg.NodeName, cfg.LivePodIP, egressLabel)
	podIPNet := ovntest.MustParseIPNet(cfg.LivePodIP + util.GetIPFullMaskString(cfg.LivePodIP))
	hwAddr, err := net.ParseMAC("00:00:5e:00:53:01")
	Expect(err).NotTo(HaveOccurred())
	pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
		IPs:  []*net.IPNet{podIPNet},
		MAC:  hwAddr,
		Role: types.NetworkRolePrimary,
	}, nadKey)
	Expect(err).NotTo(HaveOccurred())

	eip := &egressipv1.EgressIP{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.EgressIPName,
			Annotations: map[string]string{
				util.EgressIPMarkAnnotation: fmt.Sprintf("%d", cfg.EgressIPMark),
			},
		},
		Spec: egressipv1.EgressIPSpec{
			EgressIPs: []string{cfg.EgressIP},
			PodSelector: metav1.LabelSelector{
				MatchLabels: egressLabel,
			},
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: egressLabel,
			},
		},
		Status: egressipv1.EgressIPStatus{
			Items: []egressipv1.EgressIPStatusItem{{
				Node:     cfg.NodeName,
				EgressIP: cfg.EgressIP,
			}},
		},
	}

	if *wf != nil {
		(*wf).Shutdown()
		*wf = nil
	}
	(*nbsbCleanup).Cleanup()
	udnRouterName := mutableNetInfo.GetNetworkScopedClusterRouterName()
	udnJoinLRPUUID := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + cfg.NetworkName + "_" + cfg.NodeName + "-UUID"
	udnJoinLRPName := types.GWRouterToJoinSwitchPrefix + types.GWRouterPrefix + cfg.NetworkName + "_" + cfg.NodeName
	_, udnNodeSubnet, err := net.ParseCIDR(cfg.UDNNodeSubnet)
	Expect(err).NotTo(HaveOccurred())
	mgmtIP := util.GetNodeManagementIfAddr(udnNodeSubnet).IP.String()
	liveLRPUUID := fmt.Sprintf("udn-eip-reroute-%s-UUID", cfg.LivePodName)
	liveLRP := udnEgressIPRerouteLRP(
		liveLRPUUID, cfg.NetworkName, cfg.EgressIPName, cfg.Namespace, cfg.LivePodName,
		cfg.LivePodIP, cfg.UDNJoinIP, cfg.EgressIPMark,
	)
	udnRouterPolicies := []string{liveLRPUUID}
	nbData := []libovsdbtest.TestData{
		&nbdb.NBGlobal{UUID: "nbglobal-UUID", Options: map[string]string{}},
		&nbdb.LogicalRouter{Name: types.OVNClusterRouter},
		&nbdb.LogicalSwitch{Name: types.OVNJoinSwitch},
		&nbdb.LoadBalancerGroup{Name: types.ClusterLBGroupName},
		&nbdb.LoadBalancerGroup{Name: types.ClusterSwitchLBGroupName},
		&nbdb.LoadBalancerGroup{Name: types.ClusterRouterLBGroupName},
		liveLRP,
	}
	staleLRPUUID := ""
	if cfg.StalePodIP != "" {
		staleLRPUUID = fmt.Sprintf("udn-eip-reroute-%s-UUID", cfg.StalePodName)
		nbData = append(nbData, udnEgressIPRerouteLRP(
			staleLRPUUID, cfg.NetworkName, cfg.EgressIPName, cfg.Namespace, cfg.StalePodName,
			cfg.StalePodIP, cfg.UDNJoinIP, cfg.EgressIPMark,
		))
		udnRouterPolicies = append(udnRouterPolicies, staleLRPUUID)
	}
	nbData = append(nbData,
		&nbdb.LogicalRouter{
			UUID:        udnRouterName + "-UUID",
			Name:        udnRouterName,
			Policies:    udnRouterPolicies,
			ExternalIDs: map[string]string{types.NetworkExternalID: cfg.NetworkName, types.TopologyExternalID: types.Layer3Topology},
		},
		&nbdb.LogicalRouterPort{
			UUID:     udnJoinLRPUUID,
			Name:     udnJoinLRPName,
			Networks: []string{cfg.UDNJoinIP + "/16"},
		},
		&nbdb.LogicalRouter{
			UUID:        mutableNetInfo.GetNetworkScopedGWRouterName(cfg.NodeName) + "-UUID",
			Name:        mutableNetInfo.GetNetworkScopedGWRouterName(cfg.NodeName),
			Ports:       []string{udnJoinLRPUUID},
			ExternalIDs: map[string]string{types.NetworkExternalID: cfg.NetworkName, types.TopologyExternalID: types.Layer3Topology},
		},
		&nbdb.LogicalSwitchPort{
			UUID:      "k8s-" + cfg.NetworkName + "_" + cfg.NodeName + "-UUID",
			Name:      "k8s-" + cfg.NetworkName + "_" + cfg.NodeName,
			Addresses: []string{"fe:1a:b2:3f:0e:fb " + mgmtIP},
		},
		&nbdb.LogicalSwitch{
			UUID:        mutableNetInfo.GetNetworkScopedSwitchName(cfg.NodeName) + "-UUID",
			Name:        mutableNetInfo.GetNetworkScopedSwitchName(cfg.NodeName),
			Ports:       []string{"k8s-" + cfg.NetworkName + "_" + cfg.NodeName + "-UUID"},
			ExternalIDs: util.GenerateExternalIDsForSwitchOrRouter(mutableNetInfo),
		},
	)
	dbSetup := libovsdbtest.TestSetup{
		NBData: nbData,
		SBData: []libovsdbtest.TestData{
			&sbdb.SBGlobal{UUID: "sbglobal-UUID", Options: map[string]string{}},
		},
	}
	nbClient, sbClient, newCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	Expect(err).NotTo(HaveOccurred())
	*nbsbCleanup = newCleanup

	findUDNEgressIPRerouteLRP := func(podIP, egressPodName string) ([]*nbdb.LogicalRouterPolicy, error) {
		return libovsdbops.FindLogicalRouterPoliciesWithPredicate(nbClient, func(item *nbdb.LogicalRouterPolicy) bool {
			return item.Priority == types.EgressIPReroutePriority &&
				item.Match == fmt.Sprintf("ip4.src == %s", podIP) &&
				item.ExternalIDs[libovsdbops.NetworkKey.String()] == cfg.NetworkName &&
				item.ExternalIDs[libovsdbops.ObjectNameKey.String()] == fmt.Sprintf("%s_%s/%s", cfg.EgressIPName, cfg.Namespace, egressPodName)
		})
	}

	ovnClient := util.GetOVNClientset(node, ns, pod, nad, eip)
	var newWF *factory.WatchFactory
	newWF, err = factory.NewOVNKubeControllerWatchFactory(ovnClient.GetOVNKubeControllerClientset(), cfg.NodeName)
	Expect(err).NotTo(HaveOccurred())
	*wf = newWF

	cm, err := NewControllerManager(cfg.NodeName, ovnClient, newWF, nbClient, sbClient, record.NewFakeRecorder(10), wg)
	Expect(err).NotTo(HaveOccurred())

	// libovsdbtest may rewrite named UUIDs from TestData; capture the actual
	// UUIDs present in NB after harness creation for later comparisons.
	liveLRPs, err := findUDNEgressIPRerouteLRP(cfg.LivePodIP, cfg.LivePodName)
	Expect(err).NotTo(HaveOccurred())
	Expect(liveLRPs).To(HaveLen(1))
	actualLiveLRPUUID := liveLRPs[0].UUID
	actualStaleLRPUUID := ""
	if cfg.StalePodIP != "" {
		staleLRPs, findErr := findUDNEgressIPRerouteLRP(cfg.StalePodIP, cfg.StalePodName)
		Expect(findErr).NotTo(HaveOccurred())
		Expect(staleLRPs).To(HaveLen(1))
		actualStaleLRPUUID = staleLRPs[0].UUID
	}

	return &udnEgressIPNBFixture{
		cm:                        cm,
		nbClient:                  nbClient,
		sbClient:                  sbClient,
		nbsbCleanup:               newCleanup,
		mutableNetInfo:            mutableNetInfo,
		pod:                       pod,
		podIPNet:                  podIPNet,
		hwAddr:                    hwAddr,
		namespace:                 cfg.Namespace,
		nadKey:                    nadKey,
		nodeName:                  cfg.NodeName,
		findUDNEgressIPRerouteLRP: findUDNEgressIPRerouteLRP,
		livePodIP:                 cfg.LivePodIP,
		livePodName:               cfg.LivePodName,
		stalePodIP:                cfg.StalePodIP,
		stalePodName:              cfg.StalePodName,
		liveLRPUUID:               actualLiveLRPUUID,
		staleLRPUUID:              actualStaleLRPUUID,
	}
}

func udnEgressIPFakeNetworkManager(f *udnEgressIPNBFixture, dncFinishedWithoutRemovingLRP *bool) *networkmanager.FakeNetworkManager {
	return &networkmanager.FakeNetworkManager{
		PrimaryNetworks: map[string]util.NetInfo{
			f.namespace: f.mutableNetInfo,
		},
		NADNetworks: map[string]util.NetInfo{
			f.nadKey: f.mutableNetInfo,
		},
		StartFunc: func() error {
			// With NM-before-DNC, UDN stale sync runs from network manager Start
			// (or async UDN Start). When asserting DNC does not remove LRPs,
			// SyncNetworkReady is deferred until after DNC.Start returns.
			if dncFinishedWithoutRemovingLRP != nil {
				return nil
			}
			f.cm.portCache.Add(f.pod, f.mutableNetInfo.GetNetworkScopedSwitchName(f.nodeName), f.nadKey, "lsp-uuid", f.hwAddr, []*net.IPNet{f.podIPNet})
			f.cm.eIPController.EnqueueNetwork(f.mutableNetInfo.GetNetworkName())
			Expect(f.cm.eIPController.SyncNetworkReady(f.mutableNetInfo.GetNetworkName())).To(Succeed())
			return nil
		},
	}
}

func testNode(nodeName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"k8s.ovn.org/node-subnets":    `{"default":["10.128.0.0/23"]}`,
				"k8s.ovn.org/node-chassis-id": "chassis-id",
				util.OvnNodeID:                "1",
				util.OvnNodeZoneName:          nodeName,
				util.Layer2TopologyVersion:    util.TransitRouterTopoVersion,
				"k8s.ovn.org/network-ids":     `{"default":"0"}`,
				"k8s.ovn.org/l3-gateway-config": `{"default":{"mode":"shared","chassis-id":"chassis-id","ip-addresses":["10.1.1.2/24"],` +
					`"mac-address":"00:00:00:55:66:77","next-hops":["10.1.1.1"],"node-port-enable":"true"}}`,
				"k8s.ovn.org/node-gateway-router-lrp-ifaddrs": `{"default":"100.64.0.3/16"}`,
				util.OVNNodeHostCIDRs:                         `["10.1.1.2/24"]`,
				util.OVNNodeEncapIPs:                          `["10.1.1.2"]`,
				util.OvnTransitSwitchPortAddr:                 `{"ipv4":"100.88.0.2/16"}`,
			},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.1.1.2"}},
		},
	}
}

var _ = Describe("ControllerManager Start order", func() {
	const nodeName = "worker1"

	var (
		nbClient       libovsdbclient.Client
		sbClient       libovsdbclient.Client
		nbsbCleanup    *libovsdbtest.Context
		wf             *factory.WatchFactory
		wg             *sync.WaitGroup
		prevRegisterer prometheus.Registerer
		prevGatherer   prometheus.Gatherer
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.IPv4Mode = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = false
		config.OVNKubernetesFeature.EnableEgressIP = false
		config.OVNKubernetesFeature.EnableObservability = false
		config.OVNKubernetesFeature.EnableRouteAdvertisements = false
		config.OVNKubernetesFeature.EnableServiceTemplateSupport = false
		config.EnableMulticast = false

		// Allow configureMetrics to run for every Start() in this Describe.
		reg := prometheus.NewRegistry()
		prevRegisterer = prometheus.DefaultRegisterer
		prevGatherer = prometheus.DefaultGatherer
		prometheus.DefaultRegisterer = ignoreAlreadyRegisteredRegisterer{reg}
		prometheus.DefaultGatherer = reg

		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --columns=_uuid list Chassis_Template_Var",
			Err: fmt.Errorf("not supported"),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --columns=_uuid list Load_Balancer_Group",
			Output: "uuid",
		})
		Expect(util.SetExec(fexec)).To(Succeed())

		var err error
		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.NBGlobal{UUID: "nbglobal-UUID", Options: map[string]string{}},
				&nbdb.LogicalRouter{Name: types.OVNClusterRouter},
				&nbdb.LogicalSwitch{Name: types.OVNJoinSwitch},
				&nbdb.LoadBalancerGroup{Name: types.ClusterLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterSwitchLBGroupName},
				&nbdb.LoadBalancerGroup{Name: types.ClusterRouterLBGroupName},
			},
			SBData: []libovsdbtest.TestData{
				&sbdb.SBGlobal{UUID: "sbglobal-UUID", Options: map[string]string{}},
			},
		}
		nbClient, sbClient, nbsbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
		Expect(err).NotTo(HaveOccurred())

		wg = &sync.WaitGroup{}
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
			wf = nil
		}
		if nbsbCleanup != nil {
			nbsbCleanup.Cleanup()
			nbsbCleanup = nil
		}
		util.ResetRunner()
		if prevRegisterer != nil {
			prometheus.DefaultRegisterer = prevRegisterer
			prometheus.DefaultGatherer = prevGatherer
		}
	})

	newCM := func() *ControllerManager {
		ovnClient := util.GetOVNClientset(testNode(nodeName))
		var err error
		wf, err = factory.NewOVNKubeControllerWatchFactory(ovnClient.GetOVNKubeControllerClientset(), nodeName)
		Expect(err).NotTo(HaveOccurred())

		cm, err := NewControllerManager(nodeName, ovnClient, wf, nbClient, sbClient, record.NewFakeRecorder(10), wg)
		Expect(err).NotTo(HaveOccurred())
		return cm
	}

	It("starts network manager before the default network controller", func() {
		cm := newCM()

		nmStartedBeforeDNC := false
		nm := &networkmanager.FakeNetworkManager{
			StartFunc: func() error {
				return nil
			},
		}
		cm.networkManager = nm

		origStart := startDefaultNetworkController
		DeferCleanup(func() { startDefaultNetworkController = origStart })
		startDefaultNetworkController = func(ctx context.Context, c networkmanager.BaseNetworkController) error {
			nmStartedBeforeDNC = nm.Started()
			return origStart(ctx, c)
		}
		DeferCleanup(cm.Stop)

		Expect(cm.Start(context.Background())).To(Succeed())
		Expect(nmStartedBeforeDNC).To(BeTrue(), "network manager must start before DNC.Start")
		Expect(nm.Started()).To(BeTrue())
		Expect(cm.defaultNetworkController).NotTo(BeNil())
	})

	baseUDNEgressIPNBConfig := func() udnEgressIPNBTestConfig {
		return udnEgressIPNBTestConfig{
			NodeName:                nodeName,
			Namespace:               "udn-ns",
			NADName:                 "nad1",
			NetworkName:             "network1",
			NetworkSubnets:          "20.128.0.0/14",
			UDNNodeSubnet:           "20.128.0.0/16",
			EgressIPName:            "egressip",
			EgressIP:                "192.168.126.101",
			EgressIPMark:            50000,
			LivePodName:             "egress-pod",
			LivePodIP:               "20.128.0.5",
			UDNJoinIP:               "100.65.0.2",
			EgressIPHealthCheckPort: 1234,
		}
	}

	It("does not remove UDN EgressIP NBDB entries when DNC starts", func() {
		// Pre-existing UDN EgressIP OVN config must survive DefaultNetworkController.Start.
		// Default-network StartEgressIP must not treat UDN LRPs as stale; UDN cleanup is
		// scoped via SyncNetworkReady after the UDN is reconciled.
		fixture := prepareUDNEgressIPNBControllerManagerTest(wg, &wf, &nbsbCleanup, baseUDNEgressIPNBConfig())
		nbClient = fixture.nbClient
		sbClient = fixture.sbClient

		existingLRPs, err := fixture.findUDNEgressIPRerouteLRP(fixture.livePodIP, fixture.livePodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(existingLRPs).NotTo(BeEmpty())
		existingLRPUUIDs := []string{fixture.liveLRPUUID}

		dncFinishedWithoutRemovingLRP := false
		origStart := startDefaultNetworkController
		DeferCleanup(func() { startDefaultNetworkController = origStart })
		startDefaultNetworkController = func(ctx context.Context, c networkmanager.BaseNetworkController) error {
			if err := origStart(ctx, c); err != nil {
				return err
			}
			lrps, getErr := fixture.findUDNEgressIPRerouteLRP(fixture.livePodIP, fixture.livePodName)
			Expect(getErr).NotTo(HaveOccurred())
			lrpUUIDs := make([]string, 0, len(lrps))
			for _, lrp := range lrps {
				lrpUUIDs = append(lrpUUIDs, lrp.UUID)
			}
			Expect(lrpUUIDs).To(ConsistOf(existingLRPUUIDs),
				"DNC.Start must not remove or replace UDN EgressIP NBDB entries")
			dncFinishedWithoutRemovingLRP = true
			return nil
		}

		nm := udnEgressIPFakeNetworkManager(fixture, &dncFinishedWithoutRemovingLRP)
		fixture.cm.networkManager = nm
		DeferCleanup(fixture.cm.Stop)

		Expect(fixture.cm.Start(context.Background())).To(Succeed())
		Expect(dncFinishedWithoutRemovingLRP).To(BeTrue())
		Expect(nm.Started()).To(BeTrue())

		// Simulate UDN Start enqueue/sync after DNC (async UDN Start ordering).
		fixture.cm.portCache.Add(fixture.pod, fixture.mutableNetInfo.GetNetworkScopedSwitchName(fixture.nodeName),
			fixture.nadKey, "lsp-uuid", fixture.hwAddr, []*net.IPNet{fixture.podIPNet})
		fixture.cm.eIPController.EnqueueNetwork(fixture.mutableNetInfo.GetNetworkName())
		Expect(fixture.cm.eIPController.SyncNetworkReady(fixture.mutableNetInfo.GetNetworkName())).To(Succeed())

		lrps, err := fixture.findUDNEgressIPRerouteLRP(fixture.livePodIP, fixture.livePodName)
		Expect(err).NotTo(HaveOccurred())
		lrpUUIDs := make([]string, 0, len(lrps))
		for _, lrp := range lrps {
			lrpUUIDs = append(lrpUUIDs, lrp.UUID)
		}
		Expect(lrpUUIDs).To(ConsistOf(existingLRPUUIDs),
			"UDN SyncNetworkReady must not remove live UDN EgressIP NBDB entries")
	})

	It("removes stale UDN EgressIP NBDB entries during StartEgressIP", func() {
		cfg := baseUDNEgressIPNBConfig()
		cfg.StalePodName = "stale-egress-pod"
		cfg.StalePodIP = "20.128.0.99"
		fixture := prepareUDNEgressIPNBControllerManagerTest(wg, &wf, &nbsbCleanup, cfg)
		nbClient = fixture.nbClient
		sbClient = fixture.sbClient

		liveLRPs, err := fixture.findUDNEgressIPRerouteLRP(fixture.livePodIP, fixture.livePodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveLRPs).To(HaveLen(1))
		Expect(liveLRPs[0].UUID).To(Equal(fixture.liveLRPUUID))

		staleLRPs, err := fixture.findUDNEgressIPRerouteLRP(fixture.stalePodIP, fixture.stalePodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(staleLRPs).To(HaveLen(1))
		Expect(staleLRPs[0].UUID).To(Equal(fixture.staleLRPUUID))

		nm := udnEgressIPFakeNetworkManager(fixture, nil)
		fixture.cm.networkManager = nm
		DeferCleanup(fixture.cm.Stop)

		Expect(fixture.cm.Start(context.Background())).To(Succeed())
		Expect(nm.Started()).To(BeTrue())

		liveLRPs, err = fixture.findUDNEgressIPRerouteLRP(fixture.livePodIP, fixture.livePodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveLRPs).To(HaveLen(1))
		Expect(liveLRPs[0].UUID).To(Equal(fixture.liveLRPUUID))

		staleLRPs, err = fixture.findUDNEgressIPRerouteLRP(fixture.stalePodIP, fixture.stalePodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(staleLRPs).To(BeEmpty(), "syncEgressIPs must delete stale UDN EgressIP reroute policies")
	})
})
