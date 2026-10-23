// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/libovsdb/client"

	honode "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni"
	config "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/controllers/egressip"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/dpulease"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/managementport"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/ovspinning"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/podresourcesapi"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/routemanager"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
	nodeutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type CommonNodeNetworkControllerInfo struct {
	client                 clientset.Interface
	Kube                   kube.Interface
	watchFactory           factory.NodeWatchFactory
	recorder               record.EventRecorder
	name                   string // Kubernetes node name managed by this controller.
	apbExternalRouteClient adminpolicybasedrouteclientset.Interface
	// route manager that creates and manages routes
	routeManager *routemanager.Controller
}

// BaseNodeNetworkController structure per-network fields and network specific configuration
type BaseNodeNetworkController struct {
	CommonNodeNetworkControllerInfo

	// network information
	util.ReconcilableNetInfo

	// networkManager used for getting network information
	networkManager networkmanager.Interface

	// ovsClient is the libovsdb client connected to the local ovsdb-server.
	// Used by DPU representor cleanup and other OVS-aware bookkeeping.
	ovsClient client.Client

	// stopChan and WaitGroup per controller
	stopChan chan struct{}
	wg       *sync.WaitGroup
}

func newCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, kube kube.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface,
	wf factory.NodeWatchFactory, eventRecorder record.EventRecorder, name string, routeManager *routemanager.Controller) *CommonNodeNetworkControllerInfo {

	return &CommonNodeNetworkControllerInfo{
		client:                 kubeClient,
		Kube:                   kube,
		apbExternalRouteClient: apbExternalRouteClient,
		watchFactory:           wf,
		name:                   name,
		recorder:               eventRecorder,
		routeManager:           routeManager,
	}
}

// NewCommonNodeNetworkControllerInfo creates and returns the base node network controller info
func NewCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface, wf factory.NodeWatchFactory,
	eventRecorder record.EventRecorder, name string, routeManager *routemanager.Controller) *CommonNodeNetworkControllerInfo {
	return newCommonNodeNetworkControllerInfo(kubeClient, &kube.Kube{KClient: kubeClient}, apbExternalRouteClient, wf, eventRecorder, name, routeManager)
}

// DefaultNodeNetworkController is the object holder for utilities meant for node management of default network
type DefaultNodeNetworkController struct {
	BaseNodeNetworkController

	mgmtPortController managementport.Controller
	Gateway            Gateway

	// Node healthcheck server for cloud load balancers
	healthzServer *proxierHealthUpdater
	routeManager  *routemanager.Controller
	linkManager   *linkmanager.Controller

	// retry framework for endpoint slices, used for the removal of stale conntrack entries for services
	retryEndpointSlices *retry.RetryFramework

	// retry framework for nodes, used for updating routes/nftables rules for node PMTUD guarding
	retryNodes *retry.RetryFramework

	dpuNodeLeaseManager *dpulease.Manager

	apbExternalRouteNodeController *apbroute.ExternalGatewayNodeController

	cniServer *cni.Server

	udnHostIsolationManager *UDNHostIsolationManager

	masqReconciler *masqueradeReconciler

	nodeAddress net.IP
}

func newDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, stopChan chan struct{},
	wg *sync.WaitGroup, routeManager *routemanager.Controller, networkManager networkmanager.Interface, ovsClient client.Client) *DefaultNodeNetworkController {

	c := &DefaultNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			ReconcilableNetInfo:             &util.DefaultNetInfo{},
			networkManager:                  networkManager,
			stopChan:                        stopChan,
			wg:                              wg,
			ovsClient:                       ovsClient,
		},
		routeManager: routeManager,
	}
	if util.IsNetworkSegmentationSupportEnabled() && (config.IsModeDPUHost() || config.IsModeFull()) {
		c.udnHostIsolationManager = NewUDNHostIsolationManager(config.IPv4Mode, config.IPv6Mode,
			cnnci.watchFactory.PodCoreInformer(), cnnci.name, cnnci.recorder)
	}
	c.masqReconciler = &masqueradeReconciler{
		routeManager: routeManager,
		nodeName:     cnnci.name,
		watchFactory: cnnci.watchFactory,
	}
	c.linkManager = linkmanager.NewController(cnnci.name, config.IPv4Mode, config.IPv6Mode, func(link netlink.Link) error {
		if err := c.updateGatewayMAC(link); err != nil {
			return err
		}
		if c.Gateway != nil && config.Gateway.Interface != "" && link.Attrs().Name == config.Gateway.Interface {
			if err := c.masqReconciler.ensure(); err != nil {
				klog.Errorf("Masquerade reconciler on link event: %v", err)
			}
		}
		return nil
	})
	return c
}

// NewDefaultNodeNetworkController creates a new network controller for node management of the default network
func NewDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, networkManager networkmanager.Interface, ovsClient client.Client) (*DefaultNodeNetworkController, error) {
	var err error
	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	nc := newDefaultNodeNetworkController(cnnci, stopChan, wg, cnnci.routeManager, networkManager, ovsClient)

	if len(config.Kubernetes.HealthzBindAddress) != 0 {
		klog.Infof("Enable node proxy healthz server on %s", config.Kubernetes.HealthzBindAddress)
		nc.healthzServer, err = newNodeProxyHealthzServer(
			nc.name, config.Kubernetes.HealthzBindAddress, nc.recorder, nc.watchFactory)
		if err != nil {
			return nil, fmt.Errorf("could not create node proxy healthz server: %w", err)
		}
	}

	nc.apbExternalRouteNodeController, err = apbroute.NewExternalNodeController(
		nc.watchFactory.PodCoreInformer(),
		nc.watchFactory.NamespaceInformer(),
		nc.watchFactory.APBRouteInformer(),
		stopChan)
	if err != nil {
		return nil, err
	}

	nc.initRetryFrameworkForNode()

	if config.IsModeDPUHost() || config.IsModeFull() {
		err = setupRemoteNodeNFTSets()
		if err != nil {
			return nil, fmt.Errorf("failed to setup PMTUD nftables sets: %w", err)
		}

		err = setupPMTUDNFTChain()
		if err != nil {
			return nil, fmt.Errorf("failed to setup PMTUD nftables chain: %w", err)
		}

		if util.IsRouteAdvertisementsEnabled() {
			err = configureAdvertisedUDNIsolationNFTables()
			if err != nil {
				return nil, fmt.Errorf("failed to setup advertised UDN isolation nftables: %w", err)
			}
		}

		// Setup nftables sets for no-overlay SNAT exemption in LGW mode.
		// In SGW mode, OVN address sets are used instead.
		if config.Default.Transport == types.NetworkTransportNoOverlay && config.NoOverlay.OutboundSNAT == types.NoOverlaySNATEnabled && config.Gateway.Mode == config.GatewayModeLocal {
			err = setupNoOverlaySNATExemptNFTSets()
			if err != nil {
				return nil, fmt.Errorf("failed to setup no-overlay SNAT exemption nftables sets: %w", err)
			}
		}
	}

	return nc, nil
}

