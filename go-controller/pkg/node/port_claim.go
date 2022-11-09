package node

import (
	"fmt"
	"net"
	"reflect"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
)

// Constants for valid LocalHost descriptions:
const (
	nodePortDescr     = "nodePort for"
	externalPortDescr = "externalIP for"
)

type handler func(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error

type portManager interface {
	open(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error
	close(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error
}

type localPortManager struct {
	recorder          record.EventRecorder
	activeSocketsLock sync.Mutex
	localAddrSet      map[string]net.IPNet
	portsMap          map[utilnet.LocalPort]utilnet.Closeable
	portOpener        utilnet.PortOpener
}

func (p *localPortManager) open(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	klog.V(5).Infof("Opening socket for service: %s/%s, port: %v and protocol %s", svc.Namespace, svc.Name, port, protocol)

	if ip != "" {
		if _, exists := p.localAddrSet[ip]; !exists {
			klog.V(5).Infof("The IP %s is not one of the node local ports", ip)
			return nil
		}
	}
	var localPort *utilnet.LocalPort
	var portError error
	switch protocol {
	case kapi.ProtocolTCP, kapi.ProtocolUDP:
		localPort, portError = utilnet.NewLocalPort(desc, ip, "", int(port), utilnet.Protocol(protocol))
	case kapi.ProtocolSCTP:
		// Do not open ports for SCTP, ref: https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/0015-20180614-SCTP-support.md#the-solution-in-the-kubernetes-sctp-support-implementation
		return nil
	default:
		portError = fmt.Errorf("unknown protocol %q", protocol)
	}
	if portError != nil {
		p.emitPortClaimEvent(svc, port, portError)
		return portError
	}
	klog.V(5).Infof("Opening socket for LocalPort %v", localPort)
	p.activeSocketsLock.Lock()
	defer p.activeSocketsLock.Unlock()

	if _, exists := p.portsMap[*localPort]; exists {
		// If the port already exists in the map, we've already opened it.
		// Don't consider this as an error, since we've reached the desired state.
		klog.Infof("Svc %s/%s: port %v is already open, no action needed", svc.Namespace, svc.Name, port)
		return nil
	} else {
		closeable, err := p.portOpener.OpenLocalPort(localPort)
		if err != nil {
			p.emitPortClaimEvent(svc, port, err)
			return err
		}
		p.portsMap[*localPort] = closeable
	}
	return nil
}

func (p *localPortManager) close(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	klog.V(5).Infof("Closing socket claimed for service: %s/%s and port: %v", svc.Namespace, svc.Name, port)

	if protocol != kapi.ProtocolTCP && protocol != kapi.ProtocolUDP {
		return nil
	}
	if ip != "" {
		if _, exists := p.localAddrSet[ip]; !exists {
			klog.V(5).Infof("The IP %s is not one of the node local ports", ip)
			return nil
		}
	}
	localPort, err := utilnet.NewLocalPort(desc, ip, "", int(port), utilnet.Protocol(protocol))
	if err != nil {
		return fmt.Errorf("error localPort creation for svc: %s/%s on port: %v, err: %v", svc.Namespace, svc.Name, port, err)
	}
	klog.V(5).Infof("Closing socket for LocalPort %v", localPort)

	p.activeSocketsLock.Lock()
	defer p.activeSocketsLock.Unlock()

	if _, exists := p.portsMap[*localPort]; exists {
		if err = p.portsMap[*localPort].Close(); err != nil {
			return fmt.Errorf("error closing socket for svc: %s/%s on port: %v, err: %v", svc.Namespace, svc.Name, port, err)
		}
		delete(p.portsMap, *localPort)
		return nil
	}
	// If the port doesn't exist in the map, we've already closed it.
	// Don't consider this as an error, since we've reached the desired state.
	klog.Infof("Svc %s/%s: port %v is already closed, no action needed", svc.Namespace, svc.Name, port)
	return nil
}

func (p *localPortManager) emitPortClaimEvent(svc *kapi.Service, port int32, err error) {
	serviceRef := kapi.ObjectReference{
		Kind:      "Service",
		Namespace: svc.Namespace,
		Name:      svc.Name,
	}
	p.recorder.Eventf(&serviceRef, kapi.EventTypeWarning,
		"PortClaim", "Service: %s/%s requires port: %v to be opened on node, but port cannot be opened, err: %v", svc.Namespace, svc.Name, port, err)
	klog.Warningf("PortClaim for svc: %s/%s on port: %v, err: %v", svc.Namespace, svc.Name, port, err)
}

type portClaimWatcher struct {
	port portManager
}

func newPortClaimWatcher(recorder record.EventRecorder) (*portClaimWatcher, error) {
	localAddrSet, err := getLocalAddrs()
	if err != nil {
		return nil, err
	}
	return &portClaimWatcher{
		port: &localPortManager{
			recorder:          recorder,
			activeSocketsLock: sync.Mutex{},
			portsMap:          make(map[utilnet.LocalPort]utilnet.Closeable),
			localAddrSet:      localAddrSet,
			portOpener:        &utilnet.ListenPortOpener,
		},
	}, nil
}

func (p *portClaimWatcher) AddService(svc *kapi.Service) error {
	var errors []error
	if raw_errors := handleService(svc, p.port.open); len(errors) > 0 {
		for _, err := range raw_errors {
			errors = append(errors, fmt.Errorf("error claiming port for service: %s/%s: %v", svc.Namespace, svc.Name, err))
		}
	}
	return apierrors.NewAggregate(errors)
}

func (p *portClaimWatcher) UpdateService(old, new *kapi.Service) error {
	if reflect.DeepEqual(old.Spec.ExternalIPs, new.Spec.ExternalIPs) && reflect.DeepEqual(old.Spec.Ports, new.Spec.Ports) {
		return nil
	}
	var errors, raw_errors []error
	raw_errors = append(raw_errors, handleService(old, p.port.close)...)
	raw_errors = append(raw_errors, handleService(new, p.port.open)...)
	if len(raw_errors) > 0 {
		for _, err := range raw_errors {
			errors = append(errors, fmt.Errorf("error updating port claim for service: %s/%s: %v", old.Namespace, old.Name, err))
		}
	}
	return apierrors.NewAggregate(errors)
}

func (p *portClaimWatcher) DeleteService(svc *kapi.Service) error {
	var errors []error
	if raw_errors := handleService(svc, p.port.close); len(raw_errors) > 0 {
		for _, err := range raw_errors {
			errors = append(errors, fmt.Errorf("error removing port claim for service: %s/%s: %v", svc.Namespace, svc.Name, err))
		}
		return apierrors.NewAggregate(errors)
	}
	return nil
}

func (p *portClaimWatcher) SyncServices(objs []interface{}) error {
	return nil
}

func handleService(svc *kapi.Service, handler handler) []error {
	errors := []error{}
	if !util.ServiceTypeHasNodePort(svc) && len(svc.Spec.ExternalIPs) == 0 {
		return errors
	}

	for _, svcPort := range svc.Spec.Ports {
		if util.ServiceTypeHasNodePort(svc) {
			klog.V(5).Infof("Handle NodePort service %s port %d", svc.Name, svcPort.NodePort)
			if err := handlePort(getDescription(svcPort.Name, svc, true), svc, "", svcPort.NodePort, svcPort.Protocol, handler); err != nil {
				errors = append(errors, err)
			}
		}
		for _, externalIP := range svc.Spec.ExternalIPs {
			klog.V(5).Infof("Handle ExternalIPs service %s external IP %s port %d", svc.Name, externalIP, svcPort.Port)
			if err := handlePort(getDescription(svcPort.Name, svc, false), svc, utilnet.ParseIPSloppy(externalIP).String(), svcPort.Port, svcPort.Protocol, handler); err != nil {
				errors = append(errors, err)
			}
		}
	}
	return errors
}

// LocalPorts allows to add an arbitrary description, which can be used to distinguish LocalPorts instances having the
// same networking parameters by created for different services.
// kube-proxy and this implementation use the following format of the description: "
//
//	for NodePort services            - "nodePort for namespace/name[:portName]
//	for services with External IPs   - "externalIP for namespace/name[:portName]
func getDescription(portName string, svc *kapi.Service, nodePort bool) string {
	svcName := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	prefix := externalPortDescr
	if nodePort {
		prefix = nodePortDescr
	}
	if len(portName) == 0 {
		return fmt.Sprintf("%s %s", prefix, svcName.String())
	} else {
		return fmt.Sprintf("%s %s:%s", prefix, svcName.String(), portName)
	}
}

func handlePort(desc string, svc *kapi.Service, ip string, port int32, protocol kapi.Protocol, handler handler) error {
	if err := util.ValidatePort(protocol, port); err != nil {
		return fmt.Errorf("invalid service port %s, err: %v", svc.Name, err)
	}
	if err := handler(desc, ip, port, protocol, svc); err != nil {
		return err
	}
	return nil
}
