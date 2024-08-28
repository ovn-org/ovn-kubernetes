package cni

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	nad "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// *** The Server is PRIVATE API between OVN components and may be
// changed at any time.  It is in no way a supported interface or API. ***
//
// The Server accepts pod setup/teardown requests from the OVN
// CNI plugin, which is itself called by kubelet when pod networking
// should be set up or torn down.  The OVN CNI plugin gathers up
// the standard CNI environment variables and network configuration provided
// on stdin and forwards them to the Server over a private, root-only
// Unix domain socket, using HTTP as the transport and JSON as the protocol.
//
// The Server interprets standard CNI environment variables as specified
// by the Container Network Interface (CNI) specification available here:
// https://github.com/containernetworking/cni/blob/master/SPEC.md
// While the Server interface is not itself versioned, as the CNI
// specification requires that CNI network configuration is versioned, and
// since the OVN CNI plugin passes that configuration to the
// Server, versioning is ensured in exactly the same way as an executable
// CNI plugin would be versioned.
//
// Security: since the Unix domain socket created by the Server is owned
// by root and inaccessible to any other user, no unprivileged process may
// access the Server.  The Unix domain socket and its parent directory are
// removed and re-created with 0700 permissions each time ovnkube on the node is
// started.

// NewCNIServer creates and returns a new Server object which will listen on a socket in the given path
func NewCNIServer(factory factory.NodeWatchFactory, kclient kubernetes.Interface,
	nadController *nad.NetAttachDefinitionController) (*Server, error) {
	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		return nil, fmt.Errorf("unsupported ovnkube-node mode for CNI server: %s", config.OvnKubeNode.Mode)
	}

	router := mux.NewRouter()

	s := &Server{
		Server: http.Server{
			Handler: router,
		},
		clientSet: &ClientSet{
			podLister: corev1listers.NewPodLister(factory.LocalPodInformer().GetIndexer()),
			kclient:   kclient,
		},
		kubeAuth: &KubeAPIAuth{
			Kubeconfig:       config.Kubernetes.Kubeconfig,
			KubeAPIServer:    config.Kubernetes.APIServer,
			KubeAPIToken:     config.Kubernetes.Token,
			KubeAPITokenFile: config.Kubernetes.TokenFile,
		},
		handlePodRequestFunc: HandlePodRequest,
	}

	if util.IsNetworkSegmentationSupportEnabled() {
		s.nadController = nadController
	}

	if len(config.Kubernetes.CAData) > 0 {
		s.kubeAuth.KubeCAData = base64.StdEncoding.EncodeToString(config.Kubernetes.CAData)
	}

	router.NotFoundHandler = http.HandlerFunc(http.NotFound)
	router.HandleFunc("/metrics", s.handleCNIMetrics).Methods("POST")
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		result, err := s.handleCNIRequest(r)
		if err != nil {
			http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
			return
		}

		// Empty response JSON means success with no body
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(result); err != nil {
			klog.Warningf("Error writing HTTP response: %v", err)
		}
	}).Methods("POST")

	return s, nil
}

// Split the "CNI_ARGS" environment variable's value into a map.  CNI_ARGS
// contains arbitrary key/value pairs separated by ';' and is for runtime or
// plugin specific uses.  Kubernetes passes the pod namespace and name in
// CNI_ARGS.
func gatherCNIArgs(env map[string]string) (map[string]string, error) {
	cniArgs, ok := env["CNI_ARGS"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_ARGS: '%s'", env)
	}

	mapArgs := make(map[string]string)
	for _, arg := range strings.Split(cniArgs, ";") {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid CNI_ARG '%s'", arg)
		}
		mapArgs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return mapArgs, nil
}

