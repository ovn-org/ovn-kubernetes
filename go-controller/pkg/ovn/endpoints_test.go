package ovn

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

var FakeGRs = "GR_1 GR_2"

type endpoints struct{}

func newEndpointsMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		UID:       types.UID(name),
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			"name": name,
		},
	}
}

func newEndpoints(name, namespace string, addresses []v1.EndpointAddress, ports []v1.EndpointPort) *v1.Endpoints {
	return &v1.Endpoints{
		ObjectMeta: newEndpointsMeta(name, namespace),
		Subsets: []v1.EndpointSubset{
			{
				Addresses: addresses,
				Ports:     ports,
			},
		},
	}
}

func (e endpoints) addNodePortPortCmds(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	gatewayRouters := "GR_1 GR_2"
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: gatewayRouters,
	})
	for idx, gatewayR := range strings.Fields(gatewayRouters) {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + gatewayR,
			Output: "load_balancer_" + strconv.Itoa(idx),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
			Output: "169.254.33.2",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%s vips:\"%s:%v\"=\"%s:%v\"", strconv.Itoa(idx), "169.254.33.2", service.Spec.Ports[0].NodePort, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
		})
	}
}

func (e endpoints) delNodePortPortCmds(fexec *ovntest.FakeExec, service v1.Service, gatewayR string, idx int) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
		Output: "169.254.33.2",
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_switch load_balancer{>=}load_balancer_%d", idx),
		fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router load_balancer{>=}load_balancer_%d", idx),
		fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"\"", idx, "169.254.33.2", service.Spec.Ports[0].NodePort),
	})
}

func (e endpoints) addCmds(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: k8sTCPLoadBalancerIP,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer %s vips:\"%s:%v\"=\"%s:%v\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
	})
}

func (e endpoints) addExternalIPCmds(fexec *ovntest.FakeExec, loadBalancerIPs []string, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	for idx, gatewayR := range strings.Fields(FakeGRs) {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + gatewayR,
			Output: fmt.Sprintf("load_balancer_%d", idx),
		})
		for _, loadBalancerIP := range loadBalancerIPs {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"%s:%v\"", idx, loadBalancerIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
			})
		}
	}
}

func (e endpoints) delCmds(fexec *ovntest.FakeExec, service v1.Service, isNodePort bool) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	for _, sPort := range service.Spec.Ports {
		if sPort.Protocol == v1.ProtocolTCP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find logical_switch load_balancer{>=}%s", k8sTCPLoadBalancerIP),
				fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router load_balancer{>=}%s", k8sTCPLoadBalancerIP),
				fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer %s vips:\"%s:%v\"=\"\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, sPort.Port),
			})
			for idx, gatewayR := range strings.Fields(FakeGRs) {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:TCP_lb_gateway_router=" + gatewayR,
					Output: fmt.Sprintf("load_balancer_%d", idx),
				})
				if isNodePort {
					e.delNodePortPortCmds(fexec, service, gatewayR, idx)
				}
			}
		} else if sPort.Protocol == v1.ProtocolUDP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 remove load_balancer %s vips \"%s:%v\"", k8sUDPLoadBalancerIP, service.Spec.ClusterIP, sPort.Port),
			})
		}
	}
}

