// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestAdvertisedServiceRoutingTransitions(t *testing.T) {
	for _, transport := range []string{"", types.NetworkTransportNoOverlay, types.NetworkTransportEVPN} {
		t.Run("transport="+transport, func(t *testing.T) {
			g := gomega.NewWithT(t)
			g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
			t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
			netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{Name: "blue"}, Role: types.NetworkRolePrimary, Topology: types.Layer3Topology, Transport: transport,
			})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			nc := &UserDefinedNodeNetworkController{}
			nc.name = "node"
			old := util.NewMutableNetInfo(netInfo)
			previousMarks := shouldAddUDNServiceMarkRules(old, "node")
			for _, vrfs := range [][]string{nil, {types.DefaultNetworkName}, {"blue"}, nil, {types.DefaultNetworkName}} {
				current := util.NewMutableNetInfo(netInfo)
				current.SetPodNetworkAdvertisedVRFs(map[string][]string{"node": vrfs})
				expected := transport == "" && len(vrfs) == 0 || transport != types.NetworkTransportEVPN && len(vrfs) > 0 && vrfs[0] == types.DefaultNetworkName
				g.Expect(shouldAddUDNServiceMarkRules(current, "node")).To(gomega.Equal(expected))
				if previousMarks != expected {
					g.Expect(nc.shouldReconcileNetworkChange(old, current)).To(gomega.BeTrue(), "service mark changes must trigger gateway reconciliation")
				}
				if transport == types.NetworkTransportEVPN {
					g.Expect(nc.shouldReconcileNetworkChange(old, current)).To(gomega.BeFalse(), "EVPN's routing decision is independent of the mapping")
				}
				old = current
				previousMarks = expected
			}
		})
	}
}
