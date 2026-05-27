// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package clustermanager

import (
	"context"
	"fmt"
	"net"

	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/dnsnameresolver"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/egressservice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/endpointslicemirror"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/managedbgp"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/networkconnect"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/nooverlay"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/routeadvertisements"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager"
	uplinkcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/uplink"
	udncontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork"
	udntemplate "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	vtepcontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/vtep"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	nodecontroller "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controllers/node"
	networkconnectclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned"
	rainformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/informers/externalversions/routeadvertisements/v1"
	vtepinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/unidling"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// ClusterManager structure is the object which manages the cluster nodes.
// It creates a default network controller for the default network and a
// user-defined network cluster controller manager to manage the multi networks.
type ClusterManager struct {
	client                      clientset.Interface
	defaultNetClusterController *networkClusterController
	nodeController              *nodecontroller.NodeController
	nodeAllocationController    *nodeAllocationController
	wf                          *factory.WatchFactory
	udnClusterManager           *userDefinedNetworkClusterManager
	// Controller used for programming node allocation for egress IP
	// The OVN DB setup is handled by egressIPZoneController that runs in ovnkube-controller
	eIPC                          *egressIPClusterController
	egressServiceController       *egressservice.Controller
	endpointSliceMirrorController *endpointslicemirror.Controller
	// Controller used for maintaining dns name resolver objects
	dnsNameResolverController *dnsnameresolver.Controller
	// Controller for managing user-defined-network CRD
	userDefinedNetworkController *udncontroller.Controller
	// Controller for managing cluster-network-connect CRD
	networkConnectController *networkconnect.Controller
	// Controller for managing uplink CRDs
	uplinkController *uplinkcontroller.Controller
	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// unique identity for clusterManager running on different ovnkube-cluster-manager instance,
	// used for leader election
	identity      string
	statusManager *status_manager.StatusManager

	// networkManager creates and deletes network controllers
	networkManager networkmanager.Controller

	raController         *routeadvertisements.Controller
	noOverlayController  *nooverlay.Controller
	managedBGPController *managedbgp.Controller
	vtepController       *vtepcontroller.Controller
}

// NewClusterManager creates a new cluster manager to manage the cluster nodes.
func NewClusterManager(
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory,
	identity string,
	recorder record.EventRecorder,
) (*ClusterManager, error) {

	wf = wf.ShallowClone()
	cm := &ClusterManager{
		client:         ovnClient.KubeClient,
		wf:             wf,
		recorder:       recorder,
		identity:       identity,
		networkManager: networkmanager.Default(),
	}

	var (
		err                 error
		tunnelKeysAllocator *id.TunnelKeysAllocator
	)
	if config.OVNKubernetesFeature.EnableMultiNetwork {
		// tunnelKeysAllocator is now only used for NAD tunnel keys allocation, but will be reused
		// for Connecting UDNs. So we initialize it here and pass it to the networkManager.
		// The same instance should be initialized only once and passed to all the
		// users of tunnel-keys.
		tunnelKeysAllocator, err = initTunnelKeysAllocator(ovnClient.NetworkAttchDefClient, ovnClient.NetworkConnectClient)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize tunnel keys allocator: %w", err)
		}

		cm.networkManager, err = networkmanager.NewForCluster(cm, wf, ovnClient, recorder, tunnelKeysAllocator)
		if err != nil {
			return nil, err
		}
	}
	cm.statusManager = status_manager.NewStatusManager(wf, ovnClient, cm.networkManager.Interface())

	nodeController := nodecontroller.NewController(wf, "clustermanager-node", cm.networkManager.Interface())
	defaultNetClusterController := newDefaultNetworkClusterController(&util.DefaultNetInfo{}, ovnClient, wf, recorder, nodeController)
	nodeAllocationController, err := newNodeAllocationController(ovnClient, wf)
	if err != nil {
		return nil, fmt.Errorf("failed to create node allocation controller, err : %w", err)
	}
	cm.defaultNetClusterController = defaultNetClusterController
	cm.nodeController = nodeController
	cm.nodeAllocationController = nodeAllocationController

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.udnClusterManager, err = newUserDefinedNetworkClusterManager(ovnClient, wf, cm.networkManager.Interface(), recorder, nodeController)
		if err != nil {
			return nil, err
		}
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
		var vtepInformer vtepinformer.VTEPInformer
		if util.IsEVPNEnabled() {
			vtepInformer = wf.VTEPInformer()
		}
		// RouteAdvertisements informer for no-overlay transport validation
		var raInformer rainformer.RouteAdvertisementsInformer
		if util.IsRouteAdvertisementsEnabled() {
			raInformer = wf.RouteAdvertisementsInformer()
		}
		udnController := udncontroller.New(
			ovnClient.NetworkAttchDefClient, wf.NADInformer(),
			ovnClient.UserDefinedNetworkClient,
			wf.UserDefinedNetworkInformer(), wf.ClusterUserDefinedNetworkInformer(),
			udntemplate.RenderNetAttachDefManifest,
			cm.networkManager.Interface(),
			wf.PodCoreInformer(),
			wf.NamespaceInformer(),
			vtepInformer,
			raInformer,
			cm.recorder,
		)
		cm.userDefinedNetworkController = udnController
		if cm.udnClusterManager != nil {
			cm.udnClusterManager.SetNetworkStatusReporter(udnController.UpdateSubsystemCondition)
		}
	}

	if util.IsNetworkConnectEnabled() {
		cm.networkConnectController = networkconnect.NewController(wf, ovnClient, cm.networkManager.Interface(), tunnelKeysAllocator)
	}
	if util.IsUplinkEnabled() {
		cm.uplinkController = uplinkcontroller.NewController(
			wf,
			ovnClient,
			cm.networkManager.Interface(),
		)
	}

	if util.IsRouteAdvertisementsEnabled() {
		cm.raController = routeadvertisements.NewController(cm.networkManager.Interface(), wf, ovnClient)
		if config.ManagedBGP.FRRNamespace != "" {
			var err error
			cm.managedBGPController, err = managedbgp.NewController(wf, ovnClient.FRRClient, ovnClient.RouteAdvertisementsClient, ovnClient.UserDefinedNetworkClient)
			if err != nil {
				return nil, fmt.Errorf("failed to create managed BGP controller: %w", err)
			}
		}
	}

	// create the no-overlay validation controller even when
	// RouteAdvertisements are disabled, so that it can validate that routing
	// is unmanaged, the only no-overlay mode that works without
	// RouteAdvertisements
	if config.Default.Transport == types.NetworkTransportNoOverlay {
		cm.noOverlayController = nooverlay.NewController(wf, recorder)
	}

	if util.IsEVPNEnabled() {
		cm.vtepController = vtepcontroller.NewController(wf, ovnClient, recorder)
	}

	return cm, nil
}

