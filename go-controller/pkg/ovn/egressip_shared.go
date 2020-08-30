package ovn

import (
	"fmt"
	"net"
	"strings"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	utilnet "k8s.io/utils/net"
)

type egressIPShared struct {
	egressIPMode
}

func (e *egressIPShared) addPodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	podIPs := e.getPodIPs(pod)
	if podIPs == nil {
		e.podRetry.Store(getPodKey(pod), true)
		return nil
	}
	if e.needsRetry(pod) {
		e.podRetry.Delete(getPodKey(pod))
	}
	for _, status := range eIP.Status.Items {
		if err := e.createEgressPolicy(podIPs, status, 0, eIP.Name); err != nil {
			return fmt.Errorf("unable to create logical router policy for status: %v, err: %v", status, err)
		}
		if err := createNATRule(podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to create NAT rule for status: %v, err: %v", status, err)
		}
	}
	return nil
}

func (e *egressIPShared) deletePodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	podIPs := e.getPodIPs(pod)
	if podIPs == nil {
		return nil
	}
	for _, status := range eIP.Status.Items {
		if err := e.deleteEgressPolicy(podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to delete logical router policy for status: %v, err: %v", status, err)
		}
		if err := deleteNATRule(podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
		}
	}
	return nil
}

func createNATRule(podIPs []net.IP, status egressipv1.EgressIPStatusItem, egressIPName string) error {
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP)) {
			natIDs, err := findNatIDs(egressIPName, podIP.String(), status.EgressIP)
			if err != nil {
				return err
			}
			if natIDs == nil {
				_, stderr, err := util.RunOVNNbctl(
					"--id=@nat",
					"create",
					"nat",
					"type=snat",
					fmt.Sprintf("logical_port=k8s-%s", status.Node),
					fmt.Sprintf("external_ip=%s", status.EgressIP),
					fmt.Sprintf("logical_ip=%s", podIP),
					fmt.Sprintf("external_ids:name=%s", egressIPName),
					"--",
					"add",
					"logical_router",
					fmt.Sprintf("GR_%s", status.Node),
					"nat",
					"@nat",
				)
				if err != nil {
					return fmt.Errorf("unable to create nat rule, stderr: %s, err: %v", stderr, err)
				}
			}
		}
	}
	return nil
}

func deleteNATRule(podIPs []net.IP, status egressipv1.EgressIPStatusItem, egressIPName string) error {
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP)) {
			natIDs, err := findNatIDs(egressIPName, podIP.String(), status.EgressIP)
			if err != nil {
				return err
			}
			for _, natID := range natIDs {
				_, stderr, err := util.RunOVNNbctl(
					"remove",
					"logical_router",
					fmt.Sprintf("GR_%s", status.Node),
					"nat",
					natID,
				)
				if err != nil {
					return fmt.Errorf("unable to remove nat from logical_router, stderr: %s, err: %v", stderr, err)
				}
			}
		}
	}
	return nil
}

func findNatIDs(egressIPName, podIP, egressIP string) ([]string, error) {
	natIDs, stderr, err := util.RunOVNNbctl(
		"--format=csv",
		"--data=bare",
		"--no-heading",
		"--columns=_uuid",
		"find",
		"nat",
		fmt.Sprintf("external_ids:name=%s", egressIPName),
		fmt.Sprintf("logical_ip=%s", podIP),
		fmt.Sprintf("external_ip=%s", egressIP),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to find nat ID, stderr: %s, err: %v", stderr, err)
	}
	if natIDs == "" {
		return nil, nil
	}
	return strings.Split(natIDs, "\n"), nil
}
