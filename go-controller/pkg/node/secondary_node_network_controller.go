package node

import (
	"context"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/openflowmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler

	networkID *int
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for default l3 network
func NewSecondaryNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, netInfo util.NetInfo, ofmContoller *openflowmanager.Controller) *SecondaryNodeNetworkController {
	return &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			NetInfo:                         netInfo,
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
			ofmController:                   ofmContoller,
		},
	}
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())

	handler, err := nc.watchPodsDPU()
	if err != nil {
		return err
	}
	nc.podHandler = handler
	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	return nil
}

func (oc *SecondaryNodeNetworkController) getNetworkID() (int, error) {
	if oc.networkID == nil || *oc.networkID == util.InvalidNetworkID {
		oc.networkID = ptr.To(util.InvalidNetworkID)
		nodes, err := oc.watchFactory.GetNodes()
		if err != nil {
			return util.InvalidNetworkID, err
		}
		*oc.networkID, err = util.GetNetworkID(nodes, oc.NetInfo)
		if err != nil {
			return util.InvalidNetworkID, err
		}
	}
	return *oc.networkID, nil
}
