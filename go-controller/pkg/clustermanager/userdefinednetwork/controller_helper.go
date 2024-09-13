package userdefinednetwork

import (
	"context"
	"fmt"
	"reflect"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	utiludn "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/udn"
)

// TODO: replace NAD with target namespace
func (c *Controller) updateNAD(obj client.Object, nad *netv1.NetworkAttachmentDefinition) (*netv1.NetworkAttachmentDefinition, error) {
	desiredNAD, err := c.renderNadFn(obj, obj.GetNamespace())
	if err != nil {
		return nil, fmt.Errorf("failed to generate NetworkAttachmentDefinition: %w", err)
	}

	if nad == nil {
		// creating NAD in case no primary network exist should be atomic and synchronized with
		// any other thread that create NADs.
		// Since the UserDefinedNetwork controller use single thread (threadiness=1),
		// and being the only controller that create NADs, this conditions is fulfilled.
		if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
			actualNads, err := c.nadLister.NetworkAttachmentDefinitions(obj.GetNamespace()).List(labels.Everything())
			if err != nil {
				return nil, fmt.Errorf("failed to list  NetworkAttachmetDefinition: %w", err)
			}
			// This is best-effort check no primary NAD exist before creating one,
			// noting prevent primary NAD from being created right after this check.
			if err := PrimaryNetAttachDefNotExist(actualNads); err != nil {
				return nil, err
			}
		}

		// TODO: add lock for primary NAD creation, avoid conflict with other primary UDN creators
		nad, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(obj.GetNamespace()).Create(context.Background(), desiredNAD, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create NetworkAttachmentDefinition: %w", err)
		}
		klog.Infof("Created NetworkAttachmentDefinition [%s/%s]", nad.Namespace, nad.Name)

		return nad, nil
	}

	if !metav1.IsControlledBy(nad, obj) {
		return nil, fmt.Errorf("foreign NetworkAttachmentDefinition with the desired name already exist [%s/%s]", nad.Namespace, nad.Name)
	}

	if !reflect.DeepEqual(nad.Spec.Config, desiredNAD.Spec.Config) {
		nad.Spec.Config = desiredNAD.Spec.Config
		uNAD, uerr := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Update(context.Background(), nad, metav1.UpdateOptions{})
		if uerr != nil {
			return nil, fmt.Errorf("failed to update NetworkAttachmentDefinition: %w", uerr)
		}
		klog.Infof("Updated NetworkAttachmentDefinition [%s/%s]", uNAD.Namespace, uNAD.Name)
		return uNAD, nil
	}

	return nad, nil
}

func (c *Controller) deleteNAD(obj client.Object, nad *netv1.NetworkAttachmentDefinition) error {
	if nad == nil ||
		!metav1.IsControlledBy(nad, obj) ||
		!controllerutil.ContainsFinalizer(nad, template.FinalizerUserDefinedNetwork) {
		return nil
	}

	pods, err := c.podInformer.Lister().Pods(nad.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods at target namesapce %q: %w", nad.Namespace, err)
	}
	// This is best-effort check no pod using the subject NAD,
	// noting prevent a from being pod creation right after this check.
	if err := NetAttachDefNotInUse(nad, pods); err != nil {
		return &networkInUseError{err: err}
	}

	controllerutil.RemoveFinalizer(nad, template.FinalizerUserDefinedNetwork)
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Update(context.Background(), nad, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove NetworkAttachmetDefinition finalizer: %w", err)
	}
	klog.Infof("Finalizer removed from NetworkAttachmetDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(updatedNAD.Namespace).Delete(context.Background(), updatedNAD.Name, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	}
	klog.Infof("Deleted NetworkAttachmetDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return nil
}
