package ovn

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

type podCacheEntry struct {
	UID         ktypes.UID
	Namespace   string
	Name        string
	Annotations map[string]string
	HostNetwork bool
	NodeName    string
}

const (
	maxPodRetries = 10
)

// ensurePod tries to set up a pod. It returns success or failure; failure
// indicates the pod should be retried later.
func (oc *Controller) ensurePod(key string, pod *kapi.Pod) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		klog.V(5).Infof("Pod %s not scheduled; trying again later", pod.Name)
		return nil
	}

	tmp, ok := oc.podsCache.Load(key)
	if ok {
		pe := tmp.(*podCacheEntry)
		if exGatewayAnnotationsChanged(pe, pod) || networkStatusAnnotationsChanged(pe, pod) {
			oc.deletePodExternalGW(pe)
		}
	}

	oc.cachePod(key, pod)
	if util.PodWantsNetwork(pod) {
		if err := oc.addLogicalPort(pod); err != nil {
			oc.recordPodEvent(err, pod)
			return err
		}
	} else {
		if err := oc.addPodExternalGW(pod); err != nil {
			oc.recordPodEvent(err, pod)
			return err
		}
	}
	return nil
}

func (oc *Controller) cachePod(key string, pod *kapi.Pod) {
	oc.podsCache.Store(key, &podCacheEntry{
		UID:         pod.UID,
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		Annotations: pod.Annotations,
		HostNetwork: pod.Spec.HostNetwork,
		NodeName:    pod.Spec.NodeName,
	})
}

// initPodController initializes the Pod controller and *must* be called before
// runPodController
func (oc *Controller) initPodController(pods informers.PodInformer) {
	oc.pods = pods.Lister()
	oc.podsSynced = pods.Informer().HasSynced
	oc.podsQueue = workqueue.NewNamedRateLimitingQueue(
		workqueue.NewItemFastSlowRateLimiter(1*time.Second, 5*time.Second, 5),
		"pods",
	)

	pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				oc.podsQueue.Add(key)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				oc.podsQueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				oc.podsQueue.Add(key)
			}
		},
	})
}

