// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	nodeutil "k8s.io/component-helpers/node/util"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// checkNodeAnnot defines additional checks for the allowed annotations
type checkNodeAnnot func(v annotationChange, nodeName string) error

// commonNodeAnnotationChecks holds annotations allowed for ovnkube-node:<nodeName> users in all environments.
var commonNodeAnnotationChecks = map[string]checkNodeAnnot{
	util.OVNNodeBridgeEgressIPs:            nil,
	util.OVNNodeHostCIDRs:                  nil,
	util.OVNNodeSecondaryHostEgressIPs:     nil,
	util.OvnNodeL3GatewayConfig:            nil,
	util.OvnNodeManagementPortMacAddresses: nil,
	util.OvnNodeIfAddr:                     nil,
	util.OvnNodeMasqCIDR:                   nil,
	util.OvnNodeGatewayMtuSupport:          nil,
	util.OvnNodeManagementPort:             nil,
	util.OvnNodeDontSNATSubnets:            nil,
	util.OVNNodePrimaryDPUHostAddr:         nil,
	util.OvnNodeChassisID: func(v annotationChange, _ string) error {
		if v.action == removed {
			return fmt.Errorf("%s cannot be removed", util.OvnNodeChassisID)
		}
		if v.action == changed {
			return fmt.Errorf("%s cannot be changed once set", util.OvnNodeChassisID)
		}
		return nil
	},
	util.OvnNodeZoneName: func(v annotationChange, nodeName string) error {
		if (v.action == added || v.action == changed) && v.value == nodeName {
			return nil
		}

		return fmt.Errorf("%s can only be set to %s, it cannot be removed", util.OvnNodeZoneName, nodeName)
	},
	util.OVNNodeEncapIPs: nil,
	util.OVNNodeVTEPs:    nil,
}

// interconnectNodeAnnotationChecks holds annotations allowed for ovnkube-node:<nodeName> users.
var interconnectNodeAnnotationChecks = map[string]checkNodeAnnot{
	util.Layer2TopologyVersion: func(v annotationChange, _ string) error {
		// it is allowed for the annotation to be added or removed
		if v.action == added || v.action == removed {
			return nil
		}
		return fmt.Errorf("%s can only be added or removed, not updated", util.Layer2TopologyVersion)
	},
}

// hybridOverlayNodeAnnotationChecks holds annotations allowed for ovnkube-node:<nodeName> users hybrid overlay environments
var hybridOverlayNodeAnnotationChecks = map[string]checkNodeAnnot{
	hotypes.HybridOverlayDRMAC: nil,
	hotypes.HybridOverlayDRIP:  nil,
}

type NodeAdmission struct {
	annotationChecks  map[string]checkNodeAnnot
	annotationKeys    sets.Set[string]
	extraAllowedUsers sets.Set[string]
}

func NewNodeAdmissionWebhook(enableHybridOverlay bool, extraAllowedUsers ...string) *NodeAdmission {
	checks := make(map[string]checkNodeAnnot)
	maps.Copy(checks, commonNodeAnnotationChecks)
	maps.Copy(checks, interconnectNodeAnnotationChecks)
	if enableHybridOverlay {
		maps.Copy(checks, hybridOverlayNodeAnnotationChecks)
	}
	return &NodeAdmission{
		annotationChecks:  checks,
		annotationKeys:    sets.New[string](slices.Collect(maps.Keys(checks))...),
		extraAllowedUsers: sets.New[string](extraAllowedUsers...),
	}
}

var _ admission.Validator[*corev1.Node] = &NodeAdmission{}

func (p NodeAdmission) ValidateCreate(ctx context.Context, node *corev1.Node) (warnings admission.Warnings, err error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if _, isOVNKubeNode := ovnkubeNodeIdentity(req.UserInfo); isOVNKubeNode {
		// Node credentials only have update permissions on nodes/status.
		return nil, fmt.Errorf("user %q is not allowed to create nodes", req.UserInfo.Username)
	}
	return nil, p.validateProtectedAnnotations(req.UserInfo.Username, node.Name, slices.Collect(maps.Keys(node.Annotations)))
}

func (p NodeAdmission) ValidateDelete(_ context.Context, _ *corev1.Node) (warnings admission.Warnings, err error) {
	// Node deletion is outside the webhook rules.
	return nil, nil
}

