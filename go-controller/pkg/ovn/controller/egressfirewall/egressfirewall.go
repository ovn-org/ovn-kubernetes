package egressfirewall

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	efapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	efclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/clientset/versioned"
	efinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers/externalversions/egressfirewall/v1"
	eflister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"

	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

const (
	controllerName = "ovn-ef-controller"
	// maxRetries is trrhe number of itimes an object will be retried before it is dropped out of the queue.
	// With the current rate-limirter in use (5ms*2^(maxRetries-1)) the following number represent the
	// sequence of delays between successive queings of an object
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	egressFirewallDNSDefaultDuration = 30 * time.Minute
	egressFirewallAppliedCorrectly   = "EgressFirewall Rules applied"
	egressFirewallError              = "EgressFirewall Rules not applied correctly"
)

// NewController returns a new *Controller
func NewController(client clientset.Interface,
	kube kube.Interface,
	efClient efclientset.Interface,
	efInformer efinformer.EgressFirewallInformer,
	addressSetFactory addressset.AddressSetFactory,
	watchFactory *factory.WatchFactory,
) *Controller {
	klog.V(4).Info("Creating event broadcaster for egressfirewall controller")
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})

	c := &Controller{
		efClient:          efClient,
		kube:              kube,
		eventBroadcaster:  broadcaster,
		eventRecorder:     recorder,
		queue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		workerLoopPeriod:  time.Second,
		addressSetFactory: addressSetFactory,
		watchFactory:      watchFactory,
	}

	var err error
	c.egressFirewallDNS, err = NewEgressDNS(c.addressSetFactory)
	if err != nil {
		klog.Errorf("Failed to initialize the EgressDNS resolver: %+v", err)
		return nil
	}

	// egressFirewalls
	klog.Info("Setting up event handlers for egressfirewalls")
	efInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEFAdd,
		UpdateFunc: c.onEFUpdate,
		DeleteFunc: c.onEFDelete,
	})
	c.efLister = efInformer.Lister()

	c.efsSynced = efInformer.Informer().HasSynced

	// repair controller
	c.repair = NewRepair(0, kube, c.efLister, watchFactory)

	return c
}

// onEFAdd queues the EgressFirewall for processing.
func (c *Controller) onEFAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding EgressFirewall %s", key)
	c.queue.Add(key)
}

// onEFDelete queues the EgressFirewall for processing.
func (c *Controller) onEFDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting EgressFirewall %s", key)
	c.queue.Add(key)
}

// onEFUpdate queues the EgressFirewall for processing.
func (c *Controller) onEFUpdate(oldObj, newObj interface{}) {
	oldEF := oldObj.(*efapi.EgressFirewall).DeepCopy()
	newEF := newObj.(*efapi.EgressFirewall).DeepCopy()

	// don't process resync or objects that are marked for deletion
	if oldEF.ResourceVersion == newEF.ResourceVersion ||
		!newEF.GetDeletionTimestamp().IsZero() {
		return
	}
	// only process updates to the spec, ignoring changes in status
	if reflect.DeepEqual(oldEF.Spec, newEF.Spec) {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.queue.Add(key)
	}
}

// Controller manages egressFirewalls on namespaces.
type Controller struct {
	efClient         efclientset.Interface
	kube             kube.Interface
	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	// egressFirewallDNS manages the dns portion of egressfirewall rules
	egressFirewallDNS *EgressDNS

	// egressFirewalls is a sync map of all egressfirewalls in the cluster keyed on the namespace name
	egressFirewalls sync.Map
	// addressSetFactory is able to create and destroy addressSets and is populated by the AddressSetFactory
	// passed to NewController
	addressSetFactory addressset.AddressSetFactory
	// watchFactory is able to getNodes and is populated by the watchfactory passed to NewController
	watchFactory *factory.WatchFactory

	// efLister is able to list/get egressfirewall and is populated by  the shared informer
	// passed to Controller
	efLister eflister.EgressFirewallLister
	// efsSynced returns true if the egressFirewall shared informer
	// has been synced at least once
	efsSynced cache.InformerSynced

	// Egressfirewalls that need to be updated.
	queue workqueue.RateLimitingInterface

	// workerLoop  Period is the time between worker runs. The workers process the queue of
	// egressfirewall changes
	workerLoopPeriod time.Duration

	// repair contains a controller that keeps in sync OVN and Kubernetes egressFirewalls
	repair *Repair
}

