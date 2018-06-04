package cni

import (
	"net/http"
)

// Default directory for CNIServer runtime files
const CNIServerRunDir string = "/var/run/"

// CNIServer socket name, and default full path
const CNIServerSocketName string = "ovn-cni-server.sock"
const CNIServerSocketPath string = CNIServerRunDir + "/" + CNIServerSocketName

// Explicit type for CNI commands the server handles
type CNICommand string

const CNI_ADD CNICommand = "ADD"
const CNI_UPDATE CNICommand = "UPDATE"
const CNI_DEL CNICommand = "DEL"

// Request sent to the CNIServer by the OVN CNI plugin
type CNIRequest struct {
	// CNI environment variables, like CNI_COMMAND and CNI_NETNS
	Env map[string]string `json:"env,omitempty"`
	// CNI configuration passed via stdin to the CNI plugin
	Config []byte `json:"config,omitempty"`
}

// Request structure built from CNIRequest which is passed to the
// handler function given to the CNIServer at creation time
type PodRequest struct {
	// The CNI command of the operation
	Command CNICommand
	// kubernetes namespace name
	PodNamespace string
	// kubernetes pod name
	PodName string
	// kubernetes container ID
	SandboxID string
	// kernel network namespace path
	Netns string
	// Interface name to be configured
	IfName string
	// CNI version string obtained from stdin conf
	CNIVersion string
	// Channel for returning the operation result to the CNIServer
	Result chan *PodResult
}

// Result of a PodRequest sent through the PodRequest's Result channel.
type PodResult struct {
	// Response to be returned to the OpenShift SDN CNI plugin on success
	Response []byte
	// Error to be returned to the OpenShift SDN CNI plugin on failure
	Err error
}

type cniRequestFunc func(request *PodRequest) ([]byte, error)

// CNI server object that listens for JSON-marshaled CNIRequest objects
// on a private root-only Unix domain socket.
type CNIServer struct {
	http.Server
	requestFunc cniRequestFunc
	rundir      string
}
