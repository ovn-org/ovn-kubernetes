package networkmanager

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

func newNetworkController(name string, cm ControllerManager) *networkController {
	nc := &networkController{
		name:               fmt.Sprintf("[%s network controller]", name),
		cm:                 cm,
		networks:           map[string]util.MutableNetInfo{},
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

type networkController struct {
	sync.RWMutex
	name               string
	controller         controller.Reconciler
	cm                 ControllerManager
	networks           map[string]util.MutableNetInfo
	networkControllers map[string]*networkControllerState
}

// Start will cleanup stale networks that have not been ensured via
// EnsuredNetwork before this call
func (c *networkController) Start() error {
	return controller.StartWithInitialSync(c.syncAll, c.controller)
}

func (c *networkController) Stop() {
	controller.Stop(c.controller)
	for _, networkControllerState := range c.getAllNetworkStates() {
		networkControllerState.controller.Stop()
	}
}

func (c *networkController) EnsureNetwork(network util.MutableNetInfo) {
	c.setNetwork(network.GetNetworkName(), network)
	c.controller.Reconcile(network.GetNetworkName())
}

func (c *networkController) DeleteNetwork(network string) {
	c.setNetwork(network, nil)
	c.controller.Reconcile(network)
}

func (c *networkController) setNetwork(network string, netInfo util.MutableNetInfo) {
	c.Lock()
	defer c.Unlock()
	if netInfo == nil {
		delete(c.networks, network)
		return
	}
	c.networks[network] = netInfo
}

func (c *networkController) getNetwork(network string) util.MutableNetInfo {
	c.RLock()
	defer c.RUnlock()
	return c.networks[network]
}

func (c *networkController) getAllNetworks() []util.NetInfo {
	c.RLock()
	defer c.RUnlock()
	networks := make([]util.NetInfo, 0, len(c.networks))
	for _, network := range c.networks {
		networks = append(networks, network)
	}
	return networks
}

func (c *networkController) setNetworkState(network string, state *networkControllerState) {
	c.Lock()
	defer c.Unlock()
	if state == nil {
		delete(c.networkControllers, network)
		return
	}
	c.networkControllers[network] = state
}

func (c *networkController) getNetworkState(network string) *networkControllerState {
	c.RLock()
	defer c.RUnlock()
	return c.networkControllers[network]
}

func (c *networkController) getAllNetworkStates() []*networkControllerState {
	c.RLock()
	defer c.RUnlock()
	networkStates := make([]*networkControllerState, 0, len(c.networks))
	for _, state := range c.networkControllers {
		networkStates = append(networkStates, state)
	}
	return networkStates
}

func (c *networkController) sync(network string) error {
	startTime := time.Now()
	klog.V(5).Infof("%s: sync network %s", c.name, network)
	defer func() {
		klog.V(4).Infof("%s: finished syncing network %s, took %v", c.name, network, time.Since(startTime))
	}()

	want := c.getNetwork(network)
	have := c.getNetworkState(network)

	// we will dispose of the old network if deletion is in progress or if
	// configuration changed
	dispose := have != nil && (have.stoppedAndDeleting || !util.AreNetworksCompatible(have.controller, want))

	if dispose {
		if !have.stoppedAndDeleting {
			have.controller.Stop()
		}
		have.stoppedAndDeleting = true
		err := have.controller.Cleanup()
		if err != nil {
			return fmt.Errorf("%s: failed to cleanup network %s: %w", c.name, network, err)
		}
		c.setNetworkState(network, nil)
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
	nc, err := c.cm.NewNetworkController(util.NewMutableNetInfo(want))
	if err != nil {
		return fmt.Errorf("%s: failed to create network %s: %w", c.name, network, err)
	}

	err = nc.Start(context.Background())
	if err != nil {
		return fmt.Errorf("%s: failed to start network %s: %w", c.name, network, err)
	}
	c.setNetworkState(network, &networkControllerState{controller: nc})

	return nil
}

func (c *networkController) syncAll() error {
	// as we sync upon start, consider networks that have not been ensured as
	// stale and clean them up
	validNetworks := c.getAllNetworks()
	if err := c.cm.CleanupStaleNetworks(validNetworks...); err != nil {
		return err
	}

	// sync all known networks. There is no informer for networks. Keys are added by NAD controller.
	// Certain downstream controllers that handle configuration for multiple networks depend on being
	// aware of all the existing networks on initialization. To achieve that, we need to start existing
	// networks synchronously. Otherwise, these controllers might incorrectly assess valid configuration
	// as stale.
	start := time.Now()
	klog.Infof("%s: syncing all networks", c.name)
	for _, network := range validNetworks {
		if err := c.sync(network.GetNetworkName()); errors.Is(err, ErrNetworkControllerTopologyNotManaged) {
			klog.V(5).Infof(
				"ignoring network %q since %q does not manage it",
				network.GetNetworkName(),
				c.name,
			)
		} else if err != nil {
			return fmt.Errorf("failed to sync network %s: %w", network.GetNetworkName(), err)
		}
	}
	klog.Infof("%s: finished syncing all networks. Time taken: %s", c.name, time.Since(start))
	return nil
}
