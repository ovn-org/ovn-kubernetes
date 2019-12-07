package ovn

import (
	"fmt"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

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

func (e endpoints) addCmds(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	fexec.AddFakeCmd(&ovntest.ExpectedCmd{
		Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find load_balancer external_ids:k8s-cluster-lb-tcp=yes",
		Output: k8sTCPLoadBalancerIP,
	})
	fexec.AddFakeCmdsNoOutputNoError([]string{
		fmt.Sprintf("ovn-nbctl --timeout=15 set load_balancer %s vips:\"%s:%v\"=\"%s:%v\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, service.Spec.Ports[0].Port, endpoint.Subsets[0].Addresses[0].IP, endpoint.Subsets[0].Ports[0].Port),
	})
}

func (e endpoints) delCmds(fexec *ovntest.FakeExec, service v1.Service, endpoint v1.Endpoints) {
	for _, sPort := range service.Spec.Ports {
		if sPort.Protocol == v1.ProtocolTCP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 remove load_balancer %s vips \"%s:%v\"", k8sTCPLoadBalancerIP, service.Spec.ClusterIP, sPort.Port),
			})
		} else if sPort.Protocol == v1.ProtocolUDP {
			fexec.AddFakeCmdsNoOutputNoError([]string{
				fmt.Sprintf("ovn-nbctl --timeout=15 remove load_balancer %s vips \"%s:%v\"", k8sUDPLoadBalancerIP, service.Spec.ClusterIP, sPort.Port),
			})
		}
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

		It("reconciles existing endpoints", func() {
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
				)

				tExec := ovntest.NewFakeExec()
				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx, tExec,
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

				_, err := fakeOvn.fakeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(endpointsT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(tExec.CalledMatchesExpected()).To(BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reconciles deleted endpoints", func() {
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
				)
				tExec := ovntest.NewFakeExec()
				testE.addCmds(tExec, serviceT, endpointsT)

				fakeOvn.start(ctx, tExec,
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

				_, err := fakeOvn.fakeClient.CoreV1().Endpoints(endpointsT.Namespace).Get(endpointsT.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Eventually(tExec.CalledMatchesExpected).Should(BeTrue(), tExec.ErrorDesc)

				// Delete the endpoint
				testE.delCmds(tExec, serviceT, endpointsT)

				err = fakeOvn.fakeClient.CoreV1().Endpoints(endpointsT.Namespace).Delete(endpointsT.Name, metav1.NewDeleteOptions(0))
				Expect(err).NotTo(HaveOccurred())
				Eventually(tExec.CalledMatchesExpected).Should(BeTrue(), tExec.ErrorDesc)

				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
