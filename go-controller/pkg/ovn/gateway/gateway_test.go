package gateway

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestGetOvnGateways(t *testing.T) {

	tests := []struct {
		name         string
		ovnInitState []libovsdbtest.TestData
		want         []string
	}{
		{
			name: "return multiple gateways",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
					Options: map[string]string{
						"chassis": "something",
					},
				},
				&nbdb.LogicalRouter{
					Name: "GR_ovn-worker",
					Options: map[string]string{
						"chassis": "something",
					},
				},
				&nbdb.LogicalRouter{
					Name: "GR_ovn-worker2",
					Options: map[string]string{
						"chassis": "something",
					},
				},
			},
			want: []string{"GR_ovn-control-plane", "GR_ovn-worker", "GR_ovn-worker2"},
		},
		{
			name: "return no gateway because chassis is \"null\"",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
					Options: map[string]string{
						"chassis": "null",
					},
				},
			},
			want: []string{},
		},
		{
			name:         "return no gateway because no logical routers",
			ovnInitState: []libovsdbtest.TestData{},
			want:         []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopChan := make(chan struct{})
			dbSetup := libovsdbtest.TestSetup{
				NBData: tt.ovnInitState,
			}
			libovsdbOvnNBClient, err := libovsdbtest.NewNBTestHarness(dbSetup, stopChan)
			if err != nil {
				t.Errorf("libovsdb client error: %v", err)
			}
			got, err := GetOvnGateways(libovsdbOvnNBClient)
			if !sets.NewString(got...).HasAll(tt.want...) {
				t.Errorf("GetOvnGateways() got = %v, want %v", got, tt.want)
			}
			close(stopChan)
		})
	}
}

func TestGetGatewayPhysicalIPs(t *testing.T) {

	tests := []struct {
		name         string
		ovnInitState []libovsdbtest.TestData
		want         []string
		wantErr      bool
	}{
		{
			name: "multiple gateways",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
					ExternalIDs: map[string]string{
						"physical_ips": "172.19.0.3,fc00:f853:ccd:e793::3",
					},
				},
			},
			want:    []string{"172.19.0.3", "fc00:f853:ccd:e793::3"},
			wantErr: false,
		},
		{
			name: "one gateway",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
					ExternalIDs: map[string]string{
						"physical_ips": "",
						"physical_ip":  "172.19.0.3",
					},
				},
			},
			want:    []string{"172.19.0.3"},
			wantErr: false,
		},
		{
			name: "no gateway because of unset values",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
					ExternalIDs: map[string]string{
						"physical_ips": "",
						"physical_ip":  "",
					},
				},
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "no gateway because of inexistant external_ids",
			ovnInitState: []libovsdbtest.TestData{
				&nbdb.LogicalRouter{
					Name: "GR_ovn-control-plane",
				},
			},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopChan := make(chan struct{})
			dbSetup := libovsdbtest.TestSetup{
				NBData: tt.ovnInitState,
			}
			libovsdbOvnNBClient, err := libovsdbtest.NewNBTestHarness(dbSetup, stopChan)
			if err != nil {
				t.Errorf("libovsdb client error: %v", err)
			}
			got, err := GetGatewayPhysicalIPs(libovsdbOvnNBClient, "GR_ovn-control-plane")
			if (err != nil) != tt.wantErr {
				t.Errorf("GetGatewayPhysicalIPs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !sets.NewString(got...).HasAll(tt.want...) {
				t.Errorf("GetOvnGateways() got = %v, want %v", got, tt.want)
			}
			close(stopChan)
		})
	}
}
