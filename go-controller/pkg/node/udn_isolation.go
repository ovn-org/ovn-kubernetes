package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/knftables"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	nodenft "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// udn-isolation chain contains rules for udn isolation from the host side.
	UDNIsolationChain = "udn-isolation"
	// nftables set names
	nftablesUDNOpenPortsv4     = "udn-open-ports-v4"
	nftablesUDNOpenPortsv6     = "udn-open-ports-v6"
	nftablesUDNOpenPortsICMPv4 = "udn-open-ports-icmp-v4"
	nftablesUDNOpenPortsICMPv6 = "udn-open-ports-icmp-v6"
	nftablesUDNPodIPsv4        = "udn-pod-default-ips-v4"
	nftablesUDNPodIPsv6        = "udn-pod-default-ips-v6"
)

// UDNHostIsolationManager manages the host isolation for user defined networks.
// It uses nftables chain "udn-isolation" to only allow connection to primary UDN pods from kubelet.
// It also listens to systemd events to re-apply the rules after kubelet restart as cgroup matching is used.
type UDNHostIsolationManager struct {
	nft               knftables.Interface
	ipv4, ipv6        bool
	podController     controller.Controller
	podLister         corelisters.PodLister
	kubeletCgroupPath string

	udnPodIPsv4 *nftPodElementsSet
	udnPodIPsv6 *nftPodElementsSet

	udnOpenPortsv4 *nftPodElementsSet
	udnOpenPortsv6 *nftPodElementsSet

	udnOpenPortsICMPv4 *nftPodElementsSet
	udnOpenPortsICMPv6 *nftPodElementsSet
}

func NewUDNHostIsolationManager(ipv4, ipv6 bool, podInformer coreinformers.PodInformer) *UDNHostIsolationManager {
	m := &UDNHostIsolationManager{
		podLister:          podInformer.Lister(),
		ipv4:               ipv4,
		ipv6:               ipv6,
		udnPodIPsv4:        newNFTPodElementsSet(nftablesUDNPodIPsv4, false),
		udnPodIPsv6:        newNFTPodElementsSet(nftablesUDNPodIPsv6, false),
		udnOpenPortsv4:     newNFTPodElementsSet(nftablesUDNOpenPortsv4, true),
		udnOpenPortsv6:     newNFTPodElementsSet(nftablesUDNOpenPortsv6, true),
		udnOpenPortsICMPv4: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv4, false),
		udnOpenPortsICMPv6: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv6, false),
	}
	controllerConfig := &controller.ControllerConfig[v1.Pod]{
		RateLimiter:    workqueue.NewTypedItemFastSlowRateLimiter[string](time.Second, 5*time.Second, 5),
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
		ObjNeedsUpdate: podNeedsUpdate,
		Reconcile:      m.reconcilePod,
		Threadiness:    1,
	}
	m.podController = controller.NewController[v1.Pod]("udn-host-isolation-manager", controllerConfig)
	return m
}

// Start must be called on node setup.
func (m *UDNHostIsolationManager) Start(ctx context.Context) error {
	klog.Infof("Starting UDN host isolation manager")
	// find kubelet cgroup path.
	// kind cluster uses "kubelet.slice/kubelet.service", while OCP cluster uses "system.slice/kubelet.service".
	// as long as ovn-k node is running as a privileged container, we can access the host cgroup directory.
	err := filepath.WalkDir("/sys/fs/cgroup", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == "kubelet.service" {
			m.kubeletCgroupPath = strings.TrimPrefix(path, "/sys/fs/cgroup/")
			klog.Infof("Found kubelet cgroup path: %s", m.kubeletCgroupPath)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil || m.kubeletCgroupPath == "" {
		return fmt.Errorf("failed to find kubelet cgroup path: %w", err)
	}
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}

	m.nft = nft
	if err = m.setupUDNIsolationFromHost(); err != nil {
		return fmt.Errorf("failed to setup UDN host isolation: %w", err)
	}
	if err = m.runKubeletRestartTracker(ctx); err != nil {
		return fmt.Errorf("failed to run kubelet restart tracker: %w", err)
	}
	return controller.StartWithInitialSync(m.podInitialSync, m.podController)
}

func (m *UDNHostIsolationManager) Stop() {
	controller.Stop(m.podController)
}

// CleanupUDNHostIsolation removes all nftables chains and sets created by UDNHostIsolationManager.
func CleanupUDNHostIsolation() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}
	tx := nft.NewTransaction()
	safeDelete(tx, &knftables.Chain{
		Name: UDNIsolationChain,
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNPodIPsv4,
		Type: "ipv4_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNPodIPsv6,
		Type: "ipv6_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsv4,
		Type: "ipv4_addr . inet_proto . inet_service",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsv6,
		Type: "ipv6_addr . inet_proto . inet_service",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsICMPv4,
		Type: "ipv4_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsICMPv6,
		Type: "ipv6_addr",
	})
	return nft.Run(context.TODO(), tx)
}

