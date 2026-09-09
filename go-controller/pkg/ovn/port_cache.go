// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

type PortCache struct {
	sync.RWMutex
	stopChan <-chan struct{}

	// cache of logical port info (lpInfo). The first key is podName, in the form of
	// podNamespace/podName; the second key is the NAD key associated with specific port info
	cache map[string]map[string]*lpInfo
}

type lpInfo struct {
	name   string
	uuid   string
	podUID ktypes.UID
	// Network controller that wrote this applied cache entry.
	appliedNetworkName string
	logicalSwitch      string
	ips                []*net.IPNet
	mac                net.HardwareAddr
	// needsReconcile keeps a partially programmed port eligible for setup
	// retries without losing the addresses and UUID needed for teardown.
	needsReconcile bool
	// expires, if non-nil, indicates that this object is scheduled to be
	// removed at the given time
	expires time.Time
}

func cloneLPInfo(info *lpInfo) *lpInfo {
	if info == nil {
		return nil
	}

	cloned := *info
	if info.mac != nil {
		cloned.mac = append(net.HardwareAddr(nil), info.mac...)
	}
	if info.ips != nil {
		cloned.ips = make([]*net.IPNet, len(info.ips))
		for i, ipNet := range info.ips {
			if ipNet == nil {
				continue
			}
			clonedIPNet := *ipNet
			if ipNet.IP != nil {
				clonedIPNet.IP = append(net.IP(nil), ipNet.IP...)
			}
			if ipNet.Mask != nil {
				clonedIPNet.Mask = append(net.IPMask(nil), ipNet.Mask...)
			}
			cloned.ips[i] = &clonedIPNet
		}
	}
	return &cloned
}

func NewPortCache(stopChan <-chan struct{}) *PortCache {
	return &PortCache{
		stopChan: stopChan,
		cache:    make(map[string]map[string]*lpInfo),
	}
}

func (c *PortCache) get(pod *corev1.Pod, nadKey string) (*lpInfo, error) {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadKey == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
	}
	c.RLock()
	defer c.RUnlock()
	if infoMap, ok := c.cache[podName]; ok {
		if info, ok := infoMap[nadKey]; ok {
			return cloneLPInfo(info), nil
		}
	}
	return nil, fmt.Errorf("logical port %s (NAD key %s) for pod %s not found in cache",
		logicalPort, nadKey, podName)
}

func (c *PortCache) getAll(pod *corev1.Pod) (map[string]*lpInfo, error) {
	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	c.RLock()
	defer c.RUnlock()
	if infoMap, ok := c.cache[podName]; ok {
		// Return an independent applied-state snapshot. In particular, cache
		// expiration must not mutate state retained by delete retries.
		lpInfoMap := make(map[string]*lpInfo, len(infoMap))
		for k, v := range infoMap {
			lpInfoMap[k] = cloneLPInfo(v)
		}
		return lpInfoMap, nil
	}
	return nil, fmt.Errorf("logical port cache for pod %s not found", podName)
}

// Add inserts a pod's logical port into the cache.
func (c *PortCache) Add(pod *corev1.Pod, logicalSwitch, nadKey, appliedNetworkName, uuid string, mac net.HardwareAddr, ips []*net.IPNet) {
	_ = c.add(pod, logicalSwitch, nadKey, appliedNetworkName, uuid, mac, ips)
}

// add stores applied port state for a pod NAD key.
func (c *PortCache) add(pod *corev1.Pod, logicalSwitch, nadKey, appliedNetworkName, uuid string, mac net.HardwareAddr, ips []*net.IPNet) *lpInfo {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadKey == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
	}
	c.Lock()
	defer c.Unlock()
	portInfo := cloneLPInfo(&lpInfo{
		podUID:             pod.UID,
		logicalSwitch:      logicalSwitch,
		name:               logicalPort,
		uuid:               uuid,
		appliedNetworkName: appliedNetworkName,
		ips:                ips,
		mac:                mac,
	})
	klog.V(5).Infof("port-cache(%s): added port %+v with IP: %s and MAC: %s",
		logicalPort, portInfo, portInfo.ips, portInfo.mac)
	m, ok := c.cache[podName]
	if ok {
		m[nadKey] = portInfo
	} else {
		m = map[string]*lpInfo{nadKey: portInfo}
		c.cache[podName] = m
	}
	return cloneLPInfo(portInfo)
}

