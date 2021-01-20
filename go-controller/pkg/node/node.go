package node

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	honode "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// OvnNode is the object holder for utilities meant for node management
type OvnNode struct {
	name         string
	Kube         kube.Interface
	watchFactory factory.NodeWatchFactory
	stopChan     chan struct{}
	recorder     record.EventRecorder
	gateway      Gateway
}

// NewNode creates a new controller for node management
func NewNode(kubeClient kubernetes.Interface, wf factory.NodeWatchFactory, name string, stopChan chan struct{}, eventRecorder record.EventRecorder) *OvnNode {
	return &OvnNode{
		name:         name,
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
		stopChan:     stopChan,
		recorder:     eventRecorder,
	}
}

func setupOVNNode(node *kapi.Node) error {
	var err error

	encapIP := config.Default.EncapIP
	if encapIP == "" {
		encapIP, err = util.GetNodePrimaryIP(node)
		if err != nil {
			return fmt.Errorf("failed to obtain local IP from node %q: %v", node.Name, err)
		}
	} else {
		if ip := net.ParseIP(encapIP); ip == nil {
			return fmt.Errorf("invalid encapsulation IP provided %q", encapIP)
		}
	}

	_, stderr, err := util.RunOVSVsctl("set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", encapIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:ovn-openflow-probe-interval=%d",
			config.Default.OpenFlowProbe),
		fmt.Sprintf("external_ids:hostname=\"%s\"", node.Name),
		"external_ids:ovn-monitor-all=true",
	)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}
	// If EncapPort is not the default tell sbdb to use specified port.
	if config.Default.EncapPort != config.DefaultEncapPort {
		systemID, err := util.GetNodeChassisID()
		if err != nil {
			return err
		}
		uuid, _, err := util.RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "Encap",
			fmt.Sprintf("chassis_name=%s", systemID))
		if err != nil {
			return err
		}
		if len(uuid) == 0 {
			return fmt.Errorf("unable to find encap uuid to set geneve port for chassis %s", systemID)
		}
		_, stderr, errSet := util.RunOVNSbctl("set", "encap", uuid,
			fmt.Sprintf("options:dst_port=%d", config.Default.EncapPort),
		)
		if errSet != nil {
			return fmt.Errorf("error setting OVS encap-port: %v\n  %q", errSet, stderr)
		}
	}
	return nil
}

func isOVNControllerReady(name string) (bool, error) {
	runDir := util.GetOvnRunDir()

	pid, err := ioutil.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		return false, fmt.Errorf("unknown pid for ovn-controller process: %v", err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSuffix(string(pid), "\n"))
		ret, _, err := util.RunOVSAppctl("-t", ctlFile, "connection-status")
		if err == nil {
			klog.Infof("Node %s connection status = %s", name, ret)
			return ret == "connected", nil
		}
		return false, err
	})
	if err != nil {
		return false, fmt.Errorf("timed out waiting sbdb for node %s: %v", name, err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		_, _, err := util.RunOVSVsctl("--", "br-exists", "br-int")
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("timed out checking whether br-int exists or not on node %s: %v", name, err)
	}

	err = wait.PollImmediate(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		stdout, _, err := util.RunOVSOfctl("dump-aggregate", "br-int")
		if err != nil {
			klog.V(5).Infof("Error dumping aggregate flows: %v "+
				"for node: %s", err, name)
			return false, nil
		}
		ret := strings.Contains(stdout, "flow_count=0")
		if ret {
			klog.V(5).Infof("Got a flow count of 0 when "+
				"dumping flows for node: %s", name)
		}
		return !ret, nil
	})
	if err != nil {
		return false, fmt.Errorf("timed out dumping br-int flow entries for node %s: %v", name, err)
	}

	return true, nil
}

