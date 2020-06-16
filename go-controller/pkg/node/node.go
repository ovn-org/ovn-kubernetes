package node

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/healthcheck"

	"k8s.io/klog"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type endpointAddressMap map[string]struct{}

type mgmtPortHealtCheckFn func(stop <-chan struct{})

type nodePortWatcher interface {
	AddService(svc *kapi.Service) error
	DeleteService(svc *kapi.Service) error
}

// OvnNode is the object holder for utilities meant for node management
type OvnNode struct {
	name                 string
	Kube                 kube.Interface
	endpointEventHandler informer.EventHandler
	serviceEventHandler  informer.EventHandler

	// map of endpoint name to address maps
	endpointAddressMap map[string]endpointAddressMap
	// mutex to pretect endpoint address map
	endpointAddressMapMutex sync.Mutex

	// local gateway healthcheck
	server    healthcheck.Server
	services  map[ktypes.NamespacedName]uint16
	endpoints map[ktypes.NamespacedName]int

	// shared gateway healtcheck
	sharedGatewayHealthcheck *sharedGatewayHealthcheck

	// node port watcher
	npw nodePortWatcher

	// mgmt port config
	mgmtPortHealtCheck mgmtPortHealtCheckFn
}

func endpointChanged(old, new interface{}) bool {
	epNew := new.(*kapi.Endpoints)
	epOld := old.(*kapi.Endpoints)
	return !reflect.DeepEqual(epNew.Subsets, epOld.Subsets)
}

func serviceChanged(old, new interface{}) bool {
	svcNew := new.(*kapi.Service)
	svcOld := old.(*kapi.Service)
	return !reflect.DeepEqual(svcNew.Spec, svcOld.Spec)
}

// NewNode creates a new controller for node management
func NewNode(
	kubeClient kubernetes.Interface,
	name string,
	endpointInformer cache.SharedIndexInformer,
	serviceInformer cache.SharedIndexInformer,
) *OvnNode {
	o := &OvnNode{
		name:               name,
		Kube:               &kube.Kube{KClient: kubeClient},
		server:             healthcheck.NewServer(name, nil, nil, nil),
		endpointAddressMap: make(map[string]endpointAddressMap),
		services:           make(map[ktypes.NamespacedName]uint16),
		endpoints:          make(map[ktypes.NamespacedName]int),
		npw:                nil,
		mgmtPortHealtCheck: nil,
	}
	o.endpointEventHandler = informer.NewDefaultEventHandler(
		"endpoints",
		endpointInformer,
		func(obj interface{}) error {
			ep, ok := obj.(*kapi.Endpoints)
			if !ok {
				return fmt.Errorf("%s is not an endpoint", obj)
			}
			if config.Gateway.NodeportEnable {
				name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
				if _, exists := o.services[name]; exists {
					o.endpoints[name] = countLocalEndpoints(ep, o.name)
					_ = o.server.SyncEndpoints(o.endpoints)
				}
			}
			o.endpointAddressMapMutex.Lock()
			defer o.endpointAddressMapMutex.Unlock()

			newEndpointAddressMap := buildEndpointAddressMap(ep.Subsets)
			// compute addressMap for adds
			if _, ok := o.endpointAddressMap[ep.Name]; !ok {
				o.endpointAddressMap[ep.Name] = newEndpointAddressMap
				return nil
			}
			// compare addressMap with existing for updates
			for ip := range newEndpointAddressMap {
				if _, ok := o.endpointAddressMap[ep.Name][ip]; !ok {
					deleteConntrack(ip)
				}
			}
			o.endpointAddressMap[ep.Name] = newEndpointAddressMap
			return nil
		},
		func(obj interface{}) error {
			ep, ok := obj.(*kapi.Endpoints)
			if !ok {
				return fmt.Errorf("%s is not an endpoint", obj)
			}
			if config.Gateway.NodeportEnable {
				name := ktypes.NamespacedName{Namespace: ep.Namespace, Name: ep.Name}
				delete(o.endpoints, name)
				_ = o.server.SyncEndpoints(o.endpoints)
			}
			for ip := range o.endpointAddressMap[ep.Name] {
				deleteConntrack(ip)
			}
			return nil
		},
		endpointChanged,
	)

	o.serviceEventHandler = informer.NewDefaultEventHandler(
		"services",
		serviceInformer,
		func(obj interface{}) error {
			if !config.Gateway.NodeportEnable {
				return nil
			}
			svc, ok := obj.(*kapi.Service)
			if !ok {
				return fmt.Errorf("object %s is not a service", obj)
			}
			if svc.Spec.HealthCheckNodePort != 0 {
				name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
				o.services[name] = uint16(svc.Spec.HealthCheckNodePort)
				_ = o.server.SyncServices(o.services)
			}
			return o.npw.AddService(svc)
		},
		func(obj interface{}) error {
			if !config.Gateway.NodeportEnable {
				return nil
			}
			svc := obj.(*kapi.Service)
			if svc.Spec.HealthCheckNodePort != 0 {
				name := ktypes.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
				delete(o.services, name)
				// this was copied verbatim but doesn't seem right
				delete(o.endpoints, name)
				_ = o.server.SyncServices(o.services)
			}
			return o.npw.DeleteService(svc)
		},
		serviceChanged,
	)
	return o
}