// Run will not return until stopCh is closed. workers determines how many
// egressFirewalls will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}, runRepair bool) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.efsSynced) {
		return fmt.Errorf("error syncing cache")
	}

	if runRepair {
		// Run rthe repair controller only once
		// it keeps in sync Kubernetes and OVN
		// and handles removal of stale data on upgrades
		klog.Info("Remove stale OVN EgressFirewalls")
		if err := c.repair.runOnce(); err != nil {
			klog.Errorf("Error repairing EgressFirewall: %v", err)
		}
	}

	klog.Info("Started EgressFirewall DNS resolver")
	c.egressFirewallDNS.Run(egressFirewallDNSDefaultDuration, stopCh)

	// Start the workers after the repair to avoid races
	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	<-stopCh
	return nil
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same EgressFirewall
// at the same time
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	eKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(eKey)

	err := c.syncEgressFirewalls(eKey.(string))
	c.handleErr(err, eKey)

	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	ns, name, keyErr := cache.SplitMetaNamespaceKey(key.(string))
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}
	metrics.MetricRequeueEgressFirewallCount.WithLabelValues(key.(string)).Inc()

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing egressfirewall, retrying", "egressfirewall", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping egressfirewall %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (c *Controller) syncEgressFirewalls(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for egressfirewall %s on namespace %s", name, namespace)
	metrics.MetricSyncEgressFirewallCount.WithLabelValues(key).Inc()

	defer func() {
		klog.V(4).Infof("Finished syncing egressfirewall %s on namespace %s : %v", name, namespace, time.Since(startTime))
		metrics.MetricSyncEgressFirewallLatency.WithLabelValues(key).Observe(time.Since(startTime).Seconds())
	}()

	// Get current egressFirewall from the cache
	ef, err := c.efLister.EgressFirewalls(namespace).Get(name)
	// It's unlikely that we have an error different from "Not Found Object"
	// because we are getting the object from the infortmer's cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	txn := util.NewNBTxn()
	// if we do recive IsNotFound(err) that means the object being processed is no longer in kube apiserver and is to be deleted
	if apierrors.IsNotFound(err) {
		err = c.deleteEgressFirewall(namespace, txn)
		if err != nil {
			return fmt.Errorf("error deleting EgressFirewall %s/%s: %+v", namespace, name, err)
		}
	}

	if ef != nil {
		// if there is an egressfirewall object in the cache the operation is either add or update
		err = c.addOrUpdateEgressFirewall(ef, txn)
		if err != nil {
			ef.Status.Status = egressFirewallError
			updateErr := c.updateEgressFirewallWithRetry(ef)
			if updateErr != nil {
				klog.Errorf("Error updating Status of egressfirewall %s/%s: %+v", namespace, name, updateErr)
			}
			return fmt.Errorf("error adding/updating EgressFirewall %s/%s: %+v", namespace, name, err)
		}
	}
	// regardless of operation commit changes to the database
	_, stderr, err := txn.Commit()
	if err != nil {
		if ef != nil {
			// if there is an error commiting database changes and an egressFirewall exists update the status
			ef.Status.Status = egressFirewallError
			updateErr := c.updateEgressFirewallWithRetry(ef)
			if updateErr != nil {
				klog.Errorf("Error updating Status of egressfirewall %s/%s: %+v", namespace, name, updateErr)
			}
		}
		return fmt.Errorf("failed to commit db changes for egressFirewall in namespace %s stderr: %q, err: %+v", ef.Namespace, stderr, err)

	}
	if ef != nil {
		// on successful completion of database changes update the status of egressFirewall object
		ef.Status.Status = egressFirewallAppliedCorrectly
		updateErr := c.updateEgressFirewallWithRetry(ef)
		if updateErr != nil {
			return fmt.Errorf("error updating Status of egressfirewall %s/%s: %+v", namespace, name, updateErr)
		}
	}
	return nil
}

