package util

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/pkg/version"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	multinetworkpolicyclientset "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned"
	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	ocpcloudnetworkclientset "github.com/openshift/client-go/cloudnetwork/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	egressfirewallclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressipclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
	egressqosclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressserviceclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// OVNClientset is a wrapper around all clientsets used by OVN-Kubernetes
type OVNClientset struct {
	KubeClient               kubernetes.Interface
	EgressIPClient           egressipclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	CloudNetworkClient       ocpcloudnetworkclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	NetworkAttchDefClient    networkattchmentdefclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
}

// OVNMasterClientset
type OVNMasterClientset struct {
	KubeClient               kubernetes.Interface
	EgressIPClient           egressipclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
}

type OVNNodeClientset struct {
	KubeClient             kubernetes.Interface
	EgressServiceClient    egressserviceclientset.Interface
	AdminPolicyRouteClient adminpolicybasedrouteclientset.Interface
}

type OVNClusterManagerClientset struct {
	KubeClient            kubernetes.Interface
	EgressIPClient        egressipclientset.Interface
	CloudNetworkClient    ocpcloudnetworkclientset.Interface
	NetworkAttchDefClient networkattchmentdefclientset.Interface
	EgressServiceClient   egressserviceclientset.Interface
}

func (cs *OVNClientset) GetMasterClientset() *OVNMasterClientset {
	return &OVNMasterClientset{
		KubeClient:               cs.KubeClient,
		EgressIPClient:           cs.EgressIPClient,
		EgressFirewallClient:     cs.EgressFirewallClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
	}
}

func (cs *OVNClientset) GetClusterManagerClientset() *OVNClusterManagerClientset {
	return &OVNClusterManagerClientset{
		KubeClient:            cs.KubeClient,
		EgressIPClient:        cs.EgressIPClient,
		CloudNetworkClient:    cs.CloudNetworkClient,
		NetworkAttchDefClient: cs.NetworkAttchDefClient,
		EgressServiceClient:   cs.EgressServiceClient,
	}
}

func (cs *OVNClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:             cs.KubeClient,
		EgressServiceClient:    cs.EgressServiceClient,
		AdminPolicyRouteClient: cs.AdminPolicyRouteClient,
	}
}

func (cs *OVNMasterClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:          cs.KubeClient,
		EgressServiceClient: cs.EgressServiceClient,
	}
}

func adjustCommit() string {
	if len(config.Commit) < 12 {
		return "unknown"
	}
	return config.Commit[:12]
}

func adjustNodeName() string {
	hostName, err := os.Hostname()
	if err != nil {
		hostName = "unknown"
	}
	return hostName
}

