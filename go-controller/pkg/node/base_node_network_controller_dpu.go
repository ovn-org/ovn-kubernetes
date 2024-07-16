package node

import (
	"context"
	"fmt"
	"time"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

// Check if the Pod is ready so that we can add its associated DPU to br-int.
// If true, return its dpuConnDetails, otherwise return nil
func (bnnc *BaseNodeNetworkController) podReadyToAddDPU(pod *kapi.Pod, nadName string) *util.DPUConnectionDetails {
	if bnnc.name != pod.Spec.NodeName {
		klog.V(5).Infof("Pod %s/%s is not scheduled on this node %s", pod.Namespace, pod.Name, bnnc.name)
		return nil
	}

	dpuCD, err := util.UnmarshalPodDPUConnDetails(pod.Annotations, nadName)
	if err != nil {
		if !util.IsAnnotationNotSetError(err) {
			klog.Errorf("Failed to get DPU annotation for pod %s/%s NAD %s: %v",
				pod.Namespace, pod.Name, nadName, err)
		} else {
			klog.V(5).Infof("DPU connection details annotation still not found for %s/%s for NAD %s",
				pod.Namespace, pod.Name, nadName)
		}
		return nil
	}

	return dpuCD
}

func (bnnc *BaseNodeNetworkController) addDPUPodForNAD(pod *kapi.Pod, dpuCD *util.DPUConnectionDetails,
	netName, nadName string, getter cni.PodInfoGetter) error {
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
	klog.Infof("Adding %s on DPU", podDesc)
	podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations, nil,
		string(pod.UID), "", nadName, netName, config.Default.MTU)
	if err != nil {
		return fmt.Errorf("failed to get pod interface information of %s: %v. retrying", podDesc, err)
	}
	err = bnnc.addRepPort(pod, dpuCD, podInterfaceInfo, getter)
	if err != nil {
		return fmt.Errorf("failed to add rep port for %s, %v. retrying", podDesc, err)
	}
	return nil
}

func (bnnc *BaseNodeNetworkController) delDPUPodForNAD(pod *kapi.Pod, dpuCD *util.DPUConnectionDetails, nadName string, podDeleted bool) error {
	var errs []error
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
	klog.Infof("Deleting %s from DPU", podDesc)

	if podDeleted {
		// no need to unset connection status annotation if pod is deleted anyway
		err := bnnc.updatePodDPUConnStatusWithRetry(pod, nil, nadName)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to remove the old DPU connection status annotation for %s: %v", podDesc, err))
		}
	}
	vfRepName, err := util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to get old VF representor for %s, dpuConnDetail %+v Representor port may have been deleted: %v", podDesc, dpuCD, err))
	} else {
		err = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VF representor for %s: %v", podDesc, err))
		}
	}
	return utilerrors.Join(errs...)
}

func dpuConnectionDetailChanged(oldDPUCD, newDPUCD *util.DPUConnectionDetails) bool {
	if oldDPUCD == nil && newDPUCD == nil {
		return false
	}
	if (oldDPUCD != nil && newDPUCD == nil) || (oldDPUCD == nil && newDPUCD != nil) {
		return true
	}
	if oldDPUCD.PfId != newDPUCD.PfId ||
		oldDPUCD.VfId != newDPUCD.VfId || oldDPUCD.SandboxId != newDPUCD.SandboxId {
		return true
	}
	return false
}

