package status_manager

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/status_manager/zone_tracker"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"
)

func getNodeWithZone(nodeName, zoneName string) *v1.Node {
	annotations := map[string]string{}
	if zoneName != zone_tracker.UnknownZone {
		annotations[util.OvnNodeZoneName] = zoneName
	}
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
		},
	}
}

func newAdminNetworkPolicy(name string, priority int32) anpapi.AdminNetworkPolicy {
	return anpapi.AdminNetworkPolicy{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: anpapi.AdminNetworkPolicySpec{
			Priority: priority,
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newBaselineAdminNetworkPolicy(name string) anpapi.BaselineAdminNetworkPolicy {
	return anpapi.BaselineAdminNetworkPolicy{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: anpapi.BaselineAdminNetworkPolicySpec{
			Subject: anpapi.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{},
			},
		},
	}
}

func newEgressFirewall(namespace string) *egressfirewallapi.EgressFirewall {
	return &egressfirewallapi.EgressFirewall{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: egressfirewallapi.EgressFirewallSpec{
			Egress: []egressfirewallapi.EgressFirewallRule{
				{
					Type: "Allow",
					To: egressfirewallapi.EgressFirewallDestination{
						CIDRSelector: "1.2.3.4/23",
					},
				},
			},
		},
	}
}

func updateEgressFirewallStatus(egressFirewall *egressfirewallapi.EgressFirewall, status *egressfirewallapi.EgressFirewallStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	egressFirewall.Status = *status
	_, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
		Update(context.TODO(), egressFirewall, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkEFStatusEventually(egressFirewall *egressfirewallapi.EgressFirewall, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
			Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(ef.Status.Status, types.EgressFirewallErrorMsg)
		} else if expectEmpty {
			return ef.Status.Status == ""
		} else {
			return strings.Contains(ef.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected egress firewall status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyEFStatusConsistently(egressFirewall *egressfirewallapi.EgressFirewall, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).
			Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newAPBRoute(name string) *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute {
	return &adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute{
		ObjectMeta: util.NewObjectMeta(name, ""),
		Spec: adminpolicybasedrouteapi.AdminPolicyBasedExternalRouteSpec{
			From: adminpolicybasedrouteapi.ExternalNetworkSource{
				NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"name": "ns"}},
			},
			NextHops: adminpolicybasedrouteapi.ExternalNextHops{},
		},
	}
}

func updateAPBRouteStatus(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, status *adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	apbRoute.Status = *status
	_, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
		Update(context.TODO(), apbRoute, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkAPBRouteStatusEventually(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		route, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
			Get(context.TODO(), apbRoute.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return route.Status.Status == adminpolicybasedrouteapi.FailStatus
		} else if expectEmpty {
			return route.Status.Status == ""
		} else {
			return route.Status.Status == adminpolicybasedrouteapi.SuccessStatus
		}
	}).Should(BeTrue(), fmt.Sprintf("expected apbRoute status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyAPBRouteStatusConsistently(apbRoute *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.AdminPolicyRouteClient.K8sV1().AdminPolicyBasedExternalRoutes().
			Get(context.TODO(), apbRoute.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newEgressQoS(namespace string) *egressqosapi.EgressQoS {
	return &egressqosapi.EgressQoS{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: egressqosapi.EgressQoSSpec{
			Egress: []egressqosapi.EgressQoSRule{
				{
					DSCP:    60,
					DstCIDR: pointer.String("1.2.3.4/32"),
				},
			},
		},
	}
}

func updateEgressQoSStatus(egressQoS *egressqosapi.EgressQoS, status *egressqosapi.EgressQoSStatus,
	fakeClient *util.OVNClusterManagerClientset) {
	egressQoS.Status = *status
	_, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
		Update(context.TODO(), egressQoS, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkEQStatusEventually(egressQoS *egressqosapi.EgressQoS, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		eq, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
			Get(context.TODO(), egressQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(eq.Status.Status, types.EgressQoSErrorMsg)
		} else if expectEmpty {
			return eq.Status.Status == ""
		} else {
			return strings.Contains(eq.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected egress QoS status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyEQStatusConsistently(egressQoS *egressqosapi.EgressQoS, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.EgressQoSClient.K8sV1().EgressQoSes(egressQoS.Namespace).
			Get(context.TODO(), egressQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

func newNetworkQoS(namespace string) *networkqosapi.NetworkQoS {
	return &networkqosapi.NetworkQoS{
		ObjectMeta: util.NewObjectMeta("default", namespace),
		Spec: networkqosapi.Spec{
			NetworkAttachmentName: "default/stream",
			Egress: []networkqosapi.Rule{
				{
					Priority: 100,
					DSCP:     60,
					Classifier: networkqosapi.Classifier{
						To: []networkqosapi.Destination{
							{
								IPBlock: &networkingv1.IPBlock{
									CIDR: "1.2.3.4/32",
								},
							},
						},
					},
					Bandwidth: networkqosapi.Bandwidth{
						Rate:  100,
						Burst: 1000,
					},
				},
			},
		},
	}
}

func updateNetworkQoSStatus(networkQoS *networkqosapi.NetworkQoS, status *networkqosapi.Status,
	fakeClient *util.OVNClusterManagerClientset) {
	networkQoS.Status = *status
	_, err := fakeClient.NetworkQoSClient.K8sV1().NetworkQoSes(networkQoS.Namespace).
		Update(context.TODO(), networkQoS, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkNQStatusEventually(networkQoS *networkqosapi.NetworkQoS, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		eq, err := fakeClient.NetworkQoSClient.K8sV1().NetworkQoSes(networkQoS.Namespace).
			Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(eq.Status.Status, types.NetworkQoSErrorMsg)
		} else if expectEmpty {
			return eq.Status.Status == ""
		} else {
			return strings.Contains(eq.Status.Status, "applied")
		}
	}).Should(BeTrue(), fmt.Sprintf("expected network QoS status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
}

func checkEmptyNQStatusConsistently(networkQoS *networkqosapi.NetworkQoS, fakeClient *util.OVNClusterManagerClientset) {
	Consistently(func() bool {
		ef, err := fakeClient.NetworkQoSClient.K8sV1().NetworkQoSes(networkQoS.Namespace).
			Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return ef.Status.Status == ""
	}).Should(BeTrue(), "expected Status to be consistently empty")
}

var _ = Describe("Cluster Manager Status Manager", func() {
	var (
		statusManager *StatusManager
		wf            *factory.WatchFactory
		fakeClient    *util.OVNClusterManagerClientset
	)

	const (
		namespace1Name = "namespace1"
		apbrouteName   = "route"
	)

	start := func(zones sets.Set[string], objects ...runtime.Object) {
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNodeWithZone(zone, zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		wf = nil
		statusManager = nil
	})

	AfterEach(func() {
		if wf != nil {
			wf.Shutdown()
		}
		if statusManager != nil {
			statusManager.Stop()
		}
	})

	It("updates EgressFirewall status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})

	It("updates EgressFirewall status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New[string]("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkEFStatusEventually(egressFirewall, false, false, fakeClient)

	})

	It("updates EgressFirewall status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		start(zones, namespace1, egressFirewall)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyEFStatusConsistently(egressFirewall, fakeClient)
		// when new zone status is reported, status will be set
		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})
	It("updates APBRoute status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New[string]("zone1")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates APBRoute status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New[string]("zone1", "zone2")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)

	})

	It("updates APBRoute status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyAPBRouteStatusConsistently(apbRoute, fakeClient)
		// when new zone status is reported, status will be set
		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK"), types.GetZoneStatus("zone2", "OK")},
		}, fakeClient)
		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates EgressQoS status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)

		checkEQStatusEventually(egressQoS, false, false, fakeClient)
	})

	It("updates EgressQoS status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New[string]("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)

		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)

		checkEmptyEQStatusConsistently(egressQoS, fakeClient)

		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEQStatusEventually(egressQoS, false, false, fakeClient)

	})

	It("updates EgressQoS status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		egressQoS := newEgressQoS(namespace1.Name)
		start(zones, namespace1, egressQoS)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEmptyEQStatusConsistently(egressQoS, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyEQStatusConsistently(egressQoS, fakeClient)
		// when new zone status is reported, status will be set
		updateEgressQoSStatus(egressQoS, &egressqosapi.EgressQoSStatus{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "EgressQoS Rules applied",
			}},
		}, fakeClient)
		checkEQStatusEventually(egressQoS, false, false, fakeClient)
	})
	// cleanup can't be tested by unit test apiserver, since it relies on SSA logic with FieldManagers
	It("test if APIServer lister/patcher is called for AdminNetworkPolicy when the zone is deleted", func() {
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
		zones := sets.New[string]("zone1", "zone2")
		start(zones)
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2", "zone3")) // add
		// the actual status update for zones is done in ovnkube-controller but here we just want to
		// check if a zone delete at least triggers the API List calls which means we are triggering
		// the SSA logic to delete/clear that status. Real cleanup cannot be tested since fakeClient
		// doesn't support ApplyStatus patch with FieldManagers
		var anpsWereListed, banpWereListed uint32
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&anpsWereListed, anpsWereListed+1)
			anpList := &anpapi.AdminNetworkPolicyList{Items: []anpapi.AdminNetworkPolicy{newAdminNetworkPolicy("harry-potter", 5)}}
			return true, anpList, nil
		})
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&banpWereListed, banpWereListed+1)
			banpList := &anpapi.BaselineAdminNetworkPolicyList{Items: []anpapi.BaselineAdminNetworkPolicy{newBaselineAdminNetworkPolicy("default")}}
			return true, banpList, nil
		})
		var anpsWerePatched, banpWerePatched uint32
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("patch", "adminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&anpsWerePatched, anpsWerePatched+1)
			patch := action.(clienttesting.PatchAction)
			if action.GetSubresource() == "status" {
				klog.Infof("Got a patch status action for %v", patch.GetResource())
				return true, nil, nil
			}
			klog.Infof("Got a patch spec action for %v", patch.GetResource())
			return false, nil, nil
		})
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("patch", "baselineadminnetworkpolicies", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&banpWerePatched, banpWerePatched+1)
			patch := action.(clienttesting.PatchAction)
			if action.GetSubresource() == "status" {
				klog.Infof("Got a patch status action for %v", patch.GetResource())
				return true, nil, nil
			}
			klog.Infof("Got an patch spec action for %v", patch.GetResource())
			return false, nil, nil
		})
		statusManager.onZoneUpdate(sets.New[string]("zone1")) // delete "zone2", "zone3"
		// ensure list was called only once for each resource even if multiple zones are deleted
		gomega.Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWereListed)
		}).Should(gomega.Equal(uint32(1)))
		gomega.Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWereListed)
		}).Should(gomega.Equal(uint32(1)))
		// ensure patch status clean was called once for every zone, so here since two zones were deleted
		// we should have called it two times
		gomega.Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWerePatched)
		}).Should(gomega.Equal(uint32(2)))
		gomega.Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWerePatched)
		}).Should(gomega.Equal(uint32(2)))
	})

	It("updates NetworkQoS status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)

		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

	It("updates NetworkQoS status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)

		checkEmptyNQStatusConsistently(networkQoS, fakeClient)

		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)

	})

	It("updates NetworkQoS status with UnknownZone", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1", zone_tracker.UnknownZone)
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		// no matter how many messages are in the status, it won't be updated while UnknownZone is present
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkEmptyNQStatusConsistently(networkQoS, fakeClient)

		// when UnknownZone is removed, updates will be handled, but status from the new zone is not reported yet
		statusManager.onZoneUpdate(sets.New[string]("zone1", "zone2"))
		checkEmptyNQStatusConsistently(networkQoS, fakeClient)
		// when new zone status is reported, status will be set
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}, {
				Type:    "Ready-In-Zone-zone2",
				Status:  metav1.ConditionTrue,
				Reason:  "SetupSucceeded",
				Message: "NetworkQoS Destinations applied",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

})
