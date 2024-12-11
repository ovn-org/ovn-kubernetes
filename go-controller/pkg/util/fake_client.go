package util

import (
	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpfake "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/fake"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadfake "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	cloudservicefake "github.com/openshift/client-go/cloudnetwork/clientset/versioned/fake"
	ocpnetworkclientfake "github.com/openshift/client-go/network/clientset/versioned/fake"
	adminpolicybasedrouteapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	adminpolicybasedroutefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned/fake"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned/fake"
	egressip "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressipfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/fake"
	egressqos "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressqosfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned/fake"
	egressservice "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"
	egressservicefake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned/fake"
	routeadvertisements "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	routeadvertisementsfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfake "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

func GetOVNClientset(objects ...runtime.Object) *OVNClientset {
	egressIPObjects := []runtime.Object{}
	egressFirewallObjects := []runtime.Object{}
	egressQoSObjects := []runtime.Object{}
	multiNetworkPolicyObjects := []runtime.Object{}
	egressServiceObjects := []runtime.Object{}
	apbExternalRouteObjects := []runtime.Object{}
	anpObjects := []runtime.Object{}
	v1Objects := []runtime.Object{}
	nads := []runtime.Object{}
	cloudObjects := []runtime.Object{}
	dnsNameResolverObjects := []runtime.Object{}
	udnObjects := []runtime.Object{}
	raObjects := []runtime.Object{}
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
		default:
			v1Objects = append(v1Objects, object)
		}
	}

	nadClient := nadfake.NewSimpleClientset(nads...)
	// the NAD fake-client tracker must be populated manually because the NAD CRD use arbitrary API registration name
	// that cannot be resolved by the underlying API machinery [1] [2].
	// [1] https://github.com/ovn-org/ovn-kubernetes/blob/65c79af35b2c22f90c863debefa15c4fb1f088cb/go-controller/vendor/k8s.io/client-go/testing/fixture.go#L341
	// [2] https://github.com/ovn-org/ovn-kubernetes/commit/434b0590ce8c61ade75edc996b2f7f83d530f840#diff-ae287d8b2b115068905d4b5bf477d0e8cb6586d271fe872ca3b17acc94f21075R140
	populateTracker(nadClient, nads...)

	return &OVNClientset{
		KubeClient:                fake.NewSimpleClientset(v1Objects...),
		ANPClient:                 anpfake.NewSimpleClientset(anpObjects...),
		EgressIPClient:            egressipfake.NewSimpleClientset(egressIPObjects...),
		EgressFirewallClient:      egressfirewallfake.NewSimpleClientset(egressFirewallObjects...),
		CloudNetworkClient:        cloudservicefake.NewSimpleClientset(cloudObjects...),
		EgressQoSClient:           egressqosfake.NewSimpleClientset(egressQoSObjects...),
		NetworkAttchDefClient:     nadClient,
		MultiNetworkPolicyClient:  mnpfake.NewSimpleClientset(multiNetworkPolicyObjects...),
		EgressServiceClient:       egressservicefake.NewSimpleClientset(egressServiceObjects...),
		AdminPolicyRouteClient:    adminpolicybasedroutefake.NewSimpleClientset(apbExternalRouteObjects...),
		OCPNetworkClient:          ocpnetworkclientfake.NewSimpleClientset(dnsNameResolverObjects...),
		UserDefinedNetworkClient:  udnfake.NewSimpleClientset(udnObjects...),
		RouteAdvertisementsClient: routeadvertisementsfake.NewSimpleClientset(raObjects...),
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

func NewNamespace(namespace string) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: NewObjectMetaWithLabels(namespace, "", map[string]string{"name": namespace}),
		Spec:       v1.NamespaceSpec{},
		Status:     v1.NamespaceStatus{},
	}
}