func (nc *DefaultNodeNetworkController) initRetryFrameworkForNode() {
	nc.retryEndpointSlices = nc.newRetryFrameworkNode(factory.EndpointSliceForStaleConntrackRemovalType)
	nc.retryNodes = nc.newRetryFrameworkNode(factory.NodeType)
}

func (oc *DefaultNodeNetworkController) shouldReconcileNetworkChange(old, new util.NetInfo) bool {
	wasPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(old, oc.name)
	isPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(new, oc.name)
	return wasPodNetworkAdvertisedAtNode != isPodNetworkAdvertisedAtNode
}

func (oc *DefaultNodeNetworkController) Reconcile(_ context.Context, netInfo util.NetInfo) error {
	// inspect changes first
	reconcilePodNetwork := oc.shouldReconcileNetworkChange(oc.ReconcilableNetInfo, netInfo)

	// reconcile subcontrollers
	if reconcilePodNetwork {
		isPodNetworkAdvertisedAtNode := util.IsPodNetworkAdvertisedAtNode(netInfo, oc.name)
		if oc.Gateway != nil {
			oc.Gateway.SetDefaultPodNetworkAdvertised(isPodNetworkAdvertisedAtNode)
			err := oc.Gateway.Reconcile()
			if err != nil {
				return fmt.Errorf("failed to reconcile gateway: %v", err)
			}
		}

		if oc.mgmtPortController != nil {
			oc.mgmtPortController.Reconcile()
		}
	}

	// Update network information. We can do this now because gateway and
	// management port reconciliation done above does not rely on NetInfo
	err := util.ReconcileNetInfo(oc.ReconcilableNetInfo, netInfo)
	if err != nil {
		return fmt.Errorf("failed to reconcile network %s: %v", oc.GetNetworkName(), err)
	}

	return nil
}

func clearOVSFlowTargets() error {
	_, _, err := util.RunOVSVsctl(
		"--",
		"clear", "bridge", "br-int", "netflow",
		"--",
		"clear", "bridge", "br-int", "sflow",
		"--",
		"clear", "bridge", "br-int", "ipfix",
	)
	if err != nil {
		return err
	}
	return nil
}

// collectorsString joins all HostPort entry into a string that is acceptable as
// target by the ovs-vsctl command. If an entry has an empty host, it uses the Node IP
func collectorsString(node *corev1.Node, targets []config.HostPort) (string, error) {
	if len(targets) == 0 {
		return "", errors.New("collector targets can't be empty")
	}
	var joined strings.Builder
	for n, v := range targets {
		if n == 0 {
			joined.WriteByte('"')
		} else {
			joined.WriteString(`","`)
		}
		var host string
		if v.Host != nil && len(*v.Host) != 0 {
			host = v.Host.String()
		} else {
			var err error
			if host, err = util.GetNodePrimaryIP(node); err != nil {
				return "", fmt.Errorf("composing flow collectors' IPs: %w", err)
			}
		}
		joined.WriteString(util.JoinHostPortInt32(host, v.Port))
	}
	joined.WriteByte('"')
	return joined.String(), nil
}

func setOVSFlowTargets(node *corev1.Node) error {
	if len(config.Monitoring.NetFlowTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.NetFlowTargets)
		if err != nil {
			return fmt.Errorf("error joining NetFlow targets: %w", err)
		}

		_, stderr, err := util.RunOVSVsctl(
			"--",
			"--id=@netflow",
			"create",
			"netflow",
			fmt.Sprintf("targets=[%s]", collectors),
			"active_timeout=60",
			"--",
			"set", "bridge", "br-int", "netflow=@netflow",
		)
		if err != nil {
			return fmt.Errorf("error setting NetFlow: %v\n  %q", err, stderr)
		}
	}
	if len(config.Monitoring.SFlowTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.SFlowTargets)
		if err != nil {
			return fmt.Errorf("error joining SFlow targets: %w", err)
		}

		_, stderr, err := util.RunOVSVsctl(
			"--",
			"--id=@sflow",
			"create",
			"sflow",
			"agent="+types.SFlowAgent,
			fmt.Sprintf("targets=[%s]", collectors),
			"--",
			"set", "bridge", "br-int", "sflow=@sflow",
		)
		if err != nil {
			return fmt.Errorf("error setting SFlow: %v\n  %q", err, stderr)
		}
	}
	if len(config.Monitoring.IPFIXTargets) != 0 {
		collectors, err := collectorsString(node, config.Monitoring.IPFIXTargets)
		if err != nil {
			return fmt.Errorf("error joining IPFIX targets: %w", err)
		}

		args := []string{
			"--",
			"--id=@ipfix",
			"create",
			"ipfix",
			fmt.Sprintf("targets=[%s]", collectors),
			fmt.Sprintf("cache_active_timeout=%d", config.IPFIX.CacheActiveTimeout),
		}
		if config.IPFIX.CacheMaxFlows != 0 {
			args = append(args, fmt.Sprintf("cache_max_flows=%d", config.IPFIX.CacheMaxFlows))
		}
		if config.IPFIX.Sampling != 0 {
			args = append(args, fmt.Sprintf("sampling=%d", config.IPFIX.Sampling))
		}
		args = append(args, "--", "set", "bridge", "br-int", "ipfix=@ipfix")
		_, stderr, err := util.RunOVSVsctl(args...)
		if err != nil {
			return fmt.Errorf("error setting IPFIX: %v\n  %q", err, stderr)
		}
	}
	return nil
}

// validateEncapIP returns false if there is an error or if the given IP is not known local IP address.
func validateEncapIP(encapIP string) (bool, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return false, fmt.Errorf("failed to get all the links on the node: %v", err)
	}
	for _, link := range links {
		addrs, err := util.GetFilteredInterfaceAddrs(link, config.IPv4Mode, config.IPv6Mode)
		if err != nil {
			return false, err
		}
		for _, addr := range addrs {
			if addr.IP.String() == encapIP {
				return true, nil
			}
		}
	}
	return false, nil
}