type egressFirewall struct {
	sync.Mutex
	name        string
	namespace   string
	egressRules []*egressFirewallRule
}

type egressFirewallRule struct {
	id     int
	access efapi.EgressFirewallRuleType
	ports  []efapi.EgressFirewallPort
	to     destination
}

type destination struct {
	cidrSelector string
	dnsName      string
}

// cloneEgressFirewall shallow copies the efapi.EgressFirewall object provided.
// This concretely means that it create a new efapi.EgressFirewall with the name and
// namespace set, but without any rules specified.
func cloneEgressFirewall(originalEgressfirewall *efapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        originalEgressfirewall.Name,
		namespace:   originalEgressfirewall.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

// newEgressFirewallRule parses the egressFirewall object from the kube-apiserver and translates it into a friendlier
// internal data struct
func newEgressFirewallRule(rawEgressFirewallRule efapi.EgressFirewallRule, id int) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	if rawEgressFirewallRule.To.DNSName != "" {
		efr.to.dnsName = rawEgressFirewallRule.To.DNSName
	} else {

		_, _, err := net.ParseCIDR(rawEgressFirewallRule.To.CIDRSelector)
		if err != nil {
			return nil, err
		}
		efr.to.cidrSelector = rawEgressFirewallRule.To.CIDRSelector
	}
	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

// addOrUpdateEgressFirewall handles either adding a new egressFirewall or updating an already existing egressFirewall.
// The major difference between update and Add is for updates the old object gets deleted first
func (c *Controller) addOrUpdateEgressFirewall(egressFirewallObj *efapi.EgressFirewall, txn *util.NBTxn) error {
	ef := cloneEgressFirewall(egressFirewallObj)
	ef.Lock()
	defer ef.Unlock()
	// egressFirewalls can only be named "default" so kube api-server enforces one egressfirewall object per namespace
	// if there is already an egressFirewall this is an update operation
	if loadedEF, loaded := c.egressFirewalls.LoadOrStore(egressFirewallObj.Namespace, ef); loaded {
		klog.Infof("Updating egressFirewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
		if loadedEF != nil {
			oldEF := loadedEF.(*egressFirewall)
			err := c.deleteEgressFirewall(oldEF.namespace, txn)
			if err != nil {
				return fmt.Errorf("failed to delete egressFirewall %s in namespace %s as part of updating",
					egressFirewallObj.Name,
					egressFirewallObj.Namespace)
			}
			c.egressFirewalls.Store(egressFirewallObj.Namespace, ef)
		}
	} else {
		klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewallObj.Name, egressFirewallObj.Namespace)
	}

	var addErrors error
	egressFirewallStartPriorityInt, err := strconv.Atoi(types.EgressFirewallStartPriority)
	if err != nil {
		return fmt.Errorf("failed to convert egressFirewallStartPriority to Integer: cannot add egressFirewall for namespace %s", egressFirewallObj.Namespace)
	}
	minimumReservedEgressFirewallPriorityInt, err := strconv.Atoi(types.MinimumReservedEgressFirewallPriority)
	if err != nil {
		return fmt.Errorf("failed to convert minumumReservedEgressFirewallPriority to Integer: cannot add egressFirewall for namespace %s", egressFirewallObj.Namespace)
	}
	for i, egressFirewallRule := range egressFirewallObj.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > egressFirewallStartPriorityInt-minimumReservedEgressFirewallPriorityInt {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored",
				egressFirewallObj.Namespace)
			break
		}
		efr, err := newEgressFirewallRule(egressFirewallRule, i)
		if err != nil {
			addErrors = errors.Wrapf(addErrors, "error: cannot create EgressFirewall Rule to destination %s for namespace %s - %v",
				egressFirewallRule.To.CIDRSelector, egressFirewallObj.Namespace, err)
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if addErrors != nil {
		return addErrors
	}

	// EgressFirewall needs to make sure that the address_set for the namespace exists independently of the namespace object
	// so that OVN doesn't get unresolved references to the address_set.
	// TODO: This should go away once we do something like refcounting for address_sets.
	err = c.addressSetFactory.EnsureAddressSet(egressFirewallObj.Namespace)
	if err != nil {
		return fmt.Errorf("cannot Ensure that addressSet for namespace %s exists %v", egressFirewallObj.Namespace, err)
	}
	ipv4HashedAS, ipv6HashedAS := addressset.MakeAddressSetHashNames(egressFirewallObj.Namespace)
	err = c.addEgressFirewallRules(ef, ipv4HashedAS, ipv6HashedAS, egressFirewallStartPriorityInt, txn)
	if err != nil {
		return err
	}

	return nil
}

func (c *Controller) deleteEgressFirewall(namespace string, txn *util.NBTxn) error {
	deleteDNS := false
	obj, loaded := c.egressFirewalls.LoadAndDelete(namespace)
	if !loaded {
		return fmt.Errorf("there is no egressFirewall found in namespace %s", namespace)
	}

	ef, _ := obj.(*egressFirewall)
	klog.Infof("Deleting egressFirewall %s in namespace %s", ef.name, ef.namespace)

	ef.Lock()
	defer ef.Unlock()
	for _, rule := range ef.egressRules {
		if len(rule.to.dnsName) > 0 {
			deleteDNS = true
			break
		}
	}
	if deleteDNS {
		c.egressFirewallDNS.Delete(namespace)
	}
	nodes, err := c.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("unable to setup egress firewall ACLs on cluster nodes, err: %v", err)
	}

	return deleteEgressFirewallRules(namespace, nodes, txn)
}

