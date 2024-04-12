package clustermanager

import (
	"context"
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// ID of the default network.
	defaultNetworkID = 0
)

// ClusterManager structure is the object which manages the cluster nodes.
// It creates a default network controller for the default network and a
// secondary network cluster controller manager to manage the multi networks.
type ClusterManager struct {
	client                      clientset.Interface
	defaultNetClusterController *networkClusterController
	zoneClusterController       *zoneClusterController
	wf                          *factory.WatchFactory
	secondaryNetClusterManager  *secondaryNetworkClusterManager
	// Controller used for programming node allocation for egress IP
	// The OVN DB setup is handled by egressIPZoneController that runs in ovnkube-controller
	eIPC                    *egressIPClusterController
	egressServiceController *egressservice.Controller
	// Controller used for maintaining dns name resolver objects
	dnsNameResolverController *dnsnameresolver.Controller
	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// unique identity for clusterManager running on different ovnkube-cluster-manager instance,
	// used for leader election
	identity      string
	statusManager *status_manager.StatusManager
}

// NewClusterManager creates a new cluster manager to manage the cluster nodes.
func NewClusterManager(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory,
	identity string, wg *sync.WaitGroup, recorder record.EventRecorder) (*ClusterManager, error) {

	defaultNetClusterController := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, ovnClient, wf)

	zoneClusterController, err := newZoneClusterController(ovnClient, wf)
	if err != nil {
		return nil, fmt.Errorf("failed to create zone cluster controller, err : %w", err)
	}

	cm := &ClusterManager{
		client:                      ovnClient.KubeClient,
		defaultNetClusterController: defaultNetClusterController,
		zoneClusterController:       zoneClusterController,
		wf:                          wf,
		recorder:                    recorder,
		identity:                    identity,
		statusManager:               status_manager.NewStatusManager(wf, ovnClient),
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager, err = newSecondaryNetworkClusterManager(ovnClient, wf, recorder)
		if err != nil {
			return nil, err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC = newEgressIPController(ovnClient, wf, healthcheck.GetProvider(), recorder)
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		cm.egressServiceController, err = egressservice.NewController(ovnClient, wf, healthcheck.GetProvider())
		if err != nil {
			return nil, err
		}
	}
	if config.Kubernetes.OVNEmptyLbEvents {
		if _, err := unidling.NewUnidledAtController(&kube.Kube{KClient: ovnClient.KubeClient}, wf.ServiceInformer()); err != nil {
			return nil, err
		}
	}
	if util.IsDNSNameResolverEnabled() {
		cm.dnsNameResolverController = dnsnameresolver.NewController(ovnClient, wf)
	}
	return cm, nil
}

// Start the cluster manager.
func (cm *ClusterManager) Start(ctx context.Context) error {
	klog.Info("Starting the cluster manager")

	// Start and sync the watch factory to begin listening for events
	if err := cm.wf.Start(); err != nil {
		return err
	}

	if err := cm.defaultNetClusterController.Start(ctx); err != nil {
		return err
	}

	if err := cm.zoneClusterController.Start(ctx); err != nil {
		return fmt.Errorf("could not start zone controller, err: %w", err)
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		if err := cm.secondaryNetClusterManager.Start(); err != nil {
			return err
		}
	}

	err := healthcheck.Start(ctx, cm.wf.NodeCoreInformer())
	if err != nil {
		return err
	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		if err := cm.eIPC.Start(); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		if err := cm.egressServiceController.Start(1); err != nil {
			return err
		}
	}

	if err := cm.statusManager.Start(); err != nil {
		return err
	}

	if util.IsDNSNameResolverEnabled() {
		if err := cm.dnsNameResolverController.Start(); err != nil {
			return err
		}
	}
	return nil
}

// Stop the cluster manager.
func (cm *ClusterManager) Stop() {
	klog.Info("Stopping the cluster manager")
	cm.defaultNetClusterController.Stop()
	cm.zoneClusterController.Stop()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager.Stop()
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC.Stop()
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		cm.egressServiceController.Stop()
	}
	cm.statusManager.Stop()
	if util.IsDNSNameResolverEnabled() {
		cm.dnsNameResolverController.Stop()
	}
}
