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

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (nc *ovnNodeController) addDPUPod4Nad(pod *kapi.Pod, isOvnUpEnabled bool, nadName string,
	podLister corev1listers.PodLister, kclient kubernetes.Interface) error {
	podDesc := fmt.Sprintf("pod %s/%s on network %s", pod.Namespace, pod.Name, nadName)
	vfRepName, err := nc.getVfRepName(pod, nadName)
	if err != nil {
		klog.Infof("Failed to get rep name of %s, %s. retrying", podDesc, err)
		return err
	}
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, isOvnUpEnabled, string(pod.UID), "",
		nadName, 0, nc.nadInfo.NetNameInfo)
	if err != nil {
		klog.Infof("Failed to get pod interface information of %s: %v. retrying", podDesc, err)
		return err
	}
	err = nc.addRepPort(pod, vfRepName, podInterfaceInfo, podLister, kclient)
	if err != nil {
		klog.Infof("Failed to add rep port for %s, %s. retrying", podDesc, err)
	}
	return err
}

func (nc *ovnNodeController) addDPUPods(pod *kapi.Pod, isOvnUpEnabled bool,
	podLister corev1listers.PodLister, kclient kubernetes.Interface, retryCache, servedCache map[string]bool) {
	if util.PodScheduled(pod) {
		for nadName := range retryCache {
			klog.Infof("Add DPU for pod %s/%s nad %s", pod.Namespace, pod.Name, nadName)

			err := nc.addDPUPod4Nad(pod, isOvnUpEnabled, nadName, podLister, kclient)
			if err == nil {
				delete(retryCache, nadName)
				servedCache[nadName] = true
			}
		}
	}
}

//watchPodsDPU watch updates for pod dpu annotations
func (nc *ovnNodeController) watchPodsDPU(isOvnUpEnabled bool) {
	var retryPods sync.Map
	// servedPods tracks the pods that got a VF
	var servedPods sync.Map

	n := nc.node
	podLister := corev1listers.NewPodLister(n.watchFactory.LocalPodInformer().GetIndexer())
	kclient := n.Kube.(*kube.Kube)

	nc.podHandler = n.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Add for Pod: %s/%s for nad %s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			// Is this pod based on hostNetwork or it is created on different node where the DPU
			if !util.PodWantsNetwork(pod) || (util.PodScheduled(pod) && n.name != pod.Spec.NodeName) {
				return
			}
			on, networkMap, err := util.IsNetworkOnPod(pod, nc.nadInfo)
			if err != nil || !on {
				// the Pod is not attached to this specific network
				klog.V(5).Infof("Pod %s/%s is not attached on this network: %s", pod.Namespace, pod.Name, nc.nadInfo.NetName)
				return
			}
			// add all the Pod's needed Nad into Pod's retryCache
			retryCache := map[string]bool{}
			for nadName := range networkMap {
				retryCache[nadName] = true
			}
			retryPods.Store(pod.UID, retryCache)
			servedCache := map[string]bool{}
			servedPods.Store(pod.UID, servedCache)
			nc.addDPUPods(pod, isOvnUpEnabled, podLister, kclient.KClient, retryCache, servedCache)
			retryPods.Store(pod.UID, retryCache)
			servedPods.Store(pod.UID, servedCache)
		},
		UpdateFunc: func(old, newer interface{}) {
			pod := newer.(*kapi.Pod)
			klog.Infof("Update for Pod: %s/%s for network", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			if !util.PodWantsNetwork(pod) || (util.PodScheduled(pod) && n.name != pod.Spec.NodeName) {
				retryPods.Delete(pod.UID)
				servedPods.Delete(pod.UID)
				return
			}
			retryCache := map[string]bool{}
			v, ok := retryPods.Load(pod.UID)
			if ok {
				retryCache = v.(map[string]bool)
			}
			if len(retryCache) == 0 {
				return
			}
			servedCache := map[string]bool{}
			v, ok = servedPods.Load(pod.UID)
			if ok {
				servedCache = v.(map[string]bool)
			}
			nc.addDPUPods(pod, isOvnUpEnabled, podLister, kclient.KClient, retryCache, servedCache)
			retryPods.Store(pod.UID, retryCache)
			servedPods.Store(pod.UID, servedCache)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Delete for Pod: %s/%s for network %s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName(), nc.nadInfo.NetName)
			retryPods.Delete(pod.UID)
			v, ok := servedPods.Load(pod.UID)
			if !ok {
				return
			}
			servedCache := v.(map[string]bool)
			servedPods.Delete(pod.UID)
			for nadName := range servedCache {
				klog.Infof("Delelete DPU for pod %s/%s nad %s", pod.Namespace, pod.Name, nadName)
				vfRepName, err := nc.getVfRepName(pod, nadName)
				if err != nil {
					klog.Errorf("Failed to get VF Representor Name for Pod %s/%s: %s. Representor port may have been deleted.",
						pod.Namespace, pod.Name, err)
					continue
				}
				err = nc.delRepPort(vfRepName)
				if err != nil {
					klog.Errorf("Failed to delete VF representor %s for pod %s/%s. %s", vfRepName, pod.Namespace, pod.Name, err)
				}
			}
		},
	}, nil)
}

// getVfRepName returns the VF's representor of the VF assigned to the pod
func (nc *ovnNodeController) getVfRepName(pod *kapi.Pod, nadName string) (string, error) {
	klog.V(5).Infof("Get VF representor name of pod %s/%s of nad %s. annoations: %v", pod.Namespace, pod.Name,
		nadName, pod.Annotations[util.DPUConnectionDetailsAnnot])
	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, nadName)
	if err != nil {
		return "", fmt.Errorf("failed to get dpu annotation for pod %s/%s nad %s: %v", pod.Namespace, pod.Name, nadName, err)
	}
	return util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
}

// updatePodDPUConnStatusWithRetry update the pod annotion with the givin connection details
func (nc *ovnNodeController) updatePodDPUConnStatusWithRetry(kube kube.Interface, origPod *kapi.Pod,
	dpuConnStatus *util.DPUConnectionStatus, nadName string) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := kube.GetPod(origPod.Namespace, origPod.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		cpod := pod.DeepCopy()
		err = util.MarshalPodDPUConnStatus(&cpod.Annotations, dpuConnStatus, nadName)
		if err != nil {
			return err
		}
		return kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update %s annotation on pod %s/%s for nad %s: %v",
			util.DPUConnetionStatusAnnot, origPod.Namespace, origPod.Name, nadName, resultErr)
	}
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge
func (nc *ovnNodeController) addRepPort(pod *kapi.Pod, vfRepName string, ifInfo *cni.PodInterfaceInfo, podLister corev1listers.PodLister, kclient kubernetes.Interface) error {
	klog.Infof("Adding VF representor %s for pod %s/%s nad %s", vfRepName, pod.Namespace, pod.Name, ifInfo.NadName)
	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, ifInfo.NadName)
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
	err = nc.updatePodDPUConnStatusWithRetry(nc.node.Kube, pod, &connStatus, ifInfo.NadName)
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
