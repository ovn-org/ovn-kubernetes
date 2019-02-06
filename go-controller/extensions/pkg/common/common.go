package common

import (
	"fmt"
	"reflect"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/master"
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/node"
	"github.com/openvswitch/ovn-kubernetes/go-controller/extensions/pkg/types"

	kapi "k8s.io/api/core/v1"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	resyncInterval = 12 * time.Hour
)

var (
	nodeType reflect.Type = reflect.TypeOf(&kapi.Node{})
)

// RunNode is the top level function to run hybrid-sdn in node mode
func RunNode(clientset kubernetes.Interface, stopChan chan struct{}) error {
	h := node.NewNodeHandler(clientset)
	return watch(clientset, stopChan, h)
}

// RunMaster is the top level function to run hybrid-sdn in master mode
func RunMaster(clientset kubernetes.Interface, stopChan chan struct{}) error {
	h := master.NewNodeHandler(clientset)
	return watch(clientset, stopChan, h)
}

func watch(clientset kubernetes.Interface, stopChan chan struct{}, h types.NodeHandler) error {
	// create factory and start the node informer
	factory := informerfactory.NewSharedInformerFactory(clientset, resyncInterval)
	inf := factory.Core().V1().Nodes().Informer()
	factory.Start(stopChan)
	res := factory.WaitForCacheSync(stopChan)
	for _, synced := range res {
		if !synced {
			return fmt.Errorf("Failed to sync node informer")
		}
	}

	// synced, so set the handler
	inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*kapi.Node)
			if !ok {
				logrus.Errorf("Event ADD got an unexpected object of type %v", reflect.TypeOf(obj))
				return
			}
			logrus.Debugf("running %v ADD event", nodeType)
			h.Add(node)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, ok := oldObj.(*kapi.Node)
			if !ok {
				logrus.Errorf("Event UPDATE got an unexpected object of type %v", reflect.TypeOf(oldObj))
				return
			}
			newNode, ok := newObj.(*kapi.Node)
			if !ok {
				logrus.Errorf("Event UPDATE got an unexpected object of type %v", reflect.TypeOf(newObj))
				return
			}
			logrus.Debugf("running %v UPDATE event", nodeType)
			h.Update(oldNode, newNode)
		},
		DeleteFunc: func(obj interface{}) {
			if reflect.TypeOf(obj) != nodeType {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone: %+v", obj)
					return
				}
				obj = tombstone.Obj
				objType := reflect.TypeOf(obj)
				if nodeType != objType {
					logrus.Errorf("expected tombstone object resource type %v but got %v", nodeType, objType)
					return
				}
			}
			logrus.Debugf("running %v DELETE event", nodeType)
			h.Delete(obj.(*kapi.Node))
		},
	})
	return nil
}
