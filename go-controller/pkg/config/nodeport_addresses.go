// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"net"
	"strings"

	utilnet "k8s.io/utils/net"
)

const nodePortAddressesPrimary = "primary"

// NodePortAddressConfig restricts which local node IP addresses may receive
// NodePort traffic. When unset, all local node IPs are allowed.
type NodePortAddressConfig struct {
	cidrs      []*net.IPNet
	usePrimary bool
}

// ParseNodePortAddresses parses the nodeport-addresses configuration value.
// An empty string means NodePort traffic is accepted on all local node IPs.
func ParseNodePortAddresses(raw string) (*NodePortAddressConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &NodePortAddressConfig{}, nil
	}

	cfg := &NodePortAddressConfig{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == nodePortAddressesPrimary {
			cfg.usePrimary = true
			continue
		}
		_, cidr, err := utilnet.ParseCIDRSloppy(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid nodeport-addresses entry %q: %w", entry, err)
		}
		cfg.cidrs = append(cfg.cidrs, cidr)
	}

	if !cfg.usePrimary && len(cfg.cidrs) == 0 {
		return nil, fmt.Errorf("invalid nodeport-addresses value %q", raw)
	}
	return cfg, nil
}

// Restricted reports whether NodePort traffic should be limited to specific
// local node IP addresses.
func (c *NodePortAddressConfig) Restricted() bool {
	if c == nil {
		return false
	}
	return c.usePrimary || len(c.cidrs) > 0
}

// FilterHostAddresses returns the subset of hostAddresses that may receive
// NodePort traffic. primaryAddresses are used when "primary" is configured.
func (c *NodePortAddressConfig) FilterHostAddresses(hostAddresses, primaryAddresses []net.IP) []net.IP {
	if c == nil || !c.Restricted() {
		return append([]net.IP(nil), hostAddresses...)
	}

	allowed := make(map[string]net.IP)
	if c.usePrimary {
		for _, ip := range primaryAddresses {
			if ip != nil {
				allowed[ip.String()] = ip
			}
		}
	}
	for _, ip := range hostAddresses {
		if ip == nil {
			continue
		}
		if _, ok := allowed[ip.String()]; ok {
			continue
		}
		for _, cidr := range c.cidrs {
			if cidr.Contains(ip) {
				allowed[ip.String()] = ip
				break
			}
		}
	}

	filtered := make([]net.IP, 0, len(allowed))
	for _, ip := range hostAddresses {
		if _, ok := allowed[ip.String()]; ok {
			filtered = append(filtered, ip)
		}
	}
	return filtered
}
