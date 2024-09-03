package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/urfave/cli/v2"
	kexec "k8s.io/utils/exec"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
)

func TestConfig(t *testing.T) {
	gomega.RegisterFailHandler(Fail)
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
			gomega.Expect(len(parts)).To(gomega.Equal(2))
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
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
})

func createTempFile(name string) (string, []byte, error) {
	fileData := []byte{0x20}
	fname := filepath.Join(tmpDir, name)
	if err := ioutil.WriteFile(fname, fileData, 0o644); err != nil {
		return "", nil, err
	}
	return fname, fileData, nil
}

func createTempFileContent(name, value string) (string, error) {
	fname := filepath.Join(tmpDir, name)
	if err := ioutil.WriteFile(fname, []byte(value), 0o644); err != nil {
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
cluster-subnets=10.132.0.0/14/23
lflow-cache-limit=1000
lflow-cache-limit-kb=100000
zone=global

[kubernetes]
kubeconfig=/path/to/kubeconfig
bootstrap-kubeconfig=
cert-dir=
apiserver=https://1.2.3.4:6443
token=TG9yZW0gaXBzdW0gZ
tokenFile=/path/to/token
cacert=/path/to/kubeca.crt
service-cidrs=172.18.0.0/24
no-hostsubnet-nodes=label=another-test-label
healthz-bind-address=0.0.0.0:1234
dns-service-namespace=kube-system-f
dns-service-name=kube-dns-f
disable-requestedchassis=false

[metrics]
bind-address=1.1.1.1:8080
ovn-metrics-bind-address=1.1.1.2:8081
export-ovs-metrics=true
enable-pprof=true
node-server-privkey=/path/to/node-metrics-private.key
node-server-cert=/path/to/node-metrics.crt
enable-config-duration=true
enable-scale-metrics=true

[logging]
loglevel=5
logfile=/var/log/ovnkube.log

[monitoring]
netflow-targets=2.2.2.2:2055
sflow-targets=2.2.2.2:2056
ipfix-targets=2.2.2.2:2057

[ipfix]
sampling=123
cache-max-flows=456
cache-active-timeout=789

[cni]
conf-dir=/etc/cni/net.d22
plugin=ovn-k8s-cni-overlay22

[ovnnorth]
address=ssl:1.2.3.4:6641
client-privkey=/path/to/nb-client-private.key
client-cert=/path/to/nb-client.crt
client-cacert=/path/to/nb-client-ca.crt
cert-common-name=cfg-nbcommonname

[ovnsouth]
address=ssl:1.2.3.4:6642
client-privkey=/path/to/sb-client-private.key
client-cert=/path/to/sb-client.crt
client-cacert=/path/to/sb-client-ca.crt
cert-common-name=cfg-sbcommonname

[gateway]
mode=shared
interface=eth1
next-hop=1.3.4.5
vlan-id=10
nodeport=false
v4-join-subnet=100.65.0.0/16
v6-join-subnet=fd90::/64
v4-masquerade-subnet=169.254.169.0/29
v6-masquerade-subnet=fd69::/125
router-subnet=10.50.0.0/16
single-node=false
disable-forwarding=true
allow-no-uplink=false

[hybridoverlay]
enabled=true
cluster-subnets=11.132.0.0/14/23

[ovnkubenode]
mode=full

[ovnkubernetesfeature]
egressip-reachability-total-timeout=3
egressip-node-healthcheck-port=1234
enable-multi-network=false
enable-multi-networkpolicy=false
enable-interconnect=false
enable-multi-external-gateway=false
enable-admin-network-policy=false
enable-persistent-ips=false

[clustermanager]
v4-transit-switch-subnet=100.89.0.0/16
v6-transit-switch-subnet=fd98::/64
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
	return ioutil.WriteFile(path, []byte(newData), 0o644)
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
		err := PrepareTestConfig()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = Flags

		cfgFile, err = ioutil.TempFile("", "conftest-")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	AfterEach(func() {
		os.Remove(cfgFile.Name())
	})

	It("uses expected defaults", func() {
		app.Action = func(ctx *cli.Context) error {
			cfgPath, err := InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))

			gomega.Expect(Default.MTU).To(gomega.Equal(1400))
			gomega.Expect(Default.ConntrackZone).To(gomega.Equal(64000))
			gomega.Expect(Default.LFlowCacheEnable).To(gomega.BeTrue())
			gomega.Expect(Default.LFlowCacheLimit).To(gomega.Equal(uint(0)))
			gomega.Expect(Default.LFlowCacheLimitKb).To(gomega.Equal(uint(0)))
			gomega.Expect(Default.EnableUDPAggregation).To(gomega.BeFalse())
			gomega.Expect(Logging.File).To(gomega.Equal(""))
			gomega.Expect(Logging.Level).To(gomega.Equal(5))
			gomega.Expect(Monitoring.RawNetFlowTargets).To(gomega.Equal(""))
			gomega.Expect(Monitoring.RawSFlowTargets).To(gomega.Equal(""))
			gomega.Expect(Monitoring.RawIPFIXTargets).To(gomega.Equal(""))
			gomega.Expect(IPFIX.Sampling).To(gomega.Equal(uint(400)))
			gomega.Expect(IPFIX.CacheMaxFlows).To(gomega.Equal(uint(0)))
			gomega.Expect(IPFIX.CacheActiveTimeout).To(gomega.Equal(uint(60)))
			gomega.Expect(CNI.ConfDir).To(gomega.Equal("/etc/cni/net.d"))
			gomega.Expect(CNI.Plugin).To(gomega.Equal("ovn-k8s-cni-overlay"))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.CertDuration).To(gomega.Equal(Kubernetes.CertDuration))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal([]byte{}))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal(DefaultAPIServer))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.16.1.0/24"))
			gomega.Expect(Kubernetes.RawNoHostSubnetNodes).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.HealthzBindAddress).To(gomega.Equal(""))
			gomega.Expect(Kubernetes.DNSServiceNamespace).To(gomega.Equal("kube-system"))
			gomega.Expect(Kubernetes.DNSServiceName).To(gomega.Equal("kube-dns"))
			gomega.Expect(Metrics.NodeServerPrivKey).To(gomega.Equal(""))
			gomega.Expect(Metrics.NodeServerCert).To(gomega.Equal(""))
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("10.128.0.0/14"), 23},
			}))
			gomega.Expect(Default.Zone).To(gomega.Equal("global"))
			gomega.Expect(IPv4Mode).To(gomega.Equal(true))
			gomega.Expect(IPv6Mode).To(gomega.Equal(false))
			gomega.Expect(HybridOverlay.Enabled).To(gomega.Equal(false))
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeFull))
			gomega.Expect(OvnKubeNode.MgmtPortNetdev).To(gomega.Equal(""))
			gomega.Expect(OvnKubeNode.MgmtPortDPResourceName).To(gomega.Equal(""))
			gomega.Expect(Gateway.RouterSubnet).To(gomega.Equal(""))
			gomega.Expect(Gateway.SingleNode).To(gomega.BeFalse())
			gomega.Expect(Gateway.DisableForwarding).To(gomega.BeFalse())
			gomega.Expect(Gateway.AllowNoUplink).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(1))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(0))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetwork).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetworkPolicy).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableInterconnect).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeFalse())

			for _, a := range []OvnAuthConfig{OvnNorth, OvnSouth} {
				gomega.Expect(a.Scheme).To(gomega.Equal(OvnDBSchemeUnix))
				gomega.Expect(a.PrivKey).To(gomega.Equal(""))
				gomega.Expect(a.Cert).To(gomega.Equal(""))
				gomega.Expect(a.CACert).To(gomega.Equal(""))
				gomega.Expect(a.Address).To(gomega.MatchRegexp("unix:/var/run/ovn/ovn[sn]b_db.sock"))
				gomega.Expect(a.CertCommonName).To(gomega.Equal(""))
			}
			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
			// k8s-api-token-file
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-token-file",
				Output: "/new/path/to/token",
			})
			// k8s-ca-certificate
			fname, fdata, err := createTempFile("ca.crt")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				K8sTokenFile:    true,
				K8sCert:         true,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)

			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://somewhere.com:8081"))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(fname))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(fdata))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("asadfasdfasrw3atr3r3rf33fasdaa3233"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/new/path/to/token"))
			gomega.Expect(OvnNorth.Scheme).To(gomega.Equal(OvnDBSchemeTCP))
			gomega.Expect(OvnNorth.PrivKey).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.Cert).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.CACert).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.Address).To(gomega.Equal("tcp:1.1.1.1:6441"))
			gomega.Expect(OvnNorth.CertCommonName).To(gomega.Equal(""))

			gomega.Expect(OvnSouth.Scheme).To(gomega.Equal(OvnDBSchemeUnix))
			gomega.Expect(OvnSouth.PrivKey).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.Cert).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.CACert).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.Address).To(gomega.Equal("unix:/var/run/ovn/ovnsb_db.sock"))
			gomega.Expect(OvnSouth.CertCommonName).To(gomega.Equal(""))

			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
			// k8s-api-token-file
			fexec.AddFakeCmd(&ovntest.ExpectedCmd{
				Cmd:    "ovs-vsctl --timeout=15 --if-exists get Open_vSwitch . external_ids:k8s-api-token-file",
				Output: "/new/path/to/token",
			})
			// k8s-ca-certificate
			fname, fdata, err := createTempFile("kube-cacert.pem")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
			gomega.Expect(err1).NotTo(gomega.HaveOccurred())
			defer os.Remove(tokenFile)

			cfgPath, err := InitConfigSa(ctx, fexec, tmpDir, &Defaults{
				OvnNorthAddress: true,
				K8sAPIServer:    true,
				K8sToken:        true,
				K8sTokenFile:    true,
				K8sCert:         true,
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)

			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://somewhere.com:8081"))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(fname))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(fdata))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("asadfasdfasrw3atr3r3rf33fasdaa3233"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/new/path/to/token"))

			gomega.Expect(OvnNorth.Scheme).To(gomega.Equal(OvnDBSchemeTCP))
			gomega.Expect(OvnNorth.PrivKey).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.Cert).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.CACert).To(gomega.Equal(""))
			gomega.Expect(OvnNorth.Address).To(
				gomega.Equal("tcp:1.1.1.1:6441,tcp:1.1.1.2:6641,tcp:1.1.1.3:6641"))
			gomega.Expect(OvnNorth.CertCommonName).To(gomega.Equal(""))

			gomega.Expect(OvnSouth.Scheme).To(gomega.Equal(OvnDBSchemeUnix))
			gomega.Expect(OvnSouth.PrivKey).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.Cert).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.CACert).To(gomega.Equal(""))
			gomega.Expect(OvnSouth.Address).To(gomega.Equal("unix:/var/run/ovn/ovnsb_db.sock"))
			gomega.Expect(OvnSouth.CertCommonName).To(gomega.Equal(""))

			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("uses serviceaccount files", func() {
		caFile, caData, err := createTempFile("ca.crt")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(caFile)

		tokenFile, err1 := createTempFileContent("token", "TG9yZW0gaXBzdW0gZ")
		gomega.Expect(err1).NotTo(gomega.HaveOccurred())
		defer os.Remove(tokenFile)

		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(caFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(caData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("TG9yZW0gaXBzdW0gZ"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal(tokenFile))
			return nil
		}
		err2 := app.Run([]string{app.Name})
		gomega.Expect(err2).NotTo(gomega.HaveOccurred())

	})

	It("uses environment variables", func() {
		kubeconfigEnvFile, _, err := createTempFile("kubeconfig.env")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeconfigEnvFile)
		os.Setenv("KUBECONFIG", kubeconfigEnvFile)
		defer os.Setenv("KUBECONFIG", "")

		os.Setenv("K8S_TOKEN", "this is the  token test")
		defer os.Setenv("K8S_TOKEN", "")

		os.Setenv("K8S_TOKEN_FILE", "/new/path/to/token")
		defer os.Setenv("K8S_TOKEN_FILE", "")

		os.Setenv("BOOTSTRAP_KUBECONFIG", "/new/path/to/bootstrap-kubeconfig")
		defer os.Setenv("BOOTSTRAP_KUBECONFIG", "")

		os.Setenv("CERT_DIR", "/new/path/to/cert-dir")
		defer os.Setenv("CERT_DIR", "")

		os.Setenv("CERT_PREFIX", "ovnkube-node")
		defer os.Setenv("CERT_PREFIX", "")

		os.Setenv("K8S_APISERVER", "https://9.2.3.4:6443")
		defer os.Setenv("K8S_APISERVER", "")

		kubeCAFile, kubeCAData, err1 := createTempFile("kube-ca.crt")
		gomega.Expect(err1).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeCAFile)
		os.Setenv("K8S_CACERT", kubeCAFile)
		defer os.Setenv("K8S_CACERT", "")

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfigSa(ctx, kexec.New(), tmpDir, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(kubeconfigEnvFile))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal("/new/path/to/bootstrap-kubeconfig"))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal("/new/path/to/cert-dir"))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(kubeCAFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(kubeCAData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("this is the  token test"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/new/path/to/token"))
			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://9.2.3.4:6443"))

			return nil
		}
		err = app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

	})

	It("overrides defaults with config file options", func() {
		kubeconfigFile, _, err := createTempFile("kubeconfig")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeconfigFile)

		bootstrapKubeconfigFile := "/new/path/to/bootstrap-kubeconfig"
		certDir := "/new/path/to/cert-dir"

		kubeCAFile, kubeCAData, err := createTempFile("kube-ca.crt")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name(), "kubeconfig="+kubeconfigFile, "cacert="+kubeCAFile,
			"bootstrap-kubeconfig="+bootstrapKubeconfigFile,
			"cert-dir="+certDir,
			"enable-multi-network=true",
			"enable-multi-networkpolicy=true",
			"enable-interconnect=true",
			"enable-multi-external-gateway=true",
			"enable-admin-network-policy=true",
			"enable-persistent-ips=true",
			"zone=foo",
		)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))

			gomega.Expect(Default.MTU).To(gomega.Equal(1500))
			gomega.Expect(Default.ConntrackZone).To(gomega.Equal(64321))
			gomega.Expect(Default.LFlowCacheEnable).To(gomega.BeTrue())
			gomega.Expect(Default.LFlowCacheLimit).To(gomega.Equal(uint(1000)))
			gomega.Expect(Default.LFlowCacheLimitKb).To(gomega.Equal(uint(100000)))
			gomega.Expect(Logging.File).To(gomega.Equal("/var/log/ovnkube.log"))
			gomega.Expect(Logging.Level).To(gomega.Equal(5))
			gomega.Expect(Logging.ACLLoggingRateLimit).To(gomega.Equal(20))
			gomega.Expect(Monitoring.RawNetFlowTargets).To(gomega.Equal("2.2.2.2:2055"))
			gomega.Expect(Monitoring.RawSFlowTargets).To(gomega.Equal("2.2.2.2:2056"))
			gomega.Expect(Monitoring.RawIPFIXTargets).To(gomega.Equal("2.2.2.2:2057"))
			gomega.Expect(IPFIX.Sampling).To(gomega.Equal(uint(123)))
			gomega.Expect(IPFIX.CacheMaxFlows).To(gomega.Equal(uint(456)))
			gomega.Expect(IPFIX.CacheActiveTimeout).To(gomega.Equal(uint(789)))
			gomega.Expect(CNI.ConfDir).To(gomega.Equal("/etc/cni/net.d22"))
			gomega.Expect(CNI.Plugin).To(gomega.Equal("ovn-k8s-cni-overlay22"))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(kubeconfigFile))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal(bootstrapKubeconfigFile))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal(certDir))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(kubeCAFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(kubeCAData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("TG9yZW0gaXBzdW0gZ"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/path/to/token"))
			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://1.2.3.4:6443"))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.18.0.0/24"))
			gomega.Expect(Kubernetes.HealthzBindAddress).To(gomega.Equal("0.0.0.0:1234"))
			gomega.Expect(Kubernetes.DNSServiceNamespace).To(gomega.Equal("kube-system-f"))
			gomega.Expect(Kubernetes.DNSServiceName).To(gomega.Equal("kube-dns-f"))
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("10.132.0.0/14"), 23},
			}))
			gomega.Expect(Default.Zone).To(gomega.Equal("foo"))

			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("1.1.1.1:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("1.1.1.2:8081"))
			gomega.Expect(Metrics.ExportOVSMetrics).To(gomega.Equal(true))
			gomega.Expect(Metrics.EnablePprof).To(gomega.Equal(true))
			gomega.Expect(Metrics.NodeServerPrivKey).To(gomega.Equal("/path/to/node-metrics-private.key"))
			gomega.Expect(Metrics.NodeServerCert).To(gomega.Equal("/path/to/node-metrics.crt"))
			gomega.Expect(Metrics.EnableConfigDuration).To(gomega.Equal(true))
			gomega.Expect(Metrics.EnableScaleMetrics).To(gomega.Equal(true))

			gomega.Expect(OvnNorth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnNorth.PrivKey).To(gomega.Equal("/path/to/nb-client-private.key"))
			gomega.Expect(OvnNorth.Cert).To(gomega.Equal("/path/to/nb-client.crt"))
			gomega.Expect(OvnNorth.CACert).To(gomega.Equal("/path/to/nb-client-ca.crt"))
			gomega.Expect(OvnNorth.Address).To(gomega.Equal("ssl:1.2.3.4:6641"))
			gomega.Expect(OvnNorth.CertCommonName).To(gomega.Equal("cfg-nbcommonname"))

			gomega.Expect(OvnSouth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnSouth.PrivKey).To(gomega.Equal("/path/to/sb-client-private.key"))
			gomega.Expect(OvnSouth.Cert).To(gomega.Equal("/path/to/sb-client.crt"))
			gomega.Expect(OvnSouth.CACert).To(gomega.Equal("/path/to/sb-client-ca.crt"))
			gomega.Expect(OvnSouth.Address).To(gomega.Equal("ssl:1.2.3.4:6642"))
			gomega.Expect(OvnSouth.CertCommonName).To(gomega.Equal("cfg-sbcommonname"))

			gomega.Expect(Gateway.Mode).To(gomega.Equal(GatewayModeShared))
			gomega.Expect(Gateway.Interface).To(gomega.Equal("eth1"))
			gomega.Expect(Gateway.NextHop).To(gomega.Equal("1.3.4.5"))
			gomega.Expect(Gateway.VLANID).To(gomega.Equal(uint(10)))
			gomega.Expect(Gateway.NodeportEnable).To(gomega.BeFalse())
			gomega.Expect(Gateway.V4JoinSubnet).To(gomega.Equal("100.65.0.0/16"))
			gomega.Expect(Gateway.V6JoinSubnet).To(gomega.Equal("fd90::/64"))
			gomega.Expect(Gateway.V4MasqueradeSubnet).To(gomega.Equal("169.254.169.0/29"))
			gomega.Expect(Gateway.V6MasqueradeSubnet).To(gomega.Equal("fd69::/125"))
			gomega.Expect(Gateway.RouterSubnet).To(gomega.Equal("10.50.0.0/16"))
			gomega.Expect(Gateway.SingleNode).To(gomega.BeFalse())
			gomega.Expect(Gateway.DisableForwarding).To(gomega.BeTrue())
			gomega.Expect(Gateway.AllowNoUplink).To(gomega.BeFalse())

			gomega.Expect(HybridOverlay.Enabled).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(3))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(1234))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetwork).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableInterconnect).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeTrue())
			gomega.Expect(HybridOverlay.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("11.132.0.0/14"), 23},
			}))
			gomega.Expect(ClusterManager.V4TransitSwitchSubnet).To(gomega.Equal("100.89.0.0/16"))
			gomega.Expect(ClusterManager.V6TransitSwitchSubnet).To(gomega.Equal("fd98::/64"))

			return nil
		}
		err = app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides config file and defaults with CLI options", func() {
		kubeconfigFile, _, err := createTempFile("kubeconfig")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeconfigFile)

		bootstrapKubeconfigFile := "/new/path/to/bootstrap-kubeconfig"
		certDir := "/new/path/to/cert-dir"

		kubeCAFile, kubeCAData, err := createTempFile("kube-ca.crt")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))

			gomega.Expect(Default.MTU).To(gomega.Equal(1234))
			gomega.Expect(Default.ConntrackZone).To(gomega.Equal(5555))
			gomega.Expect(Default.LFlowCacheEnable).To(gomega.BeTrue())
			gomega.Expect(Default.LFlowCacheLimit).To(gomega.Equal(uint(500)))
			gomega.Expect(Default.LFlowCacheLimitKb).To(gomega.Equal(uint(50000)))
			gomega.Expect(Logging.File).To(gomega.Equal("/some/logfile"))
			gomega.Expect(Logging.Level).To(gomega.Equal(3))
			gomega.Expect(Logging.ACLLoggingRateLimit).To(gomega.Equal(30))
			gomega.Expect(CNI.ConfDir).To(gomega.Equal("/some/cni/dir"))
			gomega.Expect(CNI.Plugin).To(gomega.Equal("a-plugin"))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(kubeconfigFile))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal(bootstrapKubeconfigFile))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal(certDir))
			gomega.Expect(Kubernetes.CertDuration).To(gomega.Equal(time.Second * 999))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(kubeCAFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(kubeCAData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("asdfasdfasdfasfd"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/new/path/to/token"))
			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://4.4.3.2:8080"))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.15.0.0/24"))
			gomega.Expect(Kubernetes.RawNoHostSubnetNodes).To(gomega.Equal("test=pass"))
			gomega.Expect(Kubernetes.HealthzBindAddress).To(gomega.Equal("0.0.0.0:4321"))
			gomega.Expect(Kubernetes.DNSServiceNamespace).To(gomega.Equal("kube-system-2"))
			gomega.Expect(Kubernetes.DNSServiceName).To(gomega.Equal("kube-dns-2"))
			gomega.Expect(Kubernetes.DisableRequestedChassis).To(gomega.BeTrue())
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("10.130.0.0/15"), 24},
			}))
			gomega.Expect(Default.Zone).To(gomega.Equal("bar"))

			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("2.2.2.2:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("2.2.2.3:8081"))
			gomega.Expect(Metrics.ExportOVSMetrics).To(gomega.Equal(true))
			gomega.Expect(Metrics.EnablePprof).To(gomega.Equal(true))
			gomega.Expect(Metrics.NodeServerPrivKey).To(gomega.Equal("/tls/nodeprivkey"))
			gomega.Expect(Metrics.NodeServerCert).To(gomega.Equal("/tls/nodecert"))
			gomega.Expect(Metrics.EnableConfigDuration).To(gomega.Equal(true))
			gomega.Expect(Metrics.EnableScaleMetrics).To(gomega.Equal(true))

			gomega.Expect(OvnNorth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnNorth.PrivKey).To(gomega.Equal("/client/privkey"))
			gomega.Expect(OvnNorth.Cert).To(gomega.Equal("/client/cert"))
			gomega.Expect(OvnNorth.CACert).To(gomega.Equal("/client/cacert"))
			gomega.Expect(OvnNorth.Address).To(gomega.Equal("ssl:6.5.4.3:6651"))
			gomega.Expect(OvnNorth.CertCommonName).To(gomega.Equal("testnbcommonname"))

			gomega.Expect(OvnSouth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnSouth.PrivKey).To(gomega.Equal("/client/privkey2"))
			gomega.Expect(OvnSouth.Cert).To(gomega.Equal("/client/cert2"))
			gomega.Expect(OvnSouth.CACert).To(gomega.Equal("/client/cacert2"))
			gomega.Expect(OvnSouth.Address).To(gomega.Equal("ssl:6.5.4.1:6652"))
			gomega.Expect(OvnSouth.CertCommonName).To(gomega.Equal("testsbcommonname"))

			gomega.Expect(Gateway.Mode).To(gomega.Equal(GatewayModeShared))
			gomega.Expect(Gateway.NodeportEnable).To(gomega.BeTrue())
			gomega.Expect(Gateway.V4JoinSubnet).To(gomega.Equal("100.63.0.0/16"))
			gomega.Expect(Gateway.V6JoinSubnet).To(gomega.Equal("fd99::/48"))
			gomega.Expect(Gateway.V4MasqueradeSubnet).To(gomega.Equal("169.253.169.0/29"))
			gomega.Expect(Gateway.V6MasqueradeSubnet).To(gomega.Equal("fd68::/125"))
			gomega.Expect(Gateway.RouterSubnet).To(gomega.Equal("10.55.0.0/16"))
			gomega.Expect(Gateway.SingleNode).To(gomega.BeTrue())
			gomega.Expect(Gateway.DisableForwarding).To(gomega.BeTrue())
			gomega.Expect(Gateway.AllowNoUplink).To(gomega.BeTrue())

			gomega.Expect(HybridOverlay.Enabled).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(5))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(4321))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetwork).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableInterconnect).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeTrue())
			gomega.Expect(HybridOverlay.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("11.132.0.0/14"), 23},
			}))
			gomega.Expect(Default.MonitorAll).To(gomega.BeFalse())
			gomega.Expect(Default.OfctrlWaitBeforeClear).To(gomega.Equal(5000))
			gomega.Expect(ClusterManager.V4TransitSwitchSubnet).To(gomega.Equal("100.90.0.0/16"))
			gomega.Expect(ClusterManager.V6TransitSwitchSubnet).To(gomega.Equal("fd96::/64"))

			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-mtu=1234",
			"-conntrack-zone=5555",
			"-lflow-cache-limit=500",
			"-lflow-cache-limit-kb=50000",
			"-loglevel=3",
			"-logfile=/some/logfile",
			"-acl-logging-rate-limit=30",
			"-cni-conf-dir=/some/cni/dir",
			"-cni-plugin=a-plugin",
			"-cluster-subnets=10.130.0.0/15/24",
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-bootstrap-kubeconfig=" + bootstrapKubeconfigFile,
			"-cert-dir=" + certDir,
			"-cert-duration=999s",
			"-k8s-apiserver=https://4.4.3.2:8080",
			"-k8s-cacert=" + kubeCAFile,
			"-k8s-token=asdfasdfasdfasfd",
			"-k8s-token-file=/new/path/to/token",
			"-k8s-service-cidrs=172.15.0.0/24",
			"-nb-address=ssl:6.5.4.3:6651",
			"-no-hostsubnet-nodes=test=pass",
			"-nb-client-privkey=/client/privkey",
			"-nb-client-cert=/client/cert",
			"-nb-client-cacert=/client/cacert",
			"-nb-cert-common-name=testnbcommonname",
			"-sb-address=ssl:6.5.4.1:6652",
			"-sb-client-privkey=/client/privkey2",
			"-sb-client-cert=/client/cert2",
			"-sb-client-cacert=/client/cacert2",
			"-sb-cert-common-name=testsbcommonname",
			"-node-server-privkey=/tls/nodeprivkey",
			"-node-server-cert=/tls/nodecert",
			"-gateway-mode=shared",
			"-nodeport",
			"-gateway-v4-join-subnet=100.63.0.0/16",
			"-gateway-v6-join-subnet=fd99::/48",
			"-gateway-v4-masquerade-subnet=169.253.169.0/29",
			"-gateway-v6-masquerade-subnet=fd68::/125",
			"-gateway-router-subnet=10.55.0.0/16",
			"-single-node",
			"-disable-forwarding",
			"-allow-no-uplink",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=11.132.0.0/14/23",
			"-monitor-all=false",
			"-metrics-bind-address=2.2.2.2:8080",
			"-ovn-metrics-bind-address=2.2.2.3:8081",
			"-export-ovs-metrics=false",
			"-metrics-enable-pprof=false",
			"-ofctrl-wait-before-clear=5000",
			"-metrics-enable-config-duration=true",
			"-egressip-reachability-total-timeout=5",
			"-egressip-node-healthcheck-port=4321",
			"-enable-multi-network=true",
			"-enable-multi-networkpolicy=true",
			"-enable-interconnect=true",
			"-enable-multi-external-gateway=true",
			"-enable-admin-network-policy=true",
			"-enable-persistent-ips=true",
			"-healthz-bind-address=0.0.0.0:4321",
			"-zone=bar",
			"-dns-service-namespace=kube-system-2",
			"-dns-service-name=kube-dns-2",
			"-disable-requestedchassis=true",
			"-cluster-manager-v4-transit-switch-subnet=100.90.0.0/16",
			"-cluster-manager-v6-transit-switch-subnet=fd96::/64",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy service-cluster-ip-range option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
