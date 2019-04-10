package ovn

import (
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/dbcache"
	"github.com/TomCodeLV/OVSDB-golang-lib/pkg/ovsdb"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/sirupsen/logrus"
	kapi "k8s.io/api/core/v1"
	kapisnetworking "k8s.io/api/networking/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"reflect"
	"strings"
	"sync"
)

// Controller structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints)
type Controller struct {
	kube           kube.Interface
	nodePortEnable bool
	watchFactory   *factory.WatchFactory

	gatewayCache map[string]string
	// For TCP and UDP type traffic, cache OVN load-balancers used for the
	// cluster's east-west traffic.
	loadbalancerClusterCache map[string]string

	// For TCP and UDP type traffice, cache OVN load balancer that exists on the
	// default gateway
	loadbalancerGWCache map[string]string

	// A cache of all logical switches seen by the watcher
	logicalSwitchCache map[string]bool

	// A cache of all logical ports seen by the watcher and
	// its corresponding logical switch
	logicalPortCache map[string]string

	// A cache of all logical ports and its corresponding uuids.
	logicalPortUUIDCache map[string]string

	// For each namespace, an address_set that has all the pod IP
	// address in that namespace
	namespaceAddressSet map[string]map[string]bool

	// For each namespace, a lock to protect critical regions
	namespaceMutex map[string]*sync.Mutex

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

	// supports port_group?
	portGroupSupport bool

	// NB database handle
	ovnNBDB *ovsdb.OVSDB

	// NB database cache handle
	ovnNbCache *dbcache.Cache
}

const (
	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"
)

// dialNB dials into NorthBound database. Initialize callback creates a cache where
// all required data will be available for later use.
func dialNB() (*ovsdb.OVSDB, *dbcache.Cache) {
	var addrList [][]string

	addresses := strings.Split(config.OvnNorth.ClientAuth.OvnAddressForClient,
		",")

	var options map[string]interface{}
	var useSSL bool
	for _, address := range addresses {
		addr := strings.SplitN(address, ":", 2)

		if addr[0] == "ssl" {
			addr = append(addr, config.OvnNorth.ClientAuth.Cert)
			addr = append(addr, config.OvnNorth.ClientAuth.PrivKey)
			addr = append(addr, config.OvnNorth.ClientAuth.CACert)

			useSSL = true

		}
		addrList = append(addrList, addr)
	}

	if useSSL {
		options = map[string]interface{}{
			"ServerName":         "ovnnb id:ac380054-0a6b-4a3c-9f2d-16a9eb55a89f",
			"InsecureSkipVerify": true,
		}
	}

	nbCache := new(dbcache.Cache)
	db := ovsdb.Dial(addrList, func(db *ovsdb.OVSDB) error {
		// initialize cache
		tmpCache, err := db.Cache(ovsdb.Cache{
			Schema: "OVN_Northbound",
			Tables: map[string][]string{
				"Logical_Switch":      {"_uuid", "name", "ports", "acls", "external_ids", "other_config"},
				"Logical_Switch_Port": {"_uuid", "name", "external_ids", "addresses", "dynamic_addresses"},
				"ACL":            {"_uuid", "external_ids", "match", "action"},
				"Address_Set":    {"_uuid", "name", "external_ids"},
				"Port_Group":     {"_uuid", "name", "acls", "ports"},
				"Load_Balancer":  {"_uuid", "vips", "protocol", "external_ids"},
				"Logical_Router": {"_uuid", "name", "options", "external_ids"},
			},
			Indexes: map[string][]string{
				"Logical_Switch_Port": {"name"},
				"Logical_Switch":      {"name"},
				"Address_Set":         {"name"},
				"Logical_Router":      {"name"},
				"Port_Group":          {"name"},
			},
		})

		// fallback for older version
		if err != nil && err.Error() == "syntax error: no table named Port_Group ()" {
			tmpCache, err = db.Cache(ovsdb.Cache{
				Schema: "OVN_Northbound",
				Tables: map[string][]string{
					"Logical_Switch":      {"_uuid", "name", "ports", "acls", "external_ids", "other_config"},
					"Logical_Switch_Port": {"_uuid", "name", "external_ids", "addresses", "dynamic_addresses"},
					"ACL":            {"_uuid", "external_ids", "match", "action"},
					"Address_Set":    {"_uuid", "name", "external_ids"},
					"Load_Balancer":  {"_uuid", "vips", "protocol", "external_ids"},
					"Logical_Router": {"_uuid", "name", "options", "external_ids"},
				},
				Indexes: map[string][]string{
					"Logical_Switch_Port": {"name"},
					"Logical_Switch":      {"name"},
					"Address_Set":         {"name"},
					"Logical_Router":      {"name"},
				},
			})
		}

		if err == nil {
			*nbCache = *tmpCache
			return nil
		}

		logrus.Errorf("Error in NB cache: %v", err)
		return err
	}, options)

	return db, nbCache
}

// NewOvnController creates a new OVN controller for creating logical network
// infrastructure and policy
func NewOvnController(kubeClient kubernetes.Interface, wf *factory.WatchFactory, nodePortEnable bool) *Controller {
	db, tmpCache := dialNB()
	return &Controller{
		kube:                     &kube.Kube{KClient: kubeClient},
		watchFactory:             wf,
		logicalSwitchCache:       make(map[string]bool),
		logicalPortCache:         make(map[string]string),
		logicalPortUUIDCache:     make(map[string]string),
		namespaceAddressSet:      make(map[string]map[string]bool),
		namespacePolicies:        make(map[string]map[string]*namespacePolicy),
		namespaceMutex:           make(map[string]*sync.Mutex),
		lspIngressDenyCache:      make(map[string]int),
		lspEgressDenyCache:       make(map[string]int),
		lspMutex:                 &sync.Mutex{},
		gatewayCache:             make(map[string]string),
		loadbalancerClusterCache: make(map[string]string),
		loadbalancerGWCache:      make(map[string]string),
		nodePortEnable:           nodePortEnable,
		ovnNBDB:                  db,
		ovnNbCache:               tmpCache,
	}
}

// Run starts the actual watching. Also initializes any local structures needed.
func (oc *Controller) Run() error {
	_, _, err := util.RunOVNNbctlHA("--columns=_uuid", "list",
		"port_group")
	if err == nil {
		oc.portGroupSupport = true
	}

	for _, f := range []func() error{oc.WatchPods, oc.WatchServices, oc.WatchEndpoints, oc.WatchNamespaces, oc.WatchNetworkPolicy} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
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
			oc.AddNetworkPolicy(policy)
			return
		},
		UpdateFunc: func(old, newer interface{}) {
			oldPolicy := old.(*kapisnetworking.NetworkPolicy)
			newPolicy := newer.(*kapisnetworking.NetworkPolicy)
			if !reflect.DeepEqual(oldPolicy, newPolicy) {
				oc.deleteNetworkPolicy(oldPolicy)
				oc.AddNetworkPolicy(newPolicy)
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
			// We only use namespace's name and that does not get updated.
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