// newKubernetesRestConfig create a Kubernetes rest config from either a kubeconfig,
// TLS properties, or an apiserver URL. If the CA certificate data is passed in the
// CAData in the KubernetesConfig, the CACert path is ignored.
func newKubernetesRestConfig(conf *config.KubernetesConfig) (*rest.Config, error) {
	var kconfig *rest.Config
	var err error

	if conf.Kubeconfig != "" {
		// uses the current context in kubeconfig
		kconfig, err = clientcmd.BuildConfigFromFlags("", conf.Kubeconfig)
	} else if strings.HasPrefix(conf.APIServer, "https") {
		if conf.Token == "" || len(conf.CAData) == 0 {
			return nil, fmt.Errorf("TLS-secured apiservers require token and CA certificate")
		}
		if _, err := cert.NewPoolFromBytes(conf.CAData); err != nil {
			return nil, err
		}
		kconfig = &rest.Config{
			Host:            conf.APIServer,
			BearerToken:     conf.Token,
			BearerTokenFile: conf.TokenFile,
			TLSClientConfig: rest.TLSClientConfig{CAData: conf.CAData},
		}
	} else if strings.HasPrefix(conf.APIServer, "http") {
		kconfig, err = clientcmd.BuildConfigFromFlags(conf.APIServer, "")
	} else {
		// Assume we are running from a container managed by kubernetes
		// and read the apiserver address and tokens from the
		// container's environment.
		kconfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	kconfig.QPS = 50
	kconfig.Burst = 50
	// if all the clients are behind HA-Proxy, then on the K8s API server side we only
	// see the HAProxy's IP and we can't tell the actual client making the request.
	kconfig.UserAgent = fmt.Sprintf("%s/%s@%s (%s/%s) kubernetes/%s",
		adjustNodeName(), filepath.Base(os.Args[0]), adjustCommit(), runtime.GOOS, runtime.GOARCH,
		version.Get().GitVersion)
	return kconfig, nil
}

// NewKubernetesClientset creates a Kubernetes clientset from a KubernetesConfig
func NewKubernetesClientset(conf *config.KubernetesConfig) (*kubernetes.Clientset, error) {
	kconfig, err := newKubernetesRestConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("unable to create kubernetes rest config, err: %v", err)
	}
	kconfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	kconfig.ContentType = "application/vnd.kubernetes.protobuf"
	clientset, err := kubernetes.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

// NewOVNClientset creates a OVNClientset from a KubernetesConfig
func NewOVNClientset(conf *config.KubernetesConfig) (*OVNClientset, error) {
	kclientset, err := NewKubernetesClientset(conf)
	if err != nil {
		return nil, err
	}
	kconfig, err := newKubernetesRestConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("unable to create kubernetes rest config, err: %v", err)
	}
	egressFirewallClientset, err := egressfirewallclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	egressIPClientset, err := egressipclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	cloudNetworkClientset, err := ocpcloudnetworkclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	egressqosClientset, err := egressqosclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	networkAttchmntDefClientset, err := networkattchmentdefclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}
	multiNetworkPolicyClientset, err := multinetworkpolicyclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}

	egressserviceClientset, err := egressserviceclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}

	adminPolicyBasedRouteClientset, err := adminpolicybasedrouteclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}

	return &OVNClientset{
		KubeClient:               kclientset,
		EgressIPClient:           egressIPClientset,
		EgressFirewallClient:     egressFirewallClientset,
		CloudNetworkClient:       cloudNetworkClientset,
		EgressQoSClient:          egressqosClientset,
		NetworkAttchDefClient:    networkAttchmntDefClientset,
		MultiNetworkPolicyClient: multiNetworkPolicyClientset,
		EgressServiceClient:      egressserviceClientset,
		AdminPolicyRouteClient:   adminPolicyBasedRouteClientset,
	}, nil
}

// IsClusterIPSet checks if the service is an headless service or not
func IsClusterIPSet(service *kapi.Service) bool {
	return service.Spec.ClusterIP != kapi.ClusterIPNone && service.Spec.ClusterIP != ""
}

// GetClusterIPs return an array with the ClusterIPs present in the service
// for backward compatibility with versions < 1.20
// we need to handle the case where only ClusterIP exist
func GetClusterIPs(service *kapi.Service) []string {
	if len(service.Spec.ClusterIPs) > 0 {
		clusterIPs := []string{}
		for _, clusterIP := range service.Spec.ClusterIPs {
			clusterIPs = append(clusterIPs, utilnet.ParseIPSloppy(clusterIP).String())
		}
		return clusterIPs
	}
	if len(service.Spec.ClusterIP) > 0 && service.Spec.ClusterIP != kapi.ClusterIPNone {
		return []string{utilnet.ParseIPSloppy(service.Spec.ClusterIP).String()}
	}
	return []string{}
}

