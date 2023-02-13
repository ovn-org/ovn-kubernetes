package unidling

import (
	"fmt"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	kapi "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	GracePeriodDuration = 30 * time.Second
	IdledAtSuffix       = "/idled-at"
	UnidledAtSuffix     = "/unidled-at"
	UnidledAtAnnotation = "k8s.ovn.org" + UnidledAtSuffix
)

type unidledAtController struct {
	kube kube.Interface
}

// NewUnidledAtController installs a controller on the passed informer. Whenever a service annotation
// like "*/idled-at" is removed from a service, it sets "k8s.ovn.org/unidled-at" with the current time,
// indicating the point in time the service has been unidled.
func NewUnidledAtController(k kube.Interface, serviceInformer cache.SharedIndexInformer) *unidledAtController {

	uac := &unidledAtController{
		kube: k,
	}

	klog.Info("Setting up event handlers for services")
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: uac.onServiceUpdate,
	})

	return uac
}

// HasIdleAt returns true if the service annotation map contains a key like "*/idled-at"
func HasIdleAt(svc *kapi.Service) bool {
	if svc == nil {
		return false
	}

	if svc.Annotations == nil {
		return false
	}

	for annotationKey := range svc.Annotations {
		if strings.HasSuffix(annotationKey, IdledAtSuffix) {
			return true
		}
	}

	return false
}

// IsOnGracePeriod return true if the service has been unidled less than 30s (grace period) ago.
func IsOnGracePeriod(svc *kapi.Service) bool {
	ok, unidledAtStr := getUnidleAt(svc)
	if !ok {
		return false
	}

	unidledAtTime, err := time.Parse(time.RFC3339, unidledAtStr)
	if err != nil {
		klog.Warningf("Bad value [%s] for [%s] annotation on service [%s/%s]", unidledAtStr, UnidledAtAnnotation, svc.Namespace, svc.Name)
		return false
	}

	endOfGracePeriod := unidledAtTime.Add(GracePeriodDuration)

	return time.Now().Before(endOfGracePeriod)
}

func (uac *unidledAtController) onServiceUpdate(old, new interface{}) {
	oldSvc := old.(*kapi.Service)
	newSvc := new.(*kapi.Service)

	oldSvcIdleAt := HasIdleAt(oldSvc)
	newSvcIdleAt := HasIdleAt(newSvc)

	if oldSvcIdleAt && !newSvcIdleAt {
		// Service has been unidled, fill the unidled-at annotation with timing
		err := uac.setUnidleAtAnnotation(newSvc)
		if err != nil {
			utilruntime.HandleError(err)
		}

		return
	}

	if !oldSvcIdleAt && newSvcIdleAt {
		// Service has been idled, remove unidled-at annotation
		err := uac.removeUnidleAtAnnotation(newSvc)
		if err != nil {
			utilruntime.HandleError(err)
		}

		return
	}

}

func (uac *unidledAtController) setUnidleAtAnnotation(svc *kapi.Service) error {

	nowStr := time.Now().Format(time.RFC3339)
	err := uac.kube.SetAnnotationsOnService(svc.Namespace, svc.Name, map[string]interface{}{
		UnidledAtAnnotation: nowStr,
	})
	if err != nil {
		return fmt.Errorf("can't set service [%s/%s] unidle-at annotation to [%s]: %w", svc.Namespace, svc.Name, nowStr, err)
	}

	return nil
}

func (uac *unidledAtController) removeUnidleAtAnnotation(svc *kapi.Service) error {

	if svc.Annotations == nil {
		return nil
	}

	_, annotationPresent := svc.Annotations[UnidledAtAnnotation]
	if !annotationPresent {
		klog.V(5).Infof("Service [%s/%s] does not have annotation %s, no need to remove it", svc.Namespace, svc.Name, UnidledAtAnnotation)
		return nil
	}

	err := uac.kube.SetAnnotationsOnService(svc.Namespace, svc.Name, map[string]interface{}{
		UnidledAtAnnotation: nil,
	})
	if err != nil {
		return fmt.Errorf("can't remove service unidle-at annotation: %w", err)
	}

	return nil
}

func getUnidleAt(svc *kapi.Service) (bool, string) {
	if svc == nil {
		return false, ""
	}

	if svc.Annotations == nil {
		return false, ""
	}

	for annotationKey, value := range svc.Annotations {
		if strings.HasSuffix(annotationKey, UnidledAtSuffix) {
			return true, value
		}
	}

	return false, ""
}
