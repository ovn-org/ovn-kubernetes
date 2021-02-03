package node

import (
	"fmt"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

const (
	v4localnetGatewayIP = "10.244.0.1"
	v6localnetGatewayIP = "fd00:96:1::1"
)

func getFakeLocalAddrs() map[string]net.IPNet {
	localAddrSet := make(map[string]net.IPNet)
	for _, network := range []string{"127.0.0.1/32", "10.10.10.1/24", "fd00:96:1::1/64"} {
		ip, ipNet, err := net.ParseCIDR(network)
		Expect(err).NotTo(HaveOccurred())
		localAddrSet[ip.String()] = *ipNet
	}
	return localAddrSet
}

func initFakeNodePortWatcher(fakeOvnNode *FakeOVNNode, iptV4, iptV6 util.IPTablesHelper) *localPortWatcher {
	initIPTable := map[string]util.FakeTable{
		"nat": {},
	}

	f4 := iptV4.(*util.FakeIPTables)
	err := f4.MatchState(initIPTable)
	Expect(err).NotTo(HaveOccurred())

	f6 := iptV6.(*util.FakeIPTables)
	err = f6.MatchState(initIPTable)
	Expect(err).NotTo(HaveOccurred())

	fNPW := localPortWatcher{
		recorder:     fakeOvnNode.recorder,
		gatewayIPv4:  v4localnetGatewayIP,
		localAddrSet: getFakeLocalAddrs(),
	}
	return &fNPW
}

func startLocalPortWatcher(l *localPortWatcher, wf factory.NodeWatchFactory) error {
	if err := initLocalGatewayIPTables(); err != nil {
		return err
	}
	if err := initRoutingRules(); err != nil {
		return err
	}
	wf.AddServiceHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			l.AddService(svc)
		},
		UpdateFunc: func(old, new interface{}) {
			oldSvc := old.(*kapi.Service)
			newSvc := new.(*kapi.Service)
			l.UpdateService(oldSvc, newSvc)
		},
		DeleteFunc: func(obj interface{}) {
			svc := obj.(*kapi.Service)
			l.DeleteService(svc)
		},
	}, l.SyncServices)
	return nil
}

func newServiceMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       k8stypes.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newService(name, namespace, ip string, ports []v1.ServicePort, serviceType v1.ServiceType, externalIPs []string) *v1.Service {
	return &v1.Service{
		ObjectMeta: newServiceMeta(name, namespace),
		Spec: v1.ServiceSpec{
			ClusterIP:   ip,
			Ports:       ports,
			Type:        serviceType,
			ExternalIPs: externalIPs,
		},
	}
}

