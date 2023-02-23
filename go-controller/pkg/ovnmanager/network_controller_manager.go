package ovnmanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	controllerManager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-controller-manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type managerTypeNetworkControllerManager struct {
	ovnManagerBase
	networkControllerManager *controllerManager.NetworkControllerManager
}

func newManagerTypeNetworkControllerManager(identity string, ovnClientset *util.OVNClientset, wg *sync.WaitGroup,
	recorder record.EventRecorder) (OvnManager, error) {

	var err error

	stopChan := make(chan struct{})
	defer func() {
		if err != nil {
			close(stopChan)
		}
	}()

	var masterWatchFactory *factory.WatchFactory
	masterWatchFactory, err = factory.NewMasterWatchFactory(ovnClientset.GetMasterClientset())
	if err != nil {
		return nil, fmt.Errorf("error creating master watch factory : %w", err)
	}

	var libovsdbOvnNBClient, libovsdbOvnSBClient libovsdbclient.Client

	if libovsdbOvnNBClient, err = libovsdb.NewNBClient(stopChan); err != nil {
		return nil, fmt.Errorf("error when trying to initialize libovsdb NB client: %v", err)
	}

	if libovsdbOvnSBClient, err = libovsdb.NewSBClient(stopChan); err != nil {
		return nil, fmt.Errorf("error when trying to initialize libovsdb SB client: %v", err)
	}

	masterEventRecorder := util.EventRecorder(ovnClientset.KubeClient)
	networkControllerManager := controllerManager.NewNetworkControllerManager(ovnClientset, identity,
		masterWatchFactory, libovsdbOvnNBClient, libovsdbOvnSBClient, masterEventRecorder, wg)

	mtncm := &managerTypeNetworkControllerManager{
		ovnManagerBase: ovnManagerBase{
			name:            ManagerTypeMaster,
			identity:        identity,
			wg:              wg,
			client:          ovnClientset.KubeClient,
			recorder:        recorder,
			haConfig:        config.MasterHA,
			metricsProvider: ovnkubeNetworkControllerManagerLeaderMetricsProvider{},
			watchFactory:    masterWatchFactory,
			stopChan:        stopChan,
		},
		networkControllerManager: networkControllerManager,
	}

	mtncm.leaderCallbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			klog.Infof("Won leader election; in active mode")
			klog.Infof("Starting network controller manager")
			start := time.Now()
			defer func() {
				end := time.Since(start)
				metrics.MetricMasterReadyDuration.Set(end.Seconds())
			}()

			// Start network controller manager
			if err := mtncm.networkControllerManager.Start(ctx); err != nil {
				klog.Error(err)
				mtncm.cancelCtx()
				return
			}
		},
		OnStoppedLeading: func() {
			//This node was leader and it lost the election.
			// Whenever the node transitions from leader to follower,
			// we need to handle the transition properly like clearing
			// the cache.
			klog.Infof("No longer leader; exiting")
			mtncm.cancelCtx()

		},
		OnNewLeader: func(newLeaderName string) {
			if newLeaderName != identity {
				klog.Infof("Lost the election to %s; in standby mode", newLeaderName)
			}
		},
	}

	return mtncm, nil
}

func (mtm *managerTypeNetworkControllerManager) Start(ctx context.Context) error {
	// register prometheus metrics that do not depend on becoming ovnkube-master leader
	metrics.RegisterMasterBase()

	// Start the leader election
	return mtm.startLeaderElection(ctx)
}

func (mtcm *managerTypeNetworkControllerManager) Stop() {
	mtcm.stop()
	mtcm.networkControllerManager.Stop()
}

type ovnkubeNetworkControllerManagerLeaderMetrics struct{}

func (ovnkubeNetworkControllerManagerLeaderMetrics) On(string) {
	metrics.MetricMasterLeader.Set(1)
}

func (ovnkubeNetworkControllerManagerLeaderMetrics) Off(string) {
	metrics.MetricMasterLeader.Set(0)
}

type ovnkubeNetworkControllerManagerLeaderMetricsProvider struct{}

func (_ ovnkubeNetworkControllerManagerLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeNetworkControllerManagerLeaderMetrics{}
}
