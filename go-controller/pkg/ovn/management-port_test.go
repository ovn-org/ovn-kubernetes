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
				nodeName      string = "node1"
				nodeSubnet    string = "10.1.1.0/24"
				mgtPortMAC    string = "00:00:00:55:66:77"
				mgtPort       string = "k8s-" + nodeName
				mgtPortIP     string = "10.1.1.2"
				mgtPortPrefix string = "24"
				mgtPortCIDR   string = mgtPortIP + "/" + mgtPortPrefix
				clusterIPNet  string = "10.1.0.0"
				clusterCIDR   string = clusterIPNet + "/16"
				serviceIPNet  string = "172.16.0.0"
				serviceCIDR   string = serviceIPNet + "/16"
				mtu           string = "1400"
				gwIP          string = "10.1.1.1"
				lrpMAC        string = "00:00:00:00:00:03"
			)

			fakeCmds := ovntest.AddFakeCmdsNoOutputNoError(nil, []string{
				"ovs-vsctl --timeout=15 -- --may-exist add-br br-int",
				"ovs-vsctl --timeout=15 -- --may-exist add-port br-int " + mgtPort + " -- set interface " + mgtPort + " type=internal mtu_request=" + mtu + " external-ids:iface-id=" + mgtPort,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get interface " + mgtPort + " mac_in_use",
				Output: mgtPortMAC,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + mgtPort + " -- lsp-set-addresses " + mgtPort + " " + mgtPortMAC + " " + mgtPortIP,
			})
			fakeCmds = ovntest.AddFakeCmd(fakeCmds, &ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-get-addresses stor-" + nodeName,
				Output: lrpMAC,
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
					"ip neigh add " + gwIP + " dev " + mgtPort + " lladdr " + lrpMAC,
				})
			}

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

			err = CreateManagementPort(nodeName, nodeSubnet, serviceCIDR, []string{clusterCIDR})
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
