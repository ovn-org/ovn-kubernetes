package nat

import (
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
	utilsnet "k8s.io/utils/net"
)

type NATSyncer struct {
	nbClient       libovsdbclient.Client
	controllerName string
}

type podNetInfo struct {
	ip        []net.IP
	namespace string
	name      string
}

type podsNetInfo []podNetInfo

const legacyEIPNameExtIDKey = "name"

// getPodByIP attempts to find a reference to a pod by IP address. It will return the info if found,
// along with boolean true, otherwise returned boolean will be false.
func (p podsNetInfo) getPodByIP(ip net.IP) (podNetInfo, bool) {
	for _, pod := range p {
		for _, podIP := range pod.ip {
			if podIP.Equal(ip) {
				return pod, true
			}
		}
	}
	return podNetInfo{}, false
}

type egressIPFamilyValue string

var (
	ipFamilyValueV4 egressIPFamilyValue = "ip4"
	ipFamilyValueV6 egressIPFamilyValue = "ip6"
)

// NewNATSyncer adds owner references to a limited subnet of LRPs. controllerName is the name of the new controller that should own all LRPs without controller
func NewNATSyncer(nbClient libovsdbclient.Client, controllerName string) *NATSyncer {
	return &NATSyncer{
		nbClient:       nbClient,
		controllerName: controllerName,
	}
}

func (n *NATSyncer) Sync() error {
	if err := n.syncEgressIPNATs(); err != nil {
		return fmt.Errorf("failed to sync EgressIP NATs: %v", err)
	}
	return nil
}

func (n *NATSyncer) syncEgressIPNATs() error {
	v4PodCache, v6PodCache, err := n.buildPodCache()
	if err != nil {
		return fmt.Errorf("failed to build pod cache: %v", err)
	}
	noOwnerFn := libovsdbops.GetNoOwnerPredicate[*nbdb.NAT]()
	p := func(item *nbdb.NAT) bool {
		return noOwnerFn(item) && item.ExternalIDs[legacyEIPNameExtIDKey] != "" && item.Type == nbdb.NATTypeSNAT && item.LogicalIP != ""
	}
	nats, err := libovsdbops.FindNATsWithPredicate(n.nbClient, p)
	if err != nil {
		return fmt.Errorf("failed to retrieve OVN NATs: %v", err)
	}
	var ops []libovsdb.Operation
	for _, nat := range nats {
		eIPName := nat.ExternalIDs[legacyEIPNameExtIDKey]
		if eIPName == "" {
			klog.Errorf("Expected NAT %s to contain 'name' as a key within its external IDs", nat.UUID)
			continue
		}
		podIP, _, err := net.ParseCIDR(nat.LogicalIP)
		if err != nil {
			klog.Errorf("Failed to process logical IP %q of NAT %s", nat.LogicalIP, nat.UUID)
			continue
		}
		isV6 := utilsnet.IsIPv6(podIP)
		var ipFamily egressIPFamilyValue
		var pod podNetInfo
		var found bool
		if isV6 {
			ipFamily = ipFamilyValueV6
			pod, found = v6PodCache.getPodByIP(podIP)
		} else {
			ipFamily = ipFamilyValueV4
			pod, found = v4PodCache.getPodByIP(podIP)
		}
		if !found {
			klog.Errorf("Failed to find logical switch port that contains IP address %s", podIP.String())
			continue
		}
		nat.ExternalIDs = getEgressIPNATDbIDs(eIPName, pod.namespace, pod.name, ipFamily, n.controllerName).GetExternalIDs()
		ops, err = libovsdbops.UpdateNATOps(n.nbClient, ops, nat)
		if err != nil {
			klog.Errorf("Failed to generate NAT ops for NAT %s: %v", nat.UUID, err)
		}
		klog.Infof("## martin found %d nats", len(ops))
	}

	_, err = libovsdbops.TransactAndCheck(n.nbClient, ops)
	if err != nil {
		return fmt.Errorf("failed to transact pod to node subnet sync ops: %v", err)
	}
	return nil
}

func (n *NATSyncer) buildPodCache() (podsNetInfo, podsNetInfo, error) {
	p := func(item *nbdb.LogicalSwitchPort) bool {
		return item.ExternalIDs["pod"] == "true" && item.ExternalIDs[ovntypes.NADExternalID] == "" // ignore secondary network LSPs
	}
	lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(n.nbClient, p)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get logical switch ports: %v", err)
	}
	v4Pods, v6Pods := make(podsNetInfo, 0), make(podsNetInfo, 0)
	for _, lsp := range lsps {
		namespaceName, podName := util.GetNamespacePodFromCDNPortName(lsp.Name)
		if namespaceName == "" || podName == "" {
			klog.Errorf("Failed to extract namespace / pod from logical switch port %s", lsp.Name)
			continue
		}
		if len(lsp.Addresses) == 0 {
			klog.Errorf("Address(es) not set for pod %s/%s", namespaceName, podName)
			continue
		}
		var (
			v4IPs []net.IP
			v6IPs []net.IP
		)
		for i := 1; i < len(lsp.Addresses); i++ {
			// CIDR is supported field within OVN, but for CDN we only set IP
			ip := net.ParseIP(lsp.Addresses[i])
			if ip == nil {
				klog.Errorf("Failed to extract IP %q from logical switch port for pod %s/%s", lsp.Addresses[i], namespaceName, podName)
				continue
			}

			if utilsnet.IsIPv6(ip) {
				v6IPs = append(v6IPs, ip)
			} else {
				v4IPs = append(v4IPs, ip)
			}
		}
		if len(v4IPs) > 0 {
			v4Pods = append(v4Pods, podNetInfo{ip: v4IPs, namespace: namespaceName, name: podName})
		}
		if len(v6IPs) > 0 {
			v6Pods = append(v6Pods, podNetInfo{ip: v6IPs, namespace: namespaceName, name: podName})
		}
	}

	return v4Pods, v6Pods, nil
}

// getEgressIPNATDbIDs is copied from ovn pkg to avoid dependency
func getEgressIPNATDbIDs(eIPName, podNamespace, podName string, ipFamily egressIPFamilyValue, controller string) *libovsdbops.DbObjectIDs {
	return libovsdbops.NewDbObjectIDs(libovsdbops.NATEgressIP, controller, map[libovsdbops.ExternalIDKey]string{
		libovsdbops.ObjectNameKey: fmt.Sprintf("%s_%s/%s", eIPName, podNamespace, podName),
		libovsdbops.IPFamilyKey:   string(ipFamily),
	})
}
