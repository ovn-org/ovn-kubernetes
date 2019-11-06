package cni

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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
func NewCNIServer(rundir string) *Server {
	if len(rundir) == 0 {
		rundir = serverRunDir
	}
	router := mux.NewRouter()

	s := &Server{
		Server: http.Server{
			Handler: router,
		},
		rundir: rundir,
	}
	router.NotFoundHandler = http.HandlerFunc(http.NotFound)
	router.HandleFunc("/", s.handleCNIRequest).Methods("POST")
	return s
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
	return req, nil
}

// Dispatch a pod request to the request handler and return the result to the
// CNI server client
func (s *Server) handleCNIRequest(w http.ResponseWriter, r *http.Request) {
	var cr Request
	b, _ := ioutil.ReadAll(r.Body)
	if err := json.Unmarshal(b, &cr); err != nil {
		http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}
	req, err := cniRequestToPodRequest(&cr)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
		return
	}

	logrus.Infof("Waiting for %s result for pod %s/%s", req.Command, req.PodNamespace, req.PodName)
	result, err := s.requestFunc(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("%v", err), http.StatusBadRequest)
	} else {
		// Empty response JSON means success with no body
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(result); err != nil {
			logrus.Warningf("Error writing %s HTTP response: %v", req.Command, err)
		}
	}
}
