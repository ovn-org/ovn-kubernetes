package node

import (
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

var _ = Describe("Mananagement port tests", func() {
	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	Context("NewManagementPort Creates Management port object according to config.OvnKubeNode.Mode", func() {
		It("Creates managementPort by default", func() {
			mgmtPorts := NewManagementPorts("worker-node", nil, "", "")
			Expect(len(mgmtPorts)).To(Equal(1))
			Expect(reflect.TypeOf(mgmtPorts[0]).String()).To(Equal(reflect.TypeOf(&managementPort{}).String()))
		})
		It("Creates managementPortRepresentor for Ovnkube Node mode dpu", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPU
			netdevName, rep := "", "ens1f0v0"
			mgmtPorts := NewManagementPorts("worker-node", nil, netdevName, rep)
			Expect(len(mgmtPorts)).To(Equal(1))
			Expect(reflect.TypeOf(mgmtPorts[0]).String()).To(Equal(reflect.TypeOf(&managementPortRepresentor{}).String()))
			port, _ := mgmtPorts[0].(*managementPortRepresentor)
			Expect(port.repName).To(Equal(rep))
		})
		It("Creates managementPortNetdev for Ovnkube Node mode dpu-host", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			mgmtPorts := NewManagementPorts("worker-node", nil, "", "")
			Expect(len(mgmtPorts)).To(Equal(1))
			Expect(reflect.TypeOf(mgmtPorts[0]).String()).To(Equal(reflect.TypeOf(&managementPortNetdev{}).String()))
		})
		It("Creates managementPortNetdev and managementPortRepresentor for Ovnkube Node mode full", func() {
			config.OvnKubeNode.MgmtPortNetdev = "ens1f0v0"
			mgmtPorts := NewManagementPorts("worker-node", nil, "", "")
			Expect(len(mgmtPorts)).To(Equal(2))
			Expect(reflect.TypeOf(mgmtPorts[0]).String()).To(Equal(reflect.TypeOf(&managementPortNetdev{}).String()))
			Expect(reflect.TypeOf(mgmtPorts[1]).String()).To(Equal(reflect.TypeOf(&managementPortRepresentor{}).String()))
		})
		It("Creates managementPortNetdev and managementPortRepresentor with proper device names", func() {
			netdevName, rep := "ens1f0v0", "ens1f0_0"
			config.OvnKubeNode.MgmtPortNetdev = netdevName
			mgmtPorts := NewManagementPorts("worker-node", nil, netdevName, rep)
			Expect(len(mgmtPorts)).To(Equal(2))
			Expect(reflect.TypeOf(mgmtPorts[1]).String()).To(Equal(reflect.TypeOf(&managementPortRepresentor{}).String()))
			port, _ := mgmtPorts[1].(*managementPortRepresentor)
			Expect(port.repName).To(Equal("ens1f0_0"))
		})
	})
})
