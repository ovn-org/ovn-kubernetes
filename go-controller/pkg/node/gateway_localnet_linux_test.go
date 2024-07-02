package node

import (
	"context"
	"fmt"
	"net"
	"sync"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	nodeipt "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node/iptables"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/urfave/cli/v2"
	"github.com/vishvananda/netlink"

	"github.com/coreos/go-iptables/iptables"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
)

const (
	v4localnetGatewayIP = "10.244.0.1"
	v6localnetGatewayIP = "fd00:96:1::1"
	gwMAC               = "0a:0b:0c:0d:0e:0f"
	linkName            = "breth0"
)

func initFakeNodePortWatcher(iptV4, iptV6 util.IPTablesHelper) *nodePortWatcher {
	initIPTable := map[string]util.FakeTable{
		"nat":    {},
		"filter": {},
		"mangle": {},
	}

	f4 := iptV4.(*util.FakeIPTables)
	err := f4.MatchState(initIPTable, nil)
	Expect(err).NotTo(HaveOccurred())

	f6 := iptV6.(*util.FakeIPTables)
	err = f6.MatchState(initIPTable, nil)
	Expect(err).NotTo(HaveOccurred())

	gwMACParsed, _ := net.ParseMAC(gwMAC)

	fNPW := nodePortWatcher{
		ofportPhys:  "eth0",
		ofportPatch: "patch-breth0_ov",
		gatewayIPv4: v4localnetGatewayIP,
		gatewayIPv6: v6localnetGatewayIP,
		serviceInfo: make(map[k8stypes.NamespacedName]*serviceConfig),
		ofm: &openflowManager{
			flowCache:     map[string][]string{},
			defaultBridge: &bridgeConfiguration{macAddress: gwMACParsed},
		},
	}
	return &fNPW
}

func startNodePortWatcher(n *nodePortWatcher, fakeClient *util.OVNNodeClientset, fakeMgmtPortConfig *managementPortConfig) error {
	if err := initLocalGatewayIPTables(); err != nil {
		return err
	}

	k := &kube.Kube{KClient: fakeClient.KubeClient}
	n.nodeIPManager = newAddressManagerInternal(fakeNodeName, k, fakeMgmtPortConfig, n.watchFactory, nil, false)
	localHostNetEp := "192.168.18.15/32"
	ip, ipnet, _ := net.ParseCIDR(localHostNetEp)
	n.nodeIPManager.addAddr(net.IPNet{IP: ip, Mask: ipnet.Mask})

	// Add or delete iptables rules from FORWARD chain based on DisableForwarding. This is
	// to imitate addition or deletion of iptales rules done in newNodePortWatcher().
	var subnets []*net.IPNet
	for _, subnet := range config.Default.ClusterSubnets {
		subnets = append(subnets, subnet.CIDR)
	}
	subnets = append(subnets, config.Kubernetes.ServiceCIDRs...)
	if config.Gateway.DisableForwarding {
		if err := initExternalBridgeServiceForwardingRules(subnets); err != nil {
			return fmt.Errorf("failed to add accept rules in forwarding table for bridge %s: err %v", linkName, err)
		}
	} else {
		if err := delExternalBridgeServiceForwardingRules(subnets); err != nil {
			return fmt.Errorf("failed to delete accept rules in forwarding table for bridge %s: err %v", linkName, err)
		}
	}

	// set up a controller to handle events on services to mock the nodeportwatcher bits
	// in gateway.go and trigger code in gateway_shared_intf.go
	_, err := n.watchFactory.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			n.AddService(svc)
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			n.UpdateService(oldSvc, newSvc)
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			n.DeleteService(svc)
		},
	}, n.SyncServices)

	return err
}

