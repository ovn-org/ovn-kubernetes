package node

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func genListStalePortsCmd() string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --data=bare --no-headings --columns=name find interface ofport=-1")
}

func genDeleteStalePortCmd(ifaces ...string) string {
	staleIfacesCmd := ""
	for _, iface := range ifaces {
		if len(staleIfacesCmd) > 0 {
			staleIfacesCmd += fmt.Sprintf(" -- --if-exists --with-iface del-port %s", iface)
		} else {
			staleIfacesCmd += fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists --with-iface del-port %s", iface)
		}
	}
	return staleIfacesCmd
}

func genDeleteStaleRepPortCmd(iface string) string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --if-exists --with-iface del-port %s", iface)
}

func genFindInterfaceWithSandboxCmd() string {
	return fmt.Sprintf("ovs-vsctl --timeout=15 --columns=name,external_ids --data=bare --no-headings " +
		"--format=csv find Interface external_ids:sandbox!=\"\" external_ids:vf-netdev-name!=\"\"")
}

var _ = Describe("Healthcheck tests", func() {
	var execMock *ovntest.FakeExec
	var factoryMock *factoryMocks.ObjectCacheInterface

	BeforeEach(func() {
		execMock = ovntest.NewFakeExec()
		util.SetExec(execMock)
		factoryMock = &factoryMocks.ObjectCacheInterface{}
	})

	AfterEach(func() {
		util.ResetRunner()
	})

	Describe("checkForStaleOVSInternalPorts", func() {

		Context("bridge has stale ports", func() {
			It("removes stale ports from bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genListStalePortsCmd(),
					Output: "foo\n\nbar\n\n",
					Err:    nil,
				})
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genDeleteStalePortCmd("foo", "bar"),
					Output: "",
					Err:    nil,
				})
				checkForStaleOVSInternalPorts()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

		Context("bridge does not have stale ports", func() {
			It("Does not remove any ports from bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genListStalePortsCmd(),
					Output: "",
					Err:    nil,
				})
				checkForStaleOVSInternalPorts()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})
	})

	Describe("checkForStaleOVSRepresentorInterfaces", func() {
		nodeName := "localNode"
		podList := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "a-pod",
					Namespace:   "a-ns",
					Annotations: map[string]string{},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "b-pod",
					Namespace:   "b-ns",
					Annotations: map[string]string{},
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
				},
			},
		}

		BeforeEach(func() {
			// setup kube output
			factoryMock.On("GetPods", "").Return(podList, nil)
		})

		Context("bridge has stale representor ports", func() {
			It("removes stale VF rep ports from bridge", func() {
				// mock call to find OVS interfaces with non-empty external_ids:sandbox
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod vf-netdev-name=blah\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod vf-netdev-name=blah\n" +
						"stale-pod-ifc,sandbox=123abcfaa iface-id=stale-ns_stale-pod vf-netdev-name=blah\n",
					Err: nil,
				})

				// mock calls to remove only stale-port
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genDeleteStaleRepPortCmd("stale-pod-ifc"),
					Output: "",
					Err:    nil,
				})
				checkForStaleOVSRepresentorInterfaces(nodeName, factoryMock)
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

		Context("bridge does not have stale representor ports", func() {
			It("does not remove any port from bridge", func() {
				// ports in br-int
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod\n",
					Err: nil,
				})
				checkForStaleOVSRepresentorInterfaces(nodeName, factoryMock)
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})
	})
})