// Start learns the subnets assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (n *OvnNode) Start(wg *sync.WaitGroup) error {
	var err error
	var node *kapi.Node
	var subnets []*net.IPNet

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("Setting klog \"loglevel\" to 5 failed, err: %v", err)
	}

	for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
		if err := auth.SetDBAuth(); err != nil {
			return err
		}
	}

	if node, err = n.Kube.GetNode(n.name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", n.name, err)
	}
	err = setupOVNNode(node)
	if err != nil {
		return err
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if node, err = n.Kube.GetNode(n.name); err != nil {
			klog.Infof("Waiting to retrieve node %s: %v", n.name, err)
			return false, nil
		}
		subnets, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			klog.Infof("Waiting for node %s to start, no annotation found on node for subnet: %v", n.name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for node's: %q logical switch: %v", n.name, err)
	}

	klog.Infof("Node %s ready for ovn initialization with subnet %s", n.name, util.JoinIPNets(subnets, ","))

	if _, err = isOVNControllerReady(n.name); err != nil {
		return err
	}

	nodeAnnotator := kube.NewNodeAnnotator(n.Kube, node)
	waiter := newStartupWaiter()

	// Initialize management port resources on the node
	mgmtPortConfig, err := createManagementPort(n.name, subnets, nodeAnnotator, waiter)
	if err != nil {
		return err
	}

	// Initialize gateway resources on the node
	if err := n.initGateway(subnets, nodeAnnotator, waiter, mgmtPortConfig); err != nil {
		return err
	}

	wg.Add(1)
	go n.gateway.Run(n.stopChan)

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %v", n.name, err)
	}

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	if config.HybridOverlay.Enabled {
		nodeController, err := honode.NewNode(
			n.Kube,
			n.name,
			n.watchFactory.NodeInformer(),
			n.watchFactory.LocalPodInformer(),
			informer.NewDefaultEventHandler,
		)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeController.Run(n.stopChan)
		}()
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("Reset of initial klog \"loglevel\" failed, err: %v", err)
	}

	// start health check to ensure there are no stale OVS internal ports
	go checkForStaleOVSInterfaces(n.stopChan)

	// start management port health check
	go checkManagementPortHealth(mgmtPortConfig, n.stopChan)

	confFile := filepath.Join(config.CNI.ConfDir, config.CNIConfFileName)
	_, err = os.Stat(confFile)
	if os.IsNotExist(err) {
		err = config.WriteCNIConfig()
		if err != nil {
			return err
		}
	}

	kclient, ok := n.Kube.(*kube.Kube)
	if !ok {
		return fmt.Errorf("cannot get kubeclient for starting CNI server")
	}
	n.WatchEndpoints()

	if config.OvnKubeNode.Mode == types.NodeModeSmartNIC {
		n.watchSmartNicPods()
	} else {
		// start the cni server
		var cniServer *cni.Server
		cniServer, err = cni.NewCNIServer("", kclient.KClient, config.OvnKubeNode.Mode)
		if err == nil {
			err = cniServer.Start(cni.HandleCNIRequest)
		}
	}

	return err
}

func (n *OvnNode) WatchEndpoints() {
	n.watchFactory.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, new interface{}) {
			epNew := new.(*kapi.Endpoints)
			epOld := old.(*kapi.Endpoints)
			if reflect.DeepEqual(epNew.Subsets, epOld.Subsets) {
				return
			}
			newEpAddressMap := buildEndpointAddressMap(epNew.Subsets)
			for item := range buildEndpointAddressMap(epOld.Subsets) {
				if _, ok := newEpAddressMap[item]; !ok {
					err := deleteConntrack(item.ip, item.port, item.protocol)
					if err != nil {
						klog.Errorf("Failed to delete conntrack entry for %s: %v", item.ip, err)
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			for item := range buildEndpointAddressMap(ep.Subsets) {
				err := deleteConntrack(item.ip, item.port, item.protocol)
				if err != nil {
					klog.Errorf("Failed to delete conntrack entry for %s: %v", item.ip, err)
				}

			}
		},
	}, nil)
}

type epAddressItem struct {
	ip       string
	port     int32
	protocol kapi.Protocol
}

