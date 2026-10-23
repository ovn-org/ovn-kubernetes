// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package egressfirewall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	egressfirewallapi "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressfirewallapply "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/applyconfiguration/egressfirewall/v1"
	v1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/informers/externalversions/egressfirewall/v1"
	v2 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1/apis/listers/egressfirewall/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/observability"
	dnsnameresolver "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/dns_name_resolver"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/batching"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

const (
	// transaction time to delete 80K ACLs from 2 port groups is ~4.5 sec.
	// transaction time to delete 80K acls from one port group and add them to another
	// is ~3 sec
	// Therefore this limit is safe enough to not exceed 10 sec transaction timeout.
	aclChangePGBatchSize = 80000
	// defaultControllerOwner is the DB Owner for all EF ACLs (UDN or CDN) and CDN namespaced port group.
	// This is due to legacy reasons and left alone for now to avoid upgrade issues. May be updated in the future.
	EgressFirewallAppliedCorrectly = "EgressFirewall Rules applied"
	egressFirewallName             = "default"
)

const (
	matchKindV4CIDR matchKind = iota
	matchKindV6CIDR
	matchKindV4AddressSet
	matchKindV6AddressSet
)

type egressFirewall struct {
	name        string
	namespace   string
	egressRules []*egressFirewallRule
}

type egressFirewallRule struct {
	id     int
	access egressfirewallapi.EgressFirewallRuleType
	ports  []egressfirewallapi.EgressFirewallPort
	to     destination
}

type destination struct {
	cidrSelector string
	dnsName      string
	// clusterSubnetIntersection if egress firewall rule CIDRSelector intersects with clusterSubnet.
	// Based on these values we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection []*net.IPNet
	// nodeName: nodeIPs
	nodeAddrs    map[string][]string
	nodeSelector *metav1.LabelSelector
}

type matchTarget struct {
	kind  matchKind
	value string
	// clusterSubnetIntersection is inherited from the egressFirewallRule destination.
	// clusterSubnetIntersection holds the cluster network CIDRs for default network or UDN.
	// Based on these values we can omit clusterSubnet exclusion from the related ACL.
	// For dns-based rules, EgressDNS won't add ips from clusterSubnet to the address set.
	clusterSubnetIntersection []*net.IPNet
}

type matchKind int

type cacheEntry struct {
	pgName            string
	hasNodeSelector   bool
	subnets           []*net.IPNet
	efResourceVersion string
	logHash           string
}

type EFController struct {
	name        string
	zone        string
	ruleCounter sync.Map
	// cache stores a mapping of rendered firewall namespace -> nodes + networks
	// only one firewall (named "default") can exist per namespace
	cache *syncmap.SyncMap[*cacheEntry]

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client
	kube     *kube.KubeOVN

	nodeLister      corelisters.NodeLister
	namespaceLister corelisters.NamespaceLister
	efLister        v2.EgressFirewallLister

	controller      controller.Controller
	nodeController  controller.Controller
	networkManager  networkmanager.Interface
	nadReconciler   networkmanager.NADReconciler
	nadReconcilerID uint64
	// dnsNameResolver is used for resolving the IP addresses of DNS names
	// used in egress firewall rules
	dnsNameResolver dnsnameresolver.DNSNameResolver
	observManager   *observability.Manager
}

func NewEFController(
	name string,
	zone string,
	kube *kube.KubeOVN,
	nbClient libovsdbclient.Client,
	namespaceLister corelisters.NamespaceLister,
	nodeInformer coreinformers.NodeInformer,
	efInformer v1.EgressFirewallInformer,
	networkManager networkmanager.Interface,
	dnsNameResolver dnsnameresolver.DNSNameResolver,
	observManager *observability.Manager,
) (*EFController, error) {
	c := &EFController{
		name:            name,
		zone:            zone,
		cache:           syncmap.NewSyncMap[*cacheEntry](),
		nbClient:        nbClient,
		kube:            kube,
		nodeLister:      nodeInformer.Lister(),
		namespaceLister: namespaceLister,
		efLister:        efInformer.Lister(),
		networkManager:  networkManager,
		dnsNameResolver: dnsNameResolver,
		observManager:   observManager,
		ruleCounter:     sync.Map{},
	}

	controllerConfig := &controller.ControllerConfig[egressfirewallapi.EgressFirewall]{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Informer:    efInformer.Informer(),
		Lister:      efInformer.Lister().List,
		MaxAttempts: controller.InfiniteAttempts,
		Reconcile:   c.sync,
		ObjNeedsUpdate: func(old, new *egressfirewallapi.EgressFirewall) bool {
			return old == nil || new == nil || !reflect.DeepEqual(old.Spec, new.Spec)
		},
		Threadiness: 1,
	}

	c.controller = controller.NewController(
		c.name,
		controllerConfig,
	)

	nodeControllerConfig := &controller.ControllerConfig[corev1.Node]{
		Informer:       nodeInformer.Informer(),
		Lister:         nodeInformer.Lister().List,
		MaxAttempts:    controller.InfiniteAttempts,
		ObjNeedsUpdate: efNodeNeedsUpdate,
		Reconcile:      c.updateEgressFirewallForNode,
		Threadiness:    1,
	}

	c.nodeController = controller.NewController(
		c.name+"-node",
		nodeControllerConfig,
	)

	// this controller does not feed from an informer, nads are added
	// to the queue by NAD Controller
	nadReconcilerConfig := &controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		Reconcile:   c.syncNAD,
		Threadiness: 1,
		MaxAttempts: controller.InfiniteAttempts,
	}
	c.nadReconciler = controller.NewReconciler(
		c.name+"-NAD",
		nadReconcilerConfig,
	)

	return c, nil
}