var _ = Describe("Node Operations", func() {
	var (
		app          *cli.App
		fakeOvnNode  *FakeOVNNode
		fExec        *ovntest.FakeExec
		iptV4, iptV6 util.IPTablesHelper
		fNPW         *localPortWatcher
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fExec = ovntest.NewFakeExec()
		fakeOvnNode = NewFakeOVNNode(fExec)
		iptV4, iptV6 = util.SetFakeIPTablesHelpers()
		fNPW = initFakeNodePortWatcher(fakeOvnNode, iptV4, iptV6)
	})

	Context("on startup", func() {
		It("inits physical routing rules", func() {
			app.Action = func(ctx *cli.Context) error {
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ip rule",
					Output: "0:	from all lookup local\n32766:	from all lookup main\n32767:	from all lookup default\n",
				})
				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					"ip rule add from all table " + localnetGatewayExternalIDTable,
				})
				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					"ip route list table " + localnetGatewayExternalIDTable,
				})

				fakeOvnNode.start(ctx)
				startLocalPortWatcher(fNPW, fakeOvnNode.watcher)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OVN-KUBE-NODEPORT":   []string{},
						"OVN-KUBE-EXTERNALIP": []string{},
					},
				}
				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				expectedTables = map[string]util.FakeTable{
					"nat": {},
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

		It("removes stale physical routing rules while keeping remaining intact", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"
				externalIPPort := int32(8032)

				// Create some fake routing and iptable rules
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: "ip rule",
					Output: "0:	from all lookup local\n32766:	from all lookup main\n32767:	from all lookup default\n",
				})
				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					"ip rule add from all table " + localnetGatewayExternalIDTable,
				})
				fakeOvnNode.fakeExec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ip route list table " + localnetGatewayExternalIDTable,
					Output: fmt.Sprintf("%s via %s dev %s\n9.9.9.9 via %s dev %s\n", externalIP, v4localnetGatewayIP, types.K8sMgmtIntfName, v4localnetGatewayIP, types.K8sMgmtIntfName),
				})
				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ip route del 9.9.9.9 via %s dev %s table %s", v4localnetGatewayIP, types.K8sMgmtIntfName, localnetGatewayExternalIDTable),
				})
				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ip route replace %s via %s dev %s table %s", externalIP, v4localnetGatewayIP, types.K8sMgmtIntfName, localnetGatewayExternalIDTable),
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
				)

				fakeRules := getExternalIPTRules(service.Spec.Ports[0], externalIP, service.Spec.ClusterIP)
				addIptRules(fakeRules)
				fakeRules = getExternalIPTRules(
					v1.ServicePort{
						Port:     27000,
						Protocol: v1.ProtocolUDP,
						Name:     "This is going to dissapear I hope",
					},
					"10.10.10.10",
					"172.32.0.12",
				)
				addIptRules(fakeRules)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p UDP -d 10.10.10.10 --dport 27000 -j DNAT --to-destination 172.32.0.12:27000"),
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
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
				startLocalPortWatcher(fNPW, fakeOvnNode.watcher)
				Expect(fakeOvnNode.fakeExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"PREROUTING": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OUTPUT": []string{
							"-j OVN-KUBE-EXTERNALIP",
							"-j OVN-KUBE-NODEPORT",
						},
						"OVN-KUBE-NODEPORT": []string{},
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
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
		It("inits physical routing rules with ExternalIP outside any local network", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ip route replace %s via %s dev %s table %s", externalIP, v4localnetGatewayIP, types.K8sMgmtIntfName, localnetGatewayExternalIDTable),
				})

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does nothing when ExternalIP on shared network", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.2"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules when ExternalIP attached to network interface", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.1"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("inits iptables rules with NodePort", func() {
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
				)

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

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
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)
				service.Spec.ClusterIPs = []string{"10.129.0.2", "fd00:10:96::10"}
				fNPW.addService(&service)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, service.Spec.Ports[0].NodePort, service.Spec.ClusterIPs[0], service.Spec.Ports[0].Port),
						},
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
				}
				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6)
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

				service := *newService("service1", "namespace1", clusterIPv4,
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIPv4, externalIPv6},
				)
				service.Spec.ClusterIPs = []string{clusterIPv4, clusterIPv6}
				fNPW.addService(&service)

				expectedTables4 := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIPv4, service.Spec.Ports[0].Port, clusterIPv4, service.Spec.Ports[0].Port),
						},
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
				}

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables6)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on delete", func() {
		It("deletes physical routing rules with ExternalIP outside any local network", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "1.1.1.1"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fakeOvnNode.fakeExec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ip route del %s via %s dev %s table %s", externalIP, v4localnetGatewayIP, types.K8sMgmtIntfName, localnetGatewayExternalIDTable),
				})

				fNPW.deleteService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does nothing when ExternalIP on shared network", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.2"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fNPW.deleteService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes iptables rules with ExternalIP attached to network interface", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.1"

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					[]string{externalIP},
				)

				fNPW.deleteService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
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
				)

				fNPW.deleteService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				f6 := iptV6.(*util.FakeIPTables)
				err = f6.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on add and delete", func() {
		It("manages iptables rules with ExternalIP attached to network interface", func() {
			app.Action = func(ctx *cli.Context) error {
				externalIP := "10.10.10.1"
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
				)

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{
							fmt.Sprintf("-p %s -d %s --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, externalIP, service.Spec.Ports[0].Port, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fNPW.deleteService(&service)

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-EXTERNALIP": []string{},
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

		It("manages iptables rules for NodePort", func() {
			app.Action = func(ctx *cli.Context) error {
				nodePort := int32(38034)

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							NodePort: nodePort,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)

				fNPW.addService(&service)

				expectedTables := map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{
							fmt.Sprintf("-p %s -m addrtype --dst-type LOCAL --dport %v -j DNAT --to-destination %s:%v", service.Spec.Ports[0].Protocol, nodePort, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
						},
					},
				}

				f4 := iptV4.(*util.FakeIPTables)
				err := f4.MatchState(expectedTables)
				Expect(err).NotTo(HaveOccurred())

				fNPW.deleteService(&service)

				expectedTables = map[string]util.FakeTable{
					"nat": {
						"OVN-KUBE-NODEPORT": []string{},
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
	})
})
