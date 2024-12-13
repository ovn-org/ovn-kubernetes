package clustermanager

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	ocpcloudnetworkapi "github.com/openshift/api/cloudnetwork/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	objretry "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	egressIPReachabilityCheckInterval = 5 * time.Second
)

type egressIPHealthcheckClientAllocator struct{}

func (hccAlloc *egressIPHealthcheckClientAllocator) allocate(nodeName string) healthcheck.EgressIPHealthClient {
	return healthcheck.NewEgressIPHealthClient(nodeName)
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

type egressIPDialer interface {
	dial(ip net.IP, timeout time.Duration) bool
}

type egressIPDial struct{}

var dialer egressIPDialer = &egressIPDial{}

type healthcheckClientAllocator interface {
	allocate(nodeName string) healthcheck.EgressIPHealthClient
}

// Blantant copy from: https://github.com/openshift/sdn/blob/master/pkg/network/common/egressip.go#L499-L505
// Ping a node and return whether or not we think it is online. We do this by trying to
// open a TCP connection to the "discard" service (port 9); if the node is offline, the
// attempt will either time out with no response, or else return "no route to host" (and
// we will return false). If the node is online then we presumably will get a "connection
// refused" error; but the code below assumes that anything other than timeout or "no
// route" indicates that the node is online.
func (e *egressIPDial) dial(ip net.IP, timeout time.Duration) bool {
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

var hccAllocator healthcheckClientAllocator = &egressIPHealthcheckClientAllocator{}

// egressNode is a cache helper used for egress IP assignment, representing an egress node
type egressNode struct {
	egressIPConfig     *util.ParsedNodeEgressIPConfiguration
	mgmtIPs            []net.IP
	allocations        map[string]string
	healthClient       healthcheck.EgressIPHealthClient
	isReady            bool
	isReachable        bool
	isEgressAssignable bool
	name               string
}

func (e *egressNode) getAllocationCountForEgressIP(name string) (count int) {
	for _, egressIPName := range e.allocations {
		if egressIPName == name {
			count++
		}
	}
	return
}

func (eIPC *egressIPClusterController) getAllocationTotalCount() float64 {
	count := 0
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	for _, eNode := range eIPC.nodeAllocator.cache {
		count += len(eNode.allocations)
	}
	return float64(count)
}

// nodeAllocator contains all the information required to manage EgressIP assignment to egress node. This includes assignment
// of EgressIP IPs to nodes and ensuring the egress nodes are reachable. For cloud nodes, it also tracks limits for
// IP assignment to each node.
type nodeAllocator struct {
	*sync.Mutex
	// A cache used for egress IP assignments containing data for all cluster nodes
	// used for egress IP assignments
	cache map[string]*egressNode
}

type cloudPrivateIPConfigOp struct {
	toAdd    string
	toDelete string
}

// ipStringToCloudPrivateIPConfigName converts the net.IP string representation
// to a CloudPrivateIPConfig compatible name.

// The string representation of the IPv6 address fc00:f853:ccd:e793::54 will be
// represented as: fc00.f853.0ccd.e793.0000.0000.0000.0054

// We thus need to fully expand the IP string and replace every fifth
// character's colon with a dot.
func ipStringToCloudPrivateIPConfigName(ipString string) (name string) {
	ip := net.ParseIP(ipString)
	if ip.To4() != nil {
		return ipString
	}
	dst := make([]byte, hex.EncodedLen(len(ip)))
	hex.Encode(dst, ip)
	for i := 0; i < len(dst); i += 4 {
		if len(dst)-i == 4 {
			name += string(dst[i : i+4])
		} else {
			name += string(dst[i:i+4]) + "."
		}
	}
	return
}

func (eIPC *egressIPClusterController) executeCloudPrivateIPConfigOps(egressIPName string, ops map[string]*cloudPrivateIPConfigOp) error {
	for egressIP, op := range ops {
		cloudPrivateIPConfigName := ipStringToCloudPrivateIPConfigName(egressIP)
		cloudPrivateIPConfig, err := eIPC.watchFactory.GetCloudPrivateIPConfig(cloudPrivateIPConfigName)
		// toAdd and toDelete is non-empty, this indicates an UPDATE for which
		// the object **must** exist, if not: that's an error.
		if op.toAdd != "" && op.toDelete != "" {
			if err != nil {
				return fmt.Errorf("cloud update request failed for CloudPrivateIPConfig: %s, could not get item, err: %v", cloudPrivateIPConfigName, err)
			}
			// Do not update if object is being deleted
			if !cloudPrivateIPConfig.GetDeletionTimestamp().IsZero() {
				return fmt.Errorf("cloud update request failed, CloudPrivateIPConfig: %s is being deleted", cloudPrivateIPConfigName)
			}
			cloudPrivateIPConfig.Spec.Node = op.toAdd
			if _, err := eIPC.kube.UpdateCloudPrivateIPConfig(cloudPrivateIPConfig); err != nil {
				eIPRef := v1.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "CloudUpdateFailed", "egress IP: %s for object EgressIP: %s could not be updated, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud update request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
			// toAdd is non-empty, this indicates an ADD
			// if the object already exists for the specified node that's a no-op
			// if the object already exists and the request is for a different node, that's an error
		} else if op.toAdd != "" {
			if err == nil {
				// Do not add if object is being deleted; either retry the add (if this was an update) OR (if this was a perm-delete)
				// we will retry till we exhaust our retry count
				if cloudPrivateIPConfig.GetDeletionTimestamp() != nil && !cloudPrivateIPConfig.GetDeletionTimestamp().IsZero() {
					return fmt.Errorf("cloud update request failed, CloudPrivateIPConfig: %s is being deleted", cloudPrivateIPConfigName)
				}
				if op.toAdd == cloudPrivateIPConfig.Spec.Node {
					klog.Infof("CloudPrivateIPConfig: %s already assigned to node: %s", cloudPrivateIPConfigName, cloudPrivateIPConfig.Spec.Node)
					continue
				}
				return fmt.Errorf("cloud request failed for CloudPrivateIPConfig: %s, err: cannot be assigned to node %s because cloud has it in node %s",
					cloudPrivateIPConfigName, op.toAdd, cloudPrivateIPConfig.Spec.Node)
			}
			cloudPrivateIPConfig := ocpcloudnetworkapi.CloudPrivateIPConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name: cloudPrivateIPConfigName,
					Annotations: map[string]string{
						util.OVNEgressIPOwnerRefLabel: egressIPName,
					},
				},
				Spec: ocpcloudnetworkapi.CloudPrivateIPConfigSpec{
					Node: op.toAdd,
				},
			}
			if _, err := eIPC.kube.CreateCloudPrivateIPConfig(&cloudPrivateIPConfig); err != nil {
				eIPRef := v1.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "CloudAssignmentFailed", "egress IP: %s for object EgressIP: %s could not be created, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud add request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
			// toDelete is non-empty, this indicates a DELETE - if the object does not exist, log an Info message and continue with the next op.
			// The reason for why we are not throwing an error here is that desired state (deleted) == isState (object not found).
			// If for whatever reason we have a pending toDelete op for a deleted object, then this op should simply be silently ignored.
			// Any other error, return an error to trigger a retry.
		} else if op.toDelete != "" {
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.Infof("Cloud deletion request failed for CloudPrivateIPConfig: %s, item already deleted, err: %v", cloudPrivateIPConfigName, err)
					continue
				} else {
					return fmt.Errorf("cloud deletion request failed for CloudPrivateIPConfig: %s, could not get item, err: %v", cloudPrivateIPConfigName, err)
				}
			}
			if err := eIPC.kube.DeleteCloudPrivateIPConfig(cloudPrivateIPConfigName); err != nil {
				eIPRef := v1.ObjectReference{
					Kind: "EgressIP",
					Name: egressIPName,
				}
				eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "CloudDeletionFailed", "egress IP: %s for object EgressIP: %s could not be deleted, err: %v", egressIP, egressIPName, err)
				return fmt.Errorf("cloud deletion request failed for CloudPrivateIPConfig: %s, err: %v", cloudPrivateIPConfigName, err)
			}
		}
	}
	return nil
}

// executeCloudPrivateIPConfigChange computes a diff between what needs to be
// assigned/removed and executes the object modification afterwards.
// Specifically: if one egress IP is moved from nodeA to nodeB, we actually care
// about an update on the CloudPrivateIPConfig object represented by that egress
// IP, cloudPrivateIPConfigOp is a helper used to determine that sort of
// operations from toAssign/toRemove
func (eIPC *egressIPClusterController) executeCloudPrivateIPConfigChange(egressIPName string, toAssign, toRemove []egressipv1.EgressIPStatusItem) error {
	eIPC.pendingCloudPrivateIPConfigsMutex.Lock()
	defer eIPC.pendingCloudPrivateIPConfigsMutex.Unlock()
	ops := make(map[string]*cloudPrivateIPConfigOp, len(toAssign)+len(toRemove))
	for _, assignment := range toAssign {
		ops[assignment.EgressIP] = &cloudPrivateIPConfigOp{
			toAdd: assignment.Node,
		}
	}
	for _, removal := range toRemove {
		if op, exists := ops[removal.EgressIP]; exists {
			op.toDelete = removal.Node
		} else {
			ops[removal.EgressIP] = &cloudPrivateIPConfigOp{
				toDelete: removal.Node,
			}
		}
	}
	// Merge ops into the existing pendingCloudPrivateIPConfigsOps.
	// This allows us to:
	// a) execute only the new ops
	// b) keep track of any pending changes
	if len(ops) > 0 {
		if _, ok := eIPC.pendingCloudPrivateIPConfigsOps[egressIPName]; !ok {
			// Set all operations for the EgressIP object if none are in the cache currently.
			eIPC.pendingCloudPrivateIPConfigsOps[egressIPName] = ops
		} else {
			for cloudPrivateIP, op := range ops {
				if _, ok := eIPC.pendingCloudPrivateIPConfigsOps[egressIPName][cloudPrivateIP]; !ok {
					// If this specific EgressIP object's CloudPrivateIPConfig address currently has no
					// op, simply set it.
					eIPC.pendingCloudPrivateIPConfigsOps[egressIPName][cloudPrivateIP] = op
				} else {
					// If an existing operation for this CloudPrivateIP exists, then the following logic should
					// apply:
					// If toDelete is currently set: keep the current toDelete. Theoretically, the oldest toDelete
					// is the good one. If toDelete if currently not set, overwrite it with the new value.
					// If toAdd is currently set: overwrite with the new toAdd. Theoretically, the newest toAdd is
					// the good one.
					// Therefore, only replace toAdd over a previously existing op and only replace toDelete if
					// it's unset.
					if op.toAdd != "" {
						eIPC.pendingCloudPrivateIPConfigsOps[egressIPName][cloudPrivateIP].toAdd = op.toAdd
					}
					if eIPC.pendingCloudPrivateIPConfigsOps[egressIPName][cloudPrivateIP].toDelete == "" {
						eIPC.pendingCloudPrivateIPConfigsOps[egressIPName][cloudPrivateIP].toDelete = op.toDelete
					}
				}
			}
		}
	}
	return eIPC.executeCloudPrivateIPConfigOps(egressIPName, ops)
}

