package services

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1beta1"
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
		// TODO: Add test cases.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getLbEndpoints(tt.args.slices, tt.args.svcPort, tt.args.family); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getLbEndpoints() = %v, want %v", got, tt.want)
			}
		})
	}
}
