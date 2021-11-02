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
		It("Creates managementPortSmartNIC for Ovnkube Node mode smart-nic", func() {
			config.OvnKubeNode.Mode = types.NodeModeSmartNIC
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortSmartNIC{}).String()))
		})
		It("Creates managementPortSmartNICHost for Ovnkube Node mode smart-nic-host", func() {
			config.OvnKubeNode.Mode = types.NodeModeSmartNICHost
			mgmtPort := NewManagementPort("worker-node", nil)
			Expect(reflect.TypeOf(mgmtPort).String()).To(Equal(reflect.TypeOf(&managementPortSmartNICHost{}).String()))
		})
	})
})
