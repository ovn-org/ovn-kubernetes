// +build linux

package node

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/urfave/cli"
	"github.com/vishvananda/netlink"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var tmpDir string

var _ = AfterSuite(func() {
	err := os.RemoveAll(tmpDir)
	Expect(err).NotTo(HaveOccurred())
})

func createTempFile(name string) (string, error) {
	fname := filepath.Join(tmpDir, name)
	if err := ioutil.WriteFile(fname, []byte{0x20}, 0644); err != nil {
		return "", err
	}
	return fname, nil
}

var _ = Describe("Management Port Operations", func() {
	var tmpErr error
	var app *cli.App
	var testNS ns.NetNS

	tmpDir, tmpErr = ioutil.TempDir("", "clusternodetest_certdir")
	if tmpErr != nil {
		GinkgoT().Errorf("failed to create tempdir: %v", tmpErr)
	}

	BeforeEach(func() {
		var err error
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		// Set up a fake k8s-node1
		testNS, err = testutils.NewNS()
		Expect(err).NotTo(HaveOccurred())
		err = testNS.Do(func(ns.NetNS) error {
			defer GinkgoRecover()

			err := netlink.LinkAdd(&netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name: "k8s-node1",
				},
			})
			Expect(err).NotTo(HaveOccurred())
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(testNS.Close()).To(Succeed())
	})

	It("sets up the management port", func() {
		const clusterCIDR string = "10.1.0.0/16"

		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName      string = "node1"
				nodeSubnet    string = "10.1.1.0/24"
				mgtPortMAC    string = "00:00:00:55:66:77"
				mgtPort       string = "k8s-" + nodeName
				mgtPortIP     string = "10.1.1.2"
				mgtPortPrefix string = "24"
				mgtPortCIDR   string = mgtPortIP + "/" + mgtPortPrefix
				serviceIPNet  string = "172.16.1.0"
				serviceCIDR   string = serviceIPNet + "/24"
				mtu           string = "1400"
				gwIP          string = "10.1.1.1"
				lrpMAC        string = "0a:58:0a:01:01:01"
			)

			fexec := ovntest.NewFakeExec()

			// generic setup
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 -- --may-exist add-br br-int",
				"ovs-vsctl --timeout=15 -- --may-exist add-port br-int " + mgtPort + " -- set interface " + mgtPort + " type=internal mtu_request=" + mtu + " external-ids:iface-id=" + mgtPort,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + mgtPort + " mac_in_use",
				Output: mgtPortMAC,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set interface k8s-node1 " + fmt.Sprintf("mac=%s", strings.ReplaceAll(mgtPortMAC, ":", "\\:")),
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface k8s-node1 ofport",
				Output: "1",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-ofctl --no-stats --no-names dump-flows br-int table=65,out_port=1",
				Output: " table=65, priority=100,reg15=0x2,metadata=0x2 actions=output:1",
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			fakeipt, err := util.NewFakeWithProtocol(iptables.ProtocolIPv4)
			Expect(err).NotTo(HaveOccurred())
			util.SetIPTablesHelper(iptables.ProtocolIPv4, fakeipt)
			err = fakeipt.NewChain("nat", "POSTROUTING")
			Expect(err).NotTo(HaveOccurred())
			err = fakeipt.NewChain("nat", "OVN-KUBE-SNAT-MGMTPORT")
			Expect(err).NotTo(HaveOccurred())

			existingNode := v1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					ovn.OvnNodeSubnets: nodeSubnet,
				},
			}}
			fakeClient := fake.NewSimpleClientset(&v1.NodeList{
				Items: []v1.Node{existingNode},
			})

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &existingNode)
			waiter := newStartupWaiter(nodeName)

			err = testNS.Do(func(ns.NetNS) error {
				defer GinkgoRecover()

				_, nodeSubnetCIDR, _ := net.ParseCIDR(nodeSubnet)
				err = createManagementPort(nodeName, nodeSubnetCIDR, nodeAnnotator, waiter)
				Expect(err).NotTo(HaveOccurred())
				l, err := netlink.LinkByName(mgtPort)
				Expect(err).NotTo(HaveOccurred())

				// Check whether IP has been added
				addrs, err := netlink.AddrList(l, syscall.AF_INET)
				Expect(err).NotTo(HaveOccurred())
				var foundAddr bool
				expectedAddr, err := netlink.ParseAddr(mgtPortCIDR)
				Expect(err).NotTo(HaveOccurred())
				for _, a := range addrs {
					if a.IP.Equal(expectedAddr.IP) && bytes.Equal(a.Mask, expectedAddr.Mask) {
						foundAddr = true
						break
					}
				}
				Expect(foundAddr).To(BeTrue())

				// Check whether the route has been added
				j := 0
				gatewayIP := net.ParseIP(gwIP)
				subnets := []string{clusterCIDR, serviceCIDR}
				for _, subnet := range subnets {
					foundRoute := false
					dstIPnet, err := netlink.ParseIPNet(subnet)
					Expect(err).NotTo(HaveOccurred())
					route := &netlink.Route{Dst: dstIPnet}
					filterMask := netlink.RT_FILTER_DST
					routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, route, filterMask)
					Expect(err).NotTo(HaveOccurred())
					for _, r := range routes {
						if r.Gw.Equal(gatewayIP) && r.LinkIndex == l.Attrs().Index {
							foundRoute = true
							break
						}
					}
					Expect(foundRoute).To(BeTrue())
					foundRoute = false
					j++
				}
				Expect(j).To(Equal(2))

				// Check whether router IP has been added in the arp entry for mgmt port
				neighbours, err := netlink.NeighList(l.Attrs().Index, netlink.FAMILY_ALL)
				Expect(err).NotTo(HaveOccurred())
				var foundNeighbour bool
				for _, neighbour := range neighbours {
					if neighbour.IP.Equal(gatewayIP) && (neighbour.HardwareAddr.String() == lrpMAC) {
						foundNeighbour = true
						break
					}
				}
				Expect(foundNeighbour).To(BeTrue())

				return nil
			})
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())
			err = waiter.Wait()
			Expect(err).NotTo(HaveOccurred())

			expectedTables := map[string]util.FakeTable{
				"filter": {},
				"nat": {
					"POSTROUTING": []string{
						"-o " + mgtPort + " -j OVN-KUBE-SNAT-MGMTPORT",
					},
					"OVN-KUBE-SNAT-MGMTPORT": []string{
						"-o " + mgtPort + " -j SNAT --to-source " + mgtPortIP + " -m comment --comment OVN SNAT to Management Port",
					},
				},
			}
			err = fakeipt.MatchState(expectedTables)
			Expect(err).NotTo(HaveOccurred())

			updatedNode, err := fakeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(updatedNode.Annotations).To(HaveKeyWithValue(ovn.OvnNodeManagementPortMacAddress, mgtPortMAC))

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"--cluster-subnets=" + clusterCIDR,
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
