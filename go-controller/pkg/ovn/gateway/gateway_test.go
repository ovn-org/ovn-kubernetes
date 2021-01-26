package gateway

import (
	"reflect"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
)

func TestGetOvnGateways(t *testing.T) {

	tests := []struct {
		name    string
		ovnCmd  ovntest.ExpectedCmd
		want    []string
		want1   string
		wantErr bool
	}{
		{
			name: "return multiple gateways",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
				Output: `GR_ovn-control-plane

					GR_ovn-worker
					
					GR_ovn-worker2
					`,
			},
			want:    []string{"GR_ovn-control-plane", "GR_ovn-worker", "GR_ovn-worker2"},
			want1:   "",
			wantErr: false,
		},
		{
			name: "return one gateway",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
				Output: "GR_ovn-control-plane",
			},
			want:    []string{"GR_ovn-control-plane"},
			want1:   "",
			wantErr: false,
		},
		{
			name: "return no gateway",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
				Output: "",
			},
			want:    []string{},
			want1:   "",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			got, got1, err := GetOvnGateways()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetOvnGateways() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetOvnGateways() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("GetOvnGateways() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}

func TestGetGatewayPhysicalIPs(t *testing.T) {

	tests := []struct {
		name    string
		ovnCmd  []ovntest.ExpectedCmd
		want    []string
		wantErr bool
	}{
		{
			name: "multiple gateways",
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_ovn-control-plane external_ids:physical_ips",
					Output: "172.19.0.3,fc00:f853:ccd:e793::3",
				},
			},
			want:    []string{"172.19.0.3", "fc00:f853:ccd:e793::3"},
			wantErr: false,
		},
		{
			name: "one gateway",
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_ovn-control-plane external_ids:physical_ips",
					Output: "",
				},
				{
					Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_ovn-control-plane external_ids:physical_ip",
					Output: "172.19.0.3",
				},
			},
			want:    []string{"172.19.0.3"},
			wantErr: false,
		},
		{
			name: "no gateway",
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_ovn-control-plane external_ids:physical_ips",
					Output: "",
				},
				{
					Cmd:    "ovn-nbctl --timeout=15 get logical_router GR_ovn-control-plane external_ids:physical_ip",
					Output: "",
				},
			},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewFakeExec()
			for _, cmd := range tt.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)
			}
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			got, err := GetGatewayPhysicalIPs("GR_ovn-control-plane")
			if (err != nil) != tt.wantErr {
				t.Errorf("GetGatewayPhysicalIPs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetGatewayPhysicalIPs() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCreateGatewayLoadBalancer(t *testing.T) {
	tests := []struct {
		name          string
		gatewayRouter string
		protocol      kapi.Protocol
		idkey         string
		ovnCmd        ovntest.ExpectedCmd
		want          string
		wantErr       bool
	}{
		{
			name:          "create TCP loadbalancer gateway",
			gatewayRouter: "GR_node3",
			protocol:      kapi.ProtocolTCP,
			idkey:         OvnGatewayLoadBalancerIds,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- create load_balancer external_ids:TCP_lb_gateway_router=GR_node3 protocol=tcp options:reject=true",
				Output: "97f15346-5fea-11eb-968f-a8a1590cda29",
			},
			want:    "97f15346-5fea-11eb-968f-a8a1590cda29",
			wantErr: false,
		},
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			got, err := CreateGatewayLoadBalancer(tt.gatewayRouter, tt.protocol, tt.idkey)
			if (err != nil) != tt.wantErr {
				t.Errorf("CreateGatewayLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("CreateGatewayLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}