type egressIPClusterController struct {
	recorder record.EventRecorder
	stopChan chan struct{}
	wg       *sync.WaitGroup
	kube     *kube.KubeOVN
	// egressIPAssignmentMutex is used to ensure a safe updates between
	// concurrent go-routines which could be modifying the egress IP status
	// assignment simultaneously. Currently WatchEgressNodes and WatchEgressIP
	// run two separate go-routines which do this.
	egressIPAssignmentMutex *sync.Mutex
	// pendingCloudPrivateIPConfigsMutex is used to ensure synchronized access
	// to pendingCloudPrivateIPConfigsOps which is accessed by the egress IP and
	// cloudPrivateIPConfig go-routines
	pendingCloudPrivateIPConfigsMutex *sync.Mutex
	// pendingCloudPrivateIPConfigsOps is a cache of pending
	// CloudPrivateIPConfig changes that we are waiting on an answer for. Items
	// in this map are only ever removed once the op is fully finished and we've
	// been notified of this. That means:
	// - On add operations we only delete once we've seen that the
	// CloudPrivateIPConfig is fully added.
	// - On delete: when it's fully deleted.
	// - On update: once we finish processing the add - which comes after the
	// delete.
	pendingCloudPrivateIPConfigsOps map[string]map[string]*cloudPrivateIPConfigOp
	// nodeAllocator is a cache of egress IP centric data needed to when both route
	// health-checking and tracking allocations made
	nodeAllocator nodeAllocator
	markAllocator id.Allocator
	// watchFactory watching k8s objects
	watchFactory *factory.WatchFactory
	// EgressIP Node reachability total timeout configuration
	egressIPTotalTimeout int
	// reachability check interval
	reachabilityCheckInterval time.Duration
	// EgressIP Node reachability gRPC port (0 means it should use dial instead)
	egressIPNodeHealthCheckPort int
	// retry framework for Egress nodes
	retryEgressNodes *objretry.RetryFramework
	// retry framework for egress IP
	retryEgressIPs *objretry.RetryFramework
	// retry framework for Cloud private IP config
	retryCloudPrivateIPConfig *objretry.RetryFramework
	// egressNodes events factory handler
	egressNodeHandler *factory.Handler
	// egressIP events factory handler
	egressIPHandler *factory.Handler
	// cloudPrivateIPConfig events factory handler
	cloudPrivateIPConfigHandler *factory.Handler
}

func newEgressIPController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory, recorder record.EventRecorder) *egressIPClusterController {
	kube := &kube.KubeOVN{
		Kube:               kube.Kube{KClient: ovnClient.KubeClient},
		EIPClient:          ovnClient.EgressIPClient,
		CloudNetworkClient: ovnClient.CloudNetworkClient,
	}
	markAllocator := getEgressIPMarkAllocator()

	wg := &sync.WaitGroup{}
	eIPC := &egressIPClusterController{
		kube:                              kube,
		wg:                                wg,
		egressIPAssignmentMutex:           &sync.Mutex{},
		pendingCloudPrivateIPConfigsMutex: &sync.Mutex{},
		pendingCloudPrivateIPConfigsOps:   make(map[string]map[string]*cloudPrivateIPConfigOp),
		nodeAllocator:                     nodeAllocator{&sync.Mutex{}, make(map[string]*egressNode)},
		markAllocator:                     markAllocator,
		watchFactory:                      wf,
		recorder:                          recorder,
		egressIPTotalTimeout:              config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout,
		reachabilityCheckInterval:         egressIPReachabilityCheckInterval,
		egressIPNodeHealthCheckPort:       config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort,
		stopChan:                          make(chan struct{}),
	}
	eIPC.initRetryFramework()
	return eIPC
}

func (eIPC *egressIPClusterController) initRetryFramework() {
	eIPC.retryEgressNodes = eIPC.newRetryFramework(factory.EgressNodeType)
	eIPC.retryEgressIPs = eIPC.newRetryFramework(factory.EgressIPType)
	if util.PlatformTypeIsEgressIPCloudProvider() {
		eIPC.retryCloudPrivateIPConfig = eIPC.newRetryFramework(factory.CloudPrivateIPConfigType)
	}
}

func (eIPC *egressIPClusterController) newRetryFramework(objectType reflect.Type) *objretry.RetryFramework {
	eventHandler := &egressIPClusterControllerEventHandler{
		objType:  objectType,
		eIPC:     eIPC,
		syncFunc: nil,
	}
	resourceHandler := &objretry.ResourceHandler{
		HasUpdateFunc:          true, // all egressIP types have update func
		NeedsUpdateDuringRetry: true, // true for all egressIP types
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	return objretry.NewRetryFramework(eIPC.stopChan, eIPC.wg, eIPC.watchFactory, resourceHandler)
}

func (eIPC *egressIPClusterController) Start() error {
	var err error
	// In cluster manager, we only need to watch for egressNodes, egressIPs
	// and cloudPrivateIPConfig
	if eIPC.egressNodeHandler, err = eIPC.WatchEgressNodes(); err != nil {
		return fmt.Errorf("unable to watch egress nodes %w", err)
	}
	if eIPC.egressIPHandler, err = eIPC.WatchEgressIP(); err != nil {
		return err
	}
	if util.PlatformTypeIsEgressIPCloudProvider() {
		if eIPC.cloudPrivateIPConfigHandler, err = eIPC.WatchCloudPrivateIPConfig(); err != nil {
			return err
		}
	}
	if config.OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout == 0 {
		klog.V(2).Infof("EgressIP node reachability check disabled")
	} else if config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort != 0 {
		klog.Infof("EgressIP node reachability enabled and using gRPC port %d",
			config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort)
	}
	return nil
}

// WatchEgressNodes starts the watching of egress assignable nodes and calls
// back the appropriate handler logic.
func (eIPC *egressIPClusterController) WatchEgressNodes() (*factory.Handler, error) {
	return eIPC.retryEgressNodes.WatchResource()
}

// WatchCloudPrivateIPConfig starts the watching of cloudprivateipconfigs
// resource and calls back the appropriate handler logic.
func (eIPC *egressIPClusterController) WatchCloudPrivateIPConfig() (*factory.Handler, error) {
	return eIPC.retryCloudPrivateIPConfig.WatchResource()
}

// WatchEgressIP starts the watching of egressip resource and calls back the
// appropriate handler logic. It also initiates the other dedicated resource
// handlers for egress IP setup: namespaces, pods.
func (eIPC *egressIPClusterController) WatchEgressIP() (*factory.Handler, error) {
	return eIPC.retryEgressIPs.WatchResource()
}

func (eIPC *egressIPClusterController) Stop() {
	close(eIPC.stopChan)
	eIPC.wg.Wait()
	if eIPC.egressNodeHandler != nil {
		eIPC.watchFactory.RemoveNodeHandler(eIPC.egressNodeHandler)
	}
	if eIPC.egressIPHandler != nil {
		eIPC.watchFactory.RemoveEgressIPHandler(eIPC.egressIPHandler)
	}
	if eIPC.cloudPrivateIPConfigHandler != nil {
		eIPC.watchFactory.RemoveCloudPrivateIPConfigHandler(eIPC.cloudPrivateIPConfigHandler)
	}
}

type egressIPNodeStatus struct {
	Node string
	Name string
}

// getSortedEgressData returns a sorted slice of all egressNodes based on the
// amount of allocations found in the cache
func (eIPC *egressIPClusterController) getSortedEgressData() ([]*egressNode, map[string]egressIPNodeStatus) {
	assignableNodes := []*egressNode{}
	allAllocations := make(map[string]egressIPNodeStatus)
	for _, eNode := range eIPC.nodeAllocator.cache {
		if eNode.isEgressAssignable && eNode.isReady && eNode.isReachable {
			assignableNodes = append(assignableNodes, eNode)
		}
		for ip, eipName := range eNode.allocations {
			allAllocations[ip] = egressIPNodeStatus{Node: eNode.name, Name: eipName}
		}
	}
	sort.Slice(assignableNodes, func(i, j int) bool {
		return len(assignableNodes[i].allocations) < len(assignableNodes[j].allocations)
	})
	return assignableNodes, allAllocations
}

func (eIPC *egressIPClusterController) initEgressNodeReachability(nodes []interface{}) error {
	go eIPC.checkEgressNodesReachability()
	return nil
}

func (eIPC *egressIPClusterController) setNodeEgressAssignable(nodeName string, isAssignable bool) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	if eNode, exists := eIPC.nodeAllocator.cache[nodeName]; exists {
		eNode.isEgressAssignable = isAssignable
		// if the node is not assignable/ready/reachable anymore we need to
		// empty all of it's allocations from our cache since we'll clear all
		// assignments from this node later on, because of this.
		if !isAssignable {
			eNode.allocations = make(map[string]string)
		}
	}
}

