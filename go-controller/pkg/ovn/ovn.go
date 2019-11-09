package ovn

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/openshift/origin/pkg/util/netutils"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	kapisnetworking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	kv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

// ServiceVIPKey is used for looking up service namespace information for a
// particular load balancer
type ServiceVIPKey struct {
	// Load balancer VIP in the form "ip:port"
	vip string
	// Protocol used by the load balancer
	protocol kapi.Protocol
}

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	kube         kube.Interface
	watchFactory *factory.WatchFactory

	masterSubnetAllocatorList []*netutils.SubnetAllocator

	TCPLoadBalancerUUID string
	UDPLoadBalancerUUID string

	gatewayCache map[string]string
	// For TCP and UDP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[string]string

	// For TCP and UDP type traffice, cache OVN load balancer that exists on the
	// default gateway
	loadbalancerGWCache map[string]string
	defGatewayRouter    string

	// A cache of all logical switches seen by the watcher
	logicalSwitchCache map[string]bool

	// A cache of all logical ports seen by the watcher and
	// its corresponding logical switch
	logicalPortCache map[string]string

	// A cache of all logical ports and its corresponding uuids.
	logicalPortUUIDCache map[string]string

	// For each namespace, a map from pod IP address to logical port name
	// for all pods in that namespace.
	namespaceAddressSet map[string]map[string]string

	// For each namespace, a lock to protect critical regions
	namespaceMutex map[string]*sync.Mutex

	// Need to make calls to namespaceMutex also thread-safe
	namespaceMutexMutex sync.Mutex

	// For each namespace, a map of policy name to 'namespacePolicy'.
	namespacePolicies map[string]map[string]*namespacePolicy

	// Port group for ingress deny rule
	portGroupIngressDeny string

	// Port group for egress deny rule
	portGroupEgressDeny string

	// For each logical port, the number of network policies that want
	// to add a ingress deny rule.
	lspIngressDenyCache map[string]int

	// For each logical port, the number of network policies that want
	// to add a egress deny rule.
	lspEgressDenyCache map[string]int

	// A mutex for lspIngressDenyCache and lspEgressDenyCache
	lspMutex *sync.Mutex

	// A mutex for gatewayCache and logicalSwitchCache which holds
	// logicalSwitch information
	lsMutex *sync.Mutex

	// Per namespace multicast enabled?
	multicastEnabled map[string]bool

	// Supports port_group?
	portGroupSupport bool

	// Supports multicast?
	multicastSupport bool

	// Map of load balancers to service namespace
	serviceVIPToName map[ServiceVIPKey]types.NamespacedName

	serviceVIPToNameLock sync.Mutex

	// List of subnets to assign to hybrid overlay entities
	hybridOverlayClusterSubnets []config.CIDRNetworkEntry
}

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"
)

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewOvnController(kubeClient kubernetes.Interface, wf *factory.WatchFactory, hybridOverlayClusterSubnets []config.CIDRNetworkEntry) *Controller {
	return &Controller{
		kube:                        &kube.Kube{KClient: kubeClient},
		watchFactory:                wf,
		logicalSwitchCache:          make(map[string]bool),
		logicalPortCache:            make(map[string]string),
		logicalPortUUIDCache:        make(map[string]string),
		namespaceAddressSet:         make(map[string]map[string]string),
		namespacePolicies:           make(map[string]map[string]*namespacePolicy),
		namespaceMutex:              make(map[string]*sync.Mutex),
		namespaceMutexMutex:         sync.Mutex{},
		lspIngressDenyCache:         make(map[string]int),
		lspEgressDenyCache:          make(map[string]int),
		lspMutex:                    &sync.Mutex{},
		lsMutex:                     &sync.Mutex{},
		gatewayCache:                make(map[string]string),
		loadbalancerClusterCache:    make(map[string]string),
		loadbalancerGWCache:         make(map[string]string),
		multicastEnabled:            make(map[string]bool),
		serviceVIPToName:            make(map[ServiceVIPKey]types.NamespacedName),
		serviceVIPToNameLock:        sync.Mutex{},
		hybridOverlayClusterSubnets: hybridOverlayClusterSubnets,
	}
}