func (m *UDNHostIsolationManager) setupUDNIsolationFromHost() error {
	tx := m.nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     UDNIsolationChain,
		Comment:  knftables.PtrTo("Host isolation for user defined networks"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: UDNIsolationChain,
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsv4,
		Comment: knftables.PtrTo("default network open ports of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsv6,
		Comment: knftables.PtrTo("default network open ports of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsICMPv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks that allow ICMP (IPv4)"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsICMPv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks that allow ICMP (IPv6)"),
		Type:    "ipv6_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr",
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not setup nftables rules for UDN from host isolation: %v", err)
	}
	return nil
}

func (m *UDNHostIsolationManager) addRules(tx *knftables.Transaction) {
	if m.ipv4 {
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", ".", "meta l4proto", ".", "th dport",
				"@", nftablesUDNOpenPortsv4, "accept",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", "@", nftablesUDNOpenPortsICMPv4, "meta l4proto", "icmp",
				"accept",
			),
		})

		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"socket", "cgroupv2", "level 2", m.kubeletCgroupPath,
				"ip", "daddr", "@", nftablesUDNPodIPsv4, "accept"),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", "@", nftablesUDNPodIPsv4, "drop"),
		})
	}
	if m.ipv6 {
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", ".", "meta l4proto", ".", "th dport",
				"@", nftablesUDNOpenPortsv6, "accept",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", "@", nftablesUDNOpenPortsICMPv6, "meta l4proto", "icmpv6",
				"accept",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"socket", "cgroupv2", "level 2", m.kubeletCgroupPath,
				"ip6", "daddr", "@", nftablesUDNPodIPsv6, "accept"),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", "@", nftablesUDNPodIPsv6, "drop"),
		})
	}
}

func (m *UDNHostIsolationManager) updateKubeletCgroup() error {
	tx := m.nft.NewTransaction()
	tx.Flush(&knftables.Chain{
		Name: UDNIsolationChain,
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for management port: %v", err)
	}
	return nil
}

// runKubeletRestartTracker listens to systemd events to re-apply the UDN host isolation rules after kubelet restart.
// cgroupv2 match doesn't actually match cgroup paths, but rather resolves them to numeric cgroup IDs when such
// rules are loaded into kernel, and does not automatically update them in any way afterwards.
// From the patch https://patchwork.ozlabs.org/project/netfilter-devel/patch/1479114761-19534-1-git-send-email-pablo@netfilter.org/#1511797:
// If the cgroup is gone, the filtering policy would not match anymore. You only have to subscribe to events
// and perform an incremental updates to tear down the side of the filtering policy that you don't need anymore.
// If a new cgroup is created, you load the filtering policy for the new cgroup and then add
// processes to that cgroup. You only have to follow the right sequence to avoid problems.
func (m *UDNHostIsolationManager) runKubeletRestartTracker(ctx context.Context) (err error) {
	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %w", err)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	err = conn.Subscribe()
	if err != nil {
		return fmt.Errorf("failed to subscribe to systemd events: %w", err)
	}
	// interval is important here as we need to catch the restart state, before it is running again
	events, errChan := conn.SubscribeUnitsCustom(50*time.Millisecond, 0, func(u1, u2 *dbus.UnitStatus) bool { return *u1 != *u2 },
		func(s string) bool {
			return s != "kubelet.service"
		})
	// run until context is cancelled
	go func() {
		waitingForActive := false
		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case event := <-events:
				for _, status := range event {
					if status.ActiveState != "active" {
						waitingForActive = true
					} else if waitingForActive {
						klog.Infof("Kubelet was restarted, re-applying UDN host isolation")
						err = m.updateKubeletCgroup()
						if err != nil {
							klog.Errorf("Failed to re-apply UDN host isolation: %v", err)
						} else {
							waitingForActive = false
						}
					}
				}
			case err := <-errChan:
				klog.Errorf("Systemd listener error: %v", err)
			}
		}
	}()
	return nil
}

