//go:build linux
// +build linux

package cni

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
)

// Start the Server's local HTTP server on a root-owned Unix domain socket.
// handlePodRequestFunc will be called to handle pod setup/teardown operations on each
// request to the Server's HTTP server, and should return the response bytes,
// or an error when the operation has completed.
func (s *Server) Start(rundir string) error {
	socketPath := filepath.Join(rundir, serverSocketName)

	// For security reasons the socket must be accessible only to root.
	// Listen() (which creates the socket) cannot set permissions thus the
	// socket is created with the parent directory's permissions. The
	// parent must also be root-only to avoid a race between socket creation
	// and a subsequent Chmod().
	// Unfortunately, if we are running in a container and our socket
	// parent directory has been bind-mounted into the container we cannot
	// remove the parent. Instead we verify its permissions and return an
	// error if they are not root-only.

	// Remove and re-create the socket directory with root-only permissions
	if err := os.RemoveAll(rundir); err != nil && !os.IsNotExist(err) {
		info, err := os.Stat(rundir)
		if err != nil {
			return fmt.Errorf("failed to stat old pod info socket directory %s: %v", rundir, err)
		}
		// Owner must be root
		tmp := info.Sys()
		statt, ok := tmp.(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("failed to read pod info socket directory stat info: %T", tmp)
		}
		if statt.Uid != 0 {
			return fmt.Errorf("insecure owner of pod info socket directory %s: %v", rundir, statt.Uid)
		}

		// Check permissions
		if info.Mode()&0o777 != 0o700 {
			return fmt.Errorf("insecure permissions on pod info socket directory %s: %v", rundir, info.Mode())
		}

		// Finally remove the socket file so we can re-create it
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove old pod info socket %s: %v", socketPath, err)
		}
	}
	if err := os.MkdirAll(rundir, 0o700); err != nil {
		return fmt.Errorf("failed to create pod info socket directory %s: %v", rundir, err)
	}

	// On Linux the socket is created with the permissions of the directory
	// it is in, so as long as the directory is root-only we can avoid
	// racy umask manipulation.
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on pod info socket: %v", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
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
