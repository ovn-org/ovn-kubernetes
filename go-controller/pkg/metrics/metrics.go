package metrics

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilwait "k8s.io/apimachinery/pkg/util/wait"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	MetricOvnkubeNamespace       = "ovnkube"
	MetricOvnkubeSubsystemMaster = "master"
	MetricOvnkubeSubsystemNode   = "node"
	MetricOvnNamespace           = "ovn"
	MetricOvnSubsystemDBRaft     = "db_raft"
)

// Build information. Populated at build-time.
var (
	Commit    string
	Branch    string
	BuildUser string
	BuildDate string
)

// StartMetricsServer runs the prometheus listner so that metrics can be collected
func StartMetricsServer(bindAddress string, enablePprof bool) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	if enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	go utilwait.Until(func() {
		err := http.ListenAndServe(bindAddress, mux)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("starting metrics server failed: %v", err))
		}
	}, 5*time.Second, utilwait.NeverStop)

}