// GetExternalAndLBIPs returns an array with the ExternalIPs and LoadBalancer IPs present in the service
func GetExternalAndLBIPs(service *kapi.Service) []string {
	svcVIPs := []string{}
	for _, externalIP := range service.Spec.ExternalIPs {
		parsedExternalIP := utilnet.ParseIPSloppy(externalIP)
		if parsedExternalIP != nil {
			svcVIPs = append(svcVIPs, parsedExternalIP.String())
		}
	}
	if ServiceTypeHasLoadBalancer(service) {
		for _, ingressVIP := range service.Status.LoadBalancer.Ingress {
			if len(ingressVIP.IP) > 0 {
				parsedIngressVIP := utilnet.ParseIPSloppy(ingressVIP.IP)
				if parsedIngressVIP != nil {
					svcVIPs = append(svcVIPs, parsedIngressVIP.String())
				}
			}
		}
	}
	return svcVIPs
}

// ValidatePort checks if the port is non-zero and port protocol is valid
func ValidatePort(proto kapi.Protocol, port int32) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid port number: %v", port)
	}
	return ValidateProtocol(proto)
}

// ValidateProtocol checks if the protocol is a valid kapi.Protocol type (TCP, UDP, or SCTP) or returns an error
func ValidateProtocol(proto kapi.Protocol) error {
	if proto == kapi.ProtocolTCP || proto == kapi.ProtocolUDP || proto == kapi.ProtocolSCTP {
		return nil
	}
	return fmt.Errorf("protocol %s is not a valid protocol", proto)
}

// ServiceTypeHasClusterIP checks if the service has an associated ClusterIP or not
func ServiceTypeHasClusterIP(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeClusterIP || service.Spec.Type == kapi.ServiceTypeNodePort || service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

func LoadBalancerServiceHasNodePortAllocation(service *kapi.Service) bool {
	return service.Spec.AllocateLoadBalancerNodePorts == nil || *service.Spec.AllocateLoadBalancerNodePorts
}

// ServiceTypeHasNodePort checks if the service has an associated NodePort or not
func ServiceTypeHasNodePort(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeNodePort ||
		(service.Spec.Type == kapi.ServiceTypeLoadBalancer && LoadBalancerServiceHasNodePortAllocation(service))
}

// ServiceTypeHasLoadBalancer checks if the service has an associated LoadBalancer or not
func ServiceTypeHasLoadBalancer(service *kapi.Service) bool {
	return service.Spec.Type == kapi.ServiceTypeLoadBalancer
}

func ServiceExternalTrafficPolicyLocal(service *kapi.Service) bool {
	return service.Spec.ExternalTrafficPolicy == kapi.ServiceExternalTrafficPolicyTypeLocal
}

func ServiceInternalTrafficPolicyLocal(service *kapi.Service) bool {
	return service.Spec.InternalTrafficPolicy != nil && *service.Spec.InternalTrafficPolicy == kapi.ServiceInternalTrafficPolicyLocal
}

// GetNodePrimaryIP extracts the primary IP address from the node status in the  API
func GetNodePrimaryIP(node *kapi.Node) (string, error) {
	if node == nil {
		return "", fmt.Errorf("invalid node object")
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeInternalIP {
			return utilnet.ParseIPSloppy(addr.Address).String(), nil
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == kapi.NodeExternalIP {
			return utilnet.ParseIPSloppy(addr.Address).String(), nil
		}
	}
	return "", fmt.Errorf("%s doesn't have an address with type %s or %s", node.GetName(),
		kapi.NodeInternalIP, kapi.NodeExternalIP)
}

// PodNeedsSNAT returns true if the given pod is eligible to setup snat entry
// in ovn for its egress traffic outside cluster, otherwise returns false.
func PodNeedsSNAT(pod *kapi.Pod) bool {
	return PodScheduled(pod) && !PodWantsHostNetwork(pod) && !PodCompleted(pod)
}

// PodWantsHostNetwork returns if the given pod is hostNetworked or not to determine if networking
// needs to be setup
func PodWantsHostNetwork(pod *kapi.Pod) bool {
	return pod.Spec.HostNetwork
}

// PodCompleted checks if the pod is marked as completed (in a terminal state)
func PodCompleted(pod *kapi.Pod) bool {
	return pod.Status.Phase == kapi.PodSucceeded || pod.Status.Phase == kapi.PodFailed
}

// PodScheduled returns if the given pod is scheduled
func PodScheduled(pod *kapi.Pod) bool {
	return pod.Spec.NodeName != ""
}

// PodTerminating checks if the pod has been deleted via API but still in the process of terminating
func PodTerminating(pod *kapi.Pod) bool {
	return pod.DeletionTimestamp != nil
}

// EventRecorder returns an EventRecorder type that can be
// used to post Events to different object's lifecycles.
func EventRecorder(kubeClient kubernetes.Interface) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(
		&typedcorev1.EventSinkImpl{
			Interface: kubeClient.CoreV1().Events(""),
		})
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		kapi.EventSource{Component: "controlplane"})
	return recorder
}

