package ovn

import (
	"fmt"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
)

type egressIPLocal struct {
	egressIPMode
}

func (e *egressIPLocal) addPodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	podIPs := e.getPodIPs(pod)
	if podIPs == nil {
		e.podRetry.Store(getPodKey(pod), true)
		return nil
	}
	if e.needsRetry(pod) {
		e.podRetry.Delete(getPodKey(pod))
	}
	for _, status := range eIP.Status.Items {
		mark := util.IPToUint32(status.EgressIP)
		if err := e.createEgressPolicy(podIPs, status, mark, eIP.Name); err != nil {
			return fmt.Errorf("unable to create logical router policy for status: %v, err: %v", status, err)
		}
	}
	return nil
}

func (e *egressIPLocal) deletePodEgressIP(eIP *egressipv1.EgressIP, pod *kapi.Pod) error {
	podIPs := e.getPodIPs(pod)
	if podIPs == nil {
		return nil
	}
	for _, status := range eIP.Status.Items {
		if err := e.deleteEgressPolicy(podIPs, status, eIP.Name); err != nil {
			return fmt.Errorf("unable to delete logical router policy for status: %v, err: %v", status, err)
		}
	}
	return nil
}
