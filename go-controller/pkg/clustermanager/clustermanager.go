package clustermanager

import (
	"context"
	"fmt"
	"net"
	"sync"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/endpointslicemirror"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/routeadvertisements"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager"
	udncontroller "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork"
	udntemplate "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
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
	eIPC                          *egressIPClusterController
	egressServiceController       *egressservice.Controller
	endpointSliceMirrorController *endpointslicemirror.Controller
	// Controller used for maintaining dns name resolver objects
	dnsNameResolverController *dnsnameresolver.Controller
	// Controller for managing user-defined-network CRD
	userDefinedNetworkController *udncontroller.Controller
	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// unique identity for clusterManager running on different ovnkube-cluster-manager instance,
	// used for leader election
	identity      string
	statusManager *status_manager.StatusManager

	// networkManager creates and deletes network controllers
	networkManager networkmanager.Controller

	raController *routeadvertisements.Controller
}

// NewClusterManager creates a new cluster manager to manage the cluster nodes.
func NewClusterManager(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory,
	identity string, wg *sync.WaitGroup, recorder record.EventRecorder) (*ClusterManager, error) {

	defaultNetClusterController := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, ovnClient, wf, recorder)

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

	cm.networkManager = networkmanager.Default()
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.secondaryNetClusterManager, err = newSecondaryNetworkClusterManager(ovnClient, wf, cm.networkManager.Interface(), recorder)
		if err != nil {
			return nil, err
		}

		cm.networkManager, err = networkmanager.NewForCluster(cm.secondaryNetClusterManager, wf, ovnClient, recorder)
		if err != nil {
			return nil, err
		}
		cm.secondaryNetClusterManager.networkManager = cm.networkManager.Interface()

	}

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC = newEgressIPController(ovnClient, wf, recorder)
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		// TODO: currently an ugly hack to pass the (copied) isReachable func to the egress service controller
		// without touching the egressIP controller code too much before the Controller object is created.
		// This will be removed once we consolidate all of the healthchecks to a different place and have
		// the controllers query a universal cache instead of creating multiple goroutines that do the same thing.
		timeout := config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout
		hcPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
		isReachable := func(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool {
			// Check if we need to do node reachability check
			if timeout == 0 {
				return true
			}

			if hcPort == 0 {
				return isReachableLegacy(nodeName, mgmtIPs, timeout)
			}

			return isReachableViaGRPC(mgmtIPs, healthClient, hcPort, timeout)
		}

		cm.egressServiceController, err = egressservice.NewController(ovnClient, wf, isReachable)
		if err != nil {
			return nil, err
		}
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.endpointSliceMirrorController, err = endpointslicemirror.NewController(ovnClient, wf, cm.networkManager.Interface())
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

	if util.IsNetworkSegmentationSupportEnabled() {
		udnController := udncontroller.New(
			ovnClient.NetworkAttchDefClient, wf.NADInformer(),
			ovnClient.UserDefinedNetworkClient,
			wf.UserDefinedNetworkInformer(), wf.ClusterUserDefinedNetworkInformer(),
			udntemplate.RenderNetAttachDefManifest,
			wf.PodCoreInformer(),
			wf.NamespaceInformer(),
			cm.recorder,
		)
		cm.userDefinedNetworkController = udnController
		if cm.secondaryNetClusterManager != nil {
			cm.secondaryNetClusterManager.SetNetworkStatusReporter(udnController.UpdateSubsystemCondition)
		}
	}

	if util.IsRouteAdvertisementsEnabled() {
		cm.raController = routeadvertisements.NewController(cm.networkManager.Interface(), wf, ovnClient)
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

	// Start networkManager before other controllers
	if err := cm.networkManager.Start(); err != nil {
		return err
	}

	if err := cm.defaultNetClusterController.Start(ctx); err != nil {
		return err
	}

	if err := cm.zoneClusterController.Start(ctx); err != nil {
		return fmt.Errorf("could not start zone controller, err: %w", err)
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

	if util.IsNetworkSegmentationSupportEnabled() {
		if err := cm.endpointSliceMirrorController.Start(ctx, 1); err != nil {
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

	if util.IsNetworkSegmentationSupportEnabled() {
		if err := cm.userDefinedNetworkController.Run(); err != nil {
			return err
		}
	}

	if cm.raController != nil {
		err := cm.raController.Start()
		if err != nil {
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

	if config.OVNKubernetesFeature.EnableEgressIP {
		cm.eIPC.Stop()
	}
	if config.OVNKubernetesFeature.EnableEgressService {
		cm.egressServiceController.Stop()
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.endpointSliceMirrorController.Stop()
	}
	cm.statusManager.Stop()
	if util.IsDNSNameResolverEnabled() {
		cm.dnsNameResolverController.Stop()
	}
	if util.IsNetworkSegmentationSupportEnabled() {
		cm.userDefinedNetworkController.Shutdown()
	}
	if cm.raController != nil {
		cm.raController.Stop()
		cm.raController = nil
	}
}
