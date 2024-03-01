package util

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/pkg/version"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate"
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
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

// OVNClientset is a wrapper around all clientsets used by OVN-Kubernetes
type OVNClientset struct {
	KubeClient               kubernetes.Interface
	ANPClient                anpclientset.Interface
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
	ANPClient                anpclientset.Interface
	EgressIPClient           egressipclientset.Interface
	CloudNetworkClient       ocpcloudnetworkclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
}

// OVNNetworkControllerManagerClientset
type OVNKubeControllerClientset struct {
	KubeClient               kubernetes.Interface
	ANPClient                anpclientset.Interface
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
	EgressIPClient         egressipclientset.Interface
	AdminPolicyRouteClient adminpolicybasedrouteclientset.Interface
}

type OVNClusterManagerClientset struct {
	KubeClient             kubernetes.Interface
	EgressIPClient         egressipclientset.Interface
	CloudNetworkClient     ocpcloudnetworkclientset.Interface
	NetworkAttchDefClient  networkattchmentdefclientset.Interface
	EgressServiceClient    egressserviceclientset.Interface
	AdminPolicyRouteClient adminpolicybasedrouteclientset.Interface
	EgressFirewallClient   egressfirewallclientset.Interface
}

const (
	certNamePrefix       = "ovnkube-client"
	certCommonNamePrefix = "system:ovn-node"
	certOrganization     = "system:ovn-nodes"
)

var (
	certUsages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth}
)

func (cs *OVNClientset) GetMasterClientset() *OVNMasterClientset {
	return &OVNMasterClientset{
		KubeClient:               cs.KubeClient,
		ANPClient:                cs.ANPClient,
		EgressIPClient:           cs.EgressIPClient,
		CloudNetworkClient:       cs.CloudNetworkClient,
		EgressFirewallClient:     cs.EgressFirewallClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
	}
}

func (cs *OVNMasterClientset) GetOVNKubeControllerClientset() *OVNKubeControllerClientset {
	return &OVNKubeControllerClientset{
		KubeClient:               cs.KubeClient,
		ANPClient:                cs.ANPClient,
		EgressIPClient:           cs.EgressIPClient,
		EgressFirewallClient:     cs.EgressFirewallClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
	}
}

func (cs *OVNClientset) GetOVNKubeControllerClientset() *OVNKubeControllerClientset {
	return &OVNKubeControllerClientset{
		KubeClient:               cs.KubeClient,
		ANPClient:                cs.ANPClient,
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
		KubeClient:             cs.KubeClient,
		EgressIPClient:         cs.EgressIPClient,
		CloudNetworkClient:     cs.CloudNetworkClient,
		NetworkAttchDefClient:  cs.NetworkAttchDefClient,
		EgressServiceClient:    cs.EgressServiceClient,
		AdminPolicyRouteClient: cs.AdminPolicyRouteClient,
		EgressFirewallClient:   cs.EgressFirewallClient,
	}
}

func (cs *OVNClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:             cs.KubeClient,
		EgressServiceClient:    cs.EgressServiceClient,
		EgressIPClient:         cs.EgressIPClient,
		AdminPolicyRouteClient: cs.AdminPolicyRouteClient,
	}
}

