package services

import (
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExternalIDsForLoadBalancer(t *testing.T) {
	name := "svc-ab23"
	namespace := "ns"
	defaultNetInfo := util.DefaultNetInfo{}
	config.IPv4Mode = true
	UDNNetInfo, err := getSampleUDNNetInfo(namespace, "layer3")
	assert.Equal(t, err, nil)
	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&v1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
		},
		getExternalIDsForLoadBalancer(&v1.Service{
			// also handle no TypeMeta, which can happen.
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, &defaultNetInfo),
	)

	assert.Equal(t,
		map[string]string{
			types.LoadBalancerKindExternalID:  "Service",
			types.LoadBalancerOwnerExternalID: "ns/svc-ab23",
			types.NetworkExternalID:           UDNNetInfo.GetNetworkName(),
			types.NetworkRoleExternalID:       types.NetworkRolePrimary,
		},
		getExternalIDsForLoadBalancer(&v1.Service{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Service",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{discovery.LabelServiceName: "svc"},
			},
		}, UDNNetInfo),
	)

}
