package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/probe"
	httpprober "k8s.io/kubernetes/pkg/probe/http"
	utilpointer "k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/scheme"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovnwebhook"
	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
)

type config struct {
	kubeconfig                 string
	apiServer                  string
	logLevel                   int
	port                       int
	host                       string
	certDir                    string
	metricsAddress             string
	leaseNamespace             string
	enableInterconnect         bool
	enableHybridOverlay        bool
	disableWebhook             bool
	disableApprover            bool
	waitForKAPIDuration        time.Duration
	localKAPIPort              int
	extraAllowedUsers          cli.StringSlice
	csrAcceptanceConditionFile string
	csrAcceptanceConditions    []csrapprover.CSRAcceptanceCondition
	podAdmissionConditionFile  string
	podAdmissionConditions     []ovnwebhook.PodAdmissionConditionOption
}

var cliCfg config
var logger = klog.NewKlogr()

// waitForKAPIToStop waits for the local kubernetes-api instance
// to terminate by probing the localhost readyz endpoint.
// If the probe is successful, it means that kubernetes-api is not terminating and the function exits.
// The function periodically (every 5s) probes the readiness endpoint until the request times out or there is an error.
func waitForKAPIToStop(duration time.Duration) error {
	klog.Infof("Waiting (%s) for kubernetes-api to stop...", duration)

	prober := httpprober.New(false)
	kapiURL, err := url.Parse(fmt.Sprintf("https://localhost:%d/readyz", cliCfg.localKAPIPort))
	if err != nil {
		return fmt.Errorf("failed to parse kubernetes-api url: %v", err)
	}
	req, err := httpprober.NewProbeRequest(kapiURL, http.Header{})
	if err != nil {
		return fmt.Errorf("failed to create probe request: %v", err)
	}

	timeoutCh := time.After(cliCfg.waitForKAPIDuration)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			res, failure, err := prober.Probe(req, time.Second)
			if err != nil {
				return fmt.Errorf("HTTP probe failed: %v", err)
			}

			// kube-apiserver will start returning probe failures as soon as it starts to terminate
			if res == probe.Success {
				klog.Infof("kube-apiserver is not terminating, exiting...")
				return nil
			}
			// failure contains the error string on connectivity errors,
			// if the api is down, the connection will get refused
			if res == probe.Failure && strings.Contains(failure, "connection refused") {
				klog.Infof("kube-apiserver terminated, exiting...")
				return nil
			}

			klog.Infof("kube-apiserver is terminating, probe result: %s, message: %s", res, failure)
		case <-timeoutCh:
			return fmt.Errorf("timed out waiting for kubernetes-api to stop")
		}
	}
}

