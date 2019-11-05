// +build windows

package cni

import (
	"fmt"
	"net"

	"github.com/sirupsen/logrus"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
)

// Start the Server's local HTTP server on a TCP port
// TODO: use HTTPS for better security
// TODO: even better to use named pipes instead
// requestFunc will be called to handle pod setup/teardown operations on each
// request to the Server's HTTP server, and should return the response bytes,
// or an error when the operation has completed.
func (s *Server) Start(requestFunc cniRequestFunc) error {
	if requestFunc == nil {
		return fmt.Errorf("no pod request handler")
	}
	s.requestFunc = requestFunc

	l, err := net.Listen("tcp", serverTCPAddress)
	if err != nil {
		return fmt.Errorf("failed to listen on pod info socket: %v", err)
	}

	s.SetKeepAlivesEnabled(false)
	// TODO: We should have the ovnkube as a service on Windows as well so it can benefit from automatic restarts and so on.
	// We have two options:
	//  - use service wrappers to be able to register ovnkube as a Windows service;
	//  - add support in ovn-kubernetes such that ovnkube will be able to run without additional wrappers as a Windows service (example for kubernetes: kubernetes/kubernetes#60144 )

	go utilwait.Forever(func() {
		if err := s.Serve(l); err != nil {
			utilruntime.HandleError(fmt.Errorf("CNI server Serve() failed: %v", err))
		}
	}, 0)
	logrus.Infof("CNI server started")
	return nil
}