// markPodForReconcile retains partial port state while forcing setup to run
// again. A subsequent successful add replaces the entry and clears the flag.
func (c *PortCache) markPodForReconcile(pod *corev1.Pod, networkName string) {
	c.Lock()
	defer c.Unlock()
	for _, info := range c.cache[pod.Namespace+"/"+pod.Name] {
		if info.appliedNetworkName == networkName {
			info.needsReconcile = true
		}
	}
}

// invalidatePodForNetwork forgets this controller's applied port observations
// for one pod. The shared pod reconciler retains its independent applied-state
// snapshot, so delete retries remain safe while the next desired-state pass is
// forced to verify/create the actual logical ports again.
func (c *PortCache) invalidatePodForNetwork(pod *corev1.Pod, networkName string) {
	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	c.Lock()
	defer c.Unlock()

	infoMap, ok := c.cache[podName]
	if !ok {
		return
	}
	for nadKey, info := range infoMap {
		if info == nil || info.appliedNetworkName != networkName {
			continue
		}
		klog.V(5).Infof("port-cache(%s): invalidating port after dependency change for network %s", info.name, networkName)
		delete(infoMap, nadKey)
	}
	if len(infoMap) == 0 {
		delete(c.cache, podName)
	}
}

// removeAllForNetwork clears applied port state for a cleaned-up network.
func (c *PortCache) removeAllForNetwork(networkName string) {
	c.Lock()
	defer c.Unlock()
	for podName, infoMap := range c.cache {
		for nadKey, info := range infoMap {
			if info.appliedNetworkName != networkName {
				continue
			}
			klog.V(5).Infof("port-cache(%s): removing port for cleaned up network %s", info.name, networkName)
			delete(infoMap, nadKey)
		}
		if len(infoMap) == 0 {
			delete(c.cache, podName)
		}
	}
}

func (c *PortCache) remove(pod *corev1.Pod, nadKey string) {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadKey == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
	}

	c.Lock()
	defer c.Unlock()
	infoMap, ok := c.cache[podName]
	if !ok {
		klog.V(5).Infof("port-cache(%s): port not found in cache or already marked for removal", logicalPort)
		return
	}
	info, ok := infoMap[nadKey]
	if !ok || !info.expires.IsZero() {
		klog.V(5).Infof("port-cache(%s): port not found in cache or already marked for removal", logicalPort)
		return
	}
	info.expires = time.Now().Add(time.Minute)
	klog.V(5).Infof("port-cache(%s): scheduling port for removal at %v", logicalPort, info.expires)

	// Removal must be deferred, since some handlers
	// may run after the main pod handler and look for items in the cache
	// on delete events.
	go func() {
		select {
		case <-time.After(time.Minute):
			c.Lock()
			defer c.Unlock()
			// remove the port info if its expiration time has elapsed.
			// This makes sure that we don't prematurely remove a port
			// that was deleted and re-added before the timer expires.
			infoMap, ok := c.cache[podName]
			if ok {
				if info, ok := infoMap[nadKey]; ok && !info.expires.IsZero() {
					if time.Now().After(info.expires) {
						klog.V(5).Infof("port-cache(%s): removing port", logicalPort)
						delete(infoMap, nadKey)
						if len(infoMap) == 0 {
							delete(c.cache, podName)
						}
					}
				}
			}
		case <-c.stopChan:
			break
		}
	}()
}