func startNodePortWatcherWithRetry(n *nodePortWatcher, fakeClient *util.OVNNodeClientset, fakeMgmtPortConfig *managementPortConfig, stopChan chan struct{}, wg *sync.WaitGroup) (*retry.RetryFramework, error) {
	if err := initLocalGatewayIPTables(); err != nil {
		return nil, err
	}

	k := &kube.Kube{KClient: fakeClient.KubeClient}
	n.nodeIPManager = newAddressManagerInternal(fakeNodeName, k, fakeMgmtPortConfig, n.watchFactory, nil, false)
	localHostNetEp := "192.168.18.15/32"
	ip, ipnet, _ := net.ParseCIDR(localHostNetEp)
	n.nodeIPManager.addAddr(net.IPNet{IP: ip, Mask: ipnet.Mask})

	nodePortWatcherRetry := n.newRetryFrameworkForTests(factory.ServiceForFakeNodePortWatcherType, stopChan, wg)
	if _, err := nodePortWatcherRetry.WatchResource(); err != nil {
		return nil, fmt.Errorf("failed to start watching services with retry framework: %v", err)
	}
	return nodePortWatcherRetry, nil
}

func newObjectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       k8stypes.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
		Annotations: map[string]string{},
	}
}

func newService(name, namespace, ip string, ports []v1.ServicePort, serviceType v1.ServiceType,
	externalIPs []string, serviceStatus v1.ServiceStatus, isETPLocal, isITPLocal bool) *v1.Service {
	externalTrafficPolicy := v1.ServiceExternalTrafficPolicyTypeCluster
	internalTrafficPolicy := v1.ServiceInternalTrafficPolicyCluster
	if isETPLocal {
		externalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	}
	if isITPLocal {
		internalTrafficPolicy = v1.ServiceInternalTrafficPolicyLocal
	}
	return &v1.Service{
		ObjectMeta: newObjectMeta(name, namespace),
		Spec: v1.ServiceSpec{
			ClusterIP:             ip,
			ClusterIPs:            []string{ip},
			Ports:                 ports,
			Type:                  serviceType,
			ExternalIPs:           externalIPs,
			ExternalTrafficPolicy: externalTrafficPolicy,
			InternalTrafficPolicy: &internalTrafficPolicy,
		},
		Status: serviceStatus,
	}
}

func newServiceWithoutNodePortAllocation(name, namespace, ip string, ports []v1.ServicePort, serviceType v1.ServiceType,
	externalIPs []string, serviceStatus v1.ServiceStatus, isETPLocal, isITPLocal bool) *v1.Service {
	doNotAllocateNodePorts := false
	service := newService(name, namespace, ip, ports, serviceType, externalIPs, serviceStatus, isETPLocal, isITPLocal)
	service.Spec.AllocateLoadBalancerNodePorts = &doNotAllocateNodePorts
	return service
}

func newEndpointSlice(svcName, namespace string, endpoints []discovery.Endpoint, endpointPort []discovery.EndpointPort) *discovery.EndpointSlice {
	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName + "ab23",
			Namespace: namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svcName},
		},
		Ports:       endpointPort,
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

func makeConntrackFilter(ip string, port int, protocol kapi.Protocol) *netlink.ConntrackFilter {
	filter := &netlink.ConntrackFilter{}

	var err error
	if protocol == kapi.ProtocolUDP {
		err = filter.AddProtocol(17)
	} else if protocol == kapi.ProtocolTCP {
		err = filter.AddProtocol(6)
	} else if protocol == kapi.ProtocolSCTP {
		err = filter.AddProtocol(132)
	}
	Expect(err).NotTo(HaveOccurred())

	if port > 0 {
		err = filter.AddPort(netlink.ConntrackOrigDstPort, uint16(port))
		Expect(err).NotTo(HaveOccurred())
	}
	ipAddress := net.ParseIP(ip)
	Expect(ipAddress).NotTo(BeNil())
	err = filter.AddIP(netlink.ConntrackOrigDstIP, ipAddress)
	Expect(err).NotTo(HaveOccurred())

	return filter
}

type ctFilterDesc struct {
	ip   string
	port int
}

