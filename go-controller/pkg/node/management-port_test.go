package node

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"reflect"
)

var _ = Describe("Mananagement port tests", func() {
	BeforeEach(func() {
		config.PrepareTestConfig()
	})

	Context("NewManagementPort Creates Management port object according to config.OvnKubeNode.Mode", func() {
		It("Creates managementPort by default", func() {
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPort{}).String()))
		})
		It("Creates managementPortDPU for Ovnkube Node mode dpu", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPU
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortDPU{}).String()))
		})
		It("Creates managementPortDPUHost for Ovnkube Node mode dpu-host", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortDPUHost{}).String()))
		})
	})
})
