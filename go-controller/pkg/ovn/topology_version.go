package ovn

import (
	"context"
	"fmt"
	"math"
	"strconv"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

func (oc *Controller) ovnTopologyCleanup(ctx context.Context) error {
	ver, err := oc.determineOVNTopoVersionFromOVN(ctx)
	if err != nil {
		return err
	}

	// Cleanup address sets in non dual stack formats in all versions known to possibly exist.
	if ver <= ovntypes.OvnPortBindingTopoVersion {
		err = addressset.NonDualStackAddressSetCleanup(oc.nbClient)
	}
	return err
}

// reportTopologyVersion saves the topology version to two places:
// - an ExternalID on the ovn_cluster_router LogicalRouter in nbdb
// - a ConfigMap. This is used by nodes to determine the cluster's topology
func (oc *Controller) reportTopologyVersion(ctx context.Context) error {
	currentTopologyVersion := strconv.Itoa(ovntypes.OvnCurrentTopologyVersion)
	logicalRouterRes := []nbdb.LogicalRouter{}
	ctx, cancel := context.WithTimeout(ctx, ovntypes.OVSDBTimeout)
	defer cancel()
	if err := oc.nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		return lr.Name == ovntypes.OVNClusterRouter
	}).List(ctx, &logicalRouterRes); err != nil {
		return fmt.Errorf("failed in retrieving %s, error: %v", ovntypes.OVNClusterRouter, err)
	}
	// Update topology version on distributed cluster router
	logicalRouterRes[0].ExternalIDs["k8s-ovn-topo-version"] = currentTopologyVersion
	logicalRouter := nbdb.LogicalRouter{
		Name:        ovntypes.OVNClusterRouter,
		ExternalIDs: logicalRouterRes[0].ExternalIDs,
	}
	opModel := libovsdbops.OperationModel{
		Name:           &logicalRouter.Name,
		Model:          &logicalRouter,
		ModelPredicate: func(lr *nbdb.LogicalRouter) bool { return lr.Name == ovntypes.OVNClusterRouter },
		OnModelUpdates: []interface{}{
			&logicalRouter.ExternalIDs,
		},
		ErrNotFound: true,
	}
	if _, err := oc.modelClient.CreateOrUpdate(opModel); err != nil {
		return fmt.Errorf("failed to generate set topology version in OVN, err: %v", err)
	}
	klog.Infof("Updated Logical_Router %s topology version to %s", ovntypes.OVNClusterRouter, currentTopologyVersion)

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
func (oc *Controller) cleanTopologyAnnotation() error {
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
				newNode := node.DeepCopy()
				delete(newNode.Annotations, anno)
				klog.Infof("Deleting topology annotation from node %s", node.Name)
				return oc.kube.PatchNode(node, newNode)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// determineOVNTopoVersionFromOVN determines what OVN Topology version is being used
// If "k8s-ovn-topo-version" key in external_ids column does not exist, it is prior to OVN topology versioning
// and therefore set version number to OvnCurrentTopologyVersion
func (oc *Controller) determineOVNTopoVersionFromOVN(ctx context.Context) (int, error) {
	ver := 0
	logicalRouterRes := []nbdb.LogicalRouter{}
	ctx, cancel := context.WithTimeout(ctx, ovntypes.OVSDBTimeout)
	defer cancel()
	if err := oc.nbClient.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		return lr.Name == ovntypes.OVNClusterRouter
	}).List(ctx, &logicalRouterRes); err != nil {
		return ver, fmt.Errorf("failed in retrieving %s to determine the current version of OVN logical topology: "+
			"error: %v", ovntypes.OVNClusterRouter, err)
	}
	if len(logicalRouterRes) == 0 {
		// no OVNClusterRouter exists, DB is empty, nothing to upgrade
		return math.MaxInt32, nil
	}
	v, exists := logicalRouterRes[0].ExternalIDs["k8s-ovn-topo-version"]
	if !exists {
		klog.Infof("No version string found. The OVN topology is before versioning is introduced. Upgrade needed")
		return ver, nil
	}
	ver, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid OVN topology version string for the cluster, err: %v", err)
	}
	return ver, nil
}
