package networkControllerManager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containernetworking/cni/pkg/types"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

type ovnkubeMasterLeaderMetrics struct{}

func (ovnkubeMasterLeaderMetrics) On(string) {
	metrics.MetricMasterLeader.Set(1)
}

func (ovnkubeMasterLeaderMetrics) Off(string) {
	metrics.MetricMasterLeader.Set(0)
}

type ovnkubeMasterLeaderMetricsProvider struct{}

func (ovnkubeMasterLeaderMetricsProvider) NewLeaderMetric() leaderelection.SwitchMetric {
	return ovnkubeMasterLeaderMetrics{}
}

// networkControllerManager structure is the object manages all controllers for all networks
type networkControllerManager struct {
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

	defaultNetworkController nad.BaseNetworkController

	// net-attach-def controller handle net-attach-def and create/delete network controllers
	nadController *nad.NetAttachDefinitionController
}

func (cm *networkControllerManager) NewNetworkController(nInfo util.NetInfo,
	netConfInfo util.NetConfInfo) (nad.NetworkController, error) {
	cnci := cm.newCommonNetworkControllerInfo()
	topoType := netConfInfo.TopologyType()
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, nInfo, netConfInfo), nil
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, nInfo, netConfInfo), nil
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, nInfo, netConfInfo), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// newDummyNetworkController creates a dummy network controller used to clean up specific network
func (cm *networkControllerManager) newDummyNetworkController(topoType, netName string) (nad.NetworkController, error) {
	cnci := cm.newCommonNetworkControllerInfo()
	netInfo := util.NewNetInfo(&ovncnitypes.NetConf{NetConf: types.NetConf{Name: netName}, Topology: topoType})
	switch topoType {
	case ovntypes.Layer3Topology:
		return ovn.NewSecondaryLayer3NetworkController(cnci, netInfo, &util.Layer3NetConfInfo{}), nil
	case ovntypes.Layer2Topology:
		return ovn.NewSecondaryLayer2NetworkController(cnci, netInfo, &util.Layer2NetConfInfo{}), nil
	case ovntypes.LocalnetTopology:
		return ovn.NewSecondaryLocalnetNetworkController(cnci, netInfo, &util.LocalnetNetConfInfo{}), nil
	}
	return nil, fmt.Errorf("topology type %s not supported", topoType)
}

// Find all the OVN logical switches/routers for the secondary networks
func findAllSecondaryNetworkLogicalEntities(nbClient libovsdbclient.Client) ([]*nbdb.LogicalSwitch,
	[]*nbdb.LogicalRouter, error) {
	p1 := func(item *nbdb.LogicalSwitch) bool {
		_, ok := item.ExternalIDs[ovntypes.NetworkExternalID]
		return ok
	}
	nodeSwitches, err := libovsdbops.FindLogicalSwitchesWithPredicate(nbClient, p1)
	if err != nil {
		klog.Errorf("Failed to get all logical switches of secondary network error: %v", err)
		return nil, nil, err
	}
	p2 := func(item *nbdb.LogicalRouter) bool {
		_, ok := item.ExternalIDs[ovntypes.NetworkExternalID]
		return ok
	}
	clusterRouters, err := libovsdbops.FindLogicalRoutersWithPredicate(nbClient, p2)
	if err != nil {
		klog.Errorf("Failed to get all distributed logical routers: %v", err)
		return nil, nil, err
	}
	return nodeSwitches, clusterRouters, nil
}

