// +build linux

package ovn

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/coreos/go-iptables/iptables"
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
				serviceIPNet  string = "172.16.1.0"
				serviceCIDR   string = serviceIPNet + "/24"
				mtu           string = "1400"
				gwIP          string = "10.1.1.1"
				lrpMAC        string = "00:00:00:00:00:03"
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
				Cmd: "ovn-nbctl --timeout=15 -- --may-exist lsp-add " + nodeName + " " + mgtPort + " -- lsp-set-addresses " + mgtPort + " " + mgtPortMAC + " " + mgtPortIP + " -- --if-exists remove logical_switch " + nodeName + " other-config exclude_ips",
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 lsp-get-addresses stor-" + nodeName,
				Output: lrpMAC,
			})

			// linux-specific setup
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ip link set " + mgtPort + " up",
				"ip addr flush dev " + mgtPort,
				"ip addr add " + mgtPortCIDR + " dev " + mgtPort,
				"ip route flush " + clusterCIDR,
				"ip route add " + clusterCIDR + " via " + gwIP,
				"ip route flush " + serviceCIDR,
				"ip route add " + serviceCIDR + " via " + gwIP,
				"ip neigh add " + gwIP + " dev " + mgtPort + " lladdr " + lrpMAC,
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

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			err = CreateManagementPort(nodeName, nodeSubnet, []string{clusterCIDR})
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())

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

			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