func main() {
	c := cli.NewApp()
	c.Name = "ovnkube-identity"
	c.Usage = "run ovn-kubernetes identity manager, this includes the admission webhook and the CertificateSigningRequest approver"

	c.Action = func(c *cli.Context) error {
		ctrl.SetLogger(logger)
		var level klog.Level
		if err := level.Set(strconv.Itoa(cliCfg.logLevel)); err != nil {
			klog.Errorf("Failed to set klog log level %v", err)
			os.Exit(1)
		}

		klog.Infof("Config: %+v", cliCfg)
		restCfg, err := clientcmd.BuildConfigFromFlags("", cliCfg.kubeconfig)
		if err != nil {
			return err
		}
		if cliCfg.apiServer != "" {
			restCfg.Host = cliCfg.apiServer
		}

		cliCfg.csrAcceptanceConditions, err = csrapprover.InitCSRAcceptanceConditions(cliCfg.csrAcceptanceConditionFile)
		if err != nil {
			return err
		}

		cliCfg.podAdmissionConditions, err = ovnwebhook.InitPodAdmissionConditionOptions(cliCfg.podAdmissionConditionFile)
		if err != nil {
			return err
		}

		startWg := &sync.WaitGroup{}

		var errorList []error
		if !cliCfg.disableWebhook {
			startWg.Add(1)
			go func() {
				defer startWg.Done()
				if err := runWebhook(c, restCfg); err != nil {
					errorList = append(errorList, err)
				}
			}()
		}

		if !cliCfg.disableApprover {
			startWg.Add(1)
			go func() {
				defer startWg.Done()
				if err := runCSRApproverManager(c.Context, c.App.Name, restCfg); err != nil {
					errorList = append(errorList, err)
				}
			}()
		}

		startWg.Wait()
		return errors.NewAggregate(errorList)
	}

	c.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			Usage:       "kubeconfig path",
			Destination: &cliCfg.kubeconfig,
		},
		&cli.StringFlag{
			Name:        "k8s-apiserver",
			Usage:       "URL of the Kubernetes API server (not required if --kubeconfig is given)",
			Destination: &cliCfg.apiServer,
		},
		&cli.StringFlag{
			Name:        "lease-namespace",
			Usage:       "namespace in which the leader election lease object will be created (pod namespace by default)",
			Destination: &cliCfg.leaseNamespace,
		},
		&cli.IntFlag{
			Name:        "loglevel",
			Usage:       "log level. Note: info, warn, fatal, error are always printed no matter the log level. Use 5 for debug (default: 4)",
			Destination: &cliCfg.logLevel,
			Value:       4,
		},
		&cli.BoolFlag{
			Name:        "disable-webhook",
			Usage:       "If set webhook will not be started, cannot be used with disable-approver",
			Destination: &cliCfg.disableWebhook,
			Value:       false,
		},
		&cli.BoolFlag{
			Name:        "disable-approver",
			Usage:       "If set CSR approver will not be started, cannot be used with disable-webhook",
			Destination: &cliCfg.disableApprover,
			Value:       false,
		},
		&cli.StringFlag{
			Name:        "webhook-cert-dir",
			Usage:       "directory that contains the server key and certificate",
			Destination: &cliCfg.certDir,
		},
		&cli.StringFlag{
			Name:        "webhook-host",
			Usage:       "the address that the webhook server will listen on",
			Value:       "localhost",
			Destination: &cliCfg.host,
		},
		&cli.IntFlag{
			Name:        "webhook-port",
			Usage:       "port number that the webhook server will serve",
			Value:       webhook.DefaultPort,
			Destination: &cliCfg.port,
		},
		&cli.StringFlag{
			Name:        "metrics-address",
			Usage:       "address that the metrics server will serve. (Default: \"0\", which means disabled)",
			Value:       "0",
			Destination: &cliCfg.metricsAddress,
		},
		&cli.BoolFlag{
			Name:        "enable-interconnect",
			Usage:       "Configure to enable ovn interconnect checks",
			Destination: &cliCfg.enableInterconnect,
			Value:       false,
		},
		&cli.BoolFlag{
			Name:        "enable-hybrid-overlay",
			Usage:       "Configure to enable hybrid overlay checks",
			Destination: &cliCfg.enableHybridOverlay,
			Value:       false,
		},
		&cli.StringSliceFlag{
			Name:        "extra-allowed-user",
			Usage:       "Configure extra user that is allowed to modify annotations protected by the webhook, can be used multiple times",
			Destination: &cliCfg.extraAllowedUsers,
		},
		&cli.DurationFlag{
			Name:        "wait-for-kubernetes-api",
			Usage:       "Configure the duration to wait for the local kubernetes API to terminate before exiting. (Default: \"0\", which means no wait)",
			Destination: &cliCfg.waitForKAPIDuration,
			Value:       0,
		},
		&cli.IntFlag{
			Name:        "local-kubernetes-api-port",
			Usage:       "Configure the port number that will be used determine if the local kubernetes API terminated",
			Value:       6443,
			Destination: &cliCfg.localKAPIPort,
		},
		&cli.StringFlag{
			Name:        "csr-acceptance-conditions",
			Usage:       "Configure additional certificate acceptance conditions",
			Destination: &cliCfg.csrAcceptanceConditionFile,
		},
		&cli.StringFlag{
			Name:        "pod-admission-conditions",
			Usage:       "Configure additional pod validate admission conditions",
			Destination: &cliCfg.podAdmissionConditionFile,
		},
	}
	ctx := context.Background()

	// trap SIGHUP, SIGINT, SIGTERM, SIGQUIT and
	// cancel the context
	ctx, cancel := context.WithCancel(ctx)
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer func() {
		signal.Stop(exitCh)
		cancel()
	}()
	go func() {
		select {
		case s := <-exitCh:
			klog.Infof("Received signal %s....", s)
			if cliCfg.waitForKAPIDuration != 0 {
				if err := waitForKAPIToStop(cliCfg.waitForKAPIDuration); err != nil {
					klog.Errorf("Failed waiting for kubernetes-apiserver to stop: %v", err)
				}
			}
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := c.RunContext(ctx, os.Args); err != nil {
		klog.Exit(err)
	}
}

func runWebhook(c *cli.Context, restCfg *rest.Config) error {
	// We cannot use the default implementation of the webhook server because we need to enable SO_REUSEPORT
	// on the socket to allow for two instances running at the same time (required during upgrades).
	// The webhook server is set up and started in a very similar way to the default one:
	// https://github.com/ovn-org/ovn-kubernetes/blob/7c0838bb46d6de202f509abe47609c8da09311b2/go-controller/vendor/sigs.k8s.io/controller-runtime/pkg/webhook/server.go#L212

	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("error creating clientset: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)

	webhookMux := http.NewServeMux()

	nodeWebhook := admission.WithCustomValidator(
		scheme.Scheme,
		&corev1.Node{},
		ovnwebhook.NewNodeAdmissionWebhook(cliCfg.enableInterconnect, cliCfg.enableHybridOverlay, cliCfg.extraAllowedUsers.Value()...),
	).WithRecoverPanic(true)

	nodeHandler, err := admission.StandaloneWebhook(
		nodeWebhook,
		admission.StandaloneOptions{
			Logger:      logger.WithName("node.network-identity"),
			MetricsPath: "node.network-identity",
		},
	)
	if err != nil {
		return fmt.Errorf("failed to setup the node admission webhook: %w", err)
	}
	webhookMux.Handle("/node", nodeHandler)

	// in non-ic ovnkube-node without additional conditions does not have the permissions to update pods
	if cliCfg.enableInterconnect || len(cliCfg.csrAcceptanceConditions) > 1 {
		informerFactory := informers.NewSharedInformerFactory(client, 10*time.Minute)
		nodeInformer := informerFactory.Core().V1().Nodes().Informer()
		informerFactory.Start(stopCh)
		klog.Infof("Waiting for caches to sync")
		cache.WaitForCacheSync(c.Context.Done(), nodeInformer.HasSynced)

		nodeLister := listers.NewNodeLister(nodeInformer.GetIndexer())
		podWebhook := admission.WithCustomValidator(
			scheme.Scheme,
			&corev1.Pod{},
			ovnwebhook.NewPodAdmissionWebhook(nodeLister, cliCfg.podAdmissionConditions, cliCfg.extraAllowedUsers.Value()...),
		).WithRecoverPanic(true)
		podHandler, err := admission.StandaloneWebhook(
			podWebhook,
			admission.StandaloneOptions{
				Logger:      logger.WithName("pod.network-identity"),
				MetricsPath: "pod.network-identity",
			},
		)
		if err != nil {
			return fmt.Errorf("failed to setup the pod admission webhook: %w", err)
		}
		webhookMux.Handle("/pod", podHandler)
	}

	cfg := &tls.Config{
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS10,
	}

	certPath := filepath.Join(cliCfg.certDir, "tls.crt")
	keyPath := filepath.Join(cliCfg.certDir, "tls.key")
	certWatcher, err := certwatcher.New(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("failed to setup certwatcher: %v", err)
	}
	cfg.GetCertificate = certWatcher.GetCertificate

	go func() {
		if err := certWatcher.Start(c.Context); err != nil {
			klog.Fatalf("Certificate watcher failed to start: %v", err)
		}
	}()
	srv := &http.Server{
		Handler:           webhookMux,
		IdleTimeout:       90 * time.Second,
		ReadHeaderTimeout: 32 * time.Second,
	}

	l := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				// Enable SO_REUSEPORT
				err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if err != nil {
					klog.Fatalf("Failed to set SO_REUSEPORT:", err)
				}
			})
		},
	}

	idleWebhookConnectionsClosed := make(chan struct{})
	defer func() {
		klog.Infof("Waiting for the webhook server to gracefully close")
		<-idleWebhookConnectionsClosed
	}()
	innerListener, err := l.Listen(c.Context, "tcp", net.JoinHostPort(cliCfg.host, strconv.Itoa(cliCfg.port)))
	if err != nil {
		return fmt.Errorf("failed to create the listener: %v", err)
	}
	listener := tls.NewListener(innerListener, cfg)

	klog.Infof("Starting the webhook server")
	go func() {
		<-c.Context.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			klog.Errorf("Failed shutting down the HTTP server: %v", err)
		}
		close(idleWebhookConnectionsClosed)
	}()

	return srv.Serve(listener)
}

