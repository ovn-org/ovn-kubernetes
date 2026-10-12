// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
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

	It("starts the default network controller before network manager", func() {
		cm := newCM()

		releaseNM := make(chan struct{})
		nmEntered := make(chan struct{})
		var nodeSwitchErr error
		nm := &networkmanager.FakeNetworkManager{
			StartFunc: func() error {
				// DNC.Start must have programmed the local node switch before NAD/UDN sync begins.
				_, nodeSwitchErr = libovsdbops.GetLogicalSwitch(nbClient, &nbdb.LogicalSwitch{Name: nodeName})
				close(nmEntered)
				<-releaseNM
				return nil
			},
		}
		cm.networkManager = nm
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseNM) }) }
		DeferCleanup(func() {
			release()
			cm.Stop()
		})

		errCh := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			errCh <- cm.Start(context.Background())
		}()

		Eventually(nmEntered, 60*time.Second).Should(BeClosed())
		Expect(nodeSwitchErr).NotTo(HaveOccurred(), "default network node logical switch must exist before network manager Start")
		Expect(cm.defaultNetworkController).NotTo(BeNil())
		Expect(nm.Started()).To(BeFalse())
		Consistently(errCh, 50*time.Millisecond).ShouldNot(Receive())

		release()
		Eventually(errCh, 30*time.Second).Should(Receive(BeNil()))
		Expect(nm.Started()).To(BeTrue())
	})

	It("does not start network manager when default network controller start fails", func() {
		cm := newCM()

		// Fail after initDefaultNetworkController succeeds so this asserts the
		// Start-before-NM order (an init-only failure would return earlier and
		// still pass if networkManager.Start were moved before DNC.Start).
		origStart := startDefaultNetworkController
		DeferCleanup(func() { startDefaultNetworkController = origStart })
		startDefaultNetworkController = func(context.Context, networkmanager.BaseNetworkController) error {
			return fmt.Errorf("injected start failure")
		}

		nmStarted := false
		nm := &networkmanager.FakeNetworkManager{
			StartFunc: func() error {
				nmStarted = true
				return nil
			},
		}
		cm.networkManager = nm
		DeferCleanup(cm.Stop)

		err := cm.Start(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("failed to start default network controller: injected start failure"))
		Expect(nmStarted).To(BeFalse())
		Expect(nm.Started()).To(BeFalse())
	})
})
