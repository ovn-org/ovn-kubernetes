package services

import (
	"reflect"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	utilpointer "k8s.io/utils/pointer"
)

func Test_getLbEndpoints(t *testing.T) {
	type args struct {
		slices  []*discovery.EndpointSlice
		svcPort v1.ServicePort
		family  v1.IPFamily
	}
	tests := []struct {
		name string
		args args
		want []string
	}{
		{
			name: "empty slices",
			args: args{
				slices: []*discovery.EndpointSlice{},
				svcPort: v1.ServicePort{
					Name:       "tcp-example",
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
				},
				family: v1.IPv4Protocol,
			},
			want: []string{},
		},
		{
			name: "slices with endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     utilpointer.StringPtr("tcp-example"),
								Protocol: protoPtr(v1.ProtocolTCP),
								Port:     utilpointer.Int32Ptr(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.0.0.2"},
							},
						},
					},
				},
				svcPort: v1.ServicePort{
					Name:       "tcp-example",
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
				},
				family: v1.IPv4Protocol,
			},
			want: []string{"10.0.0.2:80"},
		},
		{
			name: "slices with different ports",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     utilpointer.StringPtr("tcp-example"),
								Protocol: protoPtr(v1.ProtocolTCP),
								Port:     utilpointer.Int32Ptr(int32(8080)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.0.0.2"},
							},
						},
					},
				},
				svcPort: v1.ServicePort{
					Name:       "tcp-example",
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
				},
				family: v1.IPv4Protocol,
			},
			want: []string{},
		},
		{
			name: "slices with different IP family",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     utilpointer.StringPtr("tcp-example"),
								Protocol: protoPtr(v1.ProtocolTCP),
								Port:     utilpointer.Int32Ptr(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv6,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"2001:db2::2"},
							},
						},
					},
				},
				svcPort: v1.ServicePort{
					Name:       "tcp-example",
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
				},
				family: v1.IPv4Protocol,
			},
			want: []string{},
		},
		{
			name: "multiples slices with duplicate endpoints",
			args: args{
				slices: []*discovery.EndpointSlice{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     utilpointer.StringPtr("tcp-example"),
								Protocol: protoPtr(v1.ProtocolTCP),
								Port:     utilpointer.Int32Ptr(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.0.0.2", "10.1.1.2"},
							},
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "svc-ab23",
							Namespace: "ns",
							Labels:    map[string]string{discovery.LabelServiceName: "svc"},
						},
						Ports: []discovery.EndpointPort{
							{
								Name:     utilpointer.StringPtr("tcp-example"),
								Protocol: protoPtr(v1.ProtocolTCP),
								Port:     utilpointer.Int32Ptr(int32(80)),
							},
						},
						AddressType: discovery.AddressTypeIPv4,
						Endpoints: []discovery.Endpoint{
							{
								Conditions: discovery.EndpointConditions{
									Ready: utilpointer.BoolPtr(true),
								},
								Addresses: []string{"10.0.0.2", "10.2.2.2"},
							},
						},
					},
				},
				svcPort: v1.ServicePort{
					Name:       "tcp-example",
					TargetPort: intstr.FromInt(80),
					Protocol:   v1.ProtocolTCP,
				},
				family: v1.IPv4Protocol,
			},
			want: []string{"10.0.0.2:80", "10.1.1.2:80", "10.2.2.2:80"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getLbEndpoints(tt.args.slices, tt.args.svcPort, tt.args.family); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getLbEndpoints() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_deleteVIPsFromOVN(t *testing.T) {
	const clusterPortGroupUUID = "c62dd6d4-38b3-11eb-b663-a8a1590cda29"
	type args struct {
		vips   sets.String
		svc    *v1.Service
		ovnCmd []ovntest.ExpectedCmd
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "empty",
			args: args{
				vips: sets.NewString(),
				svc:  &v1.Service{},
			},
			wantErr: false,
		},
		{
			name: "delete existing vip",
			args: args{
				vips: sets.NewString("10.0.0.1:80/TCP"),
				svc: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns"},
					Spec: v1.ServiceSpec{
						Type:       v1.ServiceTypeClusterIP,
						ClusterIP:  "10.0.0.1",
						ClusterIPs: []string{"10.0.0.1"},
						Selector:   map[string]string{"foo": "bar"},
						Ports: []v1.ServicePort{{
							Port:       80,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.FromInt(3456),
						}},
					},
				},
				ovnCmd: []ovntest.ExpectedCmd{
					{
						Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
						Output: loadbalancerTCP,
					},
					{
						Cmd:    `ovn-nbctl --timeout=15 --if-exists remove load_balancer a08ea426-2288-11eb-a30b-a8a1590cda29 vips "10.0.0.1:80"`,
						Output: "",
					},
					{
						Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=a08ea426-2288-11eb-a30b-a8a1590cda29-10.0.0.1\:80`,
						Output: "",
					},
					{
						Cmd:    `ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null`,
						Output: "",
					},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newServiceTracker()
			if len(tt.args.svc.Spec.ClusterIP) > 0 {
				st.updateKubernetesService(tt.args.svc)
			}
			// Expected OVN commands
			fexec := ovntest.NewFakeExec()
			for _, cmd := range tt.args.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)
			}
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			if err := deleteVIPsFromOVN(tt.args.vips, st, tt.args.svc.Name, tt.args.svc.Namespace, clusterPortGroupUUID); (err != nil) != tt.wantErr {
				t.Errorf("deleteVIPsFromOVN() error = %v, wantErr %v", err, tt.wantErr)
			}

			if len(tt.args.svc.Spec.ClusterIP) > 0 {
				vip := util.JoinHostPortInt32(tt.args.svc.Spec.ClusterIP, tt.args.svc.Spec.Ports[0].Port)
				if st.hasServiceVIP(tt.args.svc.Name, tt.args.svc.Namespace, vip, tt.args.svc.Spec.Ports[0].Protocol) {
					t.Fatalf("Error: VIP not deleted from Service Tracker")
				}
			}
		})
	}
}
