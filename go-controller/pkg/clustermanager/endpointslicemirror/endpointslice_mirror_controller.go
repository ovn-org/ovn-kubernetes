package endpointslicemirror

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	networkAttachDefController "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/network-attach-def-controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const maxRetries = 10

// Controller represents the EndpointSlice mirror controller.
// For namespaces that use a user-defined primary network, this controller mirrors the default EndpointSlices
// (managed by the default Kubernetes EndpointSlice controller) into new EndpointSlices that contain the addresses
// from the primary network.
type Controller struct {
	kubeClient kubernetes.Interface
	wg         *sync.WaitGroup
	queue      workqueue.TypedRateLimitingInterface[string]
	name       string

	endpointSliceLister  discoverylisters.EndpointSliceLister
	endpointSlicesSynced cache.InformerSynced
	podLister            corelisters.PodLister
	podsSynced           cache.InformerSynced
	nadController        *networkAttachDefController.NetAttachDefinitionController
	cancel               context.CancelFunc
}

// getDefaultEndpointSliceKey returns the key for the default EndpointSlice associated with the given EndpointSlice.
// For mirrored EndpointSlices it returns the key based on the value of the "k8s.ovn.org/source-endpointslice" label.
// For default EndpointSlices it returns the <namespace>/<name> key.
// For other EndpointSlices it returns an empty value.
func (c *Controller) getDefaultEndpointSliceKey(endpointSlice *v1.EndpointSlice) string {
	if c.isManagedByController(endpointSlice) {
		defaultEndpointSliceName, found := endpointSlice.Labels[types.LabelSourceEndpointSlice]
		if !found {
			utilruntime.HandleError(fmt.Errorf("couldn't determine the source EndpointSlice for %s", cache.MetaObjectToName(endpointSlice)))
			return ""
		}

		return cache.ObjectName{
			Namespace: endpointSlice.Namespace,
			Name:      defaultEndpointSliceName,
		}.String()
	}

	if isManagedByDefault(endpointSlice) {
		key, err := cache.MetaNamespaceKeyFunc(endpointSlice)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", endpointSlice, err))
			return ""
		}
		return key
	}

	// the EndpointSlice is not managed by either of the controllers
	return ""
}

func (c *Controller) enqueueEndpointSlice(obj interface{}) {
	eps, ok := obj.(*v1.EndpointSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("passed object is neither an EndpointSlice nor a DeletedFinalStateUnknown type: %#v", obj))
			return
		}
		eps, ok = tombstone.Obj.(*v1.EndpointSlice)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a EndpointSlice: %#v", obj))
			return
		}
	}
	if key := c.getDefaultEndpointSliceKey(eps); key != "" {
		c.queue.AddRateLimited(key)
	}
}

func (c *Controller) onEndpointSliceUpdate(_ interface{}, new interface{}) {
	c.enqueueEndpointSlice(new)
}

func (c *Controller) onEndpointSliceDelete(obj interface{}) {
	c.enqueueEndpointSlice(obj)
}

func (c *Controller) onEndpointSliceAdd(obj interface{}) {
	c.enqueueEndpointSlice(obj)
}

func NewController(
	ovnClient *util.OVNClusterManagerClientset,
	wf *factory.WatchFactory, nadController *networkAttachDefController.NetAttachDefinitionController) (*Controller, error) {

	wg := &sync.WaitGroup{}
	c := &Controller{
		kubeClient:    ovnClient.KubeClient,
		wg:            wg,
		name:          types.EndpointSliceMirrorControllerName,
		nadController: nadController,
	}

	c.queue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: c.name},
	)

	c.podLister = wf.PodCoreInformer().Lister()
	c.podsSynced = wf.PodCoreInformer().Informer().HasSynced

	endpointSlicesInformer := wf.EndpointSliceCoreInformer()
	c.endpointSliceLister = endpointSlicesInformer.Lister()
	c.endpointSlicesSynced = endpointSlicesInformer.Informer().HasSynced
	_, err := endpointSlicesInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onEndpointSliceAdd,
		UpdateFunc: c.onEndpointSliceUpdate,
		DeleteFunc: c.onEndpointSliceDelete,
	}))

	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Start(ctx context.Context, threadiness int) error {
	defer utilruntime.HandleCrash()

	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	klog.Infof("Starting the EndpointSlice mirror controller")
	klog.Infof("Repairing EndpointSlice mirrors")
	err := c.repair(ctx)
	if err != nil {
		klog.Errorf("Failed to repair EndpointSlice mirrors: %v", err)
	}

	for i := 0; i < threadiness; i++ {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			wait.Until(func() {
				c.runEndpointSliceWorker(ctx, c.wg)
			}, time.Second, ctx.Done())
		}()
	}

	return nil
}