// updateEgressFirewallWithRetry tries to update the egressFirewall kube object
func (c *Controller) updateEgressFirewallWithRetry(egressfirewall *efapi.EgressFirewall) error {
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return c.kube.UpdateEgressFirewall(egressfirewall)
	})
	if retryErr != nil {
		return fmt.Errorf("error in updating status on EgressFirewall %s/%s: %v",
			egressfirewall.Namespace, egressfirewall.Name, retryErr)
	}
	return nil
}

// addEgressFirewallRules takes the egressFirewall generates the `match=` section of the ACL and ensures that the ovn command gets added to the transaction
func (c *Controller) addEgressFirewallRules(ef *egressFirewall, hashedAddressSetNameIPv4, hashedAddressSetNameIPv6 string, efStartPriority int, txn *util.NBTxn) error {
	for _, rule := range ef.egressRules {
		var action string
		var matchTargets []matchTarget
		if rule.access == efapi.EgressFirewallRuleAllow {
			action = "allow"
		} else {
			action = "drop"
		}
		if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTargets = []matchTarget{{matchKindV6CIDR, rule.to.cidrSelector}}
			} else {
				matchTargets = []matchTarget{{matchKindV4CIDR, rule.to.cidrSelector}}
			}
		} else {
			// rule based on DNS NAME
			dnsNameAddressSets, err := c.egressFirewallDNS.Add(ef.namespace, rule.to.dnsName)
			if err != nil {
				return fmt.Errorf("error with EgressFirewallDNS - %v", err)
			}
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName})
			}
			if dnsNameIPv6ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName})
			}
		}
		match := generateMatch(hashedAddressSetNameIPv4, hashedAddressSetNameIPv6, matchTargets, rule.ports)
		err := c.createEgressFirewallRules(efStartPriority-rule.id, match, action, ef.namespace, txn)
		if err != nil {
			return err
		}
	}
	return nil
}

