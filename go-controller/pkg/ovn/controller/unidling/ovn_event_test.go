package unidling

import (
	"reflect"
	"testing"

	kapi "k8s.io/api/core/v1"
)

func Test_extractEmptyLBBackendsEvents(t *testing.T) {
	type args struct {
		out []byte
	}
	tests := []struct {
		name    string
		args    args
		want    []emptyLBBackendEvent
		wantErr bool
	}{
		{
			name: "loadbalancer empty event",
			args: args{
				out: ([]byte)(`{"data":[[["uuid","d8d63259-0ce4-41e8-bcbd-967d896d5b54"],["uuid","bea419d0-ff07-4e9d-878d-4e204eaeae20"],["map",[["load_balancer","420a18ee-0c34-4f87-81a3-39cb11c5e6be"],["protocol","tcp"],["vip","172.30.72.79:80"]]],"empty_lb_backends",8]],"headings":["_uuid","chassis","event_info","event_type","seq_num"]}`),
			},
			want: []emptyLBBackendEvent{
				{
					vip:      "172.30.72.79:80",
					protocol: kapi.ProtocolTCP,
					uuid:     "d8d63259-0ce4-41e8-bcbd-967d896d5b54",
				},
			},
			wantErr: false,
		},
		{
			name: "loadbalancer wrong format  event",
			args: args{
				out: ([]byte)(`{"data":[[["uuid","d8d63259-0ce4-41e8-bcbd-967d896d5b54"],["uuid","bea419d0-ff07-4e9d-878d-4e204eaeae20"],["map",[["load_balancer","420a18ee-0c34-4f87-81a3-39cb11c5e6be"],["protocol","tcp"]],"empty_lb_backends",8]],"headings":["_uuid","chassis","event_info","event_type","seq_num"]}`),
			},
			want:    []emptyLBBackendEvent{},
			wantErr: true,
		},
		{
			name:    "no event",
			want:    []emptyLBBackendEvent{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractEmptyLBBackendsEvents(tt.args.out)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractEmptyLBBackendsEvents() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractEmptyLBBackendsEvents() = %v, want %v", got, tt.want)
			}
		})
	}
}