// UseEndpointSlices detect if Endpoints Slices are enabled in the cluster
func UseEndpointSlices(kubeClient kubernetes.Interface) bool {
	if _, err := kubeClient.Discovery().ServerResourcesForGroupVersion(discovery.SchemeGroupVersion.String()); err == nil {
		klog.V(2).Infof("Kubernetes Endpoint Slices enabled on the cluster: %s", discovery.SchemeGroupVersion.String())
		return true
	}
	return false
}

type LbEndpoints struct {
	V4IPs []string
	V6IPs []string
	Port  int32
}

// GetLbEndpoints returns the IPv4 and IPv6 addresses of eligible endpoints as slices inside a struct
func GetLbEndpoints(slices []*discovery.EndpointSlice, svcPort kapi.ServicePort, service *v1.Service) LbEndpoints {
	v4ips := sets.NewString()
	v6ips := sets.NewString()

	out := LbEndpoints{}
	// return an empty object so the caller doesn't have to check for nil and can use it as an iterator
	if len(slices) == 0 {
		return out
	}

	for _, slice := range slices {
		klog.V(4).Infof("Getting endpoints for slice %s/%s", slice.Namespace, slice.Name)

		// build the list of valid endpoints in the slice
		for _, port := range slice.Ports {
			// If Service port name is set, it must match the name field in the endpoint
			// If Service port name is not set, we just use the endpoint port
			if svcPort.Name != "" && svcPort.Name != *port.Name {
				klog.V(5).Infof("Slice %s with different Port name, requested: %s received: %s",
					slice.Name, svcPort.Name, *port.Name)
				continue
			}

			// Skip ports that don't match the protocol
			if *port.Protocol != svcPort.Protocol {
				klog.V(5).Infof("Slice %s with different Port protocol, requested: %s received: %s",
					slice.Name, svcPort.Protocol, *port.Protocol)
				continue
			}

			out.Port = *port.Port
			ForEachEligibleEndpoint(slice, service, func(endpoint discovery.Endpoint, shortcut *bool) {
				for _, ip := range endpoint.Addresses {
					klog.V(4).Infof("Adding slice %s endpoint: %v, port: %d", slice.Name, endpoint.Addresses, *port.Port)
					ipStr := utilnet.ParseIPSloppy(ip).String()
					switch slice.AddressType {
					case discovery.AddressTypeIPv4:
						v4ips.Insert(ipStr)
					case discovery.AddressTypeIPv6:
						v6ips.Insert(ipStr)
					default:
						klog.V(5).Infof("Skipping FQDN slice %s/%s", slice.Namespace, slice.Name)
					}
				}
			})
		}
	}

	out.V4IPs = v4ips.List()
	out.V6IPs = v6ips.List()
	klog.V(4).Infof("LB Endpoints for %s/%s are: %v / %v on port: %d",
		slices[0].Namespace, slices[0].Labels[discovery.LabelServiceName],
		out.V4IPs, out.V6IPs, out.Port)
	return out
}

type K8sObject interface {
	metav1.Object
	k8sruntime.Object
}

