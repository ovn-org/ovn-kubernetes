package multi_homing

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ControllerManager structure is the object manages all controllers for all networks
type ControllerManager struct {
	ovn.ControllerConnections

	// default wait group and stop channel, used by default network controller
	defaultWg       *sync.WaitGroup
	defaultStopChan chan struct{}

	// unique identity for controllerManager running on different ovnkube-master instance,
	// used for leader election
	identity string

	// indexed bag of holding for the Controller for all networks
	// This map is initially provisioned with the default network controller
	// and afterwards updated whenever a net-attach-def is added/deleted.
	// All these are serialized and no lock protection is needed.
	ovnControllers map[string]Controller
}

func NewControllerManager(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	stopChan chan struct{}, ovnNBClient libovsdbclient.Client, ovnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*ControllerManager, error) {
	podRecorder := metrics.NewPodRecorder()
	ovnkKubeClient := &kube.Kube{
		KClient:              ovnClient.KubeClient,
		EIPClient:            ovnClient.EgressIPClient,
		EgressFirewallClient: ovnClient.EgressFirewallClient,
		CloudNetworkClient:   ovnClient.CloudNetworkClient,
	}
	var controllerConnections *ovn.ControllerConnections
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return nil, err
	}

	if hasSCTPSupport {
		klog.Info("SCTP support detected in OVN")
		controllerConnections = ovn.NewOvnControllerConnectionManagerWithSCTPSupport(
			ovnClient.KubeClient,
			ovnkKubeClient,
			wf,
			recorder,
			&podRecorder,
			ovnNBClient,
			ovnSBClient,
		)
	} else {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
		controllerConnections = ovn.NewOvnControllerConnectionManager(
			ovnClient.KubeClient,
			ovnkKubeClient,
			wf,
			recorder,
			&podRecorder,
			ovnNBClient,
			ovnSBClient,
		)
	}
	return &ControllerManager{
		ControllerConnections: *controllerConnections,
		defaultWg:             wg,
		defaultStopChan:       stopChan,
		identity:              identity,
		ovnControllers:        make(map[string]Controller),
	}, nil
}

func (cm *ControllerManager) Start() error {
	if err := cm.compressSBDatabase(); err != nil {
		return err
	}
	cm.configureMetrics()
	cm.PodRecorder().Run(cm.SBClient(), cm.defaultStopChan)

	// Start and sync the watch factory to begin listening for events
	if err := cm.WatchFactory().Start(); err != nil {
		return err
	}

	// TODO: start the net-attach-def watchers

	return nil
}

func (cm *ControllerManager) compressSBDatabase() error {
	// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
	// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
	// understand it. Logical datapath groups reduce the size of the southbound
	// database in large clusters. ovn-controllers should be upgraded to a version
	// that supports them before the option is turned on by the master.
	if err := libovsdbops.UpdateNBGlobalSetOptions(
		cm.NBClient(),
		&nbdb.NBGlobal{
			Options: map[string]string{"use_logical_dp_groups": "true"},
		},
	); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}
	return nil
}

func (cm *ControllerManager) configureMetrics() {
	metrics.RunTimestamp(cm.defaultStopChan, cm.SBClient(), cm.NBClient())
	metrics.MonitorIPSec(cm.NBClient())
	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.NBClient(), cm.OvnkClient(), 10, time.Second*5, cm.defaultStopChan)
	}
}

func (cm *ControllerManager) DefaultNetworkController() (Controller, error) {
	const defaultControllerKey = "default"
	defaultNetworkController, isDefaultNetworkControllerReady := cm.ovnControllers[defaultControllerKey]
	if !isDefaultNetworkControllerReady {
		defaultNetworkOVNController, err := ovn.NewDefaultNetworkOVNController(
			cm.ControllerConnections,
			cm.defaultStopChan,
			cm.defaultWg,
			config.Default.RawClusterSubnets,
		)
		if err != nil {
			return nil, err
		}
		cm.ovnControllers[defaultControllerKey] = defaultNetworkOVNController
		return defaultNetworkOVNController, nil
	}
	return defaultNetworkController, nil
}

// ControlPlaneLeaderEntryPoint waits until this process is the leader before starting master functions
func (cm *ControllerManager) ControlPlaneLeaderEntryPoint(ctx context.Context, cancel context.CancelFunc) error {
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
		cm.K8sClient().CoreV1(),
		cm.K8sClient().CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      cm.identity,
			EventRecorder: cm.EventRecorder(),
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
				// run the cluster controller to init the master
				start := time.Now()
				defer func() {
					end := time.Since(start)
					metrics.MetricMasterReadyDuration.Set(end.Seconds())
				}()

				defaultNetworkController, err := cm.DefaultNetworkController()
				if err != nil {
					klog.Error(err)
					cancel()
					return
				}

				if err := cm.Start(); err != nil {
					klog.Error(err)
					cancel()
					return
				}

				if err := defaultNetworkController.StartClusterMaster(); err != nil {
					klog.Error(err)
					cancel()
					return
				}
				if err := defaultNetworkController.Run(ctx); err != nil {
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
				if newLeaderName != cm.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderelection.SetProvider(metrics.OvnkubeMasterLeaderMetricsProvider{})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	cm.defaultWg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		klog.Infof("Stopped leader election")
		cm.defaultWg.Done()
	}()

	return nil
}
