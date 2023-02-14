package nodereachability

import (
	"context"
	"k8s.io/klog/v2"
	"net"
	"os"

	"syscall"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
)

type nodeReachabilityDialer interface {
	dial(ip net.IP, timeout time.Duration) bool
}
type nodeReachabilityDial struct{}

var dialer nodeReachabilityDialer = &nodeReachabilityDial{}

func IsReachable(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool {
	reachable := false
	// Check if we need to do node reachability check
	totalTimeout := config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout
	if totalTimeout == 0 {
		return true
	}
	healthCheckPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
	if healthCheckPort == 0 {
		reachable = isReachableLegacy(nodeName, mgmtIPs, totalTimeout)
	} else {
		reachable = isReachableViaGRPC(mgmtIPs, healthClient, healthCheckPort, totalTimeout)
	}
	return reachable
}

func isReachableLegacy(node string, mgmtIPs []net.IP, totalTimeout int) bool {
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
			if dialer.dial(ip, timeout) {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
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
func (e *nodeReachabilityDial) dial(ip net.IP, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), "9"), timeout)
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

func isReachableViaGRPC(mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient, healthCheckPort, totalTimeout int) bool {
	dialCtx, dialCancel := context.WithTimeout(context.Background(), time.Duration(totalTimeout)*time.Second)
	defer dialCancel()

	if !healthClient.IsConnected() {
		// gRPC session is not up. Attempt to connect and if that suceeds, we will declare node as reacheable.
		return healthClient.Connect(dialCtx, mgmtIPs, healthCheckPort)
	}

	// gRPC session is already established. Send a probe, which will succeed, or close the session.
	return healthClient.Probe(dialCtx)
}

func CheckNodesReachability(stopCh <-chan struct{}, checkNodesReachabilityIterate func()) {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			checkNodesReachabilityIterate()
		case <-stopCh:
			klog.V(5).Infof("Stop channel got triggered: will stop CheckNodesReachability")
			return
		}
	}
}
