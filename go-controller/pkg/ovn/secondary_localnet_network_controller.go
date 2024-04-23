package ovn

import (
	"context"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/pod"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/persistentips"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// SecondaryLocalnetNetworkController is created for logical network infrastructure and policy
// for a secondary localnet network
type SecondaryLocalnetNetworkController struct {
	BaseSecondaryLayer2NetworkController
}

// NewSecondaryLocalnetNetworkController create a new OVN controller for the given secondary localnet NAD
func NewSecondaryLocalnetNetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo) *SecondaryLocalnetNetworkController {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)
	oc := &SecondaryLocalnetNetworkController{
		BaseSecondaryLayer2NetworkController{
			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: BaseNetworkController{
					CommonNetworkControllerInfo: *cnci,
					controllerName:              getNetworkControllerName(netInfo.GetNetworkName()),
					NetInfo:                     netInfo,
					lsManager:                   lsm.NewL2SwitchManager(),
					logicalPortCache:            newPortCache(stopChan),
					namespaces:                  make(map[string]*namespaceInfo),
					namespacesMutex:             sync.Mutex{},
					addressSetFactory:           addressSetFactory,
					networkPolicies:             syncmap.NewSyncMap[*networkPolicy](),
					sharedNetpolPortGroups:      syncmap.NewSyncMap[*defaultDenyPortGroups](),
					podSelectorAddressSets:      syncmap.NewSyncMap[*PodSelectorAddressSet](),
					stopChan:                    stopChan,
					wg:                          &sync.WaitGroup{},
					cancelableCtx:               util.NewCancelableContext(),
					localZoneNodes:              &sync.Map{},
				},
			},
		},
	}

	if oc.allocatesPodAnnotation() {
		var claimsReconciler persistentips.PersistentAllocations
		if oc.allowPersistentIPs() {
			ipamClaimsReconciler := persistentips.NewIPAMClaimReconciler(
				oc.kube,
				oc.NetInfo,
				oc.watchFactory.IPAMClaimsInformer().Lister(),
			)
			oc.ipamClaimsReconciler = ipamClaimsReconciler
			claimsReconciler = ipamClaimsReconciler
		}
		oc.podAnnotationAllocator = pod.NewPodAnnotationAllocator(
			netInfo,
			cnci.watchFactory.PodCoreInformer().Lister(),
			cnci.kube,
			claimsReconciler)
	}

	// disable multicast support for secondary networks
	// TBD: changes needs to be made to support multicast in secondary networks
	oc.multicastSupport = false

	oc.BaseSecondaryLayer2NetworkController.initRetryFramework()
	return oc
}

// Start starts the secondary localnet controller, handles all events and creates all needed logical entities
func (oc *SecondaryLocalnetNetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting controller for secondary network network %s", oc.GetNetworkName())

	start := time.Now()
	defer func() {
		klog.Infof("Starting controller for secondary network network %s took %v", oc.GetNetworkName(), time.Since(start))
	}()

	if err := oc.Init(); err != nil {
		return err
	}

	return oc.run(ctx)
}

func (oc *SecondaryLocalnetNetworkController) run(ctx context.Context) error {
	return oc.BaseSecondaryLayer2NetworkController.run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLocalnetNetworkController) Cleanup(netName string) error {
	klog.Infof("Delete OVN logical entities for network %s", netName)
	return oc.BaseSecondaryLayer2NetworkController.cleanup(types.LocalnetTopology, netName)
}

func (oc *SecondaryLocalnetNetworkController) Init() error {
	switchName := oc.GetNetworkScopedName(types.OVNLocalnetSwitch)

	logicalSwitch, err := oc.initializeLogicalSwitch(switchName, oc.Subnets(), oc.ExcludeSubnets())
	if err != nil {
		return err
	}

	// Add external interface as a logical port to external_switch.
	// This is a learning switch port with "unknown" address. The external
	// world is accessed via this port.
	logicalSwitchPort := nbdb.LogicalSwitchPort{
		Name:      oc.GetNetworkScopedName(types.OVNLocalnetPort),
		Addresses: []string{"unknown"},
		Type:      "localnet",
		Options: map[string]string{
			"network_name": oc.GetNetworkName(),
		},
	}
	intVlanID := int(oc.Vlan())
	if intVlanID != 0 {
		logicalSwitchPort.TagRequest = &intVlanID
	}

	err = libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(oc.nbClient, logicalSwitch, &logicalSwitchPort)
	if err != nil {
		klog.Errorf("Failed to add logical port %+v to switch %s: %v", logicalSwitchPort, switchName, err)
		return err
	}

	return nil
}

func (oc *SecondaryLocalnetNetworkController) Stop() {
	klog.Infof("Stoping controller for secondary network %s", oc.GetNetworkName())
	oc.BaseSecondaryLayer2NetworkController.stop()
}
