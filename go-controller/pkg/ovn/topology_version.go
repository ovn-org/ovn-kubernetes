package ovn

import (
	"context"
	"strconv"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// reportTopologyVersion saves the topology version to two places:
// - an ExternalID on the ovn_cluster_router LogicalRouter in nbdb
// - a ConfigMap. This is used by nodes to determine the cluster's topology
func (oc *DefaultNetworkController) reportTopologyVersion(ctx context.Context) error {
	err := oc.updateL3TopologyVersion()
	if err != nil {
		return err
	}

	currentTopologyVersion := strconv.Itoa(ovntypes.OvnCurrentTopologyVersion)
	// Report topology version in a ConfigMap
	// (we used to report this via annotations on our Node)
	cm := corev1apply.ConfigMap(ovntypes.OvnK8sStatusCMName, globalconfig.Kubernetes.OVNConfigNamespace)
	cm.Data = map[string]string{ovntypes.OvnK8sStatusKeyTopoVersion: currentTopologyVersion}
	if _, err := oc.client.CoreV1().ConfigMaps(globalconfig.Kubernetes.OVNConfigNamespace).Apply(ctx, cm, metav1.ApplyOptions{
		Force:        true,
		FieldManager: "ovn-kubernetes",
	}); err != nil {
		return err
	}

	klog.Infof("Updated ConfigMap %s/%s topology version to %s", *cm.Namespace, *cm.Name, currentTopologyVersion)

	return oc.cleanTopologyAnnotation()
}

// Remove the old topology annotation from nodes, if it exists.
func (oc *DefaultNetworkController) cleanTopologyAnnotation() error {
	// Unset the old topology annotation on all Node objects
	nodes, err := oc.watchFactory.GetNodes()
	if err != nil {
		return err
	}
	anno := ovntypes.OvnK8sTopoAnno //nolint // otherwise we get deprecation warnings (this variable is deprecated)
	for _, node := range nodes {
		if _, ok := node.Annotations[anno]; !ok {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err := oc.kube.GetNode(node.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if _, ok := node.Annotations[anno]; ok {
				klog.Infof("Deleting topology annotation from node %s", node.Name)
				// Setting the annotation value to nil removes it
				return oc.kube.SetAnnotationsOnNode(node.Name, map[string]interface{}{anno: nil})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
