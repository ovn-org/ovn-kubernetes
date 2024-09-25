package node

import (
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/urfave/cli/v2"
	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	utilnet "k8s.io/utils/net"
)

type fakePortManager struct {
	tPortOpen       []int32
	tPortClose      []int32
	tProtocolOpen   []kapi.Protocol
	tProtocolClose  []kapi.Protocol
	tIPOpen         []string
	tIPClose        []string
	tPortOpenCount  int
	tPortCloseCount int
	tPortsMap       map[utilnet.LocalPort]bool
}

type fakePortOpener struct{}

func (f *fakePortOpener) OpenLocalPort(lp *utilnet.LocalPort) (utilnet.Closeable, error) {
	return f, nil
}

func (f *fakePortOpener) Close() error {
	return nil
}

func (p *fakePortManager) open(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	localPort, portError := newLocalPort(desc, ip, port, protocol)
	if portError != nil {
		return portError
	}
	p.tPortsMap[*localPort] = true
	// check that we set the values for assertion
	if len(p.tPortOpen) > 0 {
		Expect(port).To(Equal(p.tPortOpen[p.tPortOpenCount]))
	}
	if len(p.tProtocolOpen) > 0 {
		Expect(protocol).To(Equal(p.tProtocolOpen[p.tPortOpenCount]))
	}
	if len(p.tIPOpen) > 0 {
		Expect(ip).To(Equal(p.tIPOpen[p.tPortOpenCount]))
	}
	p.tPortOpenCount++
	return nil
}

func (p *fakePortManager) close(desc string, ip string, port int32, protocol kapi.Protocol, svc *kapi.Service) error {
	localPort, portError := newLocalPort(desc, ip, port, protocol)
	if portError != nil {
		return portError
	}
	_, exists := p.tPortsMap[*localPort]
	Expect(exists).To(Equal(true))
	delete(p.tPortsMap, *localPort)
	// check that we set the values for assertion
	if len(p.tPortClose) > 0 {
		Expect(port).To(Equal(p.tPortClose[p.tPortCloseCount]))
	}
	if len(p.tProtocolClose) > 0 {
		Expect(protocol).To(Equal(p.tProtocolClose[p.tPortCloseCount]))
	}
	if len(p.tIPClose) > 0 {
		Expect(ip).To(Equal(p.tIPClose[p.tPortCloseCount]))
	}
	p.tPortCloseCount++
	return nil
}

func newLocalPort(desc string, ip string, port int32, protocol kapi.Protocol) (*utilnet.LocalPort, error) {
	var localPort *utilnet.LocalPort
	var portError error
	switch protocol {
	case kapi.ProtocolTCP:
		localPort, portError = utilnet.NewLocalPort(desc, ip, "", int(port), utilnet.TCP)
	case kapi.ProtocolUDP:
		localPort, portError = utilnet.NewLocalPort(desc, ip, "", int(port), utilnet.UDP)
	default:
		portError = fmt.Errorf("wrong protocol %s", protocol)
	}
	return localPort, portError
}

