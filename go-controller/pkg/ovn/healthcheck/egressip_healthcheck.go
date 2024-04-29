package healthcheck

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"golang.org/x/net/context"
	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/credentials/tls/certprovider/pemfile"
	"google.golang.org/grpc/security/advancedtls"
	"k8s.io/klog/v2"
)

const (
	serviceEgressIPNode = "Service_Egress_IP"
)

// UnimplementedHealthServer must be embedded to have forward compatible implementations.
type healthServer struct {
	UnimplementedHealthServer
}

func (healthServer) Check(_ context.Context, req *HealthCheckRequest) (*HealthCheckResponse, error) {
	response := HealthCheckResponse{}

	if req.GetService() == serviceEgressIPNode {
		response.Status = HealthCheckResponse_SERVING
	} else {
		response.Status = HealthCheckResponse_NOT_SERVING
	}
	return &response, nil
}

// EgressIPHealthServer interface is the means for spawning a gRPC server for
// the egress ip health check service.
type EgressIPHealthServer interface {
	Run(stopCh <-chan struct{})
}
type egressIPHealthServer struct {
	// Management port bound by server
	nodeMgmtIP net.IP

	// EgressIP Node reachability gRPC port (0 means it should use dial instead)
	healthCheckPort int
}

// NewEgressIPHealthServer allocates an Egress IP health server.
func NewEgressIPHealthServer(nodeMgmtIP net.IP, healthCheckPort int) (EgressIPHealthServer, error) {
	return &egressIPHealthServer{
		nodeMgmtIP:      nodeMgmtIP,
		healthCheckPort: healthCheckPort,
	}, nil
}

// Run spawns gRPC server for handling the egress ip health check service.
func (ehs *egressIPHealthServer) Run(stopCh <-chan struct{}) {
	nodeAddr := net.JoinHostPort(ehs.nodeMgmtIP.String(), strconv.Itoa(ehs.healthCheckPort))
	lis, err := net.Listen("tcp", nodeAddr)
	if err != nil {
		klog.Fatalf("Health checking listen failed: %v", err)
	}

	wg := &sync.WaitGroup{}

	opts := []grpc.ServerOption{}
	cfg := &config.OvnNorth
	if cfg.Cert == "" || cfg.PrivKey == "" {
		klog.Warning("Health checking using insecure connection")
	} else {
		// certProvider is responsible for reloading the certificate if it rotates.
		// Use a short RefreshDuration to ensure that the certificate is reloaded if the cluster was suspended.
		certProvider, err := pemfile.NewProvider(pemfile.Options{
			CertFile:        cfg.Cert,
			KeyFile:         cfg.PrivKey,
			RefreshDuration: time.Minute,
		})
		if err != nil {
			klog.Fatalf("Failed to create the cert provider: %v", err)
		}
		defer certProvider.Close()

		srvOpts := &advancedtls.ServerOptions{
			IdentityOptions: advancedtls.IdentityCertificateOptions{
				IdentityProvider: certProvider,
			},
		}
		serverTLSCreds, err := advancedtls.NewServerCreds(srvOpts)
		if err != nil {
			klog.Fatalf("Failed to create the server creds: %v", err)
		}
		opts = append(opts, grpc.Creds(serverTLSCreds))
	}

	grpcServer := grpc.NewServer(opts...)

	wg.Add(1)
	go func() {
		defer wg.Done()

		RegisterHealthServer(grpcServer, &healthServer{})
		klog.Infof("Starting Egress IP Health Server on %s:%d", ehs.nodeMgmtIP.String(), ehs.healthCheckPort)
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			klog.Fatalf("Egress IP Health checking server failed: %v", err)
		}
		klog.Infof("Stopped Egress IP Health Server on %s:%d", ehs.nodeMgmtIP.String(), ehs.healthCheckPort)
	}()

	<-stopCh

	klog.Info("Shutting down Egress IP Health Server")
	grpcServer.Stop()
	wg.Wait()
	klog.Info("Egress IP Health Server is shutdown")
}

