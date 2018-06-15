package ovn

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"github.com/urfave/cli"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

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

	tmpDir, tmpErr = ioutil.TempDir("", "clusternodetest_certdir")
	if tmpErr != nil {
		GinkgoT().Errorf("failed to create tempdir: %v", tmpErr)
	}

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("sets up the management port", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName          string = "node1"
				lrpMAC            string = "00:00:00:05:46:C3"
				clusterRouterUUID string = "5cedba03-679f-41f3-b00e-b8ed7437bc6c"
				tcpLBUUID         string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
				udpLBUUID         string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
				nodeSubnet        string = "10.1.1.0/24"
				mgtPortMAC        string = "00:00:00:55:66:77"
				mgtPort           string = "k8s-" + nodeName
				mgtPortIP         string = "10.1.1.2"
				mgtPortPrefix     string = "24"
				mgtPortCIDR       string = mgtPortIP + "/" + mgtPortPrefix
				clusterIPNet      string = "10.1.0.0"
				clusterCIDR       string = clusterIPNet + "/16"
				serviceIPNet      string = "172.16.0.0"
				serviceCIDR       string = serviceIPNet + "/16"
				mtu               string = "1400"
				gwIP              string = "10.1.1.1"
				gwCIDR            string = gwIP + "/24"
			)

			fakeCmds := ovntest.AddFakeCmd(nil, &ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=5 --if-exist get logical_router_port rtos-" + nodeName + " mac",
				// Return a known MAC; otherwise code autogenerates it
				Output: lrpMAC,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=5 --data=bare --no-heading --columns=_uuid find logical_router external_ids:k8s-cluster-router=yes",
				Output: clusterRouterUUID,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=5 --may-exist lrp-add " + clusterRouterUUID + " rtos-" + nodeName + " " + lrpMAC + " " + gwCIDR,
				"ovn-nbctl --timeout=5 -- --may-exist ls-add " + nodeName + " -- set logical_switch " + nodeName + " other-config:subnet=" + nodeSubnet + " external-ids:gateway_ip=" + gwCIDR,
				"ovn-nbctl --timeout=5 -- --may-exist lsp-add " + nodeName + " stor-" + nodeName + " -- set logical_switch_port stor-" + nodeName + " type=router options:router-port=rtos-" + nodeName + " addresses=\"" + lrpMAC + "\"",
				"ovs-vsctl --timeout=5 -- --may-exist add-br br-int",
				"ovs-vsctl --timeout=5 -- --may-exist add-port br-int " + mgtPort + " -- set interface " + mgtPort + " type=internal mtu_request=" + mtu + " external-ids:iface-id=" + mgtPort,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=5 --if-exists get interface " + mgtPort + " mac_in_use",
				Output: mgtPortMAC,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=5 -- --may-exist lsp-add " + nodeName + " " + mgtPort + " -- lsp-set-addresses " + mgtPort + " " + mgtPortMAC + " " + mgtPortIP,
			})
			if runtime.GOOS == windowsOS {
				const ifindex string = "10"
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"powershell Enable-NetAdapter " + mgtPort,
					"powershell Get-NetIPAddress -InterfaceAlias " + mgtPort,
					"powershell Remove-NetIPAddress -InterfaceAlias " + mgtPort + " -Confirm:$false",
					"powershell New-NetIPAddress -IPAddress " + mgtPortIP + " -PrefixLength " + mgtPortPrefix + " -InterfaceAlias " + mgtPort,
					"netsh interface ipv4 set subinterface " + mgtPort + " mtu=" + mtu + " store=persistent",
				})
				fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
					Cmd:    "powershell $(Get-NetAdapter | Where { $_.Name -Match \"" + mgtPort + "\" }).ifIndex",
					Output: ifindex,
				})
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					// Don't print route output; test that network is not yet found
					"route print -4 " + clusterIPNet,
					"route -p add " + clusterIPNet + " mask 255.255.0.0 " + gwIP + " METRIC 2 IF " + ifindex,
					// Don't print route output; test that network is not yet found
					"route print -4 " + serviceIPNet,
					"route -p add " + serviceIPNet + " mask 255.255.0.0 " + gwIP + " METRIC 2 IF " + ifindex,
				})
			} else {
				fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
					"ip link set " + mgtPort + " up",
					"ip addr flush dev " + mgtPort,
					"ip addr add " + mgtPortCIDR + " dev " + mgtPort,
					"ip route flush " + clusterCIDR,
					"ip route add " + clusterCIDR + " via " + gwIP,
					"ip route flush " + serviceCIDR,
					"ip route add " + serviceCIDR + " via " + gwIP,
				})
			}
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=5 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
				Output: tcpLBUUID,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=5 set logical_switch " + nodeName + " load_balancer=" + tcpLBUUID,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=5 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-udp=yes",
				Output: udpLBUUID,
			})
			fakeCmds = ovntest.AddFakeCmdsNoOutputNoError(fakeCmds, []string{
				"ovn-nbctl --timeout=5 add logical_switch " + nodeName + " load_balancer " + udpLBUUID,
			})

			fexec := &fakeexec.FakeExec{
				CommandScript: fakeCmds,
				LookPathFunc: func(file string) (string, error) {
					return fmt.Sprintf("/fake-bin/%s", file), nil
				},
			}

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			err = CreateManagementPort(nodeName, nodeSubnet, clusterCIDR, serviceCIDR)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