func (cs *OVNMasterClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:          cs.KubeClient,
		EgressServiceClient: cs.EgressServiceClient,
		EgressIPClient:      cs.EgressIPClient,
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
		if (conf.Token == "" && conf.CertDir == "") || len(conf.CAData) == 0 {
			return nil, fmt.Errorf("TLS-secured apiservers require token/cert and CA certificate")
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
		if conf.CertDir != "" {
			kconfig = &rest.Config{
				Host: conf.APIServer,
				TLSClientConfig: rest.TLSClientConfig{
					KeyFile:  filepath.Join(conf.CertDir, certNamePrefix+"-current.pem"),
					CertFile: filepath.Join(conf.CertDir, certNamePrefix+"-current.pem"),
					CAData:   conf.CAData,
				},
			}
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

// StartNodeCertificateManager manages the creation and rotation of the node-specific client certificate.
// When there is no existing certificate, it will use the BootstrapKubeconfig kubeconfig to create a CSR and it will
// wait for the certificate before returning.
func StartNodeCertificateManager(ctx context.Context, wg *sync.WaitGroup, nodeName string, conf *config.KubernetesConfig) error {
	if nodeName == "" {
		return fmt.Errorf("the provided node name cannot be empty")
	}
	defaultKConfig, err := newKubernetesRestConfig(conf)
	if err != nil {
		return fmt.Errorf("unable to create kubernetes rest config, err: %v", err)
	}
	defaultKConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	defaultKConfig.ContentType = "application/vnd.kubernetes.protobuf"

	bootstrapKConfig, err := clientcmd.BuildConfigFromFlags("", conf.BootstrapKubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load bootstrap kubeconfig from %s, err: %v", conf.BootstrapKubeconfig, err)
	}
	// If we have a valid certificate, use that to fetch CSRs.
	// Otherwise, use the bootstrap credentials.
	// https://github.com/kubernetes/kubernetes/blob/068ee321bc7bfe1c2cefb87fb4d9e5deea84fbc8/cmd/kubelet/app/server.go#L953-L963
	newClientsetFn := func(current *tls.Certificate) (kubernetes.Interface, error) {
		cfg := bootstrapKConfig
		if current != nil {
			cfg = defaultKConfig
		}
		return kubernetes.NewForConfig(cfg)
	}

	certificateStore, err := certificate.NewFileStore(certNamePrefix, conf.CertDir, conf.CertDir, "", "")
	if err != nil {
		return fmt.Errorf("failed to initialize the certificate store: %v", err)
	}

	// The CSR approver only accepts CSRs created by system:ovn-node:nodeName and system:node:nodeName.
	// If the node name in the existing ovn-node certificate is different from the current node name,
	// remove the certificate so the CSR will be created using the bootstrap kubeconfig using system:node:nodeName user.
	certCommonName := fmt.Sprintf("%s:%s", certCommonNamePrefix, nodeName)
	currentCertFromFile, err := certificateStore.Current()
	if err == nil && currentCertFromFile.Leaf != nil {
		if currentCertFromFile.Leaf.Subject.CommonName != certCommonName {
			klog.Errorf("Unexpected common name found in the certificate, expected: %q, got: %q, removing %s",
				certCommonName, currentCertFromFile.Leaf.Subject.CommonName, certificateStore.CurrentPath())
			if err := os.Remove(certificateStore.CurrentPath()); err != nil {
				return fmt.Errorf("failed to remove the current certificate file: %w", err)
			}
		}
	}

	certManager, err := certificate.NewManager(&certificate.Config{
		ClientsetFn: newClientsetFn,
		Template: &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   certCommonName,
				Organization: []string{certOrganization},
			},
		},
		RequestedCertificateLifetime: &conf.CertDuration,
		SignerName:                   certificatesv1.KubeAPIServerClientSignerName,
		Usages:                       certUsages,
		CertificateStore:             certificateStore,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize the certificate manager: %v", err)
	}

	if conf.CertDuration < time.Hour {
		// the default value for CertCallbackRefreshDuration (5min) is too long for short-lived certs,
		// set it to a more sensible value
		transport.CertCallbackRefreshDuration = time.Second * 10
	}
	certManager.Start()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		certManager.Stop()
	}()

	klog.Infof("Waiting for certificate")
	err = wait.PollUntilContextTimeout(context.TODO(), time.Second, 2*time.Minute, true, func(_ context.Context) (bool, error) {
		return certManager.Current() != nil, nil
	})
	if err != nil {
		return fmt.Errorf("certificate was not signed: %v", err)
	}
	klog.Infof("Certificate found")

	// certManager is responsible for rotating the certificates; it determines when to rotate and sets up a timer.
	// With this approach, a certificate may become invalid if the system time changes unexpectedly
	// and the process is not restarted (which is common in suspended clusters).
	// After retrieving the initial certificate, run a periodic check to ensure it is valid.
	const retryInterval = time.Second * 10
	go wait.Until(func() {
		// certManager.Current() returns nil when the current cert has expired.
		currentCert := certManager.Current()
		if currentCert == nil || (currentCert.Leaf != nil && time.Now().Before(currentCert.Leaf.NotBefore)) {
			klog.Errorf("The current certificate is invalid, exiting.")
			os.Exit(1)
		}

	}, retryInterval, ctx.Done())
	return nil
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
	anpClientset, err := anpclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
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
		ANPClient:                anpClientset,
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

// GetClusterSubnets returns the v4&v6 cluster subnets in a cluster separately
func GetClusterSubnets() ([]*net.IPNet, []*net.IPNet) {
	var v4ClusterSubnets = []*net.IPNet{}
	var v6ClusterSubnets = []*net.IPNet{}
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if !utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			v4ClusterSubnets = append(v4ClusterSubnets, clusterSubnet.CIDR)
		} else {
			v6ClusterSubnets = append(v6ClusterSubnets, clusterSubnet.CIDR)
		}
	}
	return v4ClusterSubnets, v6ClusterSubnets
}

