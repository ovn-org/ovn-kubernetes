package ovnmanager

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

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type OvnManager interface {
	Start(ctx context.Context) error
	Stop()
	GetManagerWatchFactory() *factory.WatchFactory
	GetManagerEventRecorder() record.EventRecorder
}

const (
	ManagerTypeMaster                   = "ovn-kubernetes-master"
	ManagerTypeNetworkControllerManager = "ovn-kubernetes-network-controller-manager"
	ManagerTypeClusterManager           = "ovn-kubernetes-cluster-manager"
)

func NewOvnManager(managerType, identity string, ovnClientset *util.OVNClientset, wg *sync.WaitGroup,
	recorder record.EventRecorder) (OvnManager, error) {
	switch managerType {
	case ManagerTypeMaster:
		return newManagerTypeMaster(identity, ovnClientset, wg, recorder)

	case ManagerTypeNetworkControllerManager:
		return newManagerTypeNetworkControllerManager(identity, ovnClientset, wg, recorder)

	case ManagerTypeClusterManager:
		return newManagerTypeClusterManager(identity, ovnClientset, wg, recorder)
	}

	return nil, fmt.Errorf("invalid manager type - %s", managerType)
}

type ovnManagerBase struct {
	name            string
	identity        string
	ctx             context.Context
	cancelCtx       context.CancelFunc
	client          clientset.Interface
	recorder        record.EventRecorder
	wg              *sync.WaitGroup
	haConfig        config.HAConfig
	metricsProvider leaderelection.MetricsProvider
	leaderCallbacks leaderelection.LeaderCallbacks
	watchFactory    *factory.WatchFactory
	stopChan        chan struct{}
}

func (mb *ovnManagerBase) startLeaderElection(ctx context.Context) error {
	mb.ctx, mb.cancelCtx = context.WithCancel(ctx)
	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		mb.name,
		mb.client.CoreV1(),
		mb.client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      mb.identity,
			EventRecorder: mb.recorder,
		},
	)
	if err != nil {
		return err
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   time.Duration(mb.haConfig.ElectionLeaseDuration) * time.Second,
		RenewDeadline:   time.Duration(mb.haConfig.ElectionRenewDeadline) * time.Second,
		RetryPeriod:     time.Duration(mb.haConfig.ElectionRetryPeriod) * time.Second,
		ReleaseOnCancel: true,
		Callbacks:       mb.leaderCallbacks,
	}

	leaderelection.SetProvider(mb.metricsProvider)
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	mb.wg.Add(1)
	go func() {
		leaderElector.Run(mb.ctx)
		klog.Infof("Stopped leader election")
		mb.wg.Done()
	}()

	return nil
}

func (mb *ovnManagerBase) stop() {
	close(mb.stopChan)
	mb.cancelCtx()
	if mb.watchFactory != nil {
		mb.watchFactory.Shutdown()
	}
}

func (mb *ovnManagerBase) GetManagerWatchFactory() *factory.WatchFactory {
	return mb.watchFactory
}

func (mb *ovnManagerBase) GetManagerEventRecorder() record.EventRecorder {
	return mb.recorder
}
