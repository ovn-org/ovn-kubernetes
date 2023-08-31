package ovnwebhook

import (
	"context"
	"fmt"

	"golang.org/x/exp/maps"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	listers "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// checkPodAnnot defines additional checks for the allowed annotations
type checkPodAnnot func(nodeLister listers.NodeLister, v annotationChange, pod *corev1.Pod, nodeName string) error

// interconnectPodAnnotationChecks holds annotations allowed for ovnkube-node:<nodeName> users in IC environments
var interconnectPodAnnotations = map[string]checkPodAnnot{
	util.OvnPodAnnotationName: func(nodeLister listers.NodeLister, v annotationChange, pod *corev1.Pod, nodeName string) error {
		// Ignore kubevirt pods with live migration, the IP can cross node-subnet boundaries
		if kubevirt.IsPodLiveMigratable(pod) {
			return nil
		}

		if pod.Spec.HostNetwork {
			return fmt.Errorf("the annotation is not allowed on host networked pods")
		}

		podAnnot, err := util.UnmarshalPodAnnotation(map[string]string{util.OvnPodAnnotationName: v.value}, types.DefaultNetworkName)
		if err != nil {
			return err
		}
		node, err := nodeLister.Get(nodeName)
		if err != nil {
			return fmt.Errorf("could not get info on node %s from client: %w", nodeName, err)
		}

		subnets, err := util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		if err != nil {
			return err
		}
		for _, ip := range podAnnot.IPs {
			if !util.IsContainedInAnyCIDR(ip, subnets...) {
				return fmt.Errorf("%s does not belong to %s node", ip, nodeName)
			}
		}
		return nil
	},
	util.DPUConnectionDetailsAnnot: nil,
	util.DPUConnectionStatusAnnot:  nil,
}

type PodAdmission struct {
	nodeLister        listers.NodeLister
	annotations       map[string]checkPodAnnot
	annotationKeys    sets.Set[string]
	extraAllowedUsers sets.Set[string]
}

func NewPodAdmissionWebhook(nodeLister listers.NodeLister, extraAllowedUsers ...string) *PodAdmission {
	return &PodAdmission{
		nodeLister:        nodeLister,
		annotations:       interconnectPodAnnotations,
		annotationKeys:    sets.New[string](maps.Keys(interconnectPodAnnotations)...),
		extraAllowedUsers: sets.New[string](extraAllowedUsers...),
	}
}

func (p PodAdmission) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	// Ignore creation, the webhook is configured to only handle pod/status updates
	return nil, nil
}

func (p PodAdmission) ValidateDelete(_ context.Context, _ runtime.Object) (warnings admission.Warnings, err error) {
	// Ignore creation, the webhook is configured to only handle pod/status updates
	return nil, nil
}

var _ admission.CustomValidator = &PodAdmission{}

func (p PodAdmission) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	nodeName, isOVNKubeNode := ovnkubeNodeIdentity(req.UserInfo)

	changes := mapDiff(oldPod.Annotations, newPod.Annotations)
	changedKeys := maps.Keys(changes)

	if !isOVNKubeNode {
		if !p.annotationKeys.HasAny(changedKeys...) {
			// the user is not an ovnkube-node and hasn't changed any ovnkube-node annotations
			return nil, nil
		}

		if !p.extraAllowedUsers.Has(req.UserInfo.Username) {
			return nil, fmt.Errorf("user %q is not allowed to set the following annotations on pod: %q: %v",
				req.UserInfo.Username,
				newPod.Name,
				p.annotationKeys.Intersection(sets.New[string](changedKeys...)).UnsortedList())
		}

		// The user is not ovnkube-node, in this case the nodeName comes from the object
		nodeName = newPod.Spec.NodeName
	}

	for _, key := range changedKeys {
		if check := p.annotations[key]; check != nil {
			if err := check(p.nodeLister, changes[key], newPod, nodeName); err != nil {
				return nil, fmt.Errorf("user: %q is not allowed to set %s on pod %q: %v", req.UserInfo.Username, key, newPod.Name, err)
			}
		}
	}

	// All the checks beyond this point are ovnkube-node specific
	// If the user is not ovnkube-node exit here
	if !isOVNKubeNode {
		return nil, nil
	}

	if oldPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to modify pods %q annotations", nodeName, oldPod.Name)
	}
	if newPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to modify pods %q annotations", nodeName, newPod.Name)
	}

	// ovnkube-node is not allowed to change annotations outside of it's scope
	if !p.annotationKeys.HasAll(changedKeys...) {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to set the following annotations on pod: %q: %v",
			nodeName, newPod.Name,
			sets.New[string](changedKeys...).Difference(p.annotationKeys).UnsortedList())
	}

	// Verify that nothing but the annotations changed.
	// Since ovnkube-node only has the pod/status permissions, it is enough to check .Status and .ObjectMeta only.
	// Ignore .ManagedFields fields which are modified on every update.
	oldPodShallowCopy := oldPod
	newPodShallowCopy := newPod
	oldPodShallowCopy.Annotations = nil
	newPodShallowCopy.Annotations = nil
	oldPodShallowCopy.ManagedFields = nil
	newPodShallowCopy.ManagedFields = nil
	if !apiequality.Semantic.DeepEqual(oldPodShallowCopy.ObjectMeta, newPodShallowCopy.ObjectMeta) ||
		!apiequality.Semantic.DeepEqual(oldPodShallowCopy.Status, newPodShallowCopy.Status) {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to modify anything other than annotations", nodeName)
	}

	return nil, nil
}
