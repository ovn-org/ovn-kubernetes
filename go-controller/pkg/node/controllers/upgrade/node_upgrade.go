package upgrade

import (
	"context"
	"fmt"
	"strconv"
	"time"

	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// upgradeController detects TopologyVersion edges as broadcast from the ovn-kube master.
// Previously, ovn-kube used an annotation on the Node object. We now use a ConfigMap as the
// coordination point, but will read from Nodes to handle upgrading an existing cluster.
type upgradeController struct {
	client kubernetes.Interface
	wf     factory.NodeWatchFactory
}

// NewController creates a new upgrade controller
func NewController(client kubernetes.Interface, wf factory.NodeWatchFactory) *upgradeController {
	uc := &upgradeController{
		client: client,
		wf:     wf,
	}
	return uc
}

// WaitForTopologyVerions polls continuously until the running master has reported a topology of
// at least the minimum requested.
func (uc *upgradeController) WaitForTopologyVersion(ctx context.Context, minVersion int, timeout time.Duration) error {
	return wait.PollWithContext(ctx, 10*time.Second, timeout, func(ctx context.Context) (bool, error) {
		ver, err := uc.GetTopologyVersion(ctx)
		if err == nil {
			if ver >= minVersion {
				klog.Infof("Cluster topology version is now %d", ver)
				return true, nil
			}

			klog.Infof("Cluster topology version %d < %d", ver, minVersion)
			return false, nil
		}
		klog.Errorf("Failed to retrieve topology version: %v", err)
		return false, nil // swallow error so we retry
	})
}

// GetTopologyVersion polls the coordination points (Nodes and ConfigMaps) until
// the master has reported a version
func (uc *upgradeController) GetTopologyVersion(ctx context.Context) (int, error) {
	// First, check the config map
	ns := globalconfig.Kubernetes.OVNConfigNamespace
	name := ovntypes.OvnK8sStatusCMName
	cm, err := uc.client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Warningf("Error retrieving ConfigMap %s/%s: %v", ns, name, err)
		return -1, err
	}
	if err == nil {
		tv, ok := cm.Data[ovntypes.OvnK8sStatusKeyTopoVersion]
		if ok {
			out, err := strconv.Atoi(tv)
			if err == nil {
				klog.Infof("Detected cluster topology version %d from ConfigMap %s/%s", out, ns, name)
				return out, nil
			} else {
				klog.Infof("ConfigMap %s/%s had invalid value in %s: %s %v",
					ns, name, ovntypes.OvnK8sStatusKeyTopoVersion, tv, err)
			}
		}
	}

	// Masters not yet upgraded, check Node objects
	klog.Infof("Could not determine TopologyVersion via configmap, falling back to Nodes")

	// node-role.kubernetes.io/master label was renamed node-role.kubernetes.io/control-plane
	// in kubernetes 1.24, so a KIND cluster will show the new label; however, Openshift 4.11
	// still uses the old label. Let's check for both until we fully migrate.
	masterNode, err := labels.NewRequirement("node-role.kubernetes.io/master", selection.Exists, nil)
	if err != nil {
		klog.Fatalf("Unable to create labels.NewRequirement: %v", err)
	}
	nodes, err := uc.wf.ListNodes(labels.NewSelector().Add(*masterNode))
	if err != nil {
		return -1, fmt.Errorf("unable to get nodes for checking topo version: %v", err)
	}
	if len(nodes) == 0 {
		klog.Infof("No nodes found with old label node-role.kubernetes.io/master, " +
			"now checking node-role.kubernetes.io/control-plane")
		masterNode, err = labels.NewRequirement("node-role.kubernetes.io/control-plane",
			selection.Exists, nil)
		if err != nil {
			klog.Fatalf("Unable to create labels.NewRequirement: %v", err)
		}
		nodes, err = uc.wf.ListNodes(labels.NewSelector().Add(*masterNode))
		if err != nil {
			return -1, fmt.Errorf("unable to get nodes for checking topo version: %v", err)
		}
		klog.Infof("%d nodes found with node-role.kubernetes.io/control-plane",
			len(nodes))
	}

	ver := -1
	nodeName := ""
	// say, we have three ovnkube-master Pods. on rolling update, one of the Pods will
	// perform the topology/master upgrade and set the topology-version annotation for
	// that node. other ovnkube-master Pods will be in standby mode and wouldn't have
	// updated the annotation. so, we need to get the topology-version from all the
	// nodes and pick the maximum value.
	for _, node := range nodes {
		topoVer, ok := node.Annotations[ovntypes.OvnK8sTopoAnno] //nolint:staticcheck
		if ok && len(topoVer) > 0 {
			v, err := strconv.Atoi(topoVer)
			if err != nil {
				klog.Warningf("Illegal value detected for %s, on node: %s, value: %s, error: %v",
					ovntypes.OvnK8sTopoAnno, node.Name, topoVer, err) //nolint:staticcheck
			} else {
				if v > ver {
					nodeName = node.Name
					ver = v
				}
			}
		}
	}

	if ver == -1 {
		return -1, fmt.Errorf("could not find the topology annotation on Nodes")
	}

	klog.Infof("Detected cluster topology version %d from Node %s", ver, nodeName)
	return ver, nil
}
