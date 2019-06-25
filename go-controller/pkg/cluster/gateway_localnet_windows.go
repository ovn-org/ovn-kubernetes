// +build windows

package cluster

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
)

func initLocalnetGateway(nodeName string, clusterIPSubnet []string,
	subnet string, wf *factory.WatchFactory) error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}

func cleanupLocalnetGateway() error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}
