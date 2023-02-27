package node

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

type proxierHealthUpdater struct {
	address  string
	nodeRef  *kapi.ObjectReference
	recorder record.EventRecorder
	c        clock.Clock
	healthy  bool
}

// newNodeProxyHealthzServer creates and returns a new proxier health server
// if the HealthzBindAddress configuration is set. Cloud load balancers use this
// health check to determine if the node is available for services with ClusterIP
// traffic policy.
func newNodeProxyHealthzServer(nodeName, address string, eventRecorder record.EventRecorder) *proxierHealthUpdater {
	return &proxierHealthUpdater{
		address:  address,
		recorder: eventRecorder,
		c:        clock.RealClock{},
		healthy:  true,
		nodeRef: &kapi.ObjectReference{
			Kind:      "Node",
			Name:      nodeName,
			UID:       ktypes.UID(nodeName),
			Namespace: "",
		},
	}
}

func (phu *proxierHealthUpdater) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "application/json")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	if phu.healthy {
		resp.WriteHeader(http.StatusOK)
	} else {
		resp.WriteHeader(http.StatusServiceUnavailable)
	}
	now := phu.c.Now()
	fmt.Fprintf(resp, `{"lastUpdated": %q,"currentTime": %q}`, now, now)
}

// serveNodeProxyHealthz initializes and runs the healthz server. It will always
// report healthy while the node process is running.
// TODO: connect node health to something useful
func (phu *proxierHealthUpdater) Start(stopChan chan struct{}, wg *sync.WaitGroup) {
	serveMux := http.NewServeMux()
	serveMux.Handle("/healthz", phu)
	server := &http.Server{
		Addr:    phu.address,
		Handler: serveMux,
	}

	startedWg := &sync.WaitGroup{}

	wg.Add(1)
	startedWg.Add(1)
	go func() {
		defer wg.Done()
		startedWg.Done()
		<-stopChan
		server.Close()
	}()

	wg.Add(1)
	startedWg.Add(1)
	go func() {
		defer wg.Done()
		startedWg.Done()

		klog.V(3).InfoS("Starting node proxy healthz server", "address", phu.address)
		for {
			err := server.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				return
			}
			msg := fmt.Sprintf("serving healthz on %s failed: %v", phu.address, err)
			phu.recorder.Eventf(phu.nodeRef, kapi.EventTypeWarning, "FailedToStartProxierHealthcheck", "StartOVNKubernetesNode", msg)
			klog.Errorf(msg)
			time.Sleep(5 * time.Second)
		}
	}()

	startedWg.Wait()
}
