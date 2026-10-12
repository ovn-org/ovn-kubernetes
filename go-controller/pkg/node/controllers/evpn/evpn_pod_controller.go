// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// neighEntries represents static neighbor and FDB entries for a pod on EVPN devices.
type neighEntries struct {
	uid         k8stypes.UID
	sviName     string
	ovsPortName string
	macvrfVID   int
	ips         []net.IP
	mac         net.HardwareAddr
}

// podNeedsUpdate returns true for local pods when the annotation changed, the pod completed, or was deleted.
// For non-local pods, it returns true when the kubevirt migration target ready timestamp changes,
// so that reconcilePod can remove the local source pod's entries.
// For local pods, it also returns true on migration status changes so that
// reconcilePod re-programs PERMANENT neighbor entries that may have been
// overwritten by zebra's extern_learn during EVPN MAC mobility.
func (c *Controller) podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	if kubevirt.LiveMigrationStatusChanged(oldObj, newObj) {
		return true
	}

	var oldAnnot, newAnnot string
	if oldObj != nil {
		if oldObj.Spec.NodeName != c.nodeName {
			return false
		}
		oldAnnot = oldObj.Annotations[types.OvnPodAnnotationName]
	}
	if newObj != nil {
		if newObj.Spec.NodeName != c.nodeName {
			return false
		}
		if util.PodCompleted(newObj) {
			return true
		}
		newAnnot = newObj.Annotations[types.OvnPodAnnotationName]
	}
	return oldAnnot != newAnnot
}

// shouldDeleteNeighbors returns true when a pod's neighbor/FDB entries should be removed.
// This happens when the pod has completed, or when this node is the migration source
// and the target domain is ready (so FRR withdraws the Type-2 routes from this node).
func (c *Controller) shouldDeleteNeighbors(migrationStatus *kubevirt.LiveMigrationStatus, pod *corev1.Pod) bool {
	if util.PodCompleted(pod) {
		return true
	}
	if migrationStatus == nil || !migrationStatus.IsTargetDomainReady() {
		return false
	}
	if migrationStatus.SourcePod == nil {
		return false
	}
	return migrationStatus.SourcePod.Spec.NodeName == c.nodeName
}

// shouldEnsureNeighbors returns true when a pod's PERMANENT neighbor entries should be
// programmed. For non-kubevirt pods (migrationStatus==nil) it always returns true.
// During live migration, it returns true only on the target node after the target
// domain is ready, ensuring neighbors are programmed after the source has withdrawn
// its routes and zebra's extern_learn entries are gone.
func (c *Controller) shouldEnsureNeighbors(migrationStatus *kubevirt.LiveMigrationStatus) bool {
	if migrationStatus == nil {
		return true
	}
	if !migrationStatus.IsTargetDomainReady() {
		return false
	}
	if migrationStatus.TargetPod == nil {
		return false
	}
	return migrationStatus.TargetPod.Spec.NodeName == c.nodeName
}

func (c *Controller) reconcilePod(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.deletePodNeighbors(key)
		}
		return err
	}

	migrationStatus, err := kubevirt.DiscoverLiveMigrationStatus(c.podLister, pod)
	if err != nil {
		return fmt.Errorf("failed to discover live migration status: %w", err)
	}

	if c.shouldDeleteNeighbors(migrationStatus, pod) {
		return c.deletePodNeighbors(key)
	}

	// If we already have cached entries for this exact pod (same UID), ensure the
	// neighbors are present without re-parsing the annotation.
	c.podNeighLock.Lock()
	existing, hasExisting := c.podNeighbors[key]
	c.podNeighLock.Unlock()
	if hasExisting && existing.uid == pod.UID && c.shouldEnsureNeighbors(migrationStatus) {
		return c.ensurePodNeighbors(existing)
	}
	// On rapid pod delete/recreate with the same name (e.g. force-deleted
	// StatefulSet pod), the workqueue may coalesce the delete and create events
	// into a single reconcile for the new pod. Clean up the old kernel
	// FDB/neighbor entries before programming new ones, otherwise stale entries
	// for the previous pod's MAC/IPs would persist until the next periodic sync.
	if hasExisting {
		if err := c.deletePodNeighbors(key); err != nil {
			return fmt.Errorf("failed to delete stale neighbors for recreated pod %s: %v", key, err)
		}
	}

	nadKey, err := c.networkMgr.GetPrimaryNADForNamespace(pod.Namespace)
	if err != nil {
		return err
	}
	if nadKey == types.DefaultNetworkName {
		return nil
	}

	netInfo := c.networkMgr.GetNetInfoForNADKey(nadKey)
	if netInfo == nil || netInfo.EVPNMACVRFVNI() == 0 {
		return nil
	}

	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// no primary UDN annotation yet, will get an event so no need to retry here
			return nil
		}
		return err
	}

	entries := &neighEntries{
		uid:         pod.UID,
		sviName:     GetEVPNL2SVIName(netInfo),
		ovsPortName: GetEVPNOVSPortName(netInfo),
		macvrfVID:   netInfo.EVPNMACVRFVID(),
		mac:         podAnnotation.MAC,
	}
	for _, ipNet := range podAnnotation.IPs {
		entries.ips = append(entries.ips, ipNet.IP)
	}

	if c.shouldEnsureNeighbors(migrationStatus) {
		if err := c.ensurePodNeighbors(entries); err != nil {
			return err
		}
	}

	c.podNeighLock.Lock()
	c.podNeighbors[key] = entries
	c.podNeighLock.Unlock()

	return nil
}

