//go:build linux
// +build linux

package nftables

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/knftables"
)

// AddObjects adds each element of objects to nftables in a single transaction, using
// tx.Add() for each object.
func AddObjects(ctx context.Context, objects []knftables.Object) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()
	for _, obj := range objects {
		tx.Add(obj)
	}
	return nft.Run(ctx, tx)
}

// DeleteObjects deletes each element of objects from nftables, if it exists; no errors
// are returned for objects that don't exist.
//
// Currently DeleteObjects only supports knftables.Element. Every Element in objects will
// be deleted from its corresponding set/map (if it is currently a member). If an Element
// refers to a set/map that doesn't already exist, an error will be returned.
//
// If you pass map elements without the Value field set then DeleteObjects will need to
// first "list" the map to find its current contents, to ensure that it only tries to
// delete the elements that are actually present. (With set elements, or map elements with
// Value set, it can just do an Add+Delete to get the effect of "delete whether or not it
// previously existed".)
func DeleteObjects(ctx context.Context, objects []knftables.Object) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	// If there are any partial Map Elements, list their existing contents
	existingMaps := make(map[string][]*knftables.Element)
	for _, obj := range objects {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Map != "" && len(typed.Value) == 0 && existingMaps[typed.Map] == nil {
				elements, err := nft.ListElements(ctx, "map", typed.Map)
				if err != nil {
					return err
				}
				existingMaps[typed.Map] = elements
			}
		default:
			return fmt.Errorf("unsupported object type %T passed to DeleteObjects", obj)
		}
	}

	// Now build the actual transaction
	tx := nft.NewTransaction()
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
		default:
		}
	}
	return nft.Run(ctx, tx)
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

// SyncObjects synchronizes the given nftables containers to contain only the elements in
// contents. Currently containers must contain only Sets and Maps, while contents must
// contain only Elements.
func SyncObjects(ctx context.Context, containers, contents []knftables.Object) error {
	nft, err := GetNFTablesHelper()
	if err != nil {
		return err
	}

	tx := nft.NewTransaction()

	syncContainers := sets.New[string]()
	for _, obj := range containers {
		switch typed := obj.(type) {
		case *knftables.Set:
			syncContainers.Insert(typed.Name)
		case *knftables.Map:
			syncContainers.Insert(typed.Name)
		default:
			return fmt.Errorf("unsupported container type %T passed to SyncObjects", obj)
		}
		tx.Flush(obj)
	}

	for _, obj := range contents {
		switch typed := obj.(type) {
		case *knftables.Element:
			if typed.Set != "" && !syncContainers.Has(typed.Set) {
				return fmt.Errorf("unexpected element from set %q which is not in containers", typed.Set)
			} else if typed.Map != "" && !syncContainers.Has(typed.Map) {
				return fmt.Errorf("unexpected element from map %q which is not in containers", typed.Map)
			}
		default:
			return fmt.Errorf("unsupported contents type %T passed to SyncObjects", obj)
		}
		tx.Add(obj)
	}

	return nft.Run(ctx, tx)
}