func setupOVNNode(node *corev1.Node) error {
	var err error

	nodePrimaryIP, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return fmt.Errorf("failed to obtain local primary IP from node %q: %v", node.Name, err)
	}

	encapIP := config.Default.EncapIP
	if encapIP == "" {
		config.Default.EffectiveEncapIP = nodePrimaryIP
	} else {
		// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma.
		config.Default.EffectiveEncapIP = encapIP
		ovnEncapIps := strings.Split(encapIP, ",")
		for _, ovnEncapIp := range ovnEncapIps {
			if ip := net.ParseIP(strings.TrimSpace(ovnEncapIp)); ip == nil {
				return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIp, encapIP)
			}
		}
		// if there are more than one encap IPs, it must be configured explicitly. otherwise:
		if len(ovnEncapIps) == 1 {
			encapIP = ovnEncapIps[0]
			if encapIP == nodePrimaryIP {
				// the current encap IP is node primary IP, unset config.Default.EncapIP to indicate it is
				// implicitly configured through the old external_ids:ovn-encap-ip value and needs to be updated
				// if node primary IP changes.
				config.Default.EncapIP = ""
			} else {
				// the encap IP may be incorrectly set or;
				// previous implicitly set with the old primary node IP through the old external_ids:ovn-encap-ip value,
				// that has changed when ovnkube-node is down.
				// validate it to see if it is still a valid local IP address.
				valid, err := validateEncapIP(encapIP)
				if err != nil {
					return fmt.Errorf("invalid encap IP %s: %v", encapIP, err)
				}
				if !valid {
					return fmt.Errorf("invalid encap IP %s: does not exist", encapIP)
				}
			}
		}
	}

	setExternalIdsCmd := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", config.Default.EffectiveEncapIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:ovn-bridge-remote-probe-interval=%d",
			config.Default.OpenFlowProbe),
		// bundle-idle-timeout default value is 10s, it should be set
		// as high as the ovn-bridge-remote-probe-interval to allow ovn-controller
		// to finish computation specially with complex acl configuration with port range.
		fmt.Sprintf("other_config:bundle-idle-timeout=%d",
			config.Default.OpenFlowProbe),
		// Tell ovn-controller to make this node/chassis an interconnect gateway.
		"external_ids:ovn-is-interconn=true",
		fmt.Sprintf("external_ids:ovn-monitor-all=%t", config.Default.MonitorAll),
		fmt.Sprintf("external_ids:ovn-ofctrl-wait-before-clear=%d", config.Default.OfctrlWaitBeforeClear),
		fmt.Sprintf("external_ids:ovn-enable-lflow-cache=%t", config.Default.LFlowCacheEnable),
		// when creating tunnel ports set local_ip, helps ensures multiple interfaces and ipv6 will work
		"external_ids:ovn-set-local-ip=\"true\"",
	}

	if config.Default.LFlowCacheLimit > 0 {
		setExternalIdsCmd = append(setExternalIdsCmd,
			fmt.Sprintf("external_ids:ovn-limit-lflow-cache=%d", config.Default.LFlowCacheLimit),
		)
	}

	if config.Default.LFlowCacheLimitKb > 0 {
		setExternalIdsCmd = append(setExternalIdsCmd,
			fmt.Sprintf("external_ids:ovn-memlimit-lflow-cache-kb=%d", config.Default.LFlowCacheLimitKb),
		)
	}

	// In the case of DPU, the hostname should be that of the DPU and not
	// the K8s Node's. So skip setting the incorrect hostname.
	if config.IsModeDPUHost() || config.IsModeFull() {
		setExternalIdsCmd = append(setExternalIdsCmd, fmt.Sprintf("external_ids:hostname=\"%s\"", node.Name))
	}

	_, stderr, err := util.RunOVSVsctl(setExternalIdsCmd...)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}

	// clear stale ovs flow targets if needed
	err = clearOVSFlowTargets()
	if err != nil {
		return fmt.Errorf("error clearing stale ovs flow targets: %q", err)
	}
	// set new ovs flow targets if needed
	err = setOVSFlowTargets(node)
	if err != nil {
		return fmt.Errorf("error setting ovs flow targets: %q", err)
	}

	return nil
}

func setEncapPort(ctx context.Context) error {
	systemID, err := util.GetNodeChassisID()
	if err != nil {
		return err
	}
	var uuid string
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true,
		func(_ context.Context) (bool, error) {
			uuid, _, err = util.RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "Encap",
				fmt.Sprintf("chassis_name=%s", systemID))
			return len(uuid) != 0, err
		})
	if err != nil {
		return err
	}
	if len(uuid) == 0 {
		return fmt.Errorf("unable to find encap uuid to set geneve port for chassis %s", systemID)
	}
	_, stderr, errSet := util.RunOVNSbctl("set", "encap", uuid,
		fmt.Sprintf("options:dst_port=%d", config.Default.EncapPort),
	)
	if errSet != nil {
		return fmt.Errorf("error setting OVS encap-port: %v\n  %q", errSet, stderr)
	}
	return nil
}

func isOVNControllerReady(ovsClient client.Client) (bool, error) {
	// check node's connection status
	ret, _, err := util.RunOVNControllerAppCtl("connection-status")
	if err != nil {
		return false, fmt.Errorf("could not get connection status: %w", err)
	}
	klog.Infof("Node connection status = %s", ret)
	if ret != "connected" {
		return false, nil
	}

	// check whether br-int exists on node
	if _, err := ovsops.GetBridge(ovsClient, "br-int"); err != nil {
		if !errors.Is(err, client.ErrNotFound) {
			return false, fmt.Errorf("could not check br-int bridge existence: %w", err)
		}
		return false, nil
	}

	// check by dumping br-int flow entries
	stdout, _, err := util.RunOVSOfctl("dump-aggregate", "br-int")
	if err != nil {
		klog.V(5).Infof("Error dumping aggregate flows: %v", err)
		return false, nil
	}
	hasFlowCountZero := strings.Contains(stdout, "flow_count=0")
	if hasFlowCountZero {
		klog.V(5).Info("Got a flow count of 0 when dumping flows for node")
		return false, nil
	}

	return true, nil
}

// Resolve gateway interface from PCI address when configured as "derive-from-mgmt-port"
// configureGatewayInterfaceFromMgmtPort resolves and sets the gateway interface derived
// from the management port's PF when `config.Gateway.Interface` is set to derive-from-mgmt-port.
func configureGatewayInterfaceFromMgmtPort() error {
	if config.Gateway.Interface != types.DeriveFromMgmtPort {
		return nil
	}
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return err
	}
	pciAddr, err := util.GetSriovnetOps().GetPciFromNetDevice(netdevName)
	if err != nil {
		return err
	}
	pfPciAddr, err := util.GetSriovnetOps().GetPfPciFromVfPci(pciAddr)
	if err != nil {
		return err
	}
	netdevs, err := util.GetSriovnetOps().GetNetDevicesFromPci(pfPciAddr)
	if err != nil {
		return err
	}
	if len(netdevs) == 0 {
		return fmt.Errorf("no netdevs found for pci address %s", pfPciAddr)
	}
	config.Gateway.Interface = netdevs[0]
	return nil
}

func exportManagementPortAnnotation(netdevName string, nodeAnnotator kube.Annotator) error {
	klog.Infof("Exporting management port annotation for default network: netdev '%v'", netdevName)
	cfg, err := util.GetDPUOps().ResolveDeviceDetails(netdevName)
	if err != nil {
		return err
	}
	return util.SetNodeManagementPortAnnotation(nodeAnnotator, cfg)
}