// createEgressFirewallRules uses the previously generated elements and creates the
// logical_router_policy/join_switch_acl for a specific egressFirewallRouter
func (c *Controller) createEgressFirewallRules(priority int, match, action, externalID string, txn *util.NBTxn) error {
	logicalSwitches := []string{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		nodes, err := c.watchFactory.GetNodes()
		if err != nil {
			return fmt.Errorf("unable to setup egress firewall ACLs on cluster nodes, err: %v", err)
		}
		for _, node := range nodes {
			logicalSwitches = append(logicalSwitches, node.Name)
		}
	} else {
		logicalSwitches = append(logicalSwitches, types.OVNJoinSwitch)
	}
	uuids, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=_uuid", "find", "ACL", match, "action="+action,
		fmt.Sprintf("external-ids:egressFirewall=%s", externalID))
	if err != nil {
		return fmt.Errorf("error executing find ACL command, stderr: %q, %+v", stderr, err)
	}
	sort.Strings(logicalSwitches)
	for _, logicalSwitch := range logicalSwitches {
		if uuids == "" {
			id := fmt.Sprintf("%s-%d", logicalSwitch, priority)
			_, stderr, err := txn.AddOrCommit([]string{"--id=@" + id, "create", "acl",
				fmt.Sprintf("priority=%d", priority),
				fmt.Sprintf("direction=%s", types.DirectionToLPort), match, "action=" + action,
				fmt.Sprintf("external-ids:egressFirewall=%s", externalID),
				"--", "add", "logical_switch", logicalSwitch,
				"acls", "@" + id})
			if err != nil {
				return fmt.Errorf("failed to commit db changes for egressFirewall  stderr: %q, err: %+v", stderr, err)
			}

		} else {
			for _, uuid := range strings.Split(uuids, "\n") {
				//some of the lines returned are blank, skip those
				if uuid == "" {
					continue
				}
				_, stderr, err := txn.AddOrCommit([]string{"add", "logical_switch", logicalSwitch, "acls", uuid})
				if err != nil {
					return fmt.Errorf("failed to commit db changes for egressFirewall stderr: %q, err: %+v", stderr, err)
				}
			}
		}
	}
	return nil
}

// deleteEgressFirewallRules delete the specific logical router policy/join switch Acls
func deleteEgressFirewallRules(externalID string, nodes []*kapi.Node, txn *util.NBTxn) error {
	logicalSwitches := []string{}
	if config.Gateway.Mode == config.GatewayModeLocal {
		for _, node := range nodes {
			logicalSwitches = append(logicalSwitches, node.Name)
		}
	} else {
		logicalSwitches = []string{types.OVNJoinSwitch}
	}
	sort.Strings(logicalSwitches)
	stdout, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "ACL",
		fmt.Sprintf("external-ids:egressFirewall=%s", externalID))
	if err != nil {
		return fmt.Errorf("error deleting egressFirewall with external-ids %s, cannot get ACL policies - %s:%s",
			externalID, err, stderr)
	}
	uuids := strings.Fields(stdout)
	for _, logicalSwitch := range logicalSwitches {
		for _, uuid := range uuids {
			_, stderr, err = txn.AddOrCommit([]string{"remove", "logical_switch", logicalSwitch, "acls", uuid})
			if err != nil {
				return fmt.Errorf("failed to commit db changes for egressFirewall stderr: %q, err: %+v", stderr, err)
			}
		}
	}
	return nil
}

type matchTarget struct {
	kind  matchKind
	value string
}

type matchKind int

const (
	matchKindV4CIDR matchKind = iota
	matchKindV6CIDR
	matchKindV4AddressSet
	matchKindV6AddressSet
)

func (m *matchTarget) toExpr() (string, error) {
	switch m.kind {
	case matchKindV4CIDR:
		return fmt.Sprintf("ip4.dst == %s", m.value), nil
	case matchKindV6CIDR:
		return fmt.Sprintf("ip6.dst == %s", m.value), nil
	case matchKindV4AddressSet:
		if m.value != "" {
			return fmt.Sprintf("ip4.dst == $%s", m.value), nil
		}
		return "", nil
	case matchKindV6AddressSet:
		if m.value != "" {
			return fmt.Sprintf("ip6.dst == $%s", m.value), nil
		}
		return "", nil
	}
	return "", fmt.Errorf("invalid MatchKind")
}