func addConntrackMocks(nlMock *mocks.NetLinkOps, filterDescs []ctFilterDesc) {
	ctMocks := make([]ovntest.TestifyMockHelper, 0, len(filterDescs))
	for _, ctf := range filterDescs {
		ctMocks = append(ctMocks, ovntest.TestifyMockHelper{
			OnCallMethodName: "ConntrackDeleteFilter",
			OnCallMethodArgs: []interface{}{
				netlink.ConntrackTableType(netlink.ConntrackTable),
				netlink.InetFamily(netlink.FAMILY_V4),
				makeConntrackFilter(ctf.ip, ctf.port, kapi.ProtocolTCP),
			},
			RetArgList: []interface{}{uint(1), nil},
		})
	}
	ovntest.ProcessMockFnList(&nlMock.Mock, ctMocks)
}

/*
Note: all of the tests described below actually rely on OVNK node controller start up failing. This is
because no node is actually added when the controller is started, so node start up fails querying kapi for its
own node. This is either intentional or accidentally convenient as the node port watcher is then replaced with a fake
one and started again to exercise the tests.
*/
var _ = Describe("Node Operations", func() {
	var (
		app                *cli.App
		fakeOvnNode        *FakeOVNNode
		fExec              *ovntest.FakeExec
		iptV4, iptV6       util.IPTablesHelper
		fNPW               *nodePortWatcher
		fakeMgmtPortConfig managementPortConfig
		netlinkMock        *mocks.NetLinkOps
	)

	origNetlinkInst := util.GetNetLinkOps()

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		netlinkMock = &mocks.NetLinkOps{}
		util.SetNetLinkOpMockInst(netlinkMock)

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		fExec = ovntest.NewFakeExec()
		fakeOvnNode = NewFakeOVNNode(fExec)
		fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovs-vsctl --timeout=15 --no-heading --data=bare --format=csv --columns name list interface",
		})

		iptV4, iptV6 = util.SetFakeIPTablesHelpers()
		_, nodeNet, err := net.ParseCIDR("10.1.1.0/24")
		Expect(err).NotTo(HaveOccurred())
		// Make a fake MgmtPortConfig with only the fields we care about
		fakeMgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
			ipt:        nil,
			allSubnets: nil,
			ifAddr:     nodeNet,
			gwIP:       nodeNet.IP,
		}
		fakeMgmtPortConfig = managementPortConfig{
			ifName:    fakeNodeName,
			link:      nil,
			routerMAC: nil,
			ipv4:      &fakeMgmtPortIPFamilyConfig,
			ipv6:      nil,
		}
		fNPW = initFakeNodePortWatcher(iptV4, iptV6)
	})

	AfterEach(func() {
		fakeOvnNode.shutdown()
		util.SetNetLinkOpMockInst(origNetlinkInst)
	})

	Context("on startup", func() {
		It("removes stale iptables rules while keeping remaining intact", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				externalIPPort := int32(8032)
				for i := 0; i < 2; i++ {
					fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: "ovs-ofctl show ",
					})
				}

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
					v1.ServiceStatus{},
					false, false,
				)

				fakeRules := getExternalIPTRules(service.Spec.Ports[0], externalIP, service.Spec.ClusterIP, false, false)
				Expect(insertIptRules(fakeRules)).To(Succeed())
				fakeRules = getExternalIPTRules(
					v1.ServicePort{
						Port:     27000,
						Protocol: v1.ProtocolUDP,
						Name:     "This is going to dissapear I hope",
					},
					"10.10.10.10",
					"172.32.0.12",
					false,
					false,
				)
				Expect(insertIptRules(fakeRules)).To(Succeed())

				// Inject rules into SNAT MGMT chain that shouldn't exist and should be cleared on a restore, even if the chain has no rules
				fakeRule := getSkipMgmtSNATRule("TCP", "1337", "8.8.8.8", iptables.ProtocolIPv4)
				Expect(insertIptRules([]nodeipt.Rule{fakeRule})).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p UDP -d 10.10.10.10 --dport 27000 -j DNAT --to-destination 172.32.0.12:27000"),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						iptableMgmPortChain: []string{
							fmt.Sprintf("-p TCP -d 8.8.8.8 --dport 1337 -j RETURN"),
						},
					},
					"filter": {},
					"mangle": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on add", func() {
		It("inits iptables rules with ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
					v1.ServiceStatus{},
					false, false,
				)
				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					false, false,
				)
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with NodePort where ETP=local, LGW", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				epPortValue := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with LoadBalancer", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				for i := 0; i < 3; i++ {
					fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: "ovs-ofctl show ",
					})
				}
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false,
				)
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=local, LGW mode", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where AllocateLoadBalancerNodePorts=False, ETP=local, LGW mode", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newServiceWithoutNodePortAllocation("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Protocol:   v1.ProtocolTCP,
							Port:       int32(80),
							TargetPort: intstr.FromInt(int(int32(8080))),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
					NodeName:  &fakeNodeName,
				}
				otherNodeName := "node2"
				nonLocalEndpoint := discovery.Endpoint{
					Addresses: []string{"10.244.1.3"}, // is not picked since its not local to the node
					NodeName:  &otherNodeName,
				}
				ep2 := discovery.Endpoint{
					Addresses: []string{"10.244.0.4"},
					NodeName:  &fakeNodeName,
				}
				epPortName := "http"
				epPortValue := int32(8080)
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2, nonLocalEndpoint},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				fNPW.AddService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP -d %s --dport %d -j RETURN", ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p TCP -d %s --dport %d -j RETURN", ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 0.5000000000", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 1.0000000000", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 0.5000000000", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, ep1.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%d -m statistic --mode random --probability 1.0000000000", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, ep2.Addresses[0], int32(service.Spec.Ports[0].TargetPort.IntValue())),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedLBIngressFlows := []string{
					"cookie=0xd8c1fe514f305bc1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x799e0efe5404e9a1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				Expect(f4.MatchState(expectedTables, nil)).To(Succeed())
				Expect(fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_80"]).To(Equal(expectedLBIngressFlows))
				Expect(fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_80"]).To(Equal(expectedLBExternalIPFlows))
				return nil
			}
			Expect(app.Run([]string{app.Name})).To(Succeed())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=cluster, LGW mode", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeLocal
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false, // ETP=cluster
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules and openflows with LoadBalancer where ETP=local, SGW mode", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				config.Gateway.Mode = config.GatewayModeShared
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedNodePortFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}
				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, icmp, nw_dst=5.5.5.5, icmp_type=3, icmp_code=4, actions=output:patch-breth0_ov",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, tcp, nw_dst=5.5.5.5, tp_dst=8080, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x10c6b89e483ea111, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, nw_src=5.5.5.5, tp_src=8080, actions=output:eth0",
						gwMAC),
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, icmp, nw_dst=1.1.1.1, icmp_type=3, icmp_code=4, actions=output:patch-breth0_ov",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, tcp, nw_dst=1.1.1.1, tp_dst=8080, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x71765945a31dc2f1, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, nw_src=1.1.1.1, tp_src=8080, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedNodePortFlows))
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with DualStack NodePort", func() {
			app.Action = func(ctx *cli.Context) error {
				nodePort := int32(31111)

				fNPW.gatewayIPv6 = v6localnetGatewayIP

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					false, false,
				)
				service.Spec.ClusterIPs = []string{"10.129.0.2", "fd00:10:96::10"}
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIPs[0], service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables4, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedTables6 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination [%s]:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIPs[1], service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules for ExternalIP with DualStack", func() {
			app.Action = func(ctx *cli.Context) error {

				externalIPv4 := "10.10.10.1"
				externalIPv6 := "fd00:96:1::1"
				clusterIPv4 := "10.129.0.2"
				clusterIPv6 := "fd00:10:96::10"
				fNPW.gatewayIPv6 = v6localnetGatewayIP
				for i := 0; i < 3; i++ {
					fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: "ovs-ofctl show ",
					})
				}

				service := *newService("service1", "namespace1", clusterIPv4,
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIPv4, externalIPv6},
					v1.ServiceStatus{},
					false, false,
				)
				service.Spec.ClusterIPs = []string{clusterIPv4, clusterIPv6}
				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIPv4, service.Spec.Ports[0].Port, clusterIPv4, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-NODEPORT":      []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables4, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedTables6 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination [%s]:%v", service.Spec.Ports[0].Protocol, externalIPv6, service.Spec.Ports[0].Port, clusterIPv6, service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on delete", func() {
		It("deletes iptables rules with ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				for i := 0; i < 2; i++ {
					fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: "ovs-ofctl show ",
					})
				}
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
					v1.ServiceStatus{},
					false, false,
				)

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"1.1.1.1", 8032}, {"10.129.0.2", 8032}})
				err := fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT":      []string{},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes iptables rules for NodePort", func() {
			app.Action = func(ctx *cli.Context) error {
				nodePort := int32(31111)

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					false, false,
				)

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 0}, {"192.168.18.15", 31111}})
				err := fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT":      []string{},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on add and delete", func() {
		It("manages iptables rules with ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.1"
				for i := 0; i < 3; i++ {
					fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd: "ovs-ofctl show ",
					})
				}
				externalIPPort := int32(8034)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
					v1.ServiceStatus{},
					false, false,
				)
				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)
				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v",
								service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port,
								service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-NODEPORT":      []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.10.10.1", 8034}, {"10.129.0.2", 8034}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())

		})

		It("check openflows for LoadBalancer and external ip are correctly added and removed where ETP=local, LGW mode", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				externalIP2 := "1.1.1.2"
				config.Gateway.Mode = config.GatewayModeLocal
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeLoadBalancer,
					[]string{externalIP, externalIP2},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					true, false,
				)
				// endpointSlice.Endpoints is empty and yet this will come under
				// !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{},
					[]discovery.EndpointPort{})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows1 := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
				}
				expectedLBExternalIPFlows2 := []string{
					"cookie=0x77df6d2c74c0a658, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.2, actions=output:LOCAL",
				}

				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows1))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.2_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows2))

				addConntrackMocks(netlinkMock, []ctFilterDesc{
					{"1.1.1.1", 8080},
					{"1.1.1.2", 8080},
					{"5.5.5.5", 8080},
					{"192.168.18.15", 31111},
					{"10.129.0.2", 8080},
				})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())
				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.2_8080"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules with ExternalIP through retry logic", func() {
			app.Action = func(ctx *cli.Context) error {
				var nodePortWatcherRetry *retry.RetryFramework
				var err error
				badExternalIP := "10.10.10.aa"
				goodExternalIP := "10.10.10.1"
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

				externalIPPort := int32(8034)
				service_ns := "namespace1"
				service_name := "service1"
				service := *newService(service_name, service_ns, "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     externalIPPort,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{badExternalIP}, // first use an incorrect IP
					v1.ServiceStatus{},
					false, false,
				)

				fakeOvnNode.start(ctx)

				By("starting node port watcher retry framework")
				fNPW.watchFactory = fakeOvnNode.watcher
				nodePortWatcherRetry, err = startNodePortWatcherWithRetry(
					fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig, fakeOvnNode.stopChan, fakeOvnNode.wg)
				Expect(err).NotTo(HaveOccurred())
				Expect(nodePortWatcherRetry).NotTo(BeNil())

				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeFalse(), fExec.ErrorDesc) // no command is executed

				By("add service with incorrect external IP")
				_, err = fakeOvnNode.fakeClient.KubeClient.CoreV1().Services(service_ns).Create(
					context.TODO(), &service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// expected ip tables with no external IP set
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				By("verify that a new retry entry for this service exists")
				key, err := retry.GetResourceKey(&service)
				Expect(err).NotTo(HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, nodePortWatcherRetry)
				// check iptables
				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				// HACK: Fix the service by setting a correct external IP address in newObj field
				// of the retry entry
				newObj := retry.GetNewObjFieldFromRetryObj(key, nodePortWatcherRetry)
				Expect(newObj).ToNot(BeNil())
				svc := newObj.(*v1.Service)
				svc.Spec.ExternalIPs = []string{goodExternalIP}
				ok := retry.SetNewObjFieldInRetryObj(key, nodePortWatcherRetry, svc)
				Expect(ok).To(BeTrue())

				By("trigger immediate retry")
				retry.SetRetryObjWithNoBackoff(key, nodePortWatcherRetry)
				nodePortWatcherRetry.RequestRetryObjs()
				retry.CheckRetryObjectEventually(key, false, nodePortWatcherRetry) // entry should be gone

				// now expect ip tables to show the external IP
				ovn_kube_external_ip_field := []string{
					fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v",
						service.Spec.Ports[0].Protocol, goodExternalIP, service.Spec.Ports[0].Port,
						service.Spec.ClusterIP, service.Spec.Ports[0].Port)}
				expectedTables["nat"]["OVN-KUBE-EXTERNALIP"] = ovn_kube_external_ip_field
				Eventually(func(g Gomega) {
					f4 := iptV4.(*util.FakeIPTables)
					err = f4.MatchState(expectedTables, nil)
					g.Expect(err).NotTo(HaveOccurred())
				})

				// TODO Make delete operation fail, check retry entry, run a successful delete
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules for NodePort", func() {
			app.Action = func(ctx *cli.Context) error {
				nodePort := int32(38034)

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					false, false,
				)

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)
				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, nodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 38034}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ETP=local, LGW", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				epPortValue := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, false,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, config.Gateway.MasqueradeIPs.V4HostETPLocalMasqueradeIP.String(), service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ETP=local, SGW", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				epPortName := "https"
				epPortValue := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will come
				// under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					// default
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by local-host-networked pods where ETP=local, LGW", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				outport := int32(443)
				epPortName := "https"
				epPortValue := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort:   int32(31111),
							Protocol:   v1.ProtocolTCP,
							Port:       int32(8080),
							TargetPort: intstr.FromInt(int(outport)),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, false,
				)

				ep1 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"}, // host-networked endpoint local to this node
					NodeName:  &fakeNodeName,
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				// to ensure the endpoint is local-host-networked
				res := fNPW.nodeIPManager.cidrs.Has(fmt.Sprintf("%s/32", ep1.Addresses[0]))
				Expect(res).To(BeTrue())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=ct(commit,zone=64003,nat(dst=10.244.0.1:443),table=6)",
					"cookie=0xe745ecf105, priority=110, table=6, actions=output:LOCAL",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=LOCAL, tcp, tp_src=443, actions=ct(zone=64003 nat,table=7)",
					"cookie=0xe745ecf105, priority=110, table=7, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by ovn-k pods where ITP=local and ETP=local", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				epPortName := "https"
				epPortValue := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: int32(31111),
							Protocol: v1.ProtocolTCP,
							Port:     int32(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, true,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"10.244.0.3"},
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP":        []string{},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j MARK --set-xmark %s", service.Spec.Ports[0].Protocol, service.Spec.ClusterIP, service.Spec.Ports[0].Port, ovnkubeITPMark),
						},
					},
				}
				expectedFlows := []string{
					// default
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=output:patch-breth0_ov",
					fmt.Sprintf("cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, dl_src=%s, tcp, tp_src=31111, actions=output:eth0",
						gwMAC),
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-ITP":        []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules and openflows for NodePort backed by local-host-networked pods where ETP=local and ITP=local", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeLocal
				epPortName := "https"
				outport := int32(443)
				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort:   int32(31111),
							Protocol:   v1.ProtocolTCP,
							Port:       int32(8080),
							TargetPort: intstr.FromInt(int(outport)),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
					v1.ServiceStatus{},
					true, true,
				)
				ep1 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"}, // host-networked endpoint local to this node
					NodeName:  &fakeNodeName,
				}
				epPort1 := discovery.EndpointPort{
					Name: &epPortName,
					Port: &outport,
				}
				// endpointSlice.Endpoints is host-networked so this will
				// come under hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1},
					[]discovery.EndpointPort{epPort1})

				fakeOvnNode.start(ctx,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
					&endpointSlice,
				)

				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				// to ensure the endpoint is local-host-networked
				res := fNPW.nodeIPManager.cidrs.Has(fmt.Sprintf("%s/32", endpointSlice.Endpoints[0].Addresses[0]))
				Expect(res).To(BeTrue())
				err := fNPW.AddService(&service)
				Expect(err).NotTo(HaveOccurred())
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP":    []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ITP": []string{
							fmt.Sprintf("-p %s -d %s --dport %d -j REDIRECT --to-port %d", service.Spec.Ports[0].Protocol, service.Spec.ClusterIP, service.Spec.Ports[0].Port, int32(service.Spec.Ports[0].TargetPort.IntValue())),
						},
						"OVN-KUBE-ETP":        []string{},
						"OVN-KUBE-EGRESS-SVC": []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}
				expectedFlows := []string{
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=ct(commit,zone=64003,nat(dst=10.244.0.1:443),table=6)",
					"cookie=0xe745ecf105, priority=110, table=6, actions=output:LOCAL",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=LOCAL, tcp, tp_src=443, actions=ct(zone=64003 nat,table=7)",
					"cookie=0xe745ecf105, priority=110, table=7, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				addConntrackMocks(netlinkMock, []ctFilterDesc{{"10.129.0.2", 8080}, {"192.168.18.15", 31111}})
				err = fNPW.DeleteService(&service)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-NODEPORT":   []string{},
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("disable-forwarding", func() {
		It("adds or removes iptables rules upon change in forwarding mode", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableForwarding = true
				fakeOvnNode.start(ctx)
				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {
						"FORWARD": []string{
							"-d 169.254.169.1 -j ACCEPT",
							"-s 169.254.169.1 -j ACCEPT",
							"-d 172.16.1.0/24 -j ACCEPT",
							"-s 172.16.1.0/24 -j ACCEPT",
							"-d 10.1.0.0/16 -j ACCEPT",
							"-s 10.1.0.0/16 -j ACCEPT",
						},
					},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables, map[util.FakePolicyKey]string{{
					Table: "filter",
					Chain: "FORWARD",
				}: "DROP"})
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())

				// Enable forwarding and test deletion of iptables rules from FORWARD chain
				config.Gateway.DisableForwarding = false
				fNPW.watchFactory = fakeOvnNode.watcher
				Expect(configureGlobalForwarding()).To(Succeed())
				Expect(startNodePortWatcher(fNPW, fakeOvnNode.fakeClient, &fakeMgmtPortConfig)).To(Succeed())
				expectedTables = map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-ETP",
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
						"POSTROUTING": []string{
							"-j OVN-KUBE-EGRESS-SVC",
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-ETP":           []string{},
						"OVN-KUBE-ITP":           []string{},
						"OVN-KUBE-EGRESS-SVC":    []string{},
					},
					"filter": {
						"FORWARD": []string{},
					},
					"mangle": {
						"OUTPUT": []string{
							"-j OVN-KUBE-ITP",
						},
						"OVN-KUBE-ITP": []string{},
					},
				}

				f4 = iptV4.(*util.FakeIPTables)
				err = f4.MatchState(expectedTables, map[util.FakePolicyKey]string{{
					Table: "filter",
					Chain: "FORWARD",
				}: "ACCEPT"})
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 = iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables, nil)
				Expect(err).NotTo(HaveOccurred())
				return nil
			}
			err := app.Run([]string{
				app.Name,
				"--cluster-subnets=" + "10.1.0.0/16",
				"--k8s-service-cidrs=" + "172.16.1.0/24",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

})
