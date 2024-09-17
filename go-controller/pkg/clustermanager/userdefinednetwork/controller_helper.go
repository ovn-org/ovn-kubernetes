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

func (c *Controller) updateNAD(obj client.Object, namespace string) (*netv1.NetworkAttachmentDefinition, error) {
	desiredNAD, err := c.renderNadFn(obj, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to generate NetworkAttachmentDefinition: %w", err)
	}

	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !kerrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}
	nadCopy := nad.DeepCopy()

	if nadCopy == nil {
		// creating NAD in case no primary network exist should be atomic and synchronized with
		// any other thread that create NADs.
		c.createNetworkLock.Lock()
		defer c.createNetworkLock.Unlock()

		if utiludn.IsPrimaryNetwork(template.GetSpec(obj)) {
			actualNads, err := c.nadLister.NetworkAttachmentDefinitions(namespace).List(labels.Everything())
			if err != nil {
				return nil, fmt.Errorf("failed to list  NetworkAttachmentDefinition: %w", err)
			}
			// This is best-effort check no primary NAD exist before creating one,
			// noting prevent primary NAD from being created right after this check.
			if err := PrimaryNetAttachDefNotExist(actualNads); err != nil {
				return nil, err
			}
		}

		newNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), desiredNAD, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to create NetworkAttachmentDefinition: %w", err)
		}
		klog.Infof("Created NetworkAttachmentDefinition [%s/%s]", newNAD.Namespace, newNAD.Name)

		return newNAD, nil
	}

	if !metav1.IsControlledBy(nadCopy, obj) {
		return nil, fmt.Errorf("foreign NetworkAttachmentDefinition with the desired name already exist [%s/%s]", nadCopy.Namespace, nadCopy.Name)
	}

	if reflect.DeepEqual(nadCopy.Spec.Config, desiredNAD.Spec.Config) {
		return nadCopy, nil
	}

	nadCopy.Spec.Config = desiredNAD.Spec.Config
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadCopy.Namespace).Update(context.Background(), nadCopy, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to update NetworkAttachmentDefinition: %w", err)
	}
	klog.Infof("Updated NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return updatedNAD, nil
}

func (c *Controller) deleteNAD(obj client.Object, namespace string) error {
	nad, err := c.nadLister.NetworkAttachmentDefinitions(namespace).Get(obj.GetName())
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to get NetworkAttachmentDefinition %s/%s from cache: %v", namespace, obj.GetName(), err)
	}
	nadCopy := nad.DeepCopy()

	if nadCopy == nil ||
		!metav1.IsControlledBy(nadCopy, obj) ||
		!controllerutil.ContainsFinalizer(nadCopy, template.FinalizerUserDefinedNetwork) {
		return nil
	}

	pods, err := c.podInformer.Lister().Pods(nadCopy.Namespace).List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods at target namesapce %q: %w", nadCopy.Namespace, err)
	}
	// This is best-effort check no pod using the subject NAD,
	// noting prevent a from being pod creation right after this check.
	if err := NetAttachDefNotInUse(nadCopy, pods); err != nil {
		return &networkInUseError{err: err}
	}

	controllerutil.RemoveFinalizer(nadCopy, template.FinalizerUserDefinedNetwork)
	updatedNAD, err := c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadCopy.Namespace).Update(context.Background(), nadCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove NetworkAttachmentDefinition finalizer: %w", err)
	}
	klog.Infof("Finalizer removed from NetworkAttachmentDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(updatedNAD.Namespace).Delete(context.Background(), updatedNAD.Name, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	}
	klog.Infof("Deleted NetworkAttachmetDefinition [%s/%s]", updatedNAD.Namespace, updatedNAD.Name)

	return nil
}
