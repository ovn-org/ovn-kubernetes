package util

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

	// cache of logical port info (LPInfo). The first key is podName, in the form of
	// podNamespace/podName; the second key is NAD name associated with specific port info
	cache map[string]map[string]*LPInfo
}

type LPInfo struct {
	Name          string
	UUID          string
	LogicalSwitch string
	IPs           []*net.IPNet
	MAC           net.HardwareAddr
	// Expires, if non-nil, indicates that this object is scheduled to be
	// removed at the given time
	Expires time.Time
}

func NewPortCache(stopChan <-chan struct{}) *PortCache {
	return &PortCache{
		stopChan: stopChan,
		cache:    make(map[string]map[string]*LPInfo),
	}
}

func (c *PortCache) Get(pod *kapi.Pod, nadName string) (*LPInfo, error) {
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

func (c *PortCache) GetAll(pod *kapi.Pod) (map[string]*LPInfo, error) {
	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	c.RLock()
	defer c.RUnlock()
	if infoMap, ok := c.cache[podName]; ok {
		// make a copy of this LPInfo map and return
		lpInfoMap := map[string]*LPInfo{}
		for k, v := range infoMap {
			lpInfoMap[k] = v
		}
		return lpInfoMap, nil
	}
	return nil, fmt.Errorf("logical port cache for pod %s not found", podName)
}

func (c *PortCache) Add(pod *kapi.Pod, logicalSwitch, nadName, uuid string, mac net.HardwareAddr, ips []*net.IPNet) *LPInfo {
	var logicalPort string

	podName := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if nadName == types.DefaultNetworkName {
		logicalPort = util.GetLogicalPortName(pod.Namespace, pod.Name)
	} else {
		logicalPort = util.GetSecondaryNetworkLogicalPortName(pod.Namespace, pod.Name, nadName)
	}
	c.Lock()
	defer c.Unlock()
	portInfo := &LPInfo{
		LogicalSwitch: logicalSwitch,
		Name:          logicalPort,
		UUID:          uuid,
		IPs:           ips,
		MAC:           mac,
	}
	klog.V(5).Infof("port-cache(%s): added port %+v with IP: %s and MAC: %s",
		logicalPort, portInfo, portInfo.IPs, portInfo.MAC)
	m, ok := c.cache[podName]
	if ok {
		m[nadName] = portInfo
	} else {
		m = map[string]*LPInfo{nadName: portInfo}
		c.cache[podName] = m
	}
	return portInfo
}

func (c *PortCache) Remove(pod *kapi.Pod, nadName string) {
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
	if !ok || !info.Expires.IsZero() {
		klog.V(5).Infof("port-cache(%s): port not found in cache or already marked for removal", logicalPort)
		return
	}
	info.Expires = time.Now().Add(time.Minute)
	klog.V(5).Infof("port-cache(%s): scheduling port for removal at %v", logicalPort, info.Expires)

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
			// that was deleted and re-added before the timer Expires.
			infoMap, ok := c.cache[podName]
			if ok {
				if info, ok := infoMap[nadName]; ok && !info.Expires.IsZero() {
					if time.Now().After(info.Expires) {
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