var _ = ginkgo.Describe("OVN Namespace Operations", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		tExec   *ovntest.FakeExec
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		tExec = ovntest.NewFakeExec()
		fakeOvn = NewFakeOVN(tExec)
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles existing endpoints", func() {
			app.Action = func(ctx *cli.Context) error {

				testE := endpoints{}

				endpointsT := *newEndpoints("endpoint-service1", "namespace1",
					[]v1.EndpointAddress{
						{
							IP: "10.125.0.2",
						},
					},
					[]v1.EndpointPort{
						{
							Name:     "portTcp1",
							Port:     8080,
							Protocol: v1.ProtocolTCP,
						},
					})

				serviceT := *newService("endpoint-service1", "namespace1", "172.124.0.2",
					[]v1.ServicePort{
						{
							Name:     "portTcp1",
							Port:     8032,
							Protocol: v1.ProtocolTCP,
						},
					},
					v1.ServiceTypeClusterIP,
					nil,
				)

				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx,
					&v1.EndpointsList{
						Items: []v1.Endpoints{
							endpointsT,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							serviceT,
						},
					},
				)
				fakeOvn.controller.WatchEndpoints()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(context.TODO(), endpointsT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tExec.CalledMatchesExpected()).To(gomega.BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles existing endpoints with ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				testE := endpoints{}

				loadBalancerIPs := []string{"1.1.1.1", "192.168.126.11"}

				endpointsT := *newEndpoints("endpoint-service1", "namespace1",
					[]v1.EndpointAddress{
						{
							IP: "10.125.0.2",
						},
					},
					[]v1.EndpointPort{
						{
							Name:     "portTcp1",
							Port:     8080,
							Protocol: v1.ProtocolTCP,
						},
					})

				serviceT := *newService("endpoint-service1", "namespace1", "172.124.0.2",
					[]v1.ServicePort{
						{
							Name:       "portTcp1",
							Port:       9100,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.FromInt(8080),
						},
					},
					v1.ServiceTypeClusterIP,
					loadBalancerIPs,
				)

				testE.addCmds(tExec, serviceT, endpointsT)
				testE.addExternalIPCmds(tExec, loadBalancerIPs, serviceT, endpointsT)

				fakeOvn.start(ctx,
					&v1.EndpointsList{
						Items: []v1.Endpoints{
							endpointsT,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							serviceT,
						},
					},
				)
				fakeOvn.controller.WatchEndpoints()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(context.TODO(), endpointsT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tExec.CalledMatchesExpected()).To(gomega.BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles existing endpoints with NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				testE := endpoints{}

				endpointsT := *newEndpoints("endpoint-service1", "namespace1",
					[]v1.EndpointAddress{
						{
							IP: "10.125.0.2",
						},
					},
					[]v1.EndpointPort{
						{
							Name:     "portTcp1",
							Port:     8080,
							Protocol: v1.ProtocolTCP,
						},
					})

				serviceT := *newService("endpoint-service1", "namespace1", "172.124.0.2",
					[]v1.ServicePort{
						{
							Name:       "portTcp1",
							NodePort:   31111,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.FromInt(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)

				testE.addNodePortPortCmds(tExec, serviceT, endpointsT)
				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx,
					&v1.EndpointsList{
						Items: []v1.Endpoints{
							endpointsT,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							serviceT,
						},
					},
				)
				fakeOvn.controller.WatchEndpoints()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(context.TODO(), endpointsT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(tExec.CalledMatchesExpected()).To(gomega.BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles deleted endpoints", func() {
			app.Action = func(ctx *cli.Context) error {

				testE := endpoints{}

				endpointsT := *newEndpoints("endpoint-service1", "namespace1",
					[]v1.EndpointAddress{
						{
							IP: "10.125.0.2",
						},
					},
					[]v1.EndpointPort{
						{
							Name:     "portTcp1",
							Port:     8080,
							Protocol: v1.ProtocolTCP,
						},
					})

				serviceT := *newService("endpoint-service1", "namespace1", "172.124.0.2",
					[]v1.ServicePort{
						{
							Port:     8032,
							Protocol: v1.ProtocolTCP,
							Name:     "portTcp1",
						},
					},
					v1.ServiceTypeClusterIP,
					nil,
				)
				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx,
					&v1.EndpointsList{
						Items: []v1.Endpoints{
							endpointsT,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							serviceT,
						},
					},
				)
				fakeOvn.controller.WatchEndpoints()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(context.TODO(), endpointsT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)

				// Delete the endpoint
				testE.delCmds(tExec, serviceT, false)

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Delete(context.TODO(), endpointsT.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles deleted NodePort endpoints", func() {
			app.Action = func(ctx *cli.Context) error {

				testE := endpoints{}

				endpointsT := *newEndpoints("endpoint-service1", "namespace1",
					[]v1.EndpointAddress{
						{
							IP: "10.125.0.2",
						},
					},
					[]v1.EndpointPort{
						{
							Name:     "portTcp1",
							Port:     8080,
							Protocol: v1.ProtocolTCP,
						},
					})

				serviceT := *newService("endpoint-service1", "namespace1", "172.124.0.2",
					[]v1.ServicePort{
						{
							NodePort: 31100,
							Protocol: v1.ProtocolTCP,
							Name:     "portTcp1",
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)
				testE.addNodePortPortCmds(tExec, serviceT, endpointsT)
				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx,
					&v1.EndpointsList{
						Items: []v1.Endpoints{
							endpointsT,
						},
					},
					&v1.ServiceList{
						Items: []v1.Service{
							serviceT,
						},
					},
				)
				fakeOvn.controller.WatchEndpoints()

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(context.TODO(), endpointsT.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)

				// Delete the endpoint
				testE.delCmds(tExec, serviceT, true)

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Delete(context.TODO(), endpointsT.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