func ExternalIDsForObject(obj K8sObject) map[string]string {
	gk := obj.GetObjectKind().GroupVersionKind().GroupKind()
	nsn := k8stypes.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}

	if gk.String() == "" {
		kinds, _, err := scheme.Scheme.ObjectKinds(obj)
		if err != nil || len(kinds) == 0 || len(kinds) > 1 {
			klog.Warningf("BUG: object has no / ambiguous GVK: %#v, err", obj, err)
		}
		gk = kinds[0].GroupKind()
	}

	return map[string]string{
		types.LoadBalancerOwnerExternalID: nsn.String(),
		types.LoadBalancerKindExternalID:  gk.String(),
	}
}

// IsEndpointReady takes as input an endpoint from an endpoint slice and returns true if the endpoint is
// to be considered ready. Considering as ready an endpoint with Conditions.Ready==nil
// as per doc: "In most cases consumers should interpret this unknown state as ready"
// https://github.com/kubernetes/api/blob/0478a3e95231398d8b380dc2a1905972be8ae1d5/discovery/v1/types.go#L129-L131
func IsEndpointReady(endpoint discovery.Endpoint) bool {
	return endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
}

// IsEndpointServing takes as input an endpoint from an endpoint slice and returns true if the endpoint is
// to be considered serving. Falling back to IsEndpointReady when Serving field is nil, as per doc:
// "If nil, consumers should defer to the ready condition.
// https://github.com/kubernetes/api/blob/0478a3e95231398d8b380dc2a1905972be8ae1d5/discovery/v1/types.go#L138-L139
func IsEndpointServing(endpoint discovery.Endpoint) bool {
	if endpoint.Conditions.Serving != nil {
		return *endpoint.Conditions.Serving
	} else {
		return IsEndpointReady(endpoint)
	}
}

// IsEndpointEligible takes as input an endpoint from an endpoint slice and a boolean that indicates whether to include
// all terminating endpoints, as per the PublishNotReadyAddresses feature in kubernetes service spec. It always returns true
// if includeTerminating is true and falls back to IsEndpointServing otherwise.
func IsEndpointEligible(endpoint discovery.Endpoint, includeTerminating bool) bool {
	return includeTerminating || IsEndpointServing(endpoint)
}

// NoHostSubnet() compares the no-hostsubnet-nodes flag with node labels to see if the node is managing its
// own network.
func NoHostSubnet(node *v1.Node) bool {
	if config.Kubernetes.NoHostSubnetNodes == nil {
		return false
	}

	nodeSelector, _ := metav1.LabelSelectorAsSelector(config.Kubernetes.NoHostSubnetNodes)
	return nodeSelector.Matches(labels.Set(node.Labels))
}

// ForEachEligibleEndpoint iterates through each eligible endpoint in the given endpointslice and applies the input function fn to it.
// An endpoint is eligible if it is serving or if its corresponding service has Spec.PublishNotReadyAddresses set.
// PublishNotReadyAddresses tells endpoint consumers to disregard any indications of ready/not-ready and is generally used
// together with headless services so that DNS records of all endpoints (ready or not) are always published.
// Function fn takes a bool pointer "shortcut" that, when set to true inside fn, ends the iteration; this is useful when fn
// checks for a condition on the endpoints and we want to return as soon as the condition is satisfied.
func ForEachEligibleEndpoint(endpointSlice *discovery.EndpointSlice, service *kapi.Service, fn func(ep discovery.Endpoint, shortcut *bool)) {
	includeTerminating := service != nil && service.Spec.PublishNotReadyAddresses
	var shortcut bool
	for _, endpoint := range endpointSlice.Endpoints {
		if IsEndpointEligible(endpoint, includeTerminating) {
			fn(endpoint, &shortcut)
			if shortcut {
				// shortcircuit the whole iteration
				return
			}
		}
	}
}