func (oc *Controller) runPodController(threadiness int, stopCh <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()

	klog.Infof("Starting Pod Controller")

	// wait for your caches to fill before starting your work
	if !cache.WaitForCacheSync(stopCh, oc.podsSynced) {
		return
	}

	// run the repair controller
	klog.Infof("Repairing Pods")
	oc.repairPods()

	// start up your worker threads based on threadiness.  Some controllers
	// have multiple kinds of workers
	wg := &sync.WaitGroup{}
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		// runWorker will loop until "something bad" happens.  The .Until will
		// then rekick the worker after one second
		go func() {
			defer wg.Done()
			wait.Until(func() {
				oc.runPodWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	// wait until we're told to stop
	<-stopCh

	klog.Infof("Shutting down Pod controller")
	// make sure the work queue is shutdown which will trigger workers to end
	oc.podsQueue.ShutDown()
	// wait for workers to finish
	wg.Wait()
}

func (oc *Controller) runPodWorker(wg *sync.WaitGroup) {
	// hot loop until we're told to stop.  processNextWorkItem will
	// automatically wait until there's work available, so we don't worry
	// about secondary waits
	for oc.processNextPodWorkItem(wg) {
	}
}

// processNextPodWorkItem deals with one key off the queue.  It returns false
// when it's time to quit.
func (oc *Controller) processNextPodWorkItem(wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()
	// pull the next work item from queue.  It should be a key we use to lookup
	// something in a cache
	key, quit := oc.podsQueue.Get()
	if quit {
		return false
	}
	// you always have to indicate to the queue that you've completed a piece of
	// work
	defer oc.podsQueue.Done(key)

	// do your work on the key.  This method will contains your "do stuff" logic
	err := oc.syncPodHandler(key.(string))
	if err == nil {
		// if you had no error, tell the queue to stop tracking history for your
		// key. This will reset things like failure counts for per-item rate
		// limiting
		oc.podsQueue.Forget(key)
		return true
	}

	// there was a failure so be sure to report it.  This method allows for
	// pluggable error handling which can be used for things like
	// cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	// since we failed, we should requeue the item to work on later.
	// but only if we've not exceeded max retries. This method
	// will add a backoff to avoid hotlooping on particular items
	// (they're probably still not going to work right away) and overall
	// controller protection (everything I've done is broken, this controller
	// needs to calm down or it can starve other useful work) cases.
	if oc.podsQueue.NumRequeues(key) < maxPodRetries {
		oc.podsQueue.AddRateLimited(key)
		return true
	}

	// if we've exceeded MaxRetries, remove the item from the queue
	oc.podsQueue.Forget(key)

	return true
}

func (oc *Controller) repairPods() {
	var allOps []ovsdb.Operation
	// get the list of logical switch ports (equivalent to pods)
	expectedLogicalPorts := make(map[string]bool)
	klog.Infof("Getting pod list")
	pods, err := oc.pods.List(labels.Everything())
	// as the this is called after the cache has synced we shouldn't hit this
	if err != nil {
		klog.Error(err)
		utilruntime.HandleError(err)
	}
	for _, pod := range pods {
		annotations, err := util.UnmarshalPodAnnotation(pod.GetAnnotations())
		if util.PodScheduled(pod) && util.PodWantsNetwork(pod) && err == nil {
			logicalPort := util.GetLogicalPortName(pod.Namespace, pod.Name)
			expectedLogicalPorts[logicalPort] = true

			// Wait for the node logical switch to be created by the ClusterController.
			// The node switch will be created when the node's logical network infrastructure
			// is created by the node watch.
			if err := wait.PollImmediate(30*time.Millisecond, 30*time.Second, func() (bool, error) {
				subnets := oc.lsManager.GetSwitchSubnets(pod.Spec.NodeName)
				return len(subnets) > 0, nil
			}); err != nil {
				klog.Errorf("Timed out waiting for node %s to be added to cache. IP allocation may fail!",
					pod.Spec.NodeName)
			}

			if err = oc.lsManager.AllocateIPs(pod.Spec.NodeName, annotations.IPs); err != nil {
				if err == ipallocator.ErrAllocated {
					// already allocated: log an error but not stop syncPod from continuing
					klog.Errorf("Already allocated IPs: %s for pod: %s on node: %s",
						util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
						pod.Spec.NodeName)
				} else {
					klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
						" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
						pod.Spec.NodeName, err)
				}
			}
		}
	}

	// in order to minimize the number of database transactions build a map of all ports keyed by UUID
	portCache := make(map[string]nbdb.LogicalSwitchPort)
	lspList := []nbdb.LogicalSwitchPort{}
	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	if err := oc.nbClient.List(ctx, &lspList); err != nil {
		klog.Errorf("Cannot sync pods, cannot retrieve list of logical switch ports: %v", err)
		return
	}
	for _, lsp := range lspList {
		portCache[lsp.UUID] = lsp
	}
	// get all the nodes from the watchFactory
	nodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Failed to get nodes: %v", err)
		return
	}
	for _, n := range nodes {
		stalePorts := []string{}
		// find the logical switch for the node
		ls := &nbdb.LogicalSwitch{}
		if lsUUID, ok := oc.lsManager.GetUUID(n.Name); !ok {
			klog.Errorf("Error getting logical switch for node %s: %s", n.Name, "Switch not in logical switch cache")

			// Not in cache: Try getting the logical switch from ovn database (slower method)
			if ls, err = libovsdbops.FindSwitchByName(oc.nbClient, n.Name); err != nil {
				klog.Errorf("Can't find switch for node %s: %v", n.Name, err)
				continue
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
			defer cancel()

			ls.UUID = lsUUID
			if err := oc.nbClient.Get(ctx, ls); err != nil {
				klog.Errorf("Error getting logical switch for node %s (UUID: %s) from ovn database: %v", n.Name, ls.UUID, err)
				continue
			}
		}
		for _, port := range ls.Ports {
			if portCache[port].ExternalIDs["pod"] == "true" {
				if _, ok := expectedLogicalPorts[portCache[port].Name]; !ok {
					stalePorts = append(stalePorts, port)
				}
			}
		}
		if len(stalePorts) > 0 {
			ops, err := oc.nbClient.Where(ls).Mutate(ls, model.Mutation{
				Field:   &ls.Ports,
				Mutator: ovsdb.MutateOperationDelete,
				Value:   stalePorts,
			})
			if err != nil {
				klog.Errorf("Could not generate ops to delete stale ports from logical switch %s: %v", n.Name, err)
				continue
			}
			allOps = append(allOps, ops...)
		}
	}
	_, err = libovsdbops.TransactAndCheck(oc.nbClient, allOps)
	if err != nil {
		klog.Errorf("Could not remove stale logicalPorts from switches: %v", err)
		return
	}
}

func (oc *Controller) deletePod(key string) error {
	pe, ok := oc.podsCache.Load(key)
	if !ok {
		klog.Warningf("Pod %s not found in pod cache on delete; may not have been scheduled", key)
		return nil
	}
	if err := oc.deleteLogicalPort(pe.(*podCacheEntry)); err != nil {
		return err
	}
	oc.podsCache.Delete(key)
	return nil
}

// sync pod handler brings the pod resource in sync
func (oc *Controller) syncPodHandler(key string) error {
	klog.Infof("Syncing %s", key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	pod, err := oc.pods.Pods(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		// Very unlikely to see any error other than "not found" since
		// wer're pulling from the informer cache, but just try again
		return err
	}

	// Delete the pod if it was not found (already gone) or if it had a
	// deletion timestamp set (deleted but not yet finalized); if so
	if apierrors.IsNotFound(err) {
		return oc.deletePod(key)
	}

	// If the pod was deleted and re-added quickly; delete and re-add the port
	pe, ok := oc.podsCache.Load(key)
	if ok {
		cachedPod := pe.(*podCacheEntry)
		if cachedPod.UID != pod.UID {
			if err := oc.deletePod(key); err != nil {
				return err
			}
		}
	}

	// this wasn't a delete, so we run ensurePod
	if err := oc.ensurePod(key, pod); err != nil {
		return err
	}

	return nil
}

func (oc *Controller) deleteLogicalPort(pe *podCacheEntry) error {
	oc.deletePodExternalGW(pe)
	if pe.HostNetwork {
		return nil
	}

	podDesc := pe.Namespace + "/" + pe.Name
	klog.Infof("Deleting pod: %s", podDesc)

	logicalPort := util.GetLogicalPortName(pe.Namespace, pe.Name)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())

		// If ovnkube-master restarts, it is also possible the Pod's logical switch port
		// is not re-added into the cache. Delete logical switch port anyway.
		ops, err := oc.ovnNBLSPDel(logicalPort, pe.NodeName, "")
		if err != nil {
			klog.Errorf(err.Error())
		} else {
			if _, err := libovsdbops.TransactAndCheck(oc.nbClient, ops); err != nil {
				klog.Errorf("Cannot delete logical switch port %s, %v", logicalPort, err)
			}
		}

		// Even if the port is not in the cache, IPs annotated in the Pod annotation may already be allocated,
		// need to release them to avoid leakage.
		if annotation, err := util.UnmarshalPodAnnotation(pe.Annotations); err == nil {
			_ = oc.lsManager.ReleaseIPs(pe.NodeName, annotation.IPs)
		}
		return nil
	}

	// FIXME: if any of these steps fails we need to stop and try again later...
	var allOps, ops []ovsdb.Operation
	if ops, err = oc.deletePodFromNamespace(pe.Namespace, portInfo); err != nil {
		klog.Errorf(err.Error())
	} else {
		allOps = append(allOps, ops...)
	}

	ops, err = oc.ovnNBLSPDel(logicalPort, pe.NodeName, portInfo.uuid)
	if err != nil {
		klog.Errorf(err.Error())
	} else {
		allOps = append(allOps, ops...)
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, allOps)
	if err != nil {
		klog.Errorf("Cannot delete logical switch port %s, %v", logicalPort, err)
	}

	if err := oc.lsManager.ReleaseIPs(portInfo.logicalSwitch, portInfo.ips); err != nil {
		return err
	}

	if config.Gateway.DisableSNATMultipleGWs {
		if err := deletePerPodGRSNAT(oc.nbClient, pe.NodeName, []*net.IPNet{}, portInfo.ips); err != nil {
			klog.Errorf(err.Error())
		}
	}
	podNsName := ktypes.NamespacedName{Namespace: pe.Namespace, Name: pe.Name}
	oc.deleteGWRoutesForPod(podNsName, portInfo.ips)

	oc.logicalPortCache.remove(logicalPort)
	return nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet) error {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetK8sPodAllNetworks(pod)
	if err != nil {
		return fmt.Errorf("error while getting network attachment definition for [%s/%s]: %v",
			pod.Namespace, pod.Name, err)
	}
	otherDefaultRouteV4 := false
	otherDefaultRouteV6 := false
	for _, network := range networks {
		for _, gatewayRequest := range network.GatewayRequest {
			if utilnet.IsIPv6(gatewayRequest) {
				otherDefaultRouteV6 = true
			} else {
				otherDefaultRouteV4 = true
			}
		}
	}

	for _, podIfAddr := range podAnnotation.IPs {
		isIPv6 := utilnet.IsIPv6CIDR(podIfAddr)
		nodeSubnet, err := util.MatchIPNetFamily(isIPv6, nodeSubnets)
		if err != nil {
			return err
		}

		gatewayIPnet := util.GetNodeGatewayIfAddr(nodeSubnet)

		otherDefaultRoute := otherDefaultRouteV4
		if isIPv6 {
			otherDefaultRoute = otherDefaultRouteV6
		}
		var gatewayIP net.IP
		if otherDefaultRoute {
			for _, clusterSubnet := range config.Default.ClusterSubnets {
				if isIPv6 == utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
			for _, serviceSubnet := range config.Kubernetes.ServiceCIDRs {
				if isIPv6 == utilnet.IsIPv6CIDR(serviceSubnet) {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    serviceSubnet,
						NextHop: gatewayIPnet.IP,
					})
				}
			}
		} else {
			gatewayIP = gatewayIPnet.IP
		}

		if len(config.HybridOverlay.ClusterSubnets) > 0 {
			// Add a route for each hybrid overlay subnet via the hybrid
			// overlay port on the pod's logical switch.
			nextHop := util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
			for _, clusterSubnet := range config.HybridOverlay.ClusterSubnets {
				if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) == isIPv6 {
					podAnnotation.Routes = append(podAnnotation.Routes, util.PodRoute{
						Dest:    clusterSubnet.CIDR,
						NextHop: nextHop,
					})
				}
			}
		}
		if gatewayIP != nil {
			podAnnotation.Gateways = append(podAnnotation.Gateways, gatewayIP)
		}
	}
	return nil
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	logicalSwitch := pod.Spec.NodeName
	nodeSubnets := oc.lsManager.GetSwitchSubnets(logicalSwitch)
	if len(nodeSubnets) == 0 {
		// Was either a non-host-subnet node (eg, hybrid overlay) or
		// the node's switch wasn't set up yet. Try later.
		return fmt.Errorf("node switch had no assigned OVN subnets; trying again later")
	}

	lsUUID, ok := oc.lsManager.GetUUID(logicalSwitch)
	if !ok {
		return fmt.Errorf("[%s/%s] logical switch %q doesn't exist; trying again later", pod.Namespace, pod.Name, logicalSwitch)
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	portName := util.GetLogicalPortName(pod.Namespace, pod.Name)
	klog.Infof("[%s/%s] creating logical port for pod on switch %s", pod.Namespace, pod.Name, logicalSwitch)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var allOps []ovsdb.Operation
	var addresses []string
	var releaseIPs bool
	lspExist := false
	needsIP := true

	ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	getLSP := &nbdb.LogicalSwitchPort{Name: portName}
	err = oc.nbClient.Get(ctx, getLSP)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
	}
	lsp := &nbdb.LogicalSwitchPort{Name: portName}
	if len(getLSP.UUID) == 0 {
		lsp.UUID = libovsdbops.BuildNamedUUID()
	} else {
		lsp.UUID = getLSP.UUID
		lspExist = true
	}

	lsp.Options = make(map[string]string)
	// Unique identifier to distinguish interfaces for recreated pods, also set by ovnkube-node
	// ovn-controller will claim the OVS interface only if external_ids:iface-id
	// matches with the Port_Binding.logical_port and external_ids:iface-id-ver matches
	// with the Port_Binding.options:iface-id-ver. This is not mandatory.
	// If Port_binding.options:iface-id-ver is not set, then OVS
	// Interface.external_ids:iface-id-ver if set is ignored.
	// Don't set iface-id-ver for already existing LSP if it wasn't set before,
	// because the corresponding OVS port may not have it set
	// (then ovn-controller won't bind the interface).
	// May happen on upgrade, because ovnkube-node doesn't update
	// existing OVS interfaces with new iface-id-ver option.
	if !lspExist || len(getLSP.Options["iface-id-ver"]) != 0 {
		lsp.Options["iface-id-ver"] = string(pod.UID)
	}
	// Bind the port to the node's chassis; prevents ping-ponging between
	// chassis if ovnkube-node isn't running correctly and hasn't cleared
	// out iface-id for an old instance of this pod, and the pod got
	// rescheduled.
	lsp.Options["requested-chassis"] = pod.Spec.NodeName

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations)

	// the IPs we allocate in this function need to be released back to the
	// IPAM pool if there is some error in any step of addLogicalPort past
	// the point the IPs were assigned via the IPAM manager.
	// this needs to be done only when releaseIPs is set to true (the case where
	// we truly have assigned podIPs in this call) AND when there is no error in
	// the rest of the functionality of addLogicalPort. It is important to use a
	// named return variable for defer to work correctly.

	defer func() {
		if releaseIPs && err != nil {
			if relErr := oc.lsManager.ReleaseIPs(logicalSwitch, podIfAddrs); relErr != nil {
				klog.Errorf("Error when releasing IPs for node: %s, err: %q",
					logicalSwitch, relErr)
			} else {
				klog.Infof("Released IPs: %s for node: %s", util.JoinIPNetIPs(podIfAddrs, " "), logicalSwitch)
			}
		}
	}()

	if err == nil {
		podMac = annotation.MAC
		podIfAddrs = annotation.IPs

		// If the pod already has annotations use the existing static
		// IP/MAC from the annotation.
		lsp.DynamicAddresses = nil

		// ensure we have reserved the IPs in the annotation
		if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			return fmt.Errorf("unable to ensure IPs allocated for already annotated pod: %s, IPs: %s, error: %v",
				pod.Name, util.JoinIPNetIPs(podIfAddrs, " "), err)
		}
		needsIP = false
	}

	if needsIP {
		// try to get the IP from existing port in OVN first
		podMac, podIfAddrs, err = oc.getPortAddresses(logicalSwitch, portName)
		if err != nil {
			return fmt.Errorf("failed to get pod addresses for pod %s on node: %s, err: %v",
				portName, logicalSwitch, err)
		}
		needsNewAllocation := false
		// ensure we have reserved the IPs found in OVN
		if len(podIfAddrs) == 0 {
			needsNewAllocation = true
		} else if err = oc.lsManager.AllocateIPs(logicalSwitch, podIfAddrs); err != nil && err != ipallocator.ErrAllocated {
			klog.Warningf("Unable to allocate IPs found on existing OVN port: %s, for pod %s on node: %s"+
				" error: %v", util.JoinIPNetIPs(podIfAddrs, " "), portName, logicalSwitch, err)

			needsNewAllocation = true
		}
		if needsNewAllocation {
			// Previous attempts to use already configured IPs failed, need to assign new
			podMac, podIfAddrs, err = oc.assignPodAddresses(logicalSwitch)
			if err != nil {
				return fmt.Errorf("failed to assign pod addresses for pod %s on node: %s, err: %v",
					portName, logicalSwitch, err)
			}
		}

		releaseIPs = true
		network, err := util.GetK8sPodDefaultNetwork(pod)
		// handle error cases separately first to ensure binding to err, otherwise the
		// defer will fail
		if err != nil {
			return fmt.Errorf("error while getting custom MAC config for port %q from "+
				"default-network's network-attachment: %v", portName, err)
		}

		if network != nil && network.MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, network.MacRequest)
			podMac, err = net.ParseMAC(network.MacRequest)
			if err != nil {
				return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
					network.MacRequest, pod.Name, err)
			}
		}
		podAnnotation := util.PodAnnotation{
			IPs: podIfAddrs,
			MAC: podMac,
		}
		err = oc.addRoutesGatewayIP(pod, &podAnnotation, nodeSubnets)
		if err != nil {
			return err
		}
		var marshalledAnnotation map[string]interface{}
		marshalledAnnotation, err = util.MarshalPodAnnotation(&podAnnotation)
		if err != nil {
			return fmt.Errorf("error creating pod network annotation: %v", err)
		}

		klog.V(5).Infof("Annotation values: ip=%v ; mac=%s ; gw=%s\nAnnotation=%s",
			podIfAddrs, podMac, podAnnotation.Gateways, marshalledAnnotation)
		if err = oc.kube.SetAnnotationsOnPod(pod.Namespace, pod.Name, marshalledAnnotation); err != nil {
			return fmt.Errorf("failed to set annotation on pod %s: %v", pod.Name, err)
		}
		releaseIPs = false
	}

	// Ensure the namespace/nsInfo exists
	routingExternalGWs, routingPodGWs, ops, err := oc.addPodToNamespace(pod.Namespace, podIfAddrs)
	if err != nil {
		return err
	}
	allOps = append(allOps, ops...)

	// if we have any external or pod Gateways, add routes
	gateways := make([]*gatewayInfo, 0, len(routingExternalGWs.gws)+len(routingPodGWs))

	if len(routingExternalGWs.gws) > 0 {
		gateways = append(gateways, routingExternalGWs)
	}
	for _, gw := range routingPodGWs {
		if len(gw.gws) > 0 {
			if err = validateRoutingPodGWs(routingPodGWs); err != nil {
				klog.Error(err)
			}
			gateways = append(gateways, &gw)
		} else {
			klog.Warningf("Found routingPodGW with no gateways ip set for namespace %s", pod.Namespace)
		}
	}

	if len(gateways) > 0 {
		podNsName := ktypes.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
		err = oc.addGWRoutesForPod(gateways, podIfAddrs, podNsName, pod.Spec.NodeName)
		if err != nil {
			return err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set and does not have
		// namespace annotations to go through external egress router
		if extIPs, err := getExternalIPsGRSNAT(oc.watchFactory, pod.Spec.NodeName); err != nil {
			return err
		} else if err = addOrUpdatePerPodGRSNAT(oc.nbClient, pod.Spec.NodeName, extIPs, podIfAddrs); err != nil {
			return err
		}
	}

	// check if this pod is serving as an external GW
	err = oc.addPodExternalGW(pod)
	if err != nil {
		return fmt.Errorf("failed to handle external GW check: %v", err)
	}

	// set addresses on the port
	// LSP addresses in OVN are a single space-separated value
	addresses = []string{podMac.String()}
	for _, podIfAddr := range podIfAddrs {
		addresses[0] = addresses[0] + " " + podIfAddr.IP.String()
	}

	lsp.Addresses = addresses

	// add external ids
	lsp.ExternalIDs = map[string]string{"namespace": pod.Namespace, "pod": "true"}

	// CNI depends on the flows from port security, delay setting it until end
	lsp.PortSecurity = addresses

	if !lspExist {
		timeout := ovntypes.OVSDBWaitTimeout
		allOps = append(allOps, ovsdb.Operation{
			Op:      ovsdb.OperationWait,
			Timeout: &timeout,
			Table:   "Logical_Switch_Port",
			Where:   []ovsdb.Condition{{Column: "name", Function: ovsdb.ConditionEqual, Value: lsp.Name}},
			Columns: []string{"name"},
			Until:   "!=",
			Rows:    []ovsdb.Row{{"name": lsp.Name}},
		})

		// create new logical switch port
		ops, err := oc.nbClient.Create(lsp)
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)

		//add the logical switch port to the logical switch
		ls := &nbdb.LogicalSwitch{UUID: lsUUID}
		ops, err = oc.nbClient.Where(ls).Mutate(ls, model.Mutation{
			Field:   &ls.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, ops...)

	} else {
		//update Existing logical switch port
		ops, err := oc.nbClient.Where(lsp).Update(lsp, &lsp.Addresses, &lsp.ExternalIDs, &lsp.Options, &lsp.PortSecurity)
		if err != nil {
			return fmt.Errorf("could not create commands to update logical switch port %s - %+v", portName, err)
		}
		allOps = append(allOps, ops...)
	}

	results, err := libovsdbops.TransactAndCheckAndSetUUIDs(oc.nbClient, lsp, allOps)
	if err != nil {

		return fmt.Errorf("could not perform creation or update of logical switch port %s - %+v", portName, err)
	}
	go oc.metricsRecorder.AddLSPEvent(pod.UID)

	// if somehow lspUUID is empty, there is a bug here with interpreting OVSDB results
	if len(lsp.UUID) == 0 {
		return fmt.Errorf("UUID is empty from LSP: %q create operation. OVSDB results: %#v", portName, results)
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, lsp.UUID, podMac, podIfAddrs)

	// If multicast is allowed and enabled for the namespace, add the port to the allow policy.
	// FIXME: there's a race here with the Namespace multicastUpdateNamespace() handler, but
	// it's rare and easily worked around for now.
	ns, err := oc.watchFactory.GetNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if oc.multicastSupport && isNamespaceMulticastEnabled(ns.Annotations) {
		if err := podAddAllowMulticastPolicy(oc.nbClient, pod.Namespace, portInfo); err != nil {
			return err
		}
	}
	// observe the pod creation latency metric.
	metrics.RecordPodCreated(pod)
	return nil
}