// watchPodsDPU watch updates for pod DPU annotations
func (bnnc *BaseNodeNetworkController) watchPodsDPU() (*factory.Handler, error) {
	clientSet := cni.NewClientSet(bnnc.client, corev1listers.NewPodLister(bnnc.watchFactory.LocalPodInformer().GetIndexer()))

	netName := bnnc.GetNetworkName()
	return bnnc.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.V(5).Infof("Add for Pod: %s/%s for network %s", pod.Namespace, pod.Name, netName)
			if util.PodWantsHostNetwork(pod) {
				return
			}

			// add all the Pod's NADs into Pod's nadToDPUCDMap
			// For default network, NAD name is DefaultNetworkName.
			nadToDPUCDMap := map[string]*util.DPUConnectionDetails{}
			if bnnc.IsSecondary() {
				on, networkMap, err := util.GetPodNADToNetworkMapping(pod, bnnc.GetNetInfo())
				if err != nil || !on {
					if err != nil {
						// configuration error, no need to retry, do not return error
						klog.Errorf("Error getting network-attachment for pod %s/%s network %s: %v",
							pod.Namespace, pod.Name, bnnc.GetNetworkName(), err)
					} else {
						klog.V(5).Infof("Skipping Pod %s/%s as it is not attached to network: %s",
							pod.Namespace, pod.Name, netName)
					}
					return
				}
				for nadName := range networkMap {
					nadToDPUCDMap[nadName] = nil
				}
			} else {
				nadToDPUCDMap[types.DefaultNetworkName] = nil
			}

			for nadName := range nadToDPUCDMap {
				dpuCD := bnnc.podReadyToAddDPU(pod, nadName)
				if dpuCD != nil {
					err := bnnc.addDPUPodForNAD(pod, dpuCD, netName, nadName, clientSet)
					if err != nil {
						klog.Errorf(err.Error())
					} else {
						nadToDPUCDMap[nadName] = dpuCD
					}
				}
			}
			bnnc.podNADToDPUCDMap.Store(pod.UID, nadToDPUCDMap)
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPod := old.(*kapi.Pod)
			newPod := newer.(*kapi.Pod)
			klog.V(5).Infof("Update for Pod: %s/%s for network %s", newPod.Namespace, newPod.Name, netName)
			v, ok := bnnc.podNADToDPUCDMap.Load(newPod.UID)
			if !ok {
				klog.V(5).Infof("Skipping update for Pod %s/%s as it is not attached to network: %s",
					newPod.Namespace, newPod.Name, netName)
				return
			}
			nadToDPUCDMap := v.(map[string]*util.DPUConnectionDetails)
			for nadName := range nadToDPUCDMap {
				oldDPUCD := nadToDPUCDMap[nadName]
				newDPUCD := bnnc.podReadyToAddDPU(newPod, nadName)
				if !dpuConnectionDetailChanged(oldDPUCD, newDPUCD) {
					continue
				}
				if oldDPUCD != nil {
					// VF already added, but new Pod has changed, we'd need to delete the old VF
					klog.Infof("Deleting the old VF since either kubelet issued cmdDEL or assigned a new VF or "+
						"the sandbox id itself changed. Old connection details (%v), New connection details (%v)",
						oldDPUCD, newDPUCD)
					err := bnnc.delDPUPodForNAD(oldPod, oldDPUCD, nadName, false)
					if err != nil {
						klog.Errorf(err.Error())
					}
					nadToDPUCDMap[nadName] = nil
				}
				if newDPUCD != nil {
					klog.Infof("Adding VF during update because either during Pod Add we failed to add VF or "+
						"connection details weren't present or the VF ID has changed. Old connection details (%v), "+
						"New connection details (%v)", oldDPUCD, newDPUCD)
					err := bnnc.addDPUPodForNAD(newPod, newDPUCD, netName, nadName, clientSet)
					if err != nil {
						klog.Errorf(err.Error())
					} else {
						nadToDPUCDMap[nadName] = newDPUCD
					}
				}
			}
			bnnc.podNADToDPUCDMap.Store(newPod.UID, nadToDPUCDMap)
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			v, ok := bnnc.podNADToDPUCDMap.Load(pod.UID)
			if !ok {
				klog.V(5).Infof("Skipping delete for Pod %s/%s as it is not attached to network: %s",
					pod.Namespace, pod.Name, netName)
				return
			}
			klog.V(5).Infof("Delete for Pod: %s/%s for network %s", pod.Namespace, pod.Name, netName)
			nadToDPUCDMap := v.(map[string]*util.DPUConnectionDetails)
			bnnc.podNADToDPUCDMap.Delete(pod.UID)
			for nadName, dpuCD := range nadToDPUCDMap {
				if dpuCD != nil {
					err := bnnc.delDPUPodForNAD(pod, dpuCD, nadName, true)
					if err != nil {
						klog.Errorf(err.Error())
					}
				}
			}
		},
	}, nil)
}