func runCSRApproverManager(ctx context.Context, leaderID string, restCfg *rest.Config) error {
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		MetricsBindAddress:            cliCfg.metricsAddress,
		LeaderElectionNamespace:       cliCfg.leaseNamespace,
		LeaderElectionID:              leaderID,
		LeaderElection:                true,
		LeaseDuration:                 utilpointer.Duration(time.Minute),
		RenewDeadline:                 utilpointer.Duration(time.Second * 30),
		RetryPeriod:                   utilpointer.Duration(time.Second * 20),
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}

	err = ctrl.
		NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(csrapprover.Predicate)).
		WithOptions(controller.Options{
			// Explicitly enable leader election for CSR approver
			NeedLeaderElection: utilpointer.Bool(true),
			RecoverPanic:       utilpointer.Bool(true),
		}).
		Complete(csrapprover.NewController(
			mgr.GetClient(),
			cliCfg.csrAcceptanceConditions,
			csrapprover.Usages,
			csrapprover.MaxDuration,
			mgr.GetEventRecorderFor(csrapprover.ControllerName),
		))
	if err != nil {
		klog.Errorf("Failed to create %s: %v", csrapprover.ControllerName, err)
		os.Exit(1)
	}

	klog.Info("Starting certificate signing request approver")
	return mgr.Start(ctx)
}
