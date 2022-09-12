package node

import (
	"fmt"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
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
)

func initFakeNodePortWatcher(iptV4, iptV6 util.IPTablesHelper) *nodePortWatcher {
	initIPTable := map[string]util.FakeTable{
		"nat":    {},
		"filter": {},
		"mangle": {},
	}

	f4 := iptV4.(*util.FakeIPTables)
	err := f4.MatchState(initIPTable)
	Expect(err).NotTo(HaveOccurred())

	f6 := iptV6.(*util.FakeIPTables)
	err = f6.MatchState(initIPTable)
	Expect(err).NotTo(HaveOccurred())

	fNPW := nodePortWatcher{
		ofportPhys:        "eth0",
		ofportPatch:       "patch-breth0_ov",
		gatewayIPv4:       v4localnetGatewayIP,
		gatewayIPv6:       v6localnetGatewayIP,
		nodeName:          "mynode",
		serviceInfo:       make(map[k8stypes.NamespacedName]*serviceConfig),
		egressServiceInfo: make(map[k8stypes.NamespacedName]*serviceEps),
		ofm: &openflowManager{
			flowCache: map[string][]string{},
		},
	}
	return &fNPW
}

func startNodePortWatcher(n *nodePortWatcher, fakeClient *util.OVNClientset, fakeMgmtPortConfig *managementPortConfig) error {
	if err := initLocalGatewayIPTables(); err != nil {
		return err
	}

	k := &kube.Kube{fakeClient.KubeClient, nil, nil, nil}
	n.nodeIPManager = newAddressManager(fakeNodeName, k, fakeMgmtPortConfig, n.watchFactory)
	localHostNetEp := "192.168.18.15/32"
	ip, _, _ := net.ParseCIDR(localHostNetEp)
	n.nodeIPManager.addAddr(ip)

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
			Ports:                 ports,
			Type:                  serviceType,
			ExternalIPs:           externalIPs,
			ExternalTrafficPolicy: externalTrafficPolicy,
			InternalTrafficPolicy: &internalTrafficPolicy,
		},
		Status: serviceStatus,
	}
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