var _ = Describe("Node Operations", func() {

	var (
		app *cli.App
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

	})
	Context("on add service", func() {

		It("should open a port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakePort := &fakePortManager{
					tPortOpen:     []int32{8080, 9999},
					tProtocolOpen: []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP},
					tIPOpen:       []string{"8.8.8.8", "8.8.8.8"},
					tPortsMap:     make(map[utilnet.LocalPort]bool),
				}
				pcw := &portClaimWatcher{port: fakePort}

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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.AddService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open a NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				fakePort := &fakePortManager{
					tPortOpen:     []int32{32222, 31111},
					tProtocolOpen: []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP},
					tIPOpen:       []string{"", ""},
					tPortsMap:     make(map[utilnet.LocalPort]bool),
				}
				pcw := &portClaimWatcher{port: fakePort}

				service := newService("service2", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.AddService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open a NodePort and port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakePort := &fakePortManager{
					tPortOpen:     []int32{32222, 8081, 31111, 8080},
					tProtocolOpen: []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolTCP},
					tIPOpen:       []string{"", "8.8.8.8", "", "8.8.8.8"},
					tPortsMap:     make(map[utilnet.LocalPort]bool),
				}
				pcw := &portClaimWatcher{port: fakePort}

				service := newService("service3", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.AddService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should open per protocol NodePorts and ExternalIPs ports", func() {
			app.Action = func(ctx *cli.Context) error {

				fakePort := &fakePortManager{
					tPortOpen:     []int32{32222, 8081, 32222, 8081},
					tProtocolOpen: []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP, kapi.ProtocolUDP, kapi.ProtocolUDP},
					tIPOpen:       []string{"", "8.8.8.8", "", "8.8.8.8"},
					tPortsMap:     make(map[utilnet.LocalPort]bool),
				}
				pcw := &portClaimWatcher{port: fakePort}

				service := newService("service4", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.AddService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortOpenCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not open a port for ClusterIP", func() {
			app.Action = func(ctx *cli.Context) error {

				fakePort := &fakePortManager{}
				pcw := &portClaimWatcher{port: fakePort}

				service := newService("service5", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.AddService(service)
				Expect(err).NotTo(HaveOccurred())
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

				fakePort := &fakePortManager{
					tPortClose: []int32{8080, 9999},
				}
				pcw := &portClaimWatcher{port: fakePort}

				oldService := newService("service6", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)
				newService := newService("service7", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				err := pcw.UpdateService(oldService, newService)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortOpenCount).To(Equal(0))
				Expect(fakePort.tPortCloseCount).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should only remove ports when ExternalIP -> no ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {

				oldService := newService("service8", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)
				newService := newService("service9", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				portMap := make(map[utilnet.LocalPort]bool)
				lp, _ := utilnet.NewLocalPort(getDescription("", oldService, false), "8.8.8.8", "", 8080, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", oldService, false), "8.8.8.8", "", 9999, utilnet.TCP)
				portMap[*lp] = true

				fakePort := &fakePortManager{
					tPortClose:     []int32{8080, 9999},
					tProtocolClose: []kapi.Protocol{kapi.ProtocolTCP, kapi.ProtocolTCP},
					tIPClose:       []string{"8.8.8.8", "8.8.8.8"},
					tPortsMap:      portMap,
				}
				pcw := &portClaimWatcher{port: fakePort}
				err := pcw.UpdateService(oldService, newService)
				Expect(err).NotTo(HaveOccurred())
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
				service := newService("service10", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							Port:     8088,
							Protocol: kapi.ProtocolTCP,
						},
						{
							Port:     9998,
							Protocol: kapi.ProtocolUDP,
						},
					},
					kapi.ServiceTypeClusterIP,
					[]string{"8.8.8.8", "10.10.10.10"},
					v1.ServiceStatus{},
					false, false,
				)
				portMap := make(map[utilnet.LocalPort]bool)
				lp, _ := utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 8088, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "10.10.10.10", "", 8088, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 9998, utilnet.UDP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "10.10.10.10", "", 9998, utilnet.UDP)
				portMap[*lp] = true

				fakePort := &fakePortManager{
					tPortClose: []int32{8088, 8088, 9998, 9998},
					tIPClose:   []string{"8.8.8.8", "10.10.10.10", "8.8.8.8", "10.10.10.10"},
					tPortsMap:  portMap,
				}
				pcw := &portClaimWatcher{port: fakePort}
				err := pcw.DeleteService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports) * len(service.Spec.ExternalIPs)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close a NodePort and port for ExternalIP", func() {
			app.Action = func(ctx *cli.Context) error {
				service := newService("service11", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32222,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 31111,
							Port:     8080,
							Protocol: kapi.ProtocolUDP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"8.8.8.8"},
					v1.ServiceStatus{},
					false, false,
				)

				portMap := make(map[utilnet.LocalPort]bool)
				lp, _ := utilnet.NewLocalPort(getDescription("", service, true), "", "", 32222, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 8081, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, true), "", "", 31111, utilnet.UDP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 8080, utilnet.UDP)
				portMap[*lp] = true

				fakePort := &fakePortManager{
					tPortClose: []int32{32222, 8081, 31111, 8080},
					tIPClose:   []string{"", "8.8.8.8", "", "8.8.8.8"},
					tPortsMap:  portMap,
				}
				pcw := &portClaimWatcher{port: fakePort}
				err := pcw.DeleteService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close per protocol for NodePort and ExternalIP ports", func() {
			app.Action = func(ctx *cli.Context) error {
				service := newService("service12", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				portMap := make(map[utilnet.LocalPort]bool)
				lp, _ := utilnet.NewLocalPort(getDescription("", service, true), "", "", 32222, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 8081, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, true), "", "", 32222, utilnet.UDP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, false), "8.8.8.8", "", 8081, utilnet.UDP)
				portMap[*lp] = true

				fakePort := &fakePortManager{
					tPortClose: []int32{32222, 8081, 32222, 8081},
					tPortsMap:  portMap,
				}
				pcw := &portClaimWatcher{port: fakePort}
				err := pcw.DeleteService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports) * 2))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should close ports for NodePort", func() {
			app.Action = func(ctx *cli.Context) error {

				service := newService("service13", "namespace1", "10.129.0.2",
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
					v1.ServiceStatus{},
					false, false,
				)

				portMap := make(map[utilnet.LocalPort]bool)
				lp, _ := utilnet.NewLocalPort(getDescription("", service, true), "", "", 31100, utilnet.TCP)
				portMap[*lp] = true
				lp, _ = utilnet.NewLocalPort(getDescription("", service, true), "", "", 32200, utilnet.TCP)
				portMap[*lp] = true

				fakePort := &fakePortManager{
					tPortClose: []int32{31100, 32200},
					tPortsMap:  portMap,
				}
				pcw := &portClaimWatcher{port: fakePort}
				err := pcw.DeleteService(service)
				Expect(err).NotTo(HaveOccurred())
				Expect(fakePort.tPortCloseCount).To(Equal(len(service.Spec.Ports)))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
	Context("open/close check operations", func() {
		It("should open and close ports", func() {
			app.Action = func(ctx *cli.Context) error {
				localAddrSet, err := getLocalAddrs()
				Expect(err).ShouldNot(HaveOccurred())
				lpm := localPortManager{
					recorder:          record.NewFakeRecorder(10),
					activeSocketsLock: sync.Mutex{},
					localAddrSet:      localAddrSet,
					portsMap:          make(map[utilnet.LocalPort]utilnet.Closeable),
					portOpener:        &fakePortOpener{},
				}
				Expect(err).NotTo(HaveOccurred())
				service := newService("service13", "namespace1", "10.129.0.2",
					[]kapi.ServicePort{
						{
							NodePort: 32221,
							Port:     8081,
							Protocol: kapi.ProtocolTCP,
						},
						{
							NodePort: 32222,
							Port:     8082,
							Protocol: kapi.ProtocolUDP,
						},
					},
					kapi.ServiceTypeNodePort,
					[]string{"127.0.0.1", "8.8.8.8"},
					v1.ServiceStatus{},
					false, false,
				)

				errors := handleService(service, lpm.open)
				Expect(len(errors)).To(Equal(0))
				Expect(len(lpm.portsMap)).To(Equal(4))
				lps := make([]*utilnet.LocalPort, 0)
				lp, err := utilnet.NewLocalPort(getDescription("", service, true), "", "", 32221, utilnet.TCP)
				Expect(err).ShouldNot(HaveOccurred())
				lps = append(lps, lp)
				lp, err = utilnet.NewLocalPort(getDescription("", service, false), "127.0.0.1", "", 8081, utilnet.TCP)
				Expect(err).ShouldNot(HaveOccurred())
				lps = append(lps, lp)
				lp, err = utilnet.NewLocalPort(getDescription("", service, true), "", "", 32222, utilnet.UDP)
				Expect(err).ShouldNot(HaveOccurred())
				lps = append(lps, lp)
				lp, err = utilnet.NewLocalPort(getDescription("", service, false), "127.0.0.1", "", 8082, utilnet.UDP)
				Expect(err).ShouldNot(HaveOccurred())
				lps = append(lps, lp)

				for _, lp = range lps {
					_, exists := lpm.portsMap[*lp]
					Expect(exists).To(Equal(true))
				}
				errors = handleService(service, lpm.close)
				Expect(len(errors)).To(Equal(0))
				Expect(len(lpm.portsMap)).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
