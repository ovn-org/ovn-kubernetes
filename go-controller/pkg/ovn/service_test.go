package ovn

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/urfave/cli"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type service struct{}

func newServiceMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(namespace),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newService(name, namespace, ip string, ports []v1.ServicePort, serviceType v1.ServiceType) *v1.Service {
	return &v1.Service{
		ObjectMeta: newServiceMeta(name, namespace),
		Spec: v1.ServiceSpec{
			ClusterIP: ip,
			Ports:     ports,
			Type:      serviceType,
		},
	}
}

func (s service) baseCmds(fexec *ovntest.FakeExec, service v1.Service) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: k8sTCPLoadBalancerIP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer %s vips", k8sTCPLoadBalancerIP),
		Output: "{\"172.30.0.10:53\"=\"10.128.0.18:5353,10.129.0.3:5353\"}",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists remove load_balancer %s vips \"172.30.0.10:53\"", k8sTCPLoadBalancerIP),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-udp=yes",
		Output: k8sUDPLoadBalancerIP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer %s vips", k8sUDPLoadBalancerIP),
		Output: "{\"172.30.0.10:53\"=\"10.128.0.18:5353,10.129.0.3:5353\"}",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists remove load_balancer %s vips \"172.30.0.10:53\"", k8sUDPLoadBalancerIP),
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: "gateway1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=gateway1",
		Output: "tcp_load_balancer_id_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer tcp_load_balancer_id_1 vips",
		Output: "{\"172.30.0.10:53\"=\"10.128.0.18:5353,10.129.0.3:5353\"}",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove load_balancer tcp_load_balancer_id_1 vips \"172.30.0.10:53\"",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:UDP_lb_gateway_router=gateway1",
		Output: "udp_load_balancer_id_1",
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading get load_balancer udp_load_balancer_id_1 vips",
		Output: "{\"172.30.0.10:53\"=\"10.128.0.18:5353,10.129.0.3:5353\"}",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		"ovn-nbctl --timeout=15 --if-exists remove load_balancer udp_load_balancer_id_1 vips \"172.30.0.10:53\"",
	})
}

func (s service) addCmds(fexec *ovntest.FakeExec, service v1.Service) {
	s.baseCmds(fexec, service)
}

func (s service) delCmds(fexec *ovntest.FakeExec, service v1.Service) {
	for _, port := range service.Spec.Ports {
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 remove load_balancer %s vips \"%s:%v\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, port.Port),
		})
	}
}

var _ = Describe("OVN Namespace Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = &FakeOVN{}
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("on startup", func() {

		It("reconciles an existing service", func() {
			app.Action = func(ctx *cli.Context) error {

				test := service{}

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port: 8032,
						},
					},
					v1.ServiceTypeClusterIP,
				)

				fExec := ovntest.NewFakeExec()
				test.baseCmds(fExec, service)

				fakeOvn.start(ctx, fExec,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)
				fakeOvn.controller.WatchServices()

				_, err := fakeOvn.fakeClient.CoreV1().Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles a deleted service", func() {
			app.Action = func(ctx *cli.Context) error {

				test := service{}

				service := *newService("service1", "namespace1", "10.129.0.2",
					[]v1.ServicePort{
						{
							Port: 8032,
						},
					},
					v1.ServiceTypeClusterIP,
				)

				fExec := ovntest.NewFakeExec()
				test.baseCmds(fExec, service)

				fakeOvn.start(ctx, fExec,
					&v1.ServiceList{
						Items: []v1.Service{
							service,
						},
					},
				)
				fakeOvn.controller.WatchServices()

				_, err := fakeOvn.fakeClient.CoreV1().Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(fExec.CalledMatchesExpected()).To(BeTrue(), fExec.ErrorDesc)

				test.delCmds(fExec, service)
				err = fakeOvn.fakeClient.CoreV1().Services(service.Namespace).Delete(service.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(fExec.CalledMatchesExpected).Should(BeTrue(), fExec.ErrorDesc)

				s, err := fakeOvn.fakeClient.CoreV1().Services(service.Namespace).Get(service.Name, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(s).To(BeNil())

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
