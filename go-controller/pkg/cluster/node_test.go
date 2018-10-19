package cluster

import (
	"fmt"

	"github.com/urfave/cli"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Node Operations", func() {
	var app *cli.App

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	It("sets correct OVN external IDs", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName string = "1.2.5.6"
				interval int    = 100000
			)

			fakeCmds := ovntest.AddFakeCmd(nil, &ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-encap-type=geneve "+
					"external_ids:ovn-encap-ip=%s "+
					"external_ids:ovn-remote-probe-interval=%d",
					nodeName, interval),
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

			err = setupOVNNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CommandCalls).To(Equal(len(fakeCmds)))
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
})
