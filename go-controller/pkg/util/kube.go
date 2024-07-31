package util

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
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

	ipamclaimssclientset "github.com/k8snetworkplumbingwg/ipamclaims/pkg/crd/ipamclaims/v1alpha1/apis/clientset/versioned"
	multinetworkpolicyclientset "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned"
	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	ocpcloudnetworkclientset "github.com/openshift/client-go/cloudnetwork/clientset/versioned"
	ocpnetworkclientset "github.com/openshift/client-go/network/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	egressfirewallclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	egressipclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned"
	egressqosclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1/apis/clientset/versioned"
	egressserviceclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1/apis/clientset/versioned"
	networkqosclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	anpclientset "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
)

// OVNClientset is a wrapper around all clientsets used by OVN-Kubernetes
type OVNClientset struct {
	KubeClient               kubernetes.Interface
	ANPClient                anpclientset.Interface
	EgressIPClient           egressipclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	OCPNetworkClient         ocpnetworkclientset.Interface
	CloudNetworkClient       ocpcloudnetworkclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	NetworkAttchDefClient    networkattchmentdefclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
	IPAMClaimsClient         ipamclaimssclientset.Interface
	NetworkQoSClient         networkqosclientset.Interface
}

// OVNMasterClientset
type OVNMasterClientset struct {
	KubeClient               kubernetes.Interface
	ANPClient                anpclientset.Interface
	EgressIPClient           egressipclientset.Interface
	CloudNetworkClient       ocpcloudnetworkclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	OCPNetworkClient         ocpnetworkclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
	IPAMClaimsClient         ipamclaimssclientset.Interface
	NetworkAttchDefClient    networkattchmentdefclientset.Interface
	NetworkQoSClient         networkqosclientset.Interface
}

// OVNNetworkControllerManagerClientset
type OVNKubeControllerClientset struct {
	KubeClient               kubernetes.Interface
	ANPClient                anpclientset.Interface
	EgressIPClient           egressipclientset.Interface
	EgressFirewallClient     egressfirewallclientset.Interface
	OCPNetworkClient         ocpnetworkclientset.Interface
	EgressQoSClient          egressqosclientset.Interface
	MultiNetworkPolicyClient multinetworkpolicyclientset.Interface
	EgressServiceClient      egressserviceclientset.Interface
	AdminPolicyRouteClient   adminpolicybasedrouteclientset.Interface
	IPAMClaimsClient         ipamclaimssclientset.Interface
	NetworkAttchDefClient    networkattchmentdefclientset.Interface
	NetworkQoSClient         networkqosclientset.Interface
}

type OVNNodeClientset struct {
	KubeClient             kubernetes.Interface
	EgressServiceClient    egressserviceclientset.Interface
	EgressIPClient         egressipclientset.Interface
	AdminPolicyRouteClient adminpolicybasedrouteclientset.Interface
	NetworkAttchDefClient  networkattchmentdefclientset.Interface
}

type OVNClusterManagerClientset struct {
	KubeClient             kubernetes.Interface
	ANPClient              anpclientset.Interface
	EgressIPClient         egressipclientset.Interface
	CloudNetworkClient     ocpcloudnetworkclientset.Interface
	NetworkAttchDefClient  networkattchmentdefclientset.Interface
	EgressServiceClient    egressserviceclientset.Interface
	AdminPolicyRouteClient adminpolicybasedrouteclientset.Interface
	EgressFirewallClient   egressfirewallclientset.Interface
	EgressQoSClient        egressqosclientset.Interface
	IPAMClaimsClient       ipamclaimssclientset.Interface
	OCPNetworkClient       ocpnetworkclientset.Interface
	NetworkQoSClient       networkqosclientset.Interface
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
		OCPNetworkClient:         cs.OCPNetworkClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
		IPAMClaimsClient:         cs.IPAMClaimsClient,
		NetworkAttchDefClient:    cs.NetworkAttchDefClient,
		NetworkQoSClient:         cs.NetworkQoSClient,
	}
}

func (cs *OVNMasterClientset) GetOVNKubeControllerClientset() *OVNKubeControllerClientset {
	return &OVNKubeControllerClientset{
		KubeClient:               cs.KubeClient,
		ANPClient:                cs.ANPClient,
		EgressIPClient:           cs.EgressIPClient,
		EgressFirewallClient:     cs.EgressFirewallClient,
		OCPNetworkClient:         cs.OCPNetworkClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
		IPAMClaimsClient:         cs.IPAMClaimsClient,
		NetworkAttchDefClient:    cs.NetworkAttchDefClient,
		NetworkQoSClient:         cs.NetworkQoSClient,
	}
}

