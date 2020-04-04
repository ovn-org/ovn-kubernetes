// +build windows

package node

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
)

func initLocalnetGateway(nodeName string, subnet *net.IPNet,
	wf *factory.WatchFactory, nodeAnnotator kube.Annotator) error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}

func cleanupLocalnetGateway() error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}