// Reconcile queues the key to the egress firewall controller
func (oc *EFController) Reconcile(key string) {
	oc.controller.Reconcile(key)
}

func (oc *EFController) syncNAD(key string) error {
	startTime := time.Now()
	controllerName := oc.name + "-NAD"
	klog.V(5).Infof("%s: sync  NAD %s", controllerName, key)
	defer func() {
		klog.V(4).Infof("%s: finished syncing NAD %s, took %v", controllerName, key, time.Since(startTime))
	}()

	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("%s: failed splitting key %s: %v", controllerName, key, err)
		return nil
	}

	// Only reconcile namespace EF for primary network changes. Secondary NADs are irrelevant for EF.
	if ni := oc.networkManager.GetNetInfoForNADKey(key); ni != nil && !ni.IsPrimaryNetwork() {
		return nil
	}
	// On delete, NAD controller has already removed the NAD from its cache, so we can't tell if it was
	// primary or secondary. Reconcile anyway as a safe fallback.
	oc.controller.Reconcile(namespace + "/" + egressFirewallName)
	return nil
}

// initialSync deletes stale db entries for Egress Firewalls that don't exist anymore.
// Egress firewall implementation had many versions, the latest one makes no difference for gateway modes, and creates
// ACLs on namespaced port groups.
func (oc *EFController) initialSync() error {
	egressFirewalls, err := oc.efLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("%s: failed to list egress firewalls: %w", oc.name, err)
	}

	existingEFNamespaces := map[string]bool{}
	for _, ef := range egressFirewalls {
		existingEFNamespaces[ef.Namespace] = true
	}

	// find all existing egress firewall ACLs
	predicateIDs := libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, types.DefaultNetworkControllerName, nil)
	aclP := libovsdbops.GetPredicate[*nbdb.ACL](predicateIDs, nil)
	efACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, aclP)
	if err != nil {
		return fmt.Errorf("cannot find Egress Firewall ACLs: %v", err)
	}

	getPGPredicate := func(acl *nbdb.ACL) func(item *nbdb.PortGroup) bool {
		return func(item *nbdb.PortGroup) bool {
			if len(item.ACLs) == 0 {
				return false
			}
			if item.ExternalIDs[libovsdbops.OwnerTypeKey.String()] != libovsdbops.NamespaceOwnerType {
				return false
			}
			if acl.ExternalIDs[libovsdbops.ObjectNameKey.String()] != item.ExternalIDs[libovsdbops.ObjectNameKey.String()] {
				return false
			}

			for _, aclUUID := range item.ACLs {
				if acl.UUID == aclUUID {
					return true
				}
			}
			return false
		}
	}

	var deletedNSACLs = map[string][]*nbdb.ACL{}
	// iterate EF ACLs, build cache with existing entries, track stale entries to be removed
	for _, acl := range efACLs {
		namespace := acl.ExternalIDs[libovsdbops.ObjectNameKey.String()]
		if !existingEFNamespaces[namespace] {
			deletedNSACLs[namespace] = append(deletedNSACLs[namespace], acl)
			continue
		}
		// Seed cache from NBDB for namespaces that have an EF and associated ACL/PG.
		if _, cached := oc.cache.Load(namespace); cached {
			continue
		}
		foundPGs, err := libovsdbops.FindPortGroupsWithPredicate(oc.nbClient, getPGPredicate(acl))
		if err != nil {
			return fmt.Errorf("failed to search for port groups during egress firewall cache seed: %w", err)
		}
		if len(foundPGs) > 0 {
			oc.cache.Store(namespace, &cacheEntry{pgName: foundPGs[0].Name})
		}
	}

	// remove stale ACLs
	err = batching.BatchMap[*nbdb.ACL](aclChangePGBatchSize, deletedNSACLs, func(batchNsACLs map[string][]*nbdb.ACL) error {
		var ops []ovsdb.Operation
		var err error
		for _, acls := range batchNsACLs {
			// find the port group that has the stale acl
			for _, acl := range acls {
				foundPGs, err := libovsdbops.FindPortGroupsWithPredicate(oc.nbClient, getPGPredicate(acl))
				if err != nil {
					return fmt.Errorf("failed to search for port groups during egress firewall ACL sync: %w", err)
				}
				if len(foundPGs) > 1 {
					pgNames := make([]string, len(foundPGs))
					for i, pg := range foundPGs {
						pgNames[i] = pg.Name
					}
					name := libovsdbops.GetACLName(acl)
					if name == "" {
						name = acl.UUID
					}
					klog.Warningf("Found multiple port groups associated with the same ACL %q: %s",
						name, strings.Join(pgNames, ","))
				}
				for _, pg := range foundPGs {
					// DeleteACLsFromPortGroupOps does not error if the port group or ACLs are already gone.
					ops, err = libovsdbops.DeleteACLsFromPortGroupOps(oc.nbClient, ops, pg.Name, acls...)
					if err != nil {
						return fmt.Errorf("failed to build cleanup ops: %w", err)
					}
				}
			}
		}
		_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
		if err != nil {
			return fmt.Errorf("failed to clean up egress firewall ACLs: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Delete stale address sets related to EgressFirewallDNS which are not referenced by any ACL.
	return oc.dnsNameResolver.DeleteStaleAddrSets(oc.nbClient)
}

func (oc *EFController) Start() (err error) {
	klog.Infof("Starting EgressFirewall controller")
	oc.nadReconcilerID = oc.networkManager.RegisterNADReconciler(oc.nadReconciler)
	defer func() {
		if err != nil {
			oc.networkManager.DeRegisterNADReconciler(oc.nadReconcilerID)
			oc.nadReconcilerID = 0
		}
	}()
	return controller.StartWithInitialSync(oc.initialSync, oc.controller, oc.nodeController, oc.nadReconciler)
}

func (oc *EFController) Stop() {
	klog.Infof("%s: shutting down", oc.name)
	if oc.nadReconcilerID != 0 {
		oc.networkManager.DeRegisterNADReconciler(oc.nadReconcilerID)
	}
	controller.Stop(oc.nodeController, oc.controller, oc.nadReconciler)
	oc.nadReconciler = nil
	oc.nadReconcilerID = 0
}

func (oc *EFController) sync(key string) (updateErr error) {
	klog.V(5).Infof("Syncing EgressFirewall %s", key)
	namespace, efName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key for egress firewall: %s", key)
	}
	oc.cache.LockKey(namespace)
	defer oc.cache.UnlockKey(namespace)

	existingEntry, _ := oc.cache.Load(namespace)
	var newEntry *cacheEntry

	ef, err := oc.efLister.EgressFirewalls(namespace).Get(efName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else {
		skipStatusUpdate := false
		defer func() {
			if skipStatusUpdate {
				return
			}
			if statusErr := oc.setEgressFirewallStatus(ef, updateErr); statusErr != nil {
				updateErr = utilerrors.Join(updateErr, fmt.Errorf("failed to update egress firewall status %s, error: %w",
					GetEgressFirewallNamespacedName(ef), statusErr))
			}
		}()

		activeNetwork, netErr := oc.networkManager.GetActiveNetworkForNamespace(namespace)
		switch {
		case netErr != nil && !util.IsInvalidPrimaryNetworkError(netErr):
			// Failed to resolve active network; surface this in EF status.
			updateErr = netErr
		case activeNetwork == nil:
			// No active network means no EF state to program. This includes a
			// primary UDN temporarily unavailable while its NAD is added or
			// removed, and a namespace filtered by Dynamic UDN allocation. Clean
			// up stale state without retrying or setting an error status; a primary
			// NAD becoming active requeues the EF through the NAD reconciler.
			skipStatusUpdate = true
		default:
			aclLoggingLevels, logErr := oc.getNamespaceACLLogging(namespace)
			if logErr != nil {
				return fmt.Errorf("failed to get acl logging levels for egress firewall %s/%s: %w",
					namespace, efName, logErr)
			}
			ownerController := activeNetwork.GetNetworkName() + "-network-controller"
			newEntry = &cacheEntry{
				pgName:            libovsdbutil.GetPortGroupName(getNamespacePortGroupDbIDs(namespace, ownerController)),
				subnets:           subnetsForNetInfo(activeNetwork),
				efResourceVersion: ef.ResourceVersion,
				logHash:           aclLogHash(aclLoggingLevels),
			}
		}
	}

	// If nothing relevant changed since last apply, skip update.
	if existingEntry != nil && existingEntry.hasNodeSelector {
		// Node selector rules depend on node state; always recalculate.
	} else if entriesEqual(existingEntry, newEntry) {
		return updateErr
	}

	// Delete existing state if there is no desired state.
	if newEntry == nil {
		pgName := existingEntry.pgName
		if pgName != "" {
			klog.Infof("Removing egress firewall %s", key)

			p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDsNoRule(namespace), nil)
			invalidACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
			if err != nil {
				return fmt.Errorf("error finding ACLs for egress firewall %s: %w", key, err)
			}
			if err := libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{pgName}, invalidACLs...); err != nil {
				return fmt.Errorf("error deleting stale ACLs for egress firewall %s: %w", key, err)
			}
		}
		oc.cache.Delete(namespace)
		if err := oc.dnsNameResolver.Delete(namespace); err != nil {
			return err
		}
		numRules, ok := oc.ruleCounter.Load(key)
		if ok {
			metrics.UpdateEgressFirewallRuleCount(float64(-numRules.(uint32)))
			oc.ruleCounter.Delete(key)
		} else {
			klog.V(4).Infof("Unable to decrement egress firewall rule count, cache miss for key: %s", key)
		}
		metrics.DecrementEgressFirewallCount()
		return updateErr
	}

	// Apply desired state.
	if err := oc.addEgressFirewall(ef, newEntry); err != nil {
		updateErr = err
		return updateErr
	}
	oc.cache.Store(namespace, newEntry)
	// remove stale ACLs
	p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDsNoRule(namespace),
		func(acl *nbdb.ACL) bool {
			ruleIdxStr := acl.ExternalIDs[libovsdbops.RuleIndex.String()]
			if len(ruleIdxStr) == 0 {
				return true // invalid ACL without a rule index, remove it
			}
			ruleIdx, err := strconv.Atoi(ruleIdxStr)
			if err != nil {
				return true // invalid ACL with bad index, remove it
			}
			if ruleIdx > len(ef.Spec.Egress) {
				return true // all ACLs have been updated with indices respective to current EF. Higher indices are stale.
			}
			return false
		})
	invalidACLs, findErr := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
	if findErr != nil {
		updateErr = fmt.Errorf("error finding ACLs for egress firewall %s: %w", ef.Name, findErr)
		return
	}
	if removalErr := libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{newEntry.pgName}, invalidACLs...); removalErr != nil {
		updateErr = fmt.Errorf("error deleting stale ACLs for egress firewall %s: %w", ef.Name, removalErr)
	}

	// If the port-group changed, remove the (now updated) ACLs from the previous port-group so
	// we don't keep stale references around.
	if existingEntry != nil && existingEntry.pgName != "" && existingEntry.pgName != newEntry.pgName {
		p := libovsdbops.GetPredicate[*nbdb.ACL](oc.GetEgressFirewallACLDbIDsNoRule(namespace), nil)
		acls, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, p)
		if err != nil {
			updateErr = utilerrors.Join(updateErr, fmt.Errorf("error finding ACLs for egress firewall %s/%s: %w", namespace, efName, err))
		} else if err := libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{existingEntry.pgName}, acls...); err != nil {
			updateErr = utilerrors.Join(updateErr, fmt.Errorf("error deleting stale ACL refs for egress firewall %s/%s: %w", namespace, efName, err))
		}
	}

	// Clean up any DNS address sets that are no longer referenced by ACLs.
	if err := oc.dnsNameResolver.DeleteStaleAddrSets(oc.nbClient); err != nil {
		updateErr = utilerrors.Join(updateErr, fmt.Errorf("error deleting stale DNS address sets for egress firewall %s/%s: %w",
			namespace, efName, err))
	}

	return
}

