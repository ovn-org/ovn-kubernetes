package status_manager

import (
	"context"
	"reflect"
	"strings"

	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedrouteapply "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/applyconfiguration/adminpolicybasedroute/v1"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	adminpolicybasedroutelisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/listers/adminpolicybasedroute/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type apbRouteManager struct {
	lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister
	client adminpolicybasedrouteclientset.Interface
}

func newAPBRouteManager(lister adminpolicybasedroutelisters.AdminPolicyBasedExternalRouteLister, client adminpolicybasedrouteclientset.Interface) *apbRouteManager {
	return &apbRouteManager{
		lister: lister,
		client: client,
	}
}

func (m *apbRouteManager) get(namespace, name string) (*adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, error) {
	return m.lister.Get(name)
}

func (m *apbRouteManager) statusChanged(oldObj, newObj *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute) bool {
	return !reflect.DeepEqual(oldObj.Status.Messages, newObj.Status.Messages)
}

func (m *apbRouteManager) updateStatus(route *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, applyOpts *metav1.ApplyOptions) error {
	if route == nil || len(route.Status.Messages) == 0 {
		return nil
	}
	newStatus := adminpolicybasedrouteapi.SuccessStatus
	for _, message := range route.Status.Messages {
		if strings.Contains(message, types.APBRouteErrorMsg) {
			newStatus = adminpolicybasedrouteapi.FailStatus
			break
		}
	}
	if route.Status.Status == newStatus {
		return nil
	}

	applyObj := adminpolicybasedrouteapply.AdminPolicyBasedExternalRoute(route.Name).
		WithStatus(adminpolicybasedrouteapply.AdminPolicyBasedRouteStatus().
			WithStatus(newStatus))
	_, err := m.client.K8sV1().AdminPolicyBasedExternalRoutes().ApplyStatus(context.TODO(), applyObj, *applyOpts)
	return err
}
