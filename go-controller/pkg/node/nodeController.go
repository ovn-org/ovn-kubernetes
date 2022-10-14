package node

import (
	"fmt"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"time"

	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type ovnNodeController struct {
	name         string
	Kube         kube.Interface
	watchFactory factory.NodeWatchFactory

	nadInfo    *util.NetAttachDefInfo
	podHandler *factory.Handler
	started    bool
}

func (n *OvnNode) initDefaultController() *ovnNodeController {
	defaultNetConf := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
		},
		NetCidr:     config.Default.RawClusterSubnets,
		MTU:         config.Default.MTU,
		IsSecondary: false,
	}
	nadInfo := util.NewNetAttachDefInfo(defaultNetConf)
	nc := n.NewOvnNodeController(nadInfo)

	// Mark default controller to be "added" so that pod watcher won't be started
	// as result of adding other default network net-attach-def. Instead, it will
	// be started after all net-attach-def are added. That is when pod watcher can
	nc.started = true
	return nc
}

func (n *OvnNode) NewOvnNodeController(nadInfo *util.NetAttachDefInfo) *ovnNodeController {
	nc := &ovnNodeController{
		name:         n.name,
		watchFactory: n.watchFactory,
		Kube:         n.Kube,
		nadInfo:      nadInfo,
		started:      false,
	}
	n.allNodeControllers[nadInfo.NetName] = nc
	return nc
}

// watchNetworkAttachmentDefinitions starts the watching of network attachment definition
// resource and calls back the appropriate handler logic
func (n *OvnNode) watchNetworkAttachmentDefinitions() error {
	start := time.Now()
	_, err := n.watchFactory.AddNetworkattachmentdefinitionHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			netattachdef := obj.(*nettypes.NetworkAttachmentDefinition)
			n.addNetworkAttachDefinition(netattachdef)
		},
		UpdateFunc: func(old, new interface{}) {},
		DeleteFunc: func(obj interface{}) {
			netattachdef := obj.(*nettypes.NetworkAttachmentDefinition)
			n.deleteNetworkAttachDefinition(netattachdef)
		},
	}, n.syncNetworkAttachDefinition)
	klog.Infof("Bootstrapping existing Network Attachment Definitions took %v", time.Since(start))
	return err
}

func (n *OvnNode) initOvnNodeController(netattachdef *nettypes.NetworkAttachmentDefinition) (*ovnNodeController, error) {
	nadInfo, err := util.ParseNADInfo(netattachdef)
	if err != nil {
		return nil, err
	}

	// Note that net-attach-def add/delete/update events are serialized, so we don't need locks here.
	// Check if any Controller of the same netconf.Name already exists, if so, check its conf to see if they are the same.
	nc, ok := n.allNodeControllers[nadInfo.NetName]
	if ok {
		// for default network, the configuration comes from command configuration, do not validate
		if nc.nadInfo.IsSecondary && (nc.nadInfo.NetCidr != nadInfo.NetCidr || nc.nadInfo.MTU != nadInfo.MTU) {
			return nil, fmt.Errorf("network attachment definition %s/%s does not share the same CNI config of name %s",
				netattachdef.Namespace, netattachdef.Name, nadInfo.NetName)
		} else {
			nc.nadInfo.NetAttachDefs.Store(util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name), true)
			return nc, nil
		}
	}

	nc = n.NewOvnNodeController(nadInfo)
	nc.nadInfo.NetAttachDefs.Store(util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name), true)
	return nc, nil
}

// syncNetworkAttachDefinition() delete OVN logical entities of the obsoleted netNames.
func (n *OvnNode) syncNetworkAttachDefinition(netattachdefs []interface{}) error {
	// we need to walk through all net-attach-def and add them into Controller.nadInfo.NetAttachDefs, so that when each
	// Controller is running, watchPodsDPU()->IsNetworkOnPod() can correctly check Pods need to be plumbed
	// for the specific Controller
	for _, netattachdefIntf := range netattachdefs {
		netattachdef, ok := netattachdefIntf.(*nettypes.NetworkAttachmentDefinition)
		if !ok {
			klog.Errorf("Spurious object in syncNetworkAttachDefinition: %v", netattachdefIntf)
			continue
		}

		_, err := n.initOvnNodeController(netattachdef)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}
	return nil
}

func (n *OvnNode) addNetworkAttachDefinition(netattachdef *nettypes.NetworkAttachmentDefinition) {
	nc, err := n.initOvnNodeController(netattachdef)
	if err != nil {
		klog.Errorf(err.Error())
		return
	}

	if nc == nil || nc.started {
		return
	}

	nc.started = true

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		if err = nc.watchPodsDPU(n.ovnUpEnabled); err != nil {
			klog.Errorf(err.Error())
		}
	}
}

func (n *OvnNode) deleteNetworkAttachDefinition(netattachdef *nettypes.NetworkAttachmentDefinition) {
	klog.Infof("Delete Network Attachment Definition %s/%s", netattachdef.Namespace, netattachdef.Name)
	netconf, err := util.ParseNetConf(netattachdef)
	if err != nil {
		if err != util.ErrorAttachDefNotOvnManaged {
			klog.Error(err)
		}
		return
	}
	netName := netconf.Name
	nadName := util.GetNadKeyName(netattachdef.Namespace, netattachdef.Name)
	nc, ok := n.allNodeControllers[netName]
	if !ok {
		klog.Errorf("Failed to find network controller for network %s", netName)
		return
	}
	_, ok = nc.nadInfo.NetAttachDefs.LoadAndDelete(nadName)
	if !ok {
		klog.Errorf("Failed to find nad %s from network controller for network %s", nadName, netName)
		return
	}

	if !nc.nadInfo.IsSecondary {
		return
	}

	// check if there any net-attach-def sharing the same CNI conf name left, if yes, just return
	netAttachDefLeft := false
	nc.nadInfo.NetAttachDefs.Range(func(key, value interface{}) bool {
		netAttachDefLeft = true
		return false
	})

	if netAttachDefLeft {
		return
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		if nc.podHandler != nil {
			nc.watchFactory.RemovePodHandler(nc.podHandler)
		}
	}

	delete(n.allNodeControllers, netName)
}
