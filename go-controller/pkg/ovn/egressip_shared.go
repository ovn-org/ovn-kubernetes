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

const (
	// If we restart we need accept executing ovn-nbctl commands with this error.
	natAlreadyExistsMsg = "a NAT with this external_ip and logical_ip already exists"
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
		if err := e.createEgressPolicy(podIPs, status, 0); err != nil {
			return fmt.Errorf("unable to create logical router policy for status: %v, err: %v", status, err)
		}
		if err := createNATRule(podIPs, status); err != nil {
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
		if err := e.deleteEgressPolicy(podIPs, status); err != nil {
			return fmt.Errorf("unable to delete logical router policy for status: %v, err: %v", status, err)
		}
		if err := deleteNATRule(podIPs, status); err != nil {
			return fmt.Errorf("unable to delete NAT rule for status: %v, err: %v", status, err)
		}
	}
	return nil
}

func createNATRule(podIPs []net.IP, status egressipv1.EgressIPStatusItem) error {
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP)) {
			_, stderr, err := util.RunOVNNbctl("lr-nat-add", fmt.Sprintf("GR_%s", status.Node), "snat", status.EgressIP, podIP.String())
			if err != nil && !strings.Contains(stderr, natAlreadyExistsMsg) {
				return fmt.Errorf("OVN transaction error, stderr: %s, err: %v", stderr, err)
			}
		}
	}
	return nil
}

func deleteNATRule(podIPs []net.IP, status egressipv1.EgressIPStatusItem) error {
	for _, podIP := range podIPs {
		if (utilnet.IsIPv6String(status.EgressIP) && utilnet.IsIPv6(podIP)) || (!utilnet.IsIPv6String(status.EgressIP) && !utilnet.IsIPv6(podIP)) {
			_, stderr, err := util.RunOVNNbctl("lr-nat-del", fmt.Sprintf("GR_%s", status.Node), "snat", podIP.String())
			if err != nil {
				return fmt.Errorf("OVN transaction error, stderr: %s, err: %v", stderr, err)
			}
		}
	}
	return nil
}