func (eIPC *egressIPClusterController) isEgressNodeReady(egressNode *v1.Node) bool {
	for _, condition := range egressNode.Status.Conditions {
		if condition.Type == v1.NodeReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
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

// checkEgressNodesReachability continuously checks if all nodes used for egress
// IP assignment are reachable, and updates the nodes following the result. This
// is important because egress IP is based upon routing traffic to these nodes,
// and if they aren't reachable we shouldn't be using them for egress IP.
func (eIPC *egressIPClusterController) checkEgressNodesReachability() {
	timer := time.NewTicker(eIPC.reachabilityCheckInterval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			checkEgressNodesReachabilityIterate(eIPC)
		case <-eIPC.stopChan:
			klog.V(5).Infof("Stop channel got triggered: will stop checkEgressNodesReachability")
			return
		}
	}
}

func checkEgressNodesReachabilityIterate(eIPC *egressIPClusterController) {
	reAddOrDelete := map[string]bool{}
	eIPC.nodeAllocator.Lock()
	for _, eNode := range eIPC.nodeAllocator.cache {
		if eNode.isEgressAssignable && eNode.isReady {
			wasReachable := eNode.isReachable
			isReachable := eIPC.isReachable(eNode.name, eNode.mgmtIPs, eNode.healthClient)
			if wasReachable && !isReachable {
				reAddOrDelete[eNode.name] = true
			} else if !wasReachable && isReachable {
				reAddOrDelete[eNode.name] = false
			}
			eNode.isReachable = isReachable
		} else {
			// End connection (if there is one). This is important because
			// it accounts for cases where node is not labelled with
			// egress-assignable, so connection is no longer needed. Calling
			// this on a already disconnected node is expected to be cheap.
			eNode.healthClient.Disconnect()
		}
	}
	eIPC.nodeAllocator.Unlock()
	for nodeName, shouldDelete := range reAddOrDelete {
		if shouldDelete {
			metrics.RecordEgressIPUnreachableNode()
			klog.Warningf("Node: %s is detected as unreachable, deleting it from egress assignment", nodeName)
			if err := eIPC.deleteEgressNode(nodeName); err != nil {
				klog.Errorf("Node: %s is detected as unreachable, but could not re-assign egress IPs, err: %v", nodeName, err)
			}
		} else {
			klog.Infof("Node: %s is detected as reachable and ready again, adding it to egress assignment", nodeName)
			nodeToAdd, err := eIPC.watchFactory.GetNode(nodeName)
			if err != nil {
				klog.Errorf("Node: %s is detected as reachable and ready again, but could not re-assign egress IPs, err: %v", nodeName, err)
			} else if err := eIPC.retryEgressNodes.AddRetryObjWithAddNoBackoff(nodeToAdd); err != nil {
				klog.Errorf("Node: %s is detected as reachable and ready again, but could not re-assign egress IPs, err: %v", nodeName, err)
			}
		}
	}
}

func (eIPC *egressIPClusterController) isReachable(nodeName string, mgmtIPs []net.IP, healthClient healthcheck.EgressIPHealthClient) bool {
	// Check if we need to do node reachability check
	if eIPC.egressIPTotalTimeout == 0 {
		return true
	}

	if eIPC.egressIPNodeHealthCheckPort == 0 {
		return isReachableLegacy(nodeName, mgmtIPs, eIPC.egressIPTotalTimeout)
	}
	return isReachableViaGRPC(mgmtIPs, healthClient, eIPC.egressIPNodeHealthCheckPort, eIPC.egressIPTotalTimeout)
}

func (eIPC *egressIPClusterController) isEgressNodeReachable(egressNode *v1.Node) bool {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	if eNode, exists := eIPC.nodeAllocator.cache[egressNode.Name]; exists {
		return eNode.isReachable || eIPC.isReachable(eNode.name, eNode.mgmtIPs, eNode.healthClient)
	}
	return false
}

func (eIPC *egressIPClusterController) setNodeEgressReady(nodeName string, isReady bool) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	if eNode, exists := eIPC.nodeAllocator.cache[nodeName]; exists {
		eNode.isReady = isReady
		// see setNodeEgressAssignable
		if !isReady {
			eNode.allocations = make(map[string]string)
		}
	}
}

func (eIPC *egressIPClusterController) setNodeEgressReachable(nodeName string, isReachable bool) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	if eNode, exists := eIPC.nodeAllocator.cache[nodeName]; exists {
		eNode.isReachable = isReachable
		// see setNodeEgressAssignable
		if !isReachable {
			eNode.allocations = make(map[string]string)
		}
	}
}

// reconcileSecondaryHostNetworkEIPs is used to reconsider existing assigned EIPs that are assigned to secondary host
// networks and will send a 'synthetic' reconcile for any EIPs which are hosted by an invalid network which is determined
// from the nodes host-cidrs annotation
func (eIPC *egressIPClusterController) reconcileSecondaryHostNetworkEIPs(node *v1.Node) error {
	var errorAggregate []error
	egressIPs, err := eIPC.kube.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	reconcileEgressIPs := make([]*egressipv1.EgressIP, 0, len(egressIPs))
	eIPC.nodeAllocator.Lock()
	for _, egressIP := range egressIPs {
		egressIP := *egressIP
		for _, status := range egressIP.Status.Items {
			if status.Node == node.Name {
				egressIPIP := net.ParseIP(status.EgressIP)
				if egressIPIP == nil {
					return fmt.Errorf("unexpected empty egress IP found in status for egressIP %s", egressIP.Name)
				}
				eNode, exists := eIPC.nodeAllocator.cache[status.Node]
				if !exists {
					reconcileEgressIPs = append(reconcileEgressIPs, egressIP.DeepCopy())
					continue
				}
				// do not reconcile if EIP is hosted by OVN network or if network is what we expect
				if util.IsOVNNetwork(eNode.egressIPConfig, egressIPIP) {
					continue
				}
				network, err := util.GetSecondaryHostNetworkContainingIP(node, egressIPIP)
				if err != nil {
					errorAggregate = append(errorAggregate, fmt.Errorf("failed to determine if egress IP %s IP %s "+
						"is hosted by secondary host network for node %s: %w", egressIP.Name, egressIPIP.String(), node.Name, err))
					continue
				}
				if network == "" {
					reconcileEgressIPs = append(reconcileEgressIPs, egressIP.DeepCopy())
				}
			}
		}
	}
	eIPC.nodeAllocator.Unlock()
	for _, egressIP := range reconcileEgressIPs {
		if err := eIPC.reconcileEgressIP(nil, egressIP); err != nil {
			errorAggregate = append(errorAggregate, fmt.Errorf("re-assignment for EgressIP %s hosted by a "+
				"secondary host network failed, unable to update object, err: %v", egressIP.Name, err))
		}
	}
	if len(errorAggregate) > 0 {
		return utilerrors.Join(errorAggregate...)
	}
	return nil
}

func (eIPC *egressIPClusterController) addEgressNode(nodeName string) error {
	var errors []error
	klog.V(5).Infof("Egress node: %s about to be initialized", nodeName)

	// If a node has been labelled for egress IP we need to check if there are any
	// egress IPs which are missing an assignment. If there are, we need to send a
	// synthetic update since reconcileEgressIP will then try to assign those IPs to
	// this node (if possible)
	egressIPs, err := eIPC.kube.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	for _, egressIP := range egressIPs {
		egressIP := *egressIP
		if len(egressIP.Spec.EgressIPs) != len(egressIP.Status.Items) {
			// Send a "synthetic update" on all egress IPs which are not fully
			// assigned, the reconciliation loop for WatchEgressIP will try to
			// assign stuff to this new node. The workqueue's delta FIFO
			// implementation will not trigger a watch event for updates on
			// objects which have no semantic difference, hence: call the
			// reconciliation function directly.
			if err := eIPC.reconcileEgressIP(nil, &egressIP); err != nil {
				errors = append(errors, fmt.Errorf("synthetic update for EgressIP: %s failed, err: %v", egressIP.Name, err))
			}
		}
	}

	if len(errors) > 0 {
		return utilerrors.Join(errors...)
	}
	return nil
}

// deleteNodeForEgress remove the default allow logical router policies for the
// node and removes the node from the allocator cache.
func (eIPC *egressIPClusterController) deleteNodeForEgress(node *v1.Node) {
	eIPC.nodeAllocator.Lock()
	if eNode, exists := eIPC.nodeAllocator.cache[node.Name]; exists {
		eNode.healthClient.Disconnect()
	}
	delete(eIPC.nodeAllocator.cache, node.Name)
	eIPC.nodeAllocator.Unlock()
}

func (eIPC *egressIPClusterController) deleteEgressNode(nodeName string) error {
	var errorAggregate []error
	klog.V(5).Infof("Egress node: %s about to be removed", nodeName)
	// Since the node has been labelled as "not usable" for egress IP
	// assignments we need to find all egress IPs which have an assignment to
	// it, and move them elsewhere.
	egressIPs, err := eIPC.kube.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	for _, egressIP := range egressIPs {
		egressIP := *egressIP
		for _, status := range egressIP.Status.Items {
			if status.Node == nodeName {
				// Send a "synthetic update" on all egress IPs which have an
				// assignment to this node. The reconciliation loop for
				// WatchEgressIP will see that the current assignment status to
				// this node is invalid and try to re-assign elsewhere. The
				// workqueue's delta FIFO implementation will not trigger a
				// watch event for updates on objects which have no semantic
				// difference, hence: call the reconciliation function directly.
				if err := eIPC.reconcileEgressIP(nil, &egressIP); err != nil {
					errorAggregate = append(errorAggregate, fmt.Errorf("re-assignment for EgressIP: %s failed, unable to update object, err: %v", egressIP.Name, err))
				}
				break
			}
		}
	}
	if len(errorAggregate) > 0 {
		return utilerrors.Join(errorAggregate...)
	}
	return nil
}

