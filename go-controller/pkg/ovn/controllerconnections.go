package ovn

import (
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"

	networkattchmentdefclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
)

// ControllerConnections structure is placeholder for all fields shared among controllers.
type ControllerConnections struct {
	client         clientset.Interface
	multinetClient networkattchmentdefclientset.Interface
	kube           kube.Interface
	watchFactory   *factory.WatchFactory
	podRecorder    *metrics.PodRecorder

	// event recorder used to post events to k8s
	recorder record.EventRecorder

	// libovsdb northbound client interface
	nbClient libovsdbclient.Client

	// libovsdb southbound client interface
	sbClient libovsdbclient.Client

	// has SCTP support
	SCTPSupport bool
}

func (cc ControllerConnections) K8sClient() clientset.Interface {
	return cc.client
}

func (cc ControllerConnections) MultiNetClient() networkattchmentdefclientset.Interface {
	return cc.multinetClient
}

func (cc ControllerConnections) OvnkClient() kube.Interface {
	return cc.kube
}

func (cc ControllerConnections) WatchFactory() *factory.WatchFactory {
	return cc.watchFactory
}

func (cc ControllerConnections) PodRecorder() *metrics.PodRecorder {
	return cc.podRecorder
}

func (cc ControllerConnections) EventRecorder() record.EventRecorder {
	return cc.recorder
}

func (cc ControllerConnections) NBClient() libovsdbclient.Client {
	return cc.nbClient
}

func (cc ControllerConnections) SBClient() libovsdbclient.Client {
	return cc.sbClient
}

func NewOvnControllerConnectionManager(
	k8sClient clientset.Interface,
	multinetClient networkattchmentdefclientset.Interface,
	ovnkKubeClient kube.Interface,
	watchFactory *factory.WatchFactory,
	eventRecorder record.EventRecorder,
	podRecorder *metrics.PodRecorder,
	nbClient libovsdbclient.Client,
	sbClient libovsdbclient.Client,
) *ControllerConnections {
	return &ControllerConnections{
		client:         k8sClient,
		multinetClient: multinetClient,
		kube:           ovnkKubeClient,
		watchFactory:   watchFactory,
		podRecorder:    podRecorder,
		recorder:       eventRecorder,
		nbClient:       nbClient,
		sbClient:       sbClient,
		SCTPSupport:    false,
	}
}

func NewOvnControllerConnectionManagerWithSCTPSupport(
	k8sClient clientset.Interface,
	multinetClient networkattchmentdefclientset.Interface,
	ovnkKubeClient kube.Interface,
	watchFactory *factory.WatchFactory,
	eventRecorder record.EventRecorder,
	podRecorder *metrics.PodRecorder,
	nbClient libovsdbclient.Client,
	sbClient libovsdbclient.Client,
) *ControllerConnections {
	return &ControllerConnections{
		client:         k8sClient,
		multinetClient: multinetClient,
		kube:           ovnkKubeClient,
		watchFactory:   watchFactory,
		podRecorder:    podRecorder,
		recorder:       eventRecorder,
		nbClient:       nbClient,
		sbClient:       sbClient,
		SCTPSupport:    true,
	}
}
