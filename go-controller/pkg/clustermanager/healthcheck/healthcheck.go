package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func healthStateFromError(err error) HealthState {
	if err == nil {
		return AVAILABLE
	}

	if errors.Is(err, healthcheck.ErrNotServing) {
		// connected fine but not serving
		return UNAVAILABLE
	}

	status, ok := status.FromError(err)
	if !ok {
		// not a GRPC error
		return UNREACHABLE
	}

	if status.Code() != codes.Unavailable {
		// not an unavailable error
		return UNREACHABLE
	}

	msg := status.Message()

	// we want to interpret connection refused (TCP RST reply to a TCP SYN) as a
	// sign of unavailability rather than unreachability (best effort) however
	// this might be unrealiable if connecting through a SOCKS proxy so fallback
	// to UNREACHABLE
	// NOTE: A refused connection proxied through a single SOCKS proxy could be
	// interpreted, by spec, just the same as a direct connection refused.
	// However a SOCKS proxy that relies itself on another proxy (i.e. HTTP
	// CONNECT proxy) is not bound to the same interpretation; what could be
	// refused is the connection to that second proxy and not the final
	// destination. We can't tell both of this scenarios apart.
	if strings.Contains(msg, "connect: connection refused") && !strings.Contains(msg, "socks connect") {
		return UNAVAILABLE
	}

	return UNREACHABLE
}

type healthStateClient struct {
	nodeName    string
	isReachable func(ctx context.Context) error
	disconnect  func()
}

func newHealthStateClient(nodeName string, mgmtIPs []net.IP, port, timeout int) healthStateClient {
	c := healthStateClient{nodeName: nodeName}
	switch port {
	case 0:
		c.isReachable = func(ctx context.Context) error {
			return isReachableLegacy(ctx, mgmtIPs, timeout)
		}
		c.disconnect = func() {}
	default:
		client := healthcheck.NewEgressIPHealthClient(nodeName, mgmtIPs, port)
		c.isReachable = func(ctx context.Context) error {
			return isReachableViaGRPC(ctx, client, timeout)
		}
		c.disconnect = client.Disconnect
	}
	return c
}

func isReachableViaGRPC(ctx context.Context, client healthcheck.EgressIPHealthClient, timeout int) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer dialCancel()

	// Send a probe, which will succeed, or close the session.
	err := client.Probe(dialCtx)
	if err != nil {
		return fmt.Errorf("GRPC probe failed: %w", err)
	}

	return nil
}

func isReachableLegacy(ctx context.Context, mgmtIPs []net.IP, totalTimeout int) error {
	var retryTimeOut, initialRetryTimeOut time.Duration

	numMgmtIPs := len(mgmtIPs)
	if numMgmtIPs == 0 {
		return fmt.Errorf("legacy probe failed: no management IPs")
	}

	switch totalTimeout {
	// Check if we need to do node reachability check
	case 0:
		return nil
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

	var err error
	timeout := initialRetryTimeOut
	endTime := time.Now().Add(time.Second * time.Duration(totalTimeout))
	for time.Now().Before(endTime) {
		for _, ip := range mgmtIPs {
			err = dialDiscardService(ctx, ip, timeout)
			if err == nil {
				return nil
			}
			klog.V(5).Infof("Legacy probe failed to IP %s: %v", ip, err)
		}
		util.SleepWithContext(ctx, time.Duration(100)*time.Millisecond)
		timeout = retryTimeOut
	}

	if err != nil {
		return fmt.Errorf("legacy probe failed: %w", err)
	}

	return nil
}

// Blantant copy from:
// https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by
// trying to open a TCP connection to the "discard" service (port 9); if the
// node is offline, the attempt will either time out with no response, or else
// return "no route to host". If the node is online then we presumably will get
// a "connection refused" error; but the code below assumes that anything other
// than timeout or "no route" indicates that the node is online.
func dialDiscardService(ctx context.Context, ip net.IP, timeout time.Duration) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
	defer dialCancel()
	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), "9"))
	if conn != nil {
		conn.Close()
	}
	if opErr, ok := err.(*net.OpError); ok {
		if opErr.Timeout() {
			return err
		}
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok && sysErr.Err == syscall.EHOSTUNREACH {
			return err
		}
	}
	return nil
}