func (c *Controller) Stop() {
	klog.Infof("Shutting down %s", c.name)

	c.cancel()
	c.queue.ShutDown()
	c.wg.Wait()
}

// repair syncs all existing EndpointSlices to add any missing entries and remove stale ones
func (c *Controller) repair(ctx context.Context) error {
	endpointSlices, err := c.endpointSliceLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, endpointSlice := range endpointSlices {
		if key := c.getDefaultEndpointSliceKey(endpointSlice); key != "" {
			if err := c.syncDefaultEndpointSlice(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Controller) runEndpointSliceWorker(ctx context.Context, wg *sync.WaitGroup) {
	for c.processNextEgressServiceWorkItem(ctx, wg) {
	}
}

func (c *Controller) processNextEgressServiceWorkItem(ctx context.Context, wg *sync.WaitGroup) bool {
	wg.Add(1)
	defer wg.Done()

	key, quit := c.queue.Get()
	if quit {
		return false
	}

	defer c.queue.Done(key)

	err := c.syncDefaultEndpointSlice(ctx, key)
	if err == nil {
		c.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))

	if c.queue.NumRequeues(key) < maxRetries {
		c.queue.AddRateLimited(key)
		return true
	}

	klog.Warningf("Dropping EndpointSlice %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	return true
}

// getGenerateName returns a new GenerateName in the format of <network>-<name>
// if the resulting string is not a valid GenerateName, the original name is returned
func getGenerateName(name, network string) string {
	generateName := fmt.Sprintf("%s-%s", network, name)
	if errs := validation.NameIsDNSSubdomain(generateName, true); len(errs) != 0 {
		return name
	}
	return generateName
}

// syncDefaultEndpointSlice reconciles the provided default EndpointSlice into the mirrored EndpointSlice
func (c *Controller) syncDefaultEndpointSlice(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	namespacePrimaryNetwork, err := c.nadController.GetActiveNetworkForNamespace(namespace)
	if err != nil {
		return err
	}

	if namespacePrimaryNetwork.IsDefault() || !namespacePrimaryNetwork.IsPrimaryNetwork() {
		return nil
	}

	mirrorEndpointSliceSelector := labels.Set(map[string]string{
		types.LabelSourceEndpointSlice: name,
		v1.LabelManagedBy:              c.name,
	}).AsSelectorPreValidated()

	klog.Infof("Processing %s/%s EndpointSlice in %q primary network", namespace, name, namespacePrimaryNetwork.GetNetworkName())

	defaultEndpointSlice, err := c.endpointSliceLister.EndpointSlices(namespace).Get(name)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	var mirroredEndpointSlice *v1.EndpointSlice

	slices, err := c.endpointSliceLister.EndpointSlices(namespace).List(mirrorEndpointSliceSelector)
	if err != nil {
		return err
	}

	if len(slices) == 1 {
		mirroredEndpointSlice = slices[0]
	}
	if len(slices) > 1 {
		klog.Errorf("Found %d mirrored EndpointSlices for %s/%s, removing all of them", len(slices), namespace, name)
		if err := c.kubeClient.DiscoveryV1().EndpointSlices(namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: mirrorEndpointSliceSelector.String()}); err != nil {
			return err
		}
		// return an error so there is a retry that will recreate the correct mirrored EndpointSlice
		return fmt.Errorf("found and removed %d mirrored EndpointSlices for %s/%s", len(slices), namespace, name)
	}

	if defaultEndpointSlice == nil {
		if mirroredEndpointSlice != nil {
			klog.Infof("The default EndpointSlice %s/%s no longer exists, removing the mirrored one: %s", namespace, mirroredEndpointSlice.Labels[types.LabelSourceEndpointSlice], cache.MetaObjectToName(mirroredEndpointSlice))
			return c.kubeClient.DiscoveryV1().EndpointSlices(namespace).Delete(ctx, mirroredEndpointSlice.Name, metav1.DeleteOptions{})
		}
		klog.Infof("The default EndpointSlice %s/%s no longer exists", namespace, name)
		return nil
	}

	// defaultEndpointSlice cannot be nil beyond this point
	if defaultEndpointSlice.AddressType == v1.AddressTypeFQDN {
		if mirroredEndpointSlice != nil {
			klog.Infof("The default EndpointSlice %s is of type %q, removing the mirrored one: %s", cache.MetaObjectToName(defaultEndpointSlice), v1.AddressTypeFQDN, cache.MetaObjectToName(mirroredEndpointSlice))
			return c.kubeClient.DiscoveryV1().EndpointSlices(namespace).Delete(ctx, mirroredEndpointSlice.Name, metav1.DeleteOptions{})
		}
		return nil
	}

	if mirroredEndpointSlice != nil {
		// nothing to do if we already reconciled this exact EndpointSlice
		if mirroredResourceVersion, ok := mirroredEndpointSlice.Labels[types.LabelSourceEndpointSliceVersion]; ok {
			if mirroredResourceVersion == defaultEndpointSlice.ResourceVersion {
				return nil
			}
		}
	}

	currentMirror, err := c.mirrorEndpointSlice(mirroredEndpointSlice, defaultEndpointSlice, namespacePrimaryNetwork)
	if err != nil {
		return err
	}

	if !reflect.DeepEqual(currentMirror, mirroredEndpointSlice) {
		if currentMirror.Name == "" {
			klog.Infof("Creating the mirrored EndpointSlice for: %s", cache.MetaObjectToName(defaultEndpointSlice))
			_, err := c.kubeClient.DiscoveryV1().EndpointSlices(namespace).Create(ctx, currentMirror, metav1.CreateOptions{})
			return err
		}
		klog.Infof("Updating the mirrored EndpointSlice: %s for: %s", cache.MetaObjectToName(currentMirror), cache.MetaObjectToName(defaultEndpointSlice))
		_, err := c.kubeClient.DiscoveryV1().EndpointSlices(namespace).Update(ctx, currentMirror, metav1.UpdateOptions{})
		return err
	}
	return nil
}

// isManagedByController determines if the provided endpointSlice is managed by the current controller by checking the
// "endpointslice.kubernetes.io/managed-by" label value.
func (c *Controller) isManagedByController(endpointSlice *v1.EndpointSlice) bool {
	return c.name == endpointSlice.Labels[v1.LabelManagedBy]
}

// isManagedByController determines if the provided endpointSlice is managed by the default EndpointSlice
// controller by checking the "endpointslice.kubernetes.io/managed-by" label value.
func isManagedByDefault(endpointSlice *v1.EndpointSlice) bool {
	return types.EndpointSliceDefaultControllerName == endpointSlice.Labels[v1.LabelManagedBy]
}

// getPodIP retrieves the IP address of a specified Pod within a given namespace and network.
// If the pod is host networked it returns default pod IP from the status ignoring the network.
// Otherwise, it unmarshals the Pod's network annotation, and matches the IP from the provided network.
func (c *Controller) getPodIP(name, namespace, network string, isIPv6 bool) (string, error) {
	var podIP string
	pod, err := c.podLister.Pods(namespace).Get(name)
	if err != nil {
		return "", err
	}

	if pod.Spec.HostNetwork {
		podIPs, err := util.DefaultNetworkPodIPs(pod)
		if err != nil {
			return "", err
		}

		// MatchFirstIPFamily returns an error if no IP was found
		ipAddr, err := util.MatchFirstIPFamily(isIPv6, podIPs)
		if err != nil {
			return "", err
		}

		podIP = ipAddr.String()
	} else {
		net, err := util.UnmarshalPodAnnotation(pod.Annotations, network)
		if err != nil {
			return "", err
		}

		// MatchFirstIPFamily returns an error if no IP was found
		ipNet, err := util.MatchFirstIPNetFamily(isIPv6, net.IPs)
		if err != nil {
			return "", err
		}
		podIP = ipNet.IP.String()
	}

	return podIP, nil
}

// mirrorEndpointSlice creates or updates a mirrored EndpointSlice based on the provided defaultEndpointSlice.
// The mirrored EndpointSlice will have custom labels set and will be managed by the current controller.
func (c *Controller) mirrorEndpointSlice(mirroredEndpointSlice, defaultEndpointSlice *v1.EndpointSlice, network util.NetInfo) (*v1.EndpointSlice, error) {
	var currentMirror *v1.EndpointSlice
	if mirroredEndpointSlice != nil {
		currentMirror = mirroredEndpointSlice.DeepCopy()
	}
	if currentMirror == nil {
		currentMirror = &v1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       defaultEndpointSlice.Namespace,
				OwnerReferences: defaultEndpointSlice.OwnerReferences,
			},
		}
	}
	if currentMirror.Labels == nil {
		currentMirror.Labels = map[string]string{}
	}
	currentMirror.AddressType = defaultEndpointSlice.AddressType
	currentMirror.Ports = defaultEndpointSlice.Ports

	// set the custom labels, generateName and reset the endpoints
	currentMirror.Labels[v1.LabelManagedBy] = c.name
	currentMirror.Labels[types.LabelSourceEndpointSlice] = defaultEndpointSlice.Name
	currentMirror.Labels[types.LabelSourceEndpointSliceVersion] = defaultEndpointSlice.ResourceVersion
	currentMirror.Labels[types.LabelUserDefinedEndpointSliceNetwork] = network.GetNetworkName()
	currentMirror.Labels[types.LabelUserDefinedServiceName] = defaultEndpointSlice.Labels[v1.LabelServiceName]

	// Set the GenerateName only for new objects
	if len(currentMirror.Name) == 0 {
		origGenName := defaultEndpointSlice.GenerateName
		if len(origGenName) == 0 {
			origGenName = defaultEndpointSlice.Name
		}
		currentMirror.GenerateName = getGenerateName(origGenName, network.GetNetworkName())
	}

	currentMirror.Endpoints = make([]v1.Endpoint, len(defaultEndpointSlice.Endpoints))
	isIPv6 := defaultEndpointSlice.AddressType == v1.AddressTypeIPv6
	nadList := network.GetNADs()
	if len(nadList) != 1 {
		return nil, fmt.Errorf("expected one NAD in %s network, got: %d", network.GetNetworkName(), len(nadList))
	}
	for i, endpoint := range defaultEndpointSlice.Endpoints {
		if endpoint.TargetRef != nil && endpoint.TargetRef.Kind == "Pod" {
			podIP, err := c.getPodIP(endpoint.TargetRef.Name, endpoint.TargetRef.Namespace, network.GetNADs()[0], isIPv6)
			if err != nil {
				return nil, fmt.Errorf("failed to determine the Pod IP of: %s/%s: %v", endpoint.TargetRef.Namespace, endpoint.TargetRef.Name, err)
			}
			newEp := endpoint.DeepCopy()
			newEp.Addresses = []string{podIP}
			currentMirror.Endpoints[i] = *newEp
		}
	}
	return currentMirror, nil
}
