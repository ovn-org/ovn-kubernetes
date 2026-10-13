// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package evpn

import (
	"net"
	"testing"
)

func TestCountProgrammedEntries(t *testing.T) {
	entries := func(programmed bool, ips ...string) *neighEntries {
		e := &neighEntries{programmed: programmed}
		for _, ip := range ips {
			e.ips = append(e.ips, net.ParseIP(ip))
		}
		return e
	}

	tests := []struct {
		name          string
		podNeighbors  map[string]*neighEntries
		wantPods      int
		wantNeighbors int
	}{
		{
			name:         "no entries",
			podNeighbors: map[string]*neighEntries{},
		},
		{
			name: "single programmed pod with one IP",
			podNeighbors: map[string]*neighEntries{
				"ns/p1": entries(true, "10.210.0.4"),
			},
			wantPods:      1,
			wantNeighbors: 1,
		},
		{
			name: "dual stack pod counts one pod and two neighbours",
			podNeighbors: map[string]*neighEntries{
				"ns/p1": entries(true, "10.210.0.4", "fd01::4"),
			},
			wantPods:      1,
			wantNeighbors: 2,
		},
		{
			name: "entries cached during live migration are not counted",
			podNeighbors: map[string]*neighEntries{
				"ns/p1": entries(true, "10.210.0.4"),
				"ns/p2": entries(false, "10.210.0.5"),
			},
			wantPods:      1,
			wantNeighbors: 1,
		},
		{
			name: "nothing programmed yet",
			podNeighbors: map[string]*neighEntries{
				"ns/p1": entries(false, "10.210.0.4"),
				"ns/p2": entries(false, "10.210.0.5", "fd01::5"),
			},
		},
		{
			name: "pod with no IPs still counts as a pod once programmed",
			podNeighbors: map[string]*neighEntries{
				"ns/p1": entries(true),
			},
			wantPods: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods, neighbors := countProgrammedEntries(tt.podNeighbors)
			if pods != tt.wantPods || neighbors != tt.wantNeighbors {
				t.Errorf("countProgrammedEntries() = (pods=%d, neighbors=%d), want (pods=%d, neighbors=%d)",
					pods, neighbors, tt.wantPods, tt.wantNeighbors)
			}
		})
	}
}
