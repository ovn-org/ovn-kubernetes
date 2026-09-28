// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dpu

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// Controller manages the DPU representor port lifecycle for pods across all
// networks, default and user-defined.
type Controller struct {
	kube       kube.Interface
	networkMgr networkmanager.Interface
	ovsClient  libovsdbclient.Client

	// nodeName is the Kubernetes node whose pods this DPU serves.
	nodeName string

	podController controller.Controller
	podLister     corelisters.PodLister
	clientSet     cni.PodInfoGetter

	nadReconciler   controller.Reconciler
	nadReconcilerID uint64

	// podStates tracks the DPU connection state of the pods this DPU serves,
	// keyed by pod key (namespace/name).
	podStates *syncmap.SyncMap[*podDPUState]

	// nadPods holds the pods that reference each NAD, so that a NAD event can
	// requeue them without listing every pod in the cluster.
	nadPods nadPodIndex
}

func NewController(
	wf factory.NodeWatchFactory,
	kube kube.Interface,
	kclient kubernetes.Interface,
	networkMgr networkmanager.Interface,
	ovsClient libovsdbclient.Client,
	nodeName string,
) *Controller {
	podLister := corelisters.NewPodLister(wf.PodInformer().GetIndexer())

	c := &Controller{
		kube:       kube,
		networkMgr: networkMgr,
		ovsClient:  ovsClient,
		podLister:  podLister,
		clientSet:  cni.NewClientSet(kclient, podLister),
		nodeName:   nodeName,
		podStates:  syncmap.NewSyncMap[*podDPUState](),
	}

	c.podController = controller.NewController("dpu-pod-controller",
		&controller.ControllerConfig[corev1.Pod]{
			RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:      c.reconcileDPUPod,
			ObjNeedsUpdate: c.dpuPodNeedsUpdate,
			Threadiness:    1,
			// Giving up on a pod would leak its representor until the pod is
			// deleted or ovnkube restarts.
			MaxAttempts: controller.InfiniteAttempts,
			Informer:    wf.PodInformer(),
			Lister:      podLister.List,
		})

	c.nadReconciler = controller.NewReconciler("dpu-nad-reconciler",
		&controller.ReconcilerConfig{
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
			Reconcile:   c.reconcileNAD,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		})

	return c
}

func (c *Controller) Start() error {
	klog.Info("Starting DPU pod controller")

	c.nadReconcilerID = c.networkMgr.RegisterNADReconciler(c.nadReconciler)
	return controller.StartWithInitialSync(c.bootstrapDPUPodMapFromOVS, c.podController, c.nadReconciler)
}

func (c *Controller) Stop() {
	klog.Info("Stopping DPU pod controller")
	if c.nadReconcilerID != 0 {
		c.networkMgr.DeRegisterNADReconciler(c.nadReconcilerID)
	}
	controller.Stop(c.podController)
	controller.Stop(c.nadReconciler)
}

func (c *Controller) dpuPodNeedsUpdate(oldPod, newPod *corev1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return true
	}
	if oldPod.Spec.NodeName == newPod.Spec.NodeName && newPod.Spec.NodeName != c.nodeName {
		return false
	}
	// Reconcile on connection-details changes, on the pod getting scheduled, and
	// on the OVN annotation, which carries the MAC the representor is configured
	// with and can land after the connection details.
	return oldPod.Annotations[util.DPUConnectionDetailsAnnot] != newPod.Annotations[util.DPUConnectionDetailsAnnot] ||
		oldPod.Annotations[types.OvnPodAnnotationName] != newPod.Annotations[types.OvnPodAnnotationName] ||
		oldPod.Spec.NodeName != newPod.Spec.NodeName
}

// reconcileDPUPod reconciles the DPU state of a single pod. The key is
// namespace/name from the workqueue, which is also the podStates key.
func (c *Controller) reconcileDPUPod(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("Failed to split meta namespace cache key %s: %v", key, err)
		return nil
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		pod = nil
	}

	// The pod informer is cluster-wide in DPU mode. Neither a pod of another
	// node nor a host-network pod has a representor here, so both are treated
	// as a deletion.
	if pod != nil && (pod.Spec.NodeName != c.nodeName || util.PodWantsHostNetwork(pod)) {
		pod = nil
	}

	return c.podStates.DoWithLock(key, func(key string) error {
		if pod == nil {
			c.nadPods.delete(key)
			return c.handleDeleteDPUPod(ctx, key, nil)
		}

		// If the UID changed (pod recreated with same name), clean up the old pod's state first
		if ps, ok := c.podStates.Load(key); ok && ps.uid != pod.UID {
			if err := c.handleDeleteDPUPod(ctx, key, pod); err != nil {
				return err
			}
		}

		return c.handleAddOrUpdateDPUPod(ctx, key, pod, c.clientSet)
	})
}

