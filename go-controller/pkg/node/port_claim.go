package node

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
)

type handler func(port int32, protocol kapi.Protocol, svc *kapi.Service) error

type localPort interface {
	open(port int32, protocol kapi.Protocol, svc *kapi.Service) error
	close(port int32, protocol kapi.Protocol, svc *kapi.Service) error
}

var port localPort

type activeSocket interface {
	Close() error
}

type portClaimWatcher struct {
	recorder          record.EventRecorder
	activeSocketsLock sync.Mutex
	activeSockets     map[kapi.Protocol]map[int32]activeSocket
}

func newPortClaimWatcher(recorder record.EventRecorder) localPort {
	return &portClaimWatcher{
		recorder:          recorder,
		activeSocketsLock: sync.Mutex{},
		activeSockets:     make(map[kapi.Protocol]map[int32]activeSocket),
	}
}

func initPortClaimWatcher(recorder record.EventRecorder, wf factory.NodeWatchFactory) {
	port = newPortClaimWatcher(recorder)
	wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if errors := addServicePortClaim(svc); len(errors) > 0 {
				for _, err := range errors {
					klog.Errorf("Error claiming port for service: %s/%s: %v", svc.Namespace, svc.Name, err)
				}
			}
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			if errors := updateServicePortClaim(oldSvc, newSvc); len(errors) > 0 {
				for _, err := range errors {
					klog.Errorf("Error updating port claim for service: %s/%s: %v", oldSvc.Namespace, oldSvc.Name, err)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			if errors := deleteServicePortClaim(svc); len(errors) > 0 {
				for _, err := range errors {
					klog.Errorf("Error removing port claim for service: %s/%s: %v", svc.Namespace, svc.Name, err)
				}
			}
		},
	}, nil)
}

func addServicePortClaim(svc *kapi.Service) []error {
	return handleService(svc, port.open)
}

func deleteServicePortClaim(svc *kapi.Service) []error {
	return handleService(svc, port.close)
}

func handleService(svc *kapi.Service, handler handler) []error {
	errors := []error{}
	if !util.ServiceTypeHasNodePort(svc) && len(svc.Spec.ExternalIPs) == 0 {
		return errors
	}
	for _, svcPort := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			if err := handlePort(svc, svcPort.NodePort, svcPort.Protocol, handler); err != nil {
				errors = append(errors, err)
			}
		}
		if len(svc.Spec.ExternalIPs) > 0 {
			if err := handlePort(svc, svcPort.Port, svcPort.Protocol, handler); err != nil {
				errors = append(errors, err)
			}
		}
	}
	return errors
}

func handlePort(svc *kapi.Service, port int32, protocol kapi.Protocol, handler handler) error {
	if err := util.ValidatePort(protocol, port); err != nil {
		return fmt.Errorf("invalid service port %s, err: %v", svc.Name, err)
	}
	if err := handler(port, protocol, svc); err != nil {
		return err
	}
	return nil
}

func updateServicePortClaim(oldSvc, newSvc *kapi.Service) []error {
	if reflect.DeepEqual(oldSvc.Spec.ExternalIPs, newSvc.Spec.ExternalIPs) && reflect.DeepEqual(oldSvc.Spec.Ports, newSvc.Spec.Ports) {
		return nil
	}
	errors := []error{}
	errors = append(errors, deleteServicePortClaim(oldSvc)...)
	errors = append(errors, addServicePortClaim(newSvc)...)
	return errors
}

func (p *portClaimWatcher) open(port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	klog.V(5).Infof("Opening socket for service: %s/%s and port: %v", svc.Namespace, svc.Name, port)
	var socket activeSocket
	var socketError error
	switch protocol {
	case kapi.ProtocolTCP:
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			socketError = err
			break
		}
		socket = listener
	case kapi.ProtocolUDP:
		addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			socketError = err
			break
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			socketError = err
			break
		}
		socket = conn
	case kapi.ProtocolSCTP:
		// Do not open ports for SCTP, ref: https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/0015-20180614-SCTP-support.md#the-solution-in-the-kubernetes-sctp-support-implementation
		return nil
	default:
		socketError = fmt.Errorf("unknown protocol %q", protocol)
	}
	if socketError != nil {
		p.emitPortClaimEvent(svc, port, socketError)
		return socketError
	}
	p.activeSocketsLock.Lock()
	defer p.activeSocketsLock.Unlock()
	if _, exists := p.activeSockets[protocol]; exists {
		p.activeSockets[protocol][port] = socket
	} else {
		p.activeSockets[protocol] = map[int32]activeSocket{
			port: socket,
		}
	}
	return nil
}

func (p *portClaimWatcher) close(port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	p.activeSocketsLock.Lock()
	defer p.activeSocketsLock.Unlock()
	klog.V(5).Infof("Closing socket claimed for service: %s/%s and port: %v", svc.Namespace, svc.Name, port)
	if socket, exists := p.activeSockets[protocol][port]; exists {
		if err := socket.Close(); err != nil {
			return fmt.Errorf("error closing socket for svc: %s/%s on port: %v, err: %v", svc.Namespace, svc.Name, port, err)
		}
		delete(p.activeSockets[protocol], port)
		return nil
	}
	return fmt.Errorf("error closing socket for svc: %s/%s on port: %v, port was never opened...?", svc.Namespace, svc.Name, port)
}

func (p *portClaimWatcher) emitPortClaimEvent(svc *kapi.Service, port int32, err error) {
	serviceRef := kapi.ObjectReference{
		Kind:      "Service",
		Namespace: svc.Namespace,
		Name:      svc.Name,
	}
	p.recorder.Eventf(&serviceRef, kapi.EventTypeWarning,
		"PortClaim", "Service: %s/%s requires port: %v to be opened on node, but port cannot be opened, err: %v", svc.Namespace, svc.Name, port, err)
	klog.Warningf("PortClaim for svc: %s/%s on port: %v, err: %v", svc.Namespace, svc.Name, port, err)
}