// ensurePodNeighbors programs static FDB and neighbor entries for a pod's MAC/IPs.
// The static FDB entry on the OVS port prevents the bridge from aging out the MAC,
// which would cause FRR to withdraw the Type-2 route.
func (c *Controller) ensurePodNeighbors(entries *neighEntries) error {
	svi, err := util.GetNetLinkOps().LinkByName(entries.sviName)
	if err != nil {
		return fmt.Errorf("failed to get L2 SVI %s: %w", entries.sviName, err)
	}
	ovsPort, err := util.GetNetLinkOps().LinkByName(entries.ovsPortName)
	if err != nil {
		return fmt.Errorf("failed to get OVS port %s: %w", entries.ovsPortName, err)
	}
	if err := util.LinkFDBSet(ovsPort, entries.mac, entries.macvrfVID); err != nil {
		return fmt.Errorf("failed to set FDB entry for %s on %s: %w", entries.mac, entries.ovsPortName, err)
	}
	klog.V(5).Infof("Configured FDB %s vlan %d on %s", entries.mac, entries.macvrfVID, entries.ovsPortName)
	for _, ip := range entries.ips {
		if err := util.LinkNeighSet(svi, ip, entries.mac); err != nil {
			return fmt.Errorf("failed to add neighbor %s on %s: %w", ip, entries.sviName, err)
		}
		klog.V(5).Infof("Configured neighbor %s lladdr %s on %s", ip, entries.mac, entries.sviName)
	}
	return nil
}

func (c *Controller) deletePodNeighbors(key string) error {
	c.podNeighLock.Lock()
	entries, ok := c.podNeighbors[key]
	c.podNeighLock.Unlock()
	if !ok {
		return nil
	}

	ovsPort, err := util.GetNetLinkOps().LinkByName(entries.ovsPortName)
	if err != nil {
		var linkNotFound netlink.LinkNotFoundError
		if !errors.As(err, &linkNotFound) {
			return fmt.Errorf("failed to get OVS port %s for pod %s: %w", entries.ovsPortName, key, err)
		}
		// OVS port is already gone (e.g. VTEP was deleted first).
		// FDB entries are implicitly removed with the interface.
		// Fall through to clean up any remaining neighbor entries on the SVI.
		klog.V(5).Infof("OVS port %s already removed, skipping FDB cleanup for pod %s", entries.ovsPortName, key)
	} else {
		if err := util.LinkFDBDel(ovsPort, entries.mac, entries.macvrfVID); err != nil {
			if !errors.Is(err, syscall.ENOENT) {
				return fmt.Errorf("failed to delete FDB entry %s from %s: %w", entries.mac, entries.ovsPortName, err)
			}
		}
		klog.V(5).Infof("Deleted FDB %s from %s for pod %s", entries.mac, entries.ovsPortName, key)
	}

	link, err := util.GetNetLinkOps().LinkByName(entries.sviName)
	if err != nil {
		var linkNotFound netlink.LinkNotFoundError
		if errors.As(err, &linkNotFound) {
			// SVI is already gone, neighbor entries are implicitly removed.
			klog.V(5).Infof("SVI %s already removed, cleaning up cache for pod %s", entries.sviName, key)
			c.podNeighLock.Lock()
			delete(c.podNeighbors, key)
			c.podNeighLock.Unlock()
			return nil
		}
		return fmt.Errorf("failed to get SVI %s for pod %s: %w", entries.sviName, key, err)
	}
	for _, ip := range entries.ips {
		if err := util.LinkNeighDel(link, ip); err != nil {
			if !errors.Is(err, syscall.ENOENT) {
				return fmt.Errorf("failed to delete neighbor %s from %s: %w", ip, entries.sviName, err)
			}
		}
		klog.V(5).Infof("Deleted neighbor %s lladdr %s from %s for pod %s", ip, entries.mac, entries.sviName, key)
	}

	c.podNeighLock.Lock()
	delete(c.podNeighbors, key)
	c.podNeighLock.Unlock()
	return nil
}