// Given a node, gets the next set of addresses (from the IPAM) for each of the node's
// subnets to assign to the new pod
func (oc *Controller) assignPodAddresses(nodeName string) (net.HardwareAddr, []*net.IPNet, error) {
	var (
		podMAC   net.HardwareAddr
		podCIDRs []*net.IPNet
		err      error
	)
	podCIDRs, err = oc.lsManager.AllocateNextIPs(nodeName)
	if err != nil {
		return nil, nil, err
	}
	if len(podCIDRs) > 0 {
		podMAC = util.IPAddrToHWAddr(podCIDRs[0].IP)
	}
	return podMAC, podCIDRs, nil
}

// Given a pod and the node on which it is scheduled, get all addresses currently assigned
// to it from the nbdb.
func (oc *Controller) getPortAddresses(nodeName, portName string) (net.HardwareAddr, []*net.IPNet, error) {
	podMac, podIPs, err := util.GetPortAddresses(portName, oc.nbClient)
	if err != nil {
		return nil, nil, err
	}

	if podMac == nil || len(podIPs) == 0 {
		return nil, nil, nil
	}

	var podIPNets []*net.IPNet

	nodeSubnets := oc.lsManager.GetSwitchSubnets(nodeName)

	for _, ip := range podIPs {
		for _, subnet := range nodeSubnets {
			if subnet.Contains(ip) {
				podIPNets = append(podIPNets,
					&net.IPNet{
						IP:   ip,
						Mask: subnet.Mask,
					})
				break
			}
		}
	}
	return podMac, podIPNets, nil
}