// EgressIPHealthClient interface offers the functions needed for connecting to
// the egress ip health check service.
type EgressIPHealthClient interface {
	IsConnected() bool
	Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool
	Disconnect()
	Probe(dialCtx context.Context) bool
}

type egressIPHealthClient struct {
	nodeName string
	nodeAddr string
	conn     *grpc.ClientConn
	// the probeFailed state is used to mitigate situations when
	// connection just went down. With that, we do not declare node
	// unreachable unless connection could not be re-established.
	probeFailed bool
}

// NewEgressIPHealthClient allocates an Egress IP health client.
func NewEgressIPHealthClient(nodeName string) EgressIPHealthClient {
	return &egressIPHealthClient{nodeName: nodeName}
}

// IsConnected returns whether client session is established or not.
func (ehc *egressIPHealthClient) IsConnected() bool {
	return ehc.conn != nil
}

// Connect attempts to establish gRPC session with the egress ip health check service.
func (ehc *egressIPHealthClient) Connect(dialCtx context.Context, mgmtIPs []net.IP, healthCheckPort int) bool {
	var conn *grpc.ClientConn
	var nodeAddr string
	var err error

	opts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
			return proxy.Dial(ctx, "tcp", s)
		}),
	}
	cfg := &config.OvnNorth
	if cfg.CACert == "" || cfg.CertCommonName == "" {
		klog.Warning("Health checking using insecure connection")
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		// Set up the credentials for the connection.
		creds, err := credentials.NewClientTLSFromFile(cfg.CACert, cfg.CertCommonName)
		if err != nil {
			klog.Errorf("Health checking TLS key failed: %v", err)
			return false
		}
		opts = append(opts, grpc.WithTransportCredentials(creds))
	}
	for _, nodeMgmtIP := range mgmtIPs {
		nodeAddr = net.JoinHostPort(nodeMgmtIP.String(), strconv.Itoa(healthCheckPort))
		conn, err = grpc.DialContext(dialCtx, nodeAddr, opts...)
		if err == nil && conn != nil {
			break
		}
		klog.Warningf("Could not connect to %s (%s): %v", ehc.nodeName, nodeAddr, err)
	}
	if conn == nil {
		return false
	}

	klog.Infof("Connected to %s (%s)", ehc.nodeName, nodeAddr)
	ehc.nodeAddr = nodeAddr
	ehc.conn = conn
	return true
}

// Disconnect stops gRPC session with the egress ip health check service.
func (ehc *egressIPHealthClient) Disconnect() {
	if ehc.conn != nil {
		klog.Infof("Closing connection with %s (%s)", ehc.nodeName, ehc.nodeAddr)
		ehc.conn.Close()
		ehc.conn = nil
	}
}

// Probe checks the health of egress ip service using a connected gRPC session.
func (ehc *egressIPHealthClient) Probe(dialCtx context.Context) bool {
	if ehc.conn == nil {
		// should never happen
		klog.Warningf("Unexpected probing before connecting %s", ehc.nodeName)
		return false
	}

	response, err := NewHealthClient(ehc.conn).Check(dialCtx, &HealthCheckRequest{Service: serviceEgressIPNode})
	if err != nil {
		// check failed. What we will return here will depend on ehc.probeFailed. If this is the first failure,
		// let's tolerate it to account for cases where session went down and we just need it re-established.
		// Otherwise, declare it failed.
		klog.V(5).Infof("Probe failed %s (%s): %s", ehc.nodeName, ehc.nodeAddr, err)
		ehc.Disconnect()
		prevProbeFailed := ehc.probeFailed
		ehc.probeFailed = true
		return !prevProbeFailed
	}

	ehc.probeFailed = false
	klog.V(5).Infof("Got response from %s (%s): %v", ehc.nodeName, ehc.nodeAddr, response.GetStatus())
	return response.GetStatus() == HealthCheckResponse_SERVING
}