func subnetsForNetInfo(netInfo util.NetInfo) []*net.IPNet {
	if netInfo == nil {
		return nil
	}
	subnets := netInfo.Subnets()
	unsortedSubnets := make([]*net.IPNet, 0, len(subnets))
	for _, subnet := range subnets {
		if subnet.CIDR == nil {
			continue
		}
		unsortedSubnets = append(unsortedSubnets, subnet.CIDR)
	}
	return util.CopyIPNets(unsortedSubnets)
}

func entriesEqual(a, b *cacheEntry) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.pgName == b.pgName &&
			util.IsIPNetsEqual(a.subnets, b.subnets) &&
			a.efResourceVersion == b.efResourceVersion &&
			a.logHash == b.logHash
	}
}

func aclLogHash(levels *libovsdbutil.ACLLoggingLevels) string {
	if levels == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s", levels.Allow, levels.Deny)
}

func (oc *EFController) buildEgressFirewallConstruct(egressFirewall *egressfirewallapi.EgressFirewall, entry *cacheEntry) (*egressFirewall, error) {
	ef := cloneEgressFirewall(egressFirewall)
	var errorList []error
	for i, egressFirewallRule := range egressFirewall.Spec.Egress {
		// process Rules into egressFirewallRules for egressFirewall struct
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			errorList = append(errorList, fmt.Errorf("egressFirewall for namespace %s has too many rules, max allowed number is %v",
				egressFirewall.Namespace, types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority))
			break
		}
		efr, err := oc.newEgressFirewallRule(egressFirewall.Namespace, egressFirewallRule, i, entry)
		if err != nil {
			errorList = append(errorList, fmt.Errorf("cannot create EgressFirewall Rule to destination %s for namespace %s: %w",
				egressFirewallRule.To.CIDRSelector, egressFirewall.Namespace, err))
			continue

		}
		ef.egressRules = append(ef.egressRules, efr)
	}
	if len(errorList) > 0 {
		return nil, utilerrors.Join(errorList...)
	}
	return ef, nil
}

