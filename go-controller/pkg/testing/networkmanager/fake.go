package networkmanager

import (
	"context"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type FakeNetworkController struct {
	util.NetInfo
}

func (fnc *FakeNetworkController) Start(ctx context.Context) error {
	return nil
}

func (fnc *FakeNetworkController) Stop() {}

func (fnc *FakeNetworkController) Cleanup() error {
	return nil
}

func (nc *FakeNetworkController) Reconcile(util.NetInfo) error {
	return nil
}

type FakeControllerManager struct{}

func (fcm *FakeControllerManager) NewNetworkController(netInfo util.NetInfo) (networkmanager.NetworkController, error) {
	return &FakeNetworkController{netInfo}, nil
}

func (fcm *FakeControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	return nil
}

func (fcm *FakeControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return nil
}

func (fcm *FakeControllerManager) Reconcile(name string, old, new util.NetInfo) error {
	return nil
}

type FakeNetworkManager struct {
	// namespace -> netInfo
	PrimaryNetworks map[string]util.NetInfo
}

func (fnm *FakeNetworkManager) Start() error { return nil }
func (fnm *FakeNetworkManager) Stop()        {}
func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return fnm.GetActiveNetworkForNamespaceFast(namespace), nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	if primaryNetworks, ok := fnm.PrimaryNetworks[namespace]; ok && primaryNetworks != nil {
		return primaryNetworks
	}
	return &util.DefaultNetInfo{}
}

func (fnm *FakeNetworkManager) GetNetwork(name string) util.NetInfo {
	return nil
}
