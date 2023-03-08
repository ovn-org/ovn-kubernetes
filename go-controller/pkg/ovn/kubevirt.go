package ovn

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	kapi "k8s.io/api/core/v1"

	kvv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kubevirt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func (oc *DefaultNetworkController) ensureDHCPOptionsForVM(pod *corev1.Pod, lsp *nbdb.LogicalSwitchPort) error {
	if !kubevirt.PodIsLiveMigratable(pod) {
		return nil
	}

	switchNames, err := oc.getSwitchNames(pod)
	if err != nil {
		return err
	}
	var switchSubnets []*net.IPNet
	if switchSubnets = oc.lsManager.GetSwitchSubnets(switchNames.Original); switchSubnets == nil {
		return fmt.Errorf("subnet not found for switch %s to configuare DHCP at lsp %s", switchNames.Original, lsp.Name)
	}
	// Fake router to delegate on proxy arp mechanism
	vmName, ok := pod.Labels[kvv1.VirtualMachineNameLabel]
	if !ok {
		return fmt.Errorf("missing %s label at pod %s/%s when configuaring DHCP", kvv1.VirtualMachineNameLabel, pod.Namespace, pod.Name)
	}
	dhcpConfig, err := kubevirt.ComposeDHCPConfig(oc.watchFactory, vmName, switchSubnets)
	if err != nil {
		return fmt.Errorf("failed composing DHCP options: %v", err)
	}
	var (
		dhcpv4OptionsPredicate, dhcpv6OptionsPredicate func(*nbdb.DHCPOptions) bool
	)
	if dhcpConfig.V4Options != nil {
		dhcpv4OptionsDbObjectID := libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, oc.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: dhcpConfig.V4Options.Cidr,
				libovsdbops.HostnameIndex: vmName,
			})
		dhcpConfig.V4Options.ExternalIDs = dhcpv4OptionsDbObjectID.GetExternalIDs()
		dhcpConfig.V4Options.ExternalIDs[kvv1.VirtualMachineNameLabel] = vmName
		dhcpConfig.V4Options.ExternalIDs[kubevirt.NamespaceExternalIDKey] = pod.Namespace
		dhcpv4OptionsPredicate = libovsdbops.GetPredicate[*nbdb.DHCPOptions](dhcpv4OptionsDbObjectID, nil)
	}
	if dhcpConfig.V6Options != nil {
		dhcpv6OptionsDbObjectID := libovsdbops.NewDbObjectIDs(libovsdbops.VirtualMachineDHCPOptions, oc.controllerName,
			map[libovsdbops.ExternalIDKey]string{
				libovsdbops.ObjectNameKey: dhcpConfig.V6Options.Cidr,
				libovsdbops.HostnameIndex: vmName,
			})
		dhcpConfig.V6Options.ExternalIDs = dhcpv6OptionsDbObjectID.GetExternalIDs()
		dhcpConfig.V6Options.ExternalIDs[kvv1.VirtualMachineNameLabel] = vmName
		dhcpConfig.V6Options.ExternalIDs[kubevirt.NamespaceExternalIDKey] = pod.Namespace
		dhcpv6OptionsPredicate = libovsdbops.GetPredicate[*nbdb.DHCPOptions](dhcpv6OptionsDbObjectID, nil)
	}
	err = libovsdbops.CreateOrUpdateDhcpOptions(oc.nbClient, lsp, dhcpConfig.V4Options, dhcpConfig.V6Options, dhcpv4OptionsPredicate, dhcpv6OptionsPredicate)
	if err != nil {
		return fmt.Errorf("failed creation or updating OVN operations to add DHCP options: %v", err)
	}
	return nil
}

func (oc *DefaultNetworkController) deleteDHCPOptions(pod *kapi.Pod) error {
	predicate := func(item *nbdb.DHCPOptions) bool {
		return kubevirt.PodMatchesExternalIDs(pod, item.ExternalIDs)
	}
	return libovsdbops.DeleteDHCPOptionsWithPredicate(oc.nbClient, predicate)
}

func (oc *DefaultNetworkController) kubevirtCleanUp(pod *corev1.Pod) error {
	if kubevirt.PodIsLiveMigratable(pod) {
		isLiveMigrationLefover, err := kubevirt.PodIsLiveMigrationLeftOver(oc.watchFactory, pod)
		if err != nil {
			return err
		}

		if !isLiveMigrationLefover {
			if err := oc.deleteDHCPOptions(pod); err != nil {
				return err
			}
		}
	}
	return nil
}