// Run starts the actual watching.
func (oc *Controller) Run(nodeSelector *metav1.LabelSelector, stopChan chan struct{}) error {
	startOvnUpdater()

	// WatchNodes must be started first so that its initial Add will
	// create all node logical switches, which other watches may depend on.
	// https://github.com/ovn-org/ovn-kubernetes/pull/859
	if err := oc.WatchNodes(nodeSelector); err != nil {
		return err
	}

	for _, f := range []func() error{oc.WatchPods, oc.WatchServices, oc.WatchEndpoints,
		oc.WatchNamespaces, oc.WatchNetworkPolicy} {
		if err := f(); err != nil {
			return err
		}
	}

	if config.Kubernetes.OVNEmptyLbEvents {
		go oc.ovnControllerEventChecker(stopChan)
	}

	return nil
}

type eventRecord struct {
	Data     [][]interface{} `json:"Data"`
	Headings []string        `json:"Headings"`
}

type emptyLBBackendEvent struct {
	vip      string
	protocol kapi.Protocol
	uuid     string
}

func extractEmptyLBBackendsEvents(out []byte) ([]emptyLBBackendEvent, error) {
	events := make([]emptyLBBackendEvent, 0, 4)

	var f eventRecord
	err := json.Unmarshal(out, &f)
	if err != nil {
		return events, err
	}
	if len(f.Data) == 0 {
		return events, nil
	}

	var eventInfoIndex int
	var eventTypeIndex int
	var uuidIndex int
	for idx, val := range f.Headings {
		switch val {
		case "event_info":
			eventInfoIndex = idx
		case "event_type":
			eventTypeIndex = idx
		case "_uuid":
			uuidIndex = idx
		}
	}

	for _, val := range f.Data {
		if len(val) <= eventTypeIndex {
			return events, errors.New("Mismatched Data and Headings in controller event")
		}
		if val[eventTypeIndex] != "empty_lb_backends" {
			continue
		}

		uuidArray, ok := val[uuidIndex].([]interface{})
		if !ok {
			return events, errors.New("Unexpected '_uuid' data in controller event")
		}
		if len(uuidArray) < 2 {
			return events, errors.New("Malformed UUID presented in controller event")
		}
		uuid, ok := uuidArray[1].(string)
		if !ok {
			return events, errors.New("Failed to parse UUID in controller event")
		}

		// Unpack the data. There's probably a better way to do this.
		info, ok := val[eventInfoIndex].([]interface{})
		if !ok {
			return events, errors.New("Unexpected 'event_info' data in controller event")
		}
		if len(info) < 2 {
			return events, errors.New("Malformed event_info in controller event")
		}
		eventMap, ok := info[1].([]interface{})
		if !ok {
			return events, errors.New("'event_info' data is not the expected type")
		}

		var vip string
		var protocol kapi.Protocol
		for _, x := range eventMap {
			tuple, ok := x.([]interface{})
			if !ok {
				return events, errors.New("event map item failed to parse")
			}
			if len(tuple) < 2 {
				return events, errors.New("event map contains malformed data")
			}
			switch tuple[0] {
			case "vip":
				vip, ok = tuple[1].(string)
				if !ok {
					return events, errors.New("Failed to parse vip in controller event")
				}
			case "protocol":
				prot, ok := tuple[1].(string)
				if !ok {
					return events, errors.New("Failed to parse protocol in controller event")
				}
				if prot == "udp" {
					protocol = kapi.ProtocolUDP
				} else {
					protocol = kapi.ProtocolTCP
				}
			}
		}
		events = append(events, emptyLBBackendEvent{vip, protocol, uuid})
	}

	return events, nil
}

func (oc *Controller) ovnControllerEventChecker(stopChan chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)

	_, _, err := util.RunOVNNbctl("set", "nb_global", ".", "options:controller_event=true")
	if err != nil {
		logrus.Error("Unable to enable controller events. Unidling not possible")
		return
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&kv1core.EventSinkImpl{Interface: oc.kube.Events()})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, kapi.EventSource{Component: "kube-proxy"})

	for {
		select {
		case <-ticker.C:
			out, _, err := util.RunOVNSbctl("--format=json", "list", "controller_event")
			if err != nil {
				continue
			}

			events, err := extractEmptyLBBackendsEvents([]byte(out))
			if err != nil || len(events) == 0 {
				continue
			}

			for _, event := range events {
				_, _, err := util.RunOVNSbctl("destroy", "controller_event", event.uuid)
				if err != nil {
					// Don't unidle until we are able to remove the controller event
					logrus.Errorf("Unable to remove controller event %s", event.uuid)
					continue
				}
				if serviceName, ok := oc.GetServiceVIPToName(event.vip, event.protocol); ok {
					serviceRef := kapi.ObjectReference{
						Kind:      "Service",
						Namespace: serviceName.Namespace,
						Name:      serviceName.Name,
					}
					logrus.Debugf("Sending a NeedPods event for service %s in namespace %s.", serviceName.Name, serviceName.Namespace)
					recorder.Eventf(&serviceRef, kapi.EventTypeNormal, "NeedPods", "The service %s needs pods", serviceName.Name)
				}
			}
		case <-stopChan:
			return
		}
	}
}