// GetAllClusterSubnets returns all (v4&v6) cluster subnets in a cluster
func GetAllClusterSubnets() []*net.IPNet {
	v4ClusterSubnets, v6ClusterSubnets := GetClusterSubnets()
	return append(v4ClusterSubnets, v6ClusterSubnets...)
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

// PodRunning checks if the pod is in running state or not
func PodRunning(pod *kapi.Pod) bool {
	return pod.Status.Phase == kapi.PodRunning
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

// GetLbEndpoints returns the IPv4 and IPv6 addresses of eligible endpoints from a
// provided list of endpoint slices, for a given service and service port.
func GetLbEndpoints(slices []*discovery.EndpointSlice, svcPort kapi.ServicePort, service *v1.Service) LbEndpoints {
	var validSlices []*discovery.EndpointSlice
	v4IPs := sets.NewString()
	v6IPs := sets.NewString()
	out := LbEndpoints{}

	// return an empty object so the caller doesn't have to check for nil and can use it as an iterator
	if len(slices) == 0 {
		return out
	}

	for _, slice := range slices {
		klog.V(5).Infof("Getting endpoints for slice %s/%s", slice.Namespace, slice.Name)

		for _, slicePort := range slice.Ports {
			// If Service port name is set, it must match the name field in the endpoint
			// If Service port name is not set, we just use the endpoint port
			if svcPort.Name != "" && svcPort.Name != *slicePort.Name {
				klog.V(5).Infof("Slice %s with different Port name, requested: %s received: %s",
					slice.Name, svcPort.Name, *slicePort.Name)
				continue
			}

			// Skip ports that don't match the protocol
			if *slicePort.Protocol != svcPort.Protocol {
				klog.V(5).Infof("Slice %s with different Port protocol, requested: %s received: %s",
					slice.Name, svcPort.Protocol, *slicePort.Protocol)
				continue
			}

			out.Port = *slicePort.Port

			if slice.AddressType == discovery.AddressTypeFQDN {
				klog.V(5).Infof("Skipping FQDN slice %s/%s", slice.Namespace, slice.Name)
			} else { // endpoints are here either IPv4 or IPv6
				validSlices = append(validSlices, slice)
			}
		}
	}

	serviceStr := ""
	if service != nil {
		serviceStr = fmt.Sprintf(" for service %s/%s", service.Namespace, service.Name)
	}
	// separate IPv4 from IPv6 addresses for eligible endpoints
	for _, endpoint := range getEligibleEndpoints(validSlices, service) {
		for _, ip := range endpoint.Addresses {
			if utilnet.IsIPv4String(ip) {
				klog.V(5).Infof("Adding endpoint IPv4 address %s port %d%s",
					ip, out.Port, serviceStr)
				v4IPs.Insert(utilnet.ParseIPSloppy(ip).String())

			} else if utilnet.IsIPv6String(ip) {
				klog.V(5).Infof("Adding endpoint IPv6 address %s port %d%s",
					ip, out.Port, serviceStr)
				v6IPs.Insert(utilnet.ParseIPSloppy(ip).String())

			} else {
				klog.V(5).Infof("Skipping unrecognized address %s port %d%s",
					ip, out.Port, serviceStr)
			}
		}
	}

	out.V4IPs = v4IPs.List()
	out.V6IPs = v6IPs.List()
	klog.V(5).Infof("LB Endpoints for %s/%s are: %v / %v on port: %d",
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

func IsEndpointTerminating(endpoint discovery.Endpoint) bool {
	return endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating
}

// NoHostSubnet() compares the no-hostsubnet-nodes flag with node labels to see if the node is managing its
// own network.
func NoHostSubnet(node *v1.Node) bool {
	if config.Kubernetes.NoHostSubnetNodes == nil {
		return false
	}

	return config.Kubernetes.NoHostSubnetNodes.Matches(labels.Set(node.Labels))
}

// getSelectedEligibleEndpoints does the following:
// (1) filters the given endpoints with the provided condition function condFn;
// (2) further selects eligible endpoints based on readiness.
// Eligible endpoints are ready endpoints; if there are none, eligible endpoints are serving & terminating
// endpoints, as defined in KEP-1669
// (https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/1669-proxy-terminating-endpoints/README.md).
// The service corresponding to the given endpoints needs to provided as an input argument
// because if Spec.PublishNotReadyAddresses is set, then all provided endpoints must always be returned.
// PublishNotReadyAddresses tells endpoint consumers to disregard any indications of ready/not-ready and
// is generally used together with headless services so that DNS records of all endpoints (ready or not)
// are always published.
// Note that condFn, when specified, is used by utility functions to filter out non-local endpoints.
// It's important to run it /before/ the eligible endpoint selection, since the order impacts the output.
func getSelectedEligibleEndpoints(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, condFn func(ep discovery.Endpoint) bool) []discovery.Endpoint {
	var readySelectedEndpoints []discovery.Endpoint
	var servingTerminatingSelectedEndpoints []discovery.Endpoint
	var eligibleEndpoints []discovery.Endpoint

	includeAllEndpoints := service != nil && service.Spec.PublishNotReadyAddresses

	for _, slice := range endpointSlices {
		for _, endpoint := range slice.Endpoints {
			// Apply precondition on endpoints, if provided
			if condFn == nil || condFn(endpoint) {
				// Assign to the ready or the serving&terminating slice for a later decision
				if includeAllEndpoints || IsEndpointReady(endpoint) {
					readySelectedEndpoints = append(readySelectedEndpoints, endpoint)
				} else if IsEndpointServing(endpoint) && IsEndpointTerminating(endpoint) {
					servingTerminatingSelectedEndpoints = append(servingTerminatingSelectedEndpoints, endpoint)
				}
			}
		}
	}
	serviceStr := ""
	if service != nil {
		serviceStr = fmt.Sprintf(" (service %s/%s)", service.Namespace, service.Name)
	}
	klog.V(5).Infof("Endpoint selection%s: found %d ready endpoints", serviceStr, len(readySelectedEndpoints))

	// Select eligible endpoints based on readiness
	eligibleEndpoints = readySelectedEndpoints
	// Fallback to serving terminating endpoints (ready=false, serving=true, terminating=true) only if none are ready
	if len(readySelectedEndpoints) == 0 {
		eligibleEndpoints = servingTerminatingSelectedEndpoints
		klog.V(5).Infof("Endpoint selection%s: fallback to %d serving & terminating endpoints",
			serviceStr, len(servingTerminatingSelectedEndpoints))
	}

	return eligibleEndpoints
}

func getLocalEligibleEndpoints(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, nodeName string) []discovery.Endpoint {
	return getSelectedEligibleEndpoints(endpointSlices, service, func(endpoint discovery.Endpoint) bool {
		return endpoint.NodeName != nil && *endpoint.NodeName == nodeName
	})
}

func getEligibleEndpoints(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) []discovery.Endpoint {
	return getSelectedEligibleEndpoints(endpointSlices, service, nil)
}

// getEligibleEndpointAddresses takes a list of endpointSlices, a service and, optionally, a nodeName
// and applies the endpoint selection logic. It returns the IP addresses of eligible endpoints.
func getEligibleEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, nodeName string) sets.Set[string] {
	endpointsAddresses := sets.New[string]()
	var eligibleEndpoints []discovery.Endpoint

	if nodeName != "" {
		eligibleEndpoints = getLocalEligibleEndpoints(endpointSlices, service, nodeName)
	} else {
		eligibleEndpoints = getEligibleEndpoints(endpointSlices, service)
	}
	for _, endpoint := range eligibleEndpoints {
		for _, ip := range endpoint.Addresses {
			endpointsAddresses.Insert(utilnet.ParseIPSloppy(ip).String())
		}
	}

	return endpointsAddresses
}

// GetEligibleEndpointAddresses returns a list of IP addresses of all eligible endpoints from the given endpoint slices.
func GetEligibleEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) sets.Set[string] {
	return getEligibleEndpointAddresses(endpointSlices, service, "")
}

// GetLocalEligibleEndpointAddresses returns a list of IP address of endpoints that are local to the specified node
// and are eligible.
func GetLocalEligibleEndpointAddresses(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, nodeName string) sets.Set[string] {
	return getEligibleEndpointAddresses(endpointSlices, service, nodeName)
}

// DoesEndpointSliceContainEndpoint returns true if the endpointslice
// contains an endpoint with the given IP, port and Protocol and if this endpoint is considered eligible.
func DoesEndpointSliceContainEligibleEndpoint(endpointSlice *discovery.EndpointSlice,
	epIP string, epPort int32, protocol kapi.Protocol, service *kapi.Service) bool {
	for _, ep := range getEligibleEndpoints([]*discovery.EndpointSlice{endpointSlice}, service) {
		for _, ip := range ep.Addresses {
			for _, port := range endpointSlice.Ports {
				if utilnet.ParseIPSloppy(ip).String() == epIP && *port.Port == epPort && *port.Protocol == protocol {
					return true
				}
			}
		}
	}
	return false
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

func GetPhysNetNameKey() string {
	if config.Gateway.PhysNetNameKey != "" {
		return config.Gateway.PhysNetNameKey
	}
	return types.PhysicalNetworkName
}

// GetPhysNetNameKeyForNode returns name of the physical networks for the node name if it was provided
// Else value from the config will be returned if available, or the default one.
func GetPhysNetNameKeyForNode(nodeName string, labels map[string]string) string {
	if config.Gateway.PhysNetNameKey != "" {
		if nodeName != "" {
			if value, ok := labels["k8s.ovn.org/physnet-name-key"]; ok {
				return value
			}
		}
		// Default behaviour
		return config.Gateway.PhysNetNameKey
	}
	klog.Infof("Using default value: %s", types.PhysicalNetworkName)
	return types.PhysicalNetworkName
}
