package unidling

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	kapi "k8s.io/api/core/v1"
)

type emptyLBBackendEvent struct {
	vip             string
	protocol        kapi.Protocol
	controllerEvent *sbdb.ControllerEvent
}

func extractEmptyLBBackendsEvents(events []sbdb.ControllerEvent) ([]emptyLBBackendEvent, error) {
	emptyLBEvents := make([]emptyLBBackendEvent, 0, 4)

	for _, event := range events {
		if event.EventType != "empty_lb_backends" {
			continue
		}

		var vip string
		var protocol kapi.Protocol
		for k, v := range event.EventInfo {
			switch k {
			case "vip":
				vip = v
			case "protocol":
				prot := v
				if prot == "udp" {
					protocol = kapi.ProtocolUDP
				} else if prot == "sctp" {
					protocol = kapi.ProtocolSCTP
				} else {
					protocol = kapi.ProtocolTCP
				}
			}
		}
		emptyLBEvents = append(emptyLBEvents, emptyLBBackendEvent{vip, protocol, &event})

	}
	return emptyLBEvents, nil
}