func (eIPC *egressIPClusterController) initEgressIPAllocator(node *v1.Node) (err error) {
	parsedEgressIPConfig, err := util.GetNodeEIPConfig(node)
	if err != nil {
		return fmt.Errorf("failed to get egress IP config for node %s: %w", node.Name, err)
	}
	nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
	if err != nil {
		return fmt.Errorf("failed to parse node %s subnets annotation %v", node.Name, err)
	}
	mgmtIPs := make([]net.IP, len(nodeSubnets))
	for i, subnet := range nodeSubnets {
		mgmtIPs[i] = util.GetNodeManagementIfAddr(subnet).IP
	}
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	if eNode, exists := eIPC.nodeAllocator.cache[node.Name]; !exists {
		eIPC.nodeAllocator.cache[node.Name] = &egressNode{
			name:           node.Name,
			egressIPConfig: parsedEgressIPConfig,
			mgmtIPs:        mgmtIPs,
			allocations:    make(map[string]string),
			healthClient:   hccAllocator.allocate(node.Name),
		}
	} else {
		eNode.egressIPConfig = parsedEgressIPConfig
		eNode.mgmtIPs = mgmtIPs
	}
	return nil
}

// deleteAllocatorEgressIPAssignments deletes the allocation as to keep the
// cache state correct, also see addAllocatorEgressIPAssignments
func (eIPC *egressIPClusterController) deleteAllocatorEgressIPAssignments(statusAssignments []egressipv1.EgressIPStatusItem) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	for _, status := range statusAssignments {
		if eNode, exists := eIPC.nodeAllocator.cache[status.Node]; exists {
			delete(eNode.allocations, status.EgressIP)
		}
	}
}

// deleteAllocatorEgressIPAssignmentIfExists deletes egressIP config from node allocations map
// if the entry is available and returns assigned node name, otherwise returns empty string.
func (eIPC *egressIPClusterController) deleteAllocatorEgressIPAssignmentIfExists(name, egressIP string) string {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	for nodeName, eNode := range eIPC.nodeAllocator.cache {
		if egressIPName, exists := eNode.allocations[egressIP]; exists && egressIPName == name {
			delete(eNode.allocations, egressIP)
			return nodeName
		}
	}
	return ""
}

// addAllocatorEgressIPAssignments adds the allocation to the cache, so that
// they are tracked during the life-cycle of ovnkube-master
func (eIPC *egressIPClusterController) addAllocatorEgressIPAssignments(name string, statusAssignments []egressipv1.EgressIPStatusItem) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	for _, status := range statusAssignments {
		if eNode, exists := eIPC.nodeAllocator.cache[status.Node]; exists {
			eNode.allocations[status.EgressIP] = name
		}
	}
}

func (eIPC *egressIPClusterController) reconcileEgressIP(old, new *egressipv1.EgressIP) (err error) {
	// Lock the assignment, this is needed because this function can end up
	// being called from WatchEgressNodes and WatchEgressIP, i.e: two different
	// go-routines and we need to make sure the assignment is safe.
	eIPC.egressIPAssignmentMutex.Lock()
	defer eIPC.egressIPAssignmentMutex.Unlock()

	name := ""

	// Initialize a status which will be used to compare against
	// new.spec.egressIPs and decide on what from the status should get deleted
	// or kept.
	status := []egressipv1.EgressIPStatusItem{}

	// Initialize an empty objects as to avoid SIGSEGV. The code should play
	// nicely with empty objects though.
	newEIP := &egressipv1.EgressIP{}

	// Initialize a sets.String which holds egress IPs that were not fully assigned
	// but are allocated and they are meant to be removed.
	staleEgressIPs := sets.NewString()
	if old != nil {
		name = old.Name
		status = old.Status.Items
		staleEgressIPs.Insert(old.Spec.EgressIPs...)
	}
	if new != nil {
		newEIP = new
		name = newEIP.Name
		status = newEIP.Status.Items
		if staleEgressIPs.Len() > 0 {
			for _, egressIP := range newEIP.Spec.EgressIPs {
				if staleEgressIPs.Has(egressIP) {
					staleEgressIPs.Delete(egressIP)
				}
			}
		}
	} else {
		eIPC.deallocMark(name)
	}

	// Validate the spec and use only the valid egress IPs when performing any
	// successive operations, theoretically: the user could specify invalid IP
	// addresses, which would break us.
	validSpecIPs, err := eIPC.validateEgressIPSpec(name, newEIP.Spec.EgressIPs)
	if err != nil {
		return fmt.Errorf("invalid EgressIP spec, err: %v", err)
	}

	// Validate the status, on restart it could be the case that what might have
	// been assigned when ovnkube-master last ran is not a valid assignment
	// anymore (specifically if ovnkube-master has been crashing for a while).
	// Any invalid status at this point in time needs to be removed and assigned
	// to a valid node.
	validStatus, invalidStatus := eIPC.validateEgressIPStatus(name, status)
	for status := range validStatus {
		// If the spec has changed and an egress IP has been removed by the
		// user: we need to un-assign that egress IP
		if !validSpecIPs.Has(status.EgressIP) {
			invalidStatus[status] = ""
			delete(validStatus, status)
		}
	}

	invalidStatusLen := len(invalidStatus)
	if invalidStatusLen > 0 {
		metrics.RecordEgressIPRebalance(invalidStatusLen)
	}

	// Add only the diff between what is requested and valid and that which
	// isn't already assigned.
	ipsToAssign := validSpecIPs
	ipsToRemove := sets.New[string]()
	statusToAdd := make([]egressipv1.EgressIPStatusItem, 0, len(ipsToAssign))
	statusToKeep := make([]egressipv1.EgressIPStatusItem, 0, len(validStatus))
	for status := range validStatus {
		statusToKeep = append(statusToKeep, status)
		ipsToAssign.Delete(status.EgressIP)
	}
	statusToRemove := make([]egressipv1.EgressIPStatusItem, 0, invalidStatusLen)
	for status := range invalidStatus {
		statusToRemove = append(statusToRemove, status)
		ipsToRemove.Insert(status.EgressIP)
	}
	// Adding the mark to annotations is bundled with status update in-order to minimise updates, cover the case where there is no update to status
	// and mark annotation has been modified / removed. This should only occur for an update and the mark was previous set.
	if ipsToAssign.Len() == 0 && ipsToRemove.Len() == 0 {
		eIPC.ensureMark(old, new)
	}

	if ipsToRemove.Len() > 0 {
		// The following is added as to ensure that we only add after having
		// successfully removed egress IPs. This case is not very important on
		// bare-metal (since we execute the add after the remove below, and
		// hence have full control of the execution - barring its success), but
		// on a cloud: we patch all validStatsuses below, we wait for the status
		// on the CloudPrivateIPConfig(s) we create to be set before executing
		// anything in the OVN DB (Note that the status will be set by this
		// controller in cluster-manager and asynchronously the ovnkube-master
		// will read the CRD change and do the necessary plumbing (ADD/UPDATE/DELETE)
		// in the OVN DB).
		// So, we need to make sure that we delete and
		// then add, mainly because if EIP1 is added to nodeX and then EIP2 is
		// removed from nodeX, we might remove the setup made for EIP1. The
		// add/delete ordering of events is not guaranteed on the cloud where we
		// depend on other controllers to execute the work for us however. By
		// comparing the spec to the status and applying the following truth
		// table we can ensure that order of events.

		// case ID    |    Egress IP to add    |    Egress IP to remove    |    ipsToAssign
		// 1          |    e1                  |    e1                     |    e1
		// 2          |    e2                  |    e1                     |    -
		// 3          |    e2                  |    -                      |    e2
		// 4          |    -                   |    e1                     |    -

		// Case 1 handles updates. Case 2 and 3 makes sure we don't add until we
		// successfully delete. Case 4 just shows an example of what would
		// happen if we don't have anything to add
		ipsToAssign = ipsToAssign.Intersection(ipsToRemove)
	}

	if !util.PlatformTypeIsEgressIPCloudProvider() {
		if len(statusToRemove) > 0 {
			// Delete the statusToRemove from the allocator cache. If we don't
			// do this we will occupy assignment positions for the ipsToAssign,
			// even though statusToRemove will be removed afterwards
			eIPC.deleteAllocatorEgressIPAssignments(statusToRemove)
		}
		if len(ipsToAssign) > 0 {
			statusToAdd = eIPC.assignEgressIPs(name, ipsToAssign.UnsortedList())
			statusToKeep = append(statusToKeep, statusToAdd...)
		}
		// Add all assignments which are to be kept to the allocator cache,
		// allowing us to track all assignments which have been performed and
		// avoid incorrect future assignments due to a de-synchronized cache.
		eIPC.addAllocatorEgressIPAssignments(name, statusToKeep)
		// Update the object only on an ADD/UPDATE. If we are processing a
		// DELETE, new will be nil and we should not update the object.
		if len(statusToAdd) > 0 || (len(statusToRemove) > 0 && new != nil) {
			if err := eIPC.patchEgressIP(name, eIPC.generateEgressIPPatches(name, new.Annotations, statusToKeep)...); err != nil {
				return err
			}
		}
	} else {
		// Even when running on a public cloud, we must make sure that we unwire EgressIP
		// configuration from OVN *before* we instruct the CloudNetworkConfigController
		// to remove the CloudPrivateIPConfig object from the cloud.
		// CloudPrivateIPConfig objects can be in the "Deleting" state for a long time,
		// waiting for the underlying cloud to finish its action and to report success of the
		// unattach operation. Some clouds such as Azure will remove the IP address nearly
		// immediately, but then they will take a long time (seconds to minutes) to actually report
		// success of the removal operation.
		if len(statusToRemove) > 0 {
			// Delete all assignments that are to be removed from the allocator
			// cache. If we don't do this we will occupy assignment positions for
			// the ipsToAdd, even though statusToRemove will be removed afterwards
			eIPC.deleteAllocatorEgressIPAssignments(statusToRemove)
			// Before updating the cloud private IP object, we need to remove the OVN configuration
			// for these invalid statuses so that traffic is not blackholed to non-existing setup in the
			// cloud. Thus we patch the egressIP status with the valid set of statuses which will
			// trigger an event for the ovnkube-master to take action upon.
			// Note that once we figure out the statusToAdd parts below we will trigger an
			// update to cloudPrivateIP object which will trigger another patch for the eIP object.
			//
			// Update the object only on an ADD/UPDATE. If we are processing a
			// DELETE, new will be nil and we should not update the object.
			if new != nil {
				if err := eIPC.patchEgressIP(name, eIPC.generateEgressIPPatches(name, new.Annotations, statusToKeep)...); err != nil {
					return err
				}
			}
		}
		// When egress IP is not fully assigned to a node, then statusToRemove may not
		// have those entries, hence retrieve it from staleEgressIPs for removing
		// the item from cloudprivateipconfig.
		for _, toRemove := range statusToRemove {
			if !staleEgressIPs.Has(toRemove.EgressIP) {
				continue
			}
			staleEgressIPs.Delete(toRemove.EgressIP)
		}
		for staleEgressIP := range staleEgressIPs {
			if nodeName := eIPC.deleteAllocatorEgressIPAssignmentIfExists(name, staleEgressIP); nodeName != "" {
				statusToRemove = append(statusToRemove,
					egressipv1.EgressIPStatusItem{EgressIP: staleEgressIP, Node: nodeName})
			}
		}
		// If running on a public cloud we should not program OVN just yet for assignment
		// operations. We need confirmation from the cloud-network-config-controller that
		// it can assign the IPs. reconcileCloudPrivateIPConfig will take care of
		// processing the answer from the requests we make here, and update OVN
		// accordingly when we know what the outcome is.
		if len(ipsToAssign) > 0 {
			statusToAdd = eIPC.assignEgressIPs(name, ipsToAssign.UnsortedList())
			statusToKeep = append(statusToKeep, statusToAdd...)
		}
		// Same as above: Add all assignments which are to be kept to the
		// allocator cache, allowing us to track all assignments which have been
		// performed and avoid incorrect future assignments due to a
		// de-synchronized cache.
		eIPC.addAllocatorEgressIPAssignments(name, statusToKeep)

		// Execute CloudPrivateIPConfig changes for assignments which need to be
		// added/removed, assignments which don't change do not require any
		// further setup.
		if err := eIPC.executeCloudPrivateIPConfigChange(name, statusToAdd, statusToRemove); err != nil {
			return err
		}
	}

	// Record the egress IP allocator count
	metrics.RecordEgressIPCount(eIPC.getAllocationTotalCount())
	return nil
}

