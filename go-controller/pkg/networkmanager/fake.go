// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkmanager

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type FakeNetworkController struct {
	util.NetInfo
}

func (fnc *FakeNetworkController) Start(_ context.Context) error {
	return nil
}

func (fnc *FakeNetworkController) Stop() {}

func (fnc *FakeNetworkController) Cleanup() error {
	return nil
}

func (fnc *FakeNetworkController) Reconcile(util.NetInfo) error {
	return nil
}

func (fnc *FakeNetworkController) HandleNetworkRefChange(_ string, _ bool) {}

type FakeControllerManager struct{}

func (fcm *FakeControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	return &FakeNetworkController{NetInfo: netInfo}, nil
}

func (fcm *FakeControllerManager) CleanupStaleNetworks(_ ...util.NetInfo) error {
	return nil
}

func (fcm *FakeControllerManager) GetDefaultNetworkController() ReconcilableNetworkController {
	return nil
}

func (fcm *FakeControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}

type FakeNetworkManager struct {
	sync.Mutex
	// namespace -> netInfo
	// if netInfo is nil, it represents a namespace which contains the required UDN label but with no valid network. It will return invalid network error.
	PrimaryNetworks map[string]util.NetInfo
	// nad key -> netInfo for non-primary lookups
	NADNetworks           map[string]util.NetInfo
	Reconcilers           []reconcilerRegistration
	NetworkRefReconcilers []networkRefReconcilerRegistration
	nextID                uint64
	nextNetworkRefID      uint64
	// UDNNamespaces are a list of namespaces that require UDN for primary network
	UDNNamespaces sets.Set[string]
	// NotFoundNamespaces are namespaces absent from the informer cache for GetActiveNetworkForNamespace.
	NotFoundNamespaces sets.Set[string]
	// ActiveNodes tracks node activity per network for Dynamic UDN tests.
	ActiveNodes map[string]map[string]bool
	// StartFunc overrides Start when set. Used by tests that need custom
	// start behavior (errors, blocking NAD/UDN reconcile simulation).
	StartFunc func() error
	started   bool
}

func (fnm *FakeNetworkManager) RegisterNADReconciler(r NADReconciler) uint64 {
	fnm.Lock()
	defer fnm.Unlock()
	fnm.nextID++
	id := fnm.nextID
	fnm.Reconcilers = append(fnm.Reconcilers, reconcilerRegistration{id: id, r: r})
	return id
}

func (fnm *FakeNetworkManager) DeRegisterNADReconciler(id uint64) {
	fnm.Lock()
	defer fnm.Unlock()
	for i, rec := range fnm.Reconcilers {
		if rec.id == id {
			fnm.Reconcilers = append(fnm.Reconcilers[:i], fnm.Reconcilers[i+1:]...)
			return
		}
	}
}

func (fnm *FakeNetworkManager) RegisterNetworkRefReconciler(r NetworkRefReconciler) uint64 {
	fnm.Lock()
	defer fnm.Unlock()
	fnm.nextNetworkRefID++
	id := fnm.nextNetworkRefID
	fnm.NetworkRefReconcilers = append(fnm.NetworkRefReconcilers, networkRefReconcilerRegistration{id: id, r: r})
	return id
}

func (fnm *FakeNetworkManager) DeRegisterNetworkRefReconciler(id uint64) {
	fnm.Lock()
	defer fnm.Unlock()
	for i, rec := range fnm.NetworkRefReconcilers {
		if rec.id == id {
			fnm.NetworkRefReconcilers = append(fnm.NetworkRefReconcilers[:i], fnm.NetworkRefReconcilers[i+1:]...)
			return
		}
	}
}

func (fnm *FakeNetworkManager) TriggerHandlers(nadName string, info util.NetInfo, removed bool) {
	fnm.Lock()
	defer fnm.Unlock()
	_ = info
	_ = removed
	for _, entry := range fnm.Reconcilers {
		entry.r.Reconcile(nadName)
	}
}

func (fnm *FakeNetworkManager) Interface() Interface {
	return fnm
}

func (fnm *FakeNetworkManager) Start() error {
	var err error
	if fnm.StartFunc != nil {
		err = fnm.StartFunc()
	}
	fnm.Lock()
	fnm.started = true
	fnm.Unlock()
	return err
}

// Started reports whether Start has been invoked and returned.
func (fnm *FakeNetworkManager) Started() bool {
	fnm.Lock()
	defer fnm.Unlock()
	return fnm.started
}

func (fnm *FakeNetworkManager) Stop() {}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	fnm.Lock()
	if fnm.NotFoundNamespaces != nil && fnm.NotFoundNamespaces.Has(namespace) {
		fnm.Unlock()
		return nil, fmt.Errorf("failed to fetch namespace %q: %w", namespace,
			apierrors.NewNotFound(corev1.Resource("namespaces"), namespace))
	}
	fnm.Unlock()
	network := fnm.GetActiveNetworkForNamespaceFast(namespace)
	if network == nil {
		return nil, util.NewInvalidPrimaryNetworkError(namespace)
	}
	return network, nil
}

