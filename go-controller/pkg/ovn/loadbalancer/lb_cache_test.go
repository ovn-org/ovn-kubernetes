package loadbalancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestNewCache(t *testing.T) {

	initialDb := []libovsdb.TestData{
		&nbdb.LoadBalancer{
			UUID:     "cb6ebcb0-c12d-4404-ada7-5aa2b898f06b",
			Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},
			Vips: map[string]string{
				"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
				"[fe::1]:1":        "[fe::2]:1,[fe::2]:2",
			},
		},
		&nbdb.LoadBalancer{
			UUID:     "7dc190c4-c615-467f-af83-9856d832c9a0",
			Name:     "Service_default/kubernetes_TCP_node_switch_ovn-control-plane_merged",
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},
			Vips: map[string]string{
				"192.168.0.1:6443": "1.1.1.1:1,2.2.2.2:2",
				"[ff::1]:1":        "[fe::2]:1,[fe::2]:2",
			},
		},
		&nbdb.LogicalRouter{
			Name:         "GR_ovn-worker2",
			LoadBalancer: []string{"7dc190c4-c615-467f-af83-9856d832c9a0"},
		},
		&nbdb.LogicalRouter{
			Name:         "GR_ovn-worker",
			LoadBalancer: []string{"7dc190c4-c615-467f-af83-9856d832c9a0"},
		},
		&nbdb.LogicalRouter{
			Name: "ovn_cluster_router",
		},
		&nbdb.LogicalRouter{
			Name:         "GR_ovn-control-plane",
			LoadBalancer: []string{"cb6ebcb0-c12d-4404-ada7-5aa2b898f06b"},
		},
		&nbdb.LogicalSwitch{
			Name:         "ovn-worker",
			LoadBalancer: []string{"7dc190c4-c615-467f-af83-9856d832c9a0"},
		},
		&nbdb.LogicalSwitch{
			Name:         "ovn-worker2",
			LoadBalancer: []string{"cb6ebcb0-c12d-4404-ada7-5aa2b898f06b"},
		},
		&nbdb.LogicalSwitch{
			Name:         "ovn-control-plane",
			LoadBalancer: []string{"7dc190c4-c615-467f-af83-9856d832c9a0"},
		},
	}

	nbClient, cleanup, err := libovsdb.NewNBTestHarness(libovsdb.TestSetup{NBData: initialDb}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup.Cleanup)

	c, err := newCache(nbClient)
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, map[string]*CachedLB{
		"cb6ebcb0-c12d-4404-ada7-5aa2b898f06b": {
			UUID:     "cb6ebcb0-c12d-4404-ada7-5aa2b898f06b",
			Name:     "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
			Protocol: "tcp",
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},
			VIPs: sets.NewString("192.168.0.1:6443", "[fe::1]:1"),
			Switches: sets.String{
				"ovn-worker2": {},
			},
			Routers: sets.String{
				"GR_ovn-control-plane": {},
			},
		},
		"7dc190c4-c615-467f-af83-9856d832c9a0": {
			UUID:     "7dc190c4-c615-467f-af83-9856d832c9a0",
			Name:     "Service_default/kubernetes_TCP_node_switch_ovn-control-plane_merged",
			Protocol: "tcp",
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},
			VIPs: sets.NewString("192.168.0.1:6443", "[ff::1]:1"),
			Switches: sets.String{
				"ovn-worker":        {},
				"ovn-control-plane": {},
			},
			Routers: sets.String{
				"GR_ovn-worker":  {},
				"GR_ovn-worker2": {},
			},
		},
	}, c.existing)

	c.RemoveRouter("GR_ovn-worker")
	assert.Equal(t, c.existing["7dc190c4-c615-467f-af83-9856d832c9a0"].Routers, sets.String{
		"GR_ovn-worker2": {},
	})

	c.RemoveSwitch("ovn-worker")
	assert.Equal(t, c.existing["7dc190c4-c615-467f-af83-9856d832c9a0"].Switches, sets.String{
		"ovn-control-plane": {},
	})
	// nothing changed
	assert.Equal(t, c.existing["cb6ebcb0-c12d-4404-ada7-5aa2b898f06b"].Switches, sets.String{
		"ovn-worker2": {},
	})
}
