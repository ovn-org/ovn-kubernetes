package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	netattchdefapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// watchPodsDPU watch updates for pod dpu annotations
func (nc *ovnNodeController) watchPodsDPU(isOvnUpEnabled bool) error {
	// servedPods tracks the pods that already successfully got a VF,
	// key is pod.UUID, value is networkMap of map[string]bool
	var servedPods sync.Map
	// podNadCache stores all the net-attach-defs that the given Pod is attached for this network,
	// we assume that Pod's Network Attachment Selection Annotation will not change over time.
	// key is pod.UUID, value is networkMap of map[string]*NetworkSelectionElement type
	var podNadCache sync.Map

	// when specific nad is in podNadCache but not in servedPod cache, we'd need to retry

	klog.Infof("Starting Pod watch on network %s", nc.nadInfo.NetName)

	podLister := corev1listers.NewPodLister(nc.watchFactory.LocalPodInformer().GetIndexer())
	kclient := nc.Kube.(*kube.Kube)

	podHandler, err := nc.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Add for Pod: %s/%s for network", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			if !util.PodWantsNetwork(pod) || pod.Status.Phase == kapi.PodRunning {
				return
			}
			on, networkMap, err := util.IsNetworkOnPod(pod, nc.nadInfo)
			if err != nil || !on {
				// the Pod is not attached to this specific network
				klog.V(5).Infof("Skipping add for Pod %s/%s as it is not attached to network: %s",
					pod.Namespace, pod.Name, nc.nadInfo.NetName)
				return
			}
			// add all the Pod's Nad into Pod's podNadCache
			podNadCache.Store(pod.UID, networkMap)

			if nc.name != pod.Spec.NodeName {
				return
			}

			// initialize serverCache to be empty
			servedCache := map[string]bool{}
			if util.PodScheduled(pod) {
				// Is this pod created on same node as the DPU
				for nadName := range networkMap {
					vfRepName, err := nc.getVfRepName(pod, nadName)
					if err != nil {
						klog.Infof("Failed to get rep name, %s. retrying", err)
						continue
					}
					podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, isOvnUpEnabled, string(pod.UID), "", nadName, nc.nadInfo.MTU, nc.nadInfo.NetNameInfo)
					if err != nil {
						continue
					}
					err = nc.addRepPort(pod, vfRepName, podInterfaceInfo, podLister, kclient.KClient)
					if err != nil {
						klog.Infof("Failed to add rep port, %s. retrying", err)
					} else {
						servedCache[nadName] = true
					}
				}
			}
			servedPods.Store(pod.UID, servedCache)
		},
		UpdateFunc: func(old, newer interface{}) {
			pod := newer.(*kapi.Pod)
			klog.Infof("Update for Pod: %s/%s for network %s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			if !util.PodWantsNetwork(pod) || pod.Status.Phase == kapi.PodRunning ||
				(util.PodScheduled(pod) && nc.name != pod.Spec.NodeName) {
				// no more retry
				podNadCache.Delete(pod.UID)
				return
			}
			if !util.PodScheduled(pod) {
				return
			}
			v, ok := podNadCache.Load(pod.UID)
			if !ok {
				return
			}
			networkMap := v.(map[string]*netattchdefapi.NetworkSelectionElement)

			servedCache := map[string]bool{}
			v, ok = servedPods.Load(pod.UID)
			if ok {
				servedCache = v.(map[string]bool)
			}

			for nadName := range networkMap {
				_, ok := servedCache[nadName]
				if ok {
					continue
				}
				vfRepName, err := nc.getVfRepName(pod, nadName)
				if err != nil {
					klog.Infof("Failed to get rep name, %s. retrying", err)
					continue
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, isOvnUpEnabled, string(pod.UID), "", nadName, nc.nadInfo.MTU, nc.nadInfo.NetNameInfo)
				if err != nil {
					continue
				}
				err = nc.addRepPort(pod, vfRepName, podInterfaceInfo, podLister, kclient.KClient)
				if err != nil {
					klog.Infof("Failed to add rep port, %s. retrying", err)
				} else {
					servedCache[nadName] = true
				}
			}
			servedPods.Store(pod.UID, servedCache)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Delete for Pod: %s/%s for network %s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			podNadCache.Delete(pod.UID)
			v, ok := servedPods.Load(pod.UID)
			if !ok {
				return
			}
			servedCache := v.(map[string]*util.DPUConnectionDetails)
			servedPods.Delete(pod.UID)
			for nadName := range servedCache {
				podDesc := fmt.Sprintf("pod %s/%s for nad %s", pod.Namespace, pod.Name, nadName)
				klog.Infof("Deleting %s from DPU", podDesc)
				vfRepName, err := nc.getVfRepName(pod, nadName)
				if err != nil {
					klog.Errorf("Failed to get VF Representor Name from Pod: %s. Representor port may have been deleted.", err)
					return
				}
				err = nc.delRepPort(vfRepName)
				if err != nil {
					klog.Errorf("Failed to delete VF representor %s. %s", vfRepName, err)
				}
			}
		},
	}, nil)

	if err == nil {
		nc.podHandler = podHandler
	}
	return err
}