// Take care of alternative names for the netdevName by making sure we
// use the link attribute name as well as handle the case when netdevName
// was renamed to types.K8sMgmtIntfName
func getManagementPortNetDev(netdevName string) (string, error) {
	link, err := util.GetNetLinkOps().LinkByName(netdevName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return "", fmt.Errorf("failed to lookup %s link: %v", netdevName, err)
		}
		// this may not the first time invoked on the node after reboot
		// netdev may have already been renamed to ovn-k8s-mp0.
		link, err = util.GetNetLinkOps().LinkByName(types.K8sMgmtIntfName)
		if err != nil {
			return "", fmt.Errorf("failed to get link device for %s. %v", netdevName, err)
		}
	}

	if link.Attrs().Name != netdevName {
		klog.Infof("'%v' != '%v' (link.Attrs().Name != netdevName)", link.Attrs().Name, netdevName)
	}
	return link.Attrs().Name, err
}

func getMgmtPortAndRepNameModeFull() (string, string, error) {
	if config.OvnKubeNode.MgmtPortNetdev == "" {
		return "", "", nil
	}
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return "", "", err
	}
	deviceID, err := util.GetDeviceIDFromNetdevice(netdevName)
	if err != nil {
		return "", "", fmt.Errorf("failed to get device id for %s: %v", netdevName, err)
	}
	rep, err := util.GetFunctionRepresentorName(deviceID)
	if err != nil {
		return "", "", err
	}
	return netdevName, rep, err
}

// In DPU mode, read the annotation from the host side which should have been
// exported by ovn-k running in DPU host mode.
func getMgmtPortAndRepNameModeDPU(node *corev1.Node) (string, string, error) {
	cfgs, err := util.ParseNodeManagementPortAnnotation(node)
	if err != nil {
		return "", "", err
	}
	cfg, ok := cfgs[types.DefaultNetworkName]
	if !ok {
		return "", "", fmt.Errorf("failed to find management port details for %s network", types.DefaultNetworkName)
	}
	rep, err := util.GetDPUOps().GetPortRepresentor(fmt.Sprintf("%d", cfg.PfId), fmt.Sprintf("%d", cfg.FuncId))
	return "", rep, err
}

func getMgmtPortAndRepNameModeDPUHost() (string, string, error) {
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return "", "", err
	}
	return netdevName, "", nil
}

func getMgmtPortAndRepName(node *corev1.Node) (string, string, error) {
	switch config.OvnKubeNode.Mode {
	case types.NodeModeFull:
		return getMgmtPortAndRepNameModeFull()
	case types.NodeModeDPU:
		return getMgmtPortAndRepNameModeDPU(node)
	case types.NodeModeDPUHost:
		return getMgmtPortAndRepNameModeDPUHost()
	default:
		return "", "", fmt.Errorf("unexpected config.OvnKubeNode.Mode '%v'", config.OvnKubeNode.Mode)
	}
}

func createNodeManagementPortController(
	ovsClient client.Client,
	node *corev1.Node,
	subnets []*net.IPNet,
	nodeAnnotator kube.Annotator,
	routeManager *routemanager.Controller,
	netInfo util.NetInfo,
) (managementport.Controller, error) {
	netdevName, rep, err := getMgmtPortAndRepName(node)
	if err != nil {
		return nil, err
	}

	if config.OvnKubeNode.MgmtPortDPResourceName == "" && config.IsModeDPUHost() {
		// this is called only when config.OvnKubeNode.MgmtPortDPResourceName is empty and in dpu-host mode:
		// 1. If config.OvnKubeNode.MgmtPortDPResourceName is not empty, management port annotation is taken care
		//    of by node controller manage.
		// 2. In full mode, representor interface is returned by calling getMgmtPortAndRepName(), not from the annotation.
		err := exportManagementPortAnnotation(netdevName, nodeAnnotator)
		if err != nil {
			return nil, err
		}
	}
	return managementport.NewManagementPortController(ovsClient, node, subnets, netdevName, rep, routeManager, netInfo)
}

// removeStaleDPUHostAddressAnnotation clears state owned by DPU-host mode before
// full-mode initialization continues, so consumers do not classify this node as a DPU host.
func (nc *DefaultNodeNetworkController) removeStaleDPUHostAddressAnnotation() error {
	if !config.IsModeFull() {
		return nil
	}

	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, nc.name)
	nodeAnnotator.Delete(util.OVNNodePrimaryDPUHostAddr)
	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to remove stale %s annotation from node %s: %w",
			util.OVNNodePrimaryDPUHostAddr, nc.name, err)
	}
	return nil
}