// ovnNBLSPDel deletes the given logical switch using the libovsdb library
func (oc *Controller) ovnNBLSPDel(logicalPort, logicalSwitch, lspUUID string) ([]ovsdb.Operation, error) {
	var allOps []ovsdb.Operation
	ls := &nbdb.LogicalSwitch{}
	if lsUUID, ok := oc.lsManager.GetUUID(logicalSwitch); !ok {
		return nil, fmt.Errorf("error getting logical switch for node %s: switch not in logical switch cache", logicalSwitch)
	} else {
		ls.UUID = lsUUID
	}

	lsp := &nbdb.LogicalSwitchPort{
		UUID: lspUUID,
		Name: logicalPort,
	}
	if lspUUID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
		defer cancel()
		if err := oc.nbClient.Get(ctx, lsp); err != nil {
			if err != libovsdbclient.ErrNotFound {
				return nil, fmt.Errorf("cannot delete logical switch port %s failed retrieving the object %v", logicalPort, err)
			}
			// Ignore error and return success if the port is already deleted
			return nil, nil
		}
	}

	ops, err := oc.nbClient.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return nil, fmt.Errorf("cannot generate ops delete logical switch port %s: %v", logicalPort, err)
	}
	allOps = append(allOps, ops...)

	// for testing purposes the explicit delete of the logical switch port is required
	ops, err = oc.nbClient.Where(lsp).Delete()
	if err != nil {
		return nil, fmt.Errorf("cannot generate ops delete logical switch port %s: %v", logicalPort, err)
	}
	allOps = append(allOps, ops...)

	return allOps, nil
}

func exGatewayAnnotationsChanged(oldPod *podCacheEntry, newPod *kapi.Pod) bool {
	return oldPod.Annotations[routingNamespaceAnnotation] != newPod.Annotations[routingNamespaceAnnotation] ||
		oldPod.Annotations[routingNetworkAnnotation] != newPod.Annotations[routingNetworkAnnotation] ||
		oldPod.Annotations[bfdAnnotation] != newPod.Annotations[bfdAnnotation]
}

func networkStatusAnnotationsChanged(oldPod *podCacheEntry, newPod *kapi.Pod) bool {
	return oldPod.Annotations[nettypes.NetworkStatusAnnot] != newPod.Annotations[nettypes.NetworkStatusAnnot]
}