func (cm *networkControllerManager) CleanupDeletedNetworks(allControllers []nad.NetworkController) error {
	existingNetworksMap := map[string]struct{}{}
	for _, oc := range allControllers {
		existingNetworksMap[oc.GetNetworkName()] = struct{}{}
	}

	// Get all the existing secondary networks and its logical entities
	switches, routers, err := findAllSecondaryNetworkLogicalEntities(cm.nbClient)
	if err != nil {
		return err
	}

	staleNetworkControllers := map[string]nad.NetworkController{}
	for _, ls := range switches {
		netName := ls.ExternalIDs[ovntypes.NetworkExternalID]
		if _, ok := existingNetworksMap[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := ls.ExternalIDs[ovntypes.TopologyExternalID]
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}
	for _, lr := range routers {
		netName := lr.ExternalIDs[ovntypes.NetworkExternalID]
		if _, ok := existingNetworksMap[netName]; ok {
			// network still exists, no cleanup to do
			continue
		}
		// TopologyExternalID always co-exists with NetworkExternalID
		topoType := lr.ExternalIDs[ovntypes.TopologyExternalID]
		// Create dummy network controllers to clean up logical entities
		klog.V(5).Infof("Found stale %s network %s", topoType, netName)
		if oc, err := cm.newDummyNetworkController(topoType, netName); err == nil {
			staleNetworkControllers[netName] = oc
			continue
		}
	}

	for netName, oc := range staleNetworkControllers {
		klog.Infof("Cleanup entities for stale network %s", netName)
		err = oc.Cleanup(netName)
		if err != nil {
			klog.Errorf("Failed to delete stale OVN logical entities for network %s: %v", netName, err)
		}
	}
	return nil
}

// Start waits until this process is the leader before starting master functions
func (cm *networkControllerManager) Start(ctx context.Context, cancel context.CancelFunc) error {
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

				if err = cm.start(ctx); err != nil {
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
				// Note: If cluster manager LE is also running, it will
				// exit the cluster manager too.
				// TODO: Remove the leader election from cluster-manager and have one single election
				// when both cluster manager and network controller manager are running.
				// See https://issues.redhat.com/browse/OCPBUGS-8080 for details
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

	leaderelection.SetProvider(ovnkubeMasterLeaderMetricsProvider{})
	leaderElector, err := leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}

	cm.wg.Add(1)
	go func() {
		leaderElector.Run(ctx)
		klog.Infof("Stopped leader election")
		cm.wg.Done()
	}()

	return nil
}

// NewNetworkControllerManager creates a new OVN controller manager to manage all the controller for all networks
func NewNetworkControllerManager(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	libovsdbOvnNBClient libovsdbclient.Client, libovsdbOvnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) *networkControllerManager {
	podRecorder := metrics.NewPodRecorder()

	cm := &networkControllerManager{
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

		wg:       wg,
		identity: identity,
	}

	if config.OVNKubernetesFeature.EnableMultiNetwork {
		cm.nadController = nad.NewNetAttachDefinitionController("network-controller-manager", cm, ovnClient.NetworkAttchDefClient, cm.recorder)
	}
	return cm
}

func (cm *networkControllerManager) configureSCTPSupport() error {
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return err
	}

	if !hasSCTPSupport {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
	} else {
		klog.Info("SCTP support detected in OVN")
	}
	cm.SCTPSupport = hasSCTPSupport
	return nil
}

func (cm *networkControllerManager) configureMulticastSupport() {
	cm.multicastSupport = config.EnableMulticast
	if cm.multicastSupport {
		if _, _, err := util.RunOVNSbctl("--columns=_uuid", "list", "IGMP_Group"); err != nil {
			klog.Warningf("Multicast support enabled, however version of OVN in use does not support IGMP Group. " +
				"Disabling Multicast Support")
			cm.multicastSupport = false
		}
	}
}

// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
// understand it. Logical datapath groups reduce the size of the southbound
// database in large clusters. ovn-controllers should be upgraded to a version
// that supports them before the option is turned on by the master.
func (cm *networkControllerManager) enableOVNLogicalDataPathGroups() error {
	nbGlobal := nbdb.NBGlobal{
		Options: map[string]string{"use_logical_dp_groups": "true"},
	}
	if err := libovsdbops.UpdateNBGlobalSetOptions(cm.nbClient, &nbGlobal); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}
	return nil
}

func (cm *networkControllerManager) configureMetrics(stopChan <-chan struct{}) {
	metrics.RegisterMasterPerformance(cm.nbClient)
	metrics.RegisterMasterFunctional()
	metrics.RunTimestamp(stopChan, cm.sbClient, cm.nbClient)
	metrics.MonitorIPSec(cm.nbClient)
}

// newCommonNetworkControllerInfo creates and returns the common networkController info
func (cm *networkControllerManager) newCommonNetworkControllerInfo() *ovn.CommonNetworkControllerInfo {
	return ovn.NewCommonNetworkControllerInfo(cm.client, cm.kube, cm.watchFactory, cm.recorder, cm.nbClient,
		cm.sbClient, cm.podRecorder, cm.SCTPSupport, cm.multicastSupport)
}

// initDefaultNetworkController creates the controller for default network
func (cm *networkControllerManager) initDefaultNetworkController() {
	defaultController := ovn.NewDefaultNetworkController(cm.newCommonNetworkControllerInfo())
	cm.defaultNetworkController = defaultController
}

// start the network controller manager
func (cm *networkControllerManager) start(ctx context.Context) error {
	cm.configureMetrics(cm.stopChan)

	err := cm.configureSCTPSupport()
	if err != nil {
		return err
	}

	cm.configureMulticastSupport()

	err = cm.enableOVNLogicalDataPathGroups()
	if err != nil {
		return err
	}

	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.nbClient, cm.kube, 10, time.Second*5, cm.stopChan)
	}
	cm.podRecorder.Run(cm.sbClient, cm.stopChan)

	err = cm.watchFactory.Start()
	if err != nil {
		return err
	}

	cm.initDefaultNetworkController()
	err = cm.defaultNetworkController.Start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start default network controller: %v", err)
	}

	// nadController is nil if multi-network is disabled
	if cm.nadController != nil {
		return cm.nadController.Start()
	}

	return nil
}

// Stop gracefully stops all managed controllers
func (cm *networkControllerManager) Stop() {
	// stop metric recorders
	close(cm.stopChan)

	// stops the default network controller
	if cm.defaultNetworkController != nil {
		cm.defaultNetworkController.Stop()
	}

	// stops the NAD controller
	if cm.nadController != nil {
		cm.nadController.Stop()
	}
}