// syncCloudPrivateIPConfigs This method takes care syncing stale data in the
// egress ip status with cloud private ip config upon master reboot cases.
// cloud private ip config entry would have been deleted when master was down
// whereas egress ip status was not updated for the deleted entry in an error
// scenario. Hence this method ensures egress ip status is upto date with
// available cloud private ip config entry.
func (eIPC *egressIPClusterController) syncCloudPrivateIPConfigs(objs []interface{}) error {
	if !util.PlatformTypeIsEgressIPCloudProvider() {
		return nil
	}
	cloudPrivateIPConfigMap, err := eIPC.getCloudPrivateIPConfigMap(objs)
	if err != nil {
		return fmt.Errorf("syncCloudPrivateIPConfigs unable to get cloud private ip config: %w", err)
	}
	egressIPs, err := eIPC.watchFactory.GetEgressIPs()
	if err != nil {
		return fmt.Errorf("syncCloudPrivateIPConfigs unable to get Egress IPs: %w", err)
	}
	for _, egressIP := range egressIPs {
		updatedStatus := []egressipv1.EgressIPStatusItem{}
		cloudPrivateIPNotFound := false
		for _, status := range egressIP.Status.Items {
			cloudPrivateIPConfigName := ipStringToCloudPrivateIPConfigName(status.EgressIP)
			if _, exists := cloudPrivateIPConfigMap[cloudPrivateIPConfigName]; exists {
				updatedStatus = append(updatedStatus, status)
			} else {
				// Set cloudPrivateIPNotFoundOrInvalid flag to true because egress ip entry not found in
				// cloud private ip config object. Note that the egress ip status might still reflect an
				// old node assignment if the informer cache has old data, so do not invalidate if it is
				// not the same node as the cloud private ip config assignment.
				cloudPrivateIPNotFound = true
			}
		}
		if cloudPrivateIPNotFound {
			// There could be one or more stale entry found in egress ip object, remove it by patching egressip
			// object with updated status.
			err = eIPC.patchEgressIP(egressIP.Name, eIPC.generateEgressIPPatches(egressIP.Name, egressIP.Annotations, updatedStatus)...)
			if err != nil {
				return fmt.Errorf("syncCloudPrivateIPConfigs unable to update EgressIP status: %w", err)
			}
		}
	}
	return nil
}

// getCloudPrivateIPConfigMap returns cloud private ip config map cotaining ip address the key and
// assigned node node name as the value. This method is intended to be invoked only in the case of
// cloud environment.
func (eIPC *egressIPClusterController) getCloudPrivateIPConfigMap(objs []interface{}) (map[string]string, error) {
	cloudPrivateIPConfigMap := make(map[string]string)
	for _, obj := range objs {
		cloudPrivateIPConfig, ok := obj.(*ocpcloudnetworkapi.CloudPrivateIPConfig)
		if !ok {
			klog.Errorf("Could not cast %T object to *ocpcloudnetworkapi.CloudPrivateIPConfig", obj)
			continue
		}
		cloudPrivateIPConfigMap[cloudPrivateIPConfig.Name] = cloudPrivateIPConfig.Status.Node
	}
	return cloudPrivateIPConfigMap, nil
}

