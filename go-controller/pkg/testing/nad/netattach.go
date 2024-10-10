package nad

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type FakeNetworkManager struct {
	// namespace -> netInfo
	PrimaryNetworks map[string]util.NetInfo
}

func (nm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	if primaryNetworks, ok := nm.PrimaryNetworks[namespace]; ok && primaryNetworks != nil {
		return primaryNetworks, nil
	}
	return &util.DefaultNetInfo{}, nil
}
