// +build windows

package node

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
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

	tmpDir, tmpErr = ioutil.TempDir("", "clusternodetest_certdir")
	if tmpErr != nil {
		GinkgoT().Errorf("failed to create tempdir: %v", tmpErr)
	}

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("sets up the management port", func() {
		const (
			clusterIPNet string = "10.1.0.0"
			clusterCIDR  string = clusterIPNet + "/16"
		)
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName      string = "node1"
				nodeSubnet    string = "10.1.1.0/24"
				mgtPortMAC    string = "00:00:00:55:66:77"
				mgtPort       string = "k8s-" + nodeName
				mgtPortIP     string = "10.1.1.2"
				mgtPortPrefix string = "24"
				serviceIPNet  string = "172.16.1.0"
				mtu           string = "1400"
				gwIP          string = "10.1.1.1"
				lrpMAC        string = "00:00:00:00:00:03"
				ifindex       string = "10"
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
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: "ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + mgtPort + " -- lsp-set-addresses " + mgtPort + " " + mgtPortMAC + " " + mgtPortIP,
			})

			// windows-specific setup
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"powershell Enable-NetAdapter " + mgtPort,
				"powershell Get-NetIPAddress -InterfaceAlias " + mgtPort,
				"powershell Remove-NetIPAddress -InterfaceAlias " + mgtPort + " -Confirm:$false",
				"powershell New-NetIPAddress -IPAddress " + mgtPortIP + " -PrefixLength " + mgtPortPrefix + " -InterfaceAlias " + mgtPort,
				"netsh interface ipv4 set subinterface " + mgtPort + " mtu=" + mtu + " store=persistent",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "powershell $(Get-NetAdapter | Where { $_.Name -Match \"" + mgtPort + "\" }).ifIndex",
				Output: ifindex,
			})
			fexec.AddFakeCmdsNoOutputNoError([]string{
				// Don't print route output; test that network is not yet found
				"route print -4 " + clusterIPNet,
				"route -p add " + clusterIPNet + " mask 255.255.0.0 " + gwIP + " METRIC 2 IF " + ifindex,
				// Don't print route output; test that network is not yet found
				"route print -4 " + serviceIPNet,
				"route -p add " + serviceIPNet + " mask 255.255.0.0 " + gwIP + " METRIC 2 IF " + ifindex,
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

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			nodeAnnotator := kube.NewNodeAnnotator(&kube.Kube{fakeClient}, &existingNode)
			wg := &sync.WaitGroup{}
			waitErrors := make(chan error)

			n := OvnNode{name: nodeName, stopChan: waitErrors}
			err = n.createManagementPort(nodeSubnet, nodeAnnotator, wg)
			Expect(err).NotTo(HaveOccurred())

			err = nodeAnnotator.Run()
			Expect(err).NotTo(HaveOccurred())
			wg.Wait()
			close(waitErrors)

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