// assignEgressIPs is the main assignment algorithm for egress IPs to nodes.
// Specifically we have a couple of hard constraints: a) the subnet of the node
// must be able to host the egress IP b) the egress IP cannot be a node IP c)
// the IP cannot already be assigned and reference by another EgressIP object d)
// no two egress IPs for the same EgressIP object can be assigned to the same
// node e) (for public clouds) the amount of egress IPs assigned to one node
// must respect its assignment capacity. Moreover there is a soft constraint:
// the assignments need to be balanced across all cluster nodes, so that no node
// becomes a bottleneck. The balancing is achieved by sorting the nodes in
// ascending order following their existing amount of allocations, and trying to
// assign the egress IP to the node with the lowest amount of allocations every
// time, this does not guarantee complete balance, but mostly complete.
// For Egress IPs that are hosted by secondary host networks, there must be at least
// one node that hosts the network and exposed via the nodes host-cidrs annotation.
func (eIPC *egressIPClusterController) assignEgressIPs(name string, egressIPs []string) []egressipv1.EgressIPStatusItem {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	assignments := []egressipv1.EgressIPStatusItem{}
	assignableNodes, existingAllocations := eIPC.getSortedEgressData()
	if len(assignableNodes) == 0 {
		eIPRef := v1.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "NoMatchingNodeFound", "no assignable nodes for EgressIP: %s, please tag at least one node with label: %s", name, util.GetNodeEgressLabel())
		klog.Errorf("No assignable nodes found for EgressIP: %s and requested IPs: %v", name, egressIPs)
		return assignments
	}
	klog.V(5).Infof("Current assignments are: %+v", existingAllocations)
	for _, egressIP := range egressIPs {
		klog.V(5).Infof("Will attempt assignment for egress IP: %s", egressIP)
		eIP := net.ParseIP(egressIP)

		// verify the IP is not already assigned to any network interface. We can only see IPs with scope global within the
		// cluster, therefore there maybe still conflicts when we attempt to assign an egress IP with a different scope.
		if isIPConflict, conflictedHost, err := eIPC.isEgressIPAddrConflict(eIP); err != nil {
			klog.Errorf("Egress IP: %v failed to check if EgressIP already is assigned on any interface throughout the cluster: %v", eIP, err)
			return assignments
		} else if isIPConflict {
			eIPRef := v1.ObjectReference{
				Kind: "EgressIP",
				Name: name,
			}
			eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "EgressIPConflict", "Egress IP %s with IP "+
				"%v is conflicting with a host (%s) IP address and will not be assigned", name, eIP, conflictedHost)
			klog.Errorf("Egress IP: %v address is already assigned on an interface on node %s", eIP, conflictedHost)
			return assignments
		}
		if status, exists := existingAllocations[eIP.String()]; exists {
			// On public clouds we will re-process assignments for the same IP
			// multiple times due to the nature of syncing each individual
			// CloudPrivateIPConfig one at a time. This means that we are
			// expected to end up in this situation multiple times per sync. Ex:
			// Say we an EgressIP is created with IP1, IP2, IP3. We begin by
			// assigning them all the first round. Next we get the
			// CloudPrivateIPConfig confirming the addition of IP1, leading us
			// to re-assign IP2, IP3, but since we've already assigned them
			// we'll end up here. This is not an error. What would be an error
			// is if the user created EIP1 with IP1 and a second EIP2 with IP1
			if name == status.Name {
				node, err := eIPC.watchFactory.GetNode(status.Node)
				if err != nil {
					klog.Errorf("Failed to process existing egress IP %s allocation because node %s doesn't exist: %v",
						egressIP, status.Node, err)
					continue
				}
				eNode, exists := eIPC.nodeAllocator.cache[status.Node] // allocator lock was previously acquired
				if !exists {
					klog.Errorf("Failed to find entry in allocator cache for EgressIP %s and IP %s,", name, eIP.String())
					continue
				}
				eIPNetwork, err := util.GetEgressIPNetwork(node, eNode.egressIPConfig, eIP)
				if err != nil {
					klog.Errorf("Failed to determine EgressIP %s network using IP %s for node %s: %v", name, eIP.String(), node.Name, err)
					continue
				}
				if eIPNetwork == "" {
					klog.Errorf("EgressIP %s IP %s is allocated to node %s but node does not contain a network "+
						"that can host it", name, eIP.String(), node.Name)
					continue
				}
				// IP is already assigned for this EgressIP object
				assignments = append(assignments, egressipv1.EgressIPStatusItem{
					Node:     status.Node,
					EgressIP: eIP.String(),
				})
				continue
			} else {
				eIPC.recorder.Eventf(
					&v1.ObjectReference{
						Kind: "EgressIP",
						Name: name,
					},
					v1.EventTypeWarning,
					"UnsupportedRequest",
					"IP: %q for EgressIP: %s is already allocated for EgressIP: %s on %s", egressIP, name, status.Name, status.Node,
				)
				klog.Errorf("IP: %q for EgressIP: %s is already allocated for EgressIP: %s on %s", egressIP, name, status.Name, status.Node)
				return assignments
			}
		}
		// Egress IP for secondary host networks is only available on baremetal environments
		if !util.PlatformTypeIsEgressIPCloudProvider() {
			assignableNodesWithSecondaryNet := make([]*egressNode, 0)
			for _, eNode := range assignableNodes {
				node, err := eIPC.watchFactory.GetNode(eNode.name)
				if err != nil {
					klog.Warningf("Failed to determine if node %s may host EgressIP %s IP %s because unable to get node obj: %v",
						name, eNode.name, eIP.String(), err)
					continue
				}
				if util.IsOVNNetwork(eNode.egressIPConfig, eIP) {
					continue
				}
				network, err := util.GetSecondaryHostNetworkContainingIP(node, eIP)
				if err != nil {
					klog.Warningf("Failed to determine if egress IP %s is hosted by a secondary host network for node %s: %v",
						eIP.String(), node.Name, err)
					continue
				}
				if network == "" {
					continue
				}
				assignableNodesWithSecondaryNet = append(assignableNodesWithSecondaryNet, eNode)

			}
			// if the EIP is hosted by a secondary host network, then limit the assignable nodes to the set of nodes
			// that connect to this network.
			if len(assignableNodesWithSecondaryNet) > 0 {
				klog.V(5).Infof("Restricting the number of assignable nodes from %d to %d because EgressIP %s IP %s "+
					"is going to be hosted by a secondary host network", len(assignableNodes), len(assignableNodesWithSecondaryNet), name, eIP.String())
				assignableNodes = assignableNodesWithSecondaryNet
			}
		}

		var assignmentSuccessful bool
		for i := 0; i < len(assignableNodes) && !assignmentSuccessful; i++ {
			eNode := assignableNodes[i]
			klog.V(5).Infof("Attempting assignment on egress node: %+v", eNode)
			if eNode.getAllocationCountForEgressIP(name) > 0 {
				klog.V(5).Infof("Node: %s is already in use by another egress IP for this EgressIP: %s, trying another node", eNode.name, name)
				continue
			}
			node, err := eIPC.watchFactory.GetNode(eNode.name)
			if err != nil {
				klog.Errorf("Failed to consider node %s because lookup of kubernetes object failed: %v", eNode.name, err)
				continue
			}
			egressIPNetwork, err := util.GetEgressIPNetwork(node, eNode.egressIPConfig, eIP)
			if err != nil {
				klog.Errorf("Failed to consider node %s for EgressIP %s IP %s because unable to find a network to host it: %v",
					node.Name, name, eIP.String(), err)
				continue
			}
			if egressIPNetwork == "" {
				continue
			}
			if eNode.egressIPConfig.Capacity.IP < util.UnlimitedNodeCapacity {
				if eNode.egressIPConfig.Capacity.IP-len(eNode.allocations) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IP capacity, trying another node", eNode.name)
					continue
				}
			}
			if eNode.egressIPConfig.Capacity.IPv4 < util.UnlimitedNodeCapacity && utilnet.IsIPv4(eIP) {
				if eNode.egressIPConfig.Capacity.IPv4-getIPFamilyAllocationCount(eNode.allocations, false) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IPv4 capacity, trying another node", eNode.name)
					continue
				}
			}
			if eNode.egressIPConfig.Capacity.IPv6 < util.UnlimitedNodeCapacity && utilnet.IsIPv6(eIP) {
				if eNode.egressIPConfig.Capacity.IPv6-getIPFamilyAllocationCount(eNode.allocations, true) <= 0 {
					klog.V(5).Infof("Additional allocation on Node: %s exhausts it's IPv6 capacity, trying another node", eNode.name)
					continue
				}
			}
			assignments = append(assignments, egressipv1.EgressIPStatusItem{
				Node:     eNode.name,
				EgressIP: eIP.String(),
			})
			eNode.allocations[eIP.String()] = name
			assignmentSuccessful = true
			klog.Infof("Successful assignment of egress IP: %s to network %s on node: %+v", egressIP, egressIPNetwork, eNode)
			break
		}
	}
	if len(assignments) == 0 {
		eIPRef := v1.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "NoMatchingNodeFound", "No matching nodes found, which can host any of the egress IPs: %v for object EgressIP: %s", egressIPs, name)
		klog.Errorf("No matching host found for EgressIP: %s", name)
		return assignments
	}
	if len(assignments) < len(egressIPs) {
		eIPRef := v1.ObjectReference{
			Kind: "EgressIP",
			Name: name,
		}
		eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "UnassignedRequest", "Not all egress IPs for EgressIP: %s could be assigned, please tag more nodes", name)
	}
	return assignments
}

func getIPFamilyAllocationCount(allocations map[string]string, isIPv6 bool) (count int) {
	for allocation := range allocations {
		if utilnet.IsIPv4String(allocation) && !isIPv6 {
			count++
		}
		if utilnet.IsIPv6String(allocation) && isIPv6 {
			count++
		}
	}
	return
}

func (eIPC *egressIPClusterController) validateEgressIPSpec(name string, egressIPs []string) (sets.Set[string], error) {
	validatedEgressIPs := sets.New[string]()
	for _, egressIP := range egressIPs {
		ip := net.ParseIP(egressIP)
		if ip == nil {
			eIPRef := v1.ObjectReference{
				Kind: "EgressIP",
				Name: name,
			}
			eIPC.recorder.Eventf(&eIPRef, v1.EventTypeWarning, "InvalidEgressIP", "egress IP: %s for object EgressIP: %s is not a valid IP address", egressIP, name)
			return nil, fmt.Errorf("unable to parse provided EgressIP: %s, invalid", egressIP)
		}
		validatedEgressIPs.Insert(ip.String())
	}
	return validatedEgressIPs, nil
}

// isEgressIPAddrConflict iterates through all the nodes in the cluster and ensures that the IP specified by func parameter
// egressIP is not equal to any existing IP address
func (eIPC *egressIPClusterController) isEgressIPAddrConflict(egressIP net.IP) (bool, string, error) {
	nodes, err := eIPC.watchFactory.GetNodes()
	if err != nil {
		return false, "", fmt.Errorf("failed to get nodes: %v", err)
	}
	// iterate through the nodes and ensure no host IP address conflicts with EIP. Note that host-cidrs annotation
	// does not contain EgressIPs that are assigned to interfaces.
	for _, node := range nodes {
		// EgressIP is not supported on hybrid overlay nodes, and OVNNodeHostCIDRs annotation is not present
		if util.NoHostSubnet(node) {
			continue
		}
		nodeHostAddrsSet, err := util.ParseNodeHostCIDRsDropNetMask(node)
		if err != nil {
			return false, "", fmt.Errorf("failed to parse node host cidrs for node %s: %v", node.Name, err)
		}
		if nodeHostAddrsSet.Has(egressIP.String()) {
			return true, node.Name, nil
		}
	}
	return false, "", nil
}

