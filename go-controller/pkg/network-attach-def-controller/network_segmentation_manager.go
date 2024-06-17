package networkAttachDefController

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ NetAttachDefinitionManager = &NetworkSegmentationManager{}

type NetworkSegmentationManager struct {
	kube         kubernetes.Interface
	watchFactory *factory.WatchFactory
	nadClient    nadclientset.Interface
}

func NewNetworkSegmentationManager(kube kubernetes.Interface, watchFactory *factory.WatchFactory, nadClient nadclientset.Interface) *NetworkSegmentationManager {
	return &NetworkSegmentationManager{
		kube:         kube,
		watchFactory: watchFactory,
		nadClient:    nadClient,
	}
}

func (m *NetworkSegmentationManager) OnAddNetAttachDef(nad *nettypes.NetworkAttachmentDefinition, network util.NetInfo) error {
	return m.ensureNamespaceActiveNetwork(nad.Namespace, network)
}

func (m *NetworkSegmentationManager) OnDelNetAttachDef(nadName, netName string) error {
	namespace, _, err := cache.SplitMetaNamespaceKey(nadName)
	if err != nil {
		return err
	}
	nadList, err := m.nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed listing network attachment definitions after nad '%s' deletion: %w", nadName, err)
	}

	if len(nadList.Items) == 0 {
		pods, err := m.watchFactory.GetPods(namespace)
		if err != nil {
			return fmt.Errorf("failed ensuring namespace '%s' active network when listing pods: %w", namespace, err)
		}
		networkNamespace, err := m.watchFactory.GetNamespace(namespace)
		if err != nil {
			return fmt.Errorf("failed looking for network namespace '%s': %w", namespace, err)
		}
		if len(pods) == 0 {
			if err := util.UpdateNamespaceActiveNetwork(m.kube, networkNamespace, types.DefaultNetworkName); err != nil {
				return fmt.Errorf("failed annotating namespace with active-network=%s: %w", types.DefaultNetworkName, err)
			}
			return nil
		} else {
			currentActiveNetwork, ok := networkNamespace.Annotations[util.ActiveNetworkAnnotation]
			if !ok {
				return fmt.Errorf("missing active-network annotation at namespace %s", namespace)
			}
			if currentActiveNetwork != types.DefaultNetworkName {
				//TODO: Event
				klog.Warningf("Active primary network %s deleted from namespace '%s' with pods, marking namespace active network to unknown", currentActiveNetwork, networkNamespace.Name)
				if err := util.UpdateNamespaceActiveNetwork(m.kube, networkNamespace, types.UnknownNetworkName); err != nil {
					return fmt.Errorf("failed annotating namespace with active-network=unknown when a primary network was already configured: %w", err)
				}
				return nil
			}

		}
	}

	for _, nad := range nadList.Items {
		network, err := util.ParseNADInfo(&nad)
		if err != nil {
			return fmt.Errorf("failed parsing nads after nad '%s' deletion: %w", nadName, err)
		}
		if err := m.ensureNamespaceActiveNetwork(namespace, network); err != nil {
			return fmt.Errorf("failed ensuring namespace active network after nad '%s' deletion: %w", nadName, err)
		}
	}

	return nil
}

func (m *NetworkSegmentationManager) ensureNamespaceActiveNetwork(namespace string, network util.NetInfo) error {
	if m.watchFactory == nil || m.kube == nil {
		return nil
	}
	if !network.IsPrimaryNetwork() {
		return nil
	}

	networkNamespace, err := m.watchFactory.GetNamespace(namespace)
	if err != nil {
		return fmt.Errorf("failed looking for network namespace '%s': %w", namespace, err)
	}

	currentActiveNetwork, ok := networkNamespace.Annotations[util.ActiveNetworkAnnotation]
	if !ok {
		return fmt.Errorf("missing active-network annotation at namespace %s", namespace)
	}

	if currentActiveNetwork == network.GetNetworkName() {
		return nil
	}

	if currentActiveNetwork != types.DefaultNetworkName && currentActiveNetwork != types.UnknownNetworkName {
		//TODO: Event
		klog.Warningf("Active primary network %s already configured at namespace %s, marking namespace active network to unknown", currentActiveNetwork, networkNamespace.Name)
		if err := util.UpdateNamespaceActiveNetwork(m.kube, networkNamespace, types.UnknownNetworkName); err != nil {
			return fmt.Errorf("failed annotating namespace with active-network=unknown when a primary network was already configured: %w", err)
		}
		return nil
	}

	pods, err := m.watchFactory.GetPods(networkNamespace.Name)
	if err != nil {
		return fmt.Errorf("failed ensuring namespace '%s' active network when listing pods: %w", networkNamespace.Name, err)
	}

	// At this point all those pods exist before configuring the primary network,
	// so we should mark the namespace and send and event
	if len(pods) > 0 {
		//TODO: Event
		klog.Warningf("Pods present at namesapace %s before configuring primary network, marking namespace active network to unknown", networkNamespace.Name)
		if err := util.UpdateNamespaceActiveNetwork(m.kube, networkNamespace, types.UnknownNetworkName); err != nil {
			return fmt.Errorf("failed annotating namespace with active-network=unknown when namespace contains pods before configuring a primary network: %w", err)
		}
		return nil
	}

	if err := util.UpdateNamespaceActiveNetwork(m.kube, networkNamespace, network.GetNetworkName()); err != nil {
		return fmt.Errorf("failed annotating namespace with active-network=%s: %w", network.GetNetworkName(), err)
	}
	return nil
}
