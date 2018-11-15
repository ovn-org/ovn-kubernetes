package factory

import (
	"reflect"
	"testing"

	"k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Watch Factory Suite")
}

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newPod(name, namespace string) *v1.Pod {
	return &v1.Pod{
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
		},
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: "mynode",
		},
	}
}

func newNamespace(name string) *v1.Namespace {
	return &v1.Namespace{
		Status: v1.NamespaceStatus{
			Phase: v1.NamespaceActive,
		},
		ObjectMeta: newObjectMeta(name, name),
	}
}

func newNode(name string) *v1.Node {
	return &v1.Node{
		Status: v1.NodeStatus{
			Phase: v1.NodeRunning,
		},
		ObjectMeta: newObjectMeta(name, ""),
	}
}

func newPolicy(name, namespace string) *knet.NetworkPolicy {
	return &knet.NetworkPolicy{
		ObjectMeta: newObjectMeta(name, namespace),
	}
}

func newEndpoints(name, namespace string) *v1.Endpoints {
	return &v1.Endpoints{
		ObjectMeta: newObjectMeta(name, namespace),
	}
}

func newService(name, namespace string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			UID:       types.UID(name),
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
	}
}

func objSetup(c *fake.Clientset, objType string, listFn func(core.Action) (bool, runtime.Object, error)) *watch.FakeWatcher {
	w := watch.NewFake()
	c.AddWatchReactor(objType, core.DefaultWatchReactor(w, nil))
	c.AddReactor("list", objType, listFn)
	return w
}