//buildEndpointAddressMap builds a map of all UDP and SCTP ports in the endpoint subset along with that port's IP address
func buildEndpointAddressMap(epSubsets []kapi.EndpointSubset) map[epAddressItem]struct{} {
	epMap := make(map[epAddressItem]struct{})
	for _, subset := range epSubsets {
		for _, address := range subset.Addresses {
			for _, port := range subset.Ports {
				if port.Protocol == kapi.ProtocolUDP || port.Protocol == kapi.ProtocolSCTP {
					epMap[epAddressItem{
						ip:       address.IP,
						port:     port.Port,
						protocol: port.Protocol,
					}] = struct{}{}
				}
			}
		}
	}

	return epMap
}

//watchSmartNicPods watch updates for pod smart nic annotations
func (n *OvnNode) watchSmartNicPods() {
	var retryPods sync.Map
	// servedPods tracks the pods that got a VF
	var servedPods sync.Map
	_ = n.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("AddFunc for POD: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
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
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations)
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
			klog.Infof("UpdateFunc for POD: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
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
					retryPods.Store(pod.UID, true)
					return
				}
				podInterfaceInfo, err := cni.PodAnnotation2PodInfo(pod.Annotations)
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
					retryPods.Delete(pod.UID)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			klog.Infof("DeleteFunc for POD: %s/%s", pod.ObjectMeta.GetNamespace(), pod.ObjectMeta.GetName())
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
				klog.Infof("Failed to delete VF representor %s. %s", vfRepName, err)
			}
		},
	}, nil)
}

// getVfRepName returns the VF's representor of the VF assigned to the pod
func (n *OvnNode) getVfRepName(pod *kapi.Pod) (string, error) {
	smartNicCD := util.SmartNICConnectionDetails{}
	if err := smartNicCD.FromPodAnnotation(pod); err != nil {
		return "", fmt.Errorf("failed to get smart-nic annotation. %v", err)
	}
	return util.GetSriovnetOps().GetVfRepresentorSmartNIC(smartNicCD.PfId, smartNicCD.VfId)
}

// addRepPort adds the representor of the VF to the ovs bridge
func (n *OvnNode) addRepPort(pod *kapi.Pod, vfRepName string, ifInfo *cni.PodInterfaceInfo) error {
	klog.Infof("addRepPort: %s", vfRepName)
	sandboxID := pod.Annotations["sandbox"]

	err := cni.ConfigureOVS(pod.Namespace, pod.Name, vfRepName, ifInfo, sandboxID)
	if err != nil {
		return err
	}
	klog.Infof("Port %s added to bridge br-int", vfRepName)

	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		// Note(adrianc): we are lenient with cleanup in this method as pod is going to be retried anyway.
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to get link device for interface %s", vfRepName)
	}

	klog.Infof("addRepPort: set link mtu %s", vfRepName)
	if err = util.GetNetLinkOps().LinkSetMTU(link, ifInfo.MTU); err != nil {
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set MTU for interface %s", vfRepName)
	}

	klog.Infof("addRepPort: set link up for %s", vfRepName)
	if err = util.GetNetLinkOps().LinkSetUp(link); err != nil {
		_ = n.delRepPort(vfRepName)
		return fmt.Errorf("failed to setup representor port. failed to set link up for interface %s", vfRepName)
	}

	// Update connection-status annotation
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
	klog.Infof("delRepPort: %s", vfRepName)
	// Set link down for representor port
	link, err := util.GetNetLinkOps().LinkByName(vfRepName)
	if err != nil {
		klog.Warningf("Failed to get link device for representor port %s. %v", vfRepName, err)
	} else {
		if linkDownErr := util.GetNetLinkOps().LinkSetDown(link); linkDownErr != nil {
			klog.Warningf("Failed to set link down for representor port %s. %v", vfRepName, linkDownErr)
		}
	}
	klog.Infof("Port %s link state set to \"down\"", vfRepName)
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
