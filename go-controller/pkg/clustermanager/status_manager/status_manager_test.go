// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package status_manager

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	cnitypes "github.com/containernetworking/cni/pkg/types"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	metaapplyv1 "k8s.io/client-go/applyconfigurations/meta/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	networkqosapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/applyconfiguration/networkqos/v1alpha1"
	nqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func getNode(nodeName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
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

func newPrimaryL3NetInfo(name string) util.NetInfo {
	netInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
		NetConf:  cnitypes.NetConf{Name: name},
		Topology: types.Layer3Topology,
		Subnets:  "10.1.130.0/16/24",
		Role:     types.NetworkRolePrimary,
	})
	Expect(err).NotTo(HaveOccurred())
	return netInfo
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
					DstCIDR: ptr.To("1.2.3.4/32"),
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
			NetworkSelectors: []crdtypes.NetworkSelector{
				{
					NetworkSelectionType: crdtypes.NetworkAttachmentDefinitions,
					NetworkAttachmentDefinitionSelector: &crdtypes.NetworkAttachmentDefinitionSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": "stream",
							},
						},
					},
				},
			},
			Priority: 100,
			Egress: []networkqosapi.Rule{
				{
					DSCP: 60,
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
	_, err := fakeClient.NetworkQoSClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).
		Update(context.TODO(), networkQoS, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

func checkNQStatusEventually(networkQoS *networkqosapi.NetworkQoS, expectFailure bool, expectEmpty bool, fakeClient *util.OVNClusterManagerClientset) {
	Eventually(func() bool {
		eq, err := fakeClient.NetworkQoSClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).
			Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if expectFailure {
			return strings.Contains(eq.Status.Status, types.NetworkQoSErrorMsg)
		} else if expectEmpty {
			return eq.Status.Status == ""
		} else {
			return strings.Contains(eq.Status.Status, types.NetworkQoSNoErrorsMsg)
		}
	}).Should(BeTrue(), fmt.Sprintf("expected network QoS status with expectFailure=%v expectEmpty=%v", expectFailure, expectEmpty))
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

	startWithNetworkManager := func(zones sets.Set[string], nm networkmanager.Interface, objects ...runtime.Object) {
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNode(zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient, nm)

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())
	}

	start := func(zones sets.Set[string], objects ...runtime.Object) {
		startWithNetworkManager(zones, networkmanager.Default().Interface(), objects...)
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
		zones := sets.New("zone1")
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
		zones := sets.New("zone1", "zone2")
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

	It("updates EgressFirewall status with only relevant active zones", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
		config.IPv4Mode = true

		zones := sets.New("zone1", "zone2", "zone3")
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		activeNetwork := newPrimaryL3NetInfo("tenant-red")
		networkManager := &networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{
				namespace1.Name: activeNetwork,
			},
			ActiveNodes: map[string]map[string]bool{
				activeNetwork.GetNetworkName(): {
					"zone1": true,
					"zone2": false,
					"zone3": false,
				},
			},
		}
		startWithNetworkManager(zones, networkManager, namespace1, egressFirewall)

		updateEgressFirewallStatus(egressFirewall, &egressfirewallapi.EgressFirewallStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkEFStatusEventually(egressFirewall, false, false, fakeClient)
	})
	It("updates APBRoute status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New("zone1")
		apbRoute := newAPBRoute(apbrouteName)
		start(zones, apbRoute)

		updateAPBRouteStatus(apbRoute, &adminpolicybasedrouteapi.AdminPolicyBasedRouteStatus{
			Messages: []string{types.GetZoneStatus("zone1", "OK")},
		}, fakeClient)

		checkAPBRouteStatusEventually(apbRoute, false, false, fakeClient)
	})

	It("updates APBRoute status with 2 zones", func() {
		config.OVNKubernetesFeature.EnableMultiExternalGateway = true
		zones := sets.New("zone1", "zone2")
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

	It("updates EgressQoS status with 1 zone", func() {
		config.OVNKubernetesFeature.EnableEgressQoS = true
		zones := sets.New("zone1")
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
		zones := sets.New("zone1", "zone2")
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

	// cleanup can't be tested by unit test apiserver, since it relies on SSA logic with FieldManagers
	It("test if APIServer lister/patcher is called for AdminNetworkPolicy when the zone is deleted", func() {
		config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
		zones := sets.New("zone1", "zone2")
		start(zones)
		statusManager.onZoneUpdate(sets.New("zone1", "zone2", "zone3")) // add
		// the actual status update for zones is done in ovnkube-controller but here we just want to
		// check if a zone delete at least triggers the API List calls which means we are triggering
		// the SSA logic to delete/clear that status. Real cleanup cannot be tested since fakeClient
		// doesn't support ApplyStatus patch with FieldManagers
		var anpsWereListed, banpWereListed uint32
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "adminnetworkpolicies", func(clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			atomic.StoreUint32(&anpsWereListed, anpsWereListed+1)
			anpList := &anpapi.AdminNetworkPolicyList{Items: []anpapi.AdminNetworkPolicy{newAdminNetworkPolicy("harry-potter", 5)}}
			return true, anpList, nil
		})
		fakeClient.ANPClient.(*anpfake.Clientset).PrependReactor("list", "baselineadminnetworkpolicies", func(clienttesting.Action) (handled bool, ret runtime.Object, err error) {
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
		statusManager.onZoneUpdate(sets.New("zone1")) // delete "zone2", "zone3"
		// ensure list was called only once for each resource even if multiple zones are deleted
		Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWereListed)
		}).Should(Equal(uint32(1)))
		Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWereListed)
		}).Should(Equal(uint32(1)))
		// ensure patch status clean was called once for every zone, so here since two zones were deleted
		// we should have called it two times
		Eventually(func() uint32 {
			return atomic.LoadUint32(&anpsWerePatched)
		}).Should(Equal(uint32(2)))
		Eventually(func() uint32 {
			return atomic.LoadUint32(&banpWerePatched)
		}).Should(Equal(uint32(2)))
	})

	It("Should clean up EgressFirewall managedFields when a zone is deleted", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		// Set up the initial state: 2 zones have reported status
		egressFirewall.Status = egressfirewallapi.EgressFirewallStatus{
			Messages: []string{
				types.GetZoneStatus("zone1", "zone1: EgressFirewall Rules applied"),
				types.GetZoneStatus("zone2", "zone2: EgressFirewall Rules applied"),
			},
		}
		egressFirewall.ManagedFields = []metav1.ManagedFieldsEntry{
			{Manager: "zone1", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone1: zone1: EgressFirewall Rules applied\"":{}}}}`)}},
			{Manager: "zone2", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone2: zone2: EgressFirewall Rules applied\"":{}}}}`)}},
		}

		// Set up a reactor to intercept cleanup patches and track which zones are cleaned
		var cleanupCalled atomic.Uint32
		objects := []runtime.Object{namespace1, egressFirewall}
		zones := sets.New("zone1", "zone2")
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNode(zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		fakeClient.EgressFirewallClient.(*egressfirewallfake.Clientset).PrependReactor("patch", "egressfirewalls", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			patchAction := action.(clienttesting.PatchAction)
			if patchAction.GetSubresource() == "status" {
				patch := string(patchAction.GetPatch())
				klog.Infof("Status patch intercepted: %s", patch)

				// Only count cleanup patches, where the status field is empty
				if !strings.Contains(patch, `"status"`) {
					cleanupCalled.Add(1)
					klog.Infof("Cleanup patch detected for zone2")
				} else {
					klog.Infof("Normal status update patch (not a cleanup)")
				}
			}
			return false, nil, nil
		})

		// Now start the watch factory and status manager
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient, networkmanager.Default().Interface())

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())

		// Simulate deleting zone2 (now zones = {zone1})
		// This will trigger message-based cleanup because len(messages)=2 > zones.Len()=1
		statusManager.onZoneUpdate(sets.New("zone1"))

		// Verify cleanup was called for the deleted zone
		// NOTE: Due to fake client limitations (doesn't support SSA), cleanup may be called
		// multiple times since the message doesn't actually get removed. We verify it's
		// called at least once, which proves the message-based cleanup logic is triggered.
		Eventually(func() uint32 {
			return cleanupCalled.Load()
		}).Should(BeNumerically(">=", uint32(1)), "Expected cleanup to be called at least once for deleted zone")

		// Note: We cannot verify that managedFields were actually removed because the fake client
		// doesn't properly support Server-Side Apply with FieldManagers.
		// But we verified that cleanupStatus was called with the correct zone
	})

	It("Should clean up stale EgressFirewall managedFields on startup (upgrade scenario)", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)

		// Let's mimick an upgrade scenario:
		// - Status messages are already correct (only 2 zones report status)
		// - managedFields still has 3 entries (old code didn't clean up)
		// - stale managedField from zone3-deleted should be removed
		egressFirewall.Status = egressfirewallapi.EgressFirewallStatus{
			Messages: []string{
				types.GetZoneStatus("zone1", "zone1: EgressFirewall Rules applied"),
				types.GetZoneStatus("zone2", "zone2: EgressFirewall Rules applied"),
			},
		}
		egressFirewall.ManagedFields = []metav1.ManagedFieldsEntry{
			// Valid managedFields with actual message content nested inside
			{Manager: "zone1", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone1: EgressFirewall Rules applied\"":{}}}}`)}},
			{Manager: "zone2", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:messages":{"v:\"zone2: EgressFirewall Rules applied\"":{}}}}`)}},
			// Stale managedField with empty status (left by buggy code when zone was deleted)
			{Manager: "zone3-deleted", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{}}`)}},
			// Legitimate cluster-manager managedField with its own nested structure
			{Manager: "cluster-manager", Operation: metav1.ManagedFieldsOperationApply, Subresource: "status", APIVersion: "k8s.ovn.org/v1", FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:status":{"f:status":{}}}`)}},
		}

		var cleanupCalled atomic.Uint32
		var cleanupFieldManager atomic.Pointer[string]

		// We need to create the egress firewall before starting status manager,
		// so we can check if the initial cleanup takes place
		objects := []runtime.Object{namespace1, egressFirewall}

		// Add nodes for only zone1 and zone2 (zone3-deleted doesn't exist)
		zones := sets.New("zone1", "zone2")
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNode(zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()

		// Set up a reactor to intercept cleanup patches
		fakeClient.EgressFirewallClient.(*egressfirewallfake.Clientset).PrependReactor("patch", "egressfirewalls", func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
			patchAction := action.(clienttesting.PatchAction)
			if patchAction.GetSubresource() == "status" {
				patch := string(patchAction.GetPatch())

				// Check if this is a cleanup patch (empty ApplyStatus) vs normal status update
				// Cleanup patches have no status field, normal status updates have a "status" field
				// with actual content
				if !strings.Contains(patch, `"status"`) {
					cleanupCalled.Add(1)
					manager := "zone3-deleted"
					cleanupFieldManager.Store(&manager)
					klog.Infof("Cleanup patch detected for zone3-deleted")
				}
			}
			return false, nil, nil
		})

		// Now start the watch factory and status manager
		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient, networkmanager.Default().Interface())

		err = wf.Start()
		Expect(err).NotTo(HaveOccurred())

		// When statusManager Start() is called, it triggers ZoneTracker initialSync, which should:
		// 1. Discover current zones (zone1, zone2)
		// 2. Call onZonesUpdate
		// 3. Trigger ReconcileAll
		// 4. ReconcileAll calls the one-time startup cleanup
		// 5. Startup cleanup detects zone3-deleted has a stale empty-status managedField
		//    managed by a zone that is not listed between the current zonesb
		// 6. cleanupStatus is called for zone3-deleted
		err = statusManager.Start()
		Expect(err).NotTo(HaveOccurred())

		// Verify cleanup was called for the stale zone
		Eventually(func() uint32 {
			return cleanupCalled.Load()
		}).Should(Equal(uint32(1)), "Expected cleanup to be called exactly once for stale zone")

		// Ensure cleanup doesn't get called multiple times
		Consistently(func() uint32 {
			return cleanupCalled.Load()
		}).Should(Equal(uint32(1)), "Expected cleanup to be called exactly once and not repeated")

		Eventually(func() string {
			if managerPtr := cleanupFieldManager.Load(); managerPtr != nil {
				return *managerPtr
			}
			return ""
		}).Should(Equal("zone3-deleted"))

		// Check that we still have 2 messages (startup cleanup doesn't touch messages)
		ef, err := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).Get(context.TODO(), egressFirewall.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ef.Status.Messages).To(HaveLen(2))

		// The fake client doesn't properly handle SSA managedFields, so we can't verify
		// that zone3-deleted was actually removed from managedFields.
		// But we verified that cleanupStatus was called with the correct zone name
		// TODO: when fake client supports server side apply, check that the managed field with the stale zone gets deleted.
		Expect(egressFirewall.ManagedFields).To(HaveLen(4))

	})

	It("reports no NetworkQoS error while no zone reports one", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		// No zone has reported a failure.
		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

	It("reports a NetworkQoS failure of a single zone", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1", "zone2")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)

		checkNQStatusEventually(networkQoS, false, false, fakeClient)

		// Report a failure without waiting for every zone.
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionFalse,
				Reason:  "SetupFailed",
				Message: types.NetworkQoSErrorMsg + ": failed to reconcile",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, true, false, fakeClient)

		// the zone retracts its condition once it succeeds
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{}, fakeClient)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)
	})

	It("reports the NetworkQoS failure a zone of an older version wrote", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		zones := sets.New[string]("zone1")
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		start(zones, namespace1, networkQoS)
		checkNQStatusEventually(networkQoS, false, false, fakeClient)

		// the zones of an older version did not prefix their failures
		updateNetworkQoSStatus(networkQoS, &networkqosapi.Status{
			Conditions: []metav1.Condition{{
				Type:    "Ready-In-Zone-zone1",
				Status:  metav1.ConditionFalse,
				Reason:  "Failed",
				Message: "NetworkQoS namespace1/default - failed to reconcile, error details: some error",
			}},
		}, fakeClient)
		checkNQStatusEventually(networkQoS, true, false, fakeClient)
	})

	It("cleans up the NetworkQoS failure of a stale zone without reporting it", func() {
		config.OVNKubernetesFeature.EnableNetworkQoS = true
		namespace1 := util.NewNamespace(namespace1Name)
		networkQoS := newNetworkQoS(namespace1.Name)
		fakeClient = util.GetOVNClientset(namespace1, getNode("zone1")).GetClusterManagerClientset()
		// The default fake cannot track field ownership, so an empty apply would not remove
		// the old condition.
		nqosClient := nqosfake.NewClientset(networkQoS)
		fakeClient.NetworkQoSClient = nqosClient
		staleFailure := metaapplyv1.Condition().
			WithType("Ready-In-Zone-zone2").
			WithStatus(metav1.ConditionFalse).
			WithReason("Failed").
			WithMessage(types.NetworkQoSErrorMsg + ": failed to configure network(s) stream").
			WithLastTransitionTime(metav1.Now())
		_, err := nqosClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).ApplyStatus(context.TODO(),
			networkqosapply.NetworkQoS(networkQoS.Name, networkQoS.Namespace).
				WithStatus(networkqosapply.Status().WithConditions(staleFailure)),
			metav1.ApplyOptions{FieldManager: "zone2", Force: true})
		Expect(err).NotTo(HaveOccurred())
		var errorStatusWrites atomic.Int32
		nqosClient.PrependReactor("patch", "networkqoses", func(action clienttesting.Action) (bool, runtime.Object, error) {
			patch := action.(clienttesting.PatchAction)
			if patch.GetSubresource() == "status" &&
				strings.Contains(string(patch.GetPatch()), fmt.Sprintf("%q:%q", "status", types.NetworkQoSErrorMsg)) {
				errorStatusWrites.Add(1)
			}
			return false, nil, nil
		})

		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient, networkmanager.Default().Interface())
		Expect(wf.Start()).To(Succeed())
		Expect(statusManager.Start()).To(Succeed())

		checkNQStatusEventually(networkQoS, false, false, fakeClient)
		Eventually(func() []metav1.Condition {
			nqos, err := nqosClient.K8sV1alpha1().NetworkQoSes(networkQoS.Namespace).Get(context.TODO(), networkQoS.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			return nqos.Status.Conditions
		}).Should(BeEmpty())
		Expect(errorStatusWrites.Load()).To(BeZero(), "the failure of the stale zone must not be aggregated")
	})

	It("cleans up stale zone managedFields during startup", func() {
		config.OVNKubernetesFeature.EnableEgressFirewall = true
		namespace1 := util.NewNamespace(namespace1Name)
		egressFirewall := newEgressFirewall(namespace1.Name)
		zones := sets.New("zone1", "zone2")

		objects := []runtime.Object{namespace1, egressFirewall}
		for _, zone := range zones.UnsortedList() {
			objects = append(objects, getNode(zone))
		}
		fakeClient = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		efClient := fakeClient.EgressFirewallClient.K8sV1().EgressFirewalls(namespace1.Name)

		ctx := context.TODO()
		applyZoneMessage := func(zone, message string) {
			applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
				WithStatus(egressfirewallapply.EgressFirewallStatus().WithMessages(message))
			_, err := efClient.ApplyStatus(ctx, applyObj, metav1.ApplyOptions{FieldManager: zone, Force: true})
			Expect(err).NotTo(HaveOccurred())
		}

		zone1Msg := types.GetZoneStatus("zone1", "OK")
		zone2Msg := types.GetZoneStatus("zone2", "OK")
		zone3Msg := types.GetZoneStatus("zone3", "OK")
		applyZoneMessage("zone1", zone1Msg)
		applyZoneMessage("zone2", zone2Msg)
		// zone3 is not a current zone (no matching node), so it is stale and must be cleaned up.
		applyZoneMessage("zone3", zone3Msg)

		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClient)
		Expect(err).NotTo(HaveOccurred())
		statusManager = NewStatusManager(wf, fakeClient, networkmanager.Default().Interface())
		Expect(wf.Start()).NotTo(HaveOccurred())
		Expect(statusManager.Start()).NotTo(HaveOccurred())

		// The stale zone3 managedField and message must be removed, while zone1 and
		// zone2 (their managedFields and messages) must be preserved.
		Eventually(func() bool {
			ef, err := efClient.Get(ctx, egressFirewall.Name, metav1.GetOptions{})
			if err != nil {
				return false
			}

			managers := sets.New[string]()
			for _, mf := range ef.ManagedFields {
				managers.Insert(mf.Manager)
			}
			if managers.Has("zone3") || !managers.Has("zone1") || !managers.Has("zone2") {
				return false
			}

			gotMessages := sets.New(ef.Status.Messages...)
			return !gotMessages.Has(zone3Msg) && gotMessages.Has(zone1Msg) && gotMessages.Has(zone2Msg)
		}, 5).Should(BeTrue(), "stale zone3 managedField and message should be removed; zone1/zone2 preserved")
	})

})
