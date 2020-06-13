package controller

import (
	"fmt"
	"net"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	houtil "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/subnetallocator"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
	utilnet "k8s.io/utils/net"
)

// MasterController is the master hybrid overlay controller
type MasterController struct {
	kube      kube.Interface
	allocator *subnetallocator.SubnetAllocator
	wf        *factory.WatchFactory
}

// NewMaster a new master controller that listens for node events
func NewMaster(kube kube.Interface) (*MasterController, error) {
	m := &MasterController{
		kube:      kube,
		allocator: subnetallocator.NewSubnetAllocator(),
	}

	// Add our hybrid overlay CIDRs to the subnetallocator
	for _, clusterEntry := range config.HybridOverlay.ClusterSubnets {
		err := m.allocator.AddNetworkRange(clusterEntry.CIDR, 32-clusterEntry.HostSubnetLength)
		if err != nil {
			return nil, err
		}
	}

	// Mark existing hostsubnets as already allocated
	existingNodes, err := m.kube.GetNodes()
	if err != nil {
		return nil, fmt.Errorf("Error in initializing/fetching subnets: %v", err)
	}
	for _, node := range existingNodes.Items {
		hostsubnet, err := houtil.ParseHybridOverlayHostSubnet(&node)
		if err != nil {
			klog.Warningf(err.Error())
		} else if hostsubnet != nil {
			klog.V(5).Infof("marking existing node %s hybrid overlay NodeSubnet %s as allocated", node.Name, hostsubnet)
			if err := m.allocator.MarkAllocatedNetwork(hostsubnet); err != nil {
				utilruntime.HandleError(err)
			}
		}
	}

	return m, nil
}

// StartMaster creates and starts the hybrid overlay master controller
func StartMaster(kube kube.Interface, wf *factory.WatchFactory) error {
	klog.Infof("Starting hybrid overlay master...")
	master, err := NewMaster(kube)
	if err != nil {
		return err
	}

	if err := houtil.StartNodeWatch(master, wf); err != nil {
		return err
	}

	if err := master.startNamespaceWatch(wf); err != nil {
		return err
	}

	return master.startPodWatch(wf)
}

// hybridOverlayNodeEnsureSubnet allocates a subnet and sets the
// hybrid overlay subnet annotation. It returns any newly allocated subnet
// or an error. If an error occurs, the newly allocated subnet will be released.
func (m *MasterController) hybridOverlayNodeEnsureSubnet(node *kapi.Node, annotator kube.Annotator) (*net.IPNet, error) {
	// Do not allocate a subnet if the node already has one
	if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
		return nil, nil
	}

	// Allocate a new host subnet for this node
	hostsubnets, err := m.allocator.AllocateNetworks()
	if err != nil {
		return nil, fmt.Errorf("Error allocating hybrid overlay HostSubnet for node %s: %v", node.Name, err)
	}

	if err := annotator.Set(types.HybridOverlayNodeSubnet, hostsubnets[0].String()); err != nil {
		_ = m.allocator.ReleaseNetwork(hostsubnets[0])
		return nil, err
	}

	klog.Infof("Allocated hybrid overlay HostSubnet %s for node %s", hostsubnets[0], node.Name)
	return hostsubnets[0], nil
}

func (m *MasterController) releaseNodeSubnet(nodeName string, subnet *net.IPNet) error {
	if err := m.allocator.ReleaseNetwork(subnet); err != nil {
		return fmt.Errorf("Error deleting hybrid overlay HostSubnet %s for node %q: %s", subnet, nodeName, err)
	}
	klog.Infof("Deleted hybrid overlay HostSubnet %s for node %s", subnet, nodeName)
	return nil
}

