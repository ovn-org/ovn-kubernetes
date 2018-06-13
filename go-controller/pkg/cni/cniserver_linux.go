// +build linux

package cni

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

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

	// Remove and re-create the socket directory with root-only permissions
	if err := os.RemoveAll(s.rundir); err != nil && !os.IsNotExist(err) {
		utilruntime.HandleError(fmt.Errorf("failed to remove old pod info socket: %v", err))
	}
	if err := os.RemoveAll(s.rundir); err != nil && !os.IsNotExist(err) {
		utilruntime.HandleError(fmt.Errorf("failed to remove contents of socket directory: %v", err))
	}
	if err := os.MkdirAll(s.rundir, 0700); err != nil {
		return fmt.Errorf("failed to create pod info socket directory: %v", err)
	}

	// On Linux the socket is created with the permissions of the directory
	// it is in, so as long as the directory is root-only we can avoid
	// racy umask manipulation.
	socketPath := filepath.Join(s.rundir, serverSocketName)
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on pod info socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		l.Close()
		return fmt.Errorf("failed to set pod info socket mode: %v", err)
	}

	s.SetKeepAlivesEnabled(false)
	go utilwait.Forever(func() {
		if err := s.Serve(l); err != nil {
			utilruntime.HandleError(fmt.Errorf("CNI server Serve() failed: %v", err))
		}
	}, 0)
	return nil
}
