package node

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	kapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	honode "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/upgrade"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
)

// OvnNode is the object holder for utilities meant for node management
type OvnNode struct {
	name         string
	client       clientset.Interface
	Kube         kube.Interface
	watchFactory factory.NodeWatchFactory
	stopChan     chan struct{}
	recorder     record.EventRecorder
	gateway      Gateway
	sbClient     libovsdbclient.Client
}

// NewNode creates a new controller for node management
func NewNode(kubeClient clientset.Interface, wf factory.NodeWatchFactory, name string, dbclient libovsdbclient.Client, stopChan chan struct{}, eventRecorder record.EventRecorder) *OvnNode {
	return &OvnNode{
		name:         name,
		client:       kubeClient,
		Kube:         &kube.Kube{KClient: kubeClient},
		watchFactory: wf,
		stopChan:     stopChan,
		recorder:     eventRecorder,
		sbClient:     dbclient,
	}
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
func collectorsString(node *kapi.Node, targets []config.HostPort) (string, error) {
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

func setOVSFlowTargets(node *kapi.Node) error {
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

func setupOVNNode(node *kapi.Node) error {
	var err error

	encapIP := config.Default.EncapIP
	if encapIP == "" {
		encapIP, err = util.GetNodePrimaryIP(node)
		if err != nil {
			return fmt.Errorf("failed to obtain local IP from node %q: %v", node.Name, err)
		}
		config.Default.EncapIP = encapIP
	} else {
		if ip := net.ParseIP(encapIP); ip == nil {
			return fmt.Errorf("invalid encapsulation IP provided %q", encapIP)
		}
	}

	setExternalIdsCmd := []string{
		"set",
		"Open_vSwitch",
		".",
		fmt.Sprintf("external_ids:ovn-encap-type=%s", config.Default.EncapType),
		fmt.Sprintf("external_ids:ovn-encap-ip=%s", encapIP),
		fmt.Sprintf("external_ids:ovn-remote-probe-interval=%d",
			config.Default.InactivityProbe),
		fmt.Sprintf("external_ids:ovn-openflow-probe-interval=%d",
			config.Default.OpenFlowProbe),
		fmt.Sprintf("external_ids:hostname=\"%s\"", node.Name),
		fmt.Sprintf("external_ids:ovn-monitor-all=%t", config.Default.MonitorAll),
		fmt.Sprintf("external_ids:ovn-ofctrl-wait-before-clear=%d", config.Default.OfctrlWaitBeforeClear),
		fmt.Sprintf("external_ids:ovn-enable-lflow-cache=%t", config.Default.LFlowCacheEnable),
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

	_, stderr, err := util.RunOVSVsctl(setExternalIdsCmd...)
	if err != nil {
		return fmt.Errorf("error setting OVS external IDs: %v\n  %q", err, stderr)
	}
	// If EncapPort is not the default tell sbdb to use specified port.
	if config.Default.EncapPort != config.DefaultEncapPort {
		systemID, err := util.GetNodeChassisID()
		if err != nil {
			return err
		}
		uuid, _, err := util.RunOVNSbctl("--data=bare", "--no-heading", "--columns=_uuid", "find", "Encap",
			fmt.Sprintf("chassis_name=%s", systemID))
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

func isOVNControllerReady() (bool, error) {
	// check node's connection status
	runDir := util.GetOvnRunDir()
	pid, err := ioutil.ReadFile(runDir + "ovn-controller.pid")
	if err != nil {
		return false, fmt.Errorf("unknown pid for ovn-controller process: %v", err)
	}
	ctlFile := runDir + fmt.Sprintf("ovn-controller.%s.ctl", strings.TrimSuffix(string(pid), "\n"))
	ret, _, err := util.RunOVSAppctl("-t", ctlFile, "connection-status")
	if err != nil {
		return false, fmt.Errorf("could not get connection status: %w", err)
	}
	klog.Infof("Node connection status = %s", ret)
	if ret != "connected" {
		return false, nil
	}

	// check whether br-int exists on node
	_, _, err = util.RunOVSVsctl("--", "br-exists", "br-int")
	if err != nil {
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

// Starting with v21.03.0 OVN sets OVS.Interface.external-id:ovn-installed
// and OVNSB.Port_Binding.up when all OVS flows associated to a
// logical port have been successfully programmed.
// OVS.Interface.external-id:ovn-installed can only be used correctly
// in a combination with OVS.Interface.external-id:iface-id-ver
func getOVNIfUpCheckMode() (bool, error) {
	if config.OvnKubeNode.DisableOVNIfaceIdVer {
		klog.Infof("'iface-id-ver' is manually disabled, ovn-installed feature can't be used")
		return false, nil
	}
	if _, stderr, err := util.RunOVNSbctl("--columns=up", "list", "Port_Binding"); err != nil {
		if strings.Contains(stderr, "does not contain a column") {
			klog.Infof("Falling back to using legacy OVS flow readiness checks")
			return false, nil
		}
		return false, fmt.Errorf("failed to check if port_binding is supported in OVN, stderr: %q, error: %v",
			stderr, err)
	}
	klog.Infof("Detected support for port binding with external IDs")
	return true, nil
}

// Start learns the subnets assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (n *OvnNode) Start(ctx context.Context, wg *sync.WaitGroup) error {
	var err error
	var node *kapi.Node
	var subnets []*net.IPNet
	var mgmtPort ManagementPort
	var mgmtPortConfig *managementPortConfig
	var cniServer *cni.Server
	var isOvnUpEnabled bool

	klog.Infof("OVN Kube Node initialization, Mode: %s", config.OvnKubeNode.Mode)

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("Setting klog \"loglevel\" to 5 failed, err: %v", err)
	}

	// Start and sync the watch factory to begin listening for events
	if err := n.watchFactory.Start(); err != nil {
		return err
	}

	if node, err = n.Kube.GetNode(n.name); err != nil {
		return fmt.Errorf("error retrieving node %s: %v", n.name, err)
	}

	nodeAddrStr, err := util.GetNodePrimaryIP(node)
	if err != nil {
		return err
	}
	nodeAddr := net.ParseIP(nodeAddrStr)
	if nodeAddr == nil {
		return fmt.Errorf("failed to parse kubernetes node IP address. %v", err)
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
			if err := auth.SetDBAuth(); err != nil {
				return err
			}
		}

		err = setupOVNNode(node)
		if err != nil {
			return err
		}
	} else {
		err = removeStaleChassisByNodeIP(n.sbClient, nodeAddr)
		if err != nil {
			return err
		}
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
		if node, err = n.Kube.GetNode(n.name); err != nil {
			klog.Infof("Waiting to retrieve node %s: %v", n.name, err)
			return false, nil
		}
		subnets, err = util.ParseNodeHostSubnetAnnotation(node)
		if err != nil {
			klog.Infof("Waiting for node %s to start, no annotation found on node for subnet: %v", n.name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for node's: %q logical switch: %v", n.name, err)
	}
	klog.Infof("Node %s ready for ovn initialization with subnet %s", n.name, util.JoinIPNets(subnets, ","))

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		isOvnUpEnabled, err = getOVNIfUpCheckMode()
		if err != nil {
			return err
		}
	}

	// Create CNI Server
	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		kclient, ok := n.Kube.(*kube.Kube)
		if !ok {
			return fmt.Errorf("cannot get kubeclient for starting CNI server")
		}
		cniServer, err = cni.NewCNIServer("", isOvnUpEnabled, n.watchFactory, kclient.KClient)
		if err != nil {
			return err
		}
	}

	// Setup Management port and gateway
	mgmtPort = NewManagementPort(n.name, subnets)
	nodeAnnotator := kube.NewNodeAnnotator(n.Kube, node.Name)
	waiter := newStartupWaiter()

	mgmtPortConfig, err = mgmtPort.Create(nodeAnnotator, waiter)
	if err != nil {
		return err
	}

	// Initialize gateway
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		err = n.initGatewayDPUHost(nodeAddr)
		if err != nil {
			return err
		}
	} else {
		if err := n.initGateway(subnets, nodeAnnotator, waiter, mgmtPortConfig, nodeAddr); err != nil {
			return err
		}
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %v", n.name, err)
	}

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	n.gateway.Start(n.stopChan, wg)
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	// Note(adrianc): DPU deployments are expected to support the new shared gateway changes, upgrade flow
	// is not needed. Future upgrade flows will need to take DPUs into account.
	if config.OvnKubeNode.Mode == types.NodeModeFull {
		// Upgrade for Node. If we upgrade workers before masters, then we need to keep service routing via
		// mgmt port until masters have been updated and modified OVN config. Run a goroutine to handle this case
		upgradeController := upgrade.NewController(n.client, n.watchFactory)
		initialTopoVersion, err := upgradeController.GetTopologyVersion(ctx)
		if err != nil {
			return fmt.Errorf("failed to get initial topology version: %w", err)
		}
		klog.Infof("Current control-plane topology version is %d", initialTopoVersion)
		bridgeName := n.gateway.GetGatewayBridgeIface()

		needLegacySvcRoute := true
		if (initialTopoVersion >= types.OvnHostToSvcOFTopoVersion && config.GatewayModeShared == config.Gateway.Mode) ||
			(initialTopoVersion >= types.OvnRoutingViaHostTopoVersion) {
			// Configure route for svc towards shared gw bridge
			// Have to have the route to bridge for multi-NIC mode, where the default gateway may go to a non-OVS interface
			if err := configureSvcRouteViaBridge(bridgeName); err != nil {
				return err
			}
			needLegacySvcRoute = false
		}

		// Determine if we need to run upgrade checks
		if initialTopoVersion != types.OvnCurrentTopologyVersion {
			if needLegacySvcRoute {
				klog.Info("System may be upgrading, falling back to to legacy K8S Service via mp0")
				// add back legacy route for service via mp0
				link, err := util.LinkSetUp(types.K8sMgmtIntfName)
				if err != nil {
					return fmt.Errorf("unable to get link for %s, error: %v", types.K8sMgmtIntfName, err)
				}
				var gwIP net.IP
				for _, subnet := range config.Kubernetes.ServiceCIDRs {
					if utilnet.IsIPv4CIDR(subnet) {
						gwIP = mgmtPortConfig.ipv4.gwIP
					} else {
						gwIP = mgmtPortConfig.ipv6.gwIP
					}
					err := util.LinkRoutesAddOrUpdateMTU(link, gwIP, []*net.IPNet{subnet}, config.Default.RoutableMTU)
					if err != nil {
						return fmt.Errorf("unable to add legacy route for services via mp0, error: %v", err)
					}
				}
			}
			// need to run upgrade controller
			go func() {
				if err := upgradeController.WaitForTopologyVersion(ctx, types.OvnCurrentTopologyVersion, 30*time.Minute); err != nil {
					klog.Fatalf("Error while waiting for Topology Version to be updated: %v", err)
				}
				// upgrade complete now see what needs upgrading
				// migrate service route from ovn-k8s-mp0 to shared gw bridge
				if (initialTopoVersion < types.OvnHostToSvcOFTopoVersion && config.GatewayModeShared == config.Gateway.Mode) ||
					(initialTopoVersion < types.OvnRoutingViaHostTopoVersion) {
					if err := upgradeServiceRoute(bridgeName); err != nil {
						klog.Fatalf("Failed to upgrade service route for node, error: %v", err)
					}
				}
				// ensure CNI support for port binding built into OVN, as masters have been upgraded
				if initialTopoVersion < types.OvnPortBindingTopoVersion && cniServer != nil && !isOvnUpEnabled {
					isOvnUpEnabled, err = getOVNIfUpCheckMode()
					if err != nil {
						klog.Errorf("%v", err)
					}
					if isOvnUpEnabled {
						cniServer.EnableOVNPortUpSupport()
					}
				}
			}()
		}
	}

	if config.HybridOverlay.Enabled {
		// Not supported with DPUs, enforced in config
		// TODO(adrianc): Revisit above comment
		nodeController, err := honode.NewNode(
			n.Kube,
			n.name,
			n.watchFactory.NodeInformer(),
			n.watchFactory.LocalPodInformer(),
			informer.NewDefaultEventHandler,
		)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			nodeController.Run(n.stopChan)
		}()
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("Reset of initial klog \"loglevel\" failed, err: %v", err)
	}

	// start management port health check
	mgmtPort.CheckManagementPortHealth(mgmtPortConfig, n.stopChan)

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// start health check to ensure there are no stale OVS internal ports
		go wait.Until(func() {
			checkForStaleOVSInterfaces(n.name, n.watchFactory.(*factory.WatchFactory))
		}, time.Minute, n.stopChan)
		err := n.WatchEndpoints()
		if err != nil {
			return fmt.Errorf("failed to watch endpoints: %w", err)
		}
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		if err := n.watchPodsDPU(isOvnUpEnabled); err != nil {
			return err
		}
	} else {
		// start the cni server
		if err := cniServer.Start(cni.HandleCNIRequest); err != nil {
			return err
		}

		// Write CNI config file if it doesn't already exist
		if err := config.WriteCNIConfig(); err != nil {
			return err
		}
	}

	klog.Infof("OVN Kube Node initialized and ready.")
	return nil
}

func (n *OvnNode) WatchEndpoints() error {
	_, err := n.watchFactory.AddEndpointsHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, new interface{}) {
			epNew := new.(*kapi.Endpoints)
			epOld := old.(*kapi.Endpoints)
			newEpAddressMap := buildEndpointAddressMap(epNew.Subsets)
			for item := range buildEndpointAddressMap(epOld.Subsets) {
				if _, ok := newEpAddressMap[item]; !ok && item.protocol == kapi.ProtocolUDP { // flush conntrack only for UDP
					err := util.DeleteConntrack(item.ip, item.port, item.protocol, netlink.ConntrackReplyAnyIP)
					if err != nil {
						klog.Errorf("Failed to delete conntrack entry for %s: %v", item.ip, err)
					}
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			ep := obj.(*kapi.Endpoints)
			for item := range buildEndpointAddressMap(ep.Subsets) {
				if item.protocol == kapi.ProtocolUDP { // flush conntrack only for UDP
					err := util.DeleteConntrack(item.ip, item.port, item.protocol, netlink.ConntrackReplyAnyIP)
					if err != nil {
						klog.Errorf("Failed to delete conntrack entry for %s: %v", item.ip, err)
					}
				}
			}
		},
	}, nil)
	return err
}

// validateVTEPInterfaceMTU checks if the MTU of the interface that has ovn-encap-ip is big
// enough to carry the `config.Default.MTU` and the Geneve header. If the MTU is not big
// enough, it will return an error
func (n *OvnNode) validateVTEPInterfaceMTU() error {
	ovnEncapIP := net.ParseIP(config.Default.EncapIP)
	if ovnEncapIP == nil {
		return fmt.Errorf("the set OVN Encap IP is invalid: (%s)", config.Default.EncapIP)
	}
	interfaceName, mtu, err := util.GetIFNameAndMTUForAddress(ovnEncapIP)
	if err != nil {
		return fmt.Errorf("could not get MTU for the interface with address %s: %w", ovnEncapIP, err)
	}

	// calc required MTU
	var requiredMTU int
	if config.IPv4Mode && !config.IPv6Mode {
		// we run in single-stack IPv4 only
		requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv4
	} else {
		// we run in single-stack IPv6 or dual-stack mode
		requiredMTU = config.Default.MTU + types.GeneveHeaderLengthIPv6
	}

	if mtu < requiredMTU {
		return fmt.Errorf("interface MTU (%d) is too small for specified overlay MTU (%d)", mtu, requiredMTU)
	}
	klog.V(2).Infof("MTU (%d) of network interface %s is big enough to deal with Geneve header overhead (sum %d). ",
		mtu, interfaceName, requiredMTU)
	return nil
}

type epAddressItem struct {
	ip       string
	port     int32
	protocol kapi.Protocol
}

//buildEndpointAddressMap builds a map of all UDP and SCTP ports in the endpoint subset along with that port's IP address
func buildEndpointAddressMap(epSubsets []kapi.EndpointSubset) map[epAddressItem]struct{} {
	epMap := make(map[epAddressItem]struct{})
	for _, subset := range epSubsets {
		for _, address := range subset.Addresses {
			for _, port := range subset.Ports {
				if port.Protocol == kapi.ProtocolUDP || port.Protocol == kapi.ProtocolSCTP {
					epMap[epAddressItem{
						ip:       address.IP,
						port:     port.Port,
						protocol: port.Protocol,
					}] = struct{}{}
				}
			}
		}
	}

	return epMap
}

func configureSvcRouteViaBridge(bridge string) error {
	gwIPs, _, err := getGatewayNextHops()
	if err != nil {
		return fmt.Errorf("unable to get the gateway next hops, error: %v", err)
	}
	return configureSvcRouteViaInterface(bridge, gwIPs)
}

func upgradeServiceRoute(bridgeName string) error {
	klog.Info("Updating K8S Service route")
	// Flush old routes
	link, err := util.LinkSetUp(types.K8sMgmtIntfName)
	if err != nil {
		return fmt.Errorf("unable to get link: %s, error: %v", types.K8sMgmtIntfName, err)
	}
	if err := util.LinkRoutesDel(link, config.Kubernetes.ServiceCIDRs); err != nil {
		return fmt.Errorf("unable to delete routes on upgrade, error: %v", err)
	}
	// add route via OVS bridge
	if err := configureSvcRouteViaBridge(bridgeName); err != nil {
		return fmt.Errorf("unable to add svc route via OVS bridge interface, error: %v", err)
	}
	klog.Info("Successfully updated Kubernetes service route towards OVS")
	// Clean up gw0 and local ovs bridge as best effort
	if err := deleteLocalNodeAccessBridge(); err != nil {
		klog.Warningf("Error while removing Local Node Access Bridge, error: %v", err)
	}
	// Clean up gw0 related IPTable rules as best effort.
	for _, ip := range []string{types.V4NodeLocalNATSubnet, types.V6NodeLocalNATSubnet} {
		_, IPNet, err := net.ParseCIDR(ip)
		if err != nil {
			klog.Errorf("Failed to LocalGatewayNATRules: %v", err)
		}
		rules := getLocalGatewayNATRules(types.LocalnetGatewayNextHopPort, IPNet)
		if err := delIptRules(rules); err != nil {
			klog.Errorf("Failed to LocalGatewayNATRules: %v", err)
		}
	}
	return nil
}

func removeStaleChassisByNodeIP(sbClient libovsdbclient.Client, ip net.IP) error {
	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	encaps := []*sbdb.Encap{}
	if err := sbClient.List(ctx, &encaps); err != nil {
		return err
	}

	for _, encap := range encaps {
		if encap.IP == ip.String() {
			klog.V(2).Infof("Remove stale chassis: %s", encap.ChassisName)
			if err := libovsdbops.DeleteChassisWithPredicate(sbClient, func(item *sbdb.Chassis) bool {
				return item.Name == encap.ChassisName
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