// updatePodDPUConnStatusWithRetry update the pod annotion with the givin connection details
func (bnnc *BaseNodeNetworkController) updatePodDPUConnStatusWithRetry(origPod *kapi.Pod,
	dpuConnStatus *util.DPUConnectionStatus, nadName string) error {
	podDesc := fmt.Sprintf("pod %s/%s", origPod.Namespace, origPod.Name)
	klog.Infof("Updating pod %s with connection status (%+v) for NAD %s", podDesc, dpuConnStatus, nadName)
	err := util.UpdatePodDPUConnStatusWithRetry(
		bnnc.watchFactory.PodCoreInformer().Lister(),
		bnnc.Kube,
		origPod,
		dpuConnStatus,
		nadName,
	)
	if util.IsAnnotationAlreadySetError(err) {
		return nil
	}

	return err
}

// addRepPort adds the representor of the VF to the ovs bridge
func (bnnc *BaseNodeNetworkController) addRepPort(pod *kapi.Pod, dpuCD *util.DPUConnectionDetails, ifInfo *cni.PodInterfaceInfo, getter cni.PodInfoGetter) error {

	nadName := ifInfo.NADName
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
	vfRepName, err := util.GetSriovnetOps().GetVfRepresentorDPU(dpuCD.PfId, dpuCD.VfId)
	if err != nil {
		klog.Infof("Failed to get VF representor for %s dpuConnDetail %+v: %v", podDesc, dpuCD, err)
		return err
	}

	// set netdevName so OVS interface can be added with external_ids:vf-netdev-name, and is able to
	// be part of healthcheck.
	ifInfo.NetdevName = vfRepName
	vfPciAddress, err := util.GetSriovnetOps().GetPCIFromDeviceName(vfRepName)
	if err != nil {
		klog.Infof("Failed to get PCI address of VF rep %s: %v", vfRepName, err)
		return err
	}

	klog.Infof("Adding VF representor %s for %s", vfRepName, podDesc)
	err = cni.ConfigureOVS(context.TODO(), pod.Namespace, pod.Name, vfRepName, ifInfo, dpuCD.SandboxId, vfPciAddress, getter)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		return err
	}
	klog.Infof("Port %s added to bridge br-int", vfRepName)

	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		_ = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		return fmt.Errorf("failed to get link device for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
		_ = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		return fmt.Errorf("failed to setup representor port. failed to set MTU for interface %s", vfRepName)
	}

	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		_ = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		return fmt.Errorf("failed to setup representor port. failed to set link up for interface %s", vfRepName)
	}

	// Update connection-status annotation
	// TODO(adrianc): we should update Status in case of error as well
	connStatus := util.DPUConnectionStatus{Status: util.DPUConnectionStatusReady, Reason: ""}
	err = bnnc.updatePodDPUConnStatusWithRetry(pod, &connStatus, nadName)
	if err != nil {
		_ = util.GetNetLinkOps().LinkSetDown(link)
		_ = bnnc.delRepPort(pod, dpuCD, vfRepName, nadName)
		return fmt.Errorf("failed to setup representor port. failed to set pod annotations. %v", err)
	}
	return nil
}

// delRepPort delete the representor of the VF from the ovs bridge
func (bnnc *BaseNodeNetworkController) delRepPort(pod *kapi.Pod, dpuCD *util.DPUConnectionDetails, vfRepName, nadName string) error {
	//TODO(adrianc): handle: clearPodBandwidth(pr.SandboxID), pr.deletePodConntrack()
	podDesc := fmt.Sprintf("pod %s/%s for NAD %s", pod.Namespace, pod.Name, nadName)
	klog.Infof("Delete VF representor %s for %s", vfRepName, podDesc)
	ifExists, sandbox, expectedNADName, err := util.GetOVSPortPodInfo(vfRepName)
	if err != nil {
		return fmt.Errorf(err.Error())
	}
	if !ifExists {
		klog.Infof("VF representor %s for %s is not an OVS interface, nothing to do", vfRepName, podDesc)
		return nil
	}
	if sandbox != dpuCD.SandboxId {
		return fmt.Errorf("OVS port %s was added for sandbox (%s), expecting (%s)", vfRepName, sandbox, dpuCD.SandboxId)
	}
	if expectedNADName != nadName {
		return fmt.Errorf("OVS port %s was added for NAD (%s), expecting (%s)", vfRepName, expectedNADName, nadName)
	}
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
	return wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		_, _, err := util.RunOVSVsctl("--if-exists", "del-port", "br-int", vfRepName)
		if err != nil {
			return false, nil
		}
		klog.Infof("Port %s deleted from bridge br-int", vfRepName)
		return true, nil
	})
}
