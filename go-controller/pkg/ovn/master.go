package ovn

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nad-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const nadControllerName = "ovnkube-master-nad-controller"

type ovnkubeMaster struct {
	client       clientset.Interface
	kube         *kube.KubeOVN
	watchFactory *factory.WatchFactory
	podRecorder  *metrics.PodRecorder
	// event recorder used to post events to k8s
	recorder record.EventRecorder
	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	// libovsdb southbound client interface
	sbClient libovsdbclient.Client
	// has SCTP support
	SCTPSupport bool
	// Supports multicast?
	multicastSupport bool

	stopChan chan struct{}
	wg       *sync.WaitGroup

	// unique identity for controllerManager running on different ovnkube-master instance,
	// used for leader election
	identity string

	defaultNetworkController *DefaultNetworkController

	// net-attach-def controller handle net-attach-def and create/delete network controllers
	nadController *nad_controller.NetAttachDefinitionController
}

// NewMaster creates a new OVN controller manager to manage all the controller for all networks
func NewMaster(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) *ovnkubeMaster {
	podRecorder := metrics.NewPodRecorder()

	master := &ovnkubeMaster{
		client: ovnClient.KubeClient,
		kube: &kube.KubeOVN{
			Kube:                 kube.Kube{KClient: ovnClient.KubeClient},
			EIPClient:            ovnClient.EgressIPClient,
			EgressFirewallClient: ovnClient.EgressFirewallClient,
			CloudNetworkClient:   ovnClient.CloudNetworkClient,
		},
		stopChan:     make(chan struct{}),
		watchFactory: wf,
		recorder:     recorder,
		nbClient:     libovsdbOvnNBClient,
		sbClient:     libovsdbOvnSBClient,
		podRecorder:  &podRecorder,
		wg:           wg,
		identity:     identity,
	}
	cnci := NewCommonNetworkControllerInfo(master.client, master.kube, master.watchFactory, master.recorder, master.nbClient,
		master.sbClient, master.podRecorder, master.SCTPSupport, master.multicastSupport)
	master.defaultNetworkController = NewDefaultNetworkController(cnci)

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm := NewNetworkControllerManager(ovnClient, identity, wf,
			libovsdbOvnNBClient, libovsdbOvnSBClient, recorder, wg)
		klog.Infof("Multiple network supported, creating NAD controller", nadControllerName)
		master.nadController = nad_controller.NewNetAttachDefinitionController(cm, ovnClient.NetworkAttchDefClient,
			recorder, nadControllerName)
	}
	return master
}

// Start waits until this process is the leader before starting master functions
func (master *ovnkubeMaster) Start(ctx context.Context, cancel context.CancelFunc) error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		// TODO (rravaiol) (bpickard)
		// https://github.com/kubernetes/kubernetes/issues/107454
		// leader election library no longer supports leader-election
		// locks based solely on `endpoints` or `configmaps` resources.
		// Slowly migrating to new API across three releases; with k8s 1.24
		// we're now in the second step ('x+2') bullet from the link above).
		// This will have to be updated for the next k8s bump: to 1.26.
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-master",
		master.client.CoreV1(),
		master.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      master.identity,
			EventRecorder: master.recorder,
		},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(config.MasterHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(config.MasterHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(config.MasterHA.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("Won leader election; in active mode")
				klog.Infof("Starting cluster master")
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()

				if err = master.Run(ctx); err != nil {
					klog.Error(err)
					cancel()
					return
				}
			},
			OnStoppedLeading: func() {
				//This node was leader and it lost the election.
				// Whenever the node transitions from leader to follower,
				// we need to handle the transition properly like clearing
				// the cache.
				klog.Infof("No longer leader; exiting")
				cancel()
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != master.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(ovnkubeMasterLeaderMetricsProvider{})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	master.wg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		klog.Infof("Stopped leader election")
		master.wg.Done()
	}()

	return nil
}

func (master *ovnkubeMaster) Run(ctx context.Context) error {
	// configure
	master.configureMetrics(master.stopChan)
	err := master.configureSCTPSupport()
	if err != nil {
		return err
	}
	master.configureMulticastSupport()
	err = master.enableOVNLogicalDataPathGroups()
	if err != nil {
		return err
	}

	// Run
	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(master.nbClient, master.kube, 10, time.Second*5, master.stopChan)
	}
	master.podRecorder.Run(master.sbClient, master.stopChan)

	err = master.watchFactory.Start()
	if err != nil {
		return err
	}

	err = master.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	if master.nadController != nil {
		klog.Infof("Starts net-attach-def controller")
		return master.nadController.Run(master.stopChan)
	}
	return nil
}

// Stop gracefully stops all managed controllers
func (master *ovnkubeMaster) Stop() {
	// stops the net-attach-def controller
	close(master.stopChan)

	// stops the default network controller
	master.defaultNetworkController.Stop()

	// then stops each network controller associated with net-attach-def; it is ok
	// to call GetAllControllers here as net-attach-def controller has been stopped,
	// and no more change of network controllers
	if master.nadController != nil {
		for _, oc := range master.nadController.GetAllNetworkControllers() {
			oc.Stop()
		}
	}
}

func (master *ovnkubeMaster) configureSCTPSupport() error {
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return err
	}

	if !hasSCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
	}
	master.SCTPSupport = hasSCTPSupport
	return nil
}

func (master *ovnkubeMaster) configureMulticastSupport() {
	master.multicastSupport = config.EnableMulticast
	if master.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			master.multicastSupport = false
		}
	}
}

// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
// understand it. Logical datapath groups reduce the size of the southbound
// database in large clusters. ovn-controllers should be upgraded to a version
// that supports them before the option is turned on by the master.
func (master *ovnkubeMaster) enableOVNLogicalDataPathGroups() error {
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{"use_logical_dp_groups": "true"},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(master.nbClient, &nbGlobal); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}
	return nil
}

func (master *ovnkubeMaster) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterMasterPerformance(master.nbClient)
	metrics.RegisterMasterFunctional()
	metrics.RunTimestamp(stopChan, master.sbClient, master.nbClient)
	metrics.MonitorIPSec(master.nbClient)
}

type ovnkubeMasterLeaderMetrics struct{}

func (ovnkubeMasterLeaderMetrics) On(string) {
	metrics.MetricMasterLeader.Set(1)
}

func (ovnkubeMasterLeaderMetrics) Off(string) {
	metrics.MetricMasterLeader.Set(0)
}

type ovnkubeMasterLeaderMetricsProvider struct{}

func (_ ovnkubeMasterLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeMasterLeaderMetrics{}
}