func (cs *OVNClientset) GetOVNKubeControllerClientset() *OVNKubeControllerClientset {
	return &OVNKubeControllerClientset{
		KubeClient:               cs.KubeClient,
		ANPClient:                cs.ANPClient,
		EgressIPClient:           cs.EgressIPClient,
		EgressFirewallClient:     cs.EgressFirewallClient,
		OCPNetworkClient:         cs.OCPNetworkClient,
		EgressQoSClient:          cs.EgressQoSClient,
		MultiNetworkPolicyClient: cs.MultiNetworkPolicyClient,
		EgressServiceClient:      cs.EgressServiceClient,
		AdminPolicyRouteClient:   cs.AdminPolicyRouteClient,
		IPAMClaimsClient:         cs.IPAMClaimsClient,
		NetworkAttchDefClient:    cs.NetworkAttchDefClient,
		NetworkQoSClient:         cs.NetworkQoSClient,
	}
}

func (cs *OVNClientset) GetClusterManagerClientset() *OVNClusterManagerClientset {
	return &OVNClusterManagerClientset{
		KubeClient:             cs.KubeClient,
		ANPClient:              cs.ANPClient,
		EgressIPClient:         cs.EgressIPClient,
		CloudNetworkClient:     cs.CloudNetworkClient,
		NetworkAttchDefClient:  cs.NetworkAttchDefClient,
		EgressServiceClient:    cs.EgressServiceClient,
		AdminPolicyRouteClient: cs.AdminPolicyRouteClient,
		EgressFirewallClient:   cs.EgressFirewallClient,
		EgressQoSClient:        cs.EgressQoSClient,
		IPAMClaimsClient:       cs.IPAMClaimsClient,
		OCPNetworkClient:       cs.OCPNetworkClient,
		NetworkQoSClient:       cs.NetworkQoSClient,
	}
}

func (cs *OVNClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:             cs.KubeClient,
		EgressServiceClient:    cs.EgressServiceClient,
		EgressIPClient:         cs.EgressIPClient,
		AdminPolicyRouteClient: cs.AdminPolicyRouteClient,
		NetworkAttchDefClient:  cs.NetworkAttchDefClient,
	}
}

