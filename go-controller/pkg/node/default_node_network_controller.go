package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	kapi "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	v1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	honode "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
	config "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	adminpolicybasedrouteclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressip"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/controllers/egressservice"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/linkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/ovspinning"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/routemanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/controller/apbroute"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/vishvananda/netlink"
)

type CommonNodeNetworkControllerInfo struct {
	client                 clientset.Interface
	Kube                   kube.Interface
	watchFactory           factory.NodeWatchFactory
	recorder               record.EventRecorder
	name                   string
	apbExternalRouteClient adminpolicybasedrouteclientset.Interface
}

// BaseNodeNetworkController structure per-network fields and network specific configuration
type BaseNodeNetworkController struct {
	CommonNodeNetworkControllerInfo

	// network information
	util.NetInfo

	// podNADToDPUCDMap tracks the NAD/DPU_ConnectionDetails mapping for all NADs that each pod requests.
	// Key is pod.UUID; value is nadToDPUCDMap (of map[string]*util.DPUConnectionDetails). Key of nadToDPUCDMap
	// is nadName; value is DPU_ConnectionDetails when VF representor is successfully configured for that
	// given NAD. DPU mode only
	// Note that we assume that Pod's Network Attachment Selection Annotation will not change over time.
	podNADToDPUCDMap sync.Map

	// stopChan and WaitGroup per controller
	stopChan chan struct{}
	wg       *sync.WaitGroup
}

func newCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, kube kube.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface,
	wf factory.NodeWatchFactory, eventRecorder record.EventRecorder, name string) *CommonNodeNetworkControllerInfo {

	return &CommonNodeNetworkControllerInfo{
		client:                 kubeClient,
		Kube:                   kube,
		apbExternalRouteClient: apbExternalRouteClient,
		watchFactory:           wf,
		name:                   name,
		recorder:               eventRecorder,
	}
}

// NewCommonNodeNetworkControllerInfo creates and returns the base node network controller info
func NewCommonNodeNetworkControllerInfo(kubeClient clientset.Interface, apbExternalRouteClient adminpolicybasedrouteclientset.Interface, wf factory.NodeWatchFactory,
	eventRecorder record.EventRecorder, name string) *CommonNodeNetworkControllerInfo {
	return newCommonNodeNetworkControllerInfo(kubeClient, &kube.Kube{KClient: kubeClient}, apbExternalRouteClient, wf, eventRecorder, name)
}

// DefaultNodeNetworkController is the object holder for utilities meant for node management of default network
type DefaultNodeNetworkController struct {
	BaseNodeNetworkController

	gateway Gateway
	// Node healthcheck server for cloud load balancers
	healthzServer *proxierHealthUpdater
	routeManager  *routemanager.Controller

	// retry framework for namespaces, used for the removal of stale conntrack entries for external gateways
	retryNamespaces *retry.RetryFramework
	// retry framework for endpoint slices, used for the removal of stale conntrack entries for services
	retryEndpointSlices *retry.RetryFramework

	apbExternalRouteNodeController *apbroute.ExternalGatewayNodeController
}

func newDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, stopChan chan struct{},
	wg *sync.WaitGroup) *DefaultNodeNetworkController {

	return &DefaultNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			NetInfo:                         &util.DefaultNetInfo{},
			stopChan:                        stopChan,
			wg:                              wg,
		},
		routeManager: routemanager.NewController(),
	}
}

// NewDefaultNodeNetworkController creates a new network controller for node management of the default network
func NewDefaultNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo) (*DefaultNodeNetworkController, error) {
	var err error
	stopChan := make(chan struct{})
	wg := &sync.WaitGroup{}
	nc := newDefaultNodeNetworkController(cnnci, stopChan, wg)

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

	return nc, nil
}