// cleanStalePodEntries removes stale neighbor and FDB entries.
func (c *Controller) cleanStalePodEntries() error {
	if !util.WaitForInformerCacheSyncWithTimeout("evpn-pod", c.stopChan, c.watchFactory.PodCoreInformer().Informer().HasSynced) {
		return fmt.Errorf("timed out waiting for pod informer cache to sync")
	}

	type evpnNetworkDevices struct {
		sviName     string
		ovsPortName string
		nadKeys     []string
	}
	var networks []evpnNetworkDevices
	err := c.networkMgr.DoWithLock(func(netInfo util.NetInfo) error {
		if netInfo == nil || netInfo.EVPNVTEPName() == "" {
			return nil
		}
		networks = append(networks, evpnNetworkDevices{
			sviName:     GetEVPNL2SVIName(netInfo),
			ovsPortName: GetEVPNOVSPortName(netInfo),
			nadKeys:     c.networkMgr.GetNADKeysForNetwork(netInfo.GetNetworkName()),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to collect EVPN networks: %w", err)
	}

	for _, net := range networks {
		desiredIPs := sets.New[string]()
		desiredMACs := sets.New[string]()

		for _, nadKey := range net.nadKeys {
			ns, _, err := cache.SplitMetaNamespaceKey(nadKey)
			if err != nil {
				klog.Errorf("Failed to parse nad key %s: %v", nadKey, err)
				continue
			}
			pods, err := c.podLister.Pods(ns).List(labels.Everything())
			if err != nil {
				klog.Errorf("Failed to list pods in namespace %s: %v", ns, err)
				continue
			}
			for _, pod := range pods {
				if pod.Spec.NodeName != c.nodeName || util.PodCompleted(pod) {
					continue
				}
				podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
				if err != nil {
					continue
				}
				desiredMACs.Insert(podAnnotation.MAC.String())
				for _, ipNet := range podAnnotation.IPs {
					desiredIPs.Insert(ipNet.IP.String())
				}
			}
		}

		c.cleanStaleNeighbors(net.sviName, desiredIPs)
		c.cleanStaleFDB(net.ovsPortName, desiredMACs)
	}
	return nil
}

func (c *Controller) cleanStaleNeighbors(sviName string, desiredIPs sets.Set[string]) {
	svi, err := util.GetNetLinkOps().LinkByName(sviName)
	if err != nil {
		return
	}
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		neighs, err := util.GetNetLinkOps().NeighList(svi.Attrs().Index, family)
		if err != nil {
			klog.Errorf("Failed to list neighbors on %s: %v", sviName, err)
			continue
		}
		for _, n := range neighs {
			if n.State&netlink.NUD_PERMANENT == 0 {
				continue
			}
			if !desiredIPs.Has(n.IP.String()) {
				if err := util.LinkNeighDel(svi, n.IP); err != nil {
					klog.Errorf("Failed to delete stale neighbor %s from %s: %v", n.IP, sviName, err)
				} else {
					klog.V(5).Infof("Deleted stale neighbor %s from %s", n.IP, sviName)
				}
			}
		}
	}
}

func (c *Controller) cleanStaleFDB(ovsPortName string, desiredMACs sets.Set[string]) {
	ovsPort, err := util.GetNetLinkOps().LinkByName(ovsPortName)
	if err != nil {
		return
	}
	fdbs, err := util.GetNetLinkOps().NeighList(ovsPort.Attrs().Index, syscall.AF_BRIDGE)
	if err != nil {
		klog.Errorf("Failed to list FDB on %s: %v", ovsPortName, err)
		return
	}
	for _, f := range fdbs {
		if f.State&netlink.NUD_NOARP == 0 || f.Flags&netlink.NTF_MASTER == 0 {
			continue
		}
		if !desiredMACs.Has(f.HardwareAddr.String()) {
			if err := util.LinkFDBDel(ovsPort, f.HardwareAddr, f.Vlan); err != nil {
				klog.Errorf("Failed to delete stale FDB %s from %s: %v", f.HardwareAddr, ovsPortName, err)
			} else {
				klog.V(5).Infof("Deleted stale FDB %s from %s", f.HardwareAddr, ovsPortName)
			}
		}
	}
}