func (oc *EFController) addEgressFirewall(egressFirewall *egressfirewallapi.EgressFirewall, c *cacheEntry) error {
	klog.Infof("Adding egressFirewall %s in namespace %s", egressFirewall.Name, egressFirewall.Namespace)

	ef, err := oc.buildEgressFirewallConstruct(egressFirewall, c)
	if err != nil {
		return err
	}

	aclLoggingLevels, err := oc.getNamespaceACLLogging(ef.namespace)
	if err != nil {
		return fmt.Errorf("failed to get acl logging levels for egress firewall %s/%s: %w",
			egressFirewall.Namespace, egressFirewall.Name, err)
	}

	if err := oc.addEgressFirewallRules(ef, c.pgName, aclLoggingLevels); err != nil {
		return err
	}
	return nil
}

// validateAndGetEgressFirewallDestination validates an egress firewall rule destination and returns
// the parsed contents of the destination.
func (oc *EFController) validateAndGetEgressFirewallDestination(namespace string, egressFirewallDestination egressfirewallapi.EgressFirewallDestination, entry *cacheEntry) (
	cidrSelector string,
	dnsName string,
	clusterSubnetIntersection []*net.IPNet,
	nodeSelector *metav1.LabelSelector,
	err error) {
	// Validate the egress firewall rule.
	if egressFirewallDestination.DNSName != "" {
		n, err := util.ValidateAndGetEgressFirewallDNSDestination(egressFirewallDestination)
		if err != nil {
			return "", "", nil, nil, err
		}
		dnsName = n
	} else if len(egressFirewallDestination.CIDRSelector) > 0 {
		// Validate CIDR selector.
		_, ipNet, err := net.ParseCIDR(egressFirewallDestination.CIDRSelector)
		if err != nil {
			return "", "", nil, nil, err
		}
		cidrSelector = egressFirewallDestination.CIDRSelector
		if entry == nil || entry.subnets == nil {
			return "", "", nil, nil, fmt.Errorf("failed to "+
				"validate egress firewall destination: missing cached subnets for namespace %s", namespace)
		}
		for _, clusterSubnet := range entry.subnets {
			if clusterSubnet.Contains(ipNet.IP) || ipNet.Contains(clusterSubnet.IP) {
				clusterSubnetIntersection = append(clusterSubnetIntersection, clusterSubnet)
			}
		}
	} else {
		// Validate node selector.
		_, err := metav1.LabelSelectorAsSelector(egressFirewallDestination.NodeSelector)
		if err != nil {
			return "", "", nil, nil, fmt.Errorf("rule destination has invalid node selector, err: %v", err)
		}
		nodeSelector = egressFirewallDestination.NodeSelector
	}

	return
}