// reconcileNAD is called by the NAD reconciler when a NAD is added, updated or
// deleted. It only requeues the pods that reference the NAD, leaving
// reconcileDPUPod as the sole writer of representors and annotations.
func (c *Controller) reconcileNAD(_ context.Context, nadName string) error {
	podKeys := c.nadPods.pods(nadName)
	if len(podKeys) > 0 {
		klog.Infof("NAD %s changed, requeueing pods %v", nadName, podKeys)
	}
	for _, podKey := range podKeys {
		c.podController.Reconcile(podKey)
	}

	return nil
}

// bootstrapDPUPodMapFromOVS pre-populates podStates from existing OVS representor
// ports, and removes orphaned representor ports whose pods no longer exist.
func (c *Controller) bootstrapDPUPodMapFromOVS() error {
	var errs []error
	p := func(item *vswitchd.Interface) bool {
		return item.ExternalIDs["sandbox"] != "" && item.ExternalIDs["vf-netdev-name"] != ""
	}

	ovsIfaces, err := ovsops.FindInterfacesWithPredicate(c.ovsClient, p)
	if err != nil {
		return fmt.Errorf("failed to find representor interfaces during DPU bootstrap: %w", err)
	}
	if len(ovsIfaces) == 0 {
		return nil
	}

	// readyNADs holds, per pod on this node, the NADs it already reports ready.
	// Its keys are also what says a pod still exists.
	readyNADs := make(map[k8stypes.UID]sets.Set[string])
	pods, err := c.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods for DPU bootstrap: %w", err)
	}
	for _, pod := range pods {
		if pod.Spec.NodeName != c.nodeName {
			continue
		}
		ready := sets.New[string]()
		currentStatus, err := util.UnmarshalPodDPUConnStatusAllNetworks(pod.Annotations)
		if err != nil {
			klog.Warningf("DPU bootstrap: cannot read the connection status of pod %s/%s, "+
				"configuring its representors again: %v", pod.Namespace, pod.Name, err)
		}
		for nadKey, status := range currentStatus {
			if status.Status == util.DPUConnectionStatusReady {
				ready.Insert(nadKey)
			}
		}
		readyNADs[pod.UID] = ready
	}

	var orphanedPorts []string
	for _, ovsIface := range ovsIfaces {
		podUID := ovsIface.ExternalIDs["iface-id-ver"]
		sandbox := ovsIface.ExternalIDs["sandbox"]
		repName := ovsIface.ExternalIDs["vf-netdev-name"]
		ifaceID := ovsIface.ExternalIDs["iface-id"]
		nadKey := ovsIface.ExternalIDs[types.NADExternalID]
		if nadKey == "" {
			nadKey = types.DefaultNetworkName
		}

		if podUID == "" || sandbox == "" || repName == "" || ifaceID == "" {
			continue
		}

		uid := k8stypes.UID(podUID)

		ready, podExists := readyNADs[uid]
		if !podExists {
			klog.Infof("DPU bootstrap: removing orphaned representor %s (pod UID %s, NAD %s): pod no longer exists", repName, podUID, nadKey)
			orphanedPorts = append(orphanedPorts, repName)
			continue
		}

		podKey := podKeyFromIfaceID(ifaceID, nadKey)
		if podKey == "" {
			klog.Warningf("DPU bootstrap: cannot derive pod key from iface-id %q, leaving representor %s untracked", ifaceID, repName)
			continue
		}

		// No key lock needed: this runs as the initial sync, before the
		// controller workers start.
		ps, _ := c.podStates.LoadOrStore(podKey, &podDPUState{
			uid:       uid,
			nadStates: make(map[string]*dpuConnectionState),
		})
		// A pod already reported ready went through a complete ConfigureOVS,
		// since that is the last thing addRepPort does. Anything else may have
		// been interrupted part way and has to be configured again.
		ps.nadStates[nadKey] = &dpuConnectionState{
			vfRepName:  repName,
			sandboxId:  sandbox,
			revalidate: !ready.Has(nadKey),
		}
	}

	if len(orphanedPorts) > 0 {
		setRepPortInterfacesDown(orphanedPorts)
		if err := libovsdbops.DeleteMultiplePortsWithInterfaces(c.ovsClient, "br-int", orphanedPorts...); err != nil {
			errs = append(errs, fmt.Errorf("DPU bootstrap: failed to delete orphaned representor ports %v: %w", orphanedPorts, err))
		} else {
			klog.Infof("DPU bootstrap: deleted orphaned representor ports %v from br-int", orphanedPorts)
		}
	}

	klog.Infof("DPU bootstrap: populated state from OVS representor ports")
	return utilerrors.Join(errs...)
}