func (nc *DefaultNodeNetworkController) initRetryFrameworkForNode() {
	nc.retryNamespaces = nc.newRetryFrameworkNode(factory.NamespaceExGwType)
	nc.retryEndpointSlices = nc.newRetryFrameworkNode(factory.EndpointSliceForStaleConntrackRemovalType)
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
		// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma.
		ovnEncapIps := strings.Split(encapIP, ",")
		for _, ovnEncapIp := range ovnEncapIps {
			if ip := net.ParseIP(strings.TrimSpace(ovnEncapIp)); ip == nil {
				return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIp, encapIP)
			}
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
		// bundle-idle-timeout default value is 10s, it should be set
		// as high as the ovn-openflow-probe-interval to allow ovn-controller
		// to finish computation specially with complex acl configuration with port range.
		fmt.Sprintf("other_config:bundle-idle-timeout=%d",
			config.Default.OpenFlowProbe),
		// If Interconnect feature is enabled, we want to tell ovn-controller to
		// make this node/chassis as an interconnect gateway.
		fmt.Sprintf("external_ids:ovn-is-interconn=%s", strconv.FormatBool(config.OVNKubernetesFeature.EnableInterconnect)),
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
	if config.OvnKubeNode.Mode != types.NodeModeDPU {
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
		func(ctx context.Context) (bool, error) {
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

func isOVNControllerReady() (bool, error) {
	// check node's connection status
	runDir := util.GetOvnRunDir()
	pid, err := os.ReadFile(runDir + "ovn-controller.pid")
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

type managementPortEntry struct {
	port   ManagementPort
	config *managementPortConfig
}

// getEnvNameFromResourceName gets the device plugin env variable from the device plugin resource name.
func getEnvNameFromResourceName(resource string) string {
	res1 := strings.ReplaceAll(resource, ".", "_")
	res2 := strings.ReplaceAll(res1, "/", "_")
	return "PCIDEVICE_" + strings.ToUpper(res2)
}

// getDeviceIdsFromEnv gets the list of device IDs from the device plugin env variable.
func getDeviceIdsFromEnv(envName string) ([]string, error) {
	envVar := os.Getenv(envName)
	if len(envVar) == 0 {
		return nil, fmt.Errorf("unexpected empty env variable: %s", envName)
	}
	deviceIds := strings.Split(envVar, ",")
	return deviceIds, nil
}

// handleDevicePluginResources tries to retrieve any device plugin resources passed in via arguments and device plugin env variables.
func handleDevicePluginResources() error {
	mgmtPortEnvName := getEnvNameFromResourceName(config.OvnKubeNode.MgmtPortDPResourceName)
	deviceIds, err := getDeviceIdsFromEnv(mgmtPortEnvName)
	if err != nil {
		return err
	}
	// The reason why we want to store the Device Ids in a map is prepare for various features that
	// require network resources such as the Management Port or Bypass Port. It is likely that these
	// features share the same device pool.
	config.OvnKubeNode.DPResourceDeviceIdsMap = make(map[string][]string)
	config.OvnKubeNode.DPResourceDeviceIdsMap[config.OvnKubeNode.MgmtPortDPResourceName] = deviceIds
	klog.V(5).Infof("Setting DPResourceDeviceIdsMap for %s using env %s with device IDs %v",
		config.OvnKubeNode.MgmtPortDPResourceName, mgmtPortEnvName, deviceIds)
	return nil
}

// reserveDeviceId takes the first device ID from a list of device IDs
// This function will not execute during runtime, only once at startup thus there
// is no undesirable side-effects of multiple allocations (causing pressure on the
// garbage collector)
func reserveDeviceId(deviceIds []string) (string, []string) {
	ret := deviceIds[0]
	deviceIds = deviceIds[1:]
	return ret, deviceIds
}

// handleNetdevResources tries to retrieve any device plugin interfaces to be used by the system such as the management port.
func handleNetdevResources(resourceName string) (string, error) {
	var deviceId string
	deviceIdsMap := &config.OvnKubeNode.DPResourceDeviceIdsMap
	if len((*deviceIdsMap)[resourceName]) > 0 {
		deviceId, (*deviceIdsMap)[resourceName] = reserveDeviceId((*deviceIdsMap)[resourceName])
	} else {
		return "", fmt.Errorf("insufficient device IDs for resource: %s", resourceName)
	}
	netdevice, err := util.GetNetdevNameFromDeviceId(deviceId, v1.DeviceInfo{})
	if err != nil {
		return "", err
	}
	return netdevice, nil
}

func exportManagementPortAnnotation(netdevName string, nodeAnnotator kube.Annotator) error {
	klog.Infof("Exporting management port annotation for netdev '%v'", netdevName)
	deviceID, err := util.GetDeviceIDFromNetdevice(netdevName)
	if err != nil {
		return err
	}
	vfindex, err := util.GetSriovnetOps().GetVfIndexByPciAddress(deviceID)
	if err != nil {
		return err
	}
	pfindex, err := util.GetSriovnetOps().GetPfIndexByVfPciAddress(deviceID)
	if err != nil {
		return err
	}

	return util.SetNodeManagementPortAnnotation(nodeAnnotator, pfindex, vfindex)
}

func importManagementPortAnnotation(node *kapi.Node) (string, error) {
	klog.Infof("Import management port annotation on node '%v'", node.Name)
	pfId, vfId, err := util.ParseNodeManagementPortAnnotation(node)

	if err != nil {
		return "", err
	}
	klog.Infof("Imported pfId '%v' and FuncId '%v' for node '%v'", pfId, vfId, node.Name)

	return util.GetSriovnetOps().GetVfRepresentorDPU(fmt.Sprintf("%d", pfId), fmt.Sprintf("%d", vfId))
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
func getMgmtPortAndRepNameModeDPU(node *kapi.Node) (string, string, error) {
	rep, err := importManagementPortAnnotation(node)
	if err != nil {
		return "", "", err
	}
	return "", rep, err
}

func getMgmtPortAndRepNameModeDPUHost() (string, string, error) {
	netdevName, err := getManagementPortNetDev(config.OvnKubeNode.MgmtPortNetdev)
	if err != nil {
		return "", "", err
	}
	return netdevName, "", nil
}

func getMgmtPortAndRepName(node *kapi.Node) (string, string, error) {
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

func createNodeManagementPorts(node *kapi.Node, nodeAnnotator kube.Annotator, waiter *startupWaiter,
	subnets []*net.IPNet, routeManager *routemanager.Controller) ([]managementPortEntry, *managementPortConfig, error) {
	netdevName, rep, err := getMgmtPortAndRepName(node)
	if err != nil {
		return nil, nil, err
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		err := exportManagementPortAnnotation(netdevName, nodeAnnotator)
		if err != nil {
			return nil, nil, err
		}
	}
	ports := NewManagementPorts(node.Name, subnets, netdevName, rep)

	var mgmtPortConfig *managementPortConfig
	mgmtPorts := make([]managementPortEntry, 0)
	for _, port := range ports {
		config, err := port.Create(routeManager, nodeAnnotator, waiter)
		if err != nil {
			return nil, nil, err
		}
		mgmtPorts = append(mgmtPorts, managementPortEntry{port: port, config: config})
		// Save this management port config for later usage.
		// Since only one OVS internal port / Representor config may exist it is fine just to overwrite it
		if _, ok := port.(*managementPortNetdev); !ok {
			mgmtPortConfig = config
		}
	}

	return mgmtPorts, mgmtPortConfig, nil
}

// getOVNSBZone returns the zone name stored in the Southbound db.
// It returns the default zone name if "options:name" is not set in the SB_Global row
func getOVNSBZone() (string, error) {
	dbZone, stderr, err := util.RunOVNSbctl("get", "SB_Global", ".", "options:name")
	if err != nil {
		if strings.Contains(stderr, "ovn-sbctl: no key \"name\" in SB_Global record") {
			// If the options:name is not present, assume default zone
			return types.OvnDefaultZone, nil
		}
		return "", err
	}

	return dbZone, nil
}

/** HACK BEGIN **/
// TODO(tssurya): Remove this HACK a few months from now.
// checkOVNSBNodeLRSR returns true if the logical router static route for the
// the given nodeSubnet is present in the SBDB
func checkOVNSBNodeLRSR(nodeSubnet *net.IPNet) bool {
	var matchv4, matchv6 string
	v6 := true
	v4 := true
	if config.IPv6Mode && utilnet.IsIPv6CIDR(nodeSubnet) {
		matchv6 = fmt.Sprintf("match=\"reg7 == 0 && ip6.dst == %s\"", nodeSubnet)
		stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "logical_flow", matchv6)
		klog.Infof("Upgrade Hack: checkOVNSBNodeLRSR for node - %s : match %s : stdout - %s : stderr - %s : err %v",
			nodeSubnet, matchv6, stdout, stderr, err)
		v6 = (err == nil && stderr == "" && stdout != "")
	}
	if config.IPv4Mode && !utilnet.IsIPv6CIDR(nodeSubnet) {
		matchv4 = fmt.Sprintf("match=\"reg7 == 0 && ip4.dst == %s\"", nodeSubnet)
		stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "logical_flow", matchv4)
		klog.Infof("Upgrade Hack: checkOVNSBNodeLRSR for node - %s : match %s : stdout - %s : stderr - %s : err %v",
			nodeSubnet, matchv4, stdout, stderr, err)
		v4 = (err == nil && stderr == "" && stdout != "")
	}
	return v6 && v4
}

func fetchLBNames() string {
	stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "name", "find", "Load_Balancer")
	if err != nil || stderr != "" {
		klog.Errorf("Upgrade hack: fetchLBNames could not fetch services %v/%v", err, stderr)
		return stdout // will be empty and we will retry
	}
	klog.Infof("Upgrade Hack: fetchLBNames: stdout - %s : stderr - %s : err %v", stdout, stderr, err)
	return stdout
}

// lbExists returns true if the OVN load balancer for the corresponding namespace/name
// was created
func lbExists(lbNames, namespace, name string) bool {
	stitchedServiceName := "Service_" + namespace + "/" + name
	match := strings.Contains(lbNames, stitchedServiceName)
	klog.Infof("Upgrade Hack: lbExists for service - %s/%s/%s : match - %v",
		namespace, name, stitchedServiceName, match)
	return match
}

func portExists(namespace, name string) bool {
	lspName := fmt.Sprintf("logical_port=%s", util.GetLogicalPortName(namespace, name))
	stdout, stderr, err := util.RunOVNSbctl("--bare", "--columns", "_uuid", "find", "Port_Binding", lspName)
	klog.Infof("Upgrade Hack: portExists for pod - %s/%s : stdout - %s : stderr - %s", namespace, name, stdout, stderr)
	return err == nil && stderr == "" && stdout != ""
}

/** HACK END **/

// Start learns the subnets assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func (nc *DefaultNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Starting the default node network controller")

	var err error
	var node *kapi.Node
	var subnets []*net.IPNet
	var cniServer *cni.Server

	// Setting debug log level during node bring up to expose bring up process.
	// Log level is returned to configured value when bring up is complete.
	var level klog.Level
	if err := level.Set("5"); err != nil {
		klog.Errorf("Setting klog \"loglevel\" to 5 failed, err: %v", err)
	}
	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		nc.routeManager.Run(nc.stopChan, 4*time.Minute)
	}()

	// Bootstrap flows in OVS if just normal flow is present
	if err := bootstrapOVSFlows(); err != nil {
		return fmt.Errorf("failed to bootstrap OVS flows: %w", err)
	}

	if node, err = nc.Kube.GetNode(nc.name); err != nil {
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

	// Make sure that the node zone matches with the Southbound db zone.
	// Wait for 300s before giving up
	var sbZone string
	var err1 error

	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		// There is no SBDB to connect to in DPU Host mode, so we will just take the default input config zone
		sbZone = config.Default.Zone
	} else {
		err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(ctx context.Context) (bool, error) {
			sbZone, err = getOVNSBZone()
			if err != nil {
				err1 = fmt.Errorf("failed to get the zone name from the OVN Southbound db server, err : %w", err)
				return false, nil
			}

			if config.Default.Zone != sbZone {
				err1 = fmt.Errorf("node %s zone %s mismatch with the Southbound zone %s", nc.name, config.Default.Zone, sbZone)
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("timed out waiting for the node zone %s to match the OVN Southbound db zone, err: %v, err1: %v", config.Default.Zone, err, err1)
		}

		// if its nonIC OR IC=true and if its phase1 OR if its IC to IC upgrades
		if !config.OVNKubernetesFeature.EnableInterconnect || sbZone == types.OvnDefaultZone || util.HasNodeMigratedZone(node) { // if its nonIC or if its phase1
			for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
				if err := auth.SetDBAuth(); err != nil {
					return err
				}
			}
		}

		err = setupOVNNode(node)
		if err != nil {
			return err
		}
	}

	// First wait for the node logical switch to be created by the Master, timeout is 300s.
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(ctx context.Context) (bool, error) {
		if node, err = nc.Kube.GetNode(nc.name); err != nil {
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
	if config.OvnKubeNode.Mode != types.NodeModeDPU {
		kclient, ok := nc.Kube.(*kube.Kube)
		if !ok {
			return fmt.Errorf("cannot get kubeclient for starting CNI server")
		}
		cniServer, err = cni.NewCNIServer(nc.watchFactory, kclient.KClient)
		if err != nil {
			return err
		}
	}

	nodeAnnotator := kube.NewNodeAnnotator(nc.Kube, node.Name)
	waiter := newStartupWaiter()

	// Use the device from environment when the DP resource name is specified.
	if config.OvnKubeNode.MgmtPortDPResourceName != "" {
		if err := handleDevicePluginResources(); err != nil {
			return err
		}

		netdevice, err := handleNetdevResources(config.OvnKubeNode.MgmtPortDPResourceName)
		if err != nil {
			return err
		}

		if config.OvnKubeNode.MgmtPortNetdev != "" {
			klog.Warningf("MgmtPortNetdev is set explicitly (%s), overriding with resource...",
				config.OvnKubeNode.MgmtPortNetdev)
		}
		config.OvnKubeNode.MgmtPortNetdev = netdevice
		klog.V(5).Infof("Using MgmtPortNetdev (Netdev %s) passed via resource %s",
			config.OvnKubeNode.MgmtPortNetdev, config.OvnKubeNode.MgmtPortDPResourceName)
	}

	// Setup management ports
	mgmtPorts, mgmtPortConfig, err := createNodeManagementPorts(node, nodeAnnotator, waiter, subnets, nc.routeManager)
	if err != nil {
		return err
	}

	// Initialize gateway
	if config.OvnKubeNode.Mode == types.NodeModeDPUHost {
		err = nc.initGatewayDPUHost(nodeAddr)
		if err != nil {
			return err
		}
	} else {
		// Initialize gateway for OVS internal port or representor management port
		if err := nc.initGateway(subnets, nodeAnnotator, waiter, mgmtPortConfig, nodeAddr); err != nil {
			return err
		}
	}

	if err := util.SetNodeZone(nodeAnnotator, sbZone); err != nil {
		return fmt.Errorf("failed to set node zone annotation for node %s: %w", nc.name, err)
	}

	if err := nodeAnnotator.Run(); err != nil {
		return fmt.Errorf("failed to set node %s annotations: %w", nc.name, err)
	}

	// If EncapPort is not the default tell sbdb to use specified port.
	// We set the encap port after annotating the zone name so that ovnkube-controller has come up
	// and configured the chassis in SBDB (ovnkube-controller waits for ovnkube-node to set annotation
	// for at least one node in the given zone)
	// NOTE: ovnkube-node in DPU-host mode has no SBDB to connect to. The encap port will be handled by the
	// ovnkube-node running in DPU mode on behalf of the host.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.Default.EncapPort != config.DefaultEncapPort {
		if err := setEncapPort(ctx); err != nil {
			return err
		}
	}

	/** HACK BEGIN **/
	// TODO(tssurya): Remove this HACK a few months from now. This has been added only to
	// minimize disruption for upgrades when moving to interconnect=true.
	// We want the legacy ovnkube-master to wait for remote ovnkube-node to
	// signal it using "k8s.ovn.org/remote-zone-migrated" annotation before
	// considering a node as remote when we upgrade from "global" (1 zone IC)
	// zone to multi-zone. This is so that network disruption for the existing workloads
	// is negligible and until the point where ovnkube-node flips the switch to connect
	// to the new SBDB, it would continue talking to the legacy RAFT ovnkube-sbdb to ensure
	// OVN/OVS flows are intact.
	// STEP1: ovnkube-node start's up in remote zone and sets the "k8s.ovn.org/zone-name" above.
	// STEP2: We delay the flip of connection for ovnkube-node(ovn-controller) to the new remote SBDB
	//        until the new remote ovnkube-controller has finished programming all the K8s core objects
	//        like routes, services and pods. Until then the ovnkube-node will talk to legacy SBDB.
	// STEP3: Once we get the signal that the new SBDB is ready, we set the "k8s.ovn.org/remote-zone-migrated" annotation
	// STEP4: We call setDBAuth to now point to new SBDB
	// STEP5: Legacy ovnkube-master sees "k8s.ovn.org/remote-zone-migrated" annotation on this node and now knows that
	//        this node has remote-zone-migrated successfully and tears down old setup and creates new IC resource
	//        plumbing (takes 80ms based on what we saw in CI runs so we might still have that small window of disruption).
	// NOTE: ovnkube-node in DPU host mode doesn't go through upgrades for OVN-IC and has no SBDB to connect to. Thus this part shall be skipped.
	var syncNodes, syncServices, syncPods bool
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost && config.OVNKubernetesFeature.EnableInterconnect && sbZone != types.OvnDefaultZone && !util.HasNodeMigratedZone(node) { // so this should be done only once in phase2 (not in phase1)
		klog.Info("Upgrade Hack: Interconnect is enabled")
		var err1 error
		start := time.Now()
		err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 300*time.Second, true, func(ctx context.Context) (bool, error) {
			// we loop through all the nodes in the cluster and ensure ovnkube-controller has finished creating the LRSR required for pod2pod overlay communication
			if !syncNodes {
				nodes, err := nc.Kube.GetNodes()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving node %s: %v", nc.name, err)
					return false, nil
				}
				for _, node := range nodes {
					node := *node
					if nc.name != node.Name && util.GetNodeZone(&node) != config.Default.Zone && !util.NoHostSubnet(&node) {
						nodeSubnets, err := util.ParseNodeHostSubnetAnnotation(&node, types.DefaultNetworkName)
						if err != nil {
							if util.IsAnnotationNotSetError(err) {
								klog.Infof("Skipping node %q. k8s.ovn.org/node-subnets annotation was not found", node.Name)
								continue
							}
							err1 = fmt.Errorf("unable to fetch node-subnet annotation for node %s: err, %v", node.Name, err)
							return false, nil
						}
						for _, nodeSubnet := range nodeSubnets {
							klog.Infof("Upgrade Hack: node %s, subnet %s", node.Name, nodeSubnet)
							if !checkOVNSBNodeLRSR(nodeSubnet) {
								err1 = fmt.Errorf("upgrade hack: unable to find LRSR for node %s", node.Name)
								return false, nil
							}
						}
					}
				}
				klog.Infof("Upgrade Hack: Syncing nodes took %v", time.Since(start))
				syncNodes = true
			}
			// we loop through all existing services in the cluster and ensure ovnkube-controller has finished creating LoadBalancers required for services to work
			if !syncServices {
				services, err := nc.watchFactory.GetServices()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving the services %v", err)
					return false, nil
				}
				lbNames := fetchLBNames()
				for _, s := range services {
					// don't process headless service
					if !util.ServiceTypeHasClusterIP(s) || !util.IsClusterIPSet(s) {
						continue
					}
					if !lbExists(lbNames, s.Namespace, s.Name) {
						return false, nil
					}
				}
				klog.Infof("Upgrade Hack: Syncing services took %v", time.Since(start))
				syncServices = true
			}
			if !syncPods {
				pods, err := nc.watchFactory.GetAllPods()
				if err != nil {
					err1 = fmt.Errorf("upgrade hack: error retrieving the services %v", err)
					return false, nil
				}
				for _, p := range pods {
					if !util.PodScheduled(p) || util.PodCompleted(p) || util.PodWantsHostNetwork(p) {
						continue
					}
					if p.Spec.NodeName != nc.name {
						// remote pod
						continue
					}
					if !portExists(p.Namespace, p.Name) {
						return false, nil
					}
				}
				klog.Infof("Upgrade Hack: Syncing pods took %v", time.Since(start))
				syncPods = true
			}
			return true, nil
		})
		if err != nil {
			return fmt.Errorf("upgrade hack: failed while waiting for the remote ovnkube-controller to be ready: %v, %v", err, err1)
		}
		if err := util.SetNodeZoneMigrated(nodeAnnotator, sbZone); err != nil {
			return fmt.Errorf("upgrade hack: failed to set node zone annotation for node %s: %w", nc.name, err)
		}
		if err := nodeAnnotator.Run(); err != nil {
			return fmt.Errorf("upgrade hack: failed to set node %s annotations: %w", nc.name, err)
		}
		klog.Infof("ovnkube-node %s finished annotating node with remote-zone-migrated; took: %v", nc.name, time.Since(start))
		for _, auth := range []config.OvnAuthConfig{config.OvnNorth, config.OvnSouth} {
			if err := auth.SetDBAuth(); err != nil {
				return fmt.Errorf("upgrade hack: Unable to set the authentication towards OVN local dbs")
			}
		}
		klog.Infof("Upgrade hack: ovnkube-node %s finished setting DB Auth; took: %v", nc.name, time.Since(start))
	}
	/** HACK END **/

	// Wait for management port and gateway resources to be created by the master
	klog.Infof("Waiting for gateway and management port readiness...")
	start := time.Now()
	if err := waiter.Wait(); err != nil {
		return err
	}
	nc.gateway.Start()
	klog.Infof("Gateway and management port readiness took %v", time.Since(start))

	// Note(adrianc): DPU deployments are expected to support the new shared gateway changes, upgrade flow
	// is not needed. Future upgrade flows will need to take DPUs into account.
	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		bridgeName := ""
		if config.OvnKubeNode.Mode == types.NodeModeFull {
			bridgeName = nc.gateway.GetGatewayBridgeIface()
			// Configure route for svc towards shared gw bridge
			// Have to have the route to bridge for multi-NIC mode, where the default gateway may go to a non-OVS interface
			if err := configureSvcRouteViaBridge(nc.routeManager, bridgeName); err != nil {
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
			nc.watchFactory.LocalPodInformer(),
			informer.NewDefaultEventHandler,
			false,
		)
		if err != nil {
			return err
		}
		nc.wg.Add(1)
		go func() {
			defer nc.wg.Done()
			nodeController.Run(nc.stopChan)
		}()
	} else {
		// attempt to cleanup the possibly stale bridge
		_, stderr, err := util.RunOVSVsctl("--if-exists", "del-br", "br-ext")
		if err != nil {
			klog.Errorf("Deletion of bridge br-ext failed: %v (%v)", err, stderr)
		}
		_, stderr, err = util.RunOVSVsctl("--if-exists", "del-port", "br-int", "int")
		if err != nil {
			klog.Errorf("Deletion of port int on  br-int failed: %v (%v)", err, stderr)
		}
	}

	if err := level.Set(strconv.Itoa(config.Logging.Level)); err != nil {
		klog.Errorf("Reset of initial klog \"loglevel\" failed, err: %v", err)
	}

	// start management ports health check
	for _, mgmtPort := range mgmtPorts {
		mgmtPort.port.CheckManagementPortHealth(nc.routeManager, mgmtPort.config, nc.stopChan)
		if config.OVNKubernetesFeature.EnableEgressIP {
			// Start the health checking server used by egressip, if EgressIPNodeHealthCheckPort is specified
			if err := nc.startEgressIPHealthCheckingServer(mgmtPort); err != nil {
				return err
			}
		}
	}

	if config.OvnKubeNode.Mode != types.NodeModeDPUHost {
		// If interconnect is disabled OR interconnect is running in single-zone-mode,
		// the ovnkube-master is responsible for patching ICNI managed namespaces with
		// "k8s.ovn.org/external-gw-pod-ips". In that case, we need ovnkube-node to flush
		// conntrack on every node. In multi-zone-interconnect case, we will handle the flushing
		// directly on the ovnkube-controller code to avoid an extra namespace annotation
		if !config.OVNKubernetesFeature.EnableInterconnect || sbZone == types.OvnDefaultZone {
			util.SetARPTimeout()
			err := nc.WatchNamespaces()
			if err != nil {
				return fmt.Errorf("failed to watch namespaces: %w", err)
			}
			// every minute cleanup stale conntrack entries if any
			go wait.Until(func() {
				nc.checkAndDeleteStaleConntrackEntries()
			}, time.Minute*1, nc.stopChan)
		}
		err = nc.WatchEndpointSlices()
		if err != nil {
			return fmt.Errorf("failed to watch endpointSlices: %w", err)
		}
	}

	if nc.healthzServer != nil {
		nc.healthzServer.Start(nc.stopChan, nc.wg)
	}

	if config.OvnKubeNode.Mode == types.NodeModeDPU {
		if _, err := nc.watchPodsDPU(); err != nil {
			return err
		}
	} else {
		// start the cni server
		if err := cniServer.Start(cni.ServerRunDir); err != nil {
			return err
		}

		// Write CNI config file if it doesn't already exist
		if err := config.WriteCNIConfig(); err != nil {
			return err
		}
	}

	if config.OVNKubernetesFeature.EnableEgressService {
		wf := nc.watchFactory.(*factory.WatchFactory)
		c, err := egressservice.NewController(nc.stopChan, ovnKubeNodeSNATMark, nc.name,
			wf.EgressServiceInformer(), wf.ServiceInformer(), wf.EndpointSliceInformer())
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

	// create link manager, will work for egress IP as well as monitoring MAC changes to default gw bridge
	linkManager := linkmanager.NewController(nc.name, config.IPv4Mode, config.IPv6Mode, nc.updateGatewayMAC)

	if config.OVNKubernetesFeature.EnableEgressIP && !util.PlatformTypeIsEgressIPCloudProvider() {
		c, err := egressip.NewController(nc.Kube, nc.watchFactory.EgressIPInformer(), nc.watchFactory.NodeInformer(),
			nc.watchFactory.NamespaceInformer(), nc.watchFactory.PodCoreInformer(), nc.routeManager, config.IPv4Mode,
			config.IPv6Mode, nc.name, linkManager)
		if err != nil {
			return fmt.Errorf("failed to create egress IP controller: %v", err)
		}
		if err = c.Run(nc.stopChan, nc.wg, 1); err != nil {
			return fmt.Errorf("failed to run egress IP controller: %v", err)
		}
	} else {
		klog.Infof("Egress IP for secondary host network is disabled")
	}

	linkManager.Run(nc.stopChan, nc.wg)

	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		ovspinning.Run(nc.stopChan)
	}()

	klog.Infof("Default node network controller initialized and ready.")
	return nil
}

