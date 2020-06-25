package node

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
)

type testPortClaimWatcher struct {
	tPortOpen       []int32
	tPortClose      []int32
	tProtocolOpen   []kapi.Protocol
	tPortOpenCount  int
	tPortCloseCount int
	tActiveSockets  map[kapi.Protocol]map[int32]bool
}

func (p *testPortClaimWatcher) open(port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	if _, exists := p.tActiveSockets[protocol]; exists {
		p.tActiveSockets[protocol][port] = true
	} else {
		p.tActiveSockets[protocol] = map[int32]bool{
			port: true,
		}
	}
	Expect(p.tActiveSockets).To(HaveKey(protocol))
	Expect(p.tActiveSockets[protocol]).To(HaveKey(port))
	Expect(port).To(Equal(p.tPortOpen[p.tPortOpenCount]))
	Expect(protocol).To(Equal(p.tProtocolOpen[p.tPortOpenCount]))
	p.tPortOpenCount++
	return nil
}

func (p *testPortClaimWatcher) close(port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	if _, exists := p.tActiveSockets[protocol][port]; exists {
		delete(p.tActiveSockets[protocol], port)
	}
	Expect(p.tActiveSockets).To(HaveKey(protocol))
	Expect(p.tActiveSockets[protocol]).ToNot(HaveKey(port))
	Expect(port).To(Equal(p.tPortClose[p.tPortCloseCount]))
	p.tPortCloseCount++
	return nil
}

var _ = Describe("Node Operations", func() {

	var (
		app *cli.App
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

	})
	Context("on add service", func() {

		It("should open a port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortOpen:      []int32{8080, 9999},
					tProtocolOpen:  []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP},
					tActiveSockets: make(map[kapi.Protocol]map[int32]bool),
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{"8.8.8.8"},
				)

				errors := addServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open a NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortOpen:      []int32{32222, 31111},
					tProtocolOpen:  []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP},
					tActiveSockets: make(map[kapi.Protocol]map[int32]bool),
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 31111,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{},
				)

				errors := addServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open a NodePort and port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortOpen:      []int32{32222, 8081, 31111, 8080},
					tProtocolOpen:  []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolTCP},
					tActiveSockets: make(map[kapi.Protocol]map[int32]bool),
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 31111,
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"8.8.8.8"},
				)

				errors := addServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open per protocol NodePorts and ExternalIPs ports", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortOpen:      []int32{32222, 8081, 32222, 8081},
					tProtocolOpen:  []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolUDP},
					tActiveSockets: make(map[kapi.Protocol]map[int32]bool),
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolUDP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"8.8.8.8"},
				)

				errors := addServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not open a port for ClusterIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{},
				)

				errors := addServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("on delete service", func() {

		It("should not do anything ports for ClusterIP updates", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{8080, 9999},
				}

				oldService := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{},
				)
				newService := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{},
				)

				errors := updateServicePortClaim(oldService, newService)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(0))
				Expect(fakePort.tPortCloseCount).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should only remove ports when ExternalIP -> no ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{8080, 9999},
					tActiveSockets: map[kapi.Protocol]map[int32]bool{
						kapi.ProtocolTCP: {
							8080: true,
							9999: true,
						},
					},
				}

				oldService := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{"8.8.8.8"},
				)
				newService := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{},
				)

				errors := updateServicePortClaim(oldService, newService)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortOpenCount).To(Equal(0))
				Expect(fakePort.tPortCloseCount).To(Equal(len(oldService.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

	})

	Context("on delete service", func() {

		It("should close ports for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{8080, 9999},
					tActiveSockets: map[kapi.Protocol]map[int32]bool{
						kapi.ProtocolTCP: {
							8080: true,
							9999: true,
						},
					},
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9999,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{"8.8.8.8", "10.10.10.10"},
				)

				errors := deleteServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close a NodePort and port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{32222, 8081, 31111, 8080},
					tActiveSockets: map[kapi.Protocol]map[int32]bool{
						kapi.ProtocolTCP: {
							8080:  true,
							8081:  true,
							32222: true,
							31111: true,
						},
					},
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 31111,
							Port:     8080,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"8.8.8.8"},
				)

				errors := deleteServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close per protocol for NodePort and ExternalIP ports", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{32222, 8081, 32222, 8081},
					tActiveSockets: map[kapi.Protocol]map[int32]bool{
						kapi.ProtocolTCP: {
							8081:  true,
							32222: true,
						},
						kapi.ProtocolUDP: {
							8081:  true,
							32222: true,
						},
					},
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolUDP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"8.8.8.8"},
				)

				errors := deleteServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close ports for NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				port = &testPortClaimWatcher{
					tPortClose: []int32{31100, 32200},
					tActiveSockets: map[kapi.Protocol]map[int32]bool{
						kapi.ProtocolTCP: {
							31100: true,
							32200: true,
						},
					},
				}

				service := newService("service1", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 31100,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 32200,
							Protocol: kapi.ProtocolTCP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{},
				)

				errors := deleteServicePortClaim(service)
				Expect(errors).To(HaveLen(0))

				fakePort := port.(*testPortClaimWatcher)
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