// newEgressFirewallRule creates a new egressFirewallRule. For the logging level, it will pick either of
// aclLoggingAllow or aclLoggingDeny depending if this is an allow or deny rule.
func (oc *EFController) newEgressFirewallRule(namespace string, rawEgressFirewallRule egressfirewallapi.EgressFirewallRule,
	id int, entry *cacheEntry) (*egressFirewallRule, error) {
	efr := &egressFirewallRule{
		id:     id,
		access: rawEgressFirewallRule.Type,
	}

	// Validate the egress firewall rule destination and update the appropriate
	// fields of efr.
	var err error
	efr.to.cidrSelector, efr.to.dnsName, efr.to.clusterSubnetIntersection, efr.to.nodeSelector, err =
		oc.validateAndGetEgressFirewallDestination(namespace, rawEgressFirewallRule.To, entry)
	if err != nil {
		return efr, err
	}
	// If nodeSelector is set then fetch the node addresses.
	if efr.to.nodeSelector != nil {
		if entry != nil {
			entry.hasNodeSelector = true
		}
		efr.to.nodeAddrs = map[string][]string{}
		selector, err := metav1.LabelSelectorAsSelector(rawEgressFirewallRule.To.NodeSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid node selector: %w", err)
		}
		nodes, err := oc.nodeLister.List(selector)
		if err != nil {
			return efr, fmt.Errorf("unable to query nodes for egress firewall: %w", err)
		}
		for _, node := range nodes {
			hostAddresses, err := util.GetNodeHostAddrs(node)
			if err != nil {
				return efr, fmt.Errorf("unable to get node host CIDRs for egress firewall, node: %s: %w", node.Name, err)
			}
			efr.to.nodeAddrs[node.Name] = hostAddresses
		}
	}
	efr.ports = rawEgressFirewallRule.Ports

	return efr, nil
}

func efNodeNeedsUpdate(oldNode, newNode *corev1.Node) bool {
	if oldNode == nil || newNode == nil {
		return true
	}
	return !reflect.DeepEqual(oldNode.Labels, newNode.Labels) ||
		util.NodeHostCIDRsAnnotationChanged(oldNode, newNode)
}

func (oc *EFController) updateEgressFirewallForNode(nodeName string) error {
	klog.V(3).Infof("Syncing node %q for egress firewall", nodeName)

	efNamespaces := oc.cache.GetKeys()
	for _, namespace := range efNamespaces {
		if err := oc.cache.DoWithLock(namespace, func(key string) error {
			if entry, ok := oc.cache.Load(key); ok && entry.hasNodeSelector {
				ef, err := oc.efLister.EgressFirewalls(namespace).Get(egressFirewallName)
				if err != nil {
					if !apierrors.IsNotFound(err) {
						return err
					}
					return nil
				}
				efKey, err := cache.MetaNamespaceKeyFunc(ef)
				if err != nil {
					return fmt.Errorf("couldn't get key for egress firewall object %+v: %v", ef, err)
				}
				klog.Infof("Syncing egress firewall %s due to node %q change", efKey, nodeName)
				oc.controller.Reconcile(efKey)
			}
			return nil
		}); err != nil {
			return err
		}

	}

	return nil
}

