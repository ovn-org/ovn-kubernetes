// +build linux

package node

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"strconv"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/coreos/go-iptables/iptables"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"

	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ovnNodeL3GatewayConfig   = "k8s.ovn.org/l3-gateway-config"
	ovnNodeChassisID         = "k8s.ovn.org/node-chassis-id"
	interfaceId              = "eth0"
	macAddress               = "00:00:a9:fe:21:01"
	systemID                 = "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"
	ovnDefaultNetworkGateway = "default"
)

var _ = Describe("Gateway localnet unit tests", func() {
	const (
		v4Nodesubnet1 string = "10.1.1.0/24"
		v6Nodesubnet1 string = "2001:db8:abcd:0012::0/64"
	)
	var (
		subnets                    []*net.IPNet
		v4Ipt, v6Ipt               *util.FakeIPTables
		ipv4LocalNet, ipv6LocalNet *localnetData
	)
	BeforeEach(func() {
		subnets = make([]*net.IPNet, 0)
		var err error

		v4Ipt, err = util.NewFakeWithProtocol(iptables.ProtocolIPv4)
		Expect(err).NotTo(HaveOccurred())
		util.SetIPTablesHelper(iptables.ProtocolIPv4, v4Ipt)

		v6Ipt, err = util.NewFakeWithProtocol(iptables.ProtocolIPv6)
		Expect(err).NotTo(HaveOccurred())
		util.SetIPTablesHelper(iptables.ProtocolIPv6, v6Ipt)

		ipv4LocalNet = &localnetData{
			ipVersion:         ipv4,
			ipt:               v4Ipt,
			gatewayIP:         net.ParseIP(v4localnetGatewayIP),
			gatewayNextHop:    net.ParseIP(v4localnetGatewayNextHop),
			gatewaySubnetMask: net.CIDRMask(v4localnetGatewaySubnetPrefix, 32),
		}
		ipv6LocalNet = &localnetData{
			ipVersion:         ipv6,
			ipt:               v6Ipt,
			gatewayIP:         net.ParseIP(v6localnetGatewayIP),
			gatewayNextHop:    net.ParseIP(v6localnetGatewayNextHop),
			gatewaySubnetMask: net.CIDRMask(v6localnetGatewaySubnetPrefix, 128),
		}
	})

	Context("Tests for Localnet data structure creation", func() {
		var (
			expectedLocalnetData []*localnetData
		)
		BeforeEach(func() {
			expectedLocalnetData = make([]*localnetData, 0)
		})

		It("An IPv4 subnet should result in IPv4 localnet structure", func() {
			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			expectedLocalnetData = append(expectedLocalnetData, ipv4LocalNet)
			validateLocalnetData(subnets, expectedLocalnetData)
		})

		It("An IPv6 subnet should result in IPv6 localnet structure", func() {
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
			expectedLocalnetData = append(expectedLocalnetData, ipv6LocalNet)
			validateLocalnetData(subnets, expectedLocalnetData)
		})

		It("IPv4 and  IPv6 subnets should result in both IPv4 and IPv6 localnet structures", func() {
			subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
			subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
			expectedLocalnetData = append(expectedLocalnetData, ipv4LocalNet)
			expectedLocalnetData = append(expectedLocalnetData, ipv6LocalNet)
			validateLocalnetData(subnets, expectedLocalnetData)
		})

	})

	Context("Tests for localnet gateway bridge create", func() {
		var (
			fexec    *ovntest.FakeExec
			ovctlCmd = "ovs-vsctl --timeout=15 --may-exist add-br br-local"
		)
		BeforeEach(func() {
			fexec = ovntest.NewFakeExec()
		})

		It("Happy path bridge creation", func() {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				ovctlCmd,
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = createLocalnetBridge("br-local")
			Expect(err).NotTo(HaveOccurred())
		})

		It("When undelying bridge creation fails createLocalnetBridge must return error", func() {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: ovctlCmd,
				Err: fmt.Errorf("Error creating the bridge"),
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = createLocalnetBridge("br-local")
			Expect(err).NotTo(BeNil())
		})
	})

	Context("Tests for localnet gateway port create", func() {
		var (
			fexec    *ovntest.FakeExec
			ovctlCmd = "ovs-vsctl --timeout=15 --if-exists del-port br-local " + legacyLocalnetGatewayNextHopPort +
				" -- --may-exist add-port br-local " + localnetGatewayNextHopPort + " -- set interface " +
				localnetGatewayNextHopPort + " type=internal mtu_request=" + fmt.Sprintf("%d", config.Default.MTU) +
				" mac=00\\:00\\:a9\\:fe\\:21\\:01"
		)
		BeforeEach(func() {
			fexec = ovntest.NewFakeExec()
		})

		It("Happy path port creation", func() {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				ovctlCmd,
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = createGatewayPort("br-local")
			Expect(err).NotTo(HaveOccurred())
		})

		It("When undelying port creation fails createGatewayPort must return error", func() {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: ovctlCmd,
				Err: fmt.Errorf("Error creating the port"),
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = createGatewayPort("br-local")
			Expect(err).NotTo(BeNil())
		})
	})

	Context("Tests for IP address assignment to the gateway port", func() {
		var (
			testNS                           ns.NetNS
			link                             netlink.Link
			v4NextHopCIDRIP, v6NextHopCIDRIP *netlink.Addr
		)
		BeforeEach(func() {
			var err error

			v4BrNextHopCIDR := v4localnetGatewayNextHop + "/" + strconv.Itoa(v4localnetGatewaySubnetPrefix)
			v4NextHopCIDRIP, err = netlink.ParseAddr(v4BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())

			v6BrNextHopCIDR := v6localnetGatewayNextHop + "/" + strconv.Itoa(v6localnetGatewaySubnetPrefix)
			v6NextHopCIDRIP, err = netlink.ParseAddr(v6BrNextHopCIDR)
			Expect(err).NotTo(HaveOccurred())

			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				err := netlink.LinkAdd(&netlink.Dummy{
					LinkAttrs: netlink.LinkAttrs{
						Name: localnetGatewayNextHopPort,
					},
				})
				Expect(err).NotTo(HaveOccurred())
				link, err = netlink.LinkByName(localnetGatewayNextHopPort)
				Expect(err).NotTo(HaveOccurred())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
		AfterEach(func() {
			Expect(testNS.Close()).To(Succeed())
		})

		It("ipv4 localnet gateway should assign ipv4 address to the port", func() {
			validateIPAddressAsssignment(testNS, link, []*localnetData{ipv4LocalNet}, []*netlink.Addr{v4NextHopCIDRIP})
		})

		It("ipv6 localnet gateway should assign ipv6 address to the port", func() {
			validateIPAddressAsssignment(testNS, link, []*localnetData{ipv6LocalNet}, []*netlink.Addr{v6NextHopCIDRIP})
		})

		It("dualstack localnet gateway should assign both ipv4 and ipv6 addresses to the port", func() {
			validateIPAddressAsssignment(testNS, link, []*localnetData{ipv4LocalNet, ipv6LocalNet},
				[]*netlink.Addr{v4NextHopCIDRIP, v6NextHopCIDRIP})
		})
	})

	Context("Tests for MAC/IP binding on the gateway port in case of ipv6 ", func() {
		var (
			testNS     ns.NetNS
			link       netlink.Link
			macAddress net.HardwareAddr
		)
		BeforeEach(func() {
			var err error
			macAddress, err = net.ParseMAC("01:01:a9:fe:21:01")
			Expect(err).NotTo(HaveOccurred())

			testNS, err = testutils.NewNS()
			Expect(err).NotTo(HaveOccurred())
			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				err := netlink.LinkAdd(&netlink.Dummy{
					LinkAttrs: netlink.LinkAttrs{
						Name: localnetGatewayNextHopPort,
					},
				})
				Expect(err).NotTo(HaveOccurred())
				link, err = netlink.LinkByName(localnetGatewayNextHopPort)
				Expect(err).NotTo(HaveOccurred())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
		AfterEach(func() {
			Expect(testNS.Close()).To(Succeed())
		})

		It("Neighbor entry for ipv6 should be added when there is no binding", func() {
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				ipV6MacBindingWorkaround(link, macAddress, []*localnetData{ipv6LocalNet})
				exists, err := util.LinkNeighExists(link, ipv6LocalNet.gatewayIP, macAddress)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("linkNeighExists method should return false when there is no MAC binding", func() {
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				exists := linkNeighExists(link, ipv6LocalNet.gatewayIP, macAddress)
				Expect(exists).To(BeFalse())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("linkNeighExists method should return true when there is a MAC binding", func() {
			err := testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()
				err := util.LinkNeighAdd(link, ipv6LocalNet.gatewayIP, macAddress)
				Expect(err).NotTo(HaveOccurred())
				exists := linkNeighExists(link, ipv6LocalNet.gatewayIP, macAddress)
				Expect(exists).To(BeTrue())
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("Tests for L3Gateway configuration", func() {
		var (
			lnData []*localnetData
		)
		BeforeEach(func() {
			lnData = make([]*localnetData, 0)
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:system-id",
				Output: systemID,
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			config.Gateway = config.GatewayConfig{Mode: "local", Interface: "eth0", NextHop: "192.168.1.1",
				VLANID: 200, NodeportEnable: true}

		})

		It("An IPv4 localnet should set IPv4 L3gateway annotations", func() {
			validateL3GatewayData(append(lnData, ipv4LocalNet))
		})

		It("An IPv6 localnet should set IPv6 L3gateway annotations", func() {
			validateL3GatewayData(append(lnData, ipv6LocalNet))
		})

		It("Dual stack localnet should set dual stack L3gateway annotations", func() {
			validateL3GatewayData(append(append(lnData, ipv4LocalNet), ipv6LocalNet))
		})

	})

	Context("Tests for localnet gateway iptables configuration", func() {
		It("IPv4 lonalnet gateway must configure ipv4 rules", func() {
			validateLocalnetGatewayRules([]*localnetData{ipv4LocalNet}, []*util.FakeIPTables{v4Ipt})
		})
		It("IPv6 lonalnet gateway must configure ipv6 rules", func() {
			validateLocalnetGatewayRules([]*localnetData{ipv6LocalNet}, []*util.FakeIPTables{v6Ipt})
		})
		It("Dual stack lonalnet gateway must configure both ipv4 and ipv6 rules", func() {
			validateLocalnetGatewayRules([]*localnetData{ipv4LocalNet, ipv6LocalNet}, []*util.FakeIPTables{v4Ipt, v6Ipt})
		})
	})

	Describe("Tests for NodePort services", func() {
		const (
			nodePort = 80
		)
		var (
			lnData          []*localnetData
			portWatcherData localnetNodePortWatcherData
			service         *kapi.Service
		)
		BeforeEach(func() {
			lnData = make([]*localnetData, 0)
			lnData = append(append(lnData, ipv4LocalNet), ipv6LocalNet)
			portWatcherData = localnetNodePortWatcherData{localNetdata: lnData}

			service = &kapi.Service{
				TypeMeta: metav1.TypeMeta{
					Kind: "service",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-service",
				},
				Spec: kapi.ServiceSpec{
					ClusterIP: "192.168.1.1",
					Type:      kapi.ServiceTypeNodePort,
					Ports: []kapi.ServicePort{{
						Name:     "test-port",
						Protocol: "TCP",
						Port:     8080,
						NodePort: nodePort,
					}},
				},
			}
		})

		Context("Tests for IPTable helper selection for Nodeport services", func() {
			BeforeEach(func() {
				subnets = append(subnets, ovntest.MustParseIPNet(v4Nodesubnet1))
				subnets = append(subnets, ovntest.MustParseIPNet(v6Nodesubnet1))
			})

			It("IPv4 cluster IP should result in Ipv4 IPT ", func() {
				ipData, err := constructLocalnetData(subnets)
				Expect(err).NotTo(HaveOccurred())

				service := &kapi.Service{
					Spec: kapi.ServiceSpec{
						ClusterIP: "192.168.1.1",
					},
				}
				localnetdata := getLocalNetDataForService(ipData, service)
				Expect(localnetdata).NotTo(BeNil())
				Expect(localnetdata.ipVersion).To(Equal(4))

			})

			It("IPv6 cluster IP should result in Ipv6 IPT ", func() {
				ipData, err := constructLocalnetData(subnets)
				Expect(err).NotTo(HaveOccurred())

				service := &kapi.Service{
					Spec: kapi.ServiceSpec{
						ClusterIP: "fd99::2",
					},
				}
				localnetdata := getLocalNetDataForService(ipData, service)
				Expect(localnetdata).NotTo(BeNil())
				Expect(localnetdata.ipVersion).To(Equal(6))

			})

		})

		Context("Add Nodeport service tests", func() {
			It("An IPv4 node port service should install IPv4 iptables rules", func() {
				validateNodePortAddService(portWatcherData, service, v4Ipt, "192.168.1.1", nodePort,
					v4localnetGatewayIP)
			})

			It("An IPv6 node port service should install IPv6 iptables rules", func() {
				validateNodePortAddService(portWatcherData, service, v6Ipt, "fd99::2", nodePort,
					v6localnetGatewayIP)
			})
		})

		Context("Delete Nodeport service tests", func() {
			It("Deleting IPv4 node port service should delete IPv4 iptables rules", func() {
				validateNodePortDeleteService(portWatcherData, service, v4Ipt, "192.168.1.1", nodePort,
					v4localnetGatewayIP)
			})

			It("Deleting IPv6 node port service should delete IPv6 iptables rules", func() {
				validateNodePortDeleteService(portWatcherData, service, v6Ipt, "fd99::2", nodePort,
					v6localnetGatewayIP)
			})
		})

		Context("Base Nodeport watcher tests", func() {
			It("IPv4 localnet should install IPv4 iptables base rules", func() {
				validateNodePortBaseRules([]*localnetData{ipv4LocalNet}, []*util.FakeIPTables{v4Ipt})
			})
			It("IPv6 localnet should install IPv6 iptables base rules", func() {
				validateNodePortBaseRules([]*localnetData{ipv6LocalNet}, []*util.FakeIPTables{v6Ipt})
			})
			It("Dual stack localnet should install both IPv4 and IPv6 iptables base rules", func() {
				validateNodePortBaseRules([]*localnetData{ipv4LocalNet, ipv6LocalNet}, []*util.FakeIPTables{v4Ipt, v6Ipt})
			})
		})
	})

	Context("Tests for clearing Nodeport iptables rules", func() {
		It("clearOvnNodeportRules must clear rules for IPv4", func() {
			validateClearIPTableForNodeport([]*localnetData{ipv4LocalNet}, []*util.FakeIPTables{v4Ipt})
		})
		It("clearOvnNodeportRules must clear rules for IPv6", func() {
			validateClearIPTableForNodeport([]*localnetData{ipv6LocalNet}, []*util.FakeIPTables{v6Ipt})
		})
		It("clearOvnNodeportRules must clear rules for dual stack", func() {
			validateClearIPTableForNodeport([]*localnetData{ipv4LocalNet, ipv6LocalNet}, []*util.FakeIPTables{v4Ipt, v6Ipt})
		})

	})

	Context("Tests for deleting localnet gateway", func() {
		var (
			fexec    *ovntest.FakeExec
			ovctlCmd = "ovs-vsctl --timeout=15 -- --if-exists del-br br-local"
		)
		BeforeEach(func() {
			fexec = ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-bridge-mappings",
				Output: "net:br-local",
			})
		})

		It("Deleting the localnet gateway should delete the local bridge", func() {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				ovctlCmd,
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = cleanupLocalnetGateway()
			Expect(err).NotTo(HaveOccurred())
		})
		It("If error occurs in the underlying bridge delete cleanupLocalnetGateway must return error", func() {
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: ovctlCmd,
				Err: fmt.Errorf("Error deleting the bridge"),
			})
			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())
			err = cleanupLocalnetGateway()
			Expect(err).NotTo(BeNil())
		})
	})

})

func validateIPAddressAsssignment(testNS ns.NetNS, link netlink.Link, lnData []*localnetData,
	expectedIPs []*netlink.Addr) {

	err := testNS.Do(func(ns.NetNS) error {
		defer GinkgoRecover()
		err := addLinkAddresses(link, lnData)
		Expect(err).NotTo(HaveOccurred())

		addrs, err := netlink.AddrList(link, 0)
		Expect(err).NotTo(HaveOccurred())

		var foundAddr bool
		for _, expectedAddr := range expectedIPs {
			foundAddr = false
			for _, a := range addrs {
				if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
					foundAddr = true
					break
				}
			}
			if !foundAddr {
				break
			}
		}
		Expect(foundAddr).To(BeTrue())
		return nil
	})
	Expect(err).NotTo(HaveOccurred())
}

func validateLocalnetGatewayRules(lnData []*localnetData, ipts []*util.FakeIPTables) {
	err := localnetGatewayNAT(lnData, localnetGatewayNextHopPort)
	Expect(err).NotTo(HaveOccurred())
	for i := 0; i < len(ipts); i++ {
		expectedRules := expectedLocannetGatewayIPTableRules(lnData[i].gatewayIP.String())
		Expect(ipts[i].MatchState(expectedRules)).NotTo(HaveOccurred())
	}
}

func validateClearIPTableForNodeport(lnData []*localnetData, ipts []*util.FakeIPTables) {
	clearOvnNodeportRules(lnData)
	expectedRules := map[string]util.FakeTable{
		"filter": {
			"OVN-KUBE-NODEPORT": []string{},
		},
		"nat": {
			"OVN-KUBE-NODEPORT": []string{},
		},
	}
	for i := 0; i < len(ipts); i++ {
		Expect(ipts[i].MatchState(expectedRules)).NotTo(HaveOccurred())
	}
}

func validateNodePortBaseRules(lnData []*localnetData, ipts []*util.FakeIPTables) {
	wf := &factory.WatchFactory{}
	_ = localnetNodePortWatcher(lnData, wf)
	expectedRules := expectedNodePortBaseIPTableRules()
	for i := 0; i < len(ipts); i++ {
		Expect(ipts[i].MatchState(expectedRules)).NotTo(HaveOccurred())
	}
}

func validateNodePortAddService(portWatcherData localnetNodePortWatcherData, service *kapi.Service,
	ipt *util.FakeIPTables, clusterIP string, nodePort int, gatewayIP string) {
	service.Spec.ClusterIP = clusterIP
	err := portWatcherData.addService(service)
	Expect(err).NotTo(HaveOccurred())
	expectedRules := expectedNodePortServiceIPTableRules(gatewayIP, nodePort)
	Expect(ipt.MatchState(expectedRules)).NotTo(HaveOccurred())
}

func validateNodePortDeleteService(portWatcherData localnetNodePortWatcherData, service *kapi.Service,
	ipt *util.FakeIPTables, clusterIP string, nodePort int, gatewayIP string) {

	// In order to test deletes we need to first add the service.
	validateNodePortAddService(portWatcherData, service, ipt, clusterIP, nodePort, gatewayIP)

	//Now test the delete service rules
	service.Spec.ClusterIP = clusterIP
	err := portWatcherData.deleteService(service)
	Expect(err).NotTo(HaveOccurred())
	expectedRules := map[string]util.FakeTable{
		"filter": {
			"OVN-KUBE-NODEPORT": []string{},
		},
		"nat": {
			"OVN-KUBE-NODEPORT": []string{},
		},
	}
	Expect(ipt.MatchState(expectedRules)).NotTo(HaveOccurred())
}

func expectedNodePortServiceIPTableRules(gatewayIP string, nPort int) map[string]util.FakeTable {
	nodePort := fmt.Sprintf("%d", nPort)
	return map[string]util.FakeTable{
		"filter": {
			"OVN-KUBE-NODEPORT": []string{
				"-p TCP " + "--dport " + nodePort + " -j ACCEPT",
			},
		},
		"nat": {
			"OVN-KUBE-NODEPORT": []string{
				"-p TCP " + "--dport " + nodePort +
					" -j DNAT --to-destination " + net.JoinHostPort(gatewayIP, nodePort),
			},
		},
	}
}

func expectedNodePortBaseIPTableRules() map[string]util.FakeTable {
	return map[string]util.FakeTable{
		"filter": {
			"FORWARD": []string{
				"-j OVN-KUBE-NODEPORT",
			},
			"OVN-KUBE-NODEPORT": []string{},
		},
		"nat": {
			"PREROUTING": []string{
				"-j OVN-KUBE-NODEPORT",
			},
			"OUTPUT": []string{
				"-j OVN-KUBE-NODEPORT",
			},
			"OVN-KUBE-NODEPORT": []string{},
		},
	}
}

func expectedLocannetGatewayIPTableRules(gatewayIP string) map[string]util.FakeTable {
	return map[string]util.FakeTable{
		"filter": {
			"INPUT": []string{
				"-i " + localnetGatewayNextHopPort + " -m comment --comment from OVN to localhost -j ACCEPT",
			},
			"FORWARD": []string{
				"-o " + localnetGatewayNextHopPort + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
				"-i " + localnetGatewayNextHopPort + " -j ACCEPT",
			},
		},
		"nat": {
			"POSTROUTING": []string{
				"-s " + gatewayIP + " -j MASQUERADE",
			},
		},
	}
}

func validateLocalnetData(subnets []*net.IPNet, expectedData []*localnetData) {
	lnData, err := constructLocalnetData(subnets)
	Expect(err).NotTo(HaveOccurred())
	Expect(len(lnData)).To(Equal(len(subnets)))
	for i := 0; i < len(subnets); i++ {
		Expect(reflect.DeepEqual(*lnData[i], *expectedData[i])).To(BeTrue())
	}
}

func validateL3GatewayData(lnData []*localnetData) {
	nodeAnnotator := newTestKubeAnnotator()
	hw, err := net.ParseMAC(macAddress)
	Expect(err).NotTo(HaveOccurred())

	err = setupL3Gateway(nodeAnnotator, interfaceId, hw, lnData)
	Expect(err).NotTo(HaveOccurred())

	l3GatewayConfigRaw := nodeAnnotator.get(ovnNodeL3GatewayConfig)
	chassisIdRaw := nodeAnnotator.get(ovnNodeChassisID)
	Expect(l3GatewayConfigRaw).NotTo(BeNil())
	Expect(chassisIdRaw).NotTo(BeNil())

	chassisId, ok := chassisIdRaw.(string)
	Expect(ok).To(BeTrue())
	Expect(chassisId).To(Equal(systemID))

	l3GatewayConfigMap, ok := l3GatewayConfigRaw.(map[string]*util.L3GatewayConfig)
	Expect(ok).To(BeTrue())
	l3GatewayConfig := l3GatewayConfigMap[ovnDefaultNetworkGateway]
	Expect(l3GatewayConfig).NotTo(BeNil())

	ipAddrs := make([]*net.IPNet, 0)
	nextHops := make([]net.IP, 0)
	for _, ln := range lnData {
		ipAddrs = append(ipAddrs, &net.IPNet{IP: ln.gatewayIP, Mask: ln.gatewaySubnetMask})
		nextHops = append(nextHops, ln.gatewayNextHop)
	}
	expectedL3gwConfig := util.L3GatewayConfig{
		Mode:           config.GatewayModeLocal,
		ChassisID:      systemID,
		InterfaceID:    interfaceId,
		MACAddress:     hw,
		IPAddresses:    ipAddrs,
		NextHops:       nextHops,
		NodePortEnable: true,
	}
	Expect(reflect.DeepEqual(expectedL3gwConfig, *l3GatewayConfig)).To(BeTrue())
}

type testKubeAnnotator struct {
	annotations map[string]interface{}
}

func newTestKubeAnnotator() testKubeAnnotator {
	return testKubeAnnotator{annotations: make(map[string]interface{})}
}

func (kn testKubeAnnotator) Set(key string, value interface{}) error {
	kn.annotations[key] = value
	return nil
}

func (kn testKubeAnnotator) SetWithFailureHandler(key string, value interface{}, failFn kube.FailureHandlerFn) error {
	print(failFn)
	return kn.Set(key, value)
}

func (kn testKubeAnnotator) Delete(key string) {
	delete(kn.annotations, key)
}

func (kn testKubeAnnotator) Run() error {
	return nil
}

func (kn testKubeAnnotator) get(key string) interface{} {
	return kn.annotations[key]
}
