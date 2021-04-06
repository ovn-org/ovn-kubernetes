package ovn

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

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
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.GatewayLBTCP + "=" + gatewayR,
			Output: "load_balancer_" + strconv.Itoa(idx),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
			Output: "169.254.33.2",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%s vips:\"%s:%v\"=\"%s:%v\"", strconv.Itoa(idx), "169.254.33.2", service.Spec.Ports[0].NodePort, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
		})
		workerIdx := idx + 100
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.WorkerLBTCP + "=" + strings.TrimPrefix(gatewayR, "GR_"),
			Output: "load_balancer_" + strconv.Itoa(workerIdx),
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%s vips:\"%s:%v\"=\"%s:%v\"", strconv.Itoa(workerIdx), "169.254.33.2", service.Spec.Ports[0].NodePort, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
		})
	}
}

func (e endpoints) delNodePortPortCmds(fexec *ovntest.FakeExec, service v1.Service, gatewayR string, idx int) {
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"\"", idx, "169.254.33.2", service.Spec.Ports[0].NodePort),
	})
}

func (e endpoints) removeFromIdlingLBAdd(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: k8sIdlingTCPLoadBalancerIP,
	})
	fexec.AddFakeCmdsNoOutputNoError(
		[]string{"ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"172.124.0.2:8032\""},
	)
}

func (e endpoints) removeFromIdlingLBExternalIPs(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: k8sIdlingTCPLoadBalancerIP,
	})
	fexec.AddFakeCmdsNoOutputNoError(
		[]string{"ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"172.124.0.2:9100\""},
	)
	fexec.AddFakeCmdsNoOutputNoError(
		[]string{"ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"1.1.1.1:9100\""},
	)
	fexec.AddFakeCmdsNoOutputNoError(
		[]string{"ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"192.168.126.11:9100\""},
	)
}

func (e endpoints) removeFromIdlingLBNodePort(fexec *ovntest.FakeExec, service v1.Service, nodePort int, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-idling-lb-tcp=yes",
		Output: k8sIdlingTCPLoadBalancerIP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	for idx, gatewayR := range strings.Fields(FakeGRs) {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
			Output: fmt.Sprintf("169.254.33.%d", idx),
		})
	}
	for idx := range strings.Fields(FakeGRs) {
		fexec.AddFakeCmdsNoOutputNoError(
			[]string{fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"169.254.33.%d:%d\"", idx, nodePort)},
		)
	}
	fexec.AddFakeCmdsNoOutputNoError(
		[]string{"ovn-nbctl --timeout=15 --if-exists remove load_balancer k8s_tcp_idling_load_balancer vips \"172.124.0.2:4242\""},
	)
}

func (e endpoints) addCmds(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: k8sTCPLoadBalancerIP,
	})
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})
	for idx, gatewayR := range strings.Fields(FakeGRs) {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.GatewayLBTCP + "=" + gatewayR,
			Output: fmt.Sprintf("load_balancer_%d", idx),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
			Output: "254.254.254.254",
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"%s:%v\"", idx, service.Spec.ClusterIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
		})
		workerIdx := idx + 100
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.WorkerLBTCP + "=" + strings.TrimPrefix(gatewayR, "GR_"),
			Output: fmt.Sprintf("load_balancer_%d", workerIdx),
		})
		fexec.AddFakeCmdsNoOutputNoError([]string{
			fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"%s:%v\"", workerIdx, service.Spec.ClusterIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
		})
	}
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 --if-exists remove load_balancer %s vips \"%s:%v\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, service.Spec.Ports[0].Port),
	})
}

func (e endpoints) addExternalIPCmds(fexec *ovntest.FakeExec, loadBalancerIPs []string, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: FakeGRs,
	})

	for idx, gatewayR := range strings.Fields(FakeGRs) {
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.GatewayLBTCP + "=" + gatewayR,
			Output: fmt.Sprintf("load_balancer_%d", idx),
		})
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
			Output: "254.254.254.254",
		})
		for _, loadBalancerIP := range loadBalancerIPs {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"%s:%v\"", idx, loadBalancerIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
			})
		}
		workerIdx := idx + 100
		fexec.AddFakeCmd(&ovntest.ExpectedCmd{
			Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.WorkerLBTCP + "=" + strings.TrimPrefix(gatewayR, "GR_"),
			Output: fmt.Sprintf("load_balancer_%d", workerIdx),
		})
		for _, loadBalancerIP := range loadBalancerIPs {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"%s:%v\"", workerIdx, loadBalancerIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
			})
		}
	}
}