// Stop gracefully stops the controller
// deleteLogicalEntities will never be true for default network
func (nc *DefaultNodeNetworkController) Stop() {
	close(nc.stopChan)
	nc.wg.Wait()
}

func (nc *DefaultNodeNetworkController) startEgressIPHealthCheckingServer(mgmtPortEntry managementPortEntry) error {
	healthCheckPort := config.OVNKubernetesFeature.EgressIPNodeHealthCheckPort
	if healthCheckPort == 0 {
		klog.Infof("Egress IP health check server skipped: no port specified")
		return nil
	}

	var nodeMgmtIP net.IP
	var mgmtPortConfig *managementPortConfig = mgmtPortEntry.config
	// Not all management port interfaces can have IP addresses assignable to them.
	if mgmtPortEntry.port.HasIpAddr() {
		if mgmtPortConfig.ipv4 != nil {
			nodeMgmtIP = mgmtPortConfig.ipv4.ifAddr.IP
		} else if mgmtPortConfig.ipv6 != nil {
			nodeMgmtIP = mgmtPortConfig.ipv6.ifAddr.IP
			// Wait for IPv6 address to become usable.
			if err := ip.SettleAddresses(mgmtPortConfig.ifName, 10); err != nil {
				return fmt.Errorf("failed to start Egress IP health checking server due to unsettled IPv6: %w on interface %s", err, mgmtPortConfig.ifName)
			}
		} else {
			return fmt.Errorf("unable to start Egress IP health checking server on interface %s: no mgmt ip", mgmtPortConfig.ifName)
		}
	} else {
		klog.Infof("Skipping interface %s as it does not have an IP address", mgmtPortConfig.ifName)
		return nil
	}

	healthServer, err := healthcheck.NewEgressIPHealthServer(nodeMgmtIP, healthCheckPort)
	if err != nil {
		return fmt.Errorf("unable to allocate health checking server: %v", err)
	}

	nc.wg.Add(1)
	go func() {
		defer nc.wg.Done()
		healthServer.Run(nc.stopChan)
	}()
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
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("error while retrieving service for endpointslice %s/%s when reconciling conntrack: %v",
			newEndpointSlice.Namespace, newEndpointSlice.Name, err)
	}
	for _, oldPort := range oldEndpointSlice.Ports {
		if *oldPort.Protocol != kapi.ProtocolUDP { // flush conntrack only for UDP
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
				// upon update and delete events, flush conntrack only for UDP
				if err := util.DeleteConntrackServicePort(oldIPStr, *oldPort.Port, *oldPort.Protocol,
					netlink.ConntrackReplyAnyIP, nil); err != nil {
					klog.Errorf("Failed to delete conntrack entry for %s: %v", oldIPStr, err)
				}
			}
		}
	}
	return utilerrors.Join(errors...)

}
func (nc *DefaultNodeNetworkController) WatchEndpointSlices() error {
	_, err := nc.retryEndpointSlices.WatchResource()
	return err
}