func (oc *EFController) addEgressFirewallRules(ef *egressFirewall, pgName string,
	aclLogging *libovsdbutil.ACLLoggingLevels, ruleIDs ...int) error {
	var ops []ovsdb.Operation
	var err error
	var hasDNS bool
	for _, rule := range ef.egressRules {
		// check if only specific rule ids are requested to be added
		if len(ruleIDs) > 0 {
			found := false
			for _, providedID := range ruleIDs {
				if rule.id == providedID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		var action string
		var matchTargets []matchTarget
		if rule.access == egressfirewallapi.EgressFirewallRuleAllow {
			action = nbdb.ACLActionAllow
		} else {
			action = nbdb.ACLActionDrop
		}
		if len(rule.to.nodeAddrs) > 0 {
			// sort node ips to ensure the same order when no changes are present
			// this ensure ACL recalculation won't happen just because of the order changes
			allIPs := []string{}
			for _, nodeIPs := range rule.to.nodeAddrs {
				allIPs = append(allIPs, nodeIPs...)
			}
			slices.Sort(allIPs)

			for _, addr := range allIPs {
				if utilnet.IsIPv6String(addr) {
					matchTargets = append(matchTargets, matchTarget{matchKindV6CIDR, addr, nil})
				} else {
					matchTargets = append(matchTargets, matchTarget{matchKindV4CIDR, addr, nil})
				}
			}
		} else if rule.to.cidrSelector != "" {
			if utilnet.IsIPv6CIDRString(rule.to.cidrSelector) {
				matchTargets = []matchTarget{{matchKindV6CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			} else {
				matchTargets = []matchTarget{{matchKindV4CIDR, rule.to.cidrSelector, rule.to.clusterSubnetIntersection}}
			}
		} else if len(rule.to.dnsName) > 0 {
			hasDNS = true
			// rule based on DNS NAME
			dnsName := rule.to.dnsName
			// If DNSNameResolver is enabled, then use the egressFirewallExternalDNS to get the address
			// set corresponding to the DNS name, otherwise use the egressFirewallDNS
			// to get the address set.
			if config.OVNKubernetesFeature.EnableDNSNameResolver {
				// Convert the DNS name to lower case fully qualified domain name.
				dnsName = util.LowerCaseFQDN(rule.to.dnsName)
			}
			dnsNameAddressSets, err := oc.dnsNameResolver.Add(ef.namespace, dnsName)
			if err != nil {
				return fmt.Errorf("error with DNSNameResolver - %v", err)
			}
			dnsNameIPv4ASHashName, dnsNameIPv6ASHashName := dnsNameAddressSets.GetASHashNames()
			if dnsNameIPv4ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV4AddressSet, dnsNameIPv4ASHashName, rule.to.clusterSubnetIntersection})
			}
			if dnsNameIPv6ASHashName != "" {
				matchTargets = append(matchTargets, matchTarget{matchKindV6AddressSet, dnsNameIPv6ASHashName, rule.to.clusterSubnetIntersection})
			}
		}

		if len(matchTargets) == 0 {
			klog.Warningf("Egress Firewall rule: %#v has no destination...ignoring", *rule)
			// ensure the ACL is removed from OVN
			if err := oc.deleteEgressFirewallRule(ef.namespace, pgName, rule.id); err != nil {
				return err
			}
			continue
		}

		match := generateMatch(pgName, matchTargets, rule.ports)
		aclIDs := oc.GetEgressFirewallACLDbIDs(ef.namespace, rule.id)
		priority := types.EgressFirewallStartPriority - rule.id
		egressFirewallACL := libovsdbutil.BuildACLWithDefaultTier(
			aclIDs,
			priority,
			match,
			action,
			aclLogging,
			// since egressFirewall has direction to-lport, set type to ingress
			libovsdbutil.LportIngress,
		)

		ops, err = oc.createEgressFirewallACLOps(ops, egressFirewallACL, pgName)
		if err != nil {
			return err
		}
	}

	if !hasDNS {
		if err := oc.dnsNameResolver.Delete(ef.namespace); err != nil {
			return err
		}
	}

	_, err = libovsdbops.TransactAndCheck(oc.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed configure egress firewall %s/%s: %w",
			ef.namespace, ef.name, err)
	}

	return nil
}

// createEgressFirewallACLOps uses the previously generated elements and creates the
// acls for all node switches
func (oc *EFController) createEgressFirewallACLOps(ops []ovsdb.Operation, egressFirewallACL *nbdb.ACL, pgName string) ([]ovsdb.Operation, error) {
	var err error
	ops, err = libovsdbops.CreateOrUpdateACLsOps(oc.nbClient, ops, oc.GetSamplingConfig(), egressFirewallACL)
	if err != nil {
		return nil, fmt.Errorf("failed to create egressFirewall ACL %#v: %v", egressFirewallACL, err)
	}

	ops, err = libovsdbops.AddACLsToPortGroupOps(oc.nbClient, ops, pgName, egressFirewallACL)
	if err != nil {
		return nil, fmt.Errorf("failed to add egressFirewall ACL %#v to port group %s: %v",
			egressFirewallACL, pgName, err)
	}

	return ops, nil
}

func (oc *EFController) GetEgressFirewallACLDbIDsNoRule(namespace string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
		})
}

func (oc *EFController) GetEgressFirewallACLDbIDs(namespace string, ruleIdx int) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.ACLEgressFirewall, types.DefaultNetworkControllerName,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: namespace,
			libovsdbops.RuleIndex:     strconv.Itoa(ruleIdx),
		})
}