func (e endpoints) delCmds(fexec *ovntest.FakeExec, service v1.Service, isNodePort bool) {
	gatewayRouters := "GR_1 GR_2"
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
		Output: gatewayRouters,
	})
	for _, sPort := range service.Spec.Ports {
		if sPort.Protocol == v1.ProtocolTCP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer %s vips:\"%s:%v\"=\"\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, sPort.Port),
			})
			for idx, gatewayR := range strings.Fields(FakeGRs) {
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.GatewayLBTCP + "=" + gatewayR,
					Output: fmt.Sprintf("load_balancer_%d", idx),
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"\"", idx, service.Spec.ClusterIP, sPort.Port),
				})
				workerIdx := idx + 100
				fexec.AddFakeCmd(&ovntest.ExpectedCmd{
					Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:" + ovntypes.WorkerLBTCP + "=" + strings.TrimPrefix(gatewayR, "GR_"),
					Output: fmt.Sprintf("load_balancer_%d", workerIdx),
				})
				fexec.AddFakeCmdsNoOutputNoError([]string{
					fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer load_balancer_%d vips:\"%s:%v\"=\"\"", workerIdx, service.Spec.ClusterIP, sPort.Port),
				})
				if isNodePort {
					fexec.AddFakeCmd(&ovntest.ExpectedCmd{
						Cmd:    "ovn-nbctl --timeout=15 get logical_router " + gatewayR + " external_ids:physical_ips",
						Output: "169.254.33.2",
					})
					e.delNodePortPortCmds(fexec, service, gatewayR, idx)
					e.delNodePortPortCmds(fexec, service, strings.TrimPrefix(gatewayR, "GR_"), workerIdx)
				}

				//fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				//	Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=name find logical_router options:chassis!=null",
				//	Output: FakeGRs,
				//})
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
		config.Gateway.Mode = config.GatewayModeShared
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		config.Kubernetes.OVNEmptyLbEvents = true
		tExec = ovntest.NewFakeExec()
		fakeOvn = NewFakeOVN(tExec)
	})

	ginkgo.AfterEach(func() {
		config.Kubernetes.OVNEmptyLbEvents = false
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
				testE.removeFromIdlingLBAdd(tExec, serviceT, endpointsT)
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
				gomega.Expect(tExec.CalledMatchesExpected()).To(gomega.BeTrue(), tExec.ErrorDesc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

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

				testE.removeFromIdlingLBExternalIPs(tExec, serviceT, endpointsT)
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
				gomega.Expect(tExec.CalledMatchesExpected()).To(gomega.BeTrue(), tExec.ErrorDesc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

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
							Port:       4242,
							Protocol:   v1.ProtocolTCP,
							TargetPort: intstr.FromInt(8080),
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)
				testE.removeFromIdlingLBNodePort(tExec, serviceT, 31111, endpointsT)
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
				testE.removeFromIdlingLBAdd(tExec, serviceT, endpointsT)
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
				testE.removeFromIdlingLBAdd(tExec, serviceT, endpointsT)
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
							Port:     4242,
						},
					},
					v1.ServiceTypeNodePort,
					nil,
				)

				testE.removeFromIdlingLBNodePort(tExec, serviceT, 31100, endpointsT)
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
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Delete the endpoint
				testE.removeFromIdlingLBNodePort(tExec, serviceT, 31100, endpointsT)
				testE.delCmds(tExec, serviceT, true)

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Endpoints(endpointsT.Namespace).Delete(context.TODO(), endpointsT.Name, *metav1.NewDeleteOptions(0))
				gomega.Eventually(tExec.CalledMatchesExpected).Should(gomega.BeTrue(), tExec.ErrorDesc)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

func TestServiceNeedsIdling(t *testing.T) {
	config.Kubernetes.OVNEmptyLbEvents = true
	defer func() {
		config.Kubernetes.OVNEmptyLbEvents = false
	}()

	tests := []struct {
		name        string
		annotations map[string]string
		needsIdling bool
	}{
		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"foo": "bar",
			},
			needsIdling: false,
		},
		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"idling.alpha.openshift.io/idled-at": "2021-03-25T21:58:54Z",
			},
			needsIdling: true,
		},

		{
			name: "delete endpoint slice with no service",
			annotations: map[string]string{
				"k8s.ovn.org/idled-at": "2021-03-25T21:58:54Z",
			},
			needsIdling: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if svcNeedsIdling(tt.annotations) != tt.needsIdling {
				t.Errorf("needs Idling does not match")
			}
		})
	}

}
