package cluster

import (
	"fmt"

	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-encap-type=geneve "+
					"external_ids:ovn-encap-ip=%s "+
					"external_ids:ovn-remote-probe-interval=%d "+
					"external_ids:hostname=\"%s\"",
					nodeName, interval, nodeName),
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			err = setupOVNNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
	It("sets non-default OVN encap port", func() {
		app.Action = func(ctx *cli.Context) error {
			const (
				nodeName    string = "1.2.5.6"
				encapPort   uint   = 666
				interval    int    = 100000
				chassisUUID string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-encap-type=geneve "+
					"external_ids:ovn-encap-ip=%s "+
					"external_ids:ovn-remote-probe-interval=%d "+
					"external_ids:hostname=\"%s\"",
					nodeName, interval, nodeName),
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 " +
					"--if-exists get Open_vSwitch . external_ids:system-id"),
				Output: chassisUUID,
			})
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovn-sbctl --timeout=15 set encap "+
					"%s options:dst_port=%d", chassisUUID, encapPort),
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())
			config.Default.EncapPort = encapPort

			err = setupOVNNode(nodeName)
			Expect(err).NotTo(HaveOccurred())

			Expect(fexec.CalledMatchesExpected()).To(BeTrue())
			return nil
		}

		err := app.Run([]string{app.Name})
		Expect(err).NotTo(HaveOccurred())
	})
	It("test watchConfigEndpoints single IP", func() {
		app.Action = func(ctx *cli.Context) error {

			const (
				masterAddress string = "10.1.2.3"
				nbPort        int32  = 1234
				sbPort        int32  = 4321
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-nb=\"tcp:%s\"",
					util.JoinHostPortInt32(masterAddress, nbPort)),
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-remote=\"tcp:%s\"",
					util.JoinHostPortInt32(masterAddress, sbPort)),
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			fakeClient := fake.NewSimpleClientset(&kapi.EndpointsList{
				Items: []kapi.Endpoints{{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ovn-kubernetes", Name: "ovnkube-db"},
					Subsets: []kapi.EndpointSubset{
						{
							Addresses: []kapi.EndpointAddress{
								{IP: masterAddress},
							},
							Ports: []kapi.EndpointPort{
								{
									Name: "north",
									Port: nbPort,
								},
								{
									Name: "south",
									Port: sbPort,
								},
							},
						},
					},
				}},
			})
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			cluster := NewClusterController(fakeClient, f)
			Expect(cluster).NotTo(BeNil())

			Expect(config.OvnNorth.Address).To(Equal("tcp:1.1.1.1:6641"), "config.OvnNorth.Address does not equal cli arg")
			Expect(config.OvnSouth.Address).To(Equal("tcp:1.1.1.1:6642"), "config.OvnSouth.Address does not equal cli arg")

			err = cluster.watchConfigEndpoints(make(chan bool, 1))
			Expect(err).NotTo(HaveOccurred())

			// Kubernetes endpoints should eventually propogate to OvnNorth/OvnSouth
			Eventually(func() string {
				return config.OvnNorth.Address
			}).Should(Equal(fmt.Sprintf("tcp:%s", util.JoinHostPortInt32(masterAddress, nbPort))), "Northbound DB Port did not get set by watchConfigEndpoints")
			Eventually(func() string {
				return config.OvnSouth.Address
			}).Should(Equal(fmt.Sprintf("tcp:%s", util.JoinHostPortInt32(masterAddress, sbPort))), "Southbound DBPort did not get set by watchConfigEndpoints")

			return nil
		}
		err := app.Run([]string{app.Name, "-nb-address=tcp://1.1.1.1:6641", "-sb-address=tcp://1.1.1.1:6642"})
		Expect(err).NotTo(HaveOccurred())

	})

	It("test watchConfigEndpoints multiple IPs", func() {
		app.Action = func(ctx *cli.Context) error {

			const (
				masterAddress1 string = "10.1.2.3"
				masterAddress2 string = "11.1.2.3"
				nbPort         int32  = 1234
				sbPort         int32  = 4321
			)

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-nb=\"tcp:%s,tcp:%s\"",
					util.JoinHostPortInt32(masterAddress1, nbPort),
					util.JoinHostPortInt32(masterAddress2, nbPort)),
			})

			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd: fmt.Sprintf("ovs-vsctl --timeout=15 set Open_vSwitch . "+
					"external_ids:ovn-remote=\"tcp:%s,tcp:%s\"",
					util.JoinHostPortInt32(masterAddress1, sbPort),
					util.JoinHostPortInt32(masterAddress2, sbPort)),
			})

			err := util.SetExec(fexec)
			Expect(err).NotTo(HaveOccurred())

			fakeClient := fake.NewSimpleClientset(&kapi.EndpointsList{
				Items: []kapi.Endpoints{{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ovn-kubernetes", Name: "ovnkube-db"},
					Subsets: []kapi.EndpointSubset{
						{
							Addresses: []kapi.EndpointAddress{
								{IP: masterAddress1}, {IP: masterAddress2},
							},
							Ports: []kapi.EndpointPort{
								{
									Name: "north",
									Port: nbPort,
								},
								{
									Name: "south",
									Port: sbPort,
								},
							},
						},
					},
				}},
			})
			_, err = config.InitConfig(ctx, fexec, nil)
			Expect(err).NotTo(HaveOccurred())

			stopChan := make(chan struct{})
			f, err := factory.NewWatchFactory(fakeClient, stopChan)
			Expect(err).NotTo(HaveOccurred())
			defer f.Shutdown()

			cluster := NewClusterController(fakeClient, f)
			Expect(cluster).NotTo(BeNil())

			Expect(config.OvnNorth.Address).To(Equal("tcp:1.1.1.1:6641"), "config.OvnNorth.Address does not equal cli arg")
			Expect(config.OvnSouth.Address).To(Equal("tcp:1.1.1.1:6642"), "config.OvnSouth.Address does not equal cli arg")

			err = cluster.watchConfigEndpoints(make(chan bool, 1))
			Expect(err).NotTo(HaveOccurred())

			// Kubernetes endpoints should eventually propogate to OvnNorth/OvnSouth
			Eventually(func() string {
				return config.OvnNorth.Address
			}).Should(Equal(fmt.Sprintf("tcp:%s,tcp:%s", util.JoinHostPortInt32(masterAddress1, nbPort), util.JoinHostPortInt32(masterAddress2, nbPort))), "Northbound DB Port did not get set by watchConfigEndpoints")
			Eventually(func() string {
				return config.OvnSouth.Address
			}).Should(Equal(fmt.Sprintf("tcp:%s,tcp:%s", util.JoinHostPortInt32(masterAddress1, sbPort), util.JoinHostPortInt32(masterAddress2, sbPort))), "Southbound DBPort did not get set by watchConfigEndpoints")

			return nil
		}
		err := app.Run([]string{app.Name, "-nb-address=tcp://1.1.1.1:6641", "-sb-address=tcp://1.1.1.1:6642"})
		Expect(err).NotTo(HaveOccurred())

	})
})
