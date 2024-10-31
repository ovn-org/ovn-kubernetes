package networkAttachDefController

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// networkManager is a level driven controller that handles creating, starting,
// stopping and cleaning up network controllers
type networkManager interface {
	// EnsureNetwork will add the network controller for the provided network
	// configuration. If a controller already exists for the same network with a
	// different configuration, the existing controller is stopped and cleaned
	// up before creating the new one. If a controller already exists for the
	// same network configuration, it synchronizes the list of NADs to the
	// controller.
	EnsureNetwork(util.NetInfo)

	// DeleteNetwork removes the network controller for the provided network
	DeleteNetwork(string)

	// Start the controller
	Start() error

	// Stop the controller
	Stop()

	getNetwork(string) util.NetInfo
}

func newNetworkManager(name string, ncm NetworkControllerManager) networkManager {
	nc := &networkManagerImpl{
		name:               fmt.Sprintf("[%s network manager]", name),
		ncm:                ncm,
		networks:           map[string]util.NetInfo{},
		networkControllers: map[string]*networkControllerState{},
	}
	// this controller does not feed from an informer, networks are manually
	// added to the queue for processing
	config := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   nc.sync,
		Threadiness: 1,
	}
	nc.controller = controller.NewReconciler(
		nc.name,
		config,
	)
	return nc
}

type networkControllerState struct {
	controller         NetworkController
	stoppedAndDeleting bool
}

type networkManagerImpl struct {
	sync.RWMutex
	name               string
	controller         controller.Reconciler
	ncm                NetworkControllerManager
	networks           map[string]util.NetInfo
	networkControllers map[string]*networkControllerState
}

// Start will cleanup stale networks that have not been ensured via
// EnsuredNetwork before this call
func (nm *networkManagerImpl) Start() error {
	return controller.StartWithInitialSync(nm.syncAll, nm.controller)
}

func (nm *networkManagerImpl) Stop() {
	controller.Stop(nm.controller)
	for _, networkControllerState := range nm.getAllNetworkStates() {
		networkControllerState.controller.Stop()
	}
}

func (nm *networkManagerImpl) EnsureNetwork(network util.NetInfo) {
	nm.setNetwork(network.GetNetworkName(), network)
	nm.controller.Reconcile(network.GetNetworkName())
}

func (nm *networkManagerImpl) DeleteNetwork(network string) {
	nm.setNetwork(network, nil)
	nm.controller.Reconcile(network)
}

func (nm *networkManagerImpl) setNetwork(network string, netInfo util.NetInfo) {
	nm.Lock()
	defer nm.Unlock()
	if netInfo == nil {
		delete(nm.networks, network)
		return
	}
	nm.networks[network] = netInfo
}

func (nm *networkManagerImpl) getNetwork(network string) util.NetInfo {
	nm.RLock()
	defer nm.RUnlock()
	return nm.networks[network]
}

func (nm *networkManagerImpl) getAllNetworks() []util.BasicNetInfo {
	nm.RLock()
	defer nm.RUnlock()
	networks := make([]util.BasicNetInfo, 0, len(nm.networks))
	for _, network := range nm.networks {
		networks = append(networks, network)
	}
	return networks
}

func (nm *networkManagerImpl) setNetworkState(network string, state *networkControllerState) {
	nm.Lock()
	defer nm.Unlock()
	if state == nil {
		delete(nm.networkControllers, network)
		return
	}
	nm.networkControllers[network] = state
}

func (nm *networkManagerImpl) getNetworkState(network string) *networkControllerState {
	nm.RLock()
	defer nm.RUnlock()
	return nm.networkControllers[network]
}

func (nm *networkManagerImpl) getAllNetworkStates() []*networkControllerState {
	nm.RLock()
	defer nm.RUnlock()
	networkStates := make([]*networkControllerState, 0, len(nm.networks))
	for _, state := range nm.networkControllers {
		networkStates = append(networkStates, state)
	}
	return networkStates
}

func (nm *networkManagerImpl) sync(network string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync network %s", nm.name, network)
	defer func() {
		klog.V(4).Infof("%s: finished syncing network %s, took %v", nm.name, network, time.Since(startTime))
	}()

	want := nm.getNetwork(network)
	have := nm.getNetworkState(network)

	// we will dispose of the old network if deletion is in progress or if
	// configuration changed
	dispose := have != nil && (have.stoppedAndDeleting || !have.controller.Equals(want))

	if dispose {
		if !have.stoppedAndDeleting {
			have.controller.Stop()
		}
		have.stoppedAndDeleting = true
		err := have.controller.Cleanup()
		if err != nil {
			return fmt.Errorf("%s: failed to cleanup network %s: %w", nm.name, network, err)
		}
		nm.setNetworkState(network, nil)
	}

	// no network needed so nothing to do
	if want == nil {
		return nil
	}

	// we didn't dispose of current controller, so this might just be an update of the network NADs
	if have != nil && !dispose {
		have.controller.SetNADs(want.GetNADs()...)
		return nil
	}

	// setup & start the new network controller
	nc, err := nm.ncm.NewNetworkController(util.CopyNetInfo(want))
	if err != nil {
		return fmt.Errorf("%s: failed to create network %s: %w", nm.name, network, err)
	}

	err = nc.Start(context.Background())
	if err != nil {
		return fmt.Errorf("%s: failed to start network %s: %w", nm.name, network, err)
	}
	nm.setNetworkState(network, &networkControllerState{controller: nc})

	return nil
}

func (nm *networkManagerImpl) syncAll() error {
	// as we sync upon start, consider networks that have not been ensured as
	// stale and clean them up
	validNetworks := nm.getAllNetworks()
	if err := nm.ncm.CleanupDeletedNetworks(validNetworks...); err != nil {
		return err
	}

	// sync all known networks. There is no informer for networks. Keys are added by NAD controller.
	// Certain downstream controllers that handle configuration for multiple networks depend on being
	// aware of all the existing networks on initialization. To achieve that, we need to start existing
	// networks synchronously. Otherwise, these controllers might incorrectly assess valid configuration
	// as stale.
	start := time.Now()
	klog.Infof("%s: syncing all networks", nm.name)
	for _, network := range validNetworks {
		if err := nm.sync(network.GetNetworkName()); errors.Is(err, ErrNetworkControllerTopologyNotManaged) {
			klog.V(5).Infof(
				"ignoring network %q since %q does not manage it",
				network.GetNetworkName(),
				nm.name,
			)
		} else if err != nil {
			return fmt.Errorf("failed to sync network %s: %w", network.GetNetworkName(), err)
		}
	}
	klog.Infof("%s: finished syncing all networks. Time taken: %s", nm.name, time.Since(start))
	return nil
}
