package networkmanager

import (
	"context"
	"errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/tools/record"
)

var ErrNetworkControllerTopologyNotManaged = errors.New("no cluster network controller to manage topology")

// Interface is the main package entrypoint and provides network related
// information to the rest of the project.
type Interface interface {
	GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error)
}

// Controller handles the runtime of the package
type Controller interface {
	Interface() Interface
	Start() error
	Stop()
}

// Default returns a default implementation that assumes the default network is
// the only ever existing network. Used when multi-network capabilities are not
// enabled or testing.
func Default() Controller {
	return def
}

// New builds a new Controller. It's aware of networks configured in the system,
// gathers relevant information about them for the project and handles the
// lifecycle of their corresponding network controllers.
func New(
	name string,
	cm ControllerManager,
	wf watchFactory,
	recorder record.EventRecorder,
) (Controller, error) {
	return newController(name, cm, wf, recorder)
}

// ControllerManager manages controllers. Needs to be provided in order to build
// new network controllers and to to be informed of potential stale networks in
// case it has clean-up of it's own to do.
type ControllerManager interface {
	NewNetworkController(netInfo util.NetInfo) (NetworkController, error)
	CleanupStaleNetworks(validNetworks ...util.BasicNetInfo) error
}

// BaseNetworkController is a network controller that can be started and
// stopped.
type BaseNetworkController interface {
	util.NetInfo
	Start(ctx context.Context) error
	Stop()
}

// NetworkController is a BaseNetworkController that can also clean up after
// itself.
type NetworkController interface {
	BaseNetworkController
	Cleanup() error
}

// defaultNetworkManager assumes the default network is
// the only ever existing network. Used when multi-network capabilities are not
// enabled or testing.
type defaultNetworkManager struct{}

func (nm defaultNetworkManager) Interface() Interface {
	return &nm
}

func (nm defaultNetworkManager) Start() error {
	return nil
}

func (nm defaultNetworkManager) Stop() {}

func (nm defaultNetworkManager) GetActiveNetworkForNamespace(string) (util.NetInfo, error) {
	return &util.DefaultNetInfo{}, nil
}

var def Controller = &defaultNetworkManager{}
