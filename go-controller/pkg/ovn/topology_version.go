package ovn

import (
	"context"
)

// reportTopologyVersion saves the topology version to an ExternalID on the ovn_cluster_router LogicalRouter in nbdb
// This is used by nodes to determine the cluster's topology
func (oc *DefaultNetworkController) reportTopologyVersion(ctx context.Context) error {
	return oc.updateL3TopologyVersion()
}