service-cidrs=172.18.0.0/24
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.15.0.0/24"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-service-cluster-ip-range=172.15.0.0/24",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("accepts legacy service-cidr config file option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
service-cidr=172.18.0.0/24
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.18.0.0/24"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("returns an error when the k8s-service-cidrs is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("kubernetes service network CIDR \"adsfasdfaf\" invalid: invalid CIDR address: adsfasdfaf"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-k8s-service-cidr=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy cluster-subnet option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[default]
cluster-subnets=172.18.0.0/23
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("172.15.0.0/23"), 24},
			}))
			gomega.Expect(IPv4Mode).To(gomega.Equal(true))
			gomega.Expect(IPv6Mode).To(gomega.Equal(false))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-cluster-subnet=172.15.0.0/23",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("returns an error when the cluster-subnets is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("cluster subnet invalid: CIDR \"adsfasdfaf\" not properly formatted"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("returns an error when the hybrid overlay cluster-subnets is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("hybrid overlay cluster subnet invalid: CIDR \"adsfasdfaf\" not properly formatted"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-hybrid-overlay-cluster-subnets=adsfasdfaf",
			"-enable-hybrid-overlay",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy --init-gateways option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[gateway]
mode=local
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Gateway.Mode).To(gomega.Equal(GatewayModeShared))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-init-gateways",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides config file and defaults with CLI legacy --gateway-local option", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[gateway]
mode=shared
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Gateway.Mode).To(gomega.Equal(GatewayModeLocal))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-init-gateways",
			"-gateway-local",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("honors legacy [kubernetes] metrics config file options", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
metrics-bind-address=1.1.1.1:8080
ovn-metrics-bind-address=1.1.1.2:8081
metrics-enable-pprof=true
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("1.1.1.1:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("1.1.1.2:8081"))
			gomega.Expect(Metrics.EnablePprof).To(gomega.Equal(true))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("overrides legacy [kubernetes] metrics config file options with [metrics] ones", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
metrics-bind-address=1.1.1.1:8080
ovn-metrics-bind-address=1.1.1.2:8081
metrics-enable-pprof=false

[metrics]
bind-address=2.2.2.2:8080
ovn-metrics-bind-address=2.2.2.3:8081
enable-pprof=true
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("2.2.2.2:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("2.2.2.3:8081"))
			gomega.Expect(Metrics.EnablePprof).To(gomega.Equal(true))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("returns an error when the gateway mode is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid gateway mode \"adsfasdfaf\": expect one of shared,local"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-mode=adsfasdfaf",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("returns an error when the vlan-id is specified for mode other than shared gateway mode", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("gateway VLAN ID option: 30 is supported only in shared gateway mode"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-mode=local",
			"-gateway-vlanid=30",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("returns an error when the v4 join subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid gateway v4 join subnet specified, subnet: foobar: error: invalid CIDR address: foobar"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-v4-join-subnet=foobar",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("returns an error when the v6 join subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid gateway v6 join subnet specified, subnet: 192.168.0.0/16: error: <nil>"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-v6-join-subnet=192.168.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("returns an error when the v4 masquerade subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid gateway v4 masquerade subnet specified, subnet: foobar: error: invalid CIDR address: foobar"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-v4-masquerade-subnet=foobar",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("returns an error when the v6 masquerade subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid gateway v6 masquerade subnet specified, subnet: 192.168.0.0/16: error: <nil>"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-v6-masquerade-subnet=192.168.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("successfully overrides the default masquerade subnets", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-gateway-v4-masquerade-subnet=169.254.168.0/29",
			"-gateway-v6-masquerade-subnet=fd68::/125",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(Gateway.V4MasqueradeSubnet).To(gomega.Equal("169.254.168.0/29"))
		gomega.Expect(Gateway.V6MasqueradeSubnet).To(gomega.Equal("fd68::/125"))
		gomega.Expect(Gateway.MasqueradeIPs.V4OVNMasqueradeIP.String()).To(gomega.Equal("169.254.168.1"))
		gomega.Expect(Gateway.MasqueradeIPs.V6OVNMasqueradeIP.String()).To(gomega.Equal("fd68::1"))

	})
	It("returns an error when the v4 transit switch subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid transit switch v4 subnet specified, subnet: foobar: error: invalid CIDR address: foobar"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-manager-v4-transit-switch-subnet=foobar",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("returns an error when the v6 transit switch subnet specified is invalid", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("invalid transit switch v6 subnet specified, subnet: 100.89.0.0/16: error: <nil>"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-manager-v6-transit-switch-subnet=100.89.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
	It("successfully overrides the default transit switch subnets", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-manager-v4-transit-switch-subnet=100.89.0.0/16",
			"-cluster-manager-v6-transit-switch-subnet=fd98::/64",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ClusterManager.V4TransitSwitchSubnet).To(gomega.Equal("100.89.0.0/16"))
		gomega.Expect(ClusterManager.V6TransitSwitchSubnet).To(gomega.Equal("fd98::/64"))
	})
	It("overrides config file and defaults with CLI options (multi-master)", func() {
		kubeconfigFile, _, err := createTempFile("kubeconfig")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeconfigFile)

		bootstrapKubeconfigFile := "/new/path/to/bootstrap-kubeconfig"
		certDir := "/new/path/to/cert-dir"

		kubeCAFile, kubeCAData, err := createTempFile("kube-ca.crt")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))

			gomega.Expect(Default.MTU).To(gomega.Equal(1234))
			gomega.Expect(Default.ConntrackZone).To(gomega.Equal(5555))
			gomega.Expect(Default.LFlowCacheEnable).To(gomega.BeTrue())
			gomega.Expect(Default.LFlowCacheLimit).To(gomega.Equal(uint(500)))
			gomega.Expect(Default.LFlowCacheLimitKb).To(gomega.Equal(uint(50000)))
			gomega.Expect(Logging.File).To(gomega.Equal("/some/logfile"))
			gomega.Expect(Logging.Level).To(gomega.Equal(3))
			gomega.Expect(Monitoring.RawNetFlowTargets).To(gomega.Equal("2.2.2.2:2055"))
			gomega.Expect(Monitoring.RawSFlowTargets).To(gomega.Equal("2.2.2.2:2056"))
			gomega.Expect(Monitoring.RawIPFIXTargets).To(gomega.Equal("2.2.2.2:2057"))
			gomega.Expect(IPFIX.Sampling).To(gomega.Equal(uint(1123)))
			gomega.Expect(IPFIX.CacheMaxFlows).To(gomega.Equal(uint(1456)))
			gomega.Expect(IPFIX.CacheActiveTimeout).To(gomega.Equal(uint(1789)))
			gomega.Expect(CNI.ConfDir).To(gomega.Equal("/some/cni/dir"))
			gomega.Expect(CNI.Plugin).To(gomega.Equal("a-plugin"))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(kubeconfigFile))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal(bootstrapKubeconfigFile))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal(certDir))
			gomega.Expect(Kubernetes.CertDuration).To(gomega.Equal(time.Second * 999))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(kubeCAFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(kubeCAData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("asdfasdfasdfasfd"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/new/path/to/token"))
			gomega.Expect(Kubernetes.APIServer).To(gomega.Equal("https://4.4.3.2:8080"))
			gomega.Expect(Kubernetes.RawNoHostSubnetNodes).To(gomega.Equal("label=another-test-label"))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.15.0.0/24"))

			gomega.Expect(OvnNorth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnNorth.PrivKey).To(gomega.Equal("/client/privkey"))
			gomega.Expect(OvnNorth.Cert).To(gomega.Equal("/client/cert"))
			gomega.Expect(OvnNorth.CACert).To(gomega.Equal("/client/cacert"))
			gomega.Expect(OvnNorth.Address).To(
				gomega.Equal("ssl:6.5.4.3:6651,ssl:6.5.4.4:6651,ssl:6.5.4.5:6651"))
			gomega.Expect(OvnNorth.CertCommonName).To(gomega.Equal("testnbcommonname"))

			gomega.Expect(OvnSouth.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(OvnSouth.PrivKey).To(gomega.Equal("/client/privkey2"))
			gomega.Expect(OvnSouth.Cert).To(gomega.Equal("/client/cert2"))
			gomega.Expect(OvnSouth.CACert).To(gomega.Equal("/client/cacert2"))
			gomega.Expect(OvnSouth.Address).To(
				gomega.Equal("ssl:6.5.4.1:6652,ssl:6.5.4.2:6652,ssl:6.5.4.3:6652"))
			gomega.Expect(OvnSouth.CertCommonName).To(gomega.Equal("testsbcommonname"))
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(3))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(12345))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-mtu=1234",
			"-conntrack-zone=5555",
			"-lflow-cache-limit=500",
			"-lflow-cache-limit-kb=50000",
			"-loglevel=3",
			"-logfile=/some/logfile",
			"-netflow-targets=2.2.2.2:2055",
			"-sflow-targets=2.2.2.2:2056",
			"-ipfix-targets=2.2.2.2:2057",
			"-ipfix-sampling=1123",
			"-ipfix-cache-max-flows=1456",
			"-ipfix-cache-active-timeout=1789",
			"-cni-conf-dir=/some/cni/dir",
			"-cni-plugin=a-plugin",
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-bootstrap-kubeconfig=" + bootstrapKubeconfigFile,
			"-cert-dir=" + certDir,
			"-cert-duration=999s",
			"-k8s-apiserver=https://4.4.3.2:8080",
			"-k8s-cacert=" + kubeCAFile,
			"-k8s-token=asdfasdfasdfasfd",
			"-k8s-token-file=/new/path/to/token",
			"-k8s-service-cidr=172.15.0.0/24",
			"-nb-address=ssl:6.5.4.3:6651,ssl:6.5.4.4:6651,ssl:6.5.4.5:6651",
			"-nb-client-privkey=/client/privkey",
			"-nb-client-cert=/client/cert",
			"-nb-client-cacert=/client/cacert",
			"-nb-cert-common-name=testnbcommonname",
			"-sb-address=ssl:6.5.4.1:6652,ssl:6.5.4.2:6652,ssl:6.5.4.3:6652",
			"-sb-client-privkey=/client/privkey2",
			"-sb-client-cert=/client/cert2",
			"-sb-client-cacert=/client/cacert2",
			"-sb-cert-common-name=testsbcommonname",
			"-egressip-reachability-total-timeout=3",
			"-egressip-node-healthcheck-port=12345",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("does not override config file settings with default cli options", func() {
		kubeconfigFile, _, err := createTempFile("kubeconfig")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeconfigFile)

		bootstrapKubeconfigFile := "/new/path/to/bootstrap-kubeconfig"
		certDir := "/new/path/to/cert-dir"

		kubeCAFile, kubeCAData, err := createTempFile("kube-ca.crt")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(kubeCAFile)

		err = writeTestConfigFile(cfgFile.Name())
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))

			gomega.Expect(Default.MTU).To(gomega.Equal(1500))
			gomega.Expect(Default.ConntrackZone).To(gomega.Equal(64321))
			gomega.Expect(Default.LFlowCacheEnable).To(gomega.BeTrue())
			gomega.Expect(Default.LFlowCacheLimit).To(gomega.Equal(uint(1000)))
			gomega.Expect(Default.LFlowCacheLimitKb).To(gomega.Equal(uint(100000)))
			gomega.Expect(Default.RawClusterSubnets).To(gomega.Equal("10.132.0.0/14/23"))
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("10.132.0.0/14"), 23},
			}))
			gomega.Expect(Logging.File).To(gomega.Equal("/var/log/ovnkube.log"))
			gomega.Expect(Logging.Level).To(gomega.Equal(5))
			gomega.Expect(CNI.ConfDir).To(gomega.Equal("/etc/cni/net.d22"))
			gomega.Expect(CNI.Plugin).To(gomega.Equal("ovn-k8s-cni-overlay22"))
			gomega.Expect(Kubernetes.Kubeconfig).To(gomega.Equal(kubeconfigFile))
			gomega.Expect(Kubernetes.BootstrapKubeconfig).To(gomega.Equal(bootstrapKubeconfigFile))
			gomega.Expect(Kubernetes.CertDir).To(gomega.Equal(certDir))
			gomega.Expect(Kubernetes.CACert).To(gomega.Equal(kubeCAFile))
			gomega.Expect(Kubernetes.CAData).To(gomega.Equal(kubeCAData))
			gomega.Expect(Kubernetes.Token).To(gomega.Equal("TG9yZW0gaXBzdW0gZ"))
			gomega.Expect(Kubernetes.TokenFile).To(gomega.Equal("/path/to/token"))
			gomega.Expect(Kubernetes.RawServiceCIDRs).To(gomega.Equal("172.18.0.0/24"))

			return nil
		}

		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
			"-k8s-kubeconfig=" + kubeconfigFile,
			"-bootstrap-kubeconfig=" + bootstrapKubeconfigFile,
			"-cert-dir=" + certDir,
			"-k8s-cacert=" + kubeCAFile,
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("allows configuring a single-stack IPv6 cluster", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(IPv4Mode).To(gomega.Equal(false))
			gomega.Expect(IPv6Mode).To(gomega.Equal(true))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=fd01::/48/64",
			"-k8s-service-cidrs=fd02::/112",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("allows configuring a dual-stack cluster", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(IPv4Mode).To(gomega.Equal(true))
			gomega.Expect(IPv6Mode).To(gomega.Equal(true))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24,fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16,fd02::/112",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("allows configuring a dual-stack cluster with multiple IPv4 cluster subnet ranges", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(IPv4Mode).To(gomega.Equal(true))
			gomega.Expect(IPv6Mode).To(gomega.Equal(true))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24,10.2.0.0/16/24,fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16,fd02::/112",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with IPv4 pods and IPv6 services", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("illegal network configuration: IPv4 cluster subnet, IPv6 service subnet"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24",
			"-k8s-service-cidrs=fd02::/112",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with IPv6 pods and IPv4 services", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("illegal network configuration: IPv6 cluster subnet, IPv4 service subnet"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with dual-stack pods and single-stack services", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("illegal network configuration: dual-stack cluster subnet, IPv4 service subnet"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24,fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with single-stack pods and dual-stack services", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("illegal network configuration: IPv6 cluster subnet, dual-stack service subnet"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16,fd02::/112",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with multiple single-stack service CIDRs", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("kubernetes service-cidrs must contain either a single CIDR or else an IPv4/IPv6 pair"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24",
			"-k8s-service-cidrs=172.30.0.0/16,172.31.0.0/16",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a cluster with dual-stack cluster subnets and single-stack hybrid overlap subnets", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError("illegal network configuration: dual-stack cluster subnet, dual-stack service subnet, IPv4 hybrid overlay subnet"))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-cluster-subnets=10.0.0.0/16/24,fd01::/48/64",
			"-k8s-service-cidrs=172.30.0.0/16,fd02::/112",
			"-enable-hybrid-overlay",
			"-hybrid-overlay-cluster-subnets=10.132.0.0/14/23",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("ignores unknown fields in config file and does not return an error", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[default]
key=value
mtu=1234

[foobar]
foo=bar
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			var cfgPath string
			cfgPath, err = InitConfig(ctx, kexec.New(), nil)

			// unknown section foobar and its keys & values should be ignored
			// same for key=value in default section
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(Default.MTU).To(gomega.Equal(1234))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a config with invalid syntax", func() {
		err := ioutil.WriteFile(cfgFile.Name(), []byte(`[default]
mtu=1234

[foobar
foo=bar
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			_, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.HaveOccurred())

			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	Describe("OvnDBAuth operations", func() {
		var certFile, keyFile, caFile string

		BeforeEach(func() {
			var err error
			certFile, _, err = createTempFile("cert.crt")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			keyFile, _, err = createTempFile("priv.key")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			caFile = filepath.Join(tmpDir, "ca.crt")
		})

		AfterEach(func() {
			err := os.Remove(certFile)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = os.Remove(keyFile)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			os.Remove(caFile)
		})

		const (
			nbURL             string = "ssl:1.2.3.4:6641"
			sbURL             string = "ssl:1.2.3.4:6642"
			nbDummyCommonName        = "cfg-nbcommonname"
			sbDummyCommonName        = "cfg-sbcommonname"
		)

		It("configures client northbound SSL correctly", func() {
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --db=" + nbURL + " --timeout=5 --private-key=" + keyFile + " --certificate=" + certFile + " --bootstrap-ca-cert=" + caFile + " list nb_global",
			})

			cliConfig := &OvnAuthConfig{
				Address:        nbURL,
				PrivKey:        keyFile,
				Cert:           certFile,
				CACert:         caFile,
				CertCommonName: nbDummyCommonName,
			}
			a, err := buildOvnAuth(fexec, true, cliConfig, &OvnAuthConfig{}, true)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(a.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(a.PrivKey).To(gomega.Equal(keyFile))
			gomega.Expect(a.Cert).To(gomega.Equal(certFile))
			gomega.Expect(a.CACert).To(gomega.Equal(caFile))
			gomega.Expect(a.Address).To(gomega.Equal(nbURL))
			gomega.Expect(a.CertCommonName).To(gomega.Equal(nbDummyCommonName))
			gomega.Expect(a.northbound).To(gomega.BeTrue())

			gomega.Expect(a.GetURL()).To(gomega.Equal(nbURL))
			err = a.SetDBAuth()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
		})

		It("configures client southbound SSL correctly", func() {
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovn-nbctl --db=" + sbURL + " --timeout=5 --private-key=" + keyFile + " --certificate=" + certFile + " --bootstrap-ca-cert=" + caFile + " list nb_global",
				"ovs-vsctl --timeout=15 del-ssl",
				"ovs-vsctl --timeout=15 set-ssl " + keyFile + " " + certFile + " " + caFile,
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-remote=\"" + sbURL + "\"",
			})

			cliConfig := &OvnAuthConfig{
				Address:        sbURL,
				PrivKey:        keyFile,
				Cert:           certFile,
				CACert:         caFile,
				CertCommonName: sbDummyCommonName,
			}
			a, err := buildOvnAuth(fexec, false, cliConfig, &OvnAuthConfig{}, false)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(a.Scheme).To(gomega.Equal(OvnDBSchemeSSL))
			gomega.Expect(a.PrivKey).To(gomega.Equal(keyFile))
			gomega.Expect(a.Cert).To(gomega.Equal(certFile))
			gomega.Expect(a.CACert).To(gomega.Equal(caFile))
			gomega.Expect(a.Address).To(gomega.Equal(sbURL))
			gomega.Expect(a.CertCommonName).To(gomega.Equal(sbDummyCommonName))
			gomega.Expect(a.northbound).To(gomega.BeFalse())

			gomega.Expect(a.GetURL()).To(gomega.Equal(sbURL))
			err = a.SetDBAuth()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
		})

		const (
			sbURLLegacy    string = "tcp://1.2.3.4:6642"
			sbURLConverted string = "tcp:1.2.3.4:6642"
		)

		It("configures client southbound TCP legacy address correctly", func() {
			fexec := ovntest.NewFakeExec()
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-remote=\"" + sbURLConverted + "\"",
			})

			cliConfig := &OvnAuthConfig{Address: sbURLLegacy}
			a, err := buildOvnAuth(fexec, false, cliConfig, &OvnAuthConfig{}, true)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(a.Scheme).To(gomega.Equal(OvnDBSchemeTCP))
			// Config should convert :// to : in addresses
			gomega.Expect(a.Address).To(gomega.Equal(sbURLConverted))
			gomega.Expect(a.northbound).To(gomega.BeFalse())

			gomega.Expect(a.GetURL()).To(gomega.Equal(sbURLConverted))
			err = a.SetDBAuth()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
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
				gomega.Expect(err.Error()).To(gomega.ContainSubstring(match))
			} else {
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				"open /foo/bar/baz.cert: no such file or directory",
				"-k8s-apiserver=https://localhost:8443", "-k8s-cacert=/foo/bar/baz.cert")

			generateTestsSimple("apiserver URL scheme is invalid",
				"kubernetes API server URL scheme \"gggggg\" invalid",
				"-k8s-apiserver=gggggg://localhost:8443")

			generateTestsSimple("apiserver URL is invalid",
				"invalid character \" \" in host name",
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
			certFile, _, err = createTempFile("cert.crt")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			keyFile, _, err = createTempFile("priv.key")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			caFile, _, err = createTempFile("ca.crt")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
					return []string{"address=blah:1.2.3.4:5555"}
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
						"address=tcp:4.3.2.1",
					}
				})

			generateTests("certs are provided for the TCP scheme",
				"certificate or key given; perhaps you mean to use the 'ssl' scheme?",
				func() []string {
					return []string{
						"address=tcp:1.2.3.4:444",
						"client-privkey=/bar/baz/foo",
					}
				})
		})

		Context("does not return an error when", func() {
			generateTests("the SSL scheme is missing a client CA cert", "",
				func() []string {
					return []string{
						"address=ssl:1.2.3.4:444",
						"client-privkey=" + keyFile,
						"client-cert=" + certFile,
						"cert-common-name=foobar",
						"client-cacert=/foo/bar/baz",
					}
				})

			generateTests("the SSL scheme is missing a private key file", "",
				func() []string {
					return []string{
						"address=ssl:1.2.3.4:444",
						"client-privkey=/foo/bar/baz",
						"client-cert=" + certFile,
						"client-cacert=" + caFile,
						"cert-common-name=foobar",
					}
				})

			generateTests("the SSL scheme is missing a client cert file", "",
				func() []string {
					return []string{
						"address=ssl:1.2.3.4:444",
						"client-privkey=" + keyFile,
						"client-cert=/foo/bar/baz",
						"client-cacert=" + caFile,
						"cert-common-name=foobar",
					}
				})
		})
	})

	Describe("OVN Kube Node config", func() {
		// NOTE: We test this here as the test that overrides values also sets hybridOverlay to true
		// which yields an invalid configuration.
		It("Overrides value from Config file", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeFull,
				},
			}
			file := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeDPU,
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeDPU))
		})

		It("Overrides value from CLI", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode:                   types.NodeModeDPUHost,
					MgmtPortNetdev:         "enp1s0f0v0",
					MgmtPortDPResourceName: "openshift.io/mgmtvf",
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &config{})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeDPUHost))
			gomega.Expect(OvnKubeNode.MgmtPortNetdev).To(gomega.Equal("enp1s0f0v0"))
			gomega.Expect(OvnKubeNode.MgmtPortDPResourceName).To(gomega.Equal("openshift.io/mgmtvf"))
		})

		It("Fails with unsupported mode", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: "invalid",
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &config{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("unexpected ovnkube-node-mode"))
		})

		It("Fails if hybrid overlay is enabled and ovnkube node mode is not full", func() {
			HybridOverlay.Enabled = true
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeDPU,
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &config{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(
				"hybrid overlay is not supported with ovnkube-node mode"))
		})

		It("Fails if management port is provided and ovnkube node mode is dpu", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode:           types.NodeModeDPU,
					MgmtPortNetdev: "enp1s0f0v0",
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &config{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must not be provided"))
		})

		It("Fails if management port is not provided and ovnkube node mode is dpu-host", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeDPUHost,
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &config{})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must be provided"))
		})

		It("Succeeds if management netdev provided in the full mode", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode:           types.NodeModeFull,
					MgmtPortNetdev: "ens1f0v0",
				},
			}
			file := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeFull,
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		It("Succeeds if management port device plugin resource name provided in the full mode", func() {
			cliConfig := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode:                   types.NodeModeFull,
					MgmtPortDPResourceName: "openshift.io/mgmtvf",
				},
			}
			file := config{
				OvnKubeNode: OvnKubeNodeConfig{
					Mode: types.NodeModeFull,
				},
			}
			err := buildOvnKubeNodeConfig(nil, &cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})