// getVfRepName returns the VF's representor of the VF assigned to the pod
func (nc *ovnNodeController) getVfRepName(pod *kapi.Pod, nadName string) (string, error) {
	annoNadKeyName := util.GetAnnotationKeyFromNadName(nadName, !nc.nadInfo.IsSecondary)
	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, annoNadKeyName)
	if err != nil {
		return "", fmt.Errorf("failed to get dpu annotation for pod %s/%s nad %s: %v", pod.Namespace, pod.Name, nadName, err)
	}
	return util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
}

// updatePodDPUConnStatusWithRetry update the pod annotion with the givin connection details
func (nc *ovnNodeController) updatePodDPUConnStatusWithRetry(origPod *kapi.Pod,
	dpuConnStatus *util.DPUConnectionStatus, annoNadKeyName string) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := nc.watchFactory.GetPod(origPod.Namespace, origPod.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		cpod := pod.DeepCopy()
		cpod.Annotations, err = util.MarshalPodDPUConnStatus(cpod.Annotations, dpuConnStatus, annoNadKeyName)
		if err != nil {
			return err
		}
		return nc.Kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update %s annotation on pod %s/%s for nad %s: %v",
			util.DPUConnetionStatusAnnot, origPod.Namespace, origPod.Name, annoNadKeyName, resultErr)
	}
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge
func (nc *ovnNodeController) addRepPort(pod *kapi.Pod, vfRepName string, ifInfo *cni.PodInterfaceInfo, podLister corev1listers.PodLister, kclient kubernetes.Interface) error {
	klog.Infof("Adding VF representor %s for pod %s/%s nad %s", vfRepName, pod.Namespace, pod.Name, ifInfo.NadName)
	annoNadKeyName := util.GetAnnotationKeyFromNadName(ifInfo.NadName, ifInfo.IsSecondary)
	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, annoNadKeyName)
	if err != nil {
		return fmt.Errorf("failed to get dpu annotation. %v", err)
	}

	err = cni.ConfigureOVS(context.TODO(), pod.Namespace, pod.Name, vfRepName, ifInfo, dpuCD.SandboxId, podLister, kclient)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = nc.delRepPort(vfRepName)
		return err
	}
	klog.Infof("Port %s added to bridge br-int", vfRepName)

	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = nc.delRepPort(vfRepName)
		return fmt.Errorf("failed to get link device for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
		_ = nc.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set MTU %d for interface %s: %v", ifInfo.MTU, vfRepName, err)
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		_ = nc.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set link up for interface %s: %v", vfRepName, err)
	}

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	connStatus := util.DPUConnectionStatus{Status: util.DPUConnectionStatusReady, Reason: ""}
	err = nc.updatePodDPUConnStatusWithRetry(pod, &connStatus, annoNadKeyName)
	if err != nil {
		_ = util.GetNetLinkOps().LinkSetDown(link)
		_ = nc.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set pod annotations. %v", err)
	}
	return nil
}

// delRepPort delete the representor of the VF from the ovs bridge
func (nc *ovnNodeController) delRepPort(vfRepName string) error {
	//TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
	klog.Infof("Delete VF representor %s port", vfRepName)
	// Set link down for representor port
	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		klog.Warningf("Failed to get link device for representor port %s. %v", vfRepName, err)
	} else {
		if linkDownErr := util.GetNetLinkOps().LinkSetDown(link); linkDownErr != nil {
			klog.Warningf("Failed to set link down for representor port %s. %v", vfRepName, linkDownErr)
		}
	}

	// remove from br-int
	return wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		_, _, err := util.RunOVSVsctl("--if-exists", "del-port", "br-int", vfRepName)
		if err != nil {
			return false, nil
		}
		klog.Infof("Port %s deleted from bridge br-int", vfRepName)
		return true, nil
	})
}
