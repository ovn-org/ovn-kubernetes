package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

//watchSmartNicPods watch updates for pod smart nic annotations
func (nc *ovnNodeController) watchSmartNicPods() {
	var retryPods sync.Map
	// servedPods tracks the pods that got a VF
	var servedPods sync.Map

	n := nc.node
	nc.podHandler = n.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Add for Pod: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
			if !util.PodWantsNetwork(pod) || pod.Status.Phase == kapi.PodRunning {
				return
			}
			on, _, err := util.IsNetworkOnPod(pod, nc.nadInfo, &nc.node.defaultNetAttachDefs)
			if err != nil || !on {
				// the Pod is not attached to this specific network
				return
			}
			if util.PodScheduled(pod) {
				// Is this pod created on same node where the smart NIC
				if n.name != pod.Spec.NodeName {
					return
				}

				vfRepName, err := nc.getVfRepName(pod)
				if err != nil {
					klog.Infof("Failed to get rep name, %s. retrying", err)
					retryPods.Store(pod.UID, true)
					return
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, true, true,
					nc.nadInfo.NetNameInfo)
				if err != nil {
					retryPods.Store(pod.UID, true)
					return
				}
				err = nc.addRepPort(pod, vfRepName, podInterfaceInfo)
				if err != nil {
					klog.Infof("Failed to add rep port, %s. retrying", err)
					retryPods.Store(pod.UID, true)
				} else {
					servedPods.Store(pod.UID, true)
				}
			} else {
				// Handle unscheduled pods later in UpdateFunc
				retryPods.Store(pod.UID, true)
				return
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			pod := newer.(*kapi.Pod)
			klog.Infof("Update for Pod: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
			if !util.PodWantsNetwork(pod) || pod.Status.Phase == kapi.PodRunning {
				retryPods.Delete(pod.UID)
				return
			}
			on, _, err := util.IsNetworkOnPod(pod, nc.nadInfo, &nc.node.defaultNetAttachDefs)
			if err != nil || !on {
				retryPods.Delete(pod.UID)
				return
			}
			_, retry := retryPods.Load(pod.UID)
			if util.PodScheduled(pod) && retry {
				if n.name != pod.Spec.NodeName {
					retryPods.Delete(pod.UID)
					return
				}
				vfRepName, err := nc.getVfRepName(pod)
				if err != nil {
					klog.Infof("Failed to get rep name, %s. retrying", err)
					return
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, true, true,
					nc.nadInfo.NetNameInfo)
				if err != nil {
					return
				}
				err = nc.addRepPort(pod, vfRepName, podInterfaceInfo)
				if err != nil {
					klog.Infof("Failed to add rep port, %s. retrying", err)
				} else {
					servedPods.Store(pod.UID, true)
					retryPods.Delete(pod.UID)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Delete for Pod: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
			if _, ok := servedPods.Load(pod.UID); !ok {
				return
			}
			servedPods.Delete(pod.UID)
			retryPods.Delete(pod.UID)
			vfRepName, err := nc.getVfRepName(pod)
			if err != nil {
				klog.Errorf("Failed to get VF Representor Name from Pod: %s. Representor port may have been deleted.", err)
				return
			}
			err = nc.delRepPort(vfRepName)
			if err != nil {
				klog.Errorf("Failed to delete VF representor %s. %s", vfRepName, err)
			}
		},
	}, nil)
}

// getVfRepName returns the VF's representor of the VF assigned to the pod
func (nc *ovnNodeController) getVfRepName(pod *kapi.Pod) (string, error) {
	smartNicCD, err := util.UnmarshalPodSmartNicConnDetails(pod.Annotations, nc.nadInfo.NetName)
	if err != nil {
		return "", fmt.Errorf("failed to get smart-nic annotation for pod %s/%s network %sL %v",
			pod.Namespace, pod.Name, nc.nadInfo.NetName, err)
	}

	return util.GetSriovnetOps().GetVfRepresentorSmartNIC(smartNicCD.PfId, smartNicCD.VfId)
}

// updatePodSmartNicConnDetailsWithRetry update the pod annotion with the givin connection details
func (nc *ovnNodeController) updatePodSmartNicConnStatusWithRetry(kube kube.Interface, pod *kapi.Pod,
	smartNicConnStatus *util.SmartNICConnectionStatus) error {
	resultErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		cpod := pod.DeepCopy()
		err := util.MarshalPodSmartNicConnStatus(&cpod.Annotations, smartNicConnStatus, nc.nadInfo.NetName)
		if err != nil {
			return err
		}
		return kube.UpdatePod(cpod)
	})
	if resultErr != nil {
		return fmt.Errorf("failed to update %s annotation on pod %s/%s for network %s: %v",
			util.SmartNicConnetionStatusAnnot, pod.Namespace, pod.Name, nc.nadInfo.NetName, resultErr)
	}
	return nil
}

// addRepPort adds the representor of the VF to the ovs bridge
func (nc *ovnNodeController) addRepPort(pod *kapi.Pod, vfRepName string, ifInfo *cni.PodInterfaceInfo) error {
	klog.Infof("Adding VF representor for pod %s/%s network %s", pod.Namespace, pod.Name, nc.nadInfo.NetName)
	smartNicCD, err := util.UnmarshalPodSmartNicConnDetails(pod.Annotations, nc.nadInfo.NetName)
	if err != nil {
		return fmt.Errorf("failed to get smart-nic annotation. %v", err)
	}

	err = cni.ConfigureOVS(context.TODO(), pod.Namespace, pod.Name, vfRepName, ifInfo, smartNicCD.SandboxId)
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
	connStatus := util.SmartNICConnectionStatus{Status: util.SmartNicConnectionStatusReady, Reason: ""}
	err = nc.updatePodSmartNicConnStatusWithRetry(nc.node.Kube, pod, &connStatus)
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