// WatchPods starts the watching of Pod resource and calls back the appropriate handler logic
func (oc *Controller) WatchPods() error {
	_, err := oc.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			if pod.Spec.NodeName != "" {
				oc.addLogicalPort(pod)
			}
		},
		UpdateFunc: func(old, newer interface{}) {
			podNew := newer.(*kapi.Pod)
			podOld := old.(*kapi.Pod)
			if podOld.Spec.NodeName == "" && podNew.Spec.NodeName != "" {
				oc.addLogicalPort(podNew)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			oc.deleteLogicalPort(pod)
		},
	}, oc.syncPods)
	return err
}

// WatchServices starts the watching of Service resource and calls back the
// appropriate handler logic
func (oc *Controller) WatchServices() error {
	_, err := oc.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) {},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*kapi.Service)
			oc.deleteService(service)
		},
	}, oc.syncServices)
	return err
}

// WatchEndpoints starts the watching of Endpoint resource and calls back the appropriate handler logic
func (oc *Controller) WatchEndpoints() error {
	_, err := oc.watchFactory.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.AddEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in adding load balancer: %v", err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			epNew := new.(*kapi.Endpoints)
			epOld := old.(*kapi.Endpoints)
			if reflect.DeepEqual(epNew.Subsets, epOld.Subsets) {
				return
			}
			if len(epNew.Subsets) == 0 {
				err := oc.deleteEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in deleting endpoints - %v", err)
				}
			} else {
				err := oc.AddEndpoints(epNew)
				if err != nil {
					logrus.Errorf("Error in modifying endpoints: %v", err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			err := oc.deleteEndpoints(ep)
			if err != nil {
				logrus.Errorf("Error in deleting endpoints - %v", err)
			}
		},
	}, nil)
	return err
}

// WatchNetworkPolicy starts the watching of network policy resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNetworkPolicy() error {
	_, err := oc.watchFactory.AddPolicyHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.addNetworkPolicy(policy)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPolicy := old.(*kapisnetworking.NetworkPolicy)
			newPolicy := newer.(*kapisnetworking.NetworkPolicy)
			if !reflect.DeepEqual(oldPolicy, newPolicy) {
				oc.deleteNetworkPolicy(oldPolicy)
				oc.addNetworkPolicy(newPolicy)
			}
			return
		},
		DeleteFunc: func(obj interface{}) {
			policy := obj.(*kapisnetworking.NetworkPolicy)
			oc.deleteNetworkPolicy(policy)
			return
		},
	}, oc.syncNetworkPolicies)
	return err
}

// WatchNamespaces starts the watching of namespace resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNamespaces() error {
	_, err := oc.watchFactory.AddNamespaceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.AddNamespace(ns)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldNs, newNs := old.(*kapi.Namespace), newer.(*kapi.Namespace)
			oc.updateNamespace(oldNs, newNs)
			return
		},
		DeleteFunc: func(obj interface{}) {
			ns := obj.(*kapi.Namespace)
			oc.deleteNamespace(ns)
			return
		},
	}, oc.syncNamespaces)
	return err
}

func (oc *Controller) handleNodeGateway(node *kapi.Node) error {
	var gatewayConfigured bool
	subnet, err := parseNodeHostSubnet(node)
	if err == nil {
		mode := node.Annotations[OvnNodeGatewayMode]
		if mode != string(config.GatewayModeDisabled) {
			if err = oc.syncGatewayLogicalNetwork(node, mode, subnet.String()); err == nil {
				gatewayConfigured = true
			} else {
				logrus.Errorf("error creating gateway for node %s: %v", node.Name, err)
			}
		}
	}

	if !gatewayConfigured {
		if err := util.GatewayCleanup(node.Name, subnet); err != nil {
			return fmt.Errorf("error cleaning up gateway for node %s: %v", node.Name, err)
		}
	}

	return nil
}