func setupOVNNode(node *kapi.Node) error {
	var err error

	nodeName, err := util.GetNodeHostname(node)
	if err != nil {
		return fmt.Errorf("failed to obtain hostname from node %q: %v", node.Name, err)
	}

	nodeIP := config.Default.EncapIP
	if nodeIP == "" {
		nodeIP, err = util.GetNodeIP(node)
		if err != nil {
			return fmt.Errorf("failed to obtain local IP from node %q: %v", node.Name, err)
		}
	} else {
		if ip := net.ParseIP(nodeIP); ip == nil {
			return fmt.Errorf("invalid encapsulation IP provided %q", nodeIP)
		}
	}

	_, stderr, err := util.RunOVSVsctl("set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", nodeIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:ovn-openflow-probe-interval=%d",
			config.Default.OpenFlowProbe),
		fmt.Sprintf("external_ids:hostname=\"%s\"", nodeName),
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
			klog.Infof("node %s connection status = %s", name, ret)
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

// Run learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
// It then spawns the endpoint worker threads and blocks until the
// stop channel is closed
func (n *OvnNode) Run(stopChan <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	klog.Info("Starting OVN Node Controller")

	var err error
	var node *kapi.Node
	var subnets []*net.IPNet

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("setting klog \"loglevel\" to 5 failed, err: %v", err)
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
			klog.Infof("waiting to retrieve node %s: %v", n.name, err)
			return false, nil
		}
		subnets, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			klog.Infof("waiting for node %s to start, no annotation found on node for subnet: %v", n.name, err)
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

	// Initialize gateway resources on the node
	// FIXME DUAL-STACK
	if err := n.initGateway(subnets[0], nodeAnnotator, waiter); err != nil {
		return err
	}

	// Initialize management port resources on the node
	if err := n.createManagementPort(subnets, nodeAnnotator, waiter); err != nil {
		return err
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("Failed to set node %s annotations: %v", n.name, err)
	}

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	if config.Gateway.Mode == config.GatewayModeShared {
		if n.sharedGatewayHealthcheck == nil {
			return fmt.Errorf("shared gateway healtcheck is not populated")
		}
		klog.Infof("Starting conntrack healtcheck thread")
		// add health check function to check default OpenFlow flows are on the shared gateway bridge
		go checkDefaultConntrackRules(n.sharedGatewayHealthcheck, stopChan)
	}

	// start the management port health check
	if runtime.GOOS != "windows" {
		if n.mgmtPortHealtCheck == nil {
			return fmt.Errorf("mgmt port check is not populated")
		}
		klog.Infof("Starting management port healtcheck thread")
		go n.mgmtPortHealtCheck(stopChan)
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("reset of initial klog \"loglevel\" failed, err: %v", err)
	}

	// start health check to ensure there are no stale OVS internal ports
	klog.Infof("Starting stale OVS interface thread")
	go checkForStaleOVSInterfaces(stopChan)

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
		return fmt.Errorf("Cannot get kubeclient for starting CNI server")
	}

	klog.Infof("Starting workqueue worker threads")
	go func() {
		if err := n.endpointEventHandler.Run(informer.DefaultInformerThreadiness, stopChan); err != nil {
			klog.Error(err)
		}
	}()
	go func() {
		if err := n.serviceEventHandler.Run(informer.DefaultInformerThreadiness, stopChan); err != nil {
			klog.Error(err)
		}
	}()

	// start the cni server
	klog.Infof("Starting CNI server")
	cniServer := cni.NewCNIServer("", kclient.KClient)
	if err := cniServer.Start(cni.HandleCNIRequest); err != nil {
		return err
	}

	<-stopChan

	return nil
}

func buildEndpointAddressMap(epSubsets []kapi.EndpointSubset) map[string]struct{} {
	addressMap := make(map[string]struct{})
	for _, subset := range epSubsets {
		for _, address := range subset.Addresses {
			addressMap[address.IP] = struct{}{}
		}
	}
	return addressMap
}