func (oc *EFController) deleteEgressFirewallRule(namespace, pgName string, ruleIdx int) error {
	// Find ACLs for a given egressFirewall
	aclIDs := oc.GetEgressFirewallACLDbIDs(namespace, ruleIdx)
	pACL := libovsdbops.GetPredicate[*nbdb.ACL](aclIDs, nil)
	egressFirewallACLs, err := libovsdbops.FindACLsWithPredicate(oc.nbClient, pACL)
	if err != nil {
		return fmt.Errorf("unable to list egress firewall ACLs, cannot cleanup old stale data, err: %v", err)
	}

	if len(egressFirewallACLs) == 0 {
		return nil
	}

	if len(egressFirewallACLs) > 1 {
		klog.Errorf("Duplicate ACL found for egress firewall %s, ruleIdx: %d", namespace, ruleIdx)
	}

	err = libovsdbops.DeleteACLsFromPortGroups(oc.nbClient, []string{pgName}, egressFirewallACLs...)
	return err
}

func (oc *EFController) setEgressFirewallStatus(egressFirewall *egressfirewallapi.EgressFirewall, handlerErr error) error {
	var newMsg string
	if handlerErr != nil {
		newMsg = types.EgressFirewallErrorMsg + ": " + handlerErr.Error()
	} else {
		newMsg = EgressFirewallAppliedCorrectly
		key, err := cache.MetaNamespaceKeyFunc(egressFirewall)
		if err != nil {
			return err
		}
		newCount := uint32(len(egressFirewall.Spec.Egress))
		if prev, ok := oc.ruleCounter.Load(key); ok {
			// update: adjust rule count by delta (can be positive, negative, or zero)
			prevCount := prev.(uint32)
			delta := int(newCount) - int(prevCount)
			if delta != 0 {
				metrics.UpdateEgressFirewallRuleCount(float64(delta))
			}
		} else {
			// create: increment EF count and add all rules
			metrics.IncrementEgressFirewallCount()
			metrics.UpdateEgressFirewallRuleCount(float64(newCount))
		}
		oc.ruleCounter.Store(key, newCount)
	}

	newMsg = types.GetZoneStatus(oc.zone, newMsg)
	needsUpdate := true
	for _, message := range egressFirewall.Status.Messages {
		if message == newMsg {
			// found previous status
			needsUpdate = false
			break
		}
	}
	if !needsUpdate {
		return nil
	}

	applyOptions := metav1.ApplyOptions{
		Force:        true,
		FieldManager: oc.zone,
	}

	applyObj := egressfirewallapply.EgressFirewall(egressFirewall.Name, egressFirewall.Namespace).
		WithStatus(egressfirewallapply.EgressFirewallStatus().
			WithMessages(newMsg))
	_, err := oc.kube.EgressFirewallClient.K8sV1().EgressFirewalls(egressFirewall.Namespace).ApplyStatus(context.TODO(), applyObj, applyOptions)

	return err
}

func getNamespacePortGroupDbIDs(ns string, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.PortGroupNamespace, controller,
		map[libovsdbops.ExternalIDKey]string{
			libovsdbops.ObjectNameKey: ns,
		})
}

func (oc *EFController) GetSamplingConfig() *libovsdbops.SamplingConfig {
	if oc.observManager != nil {
		return oc.observManager.SamplingConfig()
	}
	return nil
}

// getNamespaceACLLogging retrieves ACLLoggingLevels for the Namespace
func (oc *EFController) getNamespaceACLLogging(namespace string) (*libovsdbutil.ACLLoggingLevels, error) {
	ns, err := oc.namespaceLister.Get(namespace)
	if err != nil {
		return nil, err
	}
	if annotation, ok := ns.Annotations[util.AclLoggingAnnotation]; ok {
		return parseACLLogging(annotation), nil
	}

	return &libovsdbutil.ACLLoggingLevels{
		Allow: "",
		Deny:  "",
	}, nil
}

// parseACLLogging parses the provided annotation values and sets aclLogging.Deny and
// aclLogging.Allow. If errors are encountered parsing the annotation, disable logging completely. If either
// value contains invalid input, disable logging for the respective key. This is needed to ensure idempotency.
// More details:
// *) If the provided annotation cannot be unmarshaled: Disable both Deny and Allow logging. Return an error.
// *) Valid values for "allow" and "deny" are  "alert", "warning", "notice", "info", "debug", "".
// *) Invalid values will return an error, and logging will be disabled for the respective key.
// *) In the following special cases, aclLogging.Deny and aclLogging.Allow. will both be reset to ""
//
//	without logging an error, meaning that logging will be switched off:
//	i) oc.aclLoggingEnabled == false
//	ii) annotation == ""
//	iii) annotation == "{}"
//
// *) If one of "allow" or "deny" can be parsed and has a valid value, but the other key is not present in the
//
//	annotation, then assume that this key should be disabled by setting its nsInfo value to "".
func parseACLLogging(annotation string) *libovsdbutil.ACLLoggingLevels {
	var aclLevels libovsdbutil.ACLLoggingLevels

	// If the annotation is "" or "{}", use empty strings. Otherwise, parse the annotation.
	if annotation != "" && annotation != "{}" {
		err := json.Unmarshal([]byte(annotation), &aclLevels)
		if err != nil {
			// Disable Allow and Deny logging to ensure idempotency.
			aclLevels.Allow = ""
			aclLevels.Deny = ""
			klog.Warningf("Could not unmarshal namespace ACL annotation '%s', disabling logging, err: %q",
				annotation, err)
			return &aclLevels
		}
	}

	// Valid log levels are the various pre-established levels or the empty string.
	validLogLevels := sets.NewString(nbdb.ACLSeverityAlert, nbdb.ACLSeverityWarning, nbdb.ACLSeverityNotice,
		nbdb.ACLSeverityInfo, nbdb.ACLSeverityDebug, "")

	// Set Deny logging.
	if !validLogLevels.Has(aclLevels.Deny) {
		aclLevels.Deny = ""
		klog.Warningf("Disabling deny logging due to invalid deny annotation. "+
			"%q is not a valid log severity", aclLevels.Deny)
	}

	// Set Allow logging.
	if !validLogLevels.Has(aclLevels.Allow) {
		aclLevels.Allow = ""
		klog.Warningf("Disabling allow logging due to an invalid allow annotation. "+
			"%q is not a valid log severity", aclLevels.Allow)
	}

	return &aclLevels
}