func (cs *OVNMasterClientset) GetNodeClientset() *OVNNodeClientset {
	return &OVNNodeClientset{
		KubeClient:            cs.KubeClient,
		EgressServiceClient:   cs.EgressServiceClient,
		EgressIPClient:        cs.EgressIPClient,
		NetworkAttchDefClient: cs.NetworkAttchDefClient,
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
		if (conf.Token == "" && conf.TokenFile == "" && conf.CertDir == "") || len(conf.CAData) == 0 {
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

	// In the unlikely event that the certificate file becomes corrupted, recover by removing
	// the certificate so the CSR will be created using the bootstrap kubeconfig.
	var noCertKeyError *certificate.NoCertKeyError
	if err != nil && !errors.As(err, &noCertKeyError) {
		var pathErr *os.PathError
		klog.Errorf("Failed to load the currect certificate file: %v", err)
		// Do not try to remove the file if os.Stat failed on it
		if errors.As(err, &pathErr) {
			return err
		}
		klog.Errorf("Removing: %s", certificateStore.CurrentPath())
		if err := os.Remove(certificateStore.CurrentPath()); err != nil {
			return fmt.Errorf("failed to remove the current certificate file: %w", err)
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
	networkClientset, err := ocpnetworkclientset.NewForConfig(kconfig)
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

	ipamClaimsClientset, err := ipamclaimssclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}

	networkqosClientset, err := networkqosclientset.NewForConfig(kconfig)
	if err != nil {
		return nil, err
	}

	return &OVNClientset{
		KubeClient:               kclientset,
		ANPClient:                anpClientset,
		EgressIPClient:           egressIPClientset,
		EgressFirewallClient:     egressFirewallClientset,
		OCPNetworkClient:         networkClientset,
		CloudNetworkClient:       cloudNetworkClientset,
		EgressQoSClient:          egressqosClientset,
		NetworkAttchDefClient:    networkAttchmntDefClientset,
		MultiNetworkPolicyClient: multiNetworkPolicyClientset,
		EgressServiceClient:      egressserviceClientset,
		AdminPolicyRouteClient:   adminPolicyBasedRouteClientset,
		IPAMClaimsClient:         ipamClaimsClientset,
		NetworkQoSClient:         networkqosClientset,
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
			klog.Warningf("Object %v either has no GroupVersionKind or has an ambiguous GroupVersionKind: %#v, err", obj, err)
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
func NoHostSubnet(node *kapi.Node) bool {
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
func getSelectedEligibleEndpoints(endpoints []discovery.Endpoint, service *kapi.Service, condFn func(ep discovery.Endpoint) bool) []discovery.Endpoint {
	var readySelectedEndpoints []discovery.Endpoint
	var servingTerminatingSelectedEndpoints []discovery.Endpoint
	var eligibleEndpoints []discovery.Endpoint

	includeAllEndpoints := service != nil && service.Spec.PublishNotReadyAddresses

	for _, endpoint := range endpoints {
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
	// Select eligible endpoints based on readiness
	eligibleEndpoints = readySelectedEndpoints
	// Fallback to serving terminating endpoints (ready=false, serving=true, terminating=true) only if none are ready
	if len(readySelectedEndpoints) == 0 {
		eligibleEndpoints = servingTerminatingSelectedEndpoints
	}

	return eligibleEndpoints
}

func getLocalEligibleEndpoints(endpoints []discovery.Endpoint, service *kapi.Service, nodeName string) []discovery.Endpoint {
	return getSelectedEligibleEndpoints(endpoints, service, func(endpoint discovery.Endpoint) bool {
		return endpoint.NodeName != nil && *endpoint.NodeName == nodeName
	})
}

func getEligibleEndpoints(endpoints []discovery.Endpoint, service *kapi.Service) []discovery.Endpoint {
	return getSelectedEligibleEndpoints(endpoints, service, nil)
}

// getEligibleEndpointAddresses takes a list of endpoints, a service and, optionally, a nodeName
// and applies the endpoint selection logic. It returns the IP addresses of eligible endpoints.
func getEligibleEndpointAddresses(endpoints []discovery.Endpoint, service *kapi.Service, nodeName string) []string {
	endpointsAddresses := sets.New[string]()
	var eligibleEndpoints []discovery.Endpoint

	if nodeName != "" {
		eligibleEndpoints = getLocalEligibleEndpoints(endpoints, service, nodeName)
	} else {
		eligibleEndpoints = getEligibleEndpoints(endpoints, service)
	}
	for _, endpoint := range eligibleEndpoints {
		for _, ip := range endpoint.Addresses {
			endpointsAddresses.Insert(utilnet.ParseIPSloppy(ip).String())
		}
	}

	return sets.List(endpointsAddresses)
}

func GetEligibleEndpointAddresses(endpoints []discovery.Endpoint, service *kapi.Service) []string {
	return getEligibleEndpointAddresses(endpoints, service, "")
}

// GetEligibleEndpointAddressesFromSlices returns a list of IP addresses of all eligible endpoints from the given endpoint slices.
func GetEligibleEndpointAddressesFromSlices(endpointSlices []*discovery.EndpointSlice, service *kapi.Service) []string {
	return getEligibleEndpointAddresses(getEndpointsFromEndpointSlices(endpointSlices), service, "")
}

// GetLocalEligibleEndpointAddressesFromSlices returns a set of IP addresses of endpoints that are local to the specified node
// and are eligible.
func GetLocalEligibleEndpointAddressesFromSlices(endpointSlices []*discovery.EndpointSlice, service *kapi.Service, nodeName string) sets.Set[string] {
	endpoints := getEligibleEndpointAddresses(getEndpointsFromEndpointSlices(endpointSlices), service, nodeName)
	return sets.New(endpoints...)
}

// DoesEndpointSliceContainEndpoint returns true if the endpointslice
// contains an endpoint with the given IP, port and Protocol and if this endpoint is considered eligible.
func DoesEndpointSliceContainEligibleEndpoint(endpointSlice *discovery.EndpointSlice,
	epIP string, epPort int32, protocol kapi.Protocol, service *kapi.Service) bool {
	endpoints := getEndpointsFromEndpointSlices([]*discovery.EndpointSlice{endpointSlice})
	for _, ep := range getEligibleEndpoints(endpoints, service) {
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

func getEndpointsFromEndpointSlices(endpointSlices []*discovery.EndpointSlice) []discovery.Endpoint {
	endpoints := []discovery.Endpoint{}
	for _, slice := range endpointSlices {
		endpoints = append(endpoints, slice.Endpoints...)
	}
	return endpoints
}

func GetConntrackZone() int {
	return config.Default.ConntrackZone
}
