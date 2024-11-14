package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

type PortCache struct {
	sync.RWMutex
	stopChan <-chan struct{}

	// cache of logical port info (lpInfo). The first key is podName, in the form of
	// podNamespace/podName; the second key is NAD name associated with specific port info
	cache map[string]map[string]*lpInfo
}

type lpInfo struct {
	name          string
	uuid          string
	logicalSwitch string
	ips           []*net.IPNet
	mac           net.HardwareAddr
	// expires, if non-nil, indicates that this object is scheduled to be
	// removed at the given time
	expires time.Time
}

func NewPortCache(stopChan <-chan struct{}) *PortCache {
	return &PortCache{
		stopChan: stopChan,
		cache:    make(map[string]map[string]*lpInfo),
	}
}

func (c *PortCache) get(pod *kapi.Pod, nadName string) (*lpInfo, error) {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadName == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetSecondaryNetworkLogicalPortName(pod.Namespace, pod.Name, nadName)
	}
	c.RLock()
	defer c.RUnlock()
	if infoMap, ok := c.cache[podName]; ok {
		if info, ok := infoMap[nadName]; ok {
			x := *info
			return &x, nil
		}
	}
	return nil, fmt.Errorf("logical port %s for pod %s not found in cache", podName, logicalPort)
}

func (c *PortCache) getAll(pod *kapi.Pod) (map[string]*lpInfo, error) {
	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	c.RLock()
	defer c.RUnlock()
	if infoMap, ok := c.cache[podName]; ok {
		// make a copy of this lpInfo map and return
		lpInfoMap := map[string]*lpInfo{}
		for k, v := range infoMap {
			lpInfoMap[k] = v
		}
		return lpInfoMap, nil
	}
	return nil, fmt.Errorf("logical port cache for pod %s not found", podName)
}

func (c *PortCache) add(pod *kapi.Pod, logicalSwitch, nadName, uuid string, mac net.HardwareAddr, ips []*net.IPNet) *lpInfo {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadName == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetSecondaryNetworkLogicalPortName(pod.Namespace, pod.Name, nadName)
	}
	c.Lock()
	defer c.Unlock()
	portInfo := &lpInfo{
		logicalSwitch: logicalSwitch,
		name:          logicalPort,
		uuid:          uuid,
		ips:           ips,
		mac:           mac,
	}
	klog.V(5).Infof("port-cache(%s): added port %+v with IP: %s and MAC: %s",
		logicalPort, portInfo, portInfo.ips, portInfo.mac)
	m, ok := c.cache[podName]
	if ok {
		m[nadName] = portInfo
	} else {
		m = map[string]*lpInfo{nadName: portInfo}
		c.cache[podName] = m
	}
	return portInfo
}

func (c *PortCache) remove(pod *kapi.Pod, nadName string) {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadName == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetSecondaryNetworkLogicalPortName(pod.Namespace, pod.Name, nadName)
	}

	c.Lock()
	defer c.Unlock()
	infoMap, ok := c.cache[podName]
	if !ok {
		klog.V(5).Infof("port-cache(%s): port not found in cache or already marked for removal", logicalPort)
		return
	}
	info, ok := infoMap[nadName]
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
				if info, ok := infoMap[nadName]; ok && !info.expires.IsZero() {
					if time.Now().After(info.expires) {
						klog.V(5).Infof("port-cache(%s): removing port", logicalPort)
						delete(infoMap, nadName)
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