// Start the cluster manager.
func (cm *ClusterManager) Start(ctx context.Context) error {
	klog.Info("Starting the cluster manager")
	config.Layer2UsesTransitRouter = true

	// Start and sync the watch factory to begin listening for events
	if err := cm.wf.Start(); err != nil {
		return err
	}

	// Start networkManager before other controllers
	if err := cm.networkManager.Start(); err != nil {
		return err
	}

	if err := cm.nodeController.Start(); err != nil {
		return err
	}

	if err := cm.defaultNetClusterController.Start(ctx); err != nil {
		return err
	}

	if err := cm.nodeAllocationController.Start(ctx); err != nil {
		return fmt.Errorf("could not start node allocation controller, err: %w", err)
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
		// Use 5 workers to match the upstream kube-controller-manager default for EndpointSlice reconciliation:
		// https://github.com/kubernetes/kubernetes/blob/462e759d1995c143fda094cd7f591b10fd8cdee6/pkg/controller/endpointslice/config/v1alpha1/defaults.go#L35
		if err := cm.endpointSliceMirrorController.Start(ctx, 5); err != nil {
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

	if cm.networkConnectController != nil {
		if err := cm.networkConnectController.Start(); err != nil {
			return err
		}
	}
	if util.IsUplinkEnabled() {
		if err := cm.uplinkController.Start(); err != nil {
			return err
		}
	}

	if cm.raController != nil {
		if cm.managedBGPController != nil {
			if err := cm.managedBGPController.Start(); err != nil {
				return err
			}
		}
		err := cm.raController.Start()
		if err != nil {
			return err
		}
	}

	// Start no-overlay validation controller
	if cm.noOverlayController != nil {
		err := cm.noOverlayController.Start()
		if err != nil {
			return err
		}
	}

	if cm.vtepController != nil {
		if err := cm.vtepController.Start(); err != nil {
			return err
		}
	}

	return nil
}

// Stop the cluster manager.
func (cm *ClusterManager) Stop() {
	klog.Info("Stopping the cluster manager")
	cm.defaultNetClusterController.Stop()
	cm.nodeAllocationController.Stop()

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
	if cm.networkConnectController != nil {
		cm.networkConnectController.Stop()
	}
	if util.IsUplinkEnabled() {
		cm.uplinkController.Stop()
	}
	if cm.vtepController != nil {
		cm.vtepController.Stop()
		cm.vtepController = nil
	}
	if cm.raController != nil {
		if cm.managedBGPController != nil {
			cm.managedBGPController.Stop()
		}
		cm.raController.Stop()
		cm.raController = nil
	}
	if cm.noOverlayController != nil {
		cm.noOverlayController.Stop()
		cm.noOverlayController = nil
	}
	cm.nodeController.Stop()
}

func (cm *ClusterManager) NewNetworkController(netInfo util.NetInfo) (networkmanager.NetworkController, error) {
	return cm.udnClusterManager.NewNetworkController(netInfo)
}

func (cm *ClusterManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return cm.defaultNetClusterController
}

func (cm *ClusterManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	return cm.udnClusterManager.CleanupStaleNetworks(validNetworks...)
}

func (cm *ClusterManager) Reconcile(name string, old, new util.NetInfo) error {
	if cm.raController != nil {
		cm.raController.ReconcileNetwork(name, old, new)
	}
	return nil
}

// initTunnelKeysAllocator reserves any existing tunnel keys to avoid re-allocation.
// It will be shared across multiple controllers and should account for different object types.
// Good news is that we don't care about missing events, because we only need to reserve ids that are already
// annotated, and no one else can annotate them except ClusterManager.
func initTunnelKeysAllocator(nadClient networkattchmentdefclientset.Interface, cncClient networkconnectclientset.Interface) (*id.TunnelKeysAllocator, error) {
	tunnelKeysAllocator := id.NewTunnelKeyAllocator("TunnelKeys")

	existingNADs, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list existing NADs: %w", err)
	}
	for _, nad := range existingNADs.Items {
		// reserve tunnel keys that are already allocated to make sure they are
		if nad.Annotations[types.OvnNetworkTunnelKeysAnnotation] != "" {
			netconf, err := util.ParseNetConf(&nad)
			if err != nil {
				// ignore non-OVN NADs; otherwise log and continue
				if err.Error() == util.ErrorAttachDefNotOvnManaged.Error() {
					continue
				}
				klog.Warningf("Failed to parse NAD config %s: %v", nad.Name, err)
				continue
			}
			networkName := netconf.Name
			tunnelKeys, err := util.ParseTunnelKeysAnnotation(nad.Annotations[types.OvnNetworkTunnelKeysAnnotation])
			if err != nil {
				return nil, fmt.Errorf("failed to parse annotated tunnel keys: %w", err)
			}
			if err = tunnelKeysAllocator.ReserveKeys(networkName, tunnelKeys); err != nil {
				return nil, fmt.Errorf("failed to reserve tunnel keys %v for network %s: %w", tunnelKeys, networkName, err)
			}
		}
	}
	if util.IsNetworkConnectEnabled() {
		existingCNCs, err := cncClient.K8sV1().ClusterNetworkConnects().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list existing CNCs: %w", err)
		}
		for _, cnc := range existingCNCs.Items {
			tunnelID, err := util.ParseNetworkConnectTunnelKeyAnnotation(&cnc)
			if err != nil {
				return nil, fmt.Errorf("failed to parse annotated tunnel ID: %w", err)
			}
			if tunnelID != 0 {
				if err = tunnelKeysAllocator.ReserveKeys(cnc.Name, []int{tunnelID}); err != nil {
					return nil, fmt.Errorf("failed to reserve tunnel ID %d for CNC %s: %w", tunnelID, cnc.Name, err)
				}
			}
		}
	}
	return tunnelKeysAllocator, nil
}
