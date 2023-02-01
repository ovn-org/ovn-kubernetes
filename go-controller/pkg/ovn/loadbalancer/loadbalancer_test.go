package loadbalancer

import (
	"fmt"
	"testing"

	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnsureLBs(t *testing.T) {
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	if err != nil {
		t.Fatalf("Error creating NB: %v", err)
	}
	t.Cleanup(cleanup.Cleanup)
	lbCache, err := GetLBCache(nbClient)
	if err != nil {
		t.Fatalf("Error creating LB Cache: %v", err)
	}
	name := "foo"
	namespace := "testns"
	defaultExternalIDs := map[string]string{
		"k8s.ovn.org/kind":  "Service",
		"k8s.ovn.org/owner": fmt.Sprintf("%s/%s", namespace, name),
	}
	// put stale lb in the cache
	staleLBs := []LB{
		{
			Name:        "Service_testns/foo_TCP_node_router_node-a",
			ExternalIDs: defaultExternalIDs,
			Routers:     []string{"gr-node-a", "non-exisitng-router"},
			Switches:    []string{"non-exisitng-switch"},
			Groups:      []string{"non-existing-group"},
			Protocol:    "TCP",
			Rules: []LBRule{
				{
					Source:  Addr{"1.2.3.4", 80},
					Targets: []Addr{{"169.254.169.2", 8080}},
				},
			},
			UUID: "test-UUID",
		},
	}
	lbCache.update(staleLBs, nil)

	defaultService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
		},
	}
	// required lb doesn't have stale router, switch, and port group reference.
	// "gr-node-a" is listed as applied in the cache, no update operation will be generated for it.
	LBs := []LB{
		{
			Name:        "Service_testns/foo_TCP_node_router_node-a",
			ExternalIDs: defaultExternalIDs,
			Routers:     []string{"gr-node-a"},
			Protocol:    "TCP",
			Rules: []LBRule{
				{
					Source:  Addr{"1.2.3.4", 80},
					Targets: []Addr{{"169.254.169.2", 8080}},
				},
			},
		},
	}
	err = EnsureLBs(nbClient, defaultService, LBs)
	if err != nil {
		t.Fatalf("Error EnsureLBs: %v", err)
	}
}
