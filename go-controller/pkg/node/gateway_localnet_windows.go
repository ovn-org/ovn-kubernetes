// +build windows

package node

import (
	"fmt"
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
)

func (n *OvnNode) initLocalnetGateway(subnet *net.IPNet, nodeAnnotator kube.Annotator) error {
	// TODO: Implement this
	return fmt.Errorf("not implemented yet on Windows")
}

func cleanupLocalnetGateway(string) error {
	// TODO: Implement this
	return fmt.Errorf("not implemented yet on Windows")
}
