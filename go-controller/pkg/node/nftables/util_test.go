// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"
	"reflect"
	"testing"
	"time"

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
		{
			name: "add rule",
			initial: `
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop
			`,
			objs: []knftables.Object{
				&knftables.Rule{
					Chain: "testchain",
					Rule:  "ip saddr 5.6.7.8 drop",
				},
				&knftables.Rule{
					Chain: "testchain",
					Rule:  "ip saddr 1.2.3.4 drop",
				},
			},
			final: `
				add table inet ovn-kubernetes
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop
				add rule inet ovn-kubernetes testchain ip saddr 5.6.7.8 drop
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = AddObjects(tc.objs)
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
		{
			name: "delete with duplicate element",
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
					Key: []string{"1.2.3.4"},
				},
			},
			final: `
				add table inet ovn-kubernetes
				add set inet ovn-kubernetes testset { type ipv4_addr ; }
				add element inet ovn-kubernetes testset { 5.6.7.8 }
			`,
		},
		{
			name: "delete rules",
			initial: `
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain bad-rule-with-no-comment
				add rule inet ovn-kubernetes testchain ip saddr 5.6.7.8 drop comment "two"
				add rule inet ovn-kubernetes testchain ip saddr 9.1.2.3 drop comment "two"
				add rule inet ovn-kubernetes testchain ip saddr 4.5.6.7 drop comment "three"
			`,
			objs: []knftables.Object{
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 5.6.7.8 drop",
					Comment: knftables.PtrTo("two"),
				},
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 1.2.3.4 drop",
					Comment: knftables.PtrTo("no match"),
				},
			},
			final: `
				add table inet ovn-kubernetes
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain bad-rule-with-no-comment
				add rule inet ovn-kubernetes testchain ip saddr 4.5.6.7 drop comment "three"
			`,
		},
		{
			name: "delete with duplicate rule",
			initial: `
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain ip saddr 5.6.7.8 drop comment "two"
			`,
			objs: []knftables.Object{
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 5.6.7.8 drop",
					Comment: knftables.PtrTo("two"),
				},
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 5.6.7.8 drop",
					Comment: knftables.PtrTo("two"),
				},
			},
			final: `
				add table inet ovn-kubernetes
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
			`,
		},
		{
			name: "delete with rule comment only",
			initial: `
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain ip saddr 5.6.7.8 drop comment "two"
			`,
			objs: []knftables.Object{
				&knftables.Rule{
					Chain:   "testchain",
					Comment: knftables.PtrTo("two"),
				},
			},
			final: `
				add table inet ovn-kubernetes
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = DeleteObjects(tc.objs)
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
		err        bool
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
		{
			name: "sync rules",
			initial: `
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain bad-rule-with-no-comment
				add rule inet ovn-kubernetes testchain ip saddr 5.6.7.8 drop comment "two"
				add rule inet ovn-kubernetes testchain ip saddr 9.1.2.3 drop comment "two"
				add rule inet ovn-kubernetes testchain ip saddr 4.5.6.7 drop comment "three"
			`,
			containers: []knftables.Object{
				&knftables.Chain{
					Name: "testchain",
				},
			},
			contents: []knftables.Object{
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 1.2.3.4 drop",
					Comment: knftables.PtrTo("one"),
				},
				&knftables.Rule{
					Chain:   "testchain",
					Rule:    "ip saddr 9.1.2.3 drop",
					Comment: knftables.PtrTo("two"),
				},
			},
			final: `
				add table inet ovn-kubernetes
				add chain inet ovn-kubernetes testchain
				add rule inet ovn-kubernetes testchain ip saddr 1.2.3.4 drop comment "one"
				add rule inet ovn-kubernetes testchain ip saddr 9.1.2.3 drop comment "two"
			`,
		},
		{
			name: "bad container type",
			containers: []knftables.Object{
				&knftables.Table{},
			},
			err: true,
		},
		{
			name: "bad contents type",
			contents: []knftables.Object{
				&knftables.Table{},
			},
			err: true,
		},
		{
			name: "element not in containers",
			containers: []knftables.Object{
				&knftables.Set{
					Name: "set",
				},
			},
			contents: []knftables.Object{
				&knftables.Element{
					Set: "wrong-set",
					Key: []string{"1.2.3.4"},
				},
			},
			err: true,
		},
		{
			name: "rule not in containers",
			containers: []knftables.Object{
				&knftables.Chain{
					Name: "chain",
				},
			},
			contents: []knftables.Object{
				&knftables.Rule{
					Chain: "wrong-chain",
					Rule:  "drop",
				},
			},
			err: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := SetFakeNFTablesHelper()
			err := fake.ParseDump(tc.initial)
			if err != nil {
				t.Fatalf("unexpected error parsing initial state: %v", err)
			}
			err = SyncObjects(tc.containers, tc.contents)
			if tc.err {
				if err == nil {
					t.Fatalf("expected an error but got none")
				}
				return
			}
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

// pausedMapLookup holds the ownership snapshot while another helper tries to
// replace the element, exercising the lookup-to-delete race deterministically.
type pausedMapLookup struct {
	knftables.Interface
	listed chan struct{}
	resume chan struct{}
}

func (n *pausedMapLookup) ListElements(ctx context.Context, objectType, name string) ([]*knftables.Element, error) {
	elements, err := n.Interface.ListElements(ctx, objectType, name)
	close(n.listed)
	<-n.resume
	return elements, err
}

// TestDeleteElementsIfMatchingConcurrentReplacement ensures that a new owner
// cannot be installed between the old owner's snapshot and key deletion.
func TestDeleteElementsIfMatchingConcurrentReplacement(t *testing.T) {
	fake := SetFakeNFTablesHelper()
	original := &knftables.Element{Map: "owners", Key: []string{"80"}, Value: []string{"1"}}
	replacement := &knftables.Element{Map: "owners", Key: []string{"80"}, Value: []string{"2"}}
	if err := AddObjects([]knftables.Object{&knftables.Map{Name: "owners", Type: "inet_service : mark"}, original}); err != nil {
		t.Fatalf("initialize ownership map: %v", err)
	}
	paused := &pausedMapLookup{Interface: fake, listed: make(chan struct{}), resume: make(chan struct{})}
	nftHelper = paused
	t.Cleanup(func() { nftHelper = fake })
	deleted := make(chan error, 1)
	go func() { deleted <- DeleteElementsIfMatching([]knftables.Object{original}) }()
	select {
	case <-paused.listed:
	case <-time.After(5 * time.Second):
		close(paused.resume)
		t.Fatal("timed out waiting for ownership lookup")
	}
	started := make(chan struct{})
	added := make(chan error, 1)
	go func() {
		close(started)
		added <- AddObjects([]knftables.Object{replacement})
	}()
	<-started
	// The replacement must wait while the conditional deletion owns its snapshot.
	var early bool
	select {
	case err := <-added:
		early = true
		t.Errorf("replacement completed during ownership lookup: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(paused.resume)
	select {
	case err := <-deleted:
		if err != nil {
			t.Errorf("delete original owner: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out deleting original owner")
	}
	if !early {
		select {
		case err := <-added:
			if err != nil {
				t.Errorf("install replacement owner: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out installing replacement owner")
		}
	}
	elements, err := fake.ListElements(context.Background(), "map", "owners")
	if err != nil {
		t.Fatalf("read replacement owner: %v", err)
	}
	if len(elements) != 1 || !reflect.DeepEqual(elements[0].Value, replacement.Value) {
		t.Fatalf("replacement owner was lost: %+v", elements)
	}
}
