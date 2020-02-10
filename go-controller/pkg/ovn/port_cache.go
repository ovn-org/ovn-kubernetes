package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type portCache struct {
	sync.RWMutex

	stopChan <-chan struct{}
	cache    map[string]*lpInfo
}

type lpInfo struct {
	name          string
	uuid          string
	logicalSwitch string
	ip            net.IP
	mac           net.HardwareAddr
	// dead indicates that this object should not be requeued for deletion
	dead bool
}

func newPortCache(stopChan <-chan struct{}) *portCache {
	return &portCache{
		stopChan: stopChan,
		cache:    make(map[string]*lpInfo),
	}
}

func (c *portCache) get(logicalPort string) (*lpInfo, error) {
	c.RLock()
	defer c.RUnlock()
	if info, ok := c.cache[logicalPort]; ok {
		logrus.Warningf("##### got port %+v from cache", info)
		return info, nil
	}
	logrus.Warningf("##### port %s not found in cache", logicalPort)
	return nil, fmt.Errorf("logical port %s not found in cache", logicalPort)
}

func (c *portCache) add(logicalSwitch, logicalPort, uuid string, mac net.HardwareAddr, ip net.IP) *lpInfo {
	c.Lock()
	defer c.Unlock()
	portInfo := &lpInfo{
		logicalSwitch: logicalSwitch,
		name:          logicalPort,
		uuid:          uuid,
		ip:            ip,
		mac:           mac,
	}
	c.cache[logicalPort] = portInfo
	logrus.Warningf("##### add port %+v to cache", portInfo)
	return portInfo
}

func (c *portCache) remove(logicalPort string) {
	c.Lock()
	defer c.Unlock()
	info, ok := c.cache[logicalPort]
	if !ok || info.dead {
		return
	}
	info.dead = true
	logrus.Warningf("##### scheduling port %+v removal from cache", info)

	// Removal must be deferred since, for example, NetworkPolicy pod handlers
	// may run after the main pod handler and look for items in the cache
	// on delete events.
	go func() {
		select {
		case <-time.After(time.Minute):
			c.Lock()
			defer c.Unlock()
			// recheck whether the port info is dead since it could
			// have been resurrected if the pod was re-added in the
			// last 10 seconds
			if info, ok := c.cache[logicalPort]; ok && info.dead {
				delete(c.cache, logicalPort)
			}
		case <-c.stopChan:
			break
		}
	}()
}
