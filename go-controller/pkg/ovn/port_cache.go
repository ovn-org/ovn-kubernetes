package ovn

import (
	"fmt"
	"net"
	"sync"
	"time"
)

type portCache struct {
	sync.Mutex

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
	c.Lock()
	defer c.Unlock()
	if info, ok := c.cache[logicalPort]; ok {
		return info, nil
	}
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

	// Removal must be deferred since, for example, NetworkPolicy pod handlers
	// may run after the main pod handler and look for items in the cache
	// on delete events.
	go func() {
		select {
		case <-time.After(10 * time.Second):
			c.Lock()
			defer c.Lock()
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