// Init executes the first steps to start the DefaultNodeNetworkController.
// It is split from Start() and executed before UserDefinedNodeNetworkController (UDNNC)
// to allow UDNNC to reference the openflow manager created in Init.
func (nc *DefaultNodeNetworkController) Init(ctx context.Context) error {
	klog.Infof("Initializing the default node network controller")

	if nc.name == "" {
		return fmt.Errorf("ovnkube-node name is required")
	}
	if err := nc.removeStaleDPUHostAddressAnnotation(); err != nil {
		return err
	}

	var err error
	var node *corev1.Node
	var subnets []*net.IPNet
	var cniServer *cni.Server

	if config.IsModeDPUHost() || config.IsModeFull() {
		if err = configureGlobalForwarding(); err != nil {
			return err
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		// Bootstrap flows in OVS if just normal flow is present
		if err := bootstrapOVSFlows(nc.ovsClient, nc.name); err != nil {
			return fmt.Errorf("failed to bootstrap OVS flows: %w", err)
		}
	}

	if node, err = nc.watchFactory.GetNode(nc.name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", nc.name, err)
	}

	nodeAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return err
	}
	nodeAddr := net.ParseIP(nodeAddrStr)
	if nodeAddr == nil {
		return fmt.Errorf("failed to parse kubernetes node IP address. %v", nodeAddrStr)
	}

	if (config.IsModeDPUHost() || config.IsModeDPU()) &&
		config.OvnKubeNode.DPUNodeLeaseRenewInterval > 0 {
		nc.dpuNodeLeaseManager = dpulease.NewManager(
			nc.client,
			config.Kubernetes.OVNConfigNamespace,
			node,
			time.Duration(config.OvnKubeNode.DPUNodeLeaseRenewInterval)*time.Second,
			time.Duration(config.OvnKubeNode.DPUNodeLeaseDuration)*time.Second,
		)
		if config.IsModeDPUHost() {
			if _, err := nc.dpuNodeLeaseManager.EnsureLease(ctx); err != nil {
				return err
			}
		}
	}

	if !config.IsModeDPUHost() {
		if err := config.OvnSouth.SetOVNRemote(); err != nil {
			return err
		}

		err = setupOVNNode(node)
		if err != nil {
			return err
		}
	}

	if config.IsModeDPUHost() || config.IsModeFull() {
		if nc.udnHostIsolationManager != nil {
			if err = nc.udnHostIsolationManager.Start(ctx); err != nil {
				return err
			}
		} else {
			if err = CleanupUDNHostIsolation(); err != nil {
				return fmt.Errorf("failed cleaning up UDN host isolation: %w", err)
			}
		}
	}

	// First wait for the node logical switch to be created by ovnkube-controller, timeout is 300s.
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(_ context.Context) (bool, error) {
		if node, err = nc.watchFactory.GetNode(nc.name); err != nil {
			klog.Infof("Waiting to retrieve node %s: %v", nc.name, err)
			return false, nil
		}
		subnets, err = util.ParseNodeHostSubnetAnnotation(node, types.DefaultNetworkName)
		if err != nil {
			klog.Infof("Waiting for node %s to start, no annotation found on node for subnet: %v", nc.name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for node's: %q logical switch: %v", nc.name, err)
	}
	klog.Infof("Node %s ready for ovn initialization with subnet %s", nc.name, util.JoinIPNets(subnets, ","))

	// Create CNI Server
	if config.IsModeDPUHost() || config.IsModeFull() {
		kclient, ok := nc.Kube.(*kube.Kube)
		if !ok {
			return fmt.Errorf("cannot get kubeclient for starting CNI server")
		}
		var dpuHealth cni.DPUStatusProvider
		if nc.dpuNodeLeaseManager != nil {
			dpuHealth = nc.dpuNodeLeaseManager
		}
		cniServer, err = cni.NewCNIServer(nc.watchFactory, kclient.KClient, nc.networkManager, nc.ovsClient, dpuHealth)
		if err != nil {
			return err
		}
		nc.cniServer = cniServer
	}

	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, node.Name)

	if config.IsModeDPUHost() {
		if err := configureGatewayInterfaceFromMgmtPort(); err != nil {
			return err
		}
	}

	// Setup management ports
	nc.mgmtPortController, err = createNodeManagementPortController(
		nc.ovsClient,
		node,
		subnets,
		nodeAnnotator,
		nc.routeManager,
		nc.GetNetInfo(),
	)
	if err != nil {
		return err
	}
	nc.nodeAddress = nodeAddr

	// Set the node-encap-ips annotation with the configured encap IP.
	// This encap IP is unavailable on the DPU host mode, so we don't need to set it there.
	if config.IsModeDPU() || config.IsModeFull() {
		encapIPList := sets.New[string]()
		encapIPList.Insert(strings.Split(config.Default.EffectiveEncapIP, ",")...)
		if err := util.SetNodeEncapIPs(nodeAnnotator, encapIPList); err != nil {
			return fmt.Errorf("failed to set node-encap-ips annotation for node %s: %w", nc.name, err)
		}
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	// Connect ovn-controller to SBDB
	if config.IsModeDPU() || config.IsModeFull() {
		if err := config.OvnSouth.SetOVNRemote(); err != nil {
			return fmt.Errorf("unable to configure the local OVN Southbound database endpoint: %w", err)
		}
	}

	// First part of gateway initialization. It will be completed by (nc *DefaultNodeNetworkController) Start()
	if config.IsModeDPU() || config.IsModeFull() {
		// IPv6 is not supported in DPU enabled nodes, error out if ovnkube is not set in IPv4 mode
		if config.IPv6Mode && config.IsModeDPU() {
			return fmt.Errorf("IPv6 mode is not supported on a DPU enabled node")
		}
		// Initialize gateway for OVS internal port or representor management port
		gw, err := nc.initGatewayPreStart(subnets, nodeAnnotator, nc.mgmtPortController)
		if err != nil {
			return err
		}
		nc.Gateway = gw
	} else {
		err = nc.initGatewayDPUHostPreStart(nc.nodeAddress, nodeAnnotator)
		if err != nil {
			return err
		}
	}

	return nil
}

// Start learns the subnets assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (nc *DefaultNodeNetworkController) Start(ctx context.Context) (err error) {
	klog.Infof("Starting the default node network controller")

	if nc.mgmtPortController == nil {
		return fmt.Errorf("default node network controller hasn't been pre-started")
	}

	waiter := newStartupWaiter()

	// Complete gateway initialization
	if config.IsModeDPUHost() {
		err = nc.initGatewayDPUHost()
		if err != nil {
			return err
		}
	} else {
		gw := nc.Gateway.(*gateway)
		if err := nc.initGatewayMainStart(gw, waiter); err != nil {
			return err
		}
	}

	// If EncapPort is not the default, tell SBDB to use the specified port.
	// setEncapPort waits for ovn-controller to create the chassis Encap row.
	// NOTE: ovnkube-node in DPU-host mode has no SBDB to connect to. The encap port will be handled by the
	// ovnkube-node running in DPU mode on behalf of the host.
	if (config.IsModeDPU() || config.IsModeFull()) && config.Default.EncapPort != config.DefaultEncapPort {
		if err := setEncapPort(ctx); err != nil {
			return err
		}
	}

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	// Set masquerade IP reconciliation callback for all modes except DPU.
	// DPU mode does not configure masquerade resources.
	if !config.IsModeDPU() {
		gw, ok := nc.Gateway.(*gateway)
		if !ok {
			return fmt.Errorf("unexpected gateway type %T, expected *gateway", nc.Gateway)
		}
		if gw.nodeIPManager == nil {
			return fmt.Errorf("node IP manager not initialized for gateway")
		}
		gw.nodeIPManager.OnMasqueradeIPChanged = func() {
			if err := nc.masqReconciler.ensure(); err != nil {
				klog.Errorf("Masquerade reconciler on masquerade IP change: %v", err)
			}
		}
	}
	err = nc.Gateway.Start()
	if err != nil {
		return fmt.Errorf("failed to start gateway: %w", err)
	}
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	// Note(adrianc): DPU deployments are expected to support the new shared gateway changes, upgrade flow
	// is not needed. Future upgrade flows will need to take DPUs into account.
	if config.IsModeDPU() || config.IsModeFull() {
		if config.IsModeFull() {
			// Configure route for svc towards shared gateway interface
			if err := configureSvcRouteViaInterface(nc.routeManager, nc.Gateway.GetGatewayIface(), DummyNextHopIPs()); err != nil {
				return err
			}
		}
	}

	if config.HybridOverlay.Enabled {
		// Not supported with DPUs, enforced in config
		// TODO(adrianc): Revisit above comment
		nodeController, err := honode.NewNode(
			nc.Kube,
			nc.name,
			nc.watchFactory.NodeInformer(),
			nc.watchFactory.PodInformer(),
			informer.NewDefaultEventHandler,
			false,
		)
		if err != nil {
			return err
		}
		nc.wg.Add(1)
		go func(stopCh <-chan struct{}) {
			defer nc.wg.Done()
			nodeController.Run(stopCh)
		}(nc.stopChan)
	} else if config.IsModeDPU() || config.IsModeFull() {
		// attempt to cleanup the possibly stale bridge
		if err := ovsops.DeleteBridge(nc.ovsClient, "br-ext"); err != nil {
			klog.Errorf("Deletion of bridge br-ext failed: %v", err)
		}
		if err := ovsops.DeletePortWithInterfaces(nc.ovsClient, "br-int", "int"); err != nil {
			klog.Errorf("Deletion of port int on br-int failed: %v", err)
		}
	}

	// start management port controller
	err = nc.mgmtPortController.Start(nc.stopChan)
	if err != nil {
		return fmt.Errorf("failed to start management port controller: %w", err)
	}
	if config.OVNKubernetesFeature.EnableEgressIP {
		// Start the health checking server used by egressip, if EgressIPNodeHealthCheckPort is specified
		if err := nc.startEgressIPHealthCheckingServer(nc.mgmtPortController); err != nil {
			return err
		}
	}

	if config.IsModeDPU() || config.IsModeFull() {
		err = nc.WatchEndpointSlices()
		if err != nil {
			return fmt.Errorf("failed to watch endpointSlices: %w", err)
		}
		err = nc.WatchNodes()
		if err != nil {
			return fmt.Errorf("failed to watch nodes: %w", err)
		}
	}

	if nc.healthzServer != nil {
		nc.healthzServer.Start(nc.stopChan, nc.wg)
	}

	if nc.dpuNodeLeaseManager != nil {
		if config.IsModeDPU() {
			nc.wg.Add(1)
			go func() {
				defer nc.wg.Done()
				nc.dpuNodeLeaseManager.RunUpdater(ctx)
			}()
		} else if config.IsModeDPUHost() {
			if err := nc.dpuNodeLeaseManager.CheckStatus(ctx); err != nil {
				klog.Warningf("Initial DPU node lease check failed: %v", err)
			}
			nc.wg.Add(1)
			go func() {
				defer nc.wg.Done()
				nc.dpuNodeLeaseManager.RunMonitor(ctx)
			}()
		}
	}

	if !config.IsModeDPU() {
		// start the cni server
		if nc.cniServer != nil {
			if err := nc.cniServer.Start(cni.ServerRunDir); err != nil {
				return err
			}
		}

		// Write CNI config file if it doesn't already exist
		if err := config.WriteCNIConfig(); err != nil {
			return err
		}
	}

	// configure NFT/IPT rules for egressService
	if config.OVNKubernetesFeature.EnableEgressService && (config.IsModeDPUHost() || config.IsModeFull()) {
		wf := nc.watchFactory.(*factory.WatchFactory)
		c, err := egressservice.NewController(nc.stopChan, nodetypes.OvnKubeNodeSNATMark, nc.name,
			wf.EgressServiceInformer(), wf.ServiceInformer(), wf.EndpointSliceInformer(), wf.PodInformer())
		if err != nil {
			return err
		}
		if err = c.Run(nc.wg, 1); err != nil {
			return err
		}
	}
	if config.OVNKubernetesFeature.EnableMultiExternalGateway {
		if err = nc.apbExternalRouteNodeController.Run(nc.wg, 1); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressIP && !util.PlatformTypeIsEgressIPCloudProvider() {
		c, err := egressip.NewController(nc.Kube, nc.watchFactory.EgressIPInformer(), nc.watchFactory.NodeInformer(),
			nc.watchFactory.NamespaceInformer(), nc.watchFactory.PodCoreInformer(), nc.networkManager.GetActiveNetworkForNamespace,
			nc.routeManager, config.IPv4Mode, config.IPv6Mode, nc.name, nc.linkManager)
		if err != nil {
			return fmt.Errorf("failed to create egress IP controller: %v", err)
		}
		if err = c.Run(nc.stopChan, nc.wg, 1); err != nil {
			return fmt.Errorf("failed to run egress IP controller: %v", err)
		}
	} else {
		klog.Infof("Egress IP for secondary host network is disabled")
	}

	nc.linkManager.Run(nc.stopChan, nc.wg)

	nc.wg.Add(1)
	go func(stopCh <-chan struct{}) {
		defer nc.wg.Done()
		podResClient, err := podresourcesapi.New()
		if err != nil {
			klog.Errorf("Failed to initialize PodResourcesAPI client: %v", err)
			return
		}
		defer func() {
			if err := podResClient.Close(); err != nil {
				klog.V(4).Infof("Error closing PodResourcesAPI client: %v", err)
			}
		}()
		ovspinning.Run(ctx, stopCh, podResClient, nc.ovsClient)
	}(nc.stopChan)

	klog.Infof("Default node network controller initialized and ready.")
	return nil
}

// Stop gracefully stops the controller
// deleteLogicalEntities will never be true for default network
func (nc *DefaultNodeNetworkController) Stop() {
	if nc.stopChan == nil {
		klog.Infof("Default node network controller is already stopped")
		return
	}
	close(nc.stopChan)
	nc.stopChan = nil
	nc.wg.Wait()
}

func (nc *DefaultNodeNetworkController) startEgressIPHealthCheckingServer(mgmtPort managementport.Interface) error {
	healthCheckPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
	if healthCheckPort == 0 {
		klog.Infof("Egress IP health check server skipped: no port specified")
		return nil
	}

	ifName := mgmtPort.GetInterfaceName()
	mgmtAddresses := mgmtPort.GetAddresses()
	if len(mgmtAddresses) == 0 {
		return fmt.Errorf("unable to start Egress IP health checking server on interface %s: no mgmt ip", ifName)
	}

	mgmtAddress := mgmtAddresses[0]
	if err := ip.SettleAddresses(ifName, 10*time.Second); err != nil {
		return fmt.Errorf("failed to start Egress IP health checking server due to unsettled IPv6: %w on interface %s", err, ifName)
	}

	healthServer, err := healthcheck.NewEgressIPHealthServer(mgmtAddress.IP, healthCheckPort)
	if err != nil {
		return fmt.Errorf("unable to allocate health checking server: %v", err)
	}

	nc.wg.Add(1)
	go func(stopCh <-chan struct{}) {
		defer nc.wg.Done()
		healthServer.Run(stopCh)
	}(nc.stopChan)
	return nil
}

func (nc *DefaultNodeNetworkController) reconcileConntrackUponEndpointSliceEvents(oldEndpointSlice, newEndpointSlice *discovery.EndpointSlice) error {
	var errors []error
	if oldEndpointSlice == nil {
		// nothing to do upon an add event
		return nil
	}
	namespacedName, err := util.ServiceNamespacedNameFromEndpointSlice(oldEndpointSlice)
	if err != nil {
		return fmt.Errorf("cannot reconcile conntrack: %v", err)
	}
	svc, err := nc.watchFactory.GetService(namespacedName.Namespace, namespacedName.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(5).Infof("Service %s/%s not found (might have been deleted) when reconciling conntrack for endpointslice %s",
				namespacedName.Namespace, namespacedName.Name, oldEndpointSlice.Name)
			// service is not found, likely deleted, flushing service conntrack entries will be handled at service reconciliation. No-op here.
			return nil
		}
		return fmt.Errorf("error while retrieving service for endpointslice %s/%s when reconciling conntrack: %v",
			oldEndpointSlice.Namespace, oldEndpointSlice.Name, err)
	}
	for _, oldPort := range oldEndpointSlice.Ports {
		if *oldPort.Protocol != corev1.ProtocolUDP { // flush conntrack only for UDP
			continue
		}
		for _, oldEndpoint := range oldEndpointSlice.Endpoints {
			for _, oldIP := range oldEndpoint.Addresses {
				oldIPStr := utilnet.ParseIPSloppy(oldIP).String()
				// upon an update event, remove conntrack entries for IP addresses that are no longer
				// in the endpointslice, skip otherwise
				if newEndpointSlice != nil && util.DoesEndpointSliceContainEligibleEndpoint(newEndpointSlice, oldIPStr, *oldPort.Port, *oldPort.Protocol, svc) {
					continue
				}
				portName := ""
				if oldPort.Name != nil {
					portName = *oldPort.Name
				}
				servicePort, err := util.FindServicePortForEndpointSlicePort(svc, portName, *oldPort.Protocol)
				if err != nil {
					klog.Errorf("Failed to get service port for endpoint %s: %v", oldIPStr, err)
					continue
				}
				// upon update and delete events, flush UDP conntrack for Service port
				if _, err := util.DeleteConntrackServicePort(oldIPStr, servicePort.Port, *oldPort.Protocol,
					netlink.ConntrackReplyAnyIP, nil); err != nil {
					klog.Errorf("Failed to delete conntrack entry for %s port %d: %v", oldIPStr, servicePort.Port, err)
					errors = append(errors, err)
				}

				// Flush UDP conntrack entries for NodePort (and LoadBalancer services that allocate NodePorts)
				// TODO: Once vishvananda/netlink support ConntrackFilterType '--reply-port-src', we can use one DeleteConntrackServicePort() call
				//       conntrack entries for both ClusterIP and NodePort.
				if util.ServiceTypeHasNodePort(svc) && servicePort.NodePort > 0 {
					if _, err := util.DeleteConntrackServicePort(oldIPStr, servicePort.NodePort, *oldPort.Protocol,
						netlink.ConntrackReplyAnyIP, nil); err != nil {
						klog.Errorf("Failed to delete conntrack entry for %s NodePort %d: %v", oldIPStr, servicePort.NodePort, err)
						errors = append(errors, err)
					}
				}
			}
		}
	}
	return utilerrors.Join(errors...)

}
func (nc *DefaultNodeNetworkController) WatchEndpointSlices() error {
	if util.IsNetworkSegmentationSupportEnabled() {
		// Filter out objects without the default serviceName label to exclude mirrored EndpointSlices
		// Only default EndpointSlices contain the discovery.LabelServiceName label
		req, err := labels.NewRequirement(discovery.LabelServiceName, selection.Exists, nil)
		if err != nil {
			return err
		}
		_, err = nc.retryEndpointSlices.WatchResourceFiltered("", labels.NewSelector().Add(*req))
		return err
	}
	_, err := nc.retryEndpointSlices.WatchResource()
	return err
}

func (nc *DefaultNodeNetworkController) WatchNodes() error {
	_, err := nc.retryNodes.WatchResource()
	return err
}

// addOrUpdateNode handles creating flows or nftables rules for each node to handle PMTUD
func (nc *DefaultNodeNetworkController) addOrUpdateNode(node *corev1.Node) error {
	var nftElems []knftables.Object
	var addrs []string

	// Use GetNodeAddresses to get all node IPs (including current node for openflow)
	ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
	if err != nil {
		return fmt.Errorf("failed to get node addresses for node %q: %w", node.Name, err)
	}

	// Process IPv4 addresses
	for _, nodeIP := range ipsv4 {
		addrs = append(addrs, nodeIP.String())
		klog.Infof("Adding remote node %q, IP: %s to PMTUD blocking rules", node.Name, nodeIP)
		// Only add to nftables if this is remote node
		if (config.IsModeDPUHost() || config.IsModeFull()) && node.Name != nc.name {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		}
	}

	// Process IPv6 addresses
	for _, nodeIP := range ipsv6 {
		addrs = append(addrs, nodeIP.String())
		klog.Infof("Adding remote node %q, IP: %s to PMTUD blocking rules", node.Name, nodeIP)
		// Only add to nftables if this is remote node
		if (config.IsModeDPUHost() || config.IsModeFull()) && node.Name != nc.name {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}
	if (config.IsModeDPUHost() || config.IsModeFull()) && len(nftElems) > 0 {
		if err := nodenft.AddObjects(nftElems); err != nil {
			return fmt.Errorf("unable to update NFT elements for node %q, error: %w", node.Name, err)
		}
	}

	gw := nc.Gateway.(*gateway)
	gw.openflowManager.updateBridgePMTUDFlowCache(getPMTUDKey(node.Name), addrs)

	return nil
}

func removePMTUDNodeNFTRules(nodeIPs []net.IP) error {
	var nftElems []knftables.Object
	for _, nodeIP := range nodeIPs {
		// Remove IPs from NFT sets
		if utilnet.IsIPv4(nodeIP) {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		} else {
			nftElems = append(nftElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}
	if len(nftElems) > 0 {
		if err := nodenft.DeleteObjects(nftElems); err != nil {
			return fmt.Errorf("unable to remove PMTUD rules for nodes: %w", err)
		}
	}
	return nil
}

// ownNodeDeletedExit terminates ovnkube-node once the Node object it runs on
// behalf of is deleted; a variable so that unit tests can intercept the exit.
var ownNodeDeletedExit = func(nodeName string) {
	klog.Errorf("Node %q that this ovnkube-node instance runs on behalf of was deleted; exiting so that "+
		"the restarted process re-initializes against the current Node object", nodeName)
	klog.FlushAndExit(klog.ExitFlushTimeout, 1)
}

func (nc *DefaultNodeNetworkController) deleteNode(node *corev1.Node) {
	if config.IsModeDPU() && node.Name == nc.name {
		// In DPU mode the process does not run on the deleted node, so nothing
		// else stops it, and what it wrote to the Node object is only written at
		// startup: exit and let the container restart against the new node.
		ownNodeDeletedExit(nc.name)
		return
	}

	gw := nc.Gateway.(*gateway)
	gw.openflowManager.deleteFlowsByKey(getPMTUDKey(node.Name))

	// Use GetNodeAddresses to get node IPs
	ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
	if err != nil {
		klog.Errorf("Failed to get node addresses for node %q: %v", node.Name, err)
		return
	}

	ipsToRemove := make([]net.IP, 0, len(ipsv4)+len(ipsv6))
	ipsToRemove = append(ipsToRemove, ipsv4...)
	ipsToRemove = append(ipsToRemove, ipsv6...)

	klog.Infof("Deleting NFT elements for node: %s", node.Name)
	if err := removePMTUDNodeNFTRules(ipsToRemove); err != nil {
		klog.Errorf("Failed to delete nftables rules for PMTUD blocking for node %q: %v", node.Name, err)
	}
}

var nodeIPSets = []knftables.Object{
	&knftables.Set{
		Name: types.NFTRemoteNodeIPsv4,
	},
	&knftables.Set{
		Name: types.NFTRemoteNodeIPsv6,
	},
}

func (nc *DefaultNodeNetworkController) syncNodes(objs []interface{}) error {
	var keepNFTSetElems []knftables.Object

	if config.IsModeDPU() {
		return nil
	}

	klog.Infof("Starting node controller node sync")
	start := time.Now()
	for _, obj := range objs {
		node, ok := obj.(*corev1.Node)
		if !ok {
			klog.Errorf("Spurious object in syncNodes: %v", obj)
			continue
		}
		if node.Name == nc.name {
			continue
		}

		// Use GetNodeAddresses to get node IPs
		ipsv4, ipsv6, err := util.GetNodeAddresses(config.IPv4Mode, config.IPv6Mode, node)
		if err != nil {
			klog.Errorf("Failed to get node addresses for node %q: %v", node.Name, err)
			continue
		}

		// Process IPv4 addresses
		for _, nodeIP := range ipsv4 {
			keepNFTSetElems = append(keepNFTSetElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv4,
				Key: []string{nodeIP.String()},
			})
		}

		// Process IPv6 addresses
		for _, nodeIP := range ipsv6 {
			keepNFTSetElems = append(keepNFTSetElems, &knftables.Element{
				Set: types.NFTRemoteNodeIPsv6,
				Key: []string{nodeIP.String()},
			})
		}
	}
	err := nodenft.SyncObjects(nodeIPSets, keepNFTSetElems)
	if err != nil {
		err = fmt.Errorf("unable to sync nftables rules for nodes: %w", err)
	}

	klog.Infof("Node controller node sync done. Time taken: %s", time.Since(start))
	return err
}

// validateVTEPInterfaceMTU checks if the MTU of the interface that has ovn-encap-ip is big
// enough to carry the `config.Default.MTU` and the Geneve header (if overlay transport is used).
// If the MTU is not big enough, it will return an error
func (nc *DefaultNodeNetworkController) validateVTEPInterfaceMTU() error {
	// calc required MTU
	var requiredMTU int
	if config.Gateway.SingleNode || config.Default.Transport == types.NetworkTransportNoOverlay {
		requiredMTU = config.Default.MTU
	} else {
		if config.IPv4Mode && !config.IPv6Mode {
			// we run in single-stack IPv4 only
			requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv4
		} else {
			// we run in single-stack IPv6 or dual-stack mode
			requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv6
		}
	}

	// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma
	ovnEncapIps := strings.Split(config.Default.EffectiveEncapIP, ",")
	for _, ip := range ovnEncapIps {
		ovnEncapIP := net.ParseIP(strings.TrimSpace(ip))
		if ovnEncapIP == nil {
			return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIP, config.Default.EffectiveEncapIP)
		}
		interfaceName, mtu, err := util.GetIFNameAndMTUForAddress(ovnEncapIP)
		if err != nil {
			return fmt.Errorf("could not get MTU for the interface with address %s: %w", ovnEncapIP, err)
		}

		if mtu < requiredMTU {
			return fmt.Errorf("MTU (%d) of network interface %s is too small for specified overlay MTU (%d)",
				mtu, interfaceName, requiredMTU)
		}
		klog.V(2).Infof("MTU (%d) of network interface %s is big enough to deal with Geneve header overhead (sum %d). ",
			mtu, interfaceName, requiredMTU)
	}
	return nil
}

func getPMTUDKey(nodeName string) string {
	return fmt.Sprintf("%s_pmtud", nodeName)
}

// DummyNextHopIPs returns the fake next hops used for service traffic routing.
// It is used in:
// - br-ex, where we don't really care about the next hop GW in use as traffic is always routed to OVN
// - OVN, only when there is no default GW as it wouldn't matter since there is no external traffic
func DummyNextHopIPs() []net.IP {
	var nextHops []net.IP
	if config.IPv4Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V4DummyNextHopMasqueradeIP)
	}
	if config.IPv6Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V6DummyNextHopMasqueradeIP)
	}
	return nextHops
}

