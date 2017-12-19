package cluster

import (
	"fmt"
	"net"
	"sync"

	"github.com/sirupsen/logrus"

	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
)

type nsFunc func(*kapi.Namespace) error
type watchFunc func(cache.ResourceEventHandler)

type namespaceWatcher struct {
	sync.Mutex

	nodeName   string
	namespaces map[string]int

	watchFactory *factory.WatchFactory

	nsAdded   nsFunc
	nsChanged nsFunc
	nsRemoved nsFunc

	kubeClient kube.Interface
}

// If nodeName is given, nsAdded/nsRemoved will only be called when pods
// scheduled on the given nodeName in that namespace are started and stopped.
func newNamespaceWatcher(kubeClient kube.Interface, nodeName string, nsAdded, nsChanged, nsRemoved nsFunc, watchFactory *factory.WatchFactory) (*namespaceWatcher, error) {
	watcher := &namespaceWatcher{
		namespaces:   make(map[string]int),
		nodeName:     nodeName,
		watchFactory: watchFactory,
		nsAdded:      nsAdded,
		nsChanged:    nsChanged,
		nsRemoved:    nsRemoved,
		kubeClient:   kubeClient,
	}

	// Read and ref existing namespaces
	existingNamespaces, err := kubeClient.GetNamespaces()
	if err != nil {
		return nil, fmt.Errorf("error fetching namespaces: %v", err)
	}
	for _, ns := range existingNamespaces.Items {
		existingPods, err := kubeClient.GetPods(ns.Name)
		if err != nil {
			return nil, fmt.Errorf("Error in initializing/fetching pods in %q: %v", ns.Name, err)
		}
		for _, pod := range existingPods.Items {
			if watcher.nodeName == "" || pod.Spec.NodeName == watcher.nodeName {
				watcher.namespaceRefUnlocked(&ns)
			}
		}
	}

	watcher.watchPods()
	watcher.watchNamespaces()
	return watcher, nil
}

func (w *namespaceWatcher) namespaceRefUnlocked(ns *kapi.Namespace) error {
	val := w.namespaces[ns.Name] + 1
	w.namespaces[ns.Name] = val
	if val == 1 && w.nsAdded != nil {
		if err := w.nsAdded(ns); err != nil {
			return fmt.Errorf("namespace %q add processing failed: %v", ns.Name, err)
		}
	}
	return nil
}

func (w *namespaceWatcher) namespaceRef(namespace string) error {
	ns, err := w.kubeClient.GetNamespace(namespace)
	if err != nil {
		return fmt.Errorf("Failed to get namespace %q: %v", namespace, err)
	}

	w.Lock()
	defer w.Unlock()
	return w.namespaceRefUnlocked(ns)
}

func (w *namespaceWatcher) namespaceChanged(ns *kapi.Namespace) error {
	w.Lock()
	defer w.Unlock()
	if _, ok := w.namespaces[ns.Name]; ok && w.nsChanged != nil {
		return w.nsChanged(ns)
	}

	return nil
}

func (w *namespaceWatcher) namespaceUnref(namespace string) error {
	ns, err := w.kubeClient.GetNamespace(namespace)
	if err != nil {
		return fmt.Errorf("Failed to get namespace %q: %v", namespace, err)
	}

	w.Lock()
	defer w.Unlock()

	val, ok := w.namespaces[namespace]
	if !ok || val == 1 {
		if !ok {
			logrus.Errorf("Unbalanced namespace unref for %q", namespace)
		}
		w.namespaceDeleteUnlocked(ns)
	} else {
		w.namespaces[namespace] = val - 1
	}
	return nil
}

func (w *namespaceWatcher) namespaceDeleteUnlocked(ns *kapi.Namespace) error {
	delete(w.namespaces, ns.Name)
	return w.nsRemoved(ns)
}

func (w *namespaceWatcher) namespaceDelete(ns *kapi.Namespace) error {
	w.Lock()
	defer w.Unlock()
	return w.namespaceDeleteUnlocked(ns)
}

func podDetail(pod *kapi.Pod) string {
	return fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
}

func (w *namespaceWatcher) watchPods() {
	w.watchFactory.AddPodHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*kapi.Pod)
			logrus.Debugf("Added event for pod %q: %#v", podDetail(pod), pod)
			if w.nodeName != "" && pod.Spec.NodeName != w.nodeName {
				// Not scheduled on this node
				return
			}
			if err := w.namespaceRef(pod.Namespace); err != nil {
				logrus.Errorf("error creating subnet for namespace %q: %v", pod.Namespace, err)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			if w.nodeName == "" {
				return
			}
			oldPod, ok := old.(*kapi.Pod)
			if !ok {
				return
			}
			newPod, ok := new.(*kapi.Pod)
			if !ok {
				return
			}
			logrus.Debugf("Updated event for pod %q", podDetail(newPod))
			if oldPod.Spec.NodeName == "" && newPod.Spec.NodeName == w.nodeName {
				if err := w.namespaceRef(newPod.Namespace); err != nil {
					logrus.Errorf("error updating pod for ref of namespace %q: %v", newPod.Namespace, err)
				}
			} else if oldPod.Spec.NodeName == w.nodeName && newPod.Spec.NodeName == "" {
				if err := w.namespaceUnref(oldPod.Namespace); err != nil {
					logrus.Errorf("error updating pod for unref of namespace %q: %v", oldPod.Namespace, err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*kapi.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				pod, ok = tombstone.Obj.(*kapi.Pod)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a pod %#v", obj)
					return
				}
			}
			logrus.Debugf("Delete event for pod %q", podDetail(pod))
			if w.nodeName != "" && pod.Spec.NodeName != w.nodeName {
				// Not scheduled on this node
				return
			}
			if err := w.namespaceUnref(pod.Namespace); err != nil {
				logrus.Errorf("error deleting pod %q: %v", podDetail(pod), err)
			}
		},
	}, nil)
}

func (w *namespaceWatcher) watchNamespaces() {
	w.watchFactory.AddNamespaceHandler(cache.ResourceEventHandlerFuncs{
		// We wait for the first pod in the namespace before we create a subnet for it
		AddFunc: func(obj interface{}) {},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := new.(*kapi.Namespace)
			if !ok {
				return
			}
			if err := w.namespaceChanged(ns); err != nil {
				logrus.Errorf("error updating namespace %q: %v", ns.Namespace, err)
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*kapi.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					logrus.Errorf("couldn't get object from tombstone %+v", obj)
					return
				}
				ns, ok = tombstone.Obj.(*kapi.Namespace)
				if !ok {
					logrus.Errorf("tombstone contained object that is not a namespace %#v", obj)
					return
				}
			}
			logrus.Debugf("Delete event for namespace %q", ns.Name)
			err := w.namespaceDelete(ns)
			if err != nil {
				logrus.Errorf("Error deleting namespace %q: %v", ns.Name, err)
			}
		},
	}, nil)
}

func gatewayMACForNamespaceSubnet(cidr *net.IPNet) string {
	return fmt.Sprintf("0a:58:%02x:%02x:%02x:%02x", cidr.IP[0], cidr.IP[1], cidr.IP[2], cidr.IP[3])
}