func exGatewayPodsAnnotationsChanged(oldNs, newNs *kapi.Namespace) bool {
	// In reality we only care about exgw pod deletions, however since the list of IPs is not expected to change
	// that often, let's check for *any* changes to these annotations compared to their previous state and trigger
	// the logic for checking if we need to delete any conntrack entries
	return (oldNs.Annotations[util.ExternalGatewayPodIPsAnnotation] != newNs.Annotations[util.ExternalGatewayPodIPsAnnotation]) ||
		(oldNs.Annotations[util.RoutingExternalGWsAnnotation] != newNs.Annotations[util.RoutingExternalGWsAnnotation])
}

func (nc *DefaultNodeNetworkController) checkAndDeleteStaleConntrackEntries() {
	namespaces, err := nc.watchFactory.GetNamespaces()
	if err != nil {
		klog.Errorf("Unable to get pods from informer: %v", err)
	}
	for _, namespace := range namespaces {
		_, foundRoutingExternalGWsAnnotation := namespace.Annotations[util.RoutingExternalGWsAnnotation]
		_, foundExternalGatewayPodIPsAnnotation := namespace.Annotations[util.ExternalGatewayPodIPsAnnotation]
		if foundRoutingExternalGWsAnnotation || foundExternalGatewayPodIPsAnnotation {
			pods, err := nc.watchFactory.GetPods(namespace.Name)
			if err != nil {
				klog.Warningf("Unable to get pods from informer for namespace %s: %v", namespace.Name, err)
			}
			if len(pods) > 0 || err != nil {
				// we only need to proceed if there is at least one pod in this namespace on this node
				// OR if we couldn't fetch the pods for some reason at this juncture
				_ = nc.syncConntrackForExternalGateways(namespace)
			}
		}
	}
}