func cniRequestToPodRequest(cr *Request) (*PodRequest, error) {
	cmd, ok := cr.Env["CNI_COMMAND"]
	if !ok {
		return nil, fmt.Errorf("unexpected or missing CNI_COMMAND")
	}

	req := &PodRequest{
		Command: command(cmd),
	}

	req.SandboxID, ok = cr.Env["CNI_CONTAINERID"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_CONTAINERID")
	}
	req.Netns, ok = cr.Env["CNI_NETNS"]
	if !ok {
		return nil, fmt.Errorf("missing CNI_NETNS")
	}

	req.IfName, ok = cr.Env["CNI_IFNAME"]
	if !ok {
		req.IfName = "eth0"
	}

	cniArgs, err := gatherCNIArgs(cr.Env)
	if err != nil {
		return nil, err
	}

	req.PodNamespace, ok = cniArgs["K8S_POD_NAMESPACE"]
	if !ok {
		return nil, fmt.Errorf("missing K8S_POD_NAMESPACE")
	}

	req.PodName, ok = cniArgs["K8S_POD_NAME"]
	if !ok {
		return nil, fmt.Errorf("missing K8S_POD_NAME")
	}

	// UID may not be passed by all runtimes yet. Will be passed
	// by CRIO 1.20+ and containerd 1.5+ soon.
	// CRIO 1.20: https://github.com/cri-o/cri-o/pull/5029
	// CRIO 1.21: https://github.com/cri-o/cri-o/pull/5028
	// CRIO 1.22: https://github.com/cri-o/cri-o/pull/5026
	// containerd 1.6: https://github.com/containerd/containerd/pull/5640
	// containerd 1.5: https://github.com/containerd/containerd/pull/5643
	req.PodUID = cniArgs["K8S_POD_UID"]

	conf, err := config.ReadCNIConfig(cr.Config)
	if err != nil {
		return nil, fmt.Errorf("broken stdin args")
	}

	// the first network to the Pod is always named as `default`,
	// capture the effective NAD Name here
	req.netName = conf.Name
	if req.netName == types.DefaultNetworkName {
		req.nadName = types.DefaultNetworkName
	} else {
		req.nadName = conf.NADName
	}

	if conf.DeviceID != "" {
		if util.IsPCIDeviceName(conf.DeviceID) {
			// DeviceID is a PCI address
			req.IsVFIO = util.GetSriovnetOps().IsVfPciVfioBound(conf.DeviceID)
		} else if util.IsAuxDeviceName(conf.DeviceID) {
			// DeviceID is an Auxiliary device name - <driver_name>.<kind_of_a_type>.<id>
			chunks := strings.Split(conf.DeviceID, ".")
			if chunks[1] != "sf" {
				return nil, fmt.Errorf("only SF auxiliary devices are supported")
			}
		} else {
			return nil, fmt.Errorf("expected PCI or Auxiliary device name, got - %s", conf.DeviceID)
		}
	}

	req.CNIConf = conf
	req.deviceInfo = cr.DeviceInfo
	req.timestamp = time.Now()
	// Match the Kubelet default CRI operation timeout of 2m
	req.ctx, req.cancel = context.WithTimeout(context.Background(), 2*time.Minute)
	return req, nil
}

// Dispatch a pod request to the request handler and return the result to the
// CNI server client
func (s *Server) handleCNIRequest(r *http.Request) ([]byte, error) {
	var cr Request
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &cr); err != nil {
		return nil, err
	}
	req, err := cniRequestToPodRequest(&cr)
	if err != nil {
		return nil, err
	}
	defer req.cancel()

	result, err := s.handlePodRequestFunc(req, s.clientSet, s.kubeAuth, s.nadController)
	if err != nil {
		// Prefix error with request information for easier debugging
		return nil, fmt.Errorf("%s %v", req, err)
	}
	return result, nil
}

func (s *Server) handleCNIMetrics(w http.ResponseWriter, r *http.Request) {
	var cm CNIRequestMetrics

	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &cm); err != nil {
		klog.Warningf("Failed to unmarshal JSON (%s) to CNIRequestMetrics struct: %v",
			string(b), err)
	} else {
		hasErr := fmt.Sprintf("%t", cm.HasErr)
		metrics.MetricCNIRequestDuration.WithLabelValues(string(cm.Command), hasErr).Observe(cm.ElapsedTime)
	}
	// Empty response JSON means success with no body
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte{}); err != nil {
		klog.Warningf("Error writing %s HTTP response for metrics post", err)
	}
}
