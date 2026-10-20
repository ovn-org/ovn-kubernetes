// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/knftables"
)

// Serialize helper transactions with read-before-delete operations so an element
// cannot be replaced between checking its owner and deleting its key.
var objectsMutex sync.Mutex

// AddObjects adds each element of objects to nftables in a single transaction, using
// tx.Add() for each object.
func AddObjects(objects []knftables.Object) error {
	objectsMutex.Lock()
	defer objectsMutex.Unlock()
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	for _, obj := range objects {
		tx.Add(obj)
	}
	return nft.Run(context.TODO(), tx)
}

// DeleteObjects deletes each element of objects from nftables, if it exists; no errors
// are returned for objects that don't exist.
//
// To avoid depending on `nft destroy` (which requires kernel 6.3+), this Add()s each
// element before Delete()ing it, so you won't get an error if the element wasn't already
// in the set/map, which means that for map elements, the Value field must be set to the
// correct value. Alternatively, you can leave Value unset, in which case DeleteObjects
// will first "list" the map to find its current contents, and then only try to delete the
// elements that are actually present.
//
// For knftables.Rule objects, if the Rule's Handle is not set, then it must have a
// non-nil Comment. DeleteObjects will first "list" the Rule's Chain to find its current
// contents, and then delete every rule in the chain with a matching comment.
func DeleteObjects(objects []knftables.Object) error {
	objectsMutex.Lock()
	defer objectsMutex.Unlock()
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	// List existing objects we need to list
	existingMaps := make(map[string][]*knftables.Element)
	existingChains := make(map[string][]*knftables.Rule)
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Map != "" && len(typed.Value) == 0 && existingMaps[typed.Map] == nil {
				existingElements, err := nft.ListElements(context.TODO(), "map", typed.Map)
				if err != nil {
					return err
				}
				existingMaps[typed.Map] = existingElements
			}
		case *knftables.Rule:
			if typed.Handle != nil {
				continue
			} else if typed.Comment == nil {
				return fmt.Errorf("rule passed to DeleteObject must have a Handle or Comment")
			}
			if existingChains[typed.Chain] == nil {
				rules, err := nft.ListRules(context.TODO(), typed.Chain)
				if err != nil {
					return err
				}
				existingChains[typed.Chain] = rules
			}
		default:
			return fmt.Errorf("unsupported object type %T passed to DeleteObjects", obj)
		}
	}

	// Now build the actual transaction
	tx := nft.NewTransaction()
	deletedRules := sets.New[int]()
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Map != "" && len(typed.Value) == 0 {
				// We can't tx.Add() a Map Element with no Value, so try
				// to find its existing value in the List output from
				// above. If the element doesn't appear in that output
				// then we just skip trying to delete it. Otherwise, we do
				// the Add+Delete below just like in the normal case; the
				// Add *should* be a no-op, but doing it anyway makes this
				// work right even if another thread Deletes the element
				// before we get to it (as long as they don't add it back
				// with a different value).
				typed = findElement(existingMaps[typed.Map], typed.Key)
				if typed == nil {
					continue
				}
			}

			// Do Add+Delete, which ensures the object is deleted whether or
			// not it previously existed.
			tx.Add(typed)
			tx.Delete(typed)
		case *knftables.Rule:
			if typed.Handle != nil {
				if !deletedRules.Has(*typed.Handle) {
					tx.Delete(typed)
					deletedRules.Insert(*typed.Handle)
				}
			} else {
				for _, rule := range findRulesByComment(existingChains[typed.Chain], *typed.Comment) {
					if !deletedRules.Has(*rule.Handle) {
						tx.Delete(rule)
						deletedRules.Insert(*rule.Handle)
					}
				}
			}
		default:
		}
	}
	return nft.Run(context.TODO(), tx)
}