var _ = Describe("Watch Factory Operations", func() {
	var (
		fakeClient                                *fake.Clientset
		podWatch, namespaceWatch, nodeWatch       *watch.FakeWatcher
		policyWatch, endpointsWatch, serviceWatch *watch.FakeWatcher
		pods                                      []*v1.Pod
		namespaces                                []*v1.Namespace
		nodes                                     []*v1.Node
		policies                                  []*knet.NetworkPolicy
		endpoints                                 []*v1.Endpoints
		services                                  []*v1.Service
		stop                                      chan struct{}
		numAdded, numUpdated, numDeleted          int
	)

	BeforeEach(func() {
		fakeClient = &fake.Clientset{}
		stop = make(chan struct{})

		pods = make([]*v1.Pod, 0)
		podWatch = objSetup(fakeClient, "pods", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.PodList{}
			for _, p := range pods {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		namespaces = make([]*v1.Namespace, 0)
		namespaceWatch = objSetup(fakeClient, "namespaces", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.NamespaceList{}
			for _, p := range namespaces {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		nodes = make([]*v1.Node, 0)
		nodeWatch = objSetup(fakeClient, "nodes", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.NodeList{}
			for _, p := range nodes {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		policies = make([]*knet.NetworkPolicy, 0)
		policyWatch = objSetup(fakeClient, "networkpolicies", func(core.Action) (bool, runtime.Object, error) {
			obj := &knet.NetworkPolicyList{}
			for _, p := range policies {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		endpoints = make([]*v1.Endpoints, 0)
		endpointsWatch = objSetup(fakeClient, "endpoints", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.EndpointsList{}
			for _, p := range endpoints {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		services = make([]*v1.Service, 0)
		serviceWatch = objSetup(fakeClient, "services", func(core.Action) (bool, runtime.Object, error) {
			obj := &v1.ServiceList{}
			for _, p := range services {
				obj.Items = append(obj.Items, *p)
			}
			return true, obj, nil
		})

		numAdded = 0
		numUpdated = 0
		numDeleted = 0
	})

	Context("when a processExisting is given", func() {
		testExisting := func(objType reflect.Type, namespace string, lsel *metav1.LabelSelector) {
			wf, err := NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.addHandler(objType, namespace, lsel,
				cache.ResourceEventHandlerFuncs{},
				func(objs []interface{}) {
					Expect(len(objs)).To(Equal(1))
				})
			Expect(err).NotTo(HaveOccurred())
			Expect(h).NotTo(BeNil())
			wf.removeHandler(objType, h)
			close(stop)
		}

		It("is called for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			testExisting(podType, "", nil)
		})

		It("is called for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			testExisting(namespaceType, "", nil)
		})

		It("is called for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			testExisting(nodeType, "", nil)
		})

		It("is called for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			testExisting(policyType, "", nil)
		})

		It("is called for each existing endpoints", func() {
			endpoints = append(endpoints, newEndpoints("myendpoint", "default"))
			testExisting(endpointsType, "", nil)
		})

		It("is called for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			testExisting(serviceType, "", nil)
		})

		It("is called for each existing pod that matches a given namespace and label", func() {
			pod := newPod("pod1", "default")
			pod.ObjectMeta.Labels["blah"] = "foobar"
			pods = append(pods, pod)
			testExisting(podType, "default", &metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			})
		})
	})

	Context("when existing items are known to the informer", func() {
		testExisting := func(objType reflect.Type) {
			wf, err := NewWatchFactory(fakeClient, stop)
			Expect(err).NotTo(HaveOccurred())
			h, err := wf.addHandler(objType, "", nil,
				cache.ResourceEventHandlerFuncs{
					AddFunc: func(obj interface{}) {
						numAdded++
					},
					UpdateFunc: func(old, new interface{}) {},
					DeleteFunc: func(obj interface{}) {},
				}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(numAdded).To(Equal(2))
			wf.removeHandler(objType, h)
			close(stop)
		}

		It("calls ADD for each existing pod", func() {
			pods = append(pods, newPod("pod1", "default"))
			pods = append(pods, newPod("pod2", "default"))
			testExisting(podType)
		})

		It("calls ADD for each existing namespace", func() {
			namespaces = append(namespaces, newNamespace("default"))
			namespaces = append(namespaces, newNamespace("default2"))
			testExisting(namespaceType)
		})

		It("calls ADD for each existing node", func() {
			nodes = append(nodes, newNode("default"))
			nodes = append(nodes, newNode("default2"))
			testExisting(nodeType)
		})

		It("calls ADD for each existing policy", func() {
			policies = append(policies, newPolicy("denyall", "default"))
			policies = append(policies, newPolicy("denyall2", "default"))
			testExisting(policyType)
		})

		It("calls ADD for each existing endpoints", func() {
			endpoints = append(endpoints, newEndpoints("myendpoint", "default"))
			endpoints = append(endpoints, newEndpoints("myendpoint2", "default"))
			testExisting(endpointsType)
		})

		It("calls ADD for each existing service", func() {
			services = append(services, newService("myservice", "default"))
			services = append(services, newService("myservice2", "default"))
			testExisting(serviceType)
		})
	})

	addFilteredHandler := func(wf *WatchFactory, objType reflect.Type, namespace string, lsel *metav1.LabelSelector, funcs cache.ResourceEventHandlerFuncs) *Handler {
		h, err := wf.addHandler(objType, namespace, lsel, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				defer GinkgoRecover()
				numAdded++
				funcs.AddFunc(obj)
			},
			UpdateFunc: func(old, new interface{}) {
				defer GinkgoRecover()
				numUpdated++
				funcs.UpdateFunc(old, new)
			},
			DeleteFunc: func(obj interface{}) {
				defer GinkgoRecover()
				numDeleted++
				funcs.DeleteFunc(obj)
			},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(h).NotTo(BeNil())
		return h
	}

	addHandler := func(wf *WatchFactory, objType reflect.Type, funcs cache.ResourceEventHandlerFuncs) *Handler {
		return addFilteredHandler(wf, objType, "", nil, funcs)
	}

	It("responds to pod add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newPod("pod1", "default")
		h := addHandler(wf, podType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newPod := new.(*v1.Pod)
				Expect(reflect.DeepEqual(newPod, added)).To(BeTrue())
				Expect(newPod.Spec.NodeName).To(Equal("foobar"))
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				Expect(reflect.DeepEqual(pod, added)).To(BeTrue())
			},
		})

		pods = append(pods, added)
		podWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Spec.NodeName = "foobar"
		podWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		pods = pods[:0]
		podWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.RemovePodHandler(h)
		close(stop)
	})

	It("responds to namespace add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h := addHandler(wf, namespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				ns := obj.(*v1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNS := new.(*v1.Namespace)
				Expect(reflect.DeepEqual(newNS, added)).To(BeTrue())
				Expect(newNS.Status.Phase).To(Equal(v1.NamespaceTerminating))
			},
			DeleteFunc: func(obj interface{}) {
				ns := obj.(*v1.Namespace)
				Expect(reflect.DeepEqual(ns, added)).To(BeTrue())
			},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Status.Phase = v1.NamespaceTerminating
		namespaceWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		namespaces = namespaces[:0]
		namespaceWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.RemoveNamespaceHandler(h)
		close(stop)
	})

	It("responds to node add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newNode("mynode")
		h := addHandler(wf, nodeType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNode := new.(*v1.Node)
				Expect(reflect.DeepEqual(newNode, added)).To(BeTrue())
				Expect(newNode.Status.Phase).To(Equal(v1.NodeTerminated))
			},
			DeleteFunc: func(obj interface{}) {
				node := obj.(*v1.Node)
				Expect(reflect.DeepEqual(node, added)).To(BeTrue())
			},
		})

		nodes = append(nodes, added)
		nodeWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Status.Phase = v1.NodeTerminated
		nodeWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		nodes = nodes[:0]
		nodeWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.removeHandler(nodeType, h)
		close(stop)
	})

	It("responds to policy add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newPolicy("mypolicy", "default")
		h := addHandler(wf, policyType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(newNP, added)).To(BeTrue())
				Expect(newNP.Spec.PolicyTypes).To(Equal([]knet.PolicyType{knet.PolicyTypeIngress}))
			},
			DeleteFunc: func(obj interface{}) {
				np := obj.(*knet.NetworkPolicy)
				Expect(reflect.DeepEqual(np, added)).To(BeTrue())
			},
		})

		policies = append(policies, added)
		policyWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Spec.PolicyTypes = []knet.PolicyType{knet.PolicyTypeIngress}
		policyWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		policies = policies[:0]
		policyWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.removeHandler(policyType, h)
		close(stop)
	})

	It("responds to endpoints add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newEndpoints("myendpoints", "default")
		h := addHandler(wf, endpointsType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				eps := obj.(*v1.Endpoints)
				Expect(reflect.DeepEqual(eps, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newEPs := new.(*v1.Endpoints)
				Expect(reflect.DeepEqual(newEPs, added)).To(BeTrue())
				Expect(len(newEPs.Subsets)).To(Equal(1))
			},
			DeleteFunc: func(obj interface{}) {
				eps := obj.(*v1.Endpoints)
				Expect(reflect.DeepEqual(eps, added)).To(BeTrue())
			},
		})

		endpoints = append(endpoints, added)
		endpointsWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Subsets = append(added.Subsets, v1.EndpointSubset{
			Ports: []v1.EndpointPort{
				{
					Name: "foobar",
					Port: 1234,
				},
			},
		})
		endpointsWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		endpoints = endpoints[:0]
		endpointsWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.removeHandler(endpointsType, h)
		close(stop)
	})

	It("responds to service add/update/delete events", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newService("myservice", "default")
		h := addHandler(wf, serviceType, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				service := obj.(*v1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
			UpdateFunc: func(old, new interface{}) {
				newService := new.(*v1.Service)
				Expect(reflect.DeepEqual(newService, added)).To(BeTrue())
				Expect(newService.Spec.ClusterIP).To(Equal("1.1.1.1"))
			},
			DeleteFunc: func(obj interface{}) {
				service := obj.(*v1.Service)
				Expect(reflect.DeepEqual(service, added)).To(BeTrue())
			},
		})

		services = append(services, added)
		serviceWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		added.Spec.ClusterIP = "1.1.1.1"
		serviceWatch.Modify(added)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))
		services = services[:0]
		serviceWatch.Delete(added)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		wf.removeHandler(serviceType, h)
		close(stop)
	})

	It("stops processing events after the handler is removed", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		added := newNamespace("default")
		h := addHandler(wf, namespaceType, cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) {},
			UpdateFunc: func(old, new interface{}) {},
			DeleteFunc: func(obj interface{}) {},
		})

		namespaces = append(namespaces, added)
		namespaceWatch.Add(added)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))
		wf.RemoveNamespaceHandler(h)

		added2 := newNamespace("other")
		namespaces = append(namespaces, added2)
		namespaceWatch.Add(added2)
		Consistently(func() int { return numAdded }, 2).Should(Equal(1))

		added2.Status.Phase = v1.NamespaceTerminating
		namespaceWatch.Modify(added2)
		Consistently(func() int { return numUpdated }, 2).Should(Equal(0))
		namespaces = []*v1.Namespace{added}
		namespaceWatch.Delete(added2)
		Consistently(func() int { return numDeleted }, 2).Should(Equal(0))

		close(stop)
	})

	It("filters correctly by label and namespace", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		passesFilter := newPod("pod1", "default")
		passesFilter.ObjectMeta.Labels["blah"] = "foobar"
		failsFilter := newPod("pod2", "default")
		failsFilter.ObjectMeta.Labels["blah"] = "baz"
		failsFilter2 := newPod("pod3", "otherns")
		failsFilter2.ObjectMeta.Labels["blah"] = "foobar"

		addFilteredHandler(wf,
			podType,
			"default",
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
				UpdateFunc: func(old, new interface{}) {
					newPod := new.(*v1.Pod)
					Expect(reflect.DeepEqual(newPod, passesFilter)).To(BeTrue())
				},
				DeleteFunc: func(obj interface{}) {
					pod := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(pod, passesFilter)).To(BeTrue())
				},
			})

		pods = append(pods, passesFilter)
		podWatch.Add(passesFilter)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter)
		podWatch.Add(failsFilter)
		Consistently(func() int { return numAdded }, 2).Should(Equal(1))

		// numAdded should remain 1
		pods = append(pods, failsFilter2)
		podWatch.Add(failsFilter2)
		Consistently(func() int { return numAdded }, 2).Should(Equal(1))

		passesFilter.Status.Phase = v1.PodFailed
		podWatch.Modify(passesFilter)
		Eventually(func() int { return numUpdated }, 2).Should(Equal(1))

		// numAdded should remain 1
		failsFilter.Status.Phase = v1.PodFailed
		podWatch.Modify(failsFilter)
		Consistently(func() int { return numUpdated }, 2).Should(Equal(1))

		failsFilter2.Status.Phase = v1.PodFailed
		podWatch.Modify(failsFilter2)
		Consistently(func() int { return numUpdated }, 2).Should(Equal(1))

		pods = []*v1.Pod{failsFilter, failsFilter2}
		podWatch.Delete(passesFilter)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))

		close(stop)
	})

	It("correctly handles object updates that cause filter changes", func() {
		wf, err := NewWatchFactory(fakeClient, stop)
		Expect(err).NotTo(HaveOccurred())

		pod := newPod("pod1", "default")
		pod.ObjectMeta.Labels["blah"] = "baz"

		equalPod := pod
		h := addFilteredHandler(wf,
			podType,
			"default",
			&metav1.LabelSelector{
				MatchLabels: map[string]string{"blah": "foobar"},
			},
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					p := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
				UpdateFunc: func(old, new interface{}) {},
				DeleteFunc: func(obj interface{}) {
					p := obj.(*v1.Pod)
					Expect(reflect.DeepEqual(p, equalPod)).To(BeTrue())
				},
			})

		pods = append(pods, pod)

		// Pod doesn't pass filter; shouldn't be added
		podWatch.Add(pod)
		Consistently(func() int { return numAdded }, 2).Should(Equal(0))

		// Update pod to pass filter; should be treated as add.  Need
		// to deep-copy pod when modifying because it's a pointer all
		// the way through when using FakeClient
		podCopy := pod.DeepCopy()
		podCopy.ObjectMeta.Labels["blah"] = "foobar"
		pods = []*v1.Pod{podCopy}
		equalPod = podCopy
		podWatch.Modify(podCopy)
		Eventually(func() int { return numAdded }, 2).Should(Equal(1))

		// Update pod to fail filter; should be treated as delete
		pod.ObjectMeta.Labels["blah"] = "baz"
		podWatch.Modify(pod)
		Eventually(func() int { return numDeleted }, 2).Should(Equal(1))
		Consistently(func() int { return numAdded }, 2).Should(Equal(1))
		Consistently(func() int { return numUpdated }, 2).Should(Equal(0))

		wf.RemovePodHandler(h)
		close(stop)
	})
})