func (m *UDNHostIsolationManager) podInitialSync() error {
	udnPodIPsv4 := map[string]sets.Set[string]{}
	udnPodIPsv6 := map[string]sets.Set[string]{}
	udnOpenPortsICMPv4 := map[string]sets.Set[string]{}
	udnOpenPortsICMPv6 := map[string]sets.Set[string]{}
	udnOpenPortsv4 := map[string]sets.Set[string]{}
	udnOpenPortsv6 := map[string]sets.Set[string]{}

	pods, err := m.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		podKey, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Warningf("UDNHostIsolationManager failed to get key for pod %s in namespace %s: %v", pod.Name, pod.Namespace, err)
			continue
		}
		// ignore openPorts parse error in initial sync
		pi, _, err := m.getPodInfo(podKey, pod)
		if err != nil {
			// don't fail because of one pod error on initial sync as it may cause crashloop.
			// expect pod event to come later with correct/updated annotations.
			klog.Warningf("UDNHostIsolationManager failed to get pod info for pod %s/%s on initial sync: %v", pod.Name, pod.Namespace, err)
			continue
		}
		if pi == nil {
			// this pod doesn't need to be updated
			continue
		}

		udnPodIPsv4[podKey] = pi.ipsv4
		udnPodIPsv6[podKey] = pi.ipsv6
		udnOpenPortsICMPv4[podKey] = pi.icmpv4
		udnOpenPortsICMPv6[podKey] = pi.icmpv6
		udnOpenPortsv4[podKey] = pi.openPortsv4
		udnOpenPortsv6[podKey] = pi.openPortsv6
	}
	if err = m.udnPodIPsv4.fullSync(m.nft, udnPodIPsv4); err != nil {
		return err
	}
	if err = m.udnPodIPsv6.fullSync(m.nft, udnPodIPsv6); err != nil {
		return err
	}
	if err = m.udnOpenPortsICMPv4.fullSync(m.nft, udnOpenPortsICMPv4); err != nil {
		return err
	}
	if err = m.udnOpenPortsICMPv6.fullSync(m.nft, udnOpenPortsICMPv6); err != nil {
		return err
	}
	if err = m.udnOpenPortsv4.fullSync(m.nft, udnOpenPortsv4); err != nil {
		return err
	}
	if err = m.udnOpenPortsv6.fullSync(m.nft, udnOpenPortsv6); err != nil {
		return err
	}
	return nil
}

func podNeedsUpdate(oldObj, newObj *v1.Pod) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	// react to pod IP changes
	return !reflect.DeepEqual(oldObj.Status, newObj.Status) ||
		oldObj.Annotations[util.OvnPodAnnotationName] != newObj.Annotations[util.OvnPodAnnotationName]
}

func (m *UDNHostIsolationManager) reconcilePod(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("UDNHostIsolationManager failed to split meta namespace cache key %s for pod: %v", key, err)
		return nil
	}
	pod, err := m.podLister.Pods(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Pod was deleted, clean up.
			return m.updateWithPodInfo(key, &podInfo{})
		}
		return fmt.Errorf("failed to fetch pod %s in namespace %s", name, namespace)
	}
	pi, parseErr, err := m.getPodInfo(key, pod)
	if err != nil {
		return err
	}
	if pi == nil {
		// this pod doesn't need to be updated
		return nil
	}
	err = m.updateWithPodInfo(key, pi)
	return errors.Join(err, parseErr)
}

type podInfo struct {
	ipsv4       sets.Set[string]
	ipsv6       sets.Set[string]
	icmpv4      sets.Set[string]
	icmpv6      sets.Set[string]
	openPortsv4 sets.Set[string]
	openPortsv6 sets.Set[string]
}

