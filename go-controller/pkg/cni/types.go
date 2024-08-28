package cni

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	current "github.com/containernetworking/cni/pkg/types/100"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kapi "k8s.io/api/core/v1"
)

// ServerRunDir is the default directory for CNIServer runtime files
const ServerRunDir string = "/var/run/ovn-kubernetes/cni/"

const serverSocketName string = "ovn-cni-server.sock"
const serverSocketPath string = ServerRunDir + "/" + serverSocketName

// KubeAPIAuth contains information necessary to create a Kube API client
type KubeAPIAuth struct {
	// Kubeconfig is the path to a kubeconfig
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// KubeAPIServer is the URL of a Kubernetes API server (not required if kubeconfig is given)
	KubeAPIServer string `json:"kube-api-server,omitempty"`
	// KubeAPIToken is a Kubernetes API token (not required if kubeconfig is given)
	KubeAPIToken string `json:"kube-api-token,omitempty"`
	// KubeAPITokenFile is the path to Kubernetes API token
	// If set, it is periodically read and takes precedence over KubeAPIToken
	KubeAPITokenFile string `json:"kube-api-token-file,omitempty"`
	// KubeCAData is the Base64-ed Kubernetes API CA certificate data (not required if kubeconfig is given)
	KubeCAData string `json:"kube-ca-data,omitempty"`
}

// PodInterfaceInfo consists of interface info result from cni server if cni client configure's interface
type PodInterfaceInfo struct {
	util.PodAnnotation

	MTU                  int    `json:"mtu"`
	RoutableMTU          int    `json:"routable-mtu"`
	Ingress              int64  `json:"ingress"`
	Egress               int64  `json:"egress"`
	IsDPUHostMode        bool   `json:"is-dpu-host-mode"`
	SkipIPConfig         bool   `json:"skip-ip-config"`
	PodUID               string `json:"pod-uid"`
	NetdevName           string `json:"vf-netdev-name"`
	EnableUDPAggregation bool   `json:"enable-udp-aggregation"`

	// network name, for default network, it is "default", otherwise it is net-attach-def's netconf spec name
	NetName string `json:"netName"`
	// NADName, for default network, it is "default", otherwise, in the form of net-attach-def's <Namespace>/<Name>
	NADName string `json:"nadName"`
}

// Explicit type for CNI commands the server handles
type command string

// CNIAdd is the command representing add operation for a new pod
const CNIAdd command = "ADD"

// CNIUpdate is the command representing update operation for an existing pod
const CNIUpdate command = "UPDATE"

// CNIDel is the command representing delete operation on a pod that is to be torn down
const CNIDel command = "DEL"

// CNICheck is the command representing check operation on a pod
const CNICheck command = "CHECK"

// Request sent to the Server by the OVN CNI plugin
type Request struct {
	// CNI environment variables, like CNI_COMMAND and CNI_NETNS
	Env map[string]string `json:"env,omitempty"`
	// CNI configuration passed via stdin to the CNI plugin
	Config []byte `json:"config,omitempty"`
	// The DeviceInfo struct
	nadapi.DeviceInfo
}

// CNIRequestMetrics info to report from CNI shim to CNI server
type CNIRequestMetrics struct {
	Command     command `json:"command"`
	ElapsedTime float64 `json:"elapsedTime"`
	HasErr      bool    `json:"hasErr"`
}

// Response sent to the OVN CNI plugin by the Server
type Response struct {
	Result    *current.Result
	PodIFInfo *PodInterfaceInfo
	KubeAuth  *KubeAPIAuth
}

func (response *Response) Marshal() ([]byte, error) {
	return json.Marshal(response)
}

// Filter out kubeAuth, since it might contain sensitive information.
func (response *Response) MarshalForLogging() ([]byte, error) {
	var noAuthJSON []byte
	var err error
	var noAuth interface{}

	if response == nil {
		return nil, nil
	}

	// Only one of Result and PodIFInfo is ever set by cmdAdd/cmdDel
	if response.Result != nil {
		noAuth = response.Result
	} else {
		noAuth = response.PodIFInfo
	}

	if noAuthJSON, err = json.Marshal(noAuth); err != nil {
		klog.Errorf("Could not JSON-encode the extracted response: %v", err)
		return nil, err
	}

	return noAuthJSON, nil
}

// PodRequest structure built from Request which is passed to the
// handler function given to the Server at creation time
type PodRequest struct {
	// The CNI command of the operation
	Command command
	// kubernetes namespace name
	PodNamespace string
	// kubernetes pod name
	PodName string
	// kubernetes pod UID
	PodUID string
	// kubernetes container ID
	SandboxID string
	// kernel network namespace path
	Netns string
	// Interface name to be configured
	IfName string
	// CNI conf obtained from stdin conf
	CNIConf *types.NetConf
	// Timestamp when the request was started
	timestamp time.Time
	// ctx is a context tracking this request's lifetime
	ctx context.Context
	// cancel should be called to cancel this request
	cancel context.CancelFunc
	// if CNIConf.DeviceID is present, then captures if the VF is of type VFIO or not
	IsVFIO bool

	// network name, for default network, this will be types.DefaultNetworkName
	netName string

	// for ovs interfaces plumbed for secondary networks, their iface-id's prefix is derived from the specific nadName;
	// also, need to find the pod annotation, dpu pod connection/status annotations of the given NAD ("default"
	// for default network).
	nadName string

	// the DeviceInfo struct
	deviceInfo nadapi.DeviceInfo
}

type podRequestFunc func(request *PodRequest, clientset *ClientSet, kubeAuth *KubeAPIAuth, nadController *nad.NetAttachDefinitionController) ([]byte, error)
type getCNIResultFunc func(request *PodRequest, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error)

type PodInfoGetter interface {
	getPod(namespace, name string) (*kapi.Pod, error)
}

type ClientSet struct {
	PodInfoGetter
	kclient   kubernetes.Interface
	podLister corev1listers.PodLister
}

func NewClientSet(kclient kubernetes.Interface, podLister corev1listers.PodLister) *ClientSet {
	return &ClientSet{
		kclient:   kclient,
		podLister: podLister,
	}
}

// Server object that listens for JSON-marshaled Request objects
// on a private root-only Unix domain socket.
type Server struct {
	http.Server
	handlePodRequestFunc podRequestFunc
	clientSet            *ClientSet
	kubeAuth             *KubeAPIAuth
	nadController        *nad.NetAttachDefinitionController
}