// DeleteElementsIfMatching deletes map elements only when their current values
// match the supplied values. Other helper mutations are excluded across the
// lookup and deletion, preserving replacements owned by another network.
func DeleteElementsIfMatching(objects []knftables.Object) error {
	objectsMutex.Lock()
	defer objectsMutex.Unlock()
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}
	existingMaps := make(map[string][]*knftables.Element)
	tx := nft.NewTransaction()
	for _, obj := range objects {
		element, ok := obj.(*knftables.Element)
		if !ok || element.Map == "" || len(element.Value) == 0 {
			return fmt.Errorf("expected a map element with an ownership value, got %T", obj)
		}
		existing, listed := existingMaps[element.Map]
		if !listed {
			existing, err = nft.ListElements(context.TODO(), "map", element.Map)
			if err != nil {
				return fmt.Errorf("failed to list map %s: %w", element.Map, err)
			}
			existingMaps[element.Map] = existing
		}
		current := findElement(existing, element.Key)
		if current != nil && slices.Equal(current.Value, element.Value) {
			tx.Delete(current)
			// A Service can refer to the same address in ExternalIPs and
			// LoadBalancer status. Delete each matching key only once.
			existingMaps[element.Map] = slices.DeleteFunc(slices.Clone(existing), func(e *knftables.Element) bool {
				return slices.Equal(e.Key, element.Key)
			})
		}
	}
	return nft.Run(context.TODO(), tx)
}

// SyncObjects synchronizes the given nftables containers to contain only the elements in
// contents. containers can contain Sets, Maps, and Chains, and contents can contain
// Elements and Rules.
func SyncObjects(containers, contents []knftables.Object) error {
	objectsMutex.Lock()
	defer objectsMutex.Unlock()
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()

	syncContainers := make(map[string]knftables.Object)
	syncChains := make(map[string]*knftables.Chain)
	for _, obj := range containers {
		switch typed := obj.(type) {
		case *knftables.Set:
			syncContainers[typed.Name] = obj
		case *knftables.Map:
			syncContainers[typed.Name] = obj
		case *knftables.Chain:
			syncChains[typed.Name] = typed
		default:
			return fmt.Errorf("unsupported container type %T passed to SyncObjects", obj)
		}
		tx.Flush(obj)
	}

	for _, obj := range contents {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Set != "" && syncContainers[typed.Set] == nil {
				return fmt.Errorf("unexpected element from set %q which is not in containers", typed.Set)
			} else if typed.Map != "" && syncContainers[typed.Map] == nil {
				return fmt.Errorf("unexpected element from map %q which is not in containers", typed.Map)
			}
		case *knftables.Rule:
			if syncChains[typed.Chain] == nil {
				return fmt.Errorf("unexpected rule from chain %q which is not in containers", typed.Chain)
			}
		default:
			return fmt.Errorf("unsupported contents type %T passed to SyncObjects", obj)
		}
		tx.Add(obj)
	}

	err = nft.Run(context.TODO(), tx)
	if err == nil || !knftables.IsNotFound(err) {
		return err
	}

	// For compatibility with
	// https://github.com/ovn-kubernetes/ovn-kubernetes/pull/5250, try again, doing
	// each container separately, ignoring errors if we are asked to make a set/map
	// empty when the set/map doesn't actually exist
	for _, containerName := range sets.List(sets.KeySet(syncContainers)) {
		tx := nft.NewTransaction()
		tx.Flush(syncContainers[containerName])
		keepElems := 0
		for _, obj := range contents {
			switch typed := obj.(type) {
			case *knftables.Element:
				if typed.Set == containerName || typed.Map == containerName {
					tx.Add(obj)
					keepElems++
				}
			}
		}
		err := nft.Run(context.TODO(), tx)
		if err != nil && (!knftables.IsNotFound(err) || keepElems > 0) {
			return err
		}
	}

	for _, chainName := range sets.List(sets.KeySet(syncChains)) {
		tx := nft.NewTransaction()
		tx.Flush(syncChains[chainName])
		keepRules := 0
		for _, obj := range contents {
			switch typed := obj.(type) {
			case *knftables.Rule:
				if typed.Chain == chainName {
					tx.Add(obj)
					keepRules++
				}
			}
		}
		err := nft.Run(context.TODO(), tx)
		if err != nil && (!knftables.IsNotFound(err) || keepRules > 0) {
			return err
		}
	}

	return nil
}

func findElement(elements []*knftables.Element, key []string) *knftables.Element {
elemLoop:
	for _, elem := range elements {
		if len(elem.Key) != len(key) {
			// All elements have the same key length, so if one fails, they all fail.
			return nil
		}
		for i := range elem.Key {
			if elem.Key[i] != key[i] {
				continue elemLoop
			}
		}
		return elem
	}
	return nil
}

func findRulesByComment(rules []*knftables.Rule, comment string) []*knftables.Rule {
	matches := make([]*knftables.Rule, 0, 1)
	for _, rule := range rules {
		if rule.Comment != nil && *rule.Comment == comment {
			matches = append(matches, rule)
		}
	}
	return matches
}
