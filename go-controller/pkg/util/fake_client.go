// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpfake "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/fake"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	cloudservicefake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"
	ocpnetworkclientfake "github.com/openshift/client-go/network/clientset/versioned/fake"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"

	adminpolicybasedrouteapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	networkconnect "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	egressfirewall "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservice "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	networkqos "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned/fake"
	observabilityconfig "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1"
	observabilityconfigfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/apis/clientset/versioned/fake"
	routeadvertisements "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	routeadvertisementsfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	uplinkv1alpha1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1"
	uplinkfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/uplink/v1alpha1/apis/clientset/versioned/fake"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
)

func GetOVNClientset(objects ...runtime.Object) *OVNClientset {
	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	egressQoSObjects := []runtime.Object{}
	multiNetworkPolicyObjects := []runtime.Object{}
	egressServiceObjects := []runtime.Object{}
	apbExternalRouteObjects := []runtime.Object{}
	anpObjects := []runtime.Object{}
	networkQoSObjects := []runtime.Object{}
	observabilityConfigObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	nads := []runtime.Object{}
	cloudObjects := []runtime.Object{}
	dnsNameResolverObjects := []runtime.Object{}
	udnObjects := []runtime.Object{}
	raObjects := []runtime.Object{}
	frrObjects := []runtime.Object{}
	networkConnectObjects := []runtime.Object{}
	uplinkObjects := []runtime.Object{}
	vtepObjects := []runtime.Object{}
	for _, object := range objects {
		switch object.(type) {
		case *egressip.EgressIP:
			egressIPObjects = append(egressIPObjects, object)
		case *egressfirewall.EgressFirewall:
			egressFirewallObjects = append(egressFirewallObjects, object)
		case *egressqos.EgressQoS:
			egressQoSObjects = append(egressQoSObjects, object)
		case *ocpcloudnetworkapi.CloudPrivateIPConfig:
			cloudObjects = append(cloudObjects, object)
		case *mnpapi.MultiNetworkPolicy:
			multiNetworkPolicyObjects = append(multiNetworkPolicyObjects, object)
		case *egressservice.EgressService:
			egressServiceObjects = append(egressServiceObjects, object)
		case *nettypes.NetworkAttachmentDefinition:
			nads = append(nads, object)
		case *adminpolicybasedrouteapi.AdminPolicyBasedExternalRoute:
			apbExternalRouteObjects = append(apbExternalRouteObjects, object)
		case *anpapi.AdminNetworkPolicy:
			anpObjects = append(anpObjects, object)
		case *ocpnetworkapiv1alpha1.DNSNameResolver:
			dnsNameResolverObjects = append(dnsNameResolverObjects, object)
		case *udnv1.UserDefinedNetwork, *udnv1.ClusterUserDefinedNetwork:
			udnObjects = append(udnObjects, object)
		case *routeadvertisements.RouteAdvertisements:
			raObjects = append(raObjects, object)
		case *frrapi.FRRConfiguration:
			frrObjects = append(frrObjects, object)
		case *networkqos.NetworkQoS:
			networkQoSObjects = append(networkQoSObjects, object)
		case *observabilityconfig.ObservabilityConfig:
			observabilityConfigObjects = append(observabilityConfigObjects, object)
		case *networkconnect.ClusterNetworkConnect:
			networkConnectObjects = append(networkConnectObjects, object)
		case *uplinkv1alpha1.Uplink, *uplinkv1alpha1.UplinkState:
			uplinkObjects = append(uplinkObjects, object)
		case *vtepv1.VTEP:
			vtepObjects = append(vtepObjects, object)
		default:
			v1Objects = append(v1Objects, object)
		}
	}

	nadClient := nadfake.NewSimpleClientset(nads...)
	// the NAD fake-client tracker must be populated manually because the NAD CRD use arbitrary API registration name
	// that cannot be resolved by the underlying API machinery [1] [2].
	// [1] https://github.com/ovn-kubernetes/ovn-kubernetes/blob/65c79af35b2c22f90c863debefa15c4fb1f088cb/go-controller/vendor/k8s.io/client-go/testing/fixture.go#L341
	// [2] https://github.com/ovn-kubernetes/ovn-kubernetes/commit/434b0590ce8c61ade75edc996b2f7f83d530f840#diff-ae287d8b2b115068905d4b5bf477d0e8cb6586d271fe872ca3b17acc94f21075R140
	populateTracker(nadClient, nads...)

	return &OVNClientset{
		KubeClient:                fake.NewSimpleClientset(v1Objects...),
		ANPClient:                 anpfake.NewSimpleClientset(anpObjects...),
		EgressIPClient:            egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient:      egressfirewallfake.NewClientset(egressFirewallObjects...),
		CloudNetworkClient:        cloudservicefake.NewSimpleClientset(cloudObjects...),
		EgressQoSClient:           egressqosfake.NewSimpleClientset(egressQoSObjects...),
		NetworkAttchDefClient:     nadClient,
		MultiNetworkPolicyClient:  mnpfake.NewSimpleClientset(multiNetworkPolicyObjects...),
		EgressServiceClient:       egressservicefake.NewSimpleClientset(egressServiceObjects...),
		AdminPolicyRouteClient:    adminpolicybasedroutefake.NewSimpleClientset(apbExternalRouteObjects...),
		OCPNetworkClient:          ocpnetworkclientfake.NewSimpleClientset(dnsNameResolverObjects...),
		UserDefinedNetworkClient:  udnfake.NewSimpleClientset(udnObjects...),
		RouteAdvertisementsClient: routeadvertisementsfake.NewSimpleClientset(raObjects...),
		FRRClient:                 frrfake.NewSimpleClientset(frrObjects...),
		NetworkQoSClient:          networkqosfake.NewSimpleClientset(networkQoSObjects...),
		ObservabilityConfigClient: observabilityconfigfake.NewSimpleClientset(observabilityConfigObjects...),
		NetworkConnectClient:      networkconnectfake.NewSimpleClientset(networkConnectObjects...),
		UplinkClient:              uplinkfake.NewSimpleClientset(uplinkObjects...),
		VTEPClient:                vtepfake.NewSimpleClientset(vtepObjects...),
	}
}

// populateTracker populate the NAD fake-client internal tracker with NAD objects
func populateTracker(nadClient *nadfake.Clientset, objects ...runtime.Object) {
	nadGVR := schema.GroupVersionResource(metav1.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	})
	for _, obj := range objects {
		if nad, ok := obj.(*nettypes.NetworkAttachmentDefinition); ok {
			if err := nadClient.Tracker().Create(nadGVR, nad, nad.Namespace); err != nil {
				panic(err)
			}
		}
	}
}

func NewObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace + name),
		Name:      name,
		Namespace: namespace,
	}
}

func NewObjectMetaWithLabels(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace + name),
		Name:      name,
		Namespace: namespace,
		Labels:    labels,
	}
}

func NewNamespace(namespace string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: NewObjectMetaWithLabels(namespace, "", map[string]string{"name": namespace}),
		Spec:       corev1.NamespaceSpec{},
		Status:     corev1.NamespaceStatus{},
	}
}