// generateMatch generates the "match" section of ACL generation for egressFirewallRules.
// It is referentially transparent as all the elements have been validated before this function is called
// sample output:
// match=\"(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14\
func generateMatch(ipv4Source, ipv6Source string, destinations []matchTarget, dstPorts []efapi.EgressFirewallPort) string {
	var src string
	var dst string
	var extraMatch string
	switch {
	case config.IPv4Mode && config.IPv6Mode:
		src = fmt.Sprintf("(ip4.src == $%s || ip6.src == $%s)", ipv4Source, ipv6Source)
	case config.IPv4Mode:
		src = fmt.Sprintf("ip4.src == $%s", ipv4Source)
	case config.IPv6Mode:
		src = fmt.Sprintf("ip6.src == $%s", ipv6Source)
	}

	for _, entry := range destinations {
		if entry.value == "" {
			continue
		}
		ipDst, err := entry.toExpr()
		if err != nil {
			klog.Error(err)
			continue
		}
		if dst == "" {
			dst = ipDst
		} else {
			dst = strings.Join([]string{dst, ipDst}, " || ")
		}
	}

	match := fmt.Sprintf("match=\"(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}

	if config.Gateway.Mode == config.GatewayModeLocal {
		extraMatch = getClusterSubnetsExclusion()
	} else {
		extraMatch = fmt.Sprintf("inport == \\\"%s%s\\\"", types.JoinSwitchToGWRouterPrefix, types.OVNClusterRouter)
	}
	return fmt.Sprintf("%s && %s\"", match, extraMatch)
}

// egressGetL4Match generates the rules for when ports are specified in an egressFirewall Rule
// since the ports can be specified in any order in an egressFirewallRule the best way to build up
// a single rule is to build up each protocol as you walk through the list and place the appropriate logic
// between the elements.
func egressGetL4Match(ports []efapi.EgressFirewallPort) string {
	var udpString string
	var tcpString string
	var sctpString string
	for _, port := range ports {
		if kapi.Protocol(port.Protocol) == kapi.ProtocolUDP && udpString != "udp" {
			if port.Port == 0 {
				udpString = "udp"
			} else {
				udpString = fmt.Sprintf("%s udp.dst == %d ||", udpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolTCP && tcpString != "tcp" {
			if port.Port == 0 {
				tcpString = "tcp"
			} else {
				tcpString = fmt.Sprintf("%s tcp.dst == %d ||", tcpString, port.Port)
			}
		} else if kapi.Protocol(port.Protocol) == kapi.ProtocolSCTP && sctpString != "sctp" {
			if port.Port == 0 {
				sctpString = "sctp"
			} else {
				sctpString = fmt.Sprintf("%s sctp.dst == %d ||", sctpString, port.Port)
			}
		}
	}
	// build the l4 match
	var l4Match string
	type tuple struct {
		protocolName     string
		protocolFormated string
	}
	list := []tuple{
		{
			protocolName:     "udp",
			protocolFormated: udpString,
		},
		{
			protocolName:     "tcp",
			protocolFormated: tcpString,
		},
		{
			protocolName:     "sctp",
			protocolFormated: sctpString,
		},
	}
	for _, entry := range list {
		if entry.protocolName == entry.protocolFormated {
			if l4Match == "" {
				l4Match = fmt.Sprintf("(%s)", entry.protocolName)
			} else {
				l4Match = fmt.Sprintf("%s || (%s)", l4Match, entry.protocolName)
			}
		} else {
			if l4Match == "" && entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("(%s && (%s))", entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			} else if entry.protocolFormated != "" {
				l4Match = fmt.Sprintf("%s || (%s && (%s))", l4Match, entry.protocolName, entry.protocolFormated[:len(entry.protocolFormated)-2])
			}
		}
	}
	return fmt.Sprintf("(%s)", l4Match)
}

func getClusterSubnetsExclusion() string {
	var exclusion string
	for _, clusterSubnet := range config.Default.ClusterSubnets {
		if exclusion != "" {
			exclusion += " && "
		}
		if utilnet.IsIPv6CIDR(clusterSubnet.CIDR) {
			exclusion += fmt.Sprintf("%s.dst != %s", "ip6", clusterSubnet.CIDR)
		} else {
			exclusion += fmt.Sprintf("%s.dst != %s", "ip4", clusterSubnet.CIDR)
		}
	}
	return exclusion
}