func (m *MasterController) handleOverlayPort(node *kapi.Node, annotator kube.Annotator) error {
	_, haveDRMACAnnotation := node.Annotations[types.HybridOverlayDRMAC]

	subnets, err := util.ParseNodeHostSubnetAnnotation(node)
	if subnets == nil || err != nil {
		// No subnet allocated yet; clean up
		if haveDRMACAnnotation {
			m.deleteOverlayPort(node)
			annotator.Delete(types.HybridOverlayDRMAC)
		}
		return nil
	}

	if haveDRMACAnnotation {
		// already set up; do nothing
		return nil
	}

	// FIXME DUAL-STACK
	subnet := subnets[0]

	portName := util.GetHybridOverlayPortName(node.Name)
	portMAC, portIPs, _ := util.GetPortAddresses(portName)
	if portMAC == nil || portIPs == nil {
		if portIPs == nil {
			portIPs = append(portIPs, util.GetNodeHybridOverlayIfAddr(subnet).IP)
		}
		if portMAC == nil {
			for _, ip := range portIPs {
				portMAC = util.IPAddrToHWAddr(ip)
				if !utilnet.IsIPv6(ip) {
					break
				}
			}
		}

		klog.Infof("creating node %s hybrid overlay port", node.Name)

		var stderr string
		_, stderr, err = util.RunOVNNbctl("--", "--may-exist", "lsp-add", node.Name, portName,
			"--", "lsp-set-addresses", portName, portMAC.String())
		if err != nil {
			return fmt.Errorf("failed to add hybrid overlay port for node %s"+
				", stderr:%s: %v", node.Name, stderr, err)
		}

		if err := util.UpdateNodeSwitchExcludeIPs(node.Name, subnet); err != nil {
			return err
		}
	}
	if err := annotator.Set(types.HybridOverlayDRMAC, portMAC.String()); err != nil {
		return fmt.Errorf("failed to set node %s hybrid overlay DRMAC annotation: %v", node.Name, err)
	}

	return nil
}

func (m *MasterController) deleteOverlayPort(node *kapi.Node) {
	klog.Infof("removing node %s hybrid overlay port", node.Name)
	portName := util.GetHybridOverlayPortName(node.Name)
	_, _, _ = util.RunOVNNbctl("--", "--if-exists", "lsp-del", portName)
}

// Add handles node additions
func (m *MasterController) Add(node *kapi.Node) {
	annotator := kube.NewNodeAnnotator(m.kube, node)

	var err error
	var allocatedSubnet *net.IPNet
	if houtil.IsHybridOverlayNode(node) {
		allocatedSubnet, err = m.hybridOverlayNodeEnsureSubnet(node, annotator)
		if err != nil {
			klog.Errorf("failed to update node %q hybrid overlay subnet annotation: %v", node.Name, err)
			return
		}
	} else {
		if err = m.handleOverlayPort(node, annotator); err != nil {
			klog.Errorf("failed to set up hybrid overlay logical switch port for %s: %v", node.Name, err)
			return
		}
	}

	if err = annotator.Run(); err != nil {
		// Release allocated subnet if any errors occurred
		if allocatedSubnet != nil {
			_ = m.releaseNodeSubnet(node.Name, allocatedSubnet)
		}
		klog.Errorf("failed to set hybrid overlay annotations for %s: %v", node.Name, err)
	}
}

// Update handles node updates
func (m *MasterController) Update(oldNode, newNode *kapi.Node) {
	m.Add(newNode)
}

// Delete handles node deletions
func (m *MasterController) Delete(node *kapi.Node) {
	if subnet, _ := houtil.ParseHybridOverlayHostSubnet(node); subnet != nil {
		if err := m.releaseNodeSubnet(node.Name, subnet); err != nil {
			klog.Errorf(err.Error())
		}
	}

	if _, ok := node.Annotations[types.HybridOverlayDRMAC]; ok && !houtil.IsHybridOverlayNode(node) {
		m.deleteOverlayPort(node)
	}
}

// Sync handles synchronizing the initial node list
func (m *MasterController) Sync(nodes []*kapi.Node) {
	// Unused because our initial node list sync needs to return
	// errors which this function cannot do
}