// getPodInfo returns nftables set elements for a pod.
// nil is returned when pod should not be updated.
// empty podInfo will delete the pod from all sets and is returned when nil pod is passed.
// first error is for parsing openPorts annotation, second error is for fetching pod IPs.
// parsing error should not stop the update, as we need to cleanup potentially present rules from the previous config.
func (m *UDNHostIsolationManager) getPodInfo(podKey string, pod *v1.Pod) (*podInfo, error, error) {
	pi := &podInfo{}
	if pod == nil {
		return pi, nil, nil
	}
	if util.PodWantsHostNetwork(pod) {
		// host network pods can't be isolated by IP
		return nil, nil, nil
	}
	// only add pods with primary UDN
	primaryUDN, err := m.isPodPrimaryUDN(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to check if pod %s is in primary UDN: %w", podKey, err)
	}
	if !primaryUDN {
		return nil, nil, nil
	}
	podIPs, err := util.DefaultNetworkPodIPs(pod)
	if err != nil {
		// update event should come later with ips
		klog.V(5).Infof("Failed to get default network pod IPs for pod %s: %v", podKey, err)
		return nil, nil, nil
	}
	openPorts, parseErr := util.UnmarshalUDNOpenPortsAnnotation(pod.Annotations)
	pi.ipsv4, pi.ipsv6 = splitIPsPerFamily(podIPs)
	pi.icmpv4, pi.icmpv6, pi.openPortsv4, pi.openPortsv6 = m.getOpenPortSets(pi.ipsv4, pi.ipsv6, openPorts)
	return pi, parseErr, nil
}

// updateWithPodInfo updates the nftables sets with given podInfo for a given pod.
// empty podInfo will delete the pod from all sets.
func (m *UDNHostIsolationManager) updateWithPodInfo(podKey string, pi *podInfo) error {
	tx := m.nft.NewTransaction()
	m.udnPodIPsv4.updatePodElementsTX(podKey, pi.ipsv4, tx)
	m.udnPodIPsv6.updatePodElementsTX(podKey, pi.ipsv6, tx)
	m.udnOpenPortsICMPv4.updatePodElementsTX(podKey, pi.icmpv4, tx)
	m.udnOpenPortsICMPv6.updatePodElementsTX(podKey, pi.icmpv6, tx)
	m.udnOpenPortsv4.updatePodElementsTX(podKey, pi.openPortsv4, tx)
	m.udnOpenPortsv6.updatePodElementsTX(podKey, pi.openPortsv6, tx)

	if tx.NumOperations() == 0 {
		return nil
	}

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables set for UDN pods: %v", err)
	}

	// update internal state only after successful transaction
	m.udnPodIPsv4.updatePodElementsAfterTX(podKey, pi.ipsv4)
	m.udnPodIPsv6.updatePodElementsAfterTX(podKey, pi.ipsv6)
	m.udnOpenPortsICMPv4.updatePodElementsAfterTX(podKey, pi.icmpv4)
	m.udnOpenPortsICMPv6.updatePodElementsAfterTX(podKey, pi.icmpv6)
	m.udnOpenPortsv4.updatePodElementsAfterTX(podKey, pi.openPortsv4)
	m.udnOpenPortsv6.updatePodElementsAfterTX(podKey, pi.openPortsv6)
	return nil
}

func (m *UDNHostIsolationManager) isPodPrimaryUDN(pod *v1.Pod) (bool, error) {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		// pod IPs were not assigned yet, should be retried later
		return false, err
	}
	// NetworkRoleInfrastructure means default network is not primary, then UDN must be the primary network
	return podAnnotation.Role == types.NetworkRoleInfrastructure, nil
}

func (m *UDNHostIsolationManager) getOpenPortSets(newV4IPs, newV6IPs sets.Set[string], openPorts []*util.OpenPort) (icmpv4, icmpv6, openPortsv4, openPortsv6 sets.Set[string]) {
	icmpv4 = sets.New[string]()
	icmpv6 = sets.New[string]()
	openPortsv4 = sets.New[string]()
	openPortsv6 = sets.New[string]()

	for _, openPort := range openPorts {
		if openPort.Protocol == "icmp" {
			icmpv4 = newV4IPs
			icmpv6 = newV6IPs
		} else {
			for podIPv4 := range newV4IPs {
				openPortsv4.Insert(joinNFTSlice([]string{podIPv4, openPort.Protocol, fmt.Sprintf("%d", *openPort.Port)}))
			}
			for podIPv6 := range newV6IPs {
				openPortsv6.Insert(joinNFTSlice([]string{podIPv6, openPort.Protocol, fmt.Sprintf("%d", *openPort.Port)}))
			}
		}
	}
	return
}

