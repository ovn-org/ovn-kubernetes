package loadbalancer

import (
	"fmt"
	"reflect"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
)

func TestGetOVNKubeLoadBalancer(t *testing.T) {
	tests := []struct {
		name     string
		protocol kapi.Protocol
		ovnCmd   ovntest.ExpectedCmd
		want     string
		wantErr  bool
	}{
		{
			name:     "existing loadbalancer TCP",
			protocol: kapi.ProtocolTCP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
				Output: "a08ea426-2288-11eb-a30b-a8a1590cda29",
			},
			want:    "a08ea426-2288-11eb-a30b-a8a1590cda29",
			wantErr: false,
		},
		{
			name:     "non existing loadbalancer UDP",
			protocol: kapi.ProtocolUDP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-udp=yes",
				Output: "",
			},
			want:    "",
			wantErr: true,
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

			got, err := GetOVNKubeLoadBalancer(tt.protocol)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetOVNKubeLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetOVNKubeLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLoadBalancerVIPs(t *testing.T) {
	tests := []struct {
		name         string
		loadBalancer string
		ovnCmd       ovntest.ExpectedCmd
		want         map[string]string
		wantErr      bool
	}{
		{
			name:         "loadbalancer with VIPs",
			loadBalancer: "my-lb",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer my-lb vips",
				Output: `{"10.96.0.10:53"="10.244.2.3:53,10.244.2.5:53", "10.96.0.10:9153"="10.244.2.3:9153,10.244.2.5:9153", "10.96.0.1:443"="172.19.0.3:6443"}`,
			},
			want: map[string]string{
				"10.96.0.10:53":   "10.244.2.3:53,10.244.2.5:53",
				"10.96.0.10:9153": "10.244.2.3:9153,10.244.2.5:9153",
				"10.96.0.1:443":   "172.19.0.3:6443",
			},
			wantErr: false,
		},
		{
			name:         "empty load balancer",
			loadBalancer: "my-lb",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer my-lb vips",
				Output: "",
			},
			want:    nil,
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
			got, err := GetLoadBalancerVIPs(tt.loadBalancer)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetLoadBalancerVIPs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLoadBalancerVIPs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeleteLoadBalancerVIP(t *testing.T) {
	tests := []struct {
		name         string
		loadBalancer string
		vip          string
		ovnCmd       ovntest.ExpectedCmd
		wantErr      bool
	}{
		{
			name:         "loadbalancer with VIPs",
			loadBalancer: "my-lb",
			vip:          "10.96.0.10:53",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer my-lb vips "10.96.0.10:53"`,
				Output: `{"10.96.0.10:53"="10.244.2.3:53,10.244.2.5:53", "10.96.0.10:9153"="10.244.2.3:9153,10.244.2.5:9153", "10.96.0.1:443"="172.19.0.3:6443"}`,
			},
			wantErr: false,
		},
		{
			name:         "load balancer and OVN error",
			loadBalancer: "my-lb",
			vip:          "10.96.0.10:53",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer my-lb vips "10.96.0.10:53"`,
				Output: "",
				Err:    fmt.Errorf("error while removing ACL: sw1, from switches"),
			},
			wantErr: true,
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
			txn := util.NewNBTxn()
			err = DeleteLoadBalancerVIP(txn, tt.loadBalancer, tt.vip)
			if err != nil {
				t.Errorf("DeleteLoadBalancerVIP error: %v", err)
			}
			_, _, err = txn.Commit()
			if (err != nil) != tt.wantErr {
				t.Errorf("DeleteLoadBalancerVIP() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestUpdateLoadBalancer(t *testing.T) {
	type args struct {
		lb      string
		vip     string
		targets []string
	}
	tests := []struct {
		name    string
		args    args
		ovnCmd  ovntest.ExpectedCmd
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			if err := UpdateLoadBalancer(tt.args.lb, tt.args.vip, tt.args.targets); (err != nil) != tt.wantErr {
				t.Errorf("UpdateLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestGetLogicalSwitchesForLoadBalancer(t *testing.T) {
	type args struct {
		lb string
	}
	tests := []struct {
		name    string
		args    args
		ovnCmd  ovntest.ExpectedCmd
		want    []string
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			got, err := GetLogicalSwitchesForLoadBalancer(tt.args.lb)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetLogicalSwitchesForLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLogicalSwitchesForLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLogicalRoutersForLoadBalancer(t *testing.T) {
	type args struct {
		lb string
	}
	tests := []struct {
		name    string
		args    args
		ovnCmd  ovntest.ExpectedCmd
		want    []string
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			got, err := GetLogicalRoutersForLoadBalancer(tt.args.lb)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetLogicalRoutersForLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetLogicalRoutersForLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetGRLogicalSwitchForLoadBalancer(t *testing.T) {
	type args struct {
		lb string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetGRLogicalSwitchForLoadBalancer(tt.args.lb)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetGRLogicalSwitchForLoadBalancer() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetGRLogicalSwitchForLoadBalancer() = %v, want %v", got, tt.want)
			}
		})
	}
}
