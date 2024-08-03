package app

import (
	"context"
	"k8s.io/klog/v2"
	"net/http"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
	kexec "k8s.io/utils/exec"
)

var metricsScrapeInterval int

var OvsExporterCommand = cli.Command{
	Name:  "ovs-exporter",
	Usage: "",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "metrics-bind-address",
			Usage: `The IP address and port for the metrics server to serve on (default ":9310")`,
		},
		&cli.IntFlag{
			Name:        "metrics-interval",
			Usage:       "The interval in seconds at which ovs metrics are collected",
			Value:       30,
			Destination: &metricsScrapeInterval,
		},
	},
	Action: func(ctx *cli.Context) error {
		stopChan := make(chan struct{})
		bindAddress := ctx.String("metrics-bind-address")
		if bindAddress == "" {
			bindAddress = "0.0.0.0:9310"
		}

		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}

		// start the ovsdb client for ovs metrics monitoring
		ovsClient, err := libovsdb.NewOVSClient(stopChan)
		if err != nil {
			klog.Errorf("Error initializing ovs client: %v", err)
		}

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		// register ovs metrics that will be served off of /metrics path
		metrics.RegisterStandaloneOvsMetrics(ovsClient, metricsScrapeInterval, stopChan)

		server := &http.Server{Addr: bindAddress, Handler: mux}
		go func() {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				klog.Exitf("Metrics server exited with error: %v", err)
			}
		}()

		// run until cancelled
		<-ctx.Context.Done()
		close(stopChan)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Error stopping metrics server: %v", err)
		}

		return nil
	},
}
