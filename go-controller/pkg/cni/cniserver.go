package cni

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	kapi "k8s.io/api/core/v1"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	criapi "k8s.io/cri-api/pkg/apis"
	kruntimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog/v2"
	kubeletremote "k8s.io/kubernetes/pkg/kubelet/cri/remote"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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
func NewCNIServer(rundir string, useOVSExternalIDs bool, factory factory.NodeWatchFactory, kclient kubernetes.Interface, runtimeSockets []string) (*Server, error) {
	if config.OvnKubeNode.Mode == types.NodeModeSmartNIC {
		return nil, fmt.Errorf("unsupported ovnkube-node mode for CNI server: %s", config.OvnKubeNode.Mode)
	}

	if len(rundir) == 0 {
		rundir = serverRunDir
	}
	router := mux.NewRouter()

	// we use atomic lib to store port binding mode state, so use int32 to represent bool
	var ovnPortBinding int32
	if useOVSExternalIDs {
		ovnPortBinding = 1
	}

	s := &Server{
		Server: http.Server{
			Handler: router,
		},
		rundir:             rundir,
		useOVSExternalIDs:  ovnPortBinding,
		podLister:          corev1listers.NewPodLister(factory.LocalPodInformer().GetIndexer()),
		kclient:            kclient,
		runningSandboxAdds: make(map[string]*PodRequest),
		mode:               config.OvnKubeNode.Mode,
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

	var err error
	s.runtimeService, err = getRuntimeService(runtimeSockets)
	if err != nil {
		return nil, err
	}

	factory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*kapi.Pod)
			oldPod := oldObj.(*kapi.Pod)
			if oldPod.UID != newPod.UID {
				s.cancelPodAdds(newPod, func(uid string) bool {
					// Cancel adds that don't match latest pod UID
					return uid != string(newPod.UID)
				})
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			s.cancelPodAdds(pod, func(uid string) bool {
				// Cancel adds that match deleted pod UID
				return uid == string(pod.UID)
			})
		},
	}, nil)

	return s, nil
}

// cancelPodAdds cancels and pod add requests that match the given filter function
func (s *Server) cancelPodAdds(pod *kapi.Pod, filterFn func(string) bool) {
	s.runningSandboxAddsLock.Lock()
	defer s.runningSandboxAddsLock.Unlock()

	for _, req := range s.runningSandboxAdds {
		// Cancel sandbox requests for this pod that have a MAC set
		// which doesn't match the expected MAC
		if req.PodNamespace != pod.Namespace || req.PodName != pod.Name {
			continue
		}

		if filterFn(req.PodUID) {
			req.cancel()
			klog.Infof("%v canceled outdated sandbox ADD request")
		}
	}
}


// RuntimeEndpoints is a constant array of default CRI runtime socket paths
var RuntimeEndpoints = []string{
	"unix:///var/run/crio/crio.sock",
	"unix:///var/run/containerd/containerd.sock",
	"unix:///var/run/dockershim.sock",
}

func getOneRuntimeService(socket string) (criapi.RuntimeService, error) {
	// 2 minutes is the current default value used in kubelet
	const runtimeRequestTimeout = 2 * time.Minute

	runtimeService, err := kubeletremote.NewRemoteRuntimeService(socket, runtimeRequestTimeout)
	if err != nil {
		return nil, err
	}

	// Ensure the runtime is actually alive; gRPC may create the client but
	// it may not be responding to requests yet
	if _, err := runtimeService.ListPodSandbox(&kruntimeapi.PodSandboxFilter{}); err != nil {
		return nil, err
	}

	return runtimeService, nil
}

func getRuntimeService(runtimeSockets []string) (criapi.RuntimeService, error) {
	var runtime criapi.RuntimeService

	err := utilwait.ExponentialBackoff(
		utilwait.Backoff{
			Duration: 100 * time.Millisecond,
			Factor:   1.2,
			Steps:    24,
		},
		func() (bool, error) {
			for _, sock := range runtimeSockets {
				runtimeService, err := getOneRuntimeService(sock)
				if err == nil {
					runtime = runtimeService
					return true, nil
				}
			}
			// Wait longer
			return false, nil
		})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch runtime service: %v", err)
	}
	return runtime, nil
}

func (s *Server) getSandboxPodUID(sandboxID string) (string, error) {
	podSandboxList, err := s.runtimeService.ListPodSandbox(&kruntimeapi.PodSandboxFilter{
		Id: sandboxID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pod sandboxes for ID %q: %v", sandboxID, err)
	}
	if len(podSandboxList) == 0 {
		return "", fmt.Errorf("pod sandbox not found for ID %q", sandboxID)
	}
	return podSandboxList[0].Metadata.Uid, nil
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

	conf, err := config.ReadCNIConfig(cr.Config)
	if err != nil {
		return nil, fmt.Errorf("broken stdin args")
	}

	req.CNIConf = conf
	req.timestamp = time.Now()
	req.ctx, req.cancel = context.WithCancel(context.Background())
	return req, nil
}

func (s *Server) startSandboxRequest(req *PodRequest) error {
	// Only sandbox add requests are tracked because only adds need
	// to be canceled when the pod is deleted. Delete requests should
	// be run to completion to clean up anything the earlier add
	// already configured.
	if req.Command == CNIAdd {
		s.runningSandboxAddsLock.Lock()
		defer s.runningSandboxAddsLock.Unlock()
		if _, ok := s.runningSandboxAdds[req.SandboxID]; ok {
			// Should never happen as the runtime is required to
			// serialize operations for the same sandbox
			return fmt.Errorf("%s ADD already started", req)
		}
		s.runningSandboxAdds[req.SandboxID] = req
	}
	return nil
}

func (s *Server) finishSandboxRequest(req *PodRequest) {
	if req.Command == CNIAdd {
		s.runningSandboxAddsLock.Lock()
		defer s.runningSandboxAddsLock.Unlock()
		delete(s.runningSandboxAdds, req.SandboxID)
	}
}

// Dispatch a pod request to the request handler and return the result to the
// CNI server client
func (s *Server) handleCNIRequest(r *http.Request) ([]byte, error) {
	var cr Request
	b, _ := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(b, &cr); err != nil {
		return nil, err
	}
	req, err := cniRequestToPodRequest(&cr)
	if err != nil {
		return nil, err
	}
	if s.mode == types.NodeModeSmartNICHost {
		req.IsSmartNIC = true
	}

	req.PodUID, err = s.getSandboxPodUID(req.SandboxID)
	if err != nil {
		return nil, err
	}
	klog.Warningf("[%s/%s %s] pod UID %s", req.PodNamespace, req.PodName, req.SandboxID, req.PodUID)

	if err := s.startSandboxRequest(req); err != nil {
		return nil, err
	}
	defer s.finishSandboxRequest(req)

	useOVSExternalIDs := false
	if atomic.LoadInt32(&s.useOVSExternalIDs) > 0 {
		useOVSExternalIDs = true
	}
	result, err := s.requestFunc(req, s.podLister, useOVSExternalIDs, s.kclient)
	if err != nil {
		// Prefix error with request information for easier debugging
		return nil, fmt.Errorf("%s %v", req, err)
	}
	return result, nil
}

func (s *Server) handleCNIMetrics(w http.ResponseWriter, r *http.Request) {
	var cm CNIRequestMetrics

	b, _ := ioutil.ReadAll(r.Body)
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

func (s *Server) EnableOVNPortUpSupport() {
	atomic.StoreInt32(&s.useOVSExternalIDs, 1)
	klog.Info("OVN Port Binding support now enabled in CNI Server")
}
