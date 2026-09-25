// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"

	. "github.com/onsi/ginkgo/v2"
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
			gomega.Expect(parts).To(gomega.HaveLen(2))
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
	if err := os.WriteFile(fname, fileData, 0o644); err != nil {
		return "", nil, err
	}
	return fname, fileData, nil
}

func createTempFileContent(name, value string) (string, error) {
	fname := filepath.Join(tmpDir, name)
	if err := os.WriteFile(fname, []byte(value), 0o644); err != nil {
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

[tls]
tls-min-version=VersionTLS12
tls-cipher-suites=TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384

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
run-dir=/custom/ovn/run/
db-location=/custom/ovn/nb.db

[ovnsouth]
run-dir=/custom/ovn/run/
db-location=/custom/ovn/sb.db

[egressip-healthcheck-tls]
client-privkey=/path/to/nb-client-private.key
client-cert=/path/to/nb-client.crt
client-cacert=/path/to/nb-client-ca.crt
cert-common-name=cfg-nbcommonname

[ovspaths]
run-dir=/custom/ovs/run/

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
disable-udn-arp-ndp-flood=false

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
enable-network-segmentation=false
enable-network-connect=false
enable-uplink=false
enable-preconfigured-udn-addresses=false
enable-route-advertisements=false
advertised-udn-isolation-mode=strict
enable-multi-external-gateway=false
enable-admin-network-policy=false
enable-persistent-ips=false

[clustermanager]
v4-transit-subnet=100.89.0.0/16
v6-transit-subnet=fd98::/64
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
	return os.WriteFile(path, []byte(newData), 0o644)
}

var _ = Describe("Config redaction for logging", func() {
	It("clears the bearer token and CA material and keeps every other field", func() {
		cfg := config{
			Default: DefaultConfig{MTU: 1234},
			Kubernetes: KubernetesConfig{
				APIServer:  "https://1.2.3.4:6443",
				Token:      "secret-bearer-token",
				TokenFile:  "/var/run/secrets/token",
				CACert:     "/etc/ca.crt",
				CACertData: "-----BEGIN CERTIFICATE-----",
				CAData:     []byte("ca-bytes"),
			},
		}

		redacted := redactConfig(cfg)
		gomega.Expect(redacted.Kubernetes.Token).To(gomega.BeEmpty())
		gomega.Expect(redacted.Kubernetes.CACertData).To(gomega.BeEmpty())
		gomega.Expect(redacted.Kubernetes.CAData).To(gomega.BeEmpty())
		gomega.Expect(fmt.Sprintf("%+v", redacted)).NotTo(gomega.ContainSubstring("secret-bearer-token"))

		// putting the three redacted fields back must yield the original struct
		redacted.Kubernetes.Token = cfg.Kubernetes.Token
		redacted.Kubernetes.CACertData = cfg.Kubernetes.CACertData
		redacted.Kubernetes.CAData = cfg.Kubernetes.CAData
		gomega.Expect(redacted).To(gomega.Equal(cfg))

		// the input is left untouched, including the shared CAData backing array
		gomega.Expect(cfg.Kubernetes.Token).To(gomega.Equal("secret-bearer-token"))
		gomega.Expect(cfg.Kubernetes.CAData).To(gomega.Equal([]byte("ca-bytes")))
	})
})

var _ = Describe("Config Operations", func() {
	var app *cli.App
	var cfgFile *os.File

	var tmpErr error
	tmpDir, tmpErr = os.MkdirTemp("", "configtest_certdir")
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

		cfgFile, err = os.CreateTemp("", "conftest-")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	AfterEach(func() {
		os.Remove(cfgFile.Name())
	})

	It("does not log the bearer token read from the config file", func() {
		const token = "secret-bearer-token"
		var logs bytes.Buffer
		state := klog.CaptureState()
		defer state.Restore()
		klog.LogToStderr(false)
		klog.SetOutput(&logs)

		err := runInit(app, 2, cfgFile, "-k8s-token="+token)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(Kubernetes.Token).To(gomega.Equal(token))

		klog.Flush()
		// runInit sets loglevel 5, so both redacted log lines are emitted
		gomega.Expect(logs.String()).To(gomega.ContainSubstring("Parsed config:"))
		gomega.Expect(logs.String()).To(gomega.ContainSubstring("Kubernetes config:"))
		gomega.Expect(logs.String()).NotTo(gomega.ContainSubstring(token))
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
			gomega.Expect(TLS.MinVersion).To(gomega.Equal(""))
			gomega.Expect(TLS.ParseCipherSuites()).To(gomega.BeEmpty())
			gomega.Expect(Default.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("10.128.0.0/14"), 23},
			}))
			gomega.Expect(IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(IPv6Mode).To(gomega.BeFalse())
			gomega.Expect(HybridOverlay.Enabled).To(gomega.BeFalse())
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeFull))
			gomega.Expect(OvnKubeNode.MgmtPortNetdev).To(gomega.Equal(""))
			gomega.Expect(OvnKubeNode.MgmtPortDPResourceName).To(gomega.Equal(""))
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(DefaultRoutingTableIDStart))
			gomega.Expect(Gateway.RouterSubnet).To(gomega.Equal(""))
			gomega.Expect(Gateway.SingleNode).To(gomega.BeFalse())
			gomega.Expect(Gateway.DisableForwarding).To(gomega.BeFalse())
			gomega.Expect(Gateway.AllowNoUplink).To(gomega.BeFalse())
			gomega.Expect(Gateway.DisableUDNARPNDPFlood).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(1))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(0))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetwork).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableNetworkSegmentation).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableNetworkConnect).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableUplink).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnablePreconfiguredUDNAddresses).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableRouteAdvertisements).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetworkPolicy).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeFalse())
			gomega.Expect(OVNKubernetesFeature.AdvertisedUDNIsolationMode).To(gomega.Equal(AdvertisedUDNIsolationModeStrict))

			gomega.Expect(OvnNorth.GetURL()).To(gomega.Equal("unix:/var/run/ovn/ovnnb_db.sock"))
			gomega.Expect(OvnSouth.GetURL()).To(gomega.Equal("unix:/var/run/ovn/ovnsb_db.sock"))
			gomega.Expect(EgressIPHealthCheckTLS).To(gomega.Equal(EgressIPHealthCheckTLSConfig{}))
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

	It("parses routing table ID start from config file", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[ovnkubenode]
routing-table-id-start=2002
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			cfgPath, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(2002))
			return nil
		}

		err = app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("parses routing table ID start from CLI", func() {
		app.Action = func(ctx *cli.Context) error {
			cfgPath, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(cfgPath).To(gomega.Equal(cfgFile.Name()))
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(2003))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"--config-file=" + cfgFile.Name(),
			"--ovnkube-node-routing-table-id-start=2003",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
			"enable-network-segmentation=true",
			"enable-network-connect=true",
			"enable-uplink=true",
			"enable-preconfigured-udn-addresses=true",
			"enable-route-advertisements=true",
			"advertised-udn-isolation-mode=loose",
			"enable-multi-external-gateway=true",
			"enable-admin-network-policy=true",
			"enable-persistent-ips=true",
			"disable-udn-arp-ndp-flood=true",
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
			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("1.1.1.1:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("1.1.1.2:8081"))
			gomega.Expect(Metrics.ExportOVSMetrics).To(gomega.BeTrue())
			gomega.Expect(Metrics.EnablePprof).To(gomega.BeTrue())
			gomega.Expect(Metrics.NodeServerPrivKey).To(gomega.Equal("/path/to/node-metrics-private.key"))
			gomega.Expect(Metrics.NodeServerCert).To(gomega.Equal("/path/to/node-metrics.crt"))
			gomega.Expect(Metrics.EnableConfigDuration).To(gomega.BeTrue())
			gomega.Expect(Metrics.EnableScaleMetrics).To(gomega.BeTrue())

			gomega.Expect(TLS.MinVersion).To(gomega.Equal("VersionTLS12"))
			gomega.Expect(TLS.ParseCipherSuites()).To(gomega.Equal([]string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			}))

			gomega.Expect(EgressIPHealthCheckTLS.PrivKey).To(gomega.Equal("/path/to/nb-client-private.key"))
			gomega.Expect(EgressIPHealthCheckTLS.Cert).To(gomega.Equal("/path/to/nb-client.crt"))
			gomega.Expect(EgressIPHealthCheckTLS.CACert).To(gomega.Equal("/path/to/nb-client-ca.crt"))
			gomega.Expect(EgressIPHealthCheckTLS.CertCommonName).To(gomega.Equal("cfg-nbcommonname"))
			gomega.Expect(OvnNorth.RunDir).To(gomega.Equal("/custom/ovn/run/"))
			gomega.Expect(OvnNorth.DbLocation).To(gomega.Equal("/custom/ovn/nb.db"))
			gomega.Expect(OvnNorth.GetURL()).To(gomega.Equal("unix:/custom/ovn/run/ovnnb_db.sock"))

			gomega.Expect(OvnSouth.RunDir).To(gomega.Equal("/custom/ovn/run/"))
			gomega.Expect(OvnSouth.DbLocation).To(gomega.Equal("/custom/ovn/sb.db"))
			gomega.Expect(OvnSouth.GetURL()).To(gomega.Equal("unix:/custom/ovn/run/ovnsb_db.sock"))

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
			gomega.Expect(Gateway.DisableUDNARPNDPFlood).To(gomega.BeTrue())

			gomega.Expect(HybridOverlay.Enabled).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EgressIPReachabiltyTotalTimeout).To(gomega.Equal(3))
			gomega.Expect(OVNKubernetesFeature.EgressIPNodeHealthCheckPort).To(gomega.Equal(1234))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetwork).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableNetworkSegmentation).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableNetworkConnect).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableUplink).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePreconfiguredUDNAddresses).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableRouteAdvertisements).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.AdvertisedUDNIsolationMode).To(gomega.Equal(AdvertisedUDNIsolationModeLoose))
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeTrue())
			gomega.Expect(HybridOverlay.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("11.132.0.0/14"), 23},
			}))
			gomega.Expect(ClusterManager.V4TransitSubnet).To(gomega.Equal("100.89.0.0/16"))
			gomega.Expect(ClusterManager.V6TransitSubnet).To(gomega.Equal("fd98::/64"))

			gomega.Expect(OvsPaths.RunDir).To(gomega.Equal("/custom/ovs/run/"))

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
			gomega.Expect(Metrics.BindAddress).To(gomega.Equal("2.2.2.2:8080"))
			gomega.Expect(Metrics.OVNMetricsBindAddress).To(gomega.Equal("2.2.2.3:8081"))
			gomega.Expect(Metrics.ExportOVSMetrics).To(gomega.BeTrue())
			gomega.Expect(Metrics.EnablePprof).To(gomega.BeTrue())
			gomega.Expect(Metrics.NodeServerPrivKey).To(gomega.Equal("/tls/nodeprivkey"))
			gomega.Expect(Metrics.NodeServerCert).To(gomega.Equal("/tls/nodecert"))
			gomega.Expect(Metrics.EnableConfigDuration).To(gomega.BeTrue())
			gomega.Expect(Metrics.EnableScaleMetrics).To(gomega.BeTrue())
			gomega.Expect(Metrics.CollectionInterval).To(gomega.Equal(15))

			gomega.Expect(TLS.MinVersion).To(gomega.Equal("VersionTLS13"))
			gomega.Expect(TLS.ParseCipherSuites()).To(gomega.Equal([]string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
			}))

			gomega.Expect(EgressIPHealthCheckTLS.PrivKey).To(gomega.Equal("/client/privkey"))
			gomega.Expect(EgressIPHealthCheckTLS.Cert).To(gomega.Equal("/client/cert"))
			gomega.Expect(EgressIPHealthCheckTLS.CACert).To(gomega.Equal("/client/cacert"))
			gomega.Expect(EgressIPHealthCheckTLS.CertCommonName).To(gomega.Equal("testnbcommonname"))

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
			gomega.Expect(OVNKubernetesFeature.EnableNetworkSegmentation).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableNetworkConnect).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableUplink).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePreconfiguredUDNAddresses).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableRouteAdvertisements).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.AdvertisedUDNIsolationMode).To(gomega.Equal(AdvertisedUDNIsolationModeLoose))
			gomega.Expect(OVNKubernetesFeature.EnableMultiNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableMultiExternalGateway).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnableAdminNetworkPolicy).To(gomega.BeTrue())
			gomega.Expect(OVNKubernetesFeature.EnablePersistentIPs).To(gomega.BeTrue())
			gomega.Expect(HybridOverlay.ClusterSubnets).To(gomega.Equal([]CIDRNetworkEntry{
				{ovntest.MustParseIPNet("11.132.0.0/14"), 23},
			}))
			gomega.Expect(Default.MonitorAll).To(gomega.BeFalse())
			gomega.Expect(Default.OfctrlWaitBeforeClear).To(gomega.Equal(5000))
			gomega.Expect(ClusterManager.V4TransitSubnet).To(gomega.Equal("100.90.0.0/16"))
			gomega.Expect(ClusterManager.V6TransitSubnet).To(gomega.Equal("fd96::/64"))

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
			"-no-hostsubnet-nodes=test=pass",
			"-nb-client-privkey=/client/privkey",
			"-nb-client-cert=/client/cert",
			"-nb-client-cacert=/client/cacert",
			"-nb-cert-common-name=testnbcommonname",
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
			"-metrics-collection-interval=15",
			"-tls-min-version=VersionTLS13",
			"-tls-cipher-suites=TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384",
			"-egressip-reachability-total-timeout=5",
			"-egressip-node-healthcheck-port=4321",
			"-enable-multi-network=true",
			"-enable-multi-networkpolicy=true",
			"-enable-network-segmentation=true",
			"-enable-network-connect=true",
			"-enable-uplink=true",
			"-enable-preconfigured-udn-addresses=true",
			"-enable-route-advertisements=true",
			"-advertised-udn-isolation-mode=loose",
			"-enable-multi-external-gateway=true",
			"-enable-admin-network-policy=true",
			"-enable-persistent-ips=true",
			"-healthz-bind-address=0.0.0.0:4321",
			"-dns-service-namespace=kube-system-2",
			"-dns-service-name=kube-dns-2",
			"-disable-requestedchassis=true",
			"-cluster-manager-v4-transit-subnet=100.90.0.0/16",
			"-cluster-manager-v6-transit-subnet=fd96::/64",
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	DescribeTable("loads Egress IP health-check TLS settings with compatibility precedence",
		func(configData string, cliArgs []string, expected EgressIPHealthCheckTLSConfig) {
			err := os.WriteFile(cfgFile.Name(), []byte(configData), 0o644)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			app.Action = func(ctx *cli.Context) error {
				_, err := InitConfig(ctx, kexec.New(), nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(EgressIPHealthCheckTLS).To(gomega.Equal(expected))
				return nil
			}
			args := []string{app.Name, "-config-file=" + cfgFile.Name()}
			args = append(args, cliArgs...)
			err = app.Run(args)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
		Entry("accepts the legacy ovnnorth section", `[ovnnorth]
client-privkey=/legacy/client.key
client-cert=/legacy/client.crt
client-cacert=/legacy/client-ca.crt
cert-common-name=legacy.example.com
`, nil, EgressIPHealthCheckTLSConfig{
			PrivKey:        "/legacy/client.key",
			Cert:           "/legacy/client.crt",
			CACert:         "/legacy/client-ca.crt",
			CertCommonName: "legacy.example.com",
		}),
		Entry("accepts the canonical section", `[egressip-healthcheck-tls]
client-privkey=/canonical/client.key
client-cert=/canonical/client.crt
client-cacert=/canonical/client-ca.crt
cert-common-name=canonical.example.com
`, nil, EgressIPHealthCheckTLSConfig{
			PrivKey:        "/canonical/client.key",
			Cert:           "/canonical/client.crt",
			CACert:         "/canonical/client-ca.crt",
			CertCommonName: "canonical.example.com",
		}),
		Entry("lets canonical fields override legacy fields independently", `[ovnnorth]
client-privkey=/legacy/client.key
client-cert=/legacy/client.crt
client-cacert=/legacy/client-ca.crt
cert-common-name=legacy.example.com

[egressip-healthcheck-tls]
client-cert=/canonical/client.crt
cert-common-name=canonical.example.com
`, nil, EgressIPHealthCheckTLSConfig{
			PrivKey:        "/legacy/client.key",
			Cert:           "/canonical/client.crt",
			CACert:         "/legacy/client-ca.crt",
			CertCommonName: "canonical.example.com",
		}),
		Entry("lets historical CLI flags override canonical fields independently", `[egressip-healthcheck-tls]
client-privkey=/canonical/client.key
client-cert=/canonical/client.crt
client-cacert=/canonical/client-ca.crt
cert-common-name=canonical.example.com
`, []string{
			"--nb-client-cert=/cli/client.crt",
			"--nb-cert-common-name=cli.example.com",
		}, EgressIPHealthCheckTLSConfig{
			PrivKey:        "/canonical/client.key",
			Cert:           "/cli/client.crt",
			CACert:         "/canonical/client-ca.crt",
			CertCommonName: "cli.example.com",
		}),
	)

	It("overrides config file and defaults with CLI legacy service-cluster-ip-range option", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[default]
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
			gomega.Expect(IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(IPv6Mode).To(gomega.BeFalse())
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[gateway]
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[gateway]
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
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
			gomega.Expect(Metrics.EnablePprof).To(gomega.BeTrue())
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[kubernetes]
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
			gomega.Expect(Metrics.EnablePprof).To(gomega.BeTrue())
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
			"-cluster-manager-v4-transit-subnet=foobar",
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
			"-cluster-manager-v6-transit-subnet=100.89.0.0/16",
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
			"-cluster-manager-v4-transit-subnet=100.89.0.0/16",
			"-cluster-manager-v6-transit-subnet=fd99::/64",
		}
		err := app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ClusterManager.V4TransitSubnet).To(gomega.Equal("100.89.0.0/16"))
		gomega.Expect(ClusterManager.V6TransitSubnet).To(gomega.Equal("fd99::/64"))
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
			gomega.Expect(IPv4Mode).To(gomega.BeFalse())
			gomega.Expect(IPv6Mode).To(gomega.BeTrue())
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
			gomega.Expect(IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(IPv6Mode).To(gomega.BeTrue())
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
			gomega.Expect(IPv4Mode).To(gomega.BeTrue())
			gomega.Expect(IPv6Mode).To(gomega.BeTrue())
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
		err := os.WriteFile(cfgFile.Name(), []byte(`[default]
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

	It("rejects a config with invalid advertised-udn-isolation-mode", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[ovnkubernetesfeature]
advertised-udn-isolation-mode=foo
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError(
				gomega.ContainSubstring("invalid advertised-udn-isolation-mode \"foo\": expect one of strict or loose")),
			)
			return nil
		}
		cliArgs := []string{app.Name, "-config-file=" + cfgFile.Name()}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	It("rejects a config with invalid syntax", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[default]
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

	It("rejects a config with invalid udn allowed services", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[default]
udn-allowed-default-services=namespace/invalid.name,test
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

	It("accepts a config with valid udn allowed services", func() {
		err := os.WriteFile(cfgFile.Name(), []byte(`[default]
udn-allowed-default-services= ns/svc, ns1/svc1
`), 0o644)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		app.Action = func(ctx *cli.Context) error {
			_, err = InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(Default.UDNAllowedDefaultServices).To(gomega.HaveLen(2))
			return nil
		}
		cliArgs := []string{
			app.Name,
			"-config-file=" + cfgFile.Name(),
		}
		err = app.Run(cliArgs)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	Describe("parseClusterDefaultNAD", func() {
		It("parses a namespace/name value", func() {
			nad, err := parseClusterDefaultNAD("custom-namespace/custom-nad")

			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nad.Namespace).To(gomega.Equal("custom-namespace"))
			gomega.Expect(nad.Name).To(gomega.Equal("custom-nad"))
		})

		DescribeTable("rejects malformed values",
			func(value string) {
				nad, err := parseClusterDefaultNAD(value)

				gomega.Expect(err).To(gomega.MatchError(
					fmt.Sprintf(`cluster-default-nad %q must be in the format of "namespace/name"`, value),
				))
				gomega.Expect(nad).To(gomega.BeNil())
			},
			Entry("empty value", ""),
			Entry("missing namespace", "/name"),
			Entry("missing name", "namespace/"),
			Entry("missing separator", "name"),
			Entry("too many separators", "namespace/name/extra"),
			Entry("empty component between separators", "namespace//name"),
			Entry("leading and trailing separators", "/namespace/name/"),
			Entry("only separators", "//"),
		)

		It("rejects an invalid namespace", func() {
			nad, err := parseClusterDefaultNAD("BadNS/default")

			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
				`cluster-default-nad "BadNS/default" has an invalid namespace`,
			)))
			gomega.Expect(nad).To(gomega.BeNil())
		})

		DescribeTable("rejects a name that is not a DNS-1123 subdomain",
			func(value string) {
				nad, err := parseClusterDefaultNAD(value)

				gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(
					fmt.Sprintf(`cluster-default-nad %q has an invalid name`, value),
				)))
				gomega.Expect(nad).To(gomega.BeNil())
			},
			Entry("underscore", "namespace/invalid_name"),
			Entry("uppercase", "namespace/UpperCase"),
			Entry("dot", "namespace/."),
			Entry("dot dot", "namespace/.."),
			Entry("percent", "namespace/a%b"),
			Entry("leading dash", "namespace/-name"),
		)

		It("accepts a dotted name", func() {
			nad, err := parseClusterDefaultNAD("namespace/my.nad")

			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nad.Namespace).To(gomega.Equal("namespace"))
			gomega.Expect(nad.Name).To(gomega.Equal("my.nad"))
		})
	})

	Describe("OvnDBConfig operations", func() {
		It("configures ovn-controller to use the local southbound DB socket", func() {
			fexec := ovntest.NewFakeExec()
			expectedURL := "unix:/var/run/ovn/ovnsb_db.sock"
			fexec.AddFakeCmdsNoOutputNoError([]string{
				"ovs-vsctl --timeout=15 set Open_vSwitch . external_ids:ovn-remote=\"" + expectedURL + "\"",
			})

			cli := &OvnDBConfig{RunDir: "/var/run/ovn/"}
			a, err := buildOvnDBConfig(fexec, false, cli, &OvnDBConfig{RunDir: "/var/run/ovn/"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(a.northbound).To(gomega.BeFalse())
			gomega.Expect(a.GetURL()).To(gomega.Equal(expectedURL))

			err = a.SetOVNRemote()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(fexec.CalledMatchesExpected()).To(gomega.BeTrue(), fexec.ErrorDesc)
		})

		It("rejects configuring ovn-controller with the northbound DB", func() {
			fexec := ovntest.NewFakeExec()
			cli := &OvnDBConfig{RunDir: "/var/run/ovn/"}
			a, err := buildOvnDBConfig(fexec, true, cli, &OvnDBConfig{RunDir: "/var/run/ovn/"})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(a.northbound).To(gomega.BeTrue())
			gomega.Expect(a.GetURL()).To(gomega.Equal("unix:/var/run/ovn/ovnnb_db.sock"))

			err = a.SetOVNRemote()
			gomega.Expect(err).To(gomega.MatchError("cannot configure ovn-controller with the northbound database"))
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

	Describe("OVN Kube Node config", func() {
		nodeConfig := func() OvnKubeNodeConfig {
			return OvnKubeNode
		}

		// NOTE: We test this here as the test that overrides values also sets hybridOverlay to true
		// which yields an invalid configuration.
		It("Overrides value from Config file", func() {
			fileNodeConfig := nodeConfig()
			fileNodeConfig.Mode = types.NodeModeDPU

			cliConfig := config{OvnKubeNode: nodeConfig()}
			file := config{OvnKubeNode: fileNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeDPU))
		})

		It("Overrides value from CLI", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.Mode = types.NodeModeDPUHost
			cliNodeConfig.MgmtPortNetdev = "enp1s0f0v0"
			cliNodeConfig.MgmtPortDPResourceName = "openshift.io/mgmtvf"
			cliNodeConfig.DPUNodeLeaseRenewInterval = 5
			cliNodeConfig.DPUNodeLeaseDuration = 20

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.Mode).To(gomega.Equal(types.NodeModeDPUHost))
			gomega.Expect(OvnKubeNode.MgmtPortNetdev).To(gomega.Equal("enp1s0f0v0"))
			gomega.Expect(OvnKubeNode.MgmtPortDPResourceName).To(gomega.Equal("openshift.io/mgmtvf"))
			gomega.Expect(OvnKubeNode.DPUNodeLeaseRenewInterval).To(gomega.Equal(5))
			gomega.Expect(OvnKubeNode.DPUNodeLeaseDuration).To(gomega.Equal(20))
		})

		It("Overrides routing table ID start from Config file", func() {
			cliConfig := config{OvnKubeNode: nodeConfig()}
			file := config{OvnKubeNode: nodeConfig()}
			file.OvnKubeNode.RoutingTableIDStart = 2000

			err := buildOvnKubeNodeConfig(&cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(2000))
		})

		It("Overrides routing table ID start from CLI", func() {
			cliConfig := config{OvnKubeNode: nodeConfig()}
			cliConfig.OvnKubeNode.RoutingTableIDStart = 2001
			file := config{OvnKubeNode: nodeConfig()}
			file.OvnKubeNode.RoutingTableIDStart = 2000

			err := buildOvnKubeNodeConfig(&cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(2001))
		})

		It("Fails if routing table ID start is below the reserved range", func() {
			cliConfig := config{OvnKubeNode: nodeConfig()}
			cliConfig.OvnKubeNode.RoutingTableIDStart = MinimumRoutingTableIDStart - 1

			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: nodeConfig()})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("routing-table-id-start"))
		})

		It("Succeeds if routing table ID start is at the maximum", func() {
			cliConfig := config{OvnKubeNode: nodeConfig()}
			cliConfig.OvnKubeNode.RoutingTableIDStart = MaximumRoutingTableIDStart

			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: nodeConfig()})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.RoutingTableIDStart).To(gomega.Equal(MaximumRoutingTableIDStart))
		})

		It("Fails if routing table ID start is above the maximum", func() {
			cliConfig := config{OvnKubeNode: nodeConfig()}
			cliConfig.OvnKubeNode.RoutingTableIDStart = MaximumRoutingTableIDStart + 1

			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: nodeConfig()})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("routing-table-id-start"))
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("must be <="))
		})

		It("Fails with unsupported mode", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.Mode = "invalid"

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("unexpected ovnkube-node-mode"))
		})

		It("Fails if hybrid overlay is enabled and ovnkube node mode is not full", func() {
			HybridOverlay.Enabled = true
			cliNodeConfig := nodeConfig()
			cliNodeConfig.Mode = types.NodeModeDPU

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(
				"hybrid overlay is not supported with ovnkube-node mode"))
		})

		It("Fails if DPU node lease renew interval is negative", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.DPUNodeLeaseRenewInterval = -1

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("dpu-node-lease-renew-interval"))
		})

		It("Succeeds if DPU node lease renew interval is zero", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.DPUNodeLeaseRenewInterval = 0
			cliNodeConfig.DPUNodeLeaseDuration = 10

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.DPUNodeLeaseRenewInterval).To(gomega.Equal(0))
			gomega.Expect(OvnKubeNode.DPUNodeLeaseDuration).To(gomega.Equal(10))
		})

		It("Fails if DPU node lease duration is non-positive", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.DPUNodeLeaseDuration = 0

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("dpu-node-lease-duration"))
		})

		It("Fails if DPU node lease duration is less than or equal to renew interval", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.DPUNodeLeaseRenewInterval = 10
			cliNodeConfig.DPUNodeLeaseDuration = 10

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.Or(
				gomega.ContainSubstring("dpu-node-lease-duration"),
				gomega.ContainSubstring("dpu-node-lease-renew-interval"),
			))
		})

		It("Fails if management port is provided and ovnkube node mode is dpu", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.Mode = types.NodeModeDPU
			cliNodeConfig.MgmtPortNetdev = "enp1s0f0v0"

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must not be provided"))
		})

		It("Fails if management port is not provided and ovnkube node mode is dpu-host", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.Mode = types.NodeModeDPUHost

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			err := buildOvnKubeNodeConfig(&cliConfig, &config{OvnKubeNode: OvnKubeNode})
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("ovnkube-node-mgmt-port-netdev or ovnkube-node-mgmt-port-dp-resource-name must be provided"))
		})

		It("Succeeds if management netdev provided in the full mode", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.MgmtPortNetdev = "ens1f0v0"

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			file := config{OvnKubeNode: nodeConfig()}
			err := buildOvnKubeNodeConfig(&cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})

		It("Succeeds if management port device plugin resource name provided in the full mode", func() {
			cliNodeConfig := nodeConfig()
			cliNodeConfig.MgmtPortDPResourceName = "openshift.io/mgmtvf"

			cliConfig := config{OvnKubeNode: cliNodeConfig}
			file := config{OvnKubeNode: nodeConfig()}
			err := buildOvnKubeNodeConfig(&cliConfig, &file)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	Describe("Path configurations", func() {
		It("has correct default OvsPaths values", func() {
			gomega.Expect(OvsPaths.RunDir).To(gomega.Equal("/var/run/openvswitch/"))
		})

		It("has correct default OvnNorth values", func() {
			gomega.Expect(OvnNorth.RunDir).To(gomega.Equal("/var/run/ovn/"))
			gomega.Expect(OvnNorth.DbLocation).To(gomega.Equal("/etc/ovn/ovnnb_db.db"))
			gomega.Expect(OvnNorth.GetURL()).To(gomega.Equal("unix:/var/run/ovn/ovnnb_db.sock"))
		})

		It("has correct default OvnSouth values", func() {
			gomega.Expect(OvnSouth.RunDir).To(gomega.Equal("/var/run/ovn/"))
			gomega.Expect(OvnSouth.DbLocation).To(gomega.Equal("/etc/ovn/ovnsb_db.db"))
		})

		It("overrides path values with CLI flags", func() {
			app.Action = func(ctx *cli.Context) error {
				_, err := InitConfig(ctx, kexec.New(), nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(OvsPaths.RunDir).To(gomega.Equal("/cli/ovs/run/"))
				gomega.Expect(OvnNorth.RunDir).To(gomega.Equal("/cli/nb/run/"))
				gomega.Expect(OvnNorth.DbLocation).To(gomega.Equal("/cli/nb.db"))
				gomega.Expect(OvnSouth.RunDir).To(gomega.Equal("/cli/sb/run/"))
				gomega.Expect(OvnSouth.DbLocation).To(gomega.Equal("/cli/sb.db"))

				return nil
			}
			err := app.Run([]string{
				app.Name,
				"-ovs-run-dir=/cli/ovs/run/",
				"-nb-run-dir=/cli/nb/run/",
				"-nb-db-location=/cli/nb.db",
				"-sb-run-dir=/cli/sb/run/",
				"-sb-db-location=/cli/sb.db",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	Describe("No-Overlay Configuration", func() {
		BeforeEach(func() {
			err := PrepareTestConfig()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Enable route advertisements - required for no-overlay transport
			OVNKubernetesFeature.EnableRouteAdvertisements = true
		})

		It("validates transport option correctly", func() {
			// Test valid default transport (empty string = use OVN default overlay)
			Default.Transport = ""
			err := validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test valid no-overlay transport with required options
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			ManagedBGP.FRRNamespace = "frr-k8s-system"
			err = validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test invalid transport
			Default.Transport = "invalid-transport"
			err = validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("invalid transport"))
		})

		It("requires outbound-snat when transport is no-overlay", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = ""
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			ManagedBGP.FRRNamespace = "frr-k8s-system"
			err := validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("outbound-snat is required"))
		})

		It("validates outbound-snat values", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			ManagedBGP.FRRNamespace = "frr-k8s-system"

			// Test valid enable
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			err := validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test valid disable
			NoOverlay.OutboundSNAT = types.NoOverlaySNATDisabled
			err = validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test invalid value
			NoOverlay.OutboundSNAT = "maybe"
			err = validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("invalid outbound-snat"))
		})

		It("requires routing when transport is no-overlay", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			NoOverlay.Routing = ""
			err := validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("routing is required"))
		})

		It("validates routing values", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled

			// Test valid managed (requires topology)
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			ManagedBGP.FRRNamespace = "frr-k8s-system"
			err := validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test valid unmanaged (topology not required)
			NoOverlay.Routing = NoOverlayRoutingUnmanaged
			ManagedBGP.Topology = ""
			err = validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test invalid value
			NoOverlay.Routing = "automatic"
			err = validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("invalid routing"))
		})

		It("builds no-overlay config from file only", func() {
			fileConfig := config{
				NoOverlay: NoOverlayConfig{
					OutboundSNAT: types.NoOverlaySNATEnabled,
					Routing:      NoOverlayRoutingManaged,
				},
				ManagedBGP: ManagedBGPConfig{
					Topology: ManagedBGPTopologyFullMesh,
				},
			}
			err := buildNoOverlayConfig(&fileConfig)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			err = buildManagedBGPConfig(&fileConfig)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			// Config file values should be applied
			gomega.Expect(NoOverlay.OutboundSNAT).To(gomega.Equal(types.NoOverlaySNATEnabled))
			gomega.Expect(NoOverlay.Routing).To(gomega.Equal(NoOverlayRoutingManaged))
			gomega.Expect(ManagedBGP.Topology).To(gomega.Equal(ManagedBGPTopologyFullMesh))
		})

		It("requires topology when routing is managed", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.Topology = ""
			err := validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("topology is required when routing=managed"))
		})

		It("validates topology values", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			NoOverlay.Routing = NoOverlayRoutingManaged

			// Test valid full-mesh
			ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			ManagedBGP.FRRNamespace = "frr-k8s-system"
			err := validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test invalid value
			ManagedBGP.Topology = "route-reflector"
			err = validateNoOverlayConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("invalid topology"))
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(`must be "full-mesh"`))
		})

		It("does not require topology when routing is unmanaged", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.OutboundSNAT = types.NoOverlaySNATEnabled
			NoOverlay.Routing = NoOverlayRoutingUnmanaged
			ManagedBGP.Topology = ""
			err := validateNoOverlayConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})

	Describe("BGP Configuration", func() {
		BeforeEach(func() {
			err := PrepareTestConfig()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		It("parses BGP config from file with all fields set", func() {
			fileConfig := config{
				ManagedBGP: ManagedBGPConfig{
					Topology:     ManagedBGPTopologyFullMesh,
					ASNumber:     64500,
					FRRNamespace: "custom-frr-namespace",
				},
			}
			err := buildManagedBGPConfig(&fileConfig)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(ManagedBGP.Topology).To(gomega.Equal(ManagedBGPTopologyFullMesh))
			gomega.Expect(ManagedBGP.ASNumber).To(gomega.Equal(uint32(64500)))
			gomega.Expect(ManagedBGP.FRRNamespace).To(gomega.Equal("custom-frr-namespace"))
		})

		It("handles partial BGP config in file", func() {
			fileConfig := config{
				ManagedBGP: savedManagedBGP,
			}
			fileConfig.ManagedBGP.Topology = ManagedBGPTopologyFullMesh
			fileConfig.ManagedBGP.FRRNamespace = "frr-k8s-system"

			err := buildManagedBGPConfig(&fileConfig)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			gomega.Expect(ManagedBGP.Topology).To(gomega.Equal(ManagedBGPTopologyFullMesh))
			// ASNumber should retain default value from init
			gomega.Expect(ManagedBGP.ASNumber).To(gomega.Equal(uint32(64512)))
		})

		It("handles empty BGP config in file", func() {
			fileConfig := config{
				ManagedBGP: savedManagedBGP,
			}
			err := buildManagedBGPConfig(&fileConfig)
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
			// Should retain default values without panicking
			gomega.Expect(ManagedBGP.ASNumber).To(gomega.Equal(uint32(64512))) // default value
			gomega.Expect(ManagedBGP.FRRNamespace).To(gomega.Equal(""))        // no default, set by templates
		})

		It("validates reserved AS number 0", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.ASNumber = 0
			err := validateManagedBGPConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("0 is reserved"))
		})

		It("validates reserved AS number 23456 (AS_TRANS)", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.ASNumber = 23456
			err := validateManagedBGPConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("23456 is reserved"))
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("AS_TRANS"))
		})

		It("validates reserved AS number 65535", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.ASNumber = 65535
			err := validateManagedBGPConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("65535 is reserved"))
		})

		It("validates reserved AS number 4294967295", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged
			ManagedBGP.ASNumber = 4294967295
			err := validateManagedBGPConfig()
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(err.Error()).To(gomega.ContainSubstring("4294967295 is reserved"))
		})

		It("accepts valid AS numbers", func() {
			Default.Transport = types.NetworkTransportNoOverlay
			NoOverlay.Routing = NoOverlayRoutingManaged

			// Test valid 16-bit AS number
			ManagedBGP.ASNumber = 64500
			err := validateManagedBGPConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test default AS number
			ManagedBGP.ASNumber = 64512
			err = validateManagedBGPConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())

			// Test valid 32-bit AS number
			ManagedBGP.ASNumber = 100000
			err = validateManagedBGPConfig()
			gomega.Expect(err).ToNot(gomega.HaveOccurred())
		})
	})
})

var _ = Describe("completeOvnKubeNodeConfig", func() {
	AfterEach(func() {
		OvnKubeNode.KubeletCgroupPath = ""
	})

	DescribeTable("normalizes the kubelet cgroup path",
		func(configured, expected string) {
			OvnKubeNode.KubeletCgroupPath = configured

			gomega.Expect(completeOvnKubeNodeConfig()).To(gomega.Succeed())
			gomega.Expect(OvnKubeNode.KubeletCgroupPath).To(gomega.Equal(expected))
		},
		Entry("unset", "", ""),
		Entry("relative path", "kubelet.slice/kubelet.service", "kubelet.slice/kubelet.service"),
		Entry("leading slash is trimmed", "/podruntime/kubelet", "podruntime/kubelet"),
		// a leftover leading slash would be counted as another level by the nftables
		// match, producing a rule that never matches.
		Entry("repeated leading slashes are trimmed", "//podruntime/kubelet", "podruntime/kubelet"),
		Entry("duplicate separators are cleaned", "podruntime//kubelet", "podruntime/kubelet"),
		Entry("trailing slash is cleaned", "podruntime/kubelet/", "podruntime/kubelet"),
		Entry("single component", "kubelet.slice", "kubelet.slice"),
	)

	DescribeTable("rejects a kubelet cgroup path that cannot be matched safely",
		func(configured, expectedError string) {
			OvnKubeNode.KubeletCgroupPath = configured

			gomega.Expect(completeOvnKubeNodeConfig()).To(gomega.MatchError(gomega.ContainSubstring(expectedError)))
		},
		// the cgroup root holds every host process, so matching on it would let all of
		// them reach primary UDN pods.
		Entry("cgroup root", "/", "must not be the cgroup root"),
		Entry("cgroup root as dot", ".", "must not be the cgroup root"),
		Entry("escapes further down", "kubelet.slice/../..", `must not contain ".."`),
		// cleaning would resolve this to "/etc" and hide the escape, so ".." has to be
		// rejected before the path is cleaned.
		Entry("escape that cleaning would resolve", "../../etc", `must not contain ".."`),
	)
})

var _ = Describe("kubelet cgroup path config", func() {
	var app *cli.App

	BeforeEach(func() {
		gomega.Expect(PrepareTestConfig()).To(gomega.Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = Flags
	})

	It("is read from the config file", func() {
		cfgFile, err := os.CreateTemp("", "config-*.conf")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(cfgFile.Name())
		_, err = cfgFile.WriteString("[ovnkubenode]\nkubelet-cgroup-path=/podruntime/kubelet\n")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(cfgFile.Close()).To(gomega.Succeed())

		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.KubeletCgroupPath).To(gomega.Equal("podruntime/kubelet"))
			return nil
		}
		gomega.Expect(app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})).To(gomega.Succeed())
	})

	It("is read from the command line", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.KubeletCgroupPath).To(gomega.Equal("kubelet.slice/kubelet.service"))
			return nil
		}
		gomega.Expect(app.Run([]string{app.Name, "-kubelet-cgroup-path=kubelet.slice/kubelet.service"})).To(gomega.Succeed())
	})

	It("prefers the command line over the config file", func() {
		cfgFile, err := os.CreateTemp("", "config-*.conf")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		defer os.Remove(cfgFile.Name())
		_, err = cfgFile.WriteString("[ovnkubenode]\nkubelet-cgroup-path=from-file/kubelet\n")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(cfgFile.Close()).To(gomega.Succeed())

		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(OvnKubeNode.KubeletCgroupPath).To(gomega.Equal("from-cli/kubelet"))
			return nil
		}
		gomega.Expect(app.Run([]string{app.Name,
			"-config-file=" + cfgFile.Name(),
			"-kubelet-cgroup-path=from-cli/kubelet",
		})).To(gomega.Succeed())
	})

	It("rejects the cgroup root", func() {
		app.Action = func(ctx *cli.Context) error {
			_, err := InitConfig(ctx, kexec.New(), nil)
			gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("must not be the cgroup root")))
			return nil
		}
		gomega.Expect(app.Run([]string{app.Name, "-kubelet-cgroup-path=/"})).To(gomega.Succeed())
	})
})
