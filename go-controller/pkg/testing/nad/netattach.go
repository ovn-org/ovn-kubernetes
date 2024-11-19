package nad

import (
	"context"
	"errors"

	networkAttachDefController "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type FakeNetworkController struct {
	util.NetInfo
}

func (nc *FakeNetworkController) Start(ctx context.Context) error {
	return nil
}

func (nc *FakeNetworkController) Stop() {}

func (nc *FakeNetworkController) Cleanup() error {
	return nil
}

type FakeNetworkControllerManager struct{}

func (ncm *FakeNetworkControllerManager) NewNetworkController(netInfo util.NetInfo) (networkAttachDefController.NetworkController, error) {
	return &FakeNetworkController{netInfo}, nil
}

func (ncm *FakeNetworkControllerManager) CleanupDeletedNetworks(validNetworks ...util.BasicNetInfo) error {
	return nil
}

type FakeNADController struct {
	// namespace -> netInfo
	PrimaryNetworks map[string]util.NetInfo
}

func (nc *FakeNADController) Start() error { return nil }
func (nc *FakeNADController) Stop()        {}
func (nc *FakeNADController) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if primaryNetworks, ok := nc.PrimaryNetworks[namespace]; ok && primaryNetworks != nil {
		return primaryNetworks, nil
	}
	return &util.DefaultNetInfo{}, nil
}
func (nc *FakeNADController) GetNetwork(networkName string) (util.NetInfo, error) {
	for _, ni := range nc.PrimaryNetworks {
		if ni.GetNetworkName() == networkName {
			return ni, nil
		}
	}
	return &util.DefaultNetInfo{}, nil
}
func (nc *FakeNADController) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
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

func (nc *FakeNADController) DoWithLock(f func(network util.NetInfo) error) error {
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
