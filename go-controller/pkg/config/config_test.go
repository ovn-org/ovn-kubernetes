package config

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli"
	kexec "k8s.io/utils/exec"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

func writeConfigFile(cfgFile *os.File, randomOptData bool, args ...string) error {
	// Convert command-line args into sections and options
	sections := make(map[string][]string)
	for _, arg := range args {
		var section string
		switch {
		case strings.HasPrefix(arg, "-k8s-"):
			section = "kubernetes"
			arg = arg[5:]
		case strings.HasPrefix(arg, "-cni-"):
			section = "cni"
			arg = arg[5:]
		case strings.HasPrefix(arg, "-log"):
			section = "logging"
			arg = arg[1:]
		case strings.HasPrefix(arg, "-conntrack-zone"):
			section = "defaults"
			arg = arg[1:]
		case strings.HasPrefix(arg, "-mtu"):
			section = "defaults"
			arg = arg[1:]
		case strings.HasPrefix(arg, "-nb-"):
			section = "ovnnorth"
			arg = arg[4:]
		case strings.HasPrefix(arg, "-sb-"):
			section = "ovnsouth"
			arg = arg[4:]
		default:
			return fmt.Errorf("unexpected argument passed")
		}

		if randomOptData {
			parts := strings.Split(arg, "=")
			Expect(len(parts)).To(Equal(2))
			sections[section] = append(sections[section], parts[0]+"=aklsdjfalsdfkjaslfdkjasfdlksa")
		} else {
			sections[section] = append(sections[section], arg)
		}
	}

	// Write sections and options to the file data
	var data string
	for k, array := range sections {
		data += fmt.Sprintf("[%s]\n", k)
		for _, item := range array {
			data += item + "\n"
		}
	}

	_, err := cfgFile.Write([]byte(data))
	return err
}

// runType 1: command-line args only
// runType 2: config file only
// runType 3: command-line args and random config file option data to test CLI override
func runInit(app *cli.App, runType int, cfgFile *os.File, args ...string) error {
	app.Action = func(ctx *cli.Context) error {
		_, err := InitConfig(ctx, kexec.New(), nil)
		return err
	}

	finalArgs := []string{app.Name, "-loglevel=5"}
	switch runType {
	case 1:
		finalArgs = append(finalArgs, args...)
	case 2:
		finalArgs = append(finalArgs, "-config-file="+cfgFile.Name())
		if err := writeConfigFile(cfgFile, false, args...); err != nil {
			return err
		}
	case 3:
		finalArgs = append(finalArgs, "-config-file="+cfgFile.Name())
		finalArgs = append(finalArgs, args...)
		if err := writeConfigFile(cfgFile, true, args...); err != nil {
			return err
		}
	default:
		panic("shouldn't get here")
	}
	return app.Run(finalArgs)
}

var tmpDir string

var _ = AfterSuite(func() {
	err := os.RemoveAll(tmpDir)
	Expect(err).NotTo(HaveOccurred())
})

func createTempFile(name string) (string, error) {
	fname := filepath.Join(tmpDir, name)
	if err := ioutil.WriteFile(fname, []byte{0x20}, 0644); err != nil {
		return "", err
	}
	return fname, nil
}

func createTempFileContent(name, value string) (string, error) {
	fname := filepath.Join(tmpDir, name)
	if err := ioutil.WriteFile(fname, []byte(value), 0644); err != nil {
		return "", err
	}
	return fname, nil
}

// writeTestConfigFile writes out a config file with well-known options but
// allows specific fields to be overridden by the testcase
func writeTestConfigFile(path string, overrides ...string) error {
	const defaultData string = `[default]
mtu=1500
conntrack-zone=64321
cluster-subnets=10.129.0.0/14/23

[kubernetes]
kubeconfig=/path/to/kubeconfig
apiserver=https://1.2.3.4:6443
token=TG9yZW0gaXBzdW0gZ
cacert=/path/to/kubeca.crt
service-cidr=172.18.0.0/24
no-hostsubnet-nodes=label=another-test-label

[logging]
loglevel=5
logfile=/var/log/ovnkube.log

[cni]
conf-dir=/etc/cni/net.d22
plugin=ovn-k8s-cni-overlay22

[ovnnorth]
address=ssl://1.2.3.4:6641
client-privkey=/path/to/nb-client-private.key
client-cert=/path/to/nb-client.crt
client-cacert=/path/to/nb-client-ca.crt

[ovnsouth]
address=ssl://1.2.3.4:6642
client-privkey=/path/to/sb-client-private.key
client-cert=/path/to/sb-client.crt
client-cacert=/path/to/sb-client-ca.crt

[gateway]
mode=shared
interface=eth1
next-hop=1.3.4.5
vlan-id=10
nodeport=false
`

	var newData string
	for _, line := range strings.Split(defaultData, "\n") {
		equalsPos := strings.Index(line, "=")
		if equalsPos >= 0 {
			for _, override := range overrides {
				if strings.HasPrefix(override, line[:equalsPos+1]) {
					line = override
					break
				}
			}
		}
		newData += line + "\n"
	}
	return ioutil.WriteFile(path, []byte(newData), 0644)
}

