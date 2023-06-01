package ovn

import (
	"context"
	"sync"

	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// SecondaryLayer2NetworkController is created for logical network infrastructure and policy
// for a secondary layer2 network
type SecondaryLayer2NetworkController struct {
	BaseSecondaryLayer2NetworkController
}

// NewSecondaryLayer2NetworkController create a new OVN controller for the given secondary layer2 nad
func NewSecondaryLayer2NetworkController(cnci *CommonNetworkControllerInfo, netInfo util.NetInfo) *SecondaryLayer2NetworkController {

	stopChan := make(chan struct{})

	ipv4Mode, ipv6Mode := netInfo.IPMode()
	addressSetFactory := addressset.NewOvnAddressSetFactory(cnci.nbClient, ipv4Mode, ipv6Mode)
	oc := &SecondaryLayer2NetworkController{
		BaseSecondaryLayer2NetworkController{
			BaseSecondaryNetworkController: BaseSecondaryNetworkController{
				BaseNetworkController: *NewBaseNetworkController(
					cnci,
					addressSetFactory,
					netInfo,
					lsm.NewL2SwitchManager(),
					stopChan,
					&sync.WaitGroup{},
					false,
				),
			},
		},
	}

	// disable multicast support for secondary networks
	// TBD: changes needs to be made to support multicast in secondary networks
	oc.multicastSupport = false

	oc.initRetryFramework()
	return oc
}

// Start starts the secondary layer2 controller, handles all events and creates all needed logical entities
func (oc *SecondaryLayer2NetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary %s network controller of network %s", oc.TopologyType(), oc.GetNetworkName())
	if err := oc.Init(); err != nil {
		return err
	}

	return oc.Run()
}

// Cleanup cleans up logical entities for the given network, called from net-attach-def routine
// could be called from a dummy Controller (only has CommonNetworkControllerInfo set)
func (oc *SecondaryLayer2NetworkController) Cleanup(netName string) error {
	return oc.cleanup(types.Layer2Topology, netName)
}

func (oc *SecondaryLayer2NetworkController) Init() error {
	switchName := oc.GetNetworkScopedName(types.OVNLayer2Switch)

	_, err := oc.InitializeLogicalSwitch(switchName, oc.Subnets(), oc.ExcludeSubnets())
	return err
}
