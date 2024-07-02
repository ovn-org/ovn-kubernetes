package userdefinednetwork

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	userdefinednetworklister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/listers/userdefinednetwork/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
)

type UserDefineNetworkController struct {
	syncLock   sync.Mutex
	controller controller.Reconciler
	lister     userdefinednetworklister.UserDefinedNetworkLister
	nadClient  netv1clientset.Interface
}

func NewUserDefineNetworkController(
	nadClient netv1clientset.Interface,
	lister userdefinednetworklister.UserDefinedNetworkLister,
) *UserDefineNetworkController {
	const reconcilerName = "userdefinednetwork"
	udnController := &UserDefineNetworkController{
		nadClient: nadClient,
		lister:    lister,
	}
	cfg := controller.ReconcilerConfig{
		RateLimiter: workqueue.DefaultControllerRateLimiter(),
		Reconcile:   udnController.sync,
		Threadiness: 1,
	}
	udnController.controller = controller.NewReconciler(reconcilerName, &cfg)

	return udnController
}

func (c *UserDefineNetworkController) Run() error {
	klog.Infof("Starting User Defined Network Controller")

	if err := controller.Start(c.controller); err != nil {
		return fmt.Errorf("unable to start UserDefinedNetwork controller: %v", err)
	}
	return nil
}

func (c *UserDefineNetworkController) Shutdown() {
	controller.Stop(c.controller)
}

func (c *UserDefineNetworkController) sync(key string) error {
	c.syncLock.Lock()
	defer c.syncLock.Unlock()

	klog.Infof("DEBUG: sync start, got key %q", key)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("failed to split meta namespace cache key %q for user-define-network: %v", key, err)
		return nil
	}

	udn, err := c.lister.UserDefinedNetworks(namespace).Get(name)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to get udn from cache: %v", err)
		}
	}

	nad, err := generateNAD(udn)
	if err == nil {
		return fmt.Errorf("failed to generate NAD: %v", err)
	}

	_, err = c.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, metav1.CreateOptions{})
	if kerrors.IsAlreadyExists(err) {
		klog.Error("nad already exist..noop")
	}
	if err != nil {
		return fmt.Errorf("failed to create nad: %v", err)
	}

	return nil
}

func generateNAD(udn *v1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
	cniNetConf, err := newNetworkCNIConfig(udn)
	if err != nil {
		return nil, err
	}
	cniNetConfRaw, err := json.Marshal(cniNetConf)
	if err != nil {
		return nil, err
	}

	const (
		netAttachDefKind   = "NetworkAttachmentDefinition"
		netAttachDefAPIVer = "v1"

		userDefinedNetworkFinalizer = "udn.finalizer.k8s.ovn.org"
	)

	blockOwnerDeletion := true
	return &netv1.NetworkAttachmentDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: netAttachDefAPIVer,
			Kind:       netAttachDefKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       udn.Name,
			Namespace:  udn.Namespace,
			Finalizers: []string{userDefinedNetworkFinalizer},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         udn.APIVersion,
					Kind:               udn.Kind,
					Name:               udn.Name,
					UID:                udn.UID,
					BlockOwnerDeletion: &blockOwnerDeletion,
				},
			},
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{
			Config: string(cniNetConfRaw),
		},
	}, nil
}

func newNetworkCNIConfig(udn *v1.UserDefinedNetwork) (map[string]interface{}, error) {
	const (
		cniVersionKey       = "cniVersion"
		cniVersion          = "1.0.0"
		topologyKey         = "topology"
		topologyLayer2      = "layer2"
		typeKey             = "type"
		ovnK8sCniOverlay    = "ovn-k8s-cni-overlay"
		nameKey             = "name"
		netAttachDefNameKey = "netAttachDefName"
	)
	cniNetConf := map[string]interface{}{
		cniVersionKey:       cniVersion,
		typeKey:             ovnK8sCniOverlay,
		nameKey:             udn.Namespace + "-" + udn.Name,
		netAttachDefNameKey: udn.Namespace + "/" + udn.Name,
		topologyKey:         topologyLayer2,
	}
	return cniNetConf, nil
}