func mustParseCIDR(cidr string) *net.IPNet {
	_, net, err := net.ParseCIDR(cidr)
	if err != nil {
		panic("bad CIDR string constant " + cidr)
	}
	return net
}

var _ = Describe("Config Operations", func() {
	var app *cli.App
	var cfgFile *os.File

	var tmpErr error
	tmpDir, tmpErr = ioutil.TempDir("", "configtest_certdir")
	if tmpErr != nil {
		GinkgoT().Errorf("failed to create tempdir: %v", tmpErr)
	}
	tmpDir += "/"

	BeforeEach(func() {
		// Restore global default values before each testcase
		RestoreDefaultConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = Flags

		var err error
		cfgFile, err = ioutil.TempFile("", "conftest-")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.Remove(cfgFile.Name())
	})

	It("uses expected defaults", func() {
		// Don't pick up defaults from the environment
		os.Unsetenv("KUBECONFIG")
		os.Unsetenv("K8S_CACERT")
		os.Unsetenv("K8S_APISERVER")
		os.Unsetenv("K8S_TOKEN")

		app.Action = func(ctx *cli.Context) error {
			cfgPath, err := InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Default.MTU).To(Equal(1400))
			Expect(Default.ConntrackZone).To(Equal(64000))
			Expect(Logging.File).To(Equal(""))
			Expect(Logging.Level).To(Equal(4))
			Expect(CNI.ConfDir).To(Equal("/etc/cni/net.d"))
			Expect(CNI.Plugin).To(Equal("ovn-k8s-cni-overlay"))
			Expect(Kubernetes.Kubeconfig).To(Equal(""))
			Expect(Kubernetes.CACert).To(Equal(""))
			Expect(Kubernetes.Token).To(Equal(""))
			Expect(Kubernetes.APIServer).To(Equal(DefaultAPIServer))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.16.1.0/24"))
			Expect(Kubernetes.RawNoHostSubnetNodes).To(Equal(""))
			Expect(Default.ClusterSubnets).To(Equal([]CIDRNetworkEntry{
				{mustParseCIDR("10.128.0.0/14"), 23},
			}))
			Expect(IPv6Mode).To(Equal(false))

			for _, a := range []OvnAuthConfig{OvnNorth, OvnSouth} {
				Expect(a.Scheme).To(Equal(OvnDBSchemeUnix))
				Expect(a.PrivKey).To(Equal(""))
				Expect(a.Cert).To(Equal(""))
				Expect(a.CACert).To(Equal(""))
				Expect(a.Address).To(Equal(""))
			}
			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads defaults from ovs-vsctl external IDs", func() {
		app.Action = func(ctx *cli.Context) error {
			fexec := ovntest.NewFakeExec()

			// k8s-api-server
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-server",
				Output: "https://somewhere.com:8081",
			})

			// k8s-api-token
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-token",
				Output: "asadfasdfasrw3atr3r3rf33fasdaa3233",
			})
			// k8s-ca-certificate
			fname, err := createTempFile("ca.crt")
			Expect(err).NotTo(HaveOccurred())
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-ca-certificate",
				Output: fname,
			})
			// ovn-nb address
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-nb",
				Output: "tcp:1.1.1.1:6441",
			})

			cfgPath, err := InitConfigSa(ctx, fexec, tmpDir, &Defaults{
				OvnNorthAddress: true,
				K8sAPIServer:    true,
				K8sToken:        true,
				K8sCert:         true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)

			Expect(Kubernetes.APIServer).To(Equal("https://somewhere.com:8081"))
			Expect(Kubernetes.CACert).To(Equal(fname))
			Expect(Kubernetes.Token).To(Equal("asadfasdfasrw3atr3r3rf33fasdaa3233"))

			Expect(OvnNorth.Scheme).To(Equal(OvnDBSchemeTCP))
			Expect(OvnNorth.PrivKey).To(Equal(""))
			Expect(OvnNorth.Cert).To(Equal(""))
			Expect(OvnNorth.CACert).To(Equal(""))
			Expect(OvnNorth.Address).To(Equal("tcp:1.1.1.1:6441"))

			Expect(OvnSouth.Scheme).To(Equal(OvnDBSchemeUnix))
			Expect(OvnSouth.PrivKey).To(Equal(""))
			Expect(OvnSouth.Cert).To(Equal(""))
			Expect(OvnSouth.CACert).To(Equal(""))
			Expect(OvnSouth.Address).To(Equal(""))

			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reads defaults (multiple master) from ovs-vsctl external IDs", func() {
		app.Action = func(ctx *cli.Context) error {
			fexec := ovntest.NewFakeExec()

			// k8s-api-server
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-server",
				Output: "https://somewhere.com:8081",
			})

			// k8s-api-token
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-token",
				Output: "asadfasdfasrw3atr3r3rf33fasdaa3233",
			})
			// k8s-ca-certificate
			fname, err := createTempFile("kube-cacert.pem")
			Expect(err).NotTo(HaveOccurred())
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-ca-certificate",
				Output: fname,
			})
			// ovn-nb address
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:ovn-nb",
				Output: "tcp:1.1.1.1:6441,tcp:1.1.1.2:6641,tcp:1.1.1.3:6641",
			})

			tokenFile, err1 := createTempFileContent("token", "TG9yZW0gaXBzdW0gZ")
			Expect(err1).NotTo(HaveOccurred())
			defer os.Remove(tokenFile)

			cfgPath, err := InitConfigSa(ctx, fexec, tmpDir, &Defaults{
				OvnNorthAddress: true,
				K8sAPIServer:    true,
				K8sToken:        true,
				K8sCert:         true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)

			Expect(Kubernetes.APIServer).To(Equal("https://somewhere.com:8081"))
			Expect(Kubernetes.CACert).To(Equal(fname))
			Expect(Kubernetes.Token).To(Equal("asadfasdfasrw3atr3r3rf33fasdaa3233"))

			Expect(OvnNorth.Scheme).To(Equal(OvnDBSchemeTCP))
			Expect(OvnNorth.PrivKey).To(Equal(""))
			Expect(OvnNorth.Cert).To(Equal(""))
			Expect(OvnNorth.CACert).To(Equal(""))
			Expect(OvnNorth.Address).To(
				Equal("tcp:1.1.1.1:6441,tcp:1.1.1.2:6641,tcp:1.1.1.3:6641"))

			Expect(OvnSouth.Scheme).To(Equal(OvnDBSchemeUnix))
			Expect(OvnSouth.PrivKey).To(Equal(""))
			Expect(OvnSouth.Cert).To(Equal(""))
			Expect(OvnSouth.CACert).To(Equal(""))
			Expect(OvnSouth.Address).To(Equal(""))

			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("uses serviceaccount files", func() {
		kubeCAcertFile, err := createTempFile("ca.crt")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeCAcertFile)

		tokenFile, err1 := createTempFileContent("token", "TG9yZW0gaXBzdW0gZ")
		Expect(err1).NotTo(HaveOccurred())
		defer os.Remove(tokenFile)

		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(Kubernetes.CACert).To(Equal(kubeCAcertFile))
			Expect(Kubernetes.Token).To(Equal("TG9yZW0gaXBzdW0gZ"))

			return nil
		}
		err2 := app.Run([]string{app.Name})
		Expect(err2).NotTo(HaveOccurred())

	})

	It("uses environment variables", func() {
		kubeconfigEnvFile, err := createTempFile("kubeconfig.env")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeconfigEnvFile)
		os.Setenv("KUBECONFIG", kubeconfigEnvFile)
		defer os.Setenv("KUBECONFIG", "")

		os.Setenv("K8S_TOKEN", "this is the  token test")
		defer os.Setenv("K8S_TOKEN", "")

		os.Setenv("K8S_APISERVER", "https://9.2.3.4:6443")
		defer os.Setenv("K8S_APISERVER", "")

		kubeCAFile, err1 := createTempFile("kube-ca.crt")
		Expect(err1).NotTo(HaveOccurred())
		defer os.Remove(kubeCAFile)
		os.Setenv("K8S_CACERT", kubeCAFile)
		defer os.Setenv("K8S_CACERT", "")

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Kubernetes.Kubeconfig).To(Equal(kubeconfigEnvFile))
			Expect(Kubernetes.CACert).To(Equal(kubeCAFile))
			Expect(Kubernetes.Token).To(Equal("this is the  token test"))
			Expect(Kubernetes.APIServer).To(Equal("https://9.2.3.4:6443"))

			return nil
		}
		err = app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())

	})

	It("overrides defaults with config file options", func() {
		kubeconfigFile, err := createTempFile("kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeconfigFile)

		kubeCAFile, err := createTempFile("kube-ca.crt")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name(), "kubeconfig="+kubeconfigFile, "cacert="+kubeCAFile)
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Default.MTU).To(Equal(1500))
			Expect(Default.ConntrackZone).To(Equal(64321))
			Expect(Logging.File).To(Equal("/var/log/ovnkube.log"))
			Expect(Logging.Level).To(Equal(5))
			Expect(CNI.ConfDir).To(Equal("/etc/cni/net.d22"))
			Expect(CNI.Plugin).To(Equal("ovn-k8s-cni-overlay22"))
			Expect(Kubernetes.Kubeconfig).To(Equal(kubeconfigFile))
			Expect(Kubernetes.CACert).To(Equal(kubeCAFile))
			Expect(Kubernetes.Token).To(Equal("TG9yZW0gaXBzdW0gZ"))
			Expect(Kubernetes.APIServer).To(Equal("https://1.2.3.4:6443"))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.18.0.0/24"))
			Expect(Default.ClusterSubnets).To(Equal([]CIDRNetworkEntry{
				{mustParseCIDR("10.129.0.0/14"), 23},
			}))

			Expect(OvnNorth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnNorth.PrivKey).To(Equal("/path/to/nb-client-private.key"))
			Expect(OvnNorth.Cert).To(Equal("/path/to/nb-client.crt"))
			Expect(OvnNorth.CACert).To(Equal("/path/to/nb-client-ca.crt"))
			Expect(OvnNorth.Address).To(Equal("ssl:1.2.3.4:6641"))

			Expect(OvnSouth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnSouth.PrivKey).To(Equal("/path/to/sb-client-private.key"))
			Expect(OvnSouth.Cert).To(Equal("/path/to/sb-client.crt"))
			Expect(OvnSouth.CACert).To(Equal("/path/to/sb-client-ca.crt"))
			Expect(OvnSouth.Address).To(Equal("ssl:1.2.3.4:6642"))

			Expect(Gateway.Mode).To(Equal(GatewayModeShared))
			Expect(Gateway.Interface).To(Equal("eth1"))
			Expect(Gateway.NextHop).To(Equal("1.3.4.5"))
			Expect(Gateway.VLANID).To(Equal(uint(10)))
			Expect(Gateway.NodeportEnable).To(BeFalse())

			return nil
		}
		err = app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI options", func() {
		kubeconfigFile, err := createTempFile("kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeconfigFile)

		kubeCAFile, err := createTempFile("kube-ca.crt")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Default.MTU).To(Equal(1234))
			Expect(Default.ConntrackZone).To(Equal(5555))
			Expect(Logging.File).To(Equal("/some/logfile"))
			Expect(Logging.Level).To(Equal(3))
			Expect(CNI.ConfDir).To(Equal("/some/cni/dir"))
			Expect(CNI.Plugin).To(Equal("a-plugin"))
			Expect(Kubernetes.Kubeconfig).To(Equal(kubeconfigFile))
			Expect(Kubernetes.CACert).To(Equal(kubeCAFile))
			Expect(Kubernetes.Token).To(Equal("asdfasdfasdfasfd"))
			Expect(Kubernetes.APIServer).To(Equal("https://4.4.3.2:8080"))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.15.0.0/24"))
			Expect(Kubernetes.RawNoHostSubnetNodes).To(Equal("test=pass"))
			Expect(Default.ClusterSubnets).To(Equal([]CIDRNetworkEntry{
				{mustParseCIDR("10.130.0.0/15"), 24},
			}))

			Expect(OvnNorth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnNorth.PrivKey).To(Equal("/client/privkey"))
			Expect(OvnNorth.Cert).To(Equal("/client/cert"))
			Expect(OvnNorth.CACert).To(Equal("/client/cacert"))
			Expect(OvnNorth.Address).To(Equal("ssl:6.5.4.3:6651"))

			Expect(OvnSouth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnSouth.PrivKey).To(Equal("/client/privkey2"))
			Expect(OvnSouth.Cert).To(Equal("/client/cert2"))
			Expect(OvnSouth.CACert).To(Equal("/client/cacert2"))
			Expect(OvnSouth.Address).To(Equal("ssl:6.5.4.1:6652"))

			Expect(Gateway.Mode).To(Equal(GatewayModeLocal))
			Expect(Gateway.NodeportEnable).To(BeTrue())
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-mtu=1234",
			"-conntrack-zone=5555",
			"-loglevel=3",
			"-logfile=/some/logfile",
			"-cni-conf-dir=/some/cni/dir",
			"-cni-plugin=a-plugin",
			"-cluster-subnets=10.130.0.0/15/24",
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-k8s-apiserver=https://4.4.3.2:8080",
			"-k8s-cacert=" + kubeCAFile,
			"-k8s-token=asdfasdfasdfasfd",
			"-k8s-service-cidr=172.15.0.0/24",
			"-nb-address=ssl://6.5.4.3:6651",
			"-no-hostsubnet-nodes=test=pass",
			"-nb-client-privkey=/client/privkey",
			"-nb-client-cert=/client/cert",
			"-nb-client-cacert=/client/cacert",
			"-sb-address=ssl://6.5.4.1:6652",
			"-sb-client-privkey=/client/privkey2",
			"-sb-client-cert=/client/cert2",
			"-sb-client-cacert=/client/cacert2",
			"-gateway-mode=local",
			"-nodeport",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy service-cluster-ip-range option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