// validateEgressIPStatus validates if the statuses are valid given what the
// cache knows about all egress nodes. WatchEgressNodes is initialized before
// any other egress IP handler, so the cache should be warm and correct once we
// start going this.
func (eIPC *egressIPClusterController) validateEgressIPStatus(name string, items []egressipv1.EgressIPStatusItem) (map[egressipv1.EgressIPStatusItem]string, map[egressipv1.EgressIPStatusItem]string) {
	eIPC.nodeAllocator.Lock()
	defer eIPC.nodeAllocator.Unlock()
	valid, invalid := make(map[egressipv1.EgressIPStatusItem]string), make(map[egressipv1.EgressIPStatusItem]string)
	for _, eIPStatus := range items {
		validAssignment := true
		eNode, exists := eIPC.nodeAllocator.cache[eIPStatus.Node]
		if !exists {
			klog.Errorf("Allocator error: EgressIP: %s claims to have an allocation on a node which is unassignable for egress IP: %s", name, eIPStatus.Node)
			validAssignment = false
		} else {
			if eNode.getAllocationCountForEgressIP(name) > 1 {
				klog.Errorf("Allocator error: EgressIP: %s claims multiple egress IPs on same node: %s, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isEgressAssignable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which does not have egress label, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isReachable {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not reachable, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			if !eNode.isReady {
				klog.Errorf("Allocator error: EgressIP: %s assigned to node: %s which is not ready, will attempt rebalancing", name, eIPStatus.Node)
				validAssignment = false
			}
			ip := net.ParseIP(eIPStatus.EgressIP)
			if ip == nil {
				klog.Errorf("Allocator error: EgressIP allocation contains unparsable IP address: %q", eIPStatus.EgressIP)
			}
			node, err := eIPC.watchFactory.GetNode(eNode.name)
			if err != nil {
				klog.Errorf("Allocator error: failed to validate and will not consider node %s for egress IP %s: %v",
					eNode.name, name, err)
			}
			isOVNNetwork := util.IsOVNNetwork(eNode.egressIPConfig, ip)
			isSecondaryHostNetwork, err := util.IsSecondaryHostNetworkContainingIP(node, ip)
			if err != nil {
				klog.Errorf("Allocator error: failed to determine if Egress IP %q is to be hosted by a secondary host "+
					"network for egress IP %s: %v", eIPStatus.EgressIP, name, err)
			}
			if !isOVNNetwork && !isSecondaryHostNetwork {
				klog.Errorf("Allocator error: failed to assign Egress IP %s IP %q", name, eIPStatus.EgressIP)
				validAssignment = false
			}
		}
		if validAssignment {
			valid[eIPStatus] = ""
		} else {
			invalid[eIPStatus] = ""
		}
	}
	return valid, invalid
}

func (eIPC *egressIPClusterController) reconcileCloudPrivateIPConfig(old, new *ocpcloudnetworkapi.CloudPrivateIPConfig) error {
	oldCloudPrivateIPConfig, newCloudPrivateIPConfig := &ocpcloudnetworkapi.CloudPrivateIPConfig{}, &ocpcloudnetworkapi.CloudPrivateIPConfig{}
	shouldDelete, shouldAdd := false, false
	nodeToDelete := ""

	if old != nil {
		oldCloudPrivateIPConfig = old
		// We need to handle three types of deletes, A) object UPDATE where the
		// old egress IP <-> node assignment has been removed. This is indicated
		// by the old object having a .status.node set and the new object having
		// .status.node empty and the condition on the new being successful. B)
		// object UPDATE where egress IP <-> node assignment has been updated.
		// This is indicated by .status.node being different on old and new
		// objects. C) object DELETE, for which new is nil
		shouldDelete = oldCloudPrivateIPConfig.Status.Node != "" || new == nil
		// On DELETE we need to delete the .spec.node for the old object
		nodeToDelete = oldCloudPrivateIPConfig.Spec.Node
	}
	if new != nil {
		newCloudPrivateIPConfig = new
		// We should only proceed to setting things up for objects where the new
		// object has the same .spec.node and .status.node, and assignment
		// condition being true. This is how the cloud-network-config-controller
		// indicates a successful cloud assignment.
		shouldAdd = newCloudPrivateIPConfig.Status.Node == newCloudPrivateIPConfig.Spec.Node &&
			ocpcloudnetworkapi.CloudPrivateIPConfigConditionType(newCloudPrivateIPConfig.Status.Conditions[0].Type) == ocpcloudnetworkapi.Assigned &&
			v1.ConditionStatus(newCloudPrivateIPConfig.Status.Conditions[0].Status) == v1.ConditionTrue
		// See above explanation for the delete
		shouldDelete = shouldDelete &&
			(newCloudPrivateIPConfig.Status.Node == "" || newCloudPrivateIPConfig.Status.Node != oldCloudPrivateIPConfig.Status.Node) &&
			ocpcloudnetworkapi.CloudPrivateIPConfigConditionType(newCloudPrivateIPConfig.Status.Conditions[0].Type) == ocpcloudnetworkapi.Assigned &&
			v1.ConditionStatus(newCloudPrivateIPConfig.Status.Conditions[0].Status) == v1.ConditionTrue
		// On UPDATE we need to delete the old .status.node
		if shouldDelete {
			nodeToDelete = oldCloudPrivateIPConfig.Status.Node
		}
	}

	// As opposed to reconcileEgressIP, here we are only interested in changes
	// made to the status (since we are the only ones performing the change made
	// to the spec). So don't process the object if there is no change made to
	// the status.
	if reflect.DeepEqual(oldCloudPrivateIPConfig.Status, newCloudPrivateIPConfig.Status) {
		return nil
	}

	if shouldDelete {
		// Get the EgressIP owner reference
		egressIPName, exists := oldCloudPrivateIPConfig.Annotations[util.OVNEgressIPOwnerRefLabel]
		if !exists {
			// If a CloudPrivateIPConfig object does not have an egress IP owner reference annotation upon deletion,
			// there is no way that the object will get one after deletion. Hence, simply log a warning message here
			// for informative purposes instead of throwing the same error and retrying time and time again.
			klog.Warningf("CloudPrivateIPConfig object %q was missing the egress IP owner reference annotation "+
				"upon deletion", oldCloudPrivateIPConfig.Name)
			return nil
		}
		// Check if the egress IP has been deleted or not, if we are processing
		// a CloudPrivateIPConfig delete because the EgressIP has been deleted
		// then we need to remove the setup made for it, but not update the
		// object.
		egressIP, err := eIPC.kube.GetEgressIP(egressIPName)
		isDeleted := apierrors.IsNotFound(err)
		if err != nil && !isDeleted {
			return err
		}
		egressIPString := cloudPrivateIPConfigNameToIPString(oldCloudPrivateIPConfig.Name)
		statusItem := egressipv1.EgressIPStatusItem{
			Node:     nodeToDelete,
			EgressIP: egressIPString,
		}
		// If we are not processing a delete, update the EgressIP object's
		// status assignments
		if !isDeleted {
			// Deleting a status here means updating the object with the statuses we
			// want to keep
			updatedStatus := []egressipv1.EgressIPStatusItem{}
			for _, status := range egressIP.Status.Items {
				if !reflect.DeepEqual(status, statusItem) {
					updatedStatus = append(updatedStatus, status)
				}
			}
			if err := eIPC.patchEgressIP(egressIP.Name, eIPC.generateEgressIPPatches(egressIP.Name, egressIP.Annotations, updatedStatus)...); err != nil {
				return err
			}
		}
		resyncEgressIPs, err := eIPC.removePendingOpsAndGetResyncs(egressIPName, egressIPString)
		if err != nil {
			return err
		}
		for _, resyncEgressIP := range resyncEgressIPs {
			if err := eIPC.reconcileEgressIP(nil, resyncEgressIP); err != nil {
				return fmt.Errorf("synthetic update for EgressIP: %s failed, err: %v", egressIP.Name, err)
			}
		}
	}
	if shouldAdd {
		// Get the EgressIP owner reference
		egressIPName, exists := newCloudPrivateIPConfig.Annotations[util.OVNEgressIPOwnerRefLabel]
		if !exists {
			// If a CloudPrivateIPConfig object does not have an egress IP owner reference annotation upon creation
			// then we should simply log this as a warning. We should get an update action later down the road where we
			// then take care of the rest. Hence, do not throw an error here to avoid rescheduling. Even though not
			// officially supported, think of someone creating a CloudPrivateIPConfig object manually which will never
			// get the annotation.
			klog.Warningf("CloudPrivateIPConfig object %q is missing the egress IP owner reference annotation. Skipping",
				oldCloudPrivateIPConfig.Name)
			return nil
		}
		egressIP, err := eIPC.kube.GetEgressIP(egressIPName)
		if err != nil {
			return err
		}
		egressIPString := cloudPrivateIPConfigNameToIPString(newCloudPrivateIPConfig.Name)
		statusItem := egressipv1.EgressIPStatusItem{
			Node:     newCloudPrivateIPConfig.Status.Node,
			EgressIP: egressIPString,
		}
		// Guard against performing the same assignment twice, which might
		// happen when multiple updates come in on the same object.
		hasStatus := false
		for _, status := range egressIP.Status.Items {
			if reflect.DeepEqual(status, statusItem) {
				hasStatus = true
				break
			}
		}
		if !hasStatus {
			statusToKeep := append(egressIP.Status.Items, statusItem)
			if err := eIPC.patchEgressIP(egressIP.Name, eIPC.generateEgressIPPatches(egressIP.Name, egressIP.Annotations, statusToKeep)...); err != nil {
				return err
			}
		}

		eIPC.pendingCloudPrivateIPConfigsMutex.Lock()
		defer eIPC.pendingCloudPrivateIPConfigsMutex.Unlock()
		// Remove the finished add / update operation from the pending cache. We
		// never process add and deletes in the same sync, and for updates:
		// deletes are always performed before adds, hence we should only ever
		// fully delete the item from the pending cache once the add has
		// finished.
		ops, pending := eIPC.pendingCloudPrivateIPConfigsOps[egressIPName]
		if !pending {
			// Do not return an error here, it will lead to spurious error
			// messages on restart because we will process a bunch of adds for
			// all existing objects, for which no CR was issued.
			klog.V(5).Infof("No pending operation found for EgressIP: %s while processing created CloudPrivateIPConfig", egressIPName)
			return nil
		}
		op, exists := ops[egressIPString]
		if !exists {
			klog.V(5).Infof("Pending operations found for EgressIP: %s, but not for the created CloudPrivateIPConfig: %s", egressIPName, egressIPString)
			return nil
		}
		// Process finalized add / updates, hence: (op.toAdd != "" &&
		// op.toDelete != "") || (op.toAdd != "" && op.toDelete == ""), which is
		// equivalent the below.
		if op.toAdd != "" {
			delete(ops, egressIPString)
		}
		if len(ops) == 0 {
			delete(eIPC.pendingCloudPrivateIPConfigsOps, egressIPName)
		}
	}
	return nil
}

// cloudPrivateIPConfigNameToIPString converts the resource name to the string
// representation of net.IP. Given a limitation in the Kubernetes API server
// (see: https://github.com/kubernetes/kubernetes/pull/100950)
// CloudPrivateIPConfig.metadata.name cannot represent an IPv6 address. To
// work-around this limitation it was decided that the network plugin creating
// the CR will fully expand the IPv6 address and replace all colons with dots,
// ex:

// The CloudPrivateIPConfig name fc00.f853.0ccd.e793.0000.0000.0000.0054 will be
// represented as address: fc00:f853:ccd:e793::54

// We thus need to replace every fifth character's dot with a colon.
func cloudPrivateIPConfigNameToIPString(name string) string {
	// Handle IPv4, which will work fine.
	if ip := net.ParseIP(name); ip != nil {
		return name
	}
	// Handle IPv6, for which we want to convert the fully expanded "special
	// name" to go's default IP representation
	name = strings.ReplaceAll(name, ".", ":")
	return net.ParseIP(name).String()
}

// removePendingOps removes the existing pending CloudPrivateIPConfig operations
// from the cache and returns the EgressIP object which can be re-synced given
// the new assignment possibilities.
func (eIPC *egressIPClusterController) removePendingOpsAndGetResyncs(egressIPName, egressIP string) ([]*egressipv1.EgressIP, error) {
	eIPC.pendingCloudPrivateIPConfigsMutex.Lock()
	defer eIPC.pendingCloudPrivateIPConfigsMutex.Unlock()
	ops, pending := eIPC.pendingCloudPrivateIPConfigsOps[egressIPName]
	if !pending {
		return nil, fmt.Errorf("no pending operation found for EgressIP: %s", egressIPName)
	}
	op, exists := ops[egressIP]
	if !exists {
		return nil, fmt.Errorf("pending operations found for EgressIP: %s, but not for the finalized IP: %s", egressIPName, egressIP)
	}
	// Make sure we are dealing with a delete operation, since for update
	// operations will still need to process the add afterwards.
	if op.toAdd == "" && op.toDelete != "" {
		delete(ops, egressIP)
	}
	if len(ops) == 0 {
		delete(eIPC.pendingCloudPrivateIPConfigsOps, egressIPName)
	}

	// Some EgressIP objects might not have all of their spec.egressIPs
	// assigned because there was no room to assign them. Hence, every time
	// we process a final deletion for a CloudPrivateIPConfig: have a look
	// at what other EgressIP objects have something un-assigned, and force
	// a reconciliation on them by sending a synthetic update.
	egressIPs, err := eIPC.kube.GetEgressIPs()
	if err != nil {
		return nil, fmt.Errorf("unable to list EgressIPs, err: %v", err)
	}
	resyncs := make([]*egressipv1.EgressIP, 0, len(egressIPs))
	for _, egressIP := range egressIPs {
		egressIP := *egressIP
		// Do not process the egress IP object which owns the
		// CloudPrivateIPConfig for which we are currently processing the
		// deletion for.
		if egressIP.Name == egressIPName {
			continue
		}
		unassigned := len(egressIP.Spec.EgressIPs) - len(egressIP.Status.Items)
		ops, pending := eIPC.pendingCloudPrivateIPConfigsOps[egressIP.Name]
		// If the EgressIP was never added to the pending cache to begin
		// with, but has un-assigned egress IPs, try it.
		if !pending && unassigned > 0 {
			resyncs = append(resyncs, &egressIP)
			continue
		}
		// If the EgressIP has pending operations, have a look at if the
		// unassigned operations superseed the pending ones. It could be
		// that it could only execute a couple of assignments at one point.
		if pending && unassigned > len(ops) {
			resyncs = append(resyncs, &egressIP)
		}
	}
	return resyncs, nil
}

// jsonPatchOperation contains all the info needed to perform a JSON path operation to a k8 object
type jsonPatchOperation struct {
	Operation string      `json:"op"`
	Path      string      `json:"path"`
	Value     interface{} `json:"value,omitempty"`
}

// patchEgressIP performs a patch operation on an EgressIP.
// There are two possible patches operations.
// 1. Mandatory, replace operation of egress IP status field. This allows us to
// update only the status field, without overwriting any other. This is
// important because processing egress IPs can take a while (when running on a
// public cloud and in the worst case), hence we don't want to perform a full
// object update which risks resetting the EgressIP object's fields to the state
// they had when we started processing the change.
// 2. Optional, add operation to its metadata.annotations field.
func (eIPC *egressIPClusterController) patchEgressIP(name string, patches ...jsonPatchOperation) error {
	klog.Infof("Patching status on EgressIP %s: %v", name, patches)
	op, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("error serializing patch operation: %+v, err: %v", patches, err)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return eIPC.kube.PatchEgressIP(name, op)
	})
}