// GetEndpointAddresses returns a list of IP addresses of all eligible endpoints in the given endpoint slices.
func GetEndpointAddressesWithCondition(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, fn func(discovery.Endpoint) bool) sets.Set[string] {
	endpointsAddress := sets.New[string]()
	for _, endpointSlice := range endpointSlices {
		ForEachEligibleEndpoint(endpointSlice, service, func(endpoint discovery.Endpoint, shortcut *bool) {
			includeEndpoint := fn == nil || fn(endpoint)
			if !includeEndpoint {
				return
			}
			for _, ip := range endpoint.Addresses {
				endpointsAddress.Insert(utilnet.ParseIPSloppy(ip).String())
			}
		})
	}
	return endpointsAddress
}

// GetEndpointAddresses returns a list of IP addresses of all eligible endpoints in the given endpoint slice.
func GetEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) sets.Set[string] {
	return GetEndpointAddressesWithCondition(endpointSlices, service, nil)
}

// GetLocalEndpointAddresses returns a list of endpoints that are local to the specified node
func GetLocalEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, nodeName string) sets.Set[string] {
	return GetEndpointAddressesWithCondition(endpointSlices, service, func(endpoint discovery.Endpoint) bool {
		return endpoint.NodeName != nil && *endpoint.NodeName == nodeName
	})
}

// HasLocalHostNetworkEndpoints returns true if any of the nodeAddresses appear in given the set of
// localEndpointAddresses. This is useful to check whether any of the provided local endpoints are host-networked.
func HasLocalHostNetworkEndpoints(localEndpointAddresses sets.Set[string], nodeAddresses []net.IP) bool {
	if len(localEndpointAddresses) == 0 || len(nodeAddresses) == 0 {
		return false
	}
	nodeAddressesSet := sets.New[string]()
	for _, ip := range nodeAddresses {
		nodeAddressesSet.Insert(ip.String())
	}
	return len(localEndpointAddresses.Intersection(nodeAddressesSet)) != 0
}

// DoesEndpointSliceContainEndpoint returns true if the endpointslice
// contains an endpoint with the given IP/Port/Protocol and this endpoint is considered eligible
func DoesEndpointSliceContainEndpoint(endpointSlice *discovery.EndpointSlice,
	epIP string, epPort int32, protocol kapi.Protocol, service *kapi.Service) bool {
	var res bool
	for _, port := range endpointSlice.Ports {
		ForEachEligibleEndpoint(endpointSlice, service, func(endpoint discovery.Endpoint, shortcut *bool) {
			for _, ip := range endpoint.Addresses {
				if utilnet.ParseIPSloppy(ip).String() == epIP && *port.Port == epPort && *port.Protocol == protocol {
					if shortcut != nil {
						*shortcut = true
					}
					res = true
					return
				}
			}
		})
	}
	return res
}

// ServiceNamespacedNameFromEndpointSlice returns the namespaced name of the service
// that corresponds to the given endpointSlice
func ServiceNamespacedNameFromEndpointSlice(endpointSlice *discovery.EndpointSlice) (k8stypes.NamespacedName, error) {
	var serviceNamespacedName k8stypes.NamespacedName
	svcName := endpointSlice.Labels[discovery.LabelServiceName]
	if svcName == "" {
		// should not happen, since the informer already filters out endpoint slices with an empty service label
		return serviceNamespacedName,
			fmt.Errorf("endpointslice %s/%s: empty value for label %s",
				endpointSlice.Namespace, endpointSlice.Name, discovery.LabelServiceName)
	}
	return k8stypes.NamespacedName{Namespace: endpointSlice.Namespace, Name: svcName}, nil
}

// isHostEndpoint determines if the given endpoint ip belongs to a host networked pod
func IsHostEndpoint(endpointIPstr string) bool {
	endpointIP := net.ParseIP(endpointIPstr)
	for _, clusterNet := range config.Default.ClusterSubnets {
		if clusterNet.CIDR.Contains(endpointIP) {
			return false
		}
	}
	return true
}
