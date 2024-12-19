//go:build linux
// +build linux

package nftables

import (
	"context"
	"testing"

	"sigs.k8s.io/knftables"
)

func TestAddObjects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		objs    []knftables.Object
		final   string
	}{
		{
			name:    "empty transaction",
			initial: "",
			objs:    []knftables.Object{},
			final:   "add table inet ovn-kubernetes",
		},
		{
			name: "add to empty set",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
		},
		{
			name: "re-add existing object",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
		},
		{
			name: "add map elements, multiple containers",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Map:   "testmap",
					Key:   []string{"10.0.0.1"},
					Value: []string{"9.9.9.9"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = AddObjects(context.Background(), tc.objs)
			if err != nil {
				t.Fatalf("unexpected error adding objects: %v", err)
			}
			err = MatchNFTRules(tc.final, fake.Dump())
			if err != nil {
				t.Fatalf("unexpected final result: %v", err)
			}
		})
	}
}

func TestDeleteObjects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial string
		objs    []knftables.Object
		final   string
	}{
		{
			name:    "empty transaction",
			initial: "",
			objs:    []knftables.Object{},
			final:   "add table inet ovn-kubernetes",
		},
		{
			name: "delete existing objects",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
		},
		{
			name: "delete non-existing object",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
			`,
		},
		{
			name: "delete map elements, multiple containers",
			initial: `
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testset { 1.2.3.4 }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Set: "testset",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Map:   "testmap",
					Key:   []string{"10.0.0.1"},
					Value: []string{"9.9.9.9"},
				},
				&knftables.Element{
					Set: "testset",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
			`,
		},
		{
			name: "delete map elements without values",
			initial: `
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testmap { 10.0.0.1 : 9.9.9.9 }
				add element inet ovn-kubernetes testmap { 10.0.0.2 : 8.8.8.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.3 : 7.7.7.7 }
				add element inet ovn-kubernetes testmap { 10.0.0.4 : 6.6.6.6 }
			`,
			objs: []knftables.Object{
				&knftables.Element{
					Map: "testmap",
					Key: []string{"10.0.0.1"},
				},
				&knftables.Element{
					Map: "testmap",
					Key: []string{"10.0.0.3"},
				},
				&knftables.Element{
					Map: "testmap",
					Key: []string{"10.0.0.5"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add map inet ovn-kubernetes testmap { type ipv4_addr : ipv4_addr ; }
				add element inet ovn-kubernetes testmap { 10.0.0.2 : 8.8.8.8 }
				add element inet ovn-kubernetes testmap { 10.0.0.4 : 6.6.6.6 }
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = DeleteObjects(context.Background(), tc.objs)
			if err != nil {
				t.Fatalf("unexpected error deleting objects: %v", err)
			}
			err = MatchNFTRules(tc.final, fake.Dump())
			if err != nil {
				t.Fatalf("unexpected final result: %v", err)
			}
		})
	}
}

func TestSyncObjects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		initial    string
		containers []knftables.Object
		contents   []knftables.Object
		final      string
	}{
		{
			name:       "empty transaction",
			initial:    "",
			containers: []knftables.Object{},
			contents:   []knftables.Object{},
			final:      "add table inet ovn-kubernetes",
		},
		{
			name: "start empty / end empty",
			initial: `
				add set inet ovn-kubernetes starts-empty { type ipv4_addr ; }
				add set inet ovn-kubernetes becomes-empty { type ipv4_addr ; }
				add element inet ovn-kubernetes becomes-empty { 1.1.1.1 }
				add element inet ovn-kubernetes becomes-empty { 2.2.2.2 }
				add element inet ovn-kubernetes becomes-empty { 3.3.3.3 }
			`,
			containers: []knftables.Object{
				&knftables.Set{
					Name: "starts-empty",
				},
				&knftables.Set{
					Name: "becomes-empty",
				},
			},
			contents: []knftables.Object{
				&knftables.Element{
					Set: "starts-empty",
					Key: []string{"1.2.3.4"},
				},
				&knftables.Element{
					Set: "starts-empty",
					Key: []string{"5.6.7.8"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes becomes-empty { type ipv4_addr ; }
				add set inet ovn-kubernetes starts-empty { type ipv4_addr ; }
				add element inet ovn-kubernetes starts-empty { 1.2.3.4 }
				add element inet ovn-kubernetes starts-empty { 5.6.7.8 }
			`,
		},
		{
			name: "no changed sets",
			initial: `
				add set inet ovn-kubernetes starts-empty { type ipv4_addr ; }
				add set inet ovn-kubernetes becomes-empty { type ipv4_addr ; }
				add element inet ovn-kubernetes becomes-empty { 1.1.1.1 }
				add element inet ovn-kubernetes becomes-empty { 2.2.2.2 }
				add element inet ovn-kubernetes becomes-empty { 3.3.3.3 }
			`,
			containers: []knftables.Object{},
			contents:   []knftables.Object{},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes becomes-empty { type ipv4_addr ; }
				add set inet ovn-kubernetes starts-empty { type ipv4_addr ; }
				add element inet ovn-kubernetes becomes-empty { 1.1.1.1 }
				add element inet ovn-kubernetes becomes-empty { 2.2.2.2 }
				add element inet ovn-kubernetes becomes-empty { 3.3.3.3 }
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = SyncObjects(context.Background(), tc.containers, tc.contents)
			if err != nil {
				t.Fatalf("unexpected error syncing objects: %v", err)
			}
			err = MatchNFTRules(tc.final, fake.Dump())
			if err != nil {
				t.Fatalf("unexpected final result: %v", err)
			}
		})
	}
}