func (nc *DefaultNodeNetworkController) syncConntrackForExternalGateways(newNs *kapi.Namespace) error {
	gatewayIPs, err := nc.apbExternalRouteNodeController.GetAdminPolicyBasedExternalRouteIPsForTargetNamespace(newNs.Name)
	if err != nil {
		return fmt.Errorf("unable to retrieve gateway IPs for Admin Policy Based External Route objects: %w", err)
	}
	// loop through all the IPs on the annotations; ARP for their MACs and form an allowlist
	gatewayIPs = gatewayIPs.Insert(strings.Split(newNs.Annotations[util.ExternalGatewayPodIPsAnnotation], ",")...)
	gatewayIPs = gatewayIPs.Insert(strings.Split(newNs.Annotations[util.RoutingExternalGWsAnnotation], ",")...)

	return util.SyncConntrackForExternalGateways(gatewayIPs, nil, func() ([]*kapi.Pod, error) {
		return nc.watchFactory.GetPods(newNs.Name)
	})
}

func (nc *DefaultNodeNetworkController) WatchNamespaces() error {
	_, err := nc.retryNamespaces.WatchResource()
	return err
}

// validateVTEPInterfaceMTU checks if the MTU of the interface that has ovn-encap-ip is big
// enough to carry the `config.Default.MTU` and the Geneve header. If the MTU is not big
// enough, it will return an error
func (nc *DefaultNodeNetworkController) validateVTEPInterfaceMTU() error {
	// OVN allows `external_ids:ovn-encap-ip` to be a list of IPs separated by comma
	ovnEncapIps := strings.Split(config.Default.EncapIP, ",")
	for _, ip := range ovnEncapIps {
		ovnEncapIP := net.ParseIP(strings.TrimSpace(ip))
		if ovnEncapIP == nil {
			return fmt.Errorf("invalid IP address %q in provided encap-ip setting %q", ovnEncapIP, config.Default.EncapIP)
		}
		interfaceName, mtu, err := util.GetIFNameAndMTUForAddress(ovnEncapIP)
		if err != nil {
			return fmt.Errorf("could not get MTU for the interface with address %s: %w", ovnEncapIP, err)
		}

		// calc required MTU
		var requiredMTU int
		if config.Gateway.SingleNode {
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

		if mtu < requiredMTU {
			return fmt.Errorf("MTU (%d) of network interface %s is too small for specified overlay MTU (%d)",
				mtu, interfaceName, requiredMTU)
		}
		klog.V(2).Infof("MTU (%d) of network interface %s is big enough to deal with Geneve header overhead (sum %d). ",
			mtu, interfaceName, requiredMTU)
	}
	return nil
}

func configureSvcRouteViaBridge(routeManager *routemanager.Controller, bridge string) error {
	return configureSvcRouteViaInterface(routeManager, bridge, DummyNextHopIPs())
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