// generateEgressIPPatches conditionally generates a mark patch if the mark doesn't exist. If it fails to allocate a mark,
// log an error instead of failing because we do not wish to block primary default network egress IP assignments due to potential
// mark range exhaustion. Primary default network egress IP currently does not utilize marks to config EgressIP.
// Generating the status patch is mandatory
func (eIPC *egressIPClusterController) generateEgressIPPatches(name string, annotations map[string]string,
	statusItems []egressipv1.EgressIPStatusItem) []jsonPatchOperation {
	patches := make([]jsonPatchOperation, 0, 1)
	if !util.IsEgressIPMarkSet(annotations) {
		if mark, _, err := eIPC.getOrAllocMark(name); err != nil {
			klog.Errorf("Failed to get mark for EgressIP %s: %v", name, err)
		} else {
			patches = append(patches, generateMarkPatchOp(mark))
		}
	}
	return append(patches, generateStatusPatchOp(statusItems))
}

func generateMarkPatchOp(mark int) jsonPatchOperation {
	return jsonPatchOperation{
		Operation: "add",
		Path:      "/metadata/annotations",
		Value:     createAnnotWithMark(mark),
	}
}

func createAnnotWithMark(mark int) map[string]string {
	return map[string]string{util.EgressIPMarkAnnotation: fmt.Sprintf("%d", mark)}
}

func generateStatusPatchOp(statusItems []egressipv1.EgressIPStatusItem) jsonPatchOperation {
	return jsonPatchOperation{
		Operation: "replace",
		Path:      "/status",
		Value: egressipv1.EgressIPStatus{
			Items: statusItems,
		},
	}
}

// syncEgressIPMarkAllocator iterates over all existing EgressIPs. It builds a mark cache of existing marks stored on each
// EgressIP annotation or allocates and adds a new mark to an EgressIP if it doesn't exist
func (eIPC *egressIPClusterController) syncEgressIPMarkAllocator(egressIPs []interface{}) error {
	// reserve previously assigned marks
	for _, object := range egressIPs {
		egressIP, ok := object.(*egressipv1.EgressIP)
		if !ok {
			return fmt.Errorf("failed to cast %T to *egressipv1.EgressIP", egressIP)
		}
		if !util.IsEgressIPMarkSet(egressIP.Annotations) {
			continue
		}
		mark, err := util.ParseEgressIPMark(egressIP.Annotations)
		if err != nil {
			return fmt.Errorf("failed to get mark from EgressIP %s: %v", egressIP.Name, err)
		}
		if !mark.IsValid() {
			return fmt.Errorf("EgressIP %s mark %q is invalid", egressIP.Name, mark.String())
		}
		if err = eIPC.reserveMark(egressIP.Name, mark.ToInt()); err != nil {
			return fmt.Errorf("failed to reserve mark for EgressIP %s: %v", egressIP.Name, err)
		}
	}
	// assign new marks for EgressIPs without a mark
	for _, object := range egressIPs {
		egressIP, ok := object.(*egressipv1.EgressIP)
		if !ok {
			return fmt.Errorf("failed to cast %T to *egressipv1.EgressIP", egressIP)
		}
		if util.IsEgressIPMarkSet(egressIP.Annotations) {
			continue
		}
		mark, releaseMarkFn, err := eIPC.getOrAllocMark(egressIP.Name)
		if err != nil {
			// Mark range is limited so do not return an error in-order not to block pods attached to the CDN
			klog.Errorf("Failed to sync mark allocator: unable to allocate for EgressIP %s: %v", egressIP.Name, err)
		} else {
			if err = eIPC.patchEgressIP(egressIP.Name, generateMarkPatchOp(mark)); err != nil {
				releaseMarkFn()
				return fmt.Errorf("failed to patch EgressIP %s: %v", egressIP.Name, err)
			}
		}
	}
	return nil
}

var (
	eipMarkMax = util.EgressIPMarkMax
	eipMarkMin = util.EgressIPMarkBase
)

func getEgressIPMarkAllocator() id.Allocator {
	return id.NewIDAllocator("eip_mark", eipMarkMax-eipMarkMin)
}

// ensureMark ensures that if a mark was remove or changed value, then restore the mark.
func (eIPC *egressIPClusterController) ensureMark(old, new *egressipv1.EgressIP) {
	// Adding the mark to annotations is bundled with status update in-order to minimise updates, cover the case where there is no update to status
	// and mark annotation has been modified / removed. This should only occur for an update and the mark was previous set.
	if old != nil && new != nil {
		if util.IsEgressIPMarkSet(old.Annotations) && util.EgressIPMarkAnnotationChanged(old.Annotations, new.Annotations) {
			mark, _, err := eIPC.getOrAllocMark(new.Name)
			if err != nil {
				klog.Errorf("Failed to restore EgressIP %s mark because unable to retrieve mark: %v", new.Name, err)
			} else if err = eIPC.patchEgressIP(new.Name, generateMarkPatchOp(mark)); err != nil {
				klog.Errorf("Failed to restore EgressIP %s mark because patching failed: %v", new.Name, err)
			}
		}
	}
}

// getOrAllocMark allocates a new mark integer for name using round-robin strategy if none was already allocated for name otherwise
// returns the previously allocated mark.
// The mark is bounded by util.EgressIPMarkBase & util.EgressIPMarkMax inclusive.
// If range is exhausted, error is returned. Before calling this func, syncEgressIPMarkAllocator must be called
// to build the initial mark cache. Return func releases the mark in-case of error.
func (eIPC *egressIPClusterController) getOrAllocMark(name string) (int, func(), error) {
	if name == "" {
		return 0, nil, fmt.Errorf("EgressIP name cannot be blank")
	}
	mark, err := eIPC.markAllocator.AllocateID(name)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to allocate mark for EgressIP %q: %v", name, err)
	}
	mark = mark + util.EgressIPMarkBase
	if !util.IsEgressIPMarkValid(mark) {
		eIPC.markAllocator.ReleaseID(name)
		return 0, nil, fmt.Errorf("for EgressIP %s, mark %d allocated is invalid. Must be between %d and %d", name, mark, util.EgressIPMarkBase, util.EgressIPMarkMax)
	}
	return mark, func() {
		eIPC.markAllocator.ReleaseID(name)
	}, nil
}

// deallocMark de-allocates a mark
func (eIPC *egressIPClusterController) deallocMark(name string) {
	eIPC.markAllocator.ReleaseID(name)
}

// reserveMark reserves a previously assigned mark to the mark cache
func (eIPC *egressIPClusterController) reserveMark(name string, mark int) error {
	if name == "" {
		return fmt.Errorf("name cannot be blank")
	}
	mark = mark - util.EgressIPMarkBase
	if mark < 0 {
		return fmt.Errorf("unable to reserve mark because calculated offset is less than zero")
	}
	if err := eIPC.markAllocator.ReserveID(name, mark); err != nil {
		return fmt.Errorf("failed to reserve mark: %v", err)
	}
	return nil
}