// startNamespaceWatch starts a watcher for namespace events
func (m *MasterController) startNamespaceWatch(wf *factory.WatchFactory) error {
	m.wf = wf
	_, err := wf.AddNamespaceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			if err := m.addOrUpdateNamespace(ns); err != nil {
				klog.Errorf("error adding namespace %s: %s", ns.Name, err)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			nsNew := newer.(*kapi.Namespace)
			nsOld := old.(*kapi.Namespace)
			if nsHybridAnnotationChanged(nsOld, nsNew) {
				if err := m.addOrUpdateNamespace(nsNew); err != nil {
					klog.Errorf("error updating namespace %s: %s", nsNew.Name, err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// dont care about namespace delete
		},
	}, nil)
	return err
}

// addOrUpdateNamespace copies namespace annotations to all pods in the namespace
func (m *MasterController) addOrUpdateNamespace(ns *kapi.Namespace) error {
	pods, err := m.wf.GetPods(ns.Name)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err := houtil.CopyNamespaceAnnotationsToPod(m.kube, ns, pod); err != nil {
			klog.Errorf("Unable to copy hybrid-overlay namespace annotations to pod %s", pod.Name)
		}
	}
	return nil
}

// startPodWatch starts a watcher for pod events
func (m *MasterController) startPodWatch(wf *factory.WatchFactory) error {
	m.wf = wf
	_, err := wf.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if err := m.addOrUpdatePod(pod); err != nil {
				klog.Errorf("error updating pod %s: %s", pod.Name, err)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			// don't care about pod update
		},
		DeleteFunc: func(obj interface{}) {
			// dont care about pod delete
		},
	}, nil)
	return err
}

// waitForNamespace fetches a namespace from the cache, or waits until it's available
func (m *MasterController) waitForNamespace(name string) (*kapi.Namespace, error) {
	var namespaceBackoff = wait.Backoff{Duration: 1 * time.Second, Steps: 7, Factor: 1.5, Jitter: 0.1}
	var namespace *kapi.Namespace
	if err := wait.ExponentialBackoff(namespaceBackoff, func() (bool, error) {
		var err error
		namespace, err = m.wf.GetNamespace(name)
		if err != nil {
			if errors.IsNotFound(err) {
				// Namespace not found; retry
				return false, nil
			}
			klog.Warningf("error getting namespace: %v", err)
			return false, err
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("failed to get namespace object: %v", err)
	}
	return namespace, nil
}

// addOrUpdatePod ensures that hybrid overlay annotations are copied to a
// pod when it's created. This allows the nodes to set up the appropriate
// flows
func (m *MasterController) addOrUpdatePod(pod *kapi.Pod) error {
	namespace, err := m.waitForNamespace(pod.Namespace)
	if err != nil {
		return err
	}

	namespaceExternalGw := namespace.Annotations[hotypes.HybridOverlayExternalGw]
	namespaceVTEP := namespace.Annotations[hotypes.HybridOverlayVTEP]

	podExternalGw := pod.Annotations[hotypes.HybridOverlayExternalGw]
	podVTEP := pod.Annotations[hotypes.HybridOverlayVTEP]

	if namespaceExternalGw != podExternalGw || namespaceVTEP != podVTEP {
		// copy namespace annotations to the pod and return
		return houtil.CopyNamespaceAnnotationsToPod(m.kube, namespace, pod)
	}
	return nil
}

// nsHybridAnnotationChanged returns true if any relevant NS attributes changed
func nsHybridAnnotationChanged(ns1 *kapi.Namespace, ns2 *kapi.Namespace) bool {
	nsExGw1 := ns1.GetAnnotations()[hotypes.HybridOverlayExternalGw]
	nsVTEP1 := ns1.GetAnnotations()[hotypes.HybridOverlayVTEP]
	nsExGw2 := ns2.GetAnnotations()[hotypes.HybridOverlayExternalGw]
	nsVTEP2 := ns2.GetAnnotations()[hotypes.HybridOverlayVTEP]

	if nsExGw1 != nsExGw2 || nsVTEP1 != nsVTEP2 {
		return true
	}
	return false
}