service-cidr=172.18.0.0/24
`), 0644)
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.15.0.0/24"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-service-cluster-ip-range=172.15.0.0/24",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns an error when the k8s-service-cidr is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			Expect(err).To(MatchError("kubernetes service network CIDR \"adsfasdfaf\" invalid: invalid CIDR address: adsfasdfaf"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-k8s-service-cidr=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy cluster-subnet option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[default]
cluster-subnets=172.18.0.0/23
`), 0644)
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(Default.ClusterSubnets).To(Equal([]CIDRNetworkEntry{
				{mustParseCIDR("172.15.0.0/23"), 24},
			}))
			Expect(IPv6Mode).To(Equal(false))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-cluster-subnet=172.15.0.0/23",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns an error when the cluster-subnets is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			Expect(err).To(MatchError("cluster subnet invalid: CIDR \"adsfasdfaf\" not properly formatted"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy --init-gateways option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[gateway]
mode=local
`), 0644)
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(Gateway.Mode).To(Equal(GatewayModeShared))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-init-gateways",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy --gateway-local option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[gateway]
mode=shared
`), 0644)
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))
			Expect(Gateway.Mode).To(Equal(GatewayModeLocal))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-init-gateways",
			"-gateway-local",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns an error when the gateway mode is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			Expect(err).To(MatchError("invalid gateway mode \"adsfasdfaf\": expect one of shared,local"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-mode=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("overrides config file and defaults with CLI options (multi-master)", func() {
		kubeconfigFile, err := createTempFile("kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeconfigFile)

		kubeCAFile, err := createTempFile("kube-ca.crt")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Default.MTU).To(Equal(1234))
			Expect(Default.ConntrackZone).To(Equal(5555))
			Expect(Logging.File).To(Equal("/some/logfile"))
			Expect(Logging.Level).To(Equal(3))
			Expect(CNI.ConfDir).To(Equal("/some/cni/dir"))
			Expect(CNI.Plugin).To(Equal("a-plugin"))
			Expect(Kubernetes.Kubeconfig).To(Equal(kubeconfigFile))
			Expect(Kubernetes.CACert).To(Equal(kubeCAFile))
			Expect(Kubernetes.Token).To(Equal("asdfasdfasdfasfd"))
			Expect(Kubernetes.APIServer).To(Equal("https://4.4.3.2:8080"))
			Expect(Kubernetes.RawNoHostSubnetNodes).To(Equal("label=another-test-label"))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.15.0.0/24"))

			Expect(OvnNorth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnNorth.PrivKey).To(Equal("/client/privkey"))
			Expect(OvnNorth.Cert).To(Equal("/client/cert"))
			Expect(OvnNorth.CACert).To(Equal("/client/cacert"))
			Expect(OvnNorth.Address).To(
				Equal("ssl:6.5.4.3:6651,ssl:6.5.4.4:6651,ssl:6.5.4.5:6651"))

			Expect(OvnSouth.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(OvnSouth.PrivKey).To(Equal("/client/privkey2"))
			Expect(OvnSouth.Cert).To(Equal("/client/cert2"))
			Expect(OvnSouth.CACert).To(Equal("/client/cacert2"))
			Expect(OvnSouth.Address).To(
				Equal("ssl:6.5.4.1:6652,ssl:6.5.4.2:6652,ssl:6.5.4.3:6652"))

			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-mtu=1234",
			"-conntrack-zone=5555",
			"-loglevel=3",
			"-logfile=/some/logfile",
			"-cni-conf-dir=/some/cni/dir",
			"-cni-plugin=a-plugin",
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-k8s-apiserver=https://4.4.3.2:8080",
			"-k8s-cacert=" + kubeCAFile,
			"-k8s-token=asdfasdfasdfasfd",
			"-k8s-service-cidr=172.15.0.0/24",
			"-nb-address=ssl://6.5.4.3:6651,ssl://6.5.4.4:6651,ssl://6.5.4.5:6651",
			"-nb-client-privkey=/client/privkey",
			"-nb-client-cert=/client/cert",
			"-nb-client-cacert=/client/cacert",
			"-sb-address=ssl://6.5.4.1:6652,ssl://6.5.4.2:6652,ssl://6.5.4.3:6652",
			"-sb-client-privkey=/client/privkey2",
			"-sb-client-cert=/client/cert2",
			"-sb-client-cacert=/client/cacert2",
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not override config file settings with default cli options", func() {
		kubeconfigFile, err := createTempFile("kubeconfig")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeconfigFile)

		kubeCAFile, err := createTempFile("kube-ca.crt")
		Expect(err).NotTo(HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		Expect(err).NotTo(HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfgPath).To(Equal(cfgFile.Name()))

			Expect(Default.MTU).To(Equal(1500))
			Expect(Default.ConntrackZone).To(Equal(64321))
			Expect(Default.RawClusterSubnets).To(Equal("10.129.0.0/14/23"))
			Expect(Default.ClusterSubnets).To(Equal([]CIDRNetworkEntry{
				{mustParseCIDR("10.129.0.0/14"), 23},
			}))
			Expect(Logging.File).To(Equal("/var/log/ovnkube.log"))
			Expect(Logging.Level).To(Equal(5))
			Expect(CNI.ConfDir).To(Equal("/etc/cni/net.d22"))
			Expect(CNI.Plugin).To(Equal("ovn-k8s-cni-overlay22"))
			Expect(Kubernetes.Kubeconfig).To(Equal(kubeconfigFile))
			Expect(Kubernetes.CACert).To(Equal(kubeCAFile))
			Expect(Kubernetes.Token).To(Equal("TG9yZW0gaXBzdW0gZ"))
			Expect(Kubernetes.ServiceCIDR).To(Equal("172.18.0.0/24"))

			return nil
		}

		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-k8s-cacert=" + kubeCAFile,
		}
		err = app.Run(cliArgs)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("OvnDBAuth operations", func() {
		var certFile, keyFile, caFile string

		BeforeEach(func() {
			var err error
			certFile, err = createTempFile("cert.crt")
			Expect(err).NotTo(HaveOccurred())
			keyFile, err = createTempFile("priv.key")
			Expect(err).NotTo(HaveOccurred())
			caFile = filepath.Join(tmpDir, "ca.crt")
		})

		AfterEach(func() {
			err := os.Remove(certFile)
			Expect(err).NotTo(HaveOccurred())
			err = os.Remove(keyFile)
			Expect(err).NotTo(HaveOccurred())
			os.Remove(caFile)
		})

		const (
			nbURL string = "ssl://1.2.3.4:6641"
			sbURL string = "ssl://1.2.3.4:6642"
		)

		It("configures client northbound SSL correctly", func() {
			const nbURLOVN string = "ssl:1.2.3.4:6641"

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --db=" + nbURLOVN + " --timeout=5 --private-key=" + keyFile + " --certificate=" + certFile + " --bootstrap-ca-cert=" + caFile + " list nb_global",
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-nb=\"" + nbURLOVN + "\"",
			})

			cliConfig := &OvnAuthConfig{
				Address: nbURL,
				PrivKey: keyFile,
				Cert:    certFile,
				CACert:  caFile,
			}
			a, err := buildOvnAuth(fexec, true, cliConfig, &OvnAuthConfig{}, true)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(a.PrivKey).To(Equal(keyFile))
			Expect(a.Cert).To(Equal(certFile))
			Expect(a.CACert).To(Equal(caFile))
			Expect(a.Address).To(Equal("ssl:1.2.3.4:6641"))
			Expect(a.northbound).To(BeTrue())
			Expect(a.externalID).To(Equal("ovn-nb"))

			Expect(a.GetURL()).To(Equal(nbURLOVN))
			err = a.SetDBAuth()
			Expect(err).NotTo(HaveOccurred())
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		})

		It("configures client southbound SSL correctly", func() {
			const sbURLOVN string = "ssl:1.2.3.4:6642"

			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --db=" + sbURLOVN + " --timeout=5 --private-key=" + keyFile + " --certificate=" + certFile + " --bootstrap-ca-cert=" + caFile + " list nb_global",
				"ovs-vsctl --timeout=15 del-ssl",
				"ovs-vsctl --timeout=15 set-ssl " + keyFile + " " + certFile + " " + caFile,
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-remote=\"" + sbURLOVN + "\"",
			})

			cliConfig := &OvnAuthConfig{
				Address: sbURL,
				PrivKey: keyFile,
				Cert:    certFile,
				CACert:  caFile,
			}
			a, err := buildOvnAuth(fexec, false, cliConfig, &OvnAuthConfig{}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(a.PrivKey).To(Equal(keyFile))
			Expect(a.Cert).To(Equal(certFile))
			Expect(a.CACert).To(Equal(caFile))
			Expect(a.Address).To(Equal("ssl:1.2.3.4:6642"))
			Expect(a.northbound).To(BeFalse())
			Expect(a.externalID).To(Equal("ovn-remote"))

			Expect(a.GetURL()).To(Equal(sbURLOVN))
			err = a.SetDBAuth()
			Expect(err).NotTo(HaveOccurred())
			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		})

		It("configures client northbound and southbound Scheme correctly when watch-endpoint specified", func() {

			fexec := ovntest.NewFakeExec()

			cliConfig := &OvnAuthConfig{
				Address: "watch-endpoint",
				PrivKey: keyFile,
				Cert:    certFile,
				CACert:  caFile,
			}
			MasterHA.ManageDBServers = true

			a, err := buildOvnAuth(fexec, false, cliConfig, &OvnAuthConfig{}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Scheme).To(Equal(OvnDBSchemeSSL))
			Expect(a.PrivKey).To(Equal(keyFile))
			Expect(a.Cert).To(Equal(certFile))
			Expect(a.CACert).To(Equal(caFile))
			Expect(a.Address).To(Equal("watch-endpoint"))
			Expect(a.northbound).To(BeFalse())
			Expect(a.externalID).To(Equal("ovn-remote"))

			Expect(fexec.CalledMatchesExpected()).To(BeTrue(), fexec.ErrorDesc)
		})
	})

	// This testcase factory function exists only to ensure that 'runType'
	// and 'dir' are evaluated when this factory function is called (and
	// the It() is created), but that the CLI arguments are evaluated only
	// when the test function is actually executed.
	createOneTest := func(runType int, dir, match string, getArgs func() []string) func() {
		return func() {
			args := getArgs()
			finalArgs := make([]string, len(args))
			if dir == "" {
				finalArgs = args
			} else {
				// Update args for OVN NB/SB database options
				for i, a := range args {
					finalArgs[i] = fmt.Sprintf("-%s-%s", dir, a)
				}
			}
			err := runInit(app, runType, cfgFile, finalArgs...)
			if match != "" {
				Expect(err).To(MatchError(match))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	}

	// Generates multiple runType and direction It() tests for a given description, match, and args
	generateTests := func(desc, match string, getArgs func() []string) {
		for _, dir := range []string{"nb", "sb"} {
			for runType := 1; runType <= 3; runType++ {
				realDesc := fmt.Sprintf("(%d/%s) %s", runType, dir, desc)
				It(realDesc, createOneTest(runType, dir, match, getArgs))
			}
		}
	}

	// Generates multiple runType It() tests for a given description, match, and args
	generateTestsSimple := func(desc, match string, args ...string) {
		for runType := 1; runType <= 3; runType++ {
			realDesc := fmt.Sprintf("(%d) %s", runType, desc)
			It(realDesc, createOneTest(runType, "", match, func() []string {
				return args
			}))
		}
	}

	// Run once without config file, once with
	Describe("Kubernetes config options", func() {
		Context("returns an error when the", func() {
			generateTestsSimple("CA cert does not exist",
				"kubernetes CA certificate file \"/foo/bar/baz.cert\" not found",
				"-k8s-apiserver=https://localhost:8443", "-k8s-cacert=/foo/bar/baz.cert")

			generateTestsSimple("apiserver URL scheme is invalid",
				"kubernetes API server URL scheme \"gggggg\" invalid",
				"-k8s-apiserver=gggggg://localhost:8443")

			generateTestsSimple("apiserver URL is invalid",
				"kubernetes API server address \"http://a b.com/\" invalid: parse http://a b.com/: invalid character \" \" in host name",
				"-k8s-apiserver=http://a b.com/")

			generateTestsSimple("kubeconfig file does not exist",
				"kubernetes kubeconfig file \"/foo/bar/baz\" not found",
				"-k8s-kubeconfig=/foo/bar/baz")
		})
	})

	Describe("OVN API config options", func() {
		var certFile, keyFile, caFile string

		BeforeEach(func() {
			var err error
			certFile, err = createTempFile("cert.crt")
			Expect(err).NotTo(HaveOccurred())
			keyFile, err = createTempFile("priv.key")
			Expect(err).NotTo(HaveOccurred())
			caFile, err = createTempFile("ca.crt")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.Remove(certFile)
			os.Remove(keyFile)
			os.Remove(caFile)
		})

		Context("returns an error when", func() {
			generateTests("the scheme is not empty/tcp/ssl",
				"unknown OVN DB scheme \"blah\"",
				func() []string {
					return []string{"address=blah://1.2.3.4:5555"}
				})

			generateTests("the address is unix socket and certs are given",
				"certificate or key given; perhaps you mean to use the 'ssl' scheme?",
				func() []string {
					return []string{
						"client-privkey=/bar/baz/foo",
						"client-cert=/bar/baz/foo",
						"client-cacert=/var/baz/foo",
					}
				})

			generateTests("the OVN URL has no port",
				"failed to parse OVN DB host/port \"4.3.2.1\": address 4.3.2.1: missing port in address",
				func() []string {
					return []string{
						"address=tcp://4.3.2.1",
					}
				})

			generateTests("certs are provided for the TCP scheme",
				"certificate or key given; perhaps you mean to use the 'ssl' scheme?",
				func() []string {
					return []string{
						"address=tcp://1.2.3.4:444",
						"client-privkey=/bar/baz/foo",
					}
				})
		})

		Context("does not return an error when", func() {
			generateTests("the SSL scheme is missing a client CA cert", "",
				func() []string {
					return []string{
						"address=ssl://1.2.3.4:444",
						"client-privkey=" + keyFile,
						"client-cert=" + certFile,
						"client-cacert=/foo/bar/baz",
					}
				})

			generateTests("the SSL scheme is missing a private key file", "",
				func() []string {
					return []string{
						"address=ssl://1.2.3.4:444",
						"client-privkey=/foo/bar/baz",
						"client-cert=" + certFile,
						"client-cacert=" + caFile,
					}
				})

			generateTests("the SSL scheme is missing a client cert file", "",
				func() []string {
					return []string{
						"address=ssl://1.2.3.4:444",
						"client-privkey=" + keyFile,
						"client-cert=/foo/bar/baz",
						"client-cacert=" + caFile,
					}
				})

		})
	})
})
