package networkControllerManager

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	factoryMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
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
		"--format=csv find Interface external_ids:sandbox!=\"\" external_ids:vf-netdev-name!=\"\" external_ids:ovn_kube_mode=full")
}

var _ = Describe("Healthcheck tests", func() {
	var execMock *ovntest.FakeExec
	var factoryMock factoryMocks.NodeWatchFactory
	var fakeClient *util.OVNClientset
	var err error

	BeforeEach(func() {
		execMock = ovntest.NewFakeExec()
		Expect(util.SetExec(execMock)).To(Succeed())
		factoryMock = factoryMocks.NodeWatchFactory{}
		v1Objects := []runtime.Object{}
		fakeClient = &util.OVNClientset{
			KubeClient: fake.NewSimpleClientset(v1Objects...),
		}
	})

	AfterEach(func() {
		util.ResetRunner()
	})

	Describe("checkForStaleOVSInternalPorts", func() {

		Context("bridge has stale ports", func() {
			It("removes stale ports from bridge", func() {
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genListStalePortsCmd(),
					Output: "foo\n\nbar\n\n" + types.K8sMgmtIntfName + "\n\n",
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
					Output: types.K8sMgmtIntfName + "\n\n",
					Err:    nil,
				})
				checkForStaleOVSInternalPorts()
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})
	})

	Describe("checkForStaleOVSRepresentorInterfaces", func() {
		var ncm *nodeNetworkControllerManager
		nodeName := "localNode"
		podList := []*v1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "a-pod",
					Namespace:   "a-ns",
					Annotations: map[string]string{},
					UID:         "pod-a-uuid-1",
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
					UID:         "pod-b-uuid-2",
				},
				Spec: v1.PodSpec{
					NodeName: nodeName,
				},
			},
		}

		BeforeEach(func() {
			// setup kube output
			ncm, err = NewNodeNetworkControllerManager(fakeClient, &factoryMock, nodeName, nil)
			Expect(err).NotTo(HaveOccurred())
			factoryMock.On("GetPods", "").Return(podList, nil)
		})

		Context("bridge has stale representor ports", func() {
			It("removes stale VF rep ports from bridge", func() {
				// mock call to find OVS interfaces with non-empty external_ids:sandbox
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod iface-id-ver=pod-a-uuid-1 vf-netdev-name=blah\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod iface-id-ver=pod-b-uuid-2 vf-netdev-name=blah\n" +
						"stale-pod-ifc,sandbox=123abcfaa iface-id=stale-ns_stale-pod iface-id-ver=pod-stale-uuid-3 vf-netdev-name=blah\n",
					Err: nil,
				})

				// mock calls to remove only stale-port
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    genDeleteStaleRepPortCmd("stale-pod-ifc"),
					Output: "",
					Err:    nil,
				})
				ncm.checkForStaleOVSRepresentorInterfaces()
				config.OvnKubeNode.Mode = "full"
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})

		Context("bridge does not have stale representor ports", func() {
			It("does not remove any port from bridge", func() {
				// ports in br-int
				execMock.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd: genFindInterfaceWithSandboxCmd(),
					Output: "pod-a-ifc,sandbox=123abcfaa iface-id=a-ns_a-pod iface-id-ver=pod-a-uuid-1 vf-netdev-name=blah\n" +
						"pod-b-ifc,sandbox=123abcfaa iface-id=b-ns_b-pod iface-id-ver=pod-b-uuid-2 vf-netdev-name=blah\n",
					Err: nil,
				})
				ncm.checkForStaleOVSRepresentorInterfaces()
				config.OvnKubeNode.Mode = "full"
				Expect(execMock.CalledMatchesExpected()).To(BeTrue(), execMock.ErrorDesc)
			})
		})
	})
})
