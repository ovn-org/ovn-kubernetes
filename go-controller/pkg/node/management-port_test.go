package node

import (
	"reflect"

	. "github.com/onsi/ginkgo"
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
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPort{}).String()))
		})
		It("Creates managementPortRepresentor for Ovnkube Node mode dpu", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPU
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortRepresentor{}).String()))
		})
		It("Creates managementPortNetdev for Ovnkube Node mode dpu-host", func() {
			config.OvnKubeNode.Mode = types.NodeModeDPUHost
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortNetdev{}).String()))
		})
	})
})
