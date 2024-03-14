package healthcheck

import (
	"context"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/klog/v2"
)

// IsReachable checks the reachability of a node through the health check
// service if its port is provided (!=0) or through the well-known discard
// service. A 0 timeout inhibits the check.
func IsReachable(ctx context.Context, nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient, port, timeout int) bool {
	// Check if we need to do node reachability check
	if timeout == 0 {
		return true
	}
	if port == 0 {
		return isReachableLegacy(ctx, nodeName, mgmtIPs, timeout)
	}
	return isReachableViaGRPC(ctx, mgmtIPs, healthClient, port, timeout)
}

func isReachableViaGRPC(ctx context.Context, mgmtIPs []net.IP, client healthcheck.EgressIPHealthClient, port, timeout int) bool {
	dialCtx, dialCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer dialCancel()

	if !client.IsConnected() {
		// gRPC session is not up. Attempt to connect and if that suceeds, we will declare node as reacheable.
		return client.Connect(dialCtx, mgmtIPs, port)
	}

	// gRPC session is already established. Send a probe, which will succeed, or close the session.
	return client.Probe(dialCtx)
}

func isReachableLegacy(ctx context.Context, node string, mgmtIPs []net.IP, totalTimeout int) bool {
	var retryTimeOut, initialRetryTimeOut time.Duration

	numMgmtIPs := len(mgmtIPs)
	if numMgmtIPs == 0 {
		return false
	}

	switch totalTimeout {
	// Check if we need to do node reachability check
	case 0:
		return true
	case 1:
		// Using time duration for initial retry with 700/numIPs msec and retry of 100/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(700/numMgmtIPs) * time.Millisecond
		retryTimeOut = time.Duration(100/numMgmtIPs) * time.Millisecond
	default:
		// Using time duration for initial retry with 900/numIPs msec
		// to ensure total wait time will be in range with the configured value including a sleep of 100msec between attempts.
		initialRetryTimeOut = time.Duration(900/numMgmtIPs) * time.Millisecond
		retryTimeOut = initialRetryTimeOut
	}

	timeout := initialRetryTimeOut
	endTime := time.Now().Add(time.Second * time.Duration(totalTimeout))
	for time.Now().Before(endTime) {
		for _, ip := range mgmtIPs {
			if dialDiscardService(ctx, ip, timeout) {
				return true
			}
		}
		util.SleepWithContext(ctx, time.Duration(100)*time.Millisecond)
		timeout = retryTimeOut
	}
	klog.Errorf("Failed reachability check for %s", node)
	return false
}

// Blantant copy from: https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by trying to
// open a TCP connection to the "discard" service (port 9); if the node is offline, the
// attempt will either time out with no response, or else return "no route to host" (and
// we will return false). If the node is online then we presumably will get a "connection
// refused" error; but the code below assumes that anything other than timeout or "no
// route" indicates that the node is online.
func dialDiscardService(ctx context.Context, ip net.IP, timeout time.Duration) bool {
	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	defer dialCancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), "9"))
	if conn != nil {
		conn.Close()
	}
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Timeout() {
			return false
		}
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok && sysErr.Err == syscall.EHOSTUNREACH {
			return false
		}
	}
	return true
}