// cloneEgressFirewall shallow copies the egressfirewallapi.EgressFirewall object provided.
// This concretely means that it create a new egressfirewallapi.EgressFirewall with the name and
// namespace set, but without any rules specified.
func cloneEgressFirewall(originalEgressfirewall *egressfirewallapi.EgressFirewall) *egressFirewall {
	ef := &egressFirewall{
		name:        originalEgressfirewall.Name,
		namespace:   originalEgressfirewall.Namespace,
		egressRules: make([]*egressFirewallRule, 0),
	}
	return ef
}

// generateMatch generates the "match" section of ACL generation for egressFirewallRules.
// It is referentially transparent as all the elements have been validated before this function is called
// sample output:
// match=\"(ip4.dst == 1.2.3.4/32) && ip4.src == $testv4 && ip4.dst != 10.128.0.0/14\
func generateMatch(pgName string, destinations []matchTarget, dstPorts []egressfirewallapi.EgressFirewallPort) string {
	var dst string
	src := "inport == @" + pgName

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
	match := fmt.Sprintf("(%s) && %s", dst, src)
	if len(dstPorts) > 0 {
		match = fmt.Sprintf("%s && %s", match, egressGetL4Match(dstPorts))
	}
	return match
}

// egressGetL4Match generates the rules for when ports are specified in an egressFirewall Rule
// since the ports can be specified in any order in an egressFirewallRule the best way to build up
// a single rule is to build up each protocol as you walk through the list and place the appropriate logic
// between the elements.
func egressGetL4Match(ports []egressfirewallapi.EgressFirewallPort) string {
	var udpString string
	var tcpString string
	var sctpString string
	for _, port := range ports {
		if corev1.Protocol(port.Protocol) == corev1.ProtocolUDP && udpString != "udp" {
			if port.Port == 0 {
				udpString = "udp"
			} else {
				udpString = fmt.Sprintf("%s udp.dst == %d ||", udpString, port.Port)
			}
		} else if corev1.Protocol(port.Protocol) == corev1.ProtocolTCP && tcpString != "tcp" {
			if port.Port == 0 {
				tcpString = "tcp"
			} else {
				tcpString = fmt.Sprintf("%s tcp.dst == %d ||", tcpString, port.Port)
			}
		} else if corev1.Protocol(port.Protocol) == corev1.ProtocolSCTP && sctpString != "sctp" {
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

func getV4ClusterSubnetsExclusion(subnets []*net.IPNet) string {
	var exclusions []string
	for _, clusterSubnet := range subnets {
		if utilnet.IsIPv4CIDR(clusterSubnet) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip4", clusterSubnet))
		}
	}
	return strings.Join(exclusions, "&&")
}

func getV6ClusterSubnetsExclusion(subnets []*net.IPNet) string {
	var exclusions []string
	for _, clusterSubnet := range subnets {
		if utilnet.IsIPv6CIDR(clusterSubnet) {
			exclusions = append(exclusions, fmt.Sprintf("%s.dst != %s", "ip6", clusterSubnet))
		}
	}
	return strings.Join(exclusions, "&&")
}

func GetEgressFirewallNamespacedName(egressFirewall *egressfirewallapi.EgressFirewall) string {
	return fmt.Sprintf("%v/%v", egressFirewall.Namespace, egressFirewall.Name)
}

func (m *matchTarget) toExpr() (string, error) {
	var match string
	switch m.kind {
	case matchKindV4CIDR:
		match = fmt.Sprintf("ip4.dst == %s", m.value)
		if m.clusterSubnetIntersection != nil {
			match = fmt.Sprintf("%s && %s", match, getV4ClusterSubnetsExclusion(m.clusterSubnetIntersection))
		}
	case matchKindV6CIDR:
		match = fmt.Sprintf("ip6.dst == %s", m.value)
		if m.clusterSubnetIntersection != nil {
			match = fmt.Sprintf("%s && %s", match, getV6ClusterSubnetsExclusion(m.clusterSubnetIntersection))
		}
	case matchKindV4AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip4.dst == $%s", m.value)
		}
	case matchKindV6AddressSet:
		if m.value != "" {
			match = fmt.Sprintf("ip6.dst == $%s", m.value)
		}
	default:
		return "", fmt.Errorf("invalid MatchKind")
	}
	return match, nil
}
