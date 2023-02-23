package clustermanager

import (
	"context"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"
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

	// This lock protects accessing the Start and Stop function from different go routines.
	// Mostly needed for the unit test cases otherwise the test fails with the data race warning.
	// The warning is seen because 2 go routines call Start() and Stop().
	sync.Mutex
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
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager = newSecondaryNetworkClusterManager(ovnClient, wf, recorder)
	}
	return cm
}

// Start starts managing the cluster operations
// It starts the default network cluster controller
func (cm *ClusterManager) Start(ctx context.Context) error {
	cm.Lock()
	defer cm.Unlock()

	klog.Info("Starting the cluster manager")
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
		if err := cm.secondaryNetClusterManager.Start(); err != nil {
			return err
		}
	}

	return nil
}

// Stop stops managing the cluster operations by stopping the default
// network cluster controller
func (cm *ClusterManager) Stop() {
	cm.Lock()
	defer cm.Unlock()

	klog.Info("Stopping the cluster manager")
	metrics.UnregisterClusterManagerFunctional()
	cm.defaultNetClusterController.Stop()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager.Stop()
	}
}
