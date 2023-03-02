package clustermanager

import (
	"context"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ClusterManager structure is the object which manages the cluster nodes.
// It creates a default network controller for the default network and a
// secondary network cluster controller manager to manage the multi networks.
type ClusterManager struct {
	client                      clientset.Interface
	defaultNetClusterController *networkClusterController
	wf                          *factory.WatchFactory
	wg                          *sync.WaitGroup
	secondaryNetClusterManager  *secondaryNetworkClusterManager
	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// unique identity for clusterManager running on different ovnkube-cluster-manager instance,
	// used for leader election
	identity string

	// Indicates if cluster manager is leader and it is active or not.  This variable is protected
	// by the mutex and is modified by only  ClusterManager.run() and ClusterManager.halt().
	// Once 'isActive' is set, calling ClusterManager.run() will be a no-op and once
	// it is false, calling ClusterManager.halt() will be a no-op.
	// ClusterManager.halt() is called from two places
	//    - ClusterManagber.Stop()
	//    - and when ClusterManager LE loses the lock and become standby
	// and 'isActive' makes sure that ClusterManager.halt() is idempotent.
	isActive   bool
	sync.Mutex // This lock protects accessing the run and halt function from different go routines
}

// NewClusterManager creates a new Cluster Manager for managing the
// cluster nodes.
func NewClusterManager(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory,
	identity string, wg *sync.WaitGroup, recorder record.EventRecorder) *ClusterManager {
	defaultNetClusterController := newNetworkClusterController(ovntypes.DefaultNetworkName, config.Default.ClusterSubnets,
		ovnClient, wf, config.HybridOverlay.Enabled, &util.DefaultNetInfo{}, &util.DefaultNetConfInfo{})
	cm := &ClusterManager{
		client:                      ovnClient.KubeClient,
		defaultNetClusterController: defaultNetClusterController,
		wg:                          wg,
		wf:                          wf,
		recorder:                    recorder,
		identity:                    identity,
		isActive:                    false,
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager = newSecondaryNetworkClusterManager(ovnClient, wf, recorder)
	}
	return cm
}

// Start waits until this process is the leader before starting the cluster manager functions
func (cm *ClusterManager) Start(ctx context.Context, cancel context.CancelFunc) error {
	metrics.RegisterClusterManagerBase()

	// Set up leader election process first.
	// User lease resource lock as configmap and endpoint lock support is removed from leader election library.
	// TODO: Remove the leader election from cluster-manager and have one single election
	// when both cluster manager and network controller manager are running.
	// See https://issues.redhat.com/browse/OCPBUGS-8080 for details
	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		"ovn-kubernetes-cluster-manager",
		cm.client.CoreV1(),
		cm.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      cm.identity,
			EventRecorder: cm.recorder,
		},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(config.ClusterMgrHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(config.ClusterMgrHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(config.ClusterMgrHA.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Infof("Won leader election; in active mode")
				// run only on the active master node
				if err := cm.run(ctx, cancel); err != nil {
					klog.Error(err)
					cancel()
				}
			},
			OnStoppedLeading: func() {
				klog.Infof("No longer leader; transitioning to standby mode")
				// This node was leader and it lost the election.
				// Call halt() to stop all the cluster manager responsibilities
				// and become a standby. This doesn't exit the process because
				//    - network cluster manager may also be running in an another
				//      leader election session and we don't want to disrupt it.
				//    - ClusterManager run() can be called again if it starts
				//      leading again.
				if err := cm.halt(); err != nil {
					klog.Error(err)
					cancel()
				}
			},
			OnNewLeader: func(newLeaderName string) {
				if newLeaderName != cm.identity {
					klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
				}
			},
		},
	}

	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	cm.wg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		cm.wg.Done()
	}()

	return nil
}

// Stop stops the cluster manager if it is active
func (cm *ClusterManager) Stop() {
	if err := cm.halt(); err != nil {
		klog.Error(err)
	}
}

// run starts managing the cluster operations
// It starts the default network cluster controller
func (cm *ClusterManager) run(ctx context.Context, cancel context.CancelFunc) error {
	cm.Lock()
	defer cm.Unlock()
	if cm.isActive {
		// Is already active and nothing to do
		return nil
	}

	klog.Info("Starting the cluster manager active operations")
	metrics.RegisterClusterManagerFunctional()

	start := time.Now()
	defer func() {
		end := time.Since(start)
		metrics.MetricClusterManagerReadyDuration.Set(end.Seconds())
	}()

	// Start and sync the watch factory to begin listening for events
	if err := cm.wf.Start(); err != nil {
		return err
	}

	if err := cm.defaultNetClusterController.Start(ctx); err != nil {
		return err
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		if err := cm.secondaryNetClusterManager.Start(cancel); err != nil {
			return err
		}
	}

	cm.isActive = true
	return nil
}

// halt stops managing the cluster operations by stopping the default
// network cluster controller
func (cm *ClusterManager) halt() error {
	cm.Lock()
	defer cm.Unlock()
	if !cm.isActive {
		// Is not active.  Nothing to halt.
		return nil
	}

	klog.Info("Halting the cluster manager operations")
	metrics.UnregisterClusterManagerFunctional()
	cm.defaultNetClusterController.Stop()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager.Stop()
	}

	cm.isActive = false
	return nil
}
