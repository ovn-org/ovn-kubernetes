package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	goovn "github.com/ebay/go-ovn"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

type podCacheEntry struct {
	Namespace   string
	Name        string
	Annotations map[string]string
	HostNetwork bool
	NodeName    string
}

const (
	maxPodRetries = 5
)

// ensurePod tries to set up a pod. It returns success or failure; failure
// indicates the pod should be retried later.
func (oc *Controller) ensurePod(key string, pod *kapi.Pod) error {
	// Try unscheduled pods later
	if !util.PodScheduled(pod) {
		return fmt.Errorf("pod %s not scheduled", pod.Name)
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
		workqueue.DefaultControllerRateLimiter(),
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
			wait.Until(oc.runPodWorker, time.Second, stopCh)
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

func (oc *Controller) runPodWorker() {
	// hot loop until we're told to stop.  processNextWorkItem will
	// automatically wait until there's work available, so we don't worry
	// about secondary waits
	for oc.processNextPodWorkItem() {
	}
}

// processNextPodWorkItem deals with one key off the queue.  It returns false
// when it's time to quit.
func (oc *Controller) processNextPodWorkItem() bool {
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

// Builds the logical switch port name for a given pod.
func logicalPortName(namespace, name string) string {
	return namespace + "_" + name
}

// Builds the logical switch port name for a given pod.
func podLogicalPortName(pod *kapi.Pod) string {
	return logicalPortName(pod.Namespace, pod.Name)
}

func (oc *Controller) repairPods() {
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
			logicalPort := podLogicalPortName(pod)
			expectedLogicalPorts[logicalPort] = true
			if err = oc.lsManager.AllocateIPs(pod.Spec.NodeName, annotations.IPs); err != nil {
				klog.Errorf("Couldn't allocate IPs: %s for pod: %s on node: %s"+
					" error: %v", util.JoinIPNetIPs(annotations.IPs, " "), logicalPort,
					pod.Spec.NodeName, err)
			}
		}
	}

	existingLogicalPorts := make([]string, 0)
	klog.Info("Getting logical ports from OVN")
	// get the list of logical ports from OVN
	nodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		klog.Errorf("Failed to get nodes")
		return
	}
	for _, n := range nodes {
		klog.Infof("Node %v", n.Name)
		nodeSwitchPorts, err := oc.ovnNBClient.LSPList(n.Name)
		if err != nil {
			klog.Errorf("Failed to list lsp for switch %s: error %v", n.Name, err)
			continue
		}
		for _, port := range nodeSwitchPorts {
			if port.ExternalID["pod"] == "true" {
				existingLogicalPorts = append(existingLogicalPorts, port.Name)
			}
		}
	}

	for _, existingPort := range existingLogicalPorts {
		if _, ok := expectedLogicalPorts[existingPort]; !ok {
			// not found, delete this logical port
			klog.Infof("Stale logical port found: %s. This logical port will be deleted.", existingPort)
			cmd, err := oc.ovnNBClient.LSPDel(existingPort)
			if err != nil {
				klog.Errorf("Error in getting the cmd to delete pod's logical port %s %v", existingPort, err)
				continue
			}
			err = oc.ovnNBClient.Execute(cmd)
			if err != nil {
				klog.Errorf("Error deleting pod's logical port %s %v", existingPort, err)
				continue
			}
		}
	}
	// Remove legacy ecmp routes when moving to external bridge
	oc.cleanECMPRoutes()
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
	if apierrors.IsNotFound(err) || !pod.GetDeletionTimestamp().IsZero() {
		pe, ok := oc.podsCache.Load(key)
		if !ok {
			klog.Warningf("Pod %s not found in pod cache on delete", key)
			return nil
		}
		if err := oc.deleteLogicalPort(pe.(*podCacheEntry)); err != nil {
			return err
		}
		oc.podsCache.Delete(key)
		return nil
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

	logicalPort := logicalPortName(pe.Namespace, pe.Name)
	portInfo, err := oc.logicalPortCache.get(logicalPort)
	if err != nil {
		klog.Errorf(err.Error())
		// If ovnkube-master restarts, it is also possible the Pod's logical switch port
		// is not readded into the cache. Delete logical switch port anyway.
		err = util.OvnNBLSPDel(oc.ovnNBClient, logicalPort)
		if err != nil {
			return err
		}

		// Even if the port is not in the cache, IPs annotated in the Pod annotation may already be allocated,
		// need to release them to avoid leakage.
		logicalSwitch := pe.NodeName
		if logicalSwitch != "" {
			annotation, err := util.UnmarshalPodAnnotation(pe.Annotations)
			if err == nil {
				podIfAddrs := annotation.IPs
				_ = oc.lsManager.ReleaseIPs(logicalSwitch, podIfAddrs)
			}
		}
		return nil
	}

	if err := oc.deletePodFromNamespace(pe.Namespace, portInfo); err != nil {
		return err
	}

	err = util.OvnNBLSPDel(oc.ovnNBClient, logicalPort)
	if err != nil {
		return err
	}

	if err := oc.lsManager.ReleaseIPs(portInfo.logicalSwitch, portInfo.ips); err != nil {
		return err
	}

	if config.Gateway.DisableSNATMultipleGWs {
		oc.deletePerPodGRSNAT(pe.NodeName, portInfo.ips)
	}
	oc.deleteGWRoutesForPod(pe.Namespace, portInfo.ips)
	oc.logicalPortCache.remove(logicalPort)
	return nil
}

func (oc *Controller) addRoutesGatewayIP(pod *kapi.Pod, podAnnotation *util.PodAnnotation, nodeSubnets []*net.IPNet) error {
	// if there are other network attachments for the pod, then check if those network-attachment's
	// annotation has default-route key. If present, then we need to skip adding default route for
	// OVN interface
	networks, err := util.GetPodNetSelAnnotation(pod, util.NetworkAttachmentAnnotation)
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

func (oc *Controller) getRoutingExternalGWs(ns string) gatewayInfo {
	res := gatewayInfo{}
	nsInfo := oc.getNamespaceLocked(ns)
	if nsInfo == nil {
		return res
	}
	defer nsInfo.Unlock()
	// return a copy of the object so it can be handled without the
	// namespace locked
	res.bfdEnabled = nsInfo.routingExternalGWs.bfdEnabled
	res.gws = make([]net.IP, len(nsInfo.routingExternalGWs.gws))
	copy(res.gws, nsInfo.routingExternalGWs.gws)
	return res
}

func (oc *Controller) getRoutingPodGWs(ns string) map[string]gatewayInfo {
	nsInfo := oc.getNamespaceLocked(ns)
	if nsInfo == nil {
		return nil
	}
	defer nsInfo.Unlock()
	// return a copy of the object so it can be handled without the
	// namespace locked
	res := make(map[string]gatewayInfo)
	for k, v := range nsInfo.routingExternalPodGWs {
		item := gatewayInfo{
			bfdEnabled: v.bfdEnabled,
			gws:        make([]net.IP, len(v.gws)),
		}
		copy(item.gws, v.gws)
		res[k] = item
	}
	return res
}

func (oc *Controller) addLogicalPort(pod *kapi.Pod) (err error) {
	logicalSwitch := pod.Spec.NodeName
	nodeSubnets := oc.lsManager.GetSwitchSubnets(logicalSwitch)
	if len(nodeSubnets) == 0 {
		// Was either a non-host-subnet node (eg, hybrid overlay) or
		// the node's switch wasn't set up yet. Try later.
		return fmt.Errorf("node switch had no assigned OVN subnets; trying again later")
	}

	// Keep track of how long syncs take.
	start := time.Now()
	defer func() {
		klog.Infof("[%s/%s] addLogicalPort took %v", pod.Namespace, pod.Name, time.Since(start))
	}()

	portName := podLogicalPortName(pod)
	klog.Infof("[%s/%s] creating logical port for pod on switch %s", pod.Namespace, pod.Name, logicalSwitch)

	var podMac net.HardwareAddr
	var podIfAddrs []*net.IPNet
	var cmds []*goovn.OvnCommand
	var addresses []string
	var cmd *goovn.OvnCommand
	var releaseIPs bool
	needsIP := true

	// Check if the pod's logical switch port already exists. If it
	// does don't re-add the port to OVN as this will change its
	// UUID and and the port cache, address sets, and port groups
	// will still have the old UUID.
	lsp, err := oc.ovnNBClient.LSPGet(portName)
	if err != nil && err != goovn.ErrorNotFound && err != goovn.ErrorSchema {
		return fmt.Errorf("unable to get the lsp: %s from the nbdb: %s", portName, err)
	}

	if lsp == nil {
		cmd, err = oc.ovnNBClient.LSPAdd(logicalSwitch, portName)
		if err != nil {
			return fmt.Errorf("unable to create the LSPAdd command for port: %s from the nbdb: %v", portName, err)
		}
		cmds = append(cmds, cmd)
	} else {
		klog.Infof("LSP already exists for port: %s", portName)
	}

	// Bind the port to the node's chassis; prevents ping-ponging between
	// chassis if ovnkube-node isn't running correctly and hasn't cleared
	// out iface-id for an old instance of this pod, and the pod got
	// rescheduled.
	opts, err := oc.ovnNBClient.LSPGetOptions(portName)
	if err != nil && err != goovn.ErrorNotFound {
		klog.Warningf("Failed to get options for port %s: %v", portName, err)
	}
	if opts == nil {
		opts = make(map[string]string)
	}
	opts["requested-chassis"] = pod.Spec.NodeName
	cmd, err = oc.ovnNBClient.LSPSetOptions(portName, opts)
	if err != nil {
		return fmt.Errorf("unable to create the LSPSetOptions command for port: %s from the nbdb: %v", portName, err)
	}
	cmds = append(cmds, cmd)

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
		cmd, err = oc.ovnNBClient.LSPSetDynamicAddresses(portName, "")
		if err != nil {
			return fmt.Errorf("unable to create LSPSetDynamicAddresses command for port: %s", portName)
		}
		cmds = append(cmds, cmd)

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
		var networks []*types.NetworkSelectionElement

		networks, err = util.GetPodNetSelAnnotation(pod, util.DefNetworkAnnotation)
		// handle error cases separately first to ensure binding to err, otherwise the
		// defer will fail
		if err != nil {
			return fmt.Errorf("error while getting custom MAC config for port %q from "+
				"default-network's network-attachment: %v", portName, err)
		} else if networks != nil && len(networks) != 1 {
			err = fmt.Errorf("invalid network annotation size while getting custom MAC config"+
				" for port %q", portName)
			return err
		}

		if networks != nil && networks[0].MacRequest != "" {
			klog.V(5).Infof("Pod %s/%s requested custom MAC: %s", pod.Namespace, pod.Name, networks[0].MacRequest)
			podMac, err = net.ParseMAC(networks[0].MacRequest)
			if err != nil {
				return fmt.Errorf("failed to parse mac %s requested in annotation for pod %s: Error %v",
					networks[0].MacRequest, pod.Name, err)
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
		var marshalledAnnotation map[string]string
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

	// set addresses on the port
	addresses = make([]string, len(podIfAddrs)+1)
	addresses[0] = podMac.String()
	for idx, podIfAddr := range podIfAddrs {
		addresses[idx+1] = podIfAddr.IP.String()
	}
	// LSP addresses in OVN are a single space-separated value
	cmd, err = oc.ovnNBClient.LSPSetAddress(portName, strings.Join(addresses, " "))
	if err != nil {
		return fmt.Errorf("unable to create LSPSetAddress command for port: %s", portName)
	}
	cmds = append(cmds, cmd)

	// add external ids
	extIds := map[string]string{"namespace": pod.Namespace, "pod": "true"}
	cmd, err = oc.ovnNBClient.LSPSetExternalIds(portName, extIds)
	if err != nil {
		return fmt.Errorf("unable to create LSPSetExternalIds command for port: %s", portName)
	}
	cmds = append(cmds, cmd)

	// execute all the commands together.
	err = oc.ovnNBClient.Execute(cmds...)
	if err != nil {
		return fmt.Errorf("error while creating logical port %s error: %v",
			portName, err)
	}

	lsp, err = oc.ovnNBClient.LSPGet(portName)
	if err != nil || lsp == nil {
		return fmt.Errorf("failed to get the logical switch port: %s from the ovn client, error: %s", portName, err)
	}

	// Add the pod's logical switch port to the port cache
	portInfo := oc.logicalPortCache.add(logicalSwitch, portName, lsp.UUID, podMac, podIfAddrs)

	// Ensure the namespace/nsInfo exists
	if err = oc.addPodToNamespace(pod.Namespace, portInfo); err != nil {
		return err
	}

	// add src-ip routes to GR if external gw annotation is set
	routingExternalGWs := oc.getRoutingExternalGWs(pod.Namespace)
	routingPodGWs := oc.getRoutingPodGWs(pod.Namespace)

	// if we have any external or pod Gateways, add routes
	gateways := make([]gatewayInfo, 0)

	if len(routingExternalGWs.gws) > 0 {
		gateways = append(gateways, routingExternalGWs)
	}
	for _, gw := range routingPodGWs {
		if len(gw.gws) > 0 {
			gateways = append(gateways, gw)
		} else {
			klog.Warningf("Found routingPodGW with no gateways ip set for namespace %s", pod.Namespace)
		}
	}

	if len(gateways) > 0 {
		err = oc.addGWRoutesForPod(gateways, podIfAddrs, pod.Namespace, pod.Spec.NodeName)
		if err != nil {
			return err
		}
	} else if config.Gateway.DisableSNATMultipleGWs {
		// Add NAT rules to pods if disable SNAT is set and does not have
		// namespace annotations to go through external egress router
		if err = oc.addPerPodGRSNAT(pod, podIfAddrs); err != nil {
			return err
		}
	}

	// check if this pod is serving as an external GW
	err = oc.addPodExternalGW(pod)
	if err != nil {
		return fmt.Errorf("failed to handle external GW check: %v", err)
	}

	// CNI depends on the flows from port security, delay setting it until end
	cmd, err = oc.ovnNBClient.LSPSetPortSecurity(portName, strings.Join(addresses, " "))
	if err != nil {
		return fmt.Errorf("unable to create LSPSetPortSecurity command for port: %s", portName)
	}

	err = oc.ovnNBClient.Execute(cmd)
	if err != nil {
		return fmt.Errorf("error while setting port security on port: %s error: %v",
			portName, err)
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
	podMac, podIPs, err := util.GetPortAddresses(portName, oc.ovnNBClient)
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

func exGatewayAnnotationsChanged(oldPod *podCacheEntry, newPod *kapi.Pod) bool {
	return oldPod.Annotations[routingNamespaceAnnotation] != newPod.Annotations[routingNamespaceAnnotation] ||
		oldPod.Annotations[routingNetworkAnnotation] != newPod.Annotations[routingNetworkAnnotation] ||
		oldPod.Annotations[bfdAnnotation] != newPod.Annotations[bfdAnnotation]
}

func networkStatusAnnotationsChanged(oldPod *podCacheEntry, newPod *kapi.Pod) bool {
	return oldPod.Annotations[nettypes.NetworkStatusAnnot] != newPod.Annotations[nettypes.NetworkStatusAnnot]
}
