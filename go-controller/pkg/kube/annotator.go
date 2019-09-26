package kube

import (
	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
)

// Annotator represents the exported methods for handling node annotations
type Annotator interface {
	Set(key, value string)
	SetWithFailureHandler(key, value string, failFn FailureHandlerFn)
	Del(key string)
	Run()
}

// FailureHandlerFn is a function called when adding an annotation fails
type FailureHandlerFn func(node *kapi.Node, key, val string)

type action struct {
	key    string
	val    string
	failFn FailureHandlerFn
}

type nodeAnnotator struct {
	kube *Kube
	node *kapi.Node

	adds map[string]*action
	dels map[string]*action
}

// NewNodeAnnotator returns a new annotator for Node objects
func NewNodeAnnotator(kube *Kube, node *kapi.Node) Annotator {
	return &nodeAnnotator{
		kube: kube,
		node: node,
		adds: make(map[string]*action),
		dels: make(map[string]*action),
	}
}

func (na *nodeAnnotator) Set(key, val string) {
	na.SetWithFailureHandler(key, val, nil)
}

func (na *nodeAnnotator) SetWithFailureHandler(key, val string, failFn FailureHandlerFn) {
	na.adds[key] = &action{
		key:    key,
		val:    val,
		failFn: failFn,
	}
	delete(na.dels, key)
}

func (na *nodeAnnotator) Del(key string) {
	na.dels[key] = &action{key: key}
	if act, ok := na.adds[key]; ok {
		delete(na.adds, key)
		act.failFn(na.node, act.key, act.val)
	}
}

func (na *nodeAnnotator) Run() {
	adds := make(map[string]string)
	for k, act := range na.adds {
		// Ignore annotations that already exist with the same value
		if existing := na.node.Annotations[k]; existing != act.val {
			adds[k] = act.val
		}
	}
	if len(adds) > 0 {
		if err := na.kube.SetAnnotationsOnNode(na.node, adds); err != nil {
			logrus.Errorf(err.Error())
			// Let failure handlers clean up
			for _, act := range na.adds {
				act.failFn(na.node, act.key, act.val)
			}
		}
	}

	dels := make([]string, 0, len(na.dels))
	for k := range na.dels {
		if _, ok := na.node.Annotations[k]; ok {
			dels = append(dels, k)
		}
	}
	if len(dels) > 0 {
		if err := na.kube.DeleteAnnotationsOnNode(na.node, dels); err != nil {
			logrus.Errorf(err.Error())
		}
	}
}