// nftPodElementsSet is a helper struct to manage an nftables set with pod-owned elements.
// Can be used to store pod IPs, or more complex elements.
type nftPodElementsSet struct {
	setName string
	// podName: set elements
	podElements map[string]sets.Set[string]
	// podIPs may be reused as soon as the pod reaches Terminating state, and delete event may come later.
	// That means a new pod with the same IP may be added before the previous pod is deleted.
	// To avoid deleting newly-added pod IP thinking we are deleting old pod IP, we keep track of re-used set elements.
	elementToPods map[string]sets.Set[string]
	// if a set element is composed of multiple strings
	// set to false to avoid unneeded parsing
	composedValue bool
}

func newNFTPodElementsSet(setName string, composedValue bool) *nftPodElementsSet {
	return &nftPodElementsSet{
		setName:       setName,
		composedValue: composedValue,
		podElements:   make(map[string]sets.Set[string]),
		elementToPods: make(map[string]sets.Set[string]),
	}
}

func (n *nftPodElementsSet) getKey(key string) []string {
	if n.composedValue {
		return splitNFTSlice(key)
	}
	return []string{key}
}

// updatePodElementsTX adds transaction operations to update pod elements in nftables set.
// To update internal struct, updatePodElementsAfterTX must be called if transaction is successful.
func (n *nftPodElementsSet) updatePodElementsTX(namespacedName string, podElements sets.Set[string], tx *knftables.Transaction) {
	if n.podElements[namespacedName].Equal(podElements) {
		return
	}
	// always delete all old elements, then add new elements.
	for existingElem := range n.podElements[namespacedName] {
		if n.elementToPods[existingElem].Len() == 1 {
			// only delete element is it referenced by one pod
			tx.Delete(&knftables.Element{
				Set: n.setName,
				Key: n.getKey(existingElem),
			})
		}
	}
	for newElem := range podElements {
		// adding existing element is a no-op
		tx.Add(&knftables.Element{
			Set: n.setName,
			Key: n.getKey(newElem),
		})
	}
}

func (n *nftPodElementsSet) updatePodElementsAfterTX(namespacedName string, elements sets.Set[string]) {
	for existingElem := range n.podElements[namespacedName] {
		if !elements.Has(existingElem) {
			// element was removed
			n.elementToPods[existingElem].Delete(namespacedName)
			if n.elementToPods[existingElem].Len() == 0 {
				delete(n.elementToPods, existingElem)
			}
		}
	}

	for elem := range elements {
		if n.elementToPods[elem] == nil {
			n.elementToPods[elem] = sets.New[string]()
		}
		n.elementToPods[elem].Insert(namespacedName)
	}
	if len(elements) == 0 {
		delete(n.podElements, namespacedName)
	} else {
		n.podElements[namespacedName] = elements
	}
}

// fullSync should be called on restart to sync all pods elements.
// It flushes existing elements, and adds new elements.
func (n *nftPodElementsSet) fullSync(nft knftables.Interface, podsElements map[string]sets.Set[string]) error {
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: n.setName,
	})
	for podName, podElements := range podsElements {
		if len(podElements) == 0 {
			continue
		}
		for elem := range podElements {
			tx.Add(&knftables.Element{
				Set: n.setName,
				Key: n.getKey(elem),
			})
			if n.elementToPods[elem] == nil {
				n.elementToPods[elem] = sets.New[string]()
			}
			n.elementToPods[elem].Insert(podName)
		}
		n.podElements[podName] = podElements
	}
	err := nft.Run(context.TODO(), tx)
	if err != nil {
		clear(n.podElements)
		return fmt.Errorf("initial pods sync for UDN host isolation failed: %w", err)
	}
	return nil
}

func splitIPsPerFamily(podIPs []net.IP) (sets.Set[string], sets.Set[string]) {
	newV4IPs := sets.New[string]()
	newV6IPs := sets.New[string]()
	for _, podIP := range podIPs {
		if podIP.To4() != nil {
			newV4IPs.Insert(podIP.String())
		} else {
			newV6IPs.Insert(podIP.String())
		}
	}
	return newV4IPs, newV6IPs
}

func safeDelete(tx *knftables.Transaction, obj knftables.Object) {
	tx.Add(obj)
	tx.Delete(obj)
}

// joinNFTSlice converts nft element key or value (type []string) to string to store in the nftElementStorage.
// The separator is the same as the one used by nft commands, so we know that the parsing is going to be unambiguous.
func joinNFTSlice(k []string) string {
	return strings.Join(k, " . ")
}

// splitNFTSlice converts nftElementStorage key or value string representation back to slice.
func splitNFTSlice(k string) []string {
	return strings.Split(k, " . ")
}
