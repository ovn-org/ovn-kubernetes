package node

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/pkg/errors"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

var updateInterval time.Duration = 500 * time.Millisecond

type proxierHealthUpdater struct {
	address      string
	nodeRef      *kapi.ObjectReference
	recorder     record.EventRecorder
	c            clock.Clock
	healthy      bool
	lastCalled   time.Time
	lastUpdated  time.Time
	watchFactory factory.NodeWatchFactory
	nsn          ktypes.NamespacedName
}

// newNodeProxyHealthzServer creates and returns a new proxier health server
// if the HealthzBindAddress configuration is set. Cloud load balancers use this
// health check to determine if the node is available for services with ClusterIP
// traffic policy.
func newNodeProxyHealthzServer(nodeName, address string, eventRecorder record.EventRecorder, wf factory.NodeWatchFactory) (*proxierHealthUpdater, error) {
	podName := os.Getenv("POD_NAME")
	if len(podName) == 0 {
		return nil, fmt.Errorf("found empty env variable POD_NAME")
	}
	return &proxierHealthUpdater{
		address:     address,
		recorder:    eventRecorder,
		c:           clock.RealClock{},
		healthy:     true,
		lastUpdated: time.Time{},
		nodeRef: &kapi.ObjectReference{
			Kind:      "Node",
			Name:      nodeName,
			UID:       ktypes.UID(nodeName),
			Namespace: "",
		},
		nsn: ktypes.NamespacedName{
			Namespace: config.Kubernetes.OVNConfigNamespace,
			Name:      podName},
		watchFactory: wf,
	}, nil
}

func (phu *proxierHealthUpdater) isOvnkNodePodTerminating() bool {
	pod, err := phu.watchFactory.GetPod(phu.nsn.Namespace, phu.nsn.Name)
	if err != nil {
		klog.Errorf("Could not retrieve pod %s/%s: %v", phu.nsn.Namespace, phu.nsn.Name, err)
		return false
	}
	return pod.DeletionTimestamp != nil
}

// isOvnkNodePodHealthy runs isOvnkNodePodTerminating at most every 500 ms and returns true
// if the ovnkube node pod is not set for deletion.
func (phu *proxierHealthUpdater) isOvnkNodePodHealthy() bool {
	now := phu.c.Now()
	phu.lastCalled = now
	if phu.lastUpdated != (time.Time{}) && now.Sub(phu.lastUpdated) < updateInterval {
		return phu.healthy
	}
	phu.healthy = !phu.isOvnkNodePodTerminating()
	phu.lastUpdated = now
	return phu.healthy
}

func (phu *proxierHealthUpdater) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "application/json")
	resp.Header().Set("X-Content-Type-Options", "nosniff")
	if phu.isOvnkNodePodHealthy() {
		resp.WriteHeader(http.StatusOK)
	} else {
		resp.WriteHeader(http.StatusServiceUnavailable)
	}

	fmt.Fprintf(resp, `{"lastUpdated": %q,"currentTime": %q}`, phu.lastUpdated, phu.lastCalled)
}

// serveNodeProxyHealthz initializes and runs the healthz server. It will always
// report healthy while the node process is running.
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