func (p NodeAdmission) ValidateUpdate(ctx context.Context, oldNode, newNode *corev1.Node) (warnings admission.Warnings, err error) {

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	nodeName, isOVNKubeNode := ovnkubeNodeIdentity(req.UserInfo)

	changes := mapDiff(oldNode.Annotations, newNode.Annotations)
	changedKeys := slices.Collect(maps.Keys(changes))

	if !isOVNKubeNode {
		if err := p.validateProtectedAnnotations(req.UserInfo.Username, newNode.Name, changedKeys); err != nil {
			return nil, err
		}

		// The user is not ovnkube-node, in this case the nodeName comes from the object
		nodeName = newNode.Name
	}

	for _, key := range changedKeys {
		if check := p.annotationChecks[key]; check != nil {
			if err := check(changes[key], nodeName); err != nil {
				return nil, fmt.Errorf("user: %q is not allowed to set %s on node %q: %v", req.UserInfo.Username, key, newNode.Name, err)
			}
		}
	}

	// All the checks beyond this point are ovnkube-node specific
	// If the user is not ovnkube-node exit here
	if !isOVNKubeNode {
		return nil, nil
	}

	if newNode.Name != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to modify nodes %q annotations", nodeName, newNode.Name)
	}

	// ovnkube-node is not allowed to change annotations outside of it's scope
	if !p.annotationKeys.HasAll(changedKeys...) {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to set the following annotations: %v",
			nodeName,
			sets.New[string](changedKeys...).Difference(p.annotationKeys).UnsortedList())
	}

	// Verify that nothing but the annotations changed.
	// Since ovnkube-node only has the node/status permissions, it is enough to check .Status and .ObjectMeta only.
	// Ignore .ManagedFields fields which are modified on every update.
	oldNodeShallowCopy := *oldNode
	newNodeShallowCopy := *newNode
	oldNodeShallowCopy.Annotations = nil
	newNodeShallowCopy.Annotations = nil
	oldNodeShallowCopy.ManagedFields = nil
	newNodeShallowCopy.ManagedFields = nil

	if strings.HasPrefix(newNode.Spec.ProviderID, "gce") {
		oldId, oldCondition := nodeutil.GetNodeCondition(&(oldNodeShallowCopy.Status), corev1.NodeNetworkUnavailable)
		_, newCondition := nodeutil.GetNodeCondition(&(newNodeShallowCopy.Status), corev1.NodeNetworkUnavailable)

		// In GCP OVN-Kubernetes modifies NodeNetworkUnavailable condition on the nodes, allow an update of the condition
		// https://github.com/ovn-kubernetes/ovn-kubernetes/blob/e2e442133f16699671bb6564c4b8863229841fd9/go-controller/pkg/ovn/master.go#L507
		if oldId >= 0 && newCondition != nil && !reflect.DeepEqual(oldCondition, newCondition) {
			// Replace the old NodeNetworkUnavailable condition with the new one to make DeepEqual happy
			conditionsDeepCopy := make([]corev1.NodeCondition, len(oldNodeShallowCopy.Status.Conditions))
			copy(conditionsDeepCopy, oldNodeShallowCopy.Status.Conditions)
			conditionsDeepCopy[oldId] = *newCondition

			oldNodeShallowCopy.Status.Conditions = conditionsDeepCopy
		}
	}
	if !apiequality.Semantic.DeepEqual(oldNodeShallowCopy.ObjectMeta, newNodeShallowCopy.ObjectMeta) ||
		!apiequality.Semantic.DeepEqual(oldNodeShallowCopy.Status, newNodeShallowCopy.Status) {
		return nil, fmt.Errorf("ovnkube-node on node: %q is not allowed to modify anything other than annotations", nodeName)
	}

	return nil, nil
}

// validateProtectedAnnotations restricts non-node users to explicitly trusted
// identities. Node credentials are checked against annotationKeys separately.
// Controller-owned allocations are protected by the node annotation admission policy.
func (p NodeAdmission) validateProtectedAnnotations(username, nodeName string, changedKeys []string) error {
	changedProtectedKeys := p.annotationKeys.Intersection(sets.New(changedKeys...))
	if changedProtectedKeys.Len() == 0 || p.extraAllowedUsers.Has(username) {
		return nil
	}
	return fmt.Errorf("user %q is not allowed to set the following annotations on node: %q: %v",
		username, nodeName, sets.List(changedProtectedKeys))
}