// DummyMasqueradeIPs returns the fake host masquerade IPs used for service traffic routing.
// It is used in: br-ex, where we SNAT the traffic destined towards a service IP
func DummyMasqueradeIPs() []net.IP {
	var nextHops []net.IP
	if config.IPv4Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V4HostMasqueradeIP)
	}
	if config.IPv6Mode {
		nextHops = append(nextHops, config.Gateway.MasqueradeIPs.V6HostMasqueradeIP)
	}
	return nextHops
}

// configureGlobalForwarding configures the global forwarding settings for IPv6 when
// per-interface forwarding is not available. It enables global forwarding, and sets the
// ip6tables FORWARD policy to DROP if config.Gateway.DisableForwarding is set (or sets it
// back to ACCEPT if it had previously set it DROP and DisableForwarding is now unset).
//
// This function assumes that other forwarding setup will also be performed. Specifically,
// you must call util.SetForwardingModeForInterface() to enable per-interface
// forwarding on the interfaces that need it (the bridge and management port interfaces).
// And if config.Gateway.DisableForwarding is set, you must call
// initExternalBridgeServiceForwardingRules() to create netfilter rules to forward the
// traffic that OVN-Kubernetes needs, and block other traffic from being forwarded.
func configureGlobalForwarding() error {
	if config.IPv6Mode && !util.SupportsIPv6InterfaceForwarding() {
		// Enable global forwarding since per-interface forwarding isn't available.
		if err := ip.EnableIP6Forward(); err != nil {
			return fmt.Errorf("could not set the correct global forwarding value for ipv6:  %w", err)
		}

	}

	for _, proto := range clusterIPTablesProtocols() {
		ipt, err := util.GetIPTablesHelper(proto)
		if err != nil {
			return fmt.Errorf("failed to get the iptables helper: %w", err)
		}

		desiredPolicy := ""
		if nodeutil.NeedIPTablesForwardingRules(proto) {
			desiredPolicy = "DROP"
		} else {
			// If there's evidence that we previously configured
			// DisableForwarding, then change the policy back to ACCEPT now.
			// (Note that the rules we look for here will be deleted later
			// by the gateway setup.)
			masqueradeIP := config.Gateway.MasqueradeIPs.V4OVNMasqueradeIP
			if proto == iptables.ProtocolIPv6 {
				masqueradeIP = config.Gateway.MasqueradeIPs.V6OVNMasqueradeIP
			}
			forwardRules := getMasqueradeIpTablesForwardRules(masqueradeIP, proto)
			if len(forwardRules) != 0 {
				rule := forwardRules[0]
				if ruleExists, _ := ipt.Exists(rule.Table, rule.Chain, rule.Args...); ruleExists {
					desiredPolicy = "ACCEPT"
				}
			}
		}

		if desiredPolicy != "" {
			if err := ipt.ChangePolicy("filter", "FORWARD", desiredPolicy); err != nil {
				return fmt.Errorf("failed to change the forward policy to %q: %w", desiredPolicy, err)
			}
		}
	}

	return nil
}

// GetNodeAddressManager returns the node's address manager from the gateway,
// available after Init() has completed. Returns nil if the gateway has not
// been initialized or is not the expected type.
func (nc *DefaultNodeNetworkController) GetNodeAddressManager() *addressManager {
	gw, ok := nc.Gateway.(*gateway)
	if !ok || gw == nil {
		return nil
	}
	return gw.nodeIPManager
}
