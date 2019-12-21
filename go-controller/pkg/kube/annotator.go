package kube

import (
	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
)

// Annotator represents the exported methods for handling node annotations
type Annotator interface {
	Set(key string, value interface{})
	SetWithFailureHandler(key string, value interface{}, failFn FailureHandlerFn)
	Del(key string)
	Run()
}

// FailureHandlerFn is a function called when adding an annotation fails
type FailureHandlerFn func(node *kapi.Node, key string, val interface{})

type action struct {
	key    string
	val    interface{}
	failFn FailureHandlerFn
}

type nodeAnnotator struct {
	kube *Kube
	node *kapi.Node

	changes map[string]*action
}

// NewNodeAnnotator returns a new annotator for Node objects
func NewNodeAnnotator(kube *Kube, node *kapi.Node) Annotator {
	return &nodeAnnotator{
		kube:    kube,
		node:    node,
		changes: make(map[string]*action),
	}
}

func (na *nodeAnnotator) Set(key string, val interface{}) {
	na.SetWithFailureHandler(key, val, nil)
}

func (na *nodeAnnotator) SetWithFailureHandler(key string, val interface{}, failFn FailureHandlerFn) {
	na.changes[key] = &action{
		key:    key,
		val:    val,
		failFn: failFn,
	}
}

func (na *nodeAnnotator) Del(key string) {
	na.changes[key] = &action{key: key}
}

func (na *nodeAnnotator) Run() {
	changes := make(map[string]interface{})
	for k, act := range na.changes {
		// Ignore annotations that already exist with the same value
		if existing, ok := na.node.Annotations[k]; existing != act.val || !ok {
			changes[k] = act.val
		}
	}
	if len(changes) == 0 {
		return
	}

	if err := na.kube.SetAnnotationsOnNode(na.node, changes); err != nil {
		logrus.Errorf(err.Error())
		// Let failure handlers clean up
		for _, act := range na.changes {
			act.failFn(na.node, act.key, act.val)
		}
		return
	}

	for k, act := range na.changes {
		if act.val == nil {
			delete(na.node.Annotations, k)
		} else {
			na.node.Annotations[k] = act.val.(string)
		}
	}
}
