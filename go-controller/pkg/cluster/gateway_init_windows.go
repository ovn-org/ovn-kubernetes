// +build windows

package cluster

// InitGateway does nothing for Windows.
func (cluster *OvnClusterController) InitGateway(
	nodeName, clusterIPSubnet, subnet string) error {
	return nil
}
