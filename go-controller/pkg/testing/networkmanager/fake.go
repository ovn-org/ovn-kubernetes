package networkmanager

import (
	"context"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
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

type FakeNetworkManager struct {
	// namespace -> netInfo
	PrimaryNetworks map[string]util.NetInfo
}

func (fnm *FakeNetworkManager) Start() error { return nil }

func (fnm *FakeNetworkManager) Stop() {}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if primaryNetworks, ok := fnm.PrimaryNetworks[namespace]; ok && primaryNetworks != nil {
		return primaryNetworks, nil
	}
	return &util.DefaultNetInfo{}, nil
}

func (nc *FakeNetworkManager) GetNetwork(networkName string) util.NetInfo {
	for _, ni := range nc.PrimaryNetworks {
		if ni.GetNetworkName() == networkName {
			return ni
		}
	}
	return &util.DefaultNetInfo{}
}

func (nc *FakeNetworkManager) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	namespaces := make([]string, 0)
	for namespaceName, primaryNAD := range nc.PrimaryNetworks {
		nadNetworkName := primaryNAD.GetNADs()[0]
		if nadNetworkName != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

func (nc *FakeNetworkManager) DoWithLock(f func(network util.NetInfo) error) error {
	var errs []error
	for _, ni := range nc.PrimaryNetworks {
		if err := f(ni); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
