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
			// TODO: when Bug https://bugzilla.redhat.com/show_bug.cgi?id=1861294 is cleared out: change to snat,
			// we should not really be DNAT-ing here as that would allow an external client direct access to the egress pods.
			// EgressIP should only be applied for SNAT-ing traffic.
			// GOTCHA 1: this in turn means that we only support one pod per egress IP...because you oviously can't DNAT on two separate logical IPs
			_, stderr, err := util.RunOVNNbctl("lr-nat-add", fmt.Sprintf("GR_%s", status.Node), "dnat_and_snat", status.EgressIP, podIP.String())
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
			// TODO: when Bug https://bugzilla.redhat.com/show_bug.cgi?id=1861294 is cleared out: change to snat,
			// we should not really be DNAT-ing here as that would allow an external client direct access to the egress pods.
			// EgressIP should only be applied for SNAT-ing traffic.
			// GOTCHA 2: whenever this bugs is fixed, be careful and change to:
			// util.RunOVNNbctl("lr-nat-del", fmt.Sprintf("GR_%s", status.Node), "dnat_and_snat", podIP.String()) as the ovn-nbctl API is different for snat
			_, stderr, err := util.RunOVNNbctl("lr-nat-del", fmt.Sprintf("GR_%s", status.Node), "dnat_and_snat", status.EgressIP)
			if err != nil {
				return fmt.Errorf("OVN transaction error, stderr: %s, err: %v", stderr, err)
			}
		}
	}
	return nil
}
