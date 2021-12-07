package unidling

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"

	kapi "k8s.io/api/core/v1"
)

// Ensure VIP and Protocol are correctly scraped from event
func Test_extractEmptyLBBackendsEvents(t *testing.T) {
	var chassisUUID = "bea419d0-ff07-4e9d-878d-4e204eaeae20"
	var eventUUID = libovsdbops.BuildNamedUUID()
	var emptyEvent = &sbdb.ControllerEvent{
		UUID:      eventUUID,
		Chassis:   &chassisUUID,
		EventInfo: map[string]string{"loadbalancer": "420a18ee-0c34-4f87-81a3-39cb11c5e6be", "protocol": "tcp", "vip": "172.30.72.79:80"},
		EventType: sbdb.ControllerEventEventTypeEmptyLbBackends,
		SeqNum:    8,
	}

	tests := []struct {
		name    string
		events  []sbdb.ControllerEvent
		want    []emptyLBBackendEvent
		wantErr bool
	}{
		{
			name: "loadbalancer empty event",
			events: []sbdb.ControllerEvent{
				*emptyEvent,
				{
					UUID:      "fake",
					EventType: "fake",
				},
			},
			want: []emptyLBBackendEvent{
				{
					vip:             "172.30.72.79:80",
					protocol:        kapi.ProtocolTCP,
					controllerEvent: emptyEvent,
				},
			},
			wantErr: false,
		},
		{
			name:    "no events",
			events:  []sbdb.ControllerEvent{},
			want:    []emptyLBBackendEvent{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractEmptyLBBackendsEvents(tt.events)
			if (err != nil) != tt.wantErr {
				t.Errorf("extractEmptyLBBackendsEvents() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("extractEmptyLBBackendsEvents() = %v, want %v", got, tt.want)
			}
			if len(got) == 1 && len(tt.want) == 1 {
				// Just check the VIP and Protocol were scraped correctly
				if !(got[0].vip == tt.want[0].vip && string(got[0].protocol) == string(tt.want[0].protocol)) {
					t.Errorf("extractEmptyLBBackendsEvents() = %v, want %v", got, tt.want)
				}
			}

		})
	}
}
