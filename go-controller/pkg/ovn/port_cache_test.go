// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPortCacheSnapshotsAreIndependent(t *testing.T) {
	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })

	cache := NewPortCache(stopChan)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "namespace", Name: "pod"}}
	const (
		nadKey      = "namespace/nad"
		networkName = "network"
	)
	mac := net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0x01, 0x03}
	ipNet := &net.IPNet{
		IP:   net.ParseIP("10.128.1.3"),
		Mask: net.CIDRMask(24, 32),
	}
	expectedMAC := append(net.HardwareAddr(nil), mac...)
	expectedIP := append(net.IP(nil), ipNet.IP...)
	expectedMask := append(net.IPMask(nil), ipNet.Mask...)
	ips := []*net.IPNet{ipNet, nil}
	addedInfo := cache.add(pod, "switch", nadKey, networkName, "port-uuid", mac, ips)

	// add must take ownership of neither its inputs nor its return value. The
	// cache keeps an independent snapshot of the applied state.
	mac[0] = 0xaa
	ipNet.IP[len(ipNet.IP)-1] = 0xaa
	ipNet.Mask[0] = 0
	ips[0] = &net.IPNet{IP: net.ParseIP("192.0.2.1")}
	ips[1] = &net.IPNet{IP: net.ParseIP("192.0.2.2")}
	addedInfo.name = "mutated-add-result"
	addedInfo.mac[0] = 0xbb
	addedInfo.ips[0].IP[len(addedInfo.ips[0].IP)-1] = 0xbb
	addedInfo.ips[0].Mask[0] = 0
	addedInfo.ips[1] = &net.IPNet{IP: net.ParseIP("192.0.2.3")}

	allSnapshot, err := cache.getAll(pod)
	if err != nil {
		t.Fatalf("failed to get all cached port state: %v", err)
	}
	allInfo := allSnapshot[nadKey]
	if allInfo == nil {
		t.Fatalf("missing port state for NAD %q", nadKey)
	}
	if allInfo.name == "mutated-add-result" {
		t.Fatal("mutating add result changed the cache")
	}
	if allInfo.mac[0] != expectedMAC[0] {
		t.Fatalf("mutating add input or result MAC changed the cache: got %s, want %s", allInfo.mac, expectedMAC)
	}
	if !allInfo.ips[0].IP.Equal(expectedIP) {
		t.Fatalf("mutating add input or result IP changed the cache: got %s, want %s", allInfo.ips[0].IP, expectedIP)
	}
	if allInfo.ips[0].Mask.String() != expectedMask.String() {
		t.Fatalf("mutating add input or result mask changed the cache: got %s, want %s", allInfo.ips[0].Mask, expectedMask)
	}
	if allInfo.ips[1] != nil {
		t.Fatalf("mutating add input or result nil IPNet changed the cache: %#v", allInfo.ips[1])
	}

	// Scheduling asynchronous cache expiration mutates the cached lpInfo. A
	// previously captured applied-state snapshot must remain stable for retries.
	cache.remove(pod, nadKey)
	if !allInfo.expires.IsZero() {
		t.Fatalf("cache expiration mutated the applied-state snapshot: %v", allInfo.expires)
	}

	// Mutating a getAll caller's snapshot, including nested byte slices, must
	// not mutate the cache.
	allInfo.name = "mutated-name"
	allInfo.mac[0] = 0xff
	allInfo.ips[0].IP[len(allInfo.ips[0].IP)-1] = 0xff
	allInfo.ips[0].Mask[0] = 0
	allInfo.ips[1] = &net.IPNet{IP: net.ParseIP("192.0.2.1")}
	delete(allSnapshot, nadKey)

	cachedInfo, err := cache.get(pod, nadKey)
	if err != nil {
		t.Fatalf("failed to get cached port state: %v", err)
	}
	if cachedInfo.name == "mutated-name" {
		t.Fatal("mutating snapshot fields changed the cache")
	}
	if cachedInfo.mac[0] != expectedMAC[0] {
		t.Fatalf("mutating snapshot MAC changed the cache: got %s, want %s", cachedInfo.mac, expectedMAC)
	}
	if !cachedInfo.ips[0].IP.Equal(expectedIP) {
		t.Fatalf("mutating snapshot IP changed the cache: got %s, want %s", cachedInfo.ips[0].IP, expectedIP)
	}
	if cachedInfo.ips[0].Mask.String() != expectedMask.String() {
		t.Fatalf("mutating snapshot mask changed the cache: got %s, want %s", cachedInfo.ips[0].Mask, expectedMask)
	}
	if cachedInfo.ips[1] != nil {
		t.Fatalf("mutating a nil snapshot IPNet changed the cache: %#v", cachedInfo.ips[1])
	}

	// get is also used as applied state for default-network pod retries. Its
	// nested values must be independent as well.
	cachedInfo.mac[0] = 0xee
	cachedInfo.ips[0].IP[len(cachedInfo.ips[0].IP)-1] = 0xee
	freshInfo, err := cache.get(pod, nadKey)
	if err != nil {
		t.Fatalf("failed to get fresh cached port state: %v", err)
	}
	if freshInfo.mac[0] != expectedMAC[0] || !freshInfo.ips[0].IP.Equal(expectedIP) {
		t.Fatal("mutating get result changed the cache")
	}
}
