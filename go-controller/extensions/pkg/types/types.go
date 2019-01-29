package types

import (
	kapi "k8s.io/api/core/v1"
)

// NodeHandler interface respresents the three functions that get called by the informer upon respective events
type NodeHandler interface {

	// Add is called when a new object is created
	Add(obj *kapi.Node)

	// Update is called when an object is updated, both old and new ones are passed along
	Update(oldObj *kapi.Node, newObj *kapi.Node)

	// Delete is called when an object is deleted
	Delete(obj *kapi.Node)
}
