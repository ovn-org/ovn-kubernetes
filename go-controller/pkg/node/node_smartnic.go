package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

//watchSmartNicPods watch updates for pod smart nic annotations
func (n *OvnNode) watchSmartNicPods(isOvnUpEnabled bool) {
	var retryPods sync.Map
	// servedPods tracks the pods that got a VF
	var servedPods sync.Map

	_ = n.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("Add for Pod: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
			if !util.PodWantsNetwork(pod) || pod.Status.Phase == kapi.PodRunning {
				return
			}
			if util.PodScheduled(pod) {
				// Is this pod created on same node where the smart NIC
				if n.name != pod.Spec.NodeName {
					return
				}

				vfRepName, err := n.getVfRepName(pod)
				if err != nil {
					klog.Infof("Failed to get rep name, %s. retrying", err)
					retryPods.Store(pod.UID, true)
					return
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, isOvnUpEnabled, true)
				if err != nil {
					retryPods.Store(pod.UID, true)
					return
				}
				err = n.addRepPort(pod, vfRepName, podInterfaceInfo)
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
			_, retry := retryPods.Load(pod.UID)
			if util.PodScheduled(pod) && retry {
				if n.name != pod.Spec.NodeName {
					retryPods.Delete(pod.UID)
					return
				}
				vfRepName, err := n.getVfRepName(pod)
				if err != nil {
					klog.Infof("Failed to get rep name, %s. retrying", err)
					return
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, isOvnUpEnabled, true)
				if err != nil {
					return
				}
				err = n.addRepPort(pod, vfRepName, podInterfaceInfo)
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
			vfRepName, err := n.getVfRepName(pod)
			if err != nil {
				klog.Errorf("Failed to get VF Representor Name from Pod: %s. Representor port may have been deleted.", err)
				return
			}
			err = n.delRepPort(vfRepName)
			if err != nil {
				klog.Errorf("Failed to delete VF representor %s. %s", vfRepName, err)
			}
		},
	}, nil)
}

// getVfRepName returns the VF's representor of the VF assigned to the pod
func (n *OvnNode) getVfRepName(pod *kapi.Pod) (string, error) {
	smartNicCD := util.SmartNICConnectionDetails{}
	if err := smartNicCD.FromPodAnnotation(pod.Annotations); err != nil {
		return "", fmt.Errorf("failed to get smart-nic annotation. %v", err)
	}
	return util.GetSriovnetOps().GetVfRepresentorSmartNIC(smartNicCD.PfId, smartNicCD.VfId)
}

// addRepPort adds the representor of the VF to the ovs bridge
func (n *OvnNode) addRepPort(pod *kapi.Pod, vfRepName string, ifInfo *cni.PodInterfaceInfo) error {
	klog.Infof("Adding VF representor %s", vfRepName)
	smartNicCD := util.SmartNICConnectionDetails{}
	if err := smartNicCD.FromPodAnnotation(pod.Annotations); err != nil {
		return fmt.Errorf("failed to get smart-nic annotation. %v", err)
	}

	err := cni.ConfigureOVS(context.TODO(), pod.Namespace, pod.Name, vfRepName, ifInfo, smartNicCD.SandboxId)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = n.delRepPort(vfRepName)
		return err
	}
	klog.Infof("Port %s added to bridge br-int", vfRepName)

	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to get link device for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set MTU for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set link up for interface %s", vfRepName)
	}

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	connStatus := util.SmartNICConnectionStatus{Status: util.SmartNicConnectionStatusReady, Reason: ""}
	podAnnotator := kube.NewPodAnnotator(n.Kube, pod)
	err = connStatus.SetPodAnnotation(podAnnotator)
	if err != nil {
		// we should not get here
		_ = util.GetNetLinkOps().LinkSetDown(link)
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set pod annotations. %v", err)
	}

	err = podAnnotator.Run()
	if err != nil {
		// cleanup
		_ = util.GetNetLinkOps().LinkSetDown(link)
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set pod annotations. %v", err)
	}
	return nil
}

// delRepPort delete the representor of the VF from the ovs bridge
func (n *OvnNode) delRepPort(vfRepName string) error {
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