var _ = Describe("Node Operations", func() {
	var (
		app                *cli.App
		fakeOvnNode        *FakeOVNNode
		fExec              *ovntest.FakeExec
		iptV4, iptV6       util.IPTablesHelper
		fNPW               *nodePortWatcher
		fakeMgmtPortConfig managementPortConfig
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		fExec = ovntest.NewFakeExec()
		fakeOvnNode = NewFakeOVNNode(fExec)

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

	Context("on startup", func() {
		It("removes stale iptables rules while keeping remaining intact", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				externalIPPort := int32(8032)
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

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
				Expect(addIptRules(fakeRules)).To(Succeed())
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
				Expect(addIptRules(fakeRules)).To(Succeed())

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p UDP -d 10.10.10.10 --dport 27000 -j DNAT --to-destination 172.32.0.12:27000"),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
					"filter": {},
					"mangle": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fakeOvnNode.shutdown()
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fNPW.AddService(&service)
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				fakeOvnNode.shutdown()
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with LoadBalancer", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
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
				fNPW.AddService(&service)
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				fakeOvnNode.shutdown()
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				fakeOvnNode.shutdown()
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
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
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
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=check_pkt_larger(1414)->reg0[0],resubmit(,11)",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, tcp, tp_src=31111, actions=output:eth0",
				}
				expectedLBIngressFlows := []string{
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=5.5.5.5, actions=output:LOCAL",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=eth0, tcp, nw_dst=5.5.5.5, tp_dst=8080, actions=check_pkt_larger(1414)->reg0[0],resubmit(,11)",
					"cookie=0x10c6b89e483ea111, priority=110, in_port=patch-breth0_ov, tcp, nw_src=5.5.5.5, tp_src=8080, actions=output:eth0",
				}
				expectedLBExternalIPFlows := []string{
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, arp, arp_op=1, arp_tpa=1.1.1.1, actions=output:LOCAL",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=eth0, tcp, nw_dst=1.1.1.1, tp_dst=8080, actions=check_pkt_larger(1414)->reg0[0],resubmit(,11)",
					"cookie=0x71765945a31dc2f1, priority=110, in_port=patch-breth0_ov, tcp, nw_src=1.1.1.1, tp_src=8080, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedNodePortFlows))
				flows = fNPW.ofm.flowCache["Ingress_namespace1_service1_5.5.5.5_8080"]
				Expect(flows).To(Equal(expectedLBIngressFlows))
				flows = fNPW.ofm.flowCache["External_namespace1_service1_1.1.1.1_8080"]
				Expect(flows).To(Equal(expectedLBExternalIPFlows))

				fakeOvnNode.shutdown()
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
				fNPW.AddService(&service)
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
				err := f4.MatchState(expectedTables4)
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
				err = f6.MatchState(expectedTables6)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})

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
				fNPW.AddService(&service)
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
				err := f4.MatchState(expectedTables4)
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
				err = f6.MatchState(expectedTables6)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fNPW.DeleteService(&service)
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fNPW.DeleteService(&service)
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat":    {},
					"filter": {},
					"mangle": {},
				}

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
				})
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
				fNPW.AddService(&service)
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
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				fakeOvnNode.shutdown()
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
				fNPW.AddService(&service)
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
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
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fakeOvnNode.shutdown()
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
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
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
				expectedFlows := []string{
					// default
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=check_pkt_larger(1414)->reg0[0],resubmit(,11)",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, tcp, tp_src=31111, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fakeOvnNode.shutdown()
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
				res := fNPW.nodeIPManager.addresses.Has(ep1.Addresses[0])
				Expect(res).To(BeTrue())
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fakeOvnNode.shutdown()
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
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-EXTERNALIP": []string{},
						"OVN-KUBE-SNAT-MGMTPORT": []string{
							fmt.Sprintf("-p TCP --dport %v -j RETURN", service.Spec.Ports[0].NodePort),
						},
						"OVN-KUBE-ITP": []string{},
						"OVN-KUBE-ETP": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, types.V4HostETPLocalMasqueradeIP, service.Spec.Ports[0].NodePort),
						},
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
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=eth0, tcp, tp_dst=31111, actions=check_pkt_larger(1414)->reg0[0],resubmit(,11)",
					"cookie=0x453ae29bcbbc08bd, priority=110, in_port=patch-breth0_ov, tcp, tp_src=31111, actions=output:eth0",
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fakeOvnNode.shutdown()
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
				res := fNPW.nodeIPManager.addresses.Has(endpointSlice.Endpoints[0].Addresses[0])
				Expect(res).To(BeTrue())
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())
				flows := fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(Equal(expectedFlows))

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				flows = fNPW.ofm.flowCache["NodePort_namespace1_service1_tcp_31111"]
				Expect(flows).To(BeNil())

				fakeOvnNode.shutdown()
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("manages iptables rules for LoadBalancer egress service backed by ovn-k pods", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.Mode = config.GatewayModeShared
				_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
				config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{cidr4, 24}}
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ovs-ofctl show ",
					Err: fmt.Errorf("deliberate error to fall back to output:LOCAL"),
				})
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
					v1.ServiceTypeLoadBalancer,
					[]string{},
					v1.ServiceStatus{
						LoadBalancer: v1.LoadBalancerStatus{
							Ingress: []v1.LoadBalancerIngress{{
								IP: "5.5.5.5",
							}},
						},
					},
					false, false,
				)
				service.Annotations[util.EgressSVCHostAnnotation] = "mynode"
				service.Annotations[util.EgressSVCAnnotation] = "{}"

				ep1 := discovery.Endpoint{
					Addresses: []string{"10.128.0.3"},
				}
				epPort := discovery.EndpointPort{
					Name: &epPortName,
					Port: &epPortValue,
				}

				// host-networked endpoint, should not have an SNAT rule created
				ep2 := discovery.Endpoint{
					Addresses: []string{"192.168.18.15"},
				}
				// endpointSlice.Endpoints is ovn-networked so this will
				// come under !hasLocalHostNetEp case
				endpointSlice := *newEndpointSlice(
					"service1",
					"namespace1",
					[]discovery.Endpoint{ep1, ep2},
					[]discovery.EndpointPort{epPort})

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
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-SNAT-MGMTPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Status.LoadBalancer.Ingress[0].IP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
						"OVN-KUBE-ETP": []string{},
						"OVN-KUBE-ITP": []string{},
						"OVN-KUBE-EGRESS-SVC": []string{
							"-s 10.128.0.3 -m comment --comment namespace1/service1 -j SNAT --to-source 5.5.5.5",
						},
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
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fNPW.DeleteService(&service)

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
				err = f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fakeOvnNode.shutdown()
				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
