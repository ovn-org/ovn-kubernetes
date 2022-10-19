package multi_homing

import (
	"fmt"

	"k8s.io/klog/v2"
	"sync"
	"time"

	"k8s.io/client-go/tools/record"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// ControllerManager structure is the object manages all controllers for all networks
type ControllerManager struct {
	ovn.ControllerConnections

	// default wait group and stop channel, used by default network controller
	defaultWg       *sync.WaitGroup
	defaultStopChan chan struct{}

	// unique identity for controllerManager running on different ovnkube-master instance,
	// used for leader election
	identity string

	// the default controller
	defaultController *ovn.Controller

	// controller for all networks, key is netName of net-attach-def, value is *Controller
	// this map is updated either at the very beginning of ovnkube-master when initializing the default controller
	// or when net-attach-def is added/deleted. All these are serialized and no lock protection is needed
	allOvnControllers map[string]*ovn.Controller
}

func NewControllerManager(ovnClient *util.OVNClientset, identity string, wf *factory.WatchFactory,
	stopChan chan struct{}, ovnNBClient libovsdbclient.Client, ovnSBClient libovsdbclient.Client,
	recorder record.EventRecorder, wg *sync.WaitGroup) (*ControllerManager, error) {
	podRecorder := metrics.NewPodRecorder()
	ovnkKubeClient := &kube.Kube{
		KClient:              ovnClient.KubeClient,
		EIPClient:            ovnClient.EgressIPClient,
		EgressFirewallClient: ovnClient.EgressFirewallClient,
		CloudNetworkClient:   ovnClient.CloudNetworkClient,
	}
	var controllerConnections *ovn.ControllerConnections
	hasSCTPSupport, err := util.DetectSCTPSupport()
	if err != nil {
		return nil, err
	}

	if hasSCTPSupport {
		klog.Info("SCTP support detected in OVN")
		controllerConnections = ovn.NewOvnControllerConnectionManagerWithSCTPSupport(
			ovnClient.KubeClient,
			ovnkKubeClient,
			wf,
			recorder,
			&podRecorder,
			ovnNBClient,
			ovnSBClient,
		)
	} else {
		klog.Warningf("SCTP unsupported by this version of OVN. Kubernetes service creation with SCTP will not work ")
		controllerConnections = ovn.NewOvnControllerConnectionManager(
			ovnClient.KubeClient,
			ovnkKubeClient,
			wf,
			recorder,
			&podRecorder,
			ovnNBClient,
			ovnSBClient,
		)
	}
	return &ControllerManager{
		ControllerConnections: *controllerConnections,
		defaultWg:             wg,
		defaultStopChan:       stopChan,
		identity:              identity,
		allOvnControllers:     make(map[string]*ovn.Controller),
	}, nil
}

func (cm *ControllerManager) Init() error {
	// the default network net_attach_def may not exist; we'd need to create default OVN Controller based on config.
	//_, err := cm.InitDefaultController(nil)
	//if err != nil {
	//	return err
	//}

	if err := cm.compressSBDatabase(); err != nil {
		return err
	}
	cm.configureMetrics()
	cm.PodRecorder().Run(cm.SBClient(), cm.defaultStopChan)

	// Start and sync the watch factory to begin listening for events
	if err := cm.WatchFactory().Start(); err != nil {
		return err
	}
	return nil
}

func (cm *ControllerManager) compressSBDatabase() error {
	// enableOVNLogicalDataPathGroups sets an OVN flag to enable logical datapath
	// groups on OVN 20.12 and later. The option is ignored if OVN doesn't
	// understand it. Logical datapath groups reduce the size of the southbound
	// database in large clusters. ovn-controllers should be upgraded to a version
	// that supports them before the option is turned on by the master.
	if err := libovsdbops.UpdateNBGlobalSetOptions(
		cm.NBClient(),
		&nbdb.NBGlobal{
			Options: map[string]string{"use_logical_dp_groups": "true"},
		},
	); err != nil {
		return fmt.Errorf("failed to set NB global option to enable logical datapath groups: %v", err)
	}
	return nil
}

func (cm *ControllerManager) configureMetrics() {
	metrics.RunTimestamp(cm.defaultStopChan, cm.SBClient(), cm.NBClient())
	metrics.MonitorIPSec(cm.NBClient())
	if config.Metrics.EnableConfigDuration {
		// with k=10,
		//  for a cluster with 10 nodes, measurement of 1 in every 100 requests
		//  for a cluster with 100 nodes, measurement of 1 in every 1000 requests
		metrics.GetConfigDurationRecorder().Run(cm.NBClient(), cm.OvnkClient(), 10, time.Second*5, cm.defaultStopChan)
	}
}
