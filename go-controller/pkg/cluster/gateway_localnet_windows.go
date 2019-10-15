// +build windows

package cluster

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
)

func initLocalnetGateway(nodeName string,
	subnet string, wf *factory.WatchFactory) (map[string]string, error) {
	// TODO: Implement this
	return nil, fmt.Errorf("Not implemented yet on Windows")
}

func cleanupLocalnetGateway() error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}
