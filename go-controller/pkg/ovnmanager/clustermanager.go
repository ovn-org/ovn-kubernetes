package ovnmanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managerTypeClusterManager struct {
	ovnManagerBase
	clusterManager *clustermanager.ClusterManager
}

func newManagerTypeClusterManager(identity string, ovnClientset *util.OVNClientset, wg *sync.WaitGroup,
	recorder record.EventRecorder) (OvnManager, error) {

	var err error

	stopChan := make(chan struct{})
	defer func() {
		if err != nil {
			close(stopChan)
		}
	}()

	cmWatchFactory, err := factory.NewClusterManagerWatchFactory(ovnClientset.GetClusterManagerClientset())
	if err != nil {
		return nil, fmt.Errorf("error creating clustermanager watch factory : %w", err)
	}

	cmEventRecorder := util.EventRecorder(ovnClientset.KubeClient)

	clusterManager := clustermanager.NewClusterManager(ovnClientset.GetClusterManagerClientset(), cmWatchFactory,
		identity, wg, cmEventRecorder)
	mtcm := &managerTypeClusterManager{
		ovnManagerBase: ovnManagerBase{
			name:            ManagerTypeClusterManager,
			identity:        identity,
			wg:              wg,
			client:          ovnClientset.KubeClient,
			recorder:        recorder,
			haConfig:        config.ClusterMgrHA,
			metricsProvider: ovnkubeClusterManagerLeaderMetricsProvider{},
			watchFactory:    cmWatchFactory,
			stopChan:        stopChan,
		},
		clusterManager: clusterManager,
	}

	mtcm.leaderCallbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			klog.Infof("Won leader election; in active mode")
			klog.Infof("Starting cluster manager")
			start := time.Now()
			defer func() {
				end := time.Since(start)
				metrics.MetricClusterManagerReadyDuration.Set(end.Seconds())
			}()

			// Start cluster manager
			if err := mtcm.clusterManager.Start(ctx); err != nil {
				klog.Error(err)
				mtcm.cancelCtx()
			}
		},
		OnStoppedLeading: func() {
			// This node was leader and it lost the election.
			klog.Infof("No longer leader; exiting")
			mtcm.cancelCtx()

		},
		OnNewLeader: func(newLeaderName string) {
			if newLeaderName != identity {
				klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
			}
		},
	}

	return mtcm, nil
}

func (mtcm *managerTypeClusterManager) Start(ctx context.Context) error {
	// register prometheus metrics that do not depend on becoming ovnkube-master leader
	metrics.RegisterClusterManagerBase()

	// Start the leader election
	return mtcm.startLeaderElection(ctx)
}

func (mtcm *managerTypeClusterManager) Stop() {
	mtcm.stop()
	mtcm.clusterManager.Stop()
}

// ovnkubeClusterManagerLeaderMetrics object is used for the cluster manager
// leader election metrics
type ovnkubeClusterManagerLeaderMetrics struct{}

// On will be called by leader election (LE) when cluster manager becomes a leader
func (ovnkubeClusterManagerLeaderMetrics) On(string) {
	metrics.MetricClusterManagerLeader.Set(1)
}

// Off will be called by LE when cluster manager becomes a follower
func (ovnkubeClusterManagerLeaderMetrics) Off(string) {
	metrics.MetricClusterManagerLeader.Set(0)
}

// ovnkubeClusterManagerLeaderMetricsProvider is used by LE
type ovnkubeClusterManagerLeaderMetricsProvider struct{}

// ovnkubeClusterManagerLeaderMetricsProvider is called by LE to create an instance of
// ovnkubeClusterManagerLeaderMetrics
func (_ ovnkubeClusterManagerLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeClusterManagerLeaderMetrics{}
}