func (fnm *FakeNetworkManager) GetPrimaryNADForNamespace(namespace string) (string, error) {
	fnm.Lock()
	defer fnm.Unlock()
	if fnm.NotFoundNamespaces != nil && fnm.NotFoundNamespaces.Has(namespace) {
		return "", fmt.Errorf("failed to fetch namespace %q: %w", namespace,
			apierrors.NewNotFound(corev1.Resource("namespaces"), namespace))
	}
	if primaryNetwork, ok := fnm.PrimaryNetworks[namespace]; ok {
		if primaryNetwork == nil {
			return "", util.NewInvalidPrimaryNetworkError(namespace)
		}
		var matches []string
		for nadKey, netInfo := range fnm.NADNetworks {
			if netInfo == nil || !netInfo.IsPrimaryNetwork() {
				continue
			}
			nadNamespace, _, err := cache.SplitMetaNamespaceKey(nadKey)
			if err != nil {
				continue
			}
			if nadNamespace == namespace {
				matches = append(matches, nadKey)
			}
		}
		if len(matches) == 0 {
			return "", util.NewInvalidPrimaryNetworkError(namespace)
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("multiple primary NADs found for namespace %q", namespace)
		}
		return matches[0], nil
	}
	return types.DefaultNetworkName, nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	if primaryNetworks, ok := fnm.PrimaryNetworks[namespace]; ok {
		return primaryNetworks
	}
	if fnm.UDNNamespaces != nil && fnm.UDNNamespaces.Has(namespace) {
		return nil
	}
	return &util.DefaultNetInfo{}
}

func (fnm *FakeNetworkManager) GetNetwork(networkName string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	for _, ni := range fnm.PrimaryNetworks {
		if ni.GetNetworkName() == networkName {
			return ni
		}
	}
	return &util.DefaultNetInfo{}
}

func (fnm *FakeNetworkManager) GetActiveNetwork(networkName string) util.NetInfo {
	return fnm.GetNetwork(networkName)
}

func (fnm *FakeNetworkManager) GetNetInfoForNADKey(nadKey string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	if netInfo, ok := fnm.NADNetworks[nadKey]; ok {
		return netInfo
	}
	return nil
}

func (fnm *FakeNetworkManager) GetNetworkNameForNADKey(nadKey string) string {
	fnm.Lock()
	defer fnm.Unlock()
	if netInfo, ok := fnm.NADNetworks[nadKey]; ok {
		return netInfo.GetNetworkName()
	}
	return ""
}

func (fnm *FakeNetworkManager) GetNADKeysForNetwork(networkName string) []string {
	fnm.Lock()
	defer fnm.Unlock()
	nadKeys := sets.New[string]()
	for nadKey, netInfo := range fnm.NADNetworks {
		if netInfo != nil && netInfo.GetNetworkName() == networkName {
			nadKeys.Insert(nadKey)
		}
	}
	return nadKeys.UnsortedList()
}

func (fnm *FakeNetworkManager) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	fnm.Lock()
	defer fnm.Unlock()
	namespaces := make([]string, 0)
	for namespaceName, primaryNAD := range fnm.PrimaryNetworks {
		if primaryNAD == nil || primaryNAD.GetNetworkName() != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

func (fnm *FakeNetworkManager) DoWithLock(f func(network util.NetInfo) error) error {
	fnm.Lock()
	networks := make([]util.NetInfo, 0, len(fnm.PrimaryNetworks))
	for _, ni := range fnm.PrimaryNetworks {
		networks = append(networks, ni)
	}
	fnm.Unlock()

	var errs []error
	for _, ni := range networks {
		if err := f(ni); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (fnm *FakeNetworkManager) GetNetworkByID(id int) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	for _, ni := range fnm.PrimaryNetworks {
		if ni.GetNetworkID() == id {
			return ni
		}
	}
	for _, ni := range fnm.NADNetworks {
		if ni != nil && ni.GetNetworkID() == id {
			return ni
		}
	}
	return nil
}

func (fnm *FakeNetworkManager) NodeHasNetwork(node, networkName string) bool {
	if !config.OVNKubernetesFeature.EnableDynamicUDNAllocation {
		return true
	}
	if networkName == types.DefaultNetworkName {
		return true
	}
	fnm.Lock()
	defer fnm.Unlock()
	if fnm.ActiveNodes == nil {
		return false
	}
	nodes := fnm.ActiveNodes[networkName]
	if nodes == nil {
		return false
	}
	return nodes[node]
}

// SetNodeActive marks a node as active/inactive for a specific network in Dynamic UDN tests.
func (fnm *FakeNetworkManager) SetNodeActive(networkName, nodeName string, active bool) {
	fnm.Lock()
	defer fnm.Unlock()
	if fnm.ActiveNodes == nil {
		fnm.ActiveNodes = map[string]map[string]bool{}
	}
	if fnm.ActiveNodes[networkName] == nil {
		fnm.ActiveNodes[networkName] = map[string]bool{}
	}
	fnm.ActiveNodes[networkName][nodeName] = active
}
