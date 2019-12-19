package util

import (
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway Init Operations", func() {
	It("correctly sorts gateway routers", func() {
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
			Output: `node5      chassis=842fdade-747a-43b8-b40a-d8e8e26379fa lb_force_snat_ip=100.64.0.5
node2 chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=100.64.0.2
node1 chassis=d17ddb5a-050d-42ab-ab50-7c6ce79a8f2e lb_force_snat_ip=100.64.0.1
node4 chassis=912d592c-904c-40cd-9ef1-c2e5b49a33dd lb_force_snat_ip=100.64.0.4`,
		})

		err := SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		name, ip, err := GetDefaultGatewayRouterIP()
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("node1"))
		Expect(ip.String()).To(Equal("100.64.0.1"))
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})

	It("ignores malformatted gateway router entires", func() {
		fexec := ovntest.NewFakeExec()
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd: "ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=name,options find logical_router options:lb_force_snat_ip!=-",
			Output: `node5      chassis=842fdade-747a-43b8-b40a-d8e8e26379fa lb_force_snat_ip=100.64.0.5
node2 chassis=6a47b33b-89d3-4d65-ac31-b19b549326c7 lb_force_snat_ip=asdfsadf
node1 chassis=d17ddb5a-050d-42ab-ab50-7c6ce79a8f2e lb_force_xxxxxxx=100.64.0.1
node4 chassis=912d592c-904c-40cd-9ef1-c2e5b49a33dd lb_force_snat_ip=100.64.0.4`,
		})

		err := SetExec(fexec)
		Expect(err).NotTo(HaveOccurred())

		name, ip, err := GetDefaultGatewayRouterIP()
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("node4"))
		Expect(ip.String()).To(Equal("100.64.0.4"))
		Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
	})
})
