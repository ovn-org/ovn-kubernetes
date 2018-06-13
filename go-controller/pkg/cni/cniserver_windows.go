// +build windows

package cni

import (
	"fmt"
	"net"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
)

// Start the Server's local HTTP server on a root-owned Unix domain socket.
// requestFunc will be called to handle pod setup/teardown operations on each
// request to the Server's HTTP server, and should return a PodResult
// when the operation has completed.
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
	go utilwait.Forever(func() {
		if err := s.Serve(l); err != nil {
			utilruntime.HandleError(fmt.Errorf("CNI server Serve() failed: %v", err))
		}
	}, 0)
	return nil
}
