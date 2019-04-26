// +build windows

package cluster

import (
	"fmt"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
)

func initLocalnetGateway(nodeName string, clusterIPSubnet []string,
	subnet string, nodePortEnable bool, wf *factory.WatchFactory) error {
	// TODO: Implement this
	return fmt.Errorf("Not implemented yet on Windows")
}
