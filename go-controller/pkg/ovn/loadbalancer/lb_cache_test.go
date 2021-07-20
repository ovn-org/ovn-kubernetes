package loadbalancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestNewCache(t *testing.T) {

	data := `
{
  "data": [
    [
      "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
      [
        "uuid",
        "cb6ebcb0-c12d-4404-ada7-5aa2b898f06b"
      ],
      [
        "map",
        [
          [
            "k8s.ovn.org/kind",
            "Service"
          ],
          [
            "k8s.ovn.org/owner",
            "default/kubernetes"
          ]
        ]
      ]
    ],
    [
      "Service_default/kubernetes_TCP_node_switch_ovn-control-plane_merged",
      [
        "uuid",
        "7dc190c4-c615-467f-af83-9856d832c9a0"
      ],
      [
        "map",
        [
          [
            "k8s.ovn.org/kind",
            "Service"
          ],
          [
            "k8s.ovn.org/owner",
            "default/kubernetes"
          ]
        ]
      ]
    ]
  ],
  "headings": [
    "name",
    "_uuid",
    "external_ids",
	"protocol"
  ]
}
`

	fexec := ovntest.NewFakeExec()
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --format=json --data=json --columns=name,_uuid,external_ids find load_balancer",
		Output: data,
	})

	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd: `ovn-nbctl --timeout=15 --no-heading --format=csv --data=bare --columns=name,load_balancer find logical_router`,
		Output: `GR_ovn-worker2,31bb6bff-93b9-4080-a1b9-9a1fa898b1f0 7dc190c4-c615-467f-af83-9856d832c9a0 f0747ebb-71c2-4249-bdca-f33670ae544f
GR_ovn-worker,31bb6bff-93b9-4080-a1b9-9a1fa898b1f0 7dc190c4-c615-467f-af83-9856d832c9a0 f0747ebb-71c2-4249-bdca-f33670ae544f
ovn_cluster_router,
GR_ovn-control-plane,31bb6bff-93b9-4080-a1b9-9a1fa898b1f0 cb6ebcb0-c12d-4404-ada7-5aa2b898f06b f0747ebb-71c2-4249-bdca-f33670ae544f
`,
	})
	err := util.SetExec(fexec)
	if err != nil {
		t.Fatal(err)
	}

	lbs, err := listLBs()
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, []CachedLB{
		{
			UUID: "cb6ebcb0-c12d-4404-ada7-5aa2b898f06b",
			Name: "Service_default/kubernetes_TCP_node_router_ovn-control-plane",
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},

			Switches: sets.String{},
			Routers:  sets.String{},
		},
		{
			UUID: "7dc190c4-c615-467f-af83-9856d832c9a0",
			Name: "Service_default/kubernetes_TCP_node_switch_ovn-control-plane_merged",
			ExternalIDs: map[string]string{
				"k8s.ovn.org/kind":  "Service",
				"k8s.ovn.org/owner": "default/kubernetes",
			},

			Switches: sets.String{},
			Routers:  sets.String{},
		},
	}, lbs)

	routerLBs, err := findTableLBs("logical_router")
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, routerLBs, map[string][]string{
		"GR_ovn-worker2":       {"31bb6bff-93b9-4080-a1b9-9a1fa898b1f0", "7dc190c4-c615-467f-af83-9856d832c9a0", "f0747ebb-71c2-4249-bdca-f33670ae544f"},
		"GR_ovn-worker":        {"31bb6bff-93b9-4080-a1b9-9a1fa898b1f0", "7dc190c4-c615-467f-af83-9856d832c9a0", "f0747ebb-71c2-4249-bdca-f33670ae544f"},
		"ovn_cluster_router":   {},
		"GR_ovn-control-plane": {"31bb6bff-93b9-4080-a1b9-9a1fa898b1f0", "cb6ebcb0-c12d-4404-ada7-5aa2b898f06b", "f0747ebb-71c2-4249-bdca-f33670ae544f"},
	})

}