// WatchNodes starts the watching of node resource and calls
// back the appropriate handler logic
func (oc *Controller) WatchNodes(nodeSelector *metav1.LabelSelector) error {
	gatewaysHandled := make(map[string]bool)
	_, err := oc.watchFactory.AddFilteredNodeHandler(nodeSelector, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Added event for Node %q", node.Name)
			hostSubnet, err := oc.addNode(node)
			if err != nil {
				logrus.Errorf("error creating subnet for node %s: %v", node.Name, err)
				return
			}

			err = oc.syncNodeManagementPort(node, hostSubnet)
			if err != nil {
				logrus.Errorf("error creating Node Management Port for node %s: %v", node.Name, err)
			}

			if err := oc.handleNodeGateway(node); err != nil {
				logrus.Errorf(err.Error())
			} else {
				gatewaysHandled[node.Name] = true
			}
		},
		UpdateFunc: func(old, new interface{}) {
			oldNode := old.(*kapi.Node)
			node := new.(*kapi.Node)
			oldMacAddress, _ := oldNode.Annotations[OvnNodeManagementPortMacAddress]
			macAddress, _ := node.Annotations[OvnNodeManagementPortMacAddress]
			logrus.Debugf("Updated event for Node %q", node.Name)
			if oldMacAddress != macAddress {
				err := oc.syncNodeManagementPort(node, nil)
				if err != nil {
					logrus.Errorf("error update Node Management Port for node %s: %v", node.Name, err)
				}
			}

			if !gatewaysHandled[node.Name] || gatewayChanged(oldNode, node) {
				if err := oc.handleNodeGateway(node); err != nil {
					logrus.Errorf(err.Error())
				} else {
					gatewaysHandled[node.Name] = true
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*kapi.Node)
			logrus.Debugf("Delete event for Node %q. Removing the node from "+
				"various caches", node.Name)

			nodeSubnet, _ := parseNodeHostSubnet(node)
			err := oc.deleteNode(node.Name, nodeSubnet)
			if err != nil {
				logrus.Error(err)
			}
			oc.lsMutex.Lock()
			delete(oc.gatewayCache, node.Name)
			delete(oc.logicalSwitchCache, node.Name)
			oc.lsMutex.Unlock()
			delete(gatewaysHandled, node.Name)
			if oc.defGatewayRouter == "GR_"+node.Name {
				delete(oc.loadbalancerGWCache, TCP)
				delete(oc.loadbalancerGWCache, UDP)
				oc.defGatewayRouter = ""
				oc.handleExternalIPsLB()
			}
		},
	}, oc.syncNodes)
	return err
}

// AddServiceVIPToName associates a k8s service name with a load balancer VIP
func (oc *Controller) AddServiceVIPToName(vip string, protocol kapi.Protocol, namespace, name string) {
	oc.serviceVIPToNameLock.Lock()
	defer oc.serviceVIPToNameLock.Unlock()
	oc.serviceVIPToName[ServiceVIPKey{vip, protocol}] = types.NamespacedName{namespace, name}
}

// GetServiceVIPToName retrieves the associated k8s service name for a load balancer VIP
func (oc *Controller) GetServiceVIPToName(vip string, protocol kapi.Protocol) (types.NamespacedName, bool) {
	oc.serviceVIPToNameLock.Lock()
	defer oc.serviceVIPToNameLock.Unlock()
	namespace, ok := oc.serviceVIPToName[ServiceVIPKey{vip, protocol}]
	return namespace, ok
}

// gatewayChanged() compares old annotations to new and returns true if something has changed.
func gatewayChanged(oldNode, newNode *kapi.Node) bool {

	if newNode.Annotations[OvnNodeGatewayMode] != oldNode.Annotations[OvnNodeGatewayMode] {
		return true
	}

	if newNode.Annotations[OvnNodeGatewayVlanID] != oldNode.Annotations[OvnNodeGatewayVlanID] {
		return true
	}

	if newNode.Annotations[OvnNodeGatewayIfaceID] != oldNode.Annotations[OvnNodeGatewayIfaceID] {
		return true
	}

	if newNode.Annotations[OvnNodeGatewayMacAddress] != oldNode.Annotations[OvnNodeGatewayMacAddress] {
		return true
	}

	if newNode.Annotations[OvnNodeGatewayIP] != oldNode.Annotations[OvnNodeGatewayIP] {
		return true
	}

	if newNode.Annotations[OvnNodeGatewayNextHop] != oldNode.Annotations[OvnNodeGatewayNextHop] {
		return true
	}

	return false
}
