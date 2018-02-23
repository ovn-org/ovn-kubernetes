package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/urfave/cli"

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
			panic("shouldn't get here")
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
		return InitConfig(ctx, nil)
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

var _ = Describe("Config Operations", func() {
	var app *cli.App
	var cfgFile *os.File

	BeforeEach(func() {
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
		app.Action = func(ctx *cli.Context) error {
			err := InitConfig(ctx, nil)
			Expect(err).NotTo(HaveOccurred())

			Expect(Default.MTU).To(Equal(1400))
			Expect(Default.ConntrackZone).To(Equal(64000))
			Expect(Logging.File).To(Equal(""))
			Expect(Logging.Level).To(Equal(4))
			Expect(CNI.ConfDir).To(Equal("/etc/cni/net.d"))
			Expect(CNI.Plugin).To(Equal("ovn-k8s-cni-overlay"))
			Expect(Kubernetes.Kubeconfig).To(Equal(""))
			Expect(Kubernetes.CACert).To(Equal(""))
			Expect(Kubernetes.Token).To(Equal(""))
			Expect(Kubernetes.APIServer).To(Equal("http://localhost:8443"))

			for _, a := range []*OvnDBAuth{OvnNorth.ClientAuth, OvnSouth.ClientAuth} {
				Expect(a.Scheme).To(Equal(OvnDBSchemeUnix))
				Expect(a.URL).To(Equal(""))
				Expect(a.PrivKey).To(Equal(""))
				Expect(a.Cert).To(Equal(""))
				Expect(a.CACert).To(Equal(""))
				Expect(a.server).To(BeFalse())
				Expect(a.host).To(Equal(""))
				Expect(a.port).To(Equal(""))
			}

			for _, a := range []*OvnDBAuth{OvnNorth.ServerAuth, OvnSouth.ServerAuth} {
				Expect(a.Scheme).To(Equal(OvnDBSchemeUnix))
				Expect(a.URL).To(Equal(""))
				Expect(a.PrivKey).To(Equal(""))
				Expect(a.Cert).To(Equal(""))
				Expect(a.CACert).To(Equal(""))
				Expect(a.server).To(BeTrue())
				Expect(a.host).To(Equal(""))
				Expect(a.port).To(Equal(""))
			}
			return nil
		}
		err := app.Run([]string{app.Name, "-config-file=" + cfgFile.Name()})
		Expect(err).NotTo(HaveOccurred())
	})

	// Must use a function factory to ensure that arguments used inside the
	// function (runType and args) are evaluated when the check function is
	// created, not when it is executed
	CheckFn := func(runType int, match string, args ...string) func() {
		return func() {
			err := runInit(app, runType, cfgFile, args...)
			Expect(err).To(MatchError(match))
		}
	}

	// Run once without config file, once with
	Describe("Kubernetes config options", func() {
		Context("returns an error when the", func() {
			for runType := 1; runType <= 3; runType++ {
				It(fmt.Sprintf("(%d) apiserver scheme is HTTPS but no CACert is provided", runType), CheckFn(runType,
					"kubernetes API server \"https://localhost:8443\" scheme requires a CA certificate",
					"-k8s-apiserver=https://localhost:8443"))

				It(fmt.Sprintf("(%d) CA cert does not exist", runType), CheckFn(runType,
					"kubernetes CA certificate file \"/foo/bar/baz.cert\" not found",
					"-k8s-apiserver=https://localhost:8443", "-k8s-cacert=/foo/bar/baz.cert"))

				It(fmt.Sprintf("(%d) apiserter URL scheme is invalid", runType), CheckFn(runType,
					"kubernetes API server URL scheme \"gggggg\" invalid",
					"-k8s-apiserver=gggggg://localhost:8443"))

				It(fmt.Sprintf("(%d) apiserver URL is invalid", runType), CheckFn(runType,
					"kubernetes API server address \"http://a b.com/\" invalid: parse http://a b.com/: invalid character \" \" in host name",
					"-k8s-apiserver=http://a b.com/"))

				It(fmt.Sprintf("(%d) kubeconfig file does not exist", runType), CheckFn(runType,
					"kubernetes kubeconfig file \"/foo/bar/baz\" not found",
					"-k8s-kubeconfig=/foo/bar/baz"))
			}
		})
	})

	Describe("OVN API config options", func() {
		Context("returns an error when the", func() {
			for _, dir := range []string{"nb", "sb"} {
				for runType := 1; runType <= 3; runType++ {
					It(fmt.Sprintf("(%d/%s) scheme is not empty/tcp/ssl", runType, dir), CheckFn(runType,
						"unknown OVN DB scheme \"blah\"",
						"-"+dir+"-address=blah://1.2.3.4:5555"))

					It(fmt.Sprintf("(%d/%s) address is address (unix socket) and certs are given", runType, dir), CheckFn(runType,
						"certificate or key given; perhaps you mean to use the 'ssl' scheme?",
						"-"+dir+"-client-privkey=/bar/baz/foo",
						"-"+dir+"-client-cert=/bar/baz/foo",
						"-"+dir+"-client-cacert=/var/baz/foo"))

					It(fmt.Sprintf("(%d/%s) returns an error when an OVN URL has no port", runType, dir), CheckFn(runType,
						"failed to parse OVN DB host/port \"4.3.2.1\": address 4.3.2.1: missing port in address",
						"-"+dir+"-address=tcp://4.3.2.1"))

					It(fmt.Sprintf("(%d/%s) returns an error when an OVN URL is a hostname", runType, dir), CheckFn(runType,
						"OVN DB host \"foobar.org:4444\" must be an IP address, not a DNS name",
						"-"+dir+"-address=tcp://foobar.org:4444"))

					It(fmt.Sprintf("(%d/%s) returns an error when privkey is not provided for the SSL scheme", runType, dir), CheckFn(runType,
						"must specify private key, certificate, and CA certificate for 'ssl' scheme",
						"-"+dir+"-address=ssl://1.2.3.4:444",
						"-"+dir+"-client-cert=/bar/baz/foo",
						"-"+dir+"-client-cacert=/var/baz/foo"))

					It(fmt.Sprintf("(%d/%s) returns an error when cert is not provided for the SSL scheme", runType, dir), CheckFn(runType,
						"must specify private key, certificate, and CA certificate for 'ssl' scheme",
						"-"+dir+"-address=ssl://1.2.3.4:444",
						"-"+dir+"-client-privkey=/bar/baz/foo",
						"-"+dir+"-client-cacert=/var/baz/foo"))

					It(fmt.Sprintf("(%d/%s) returns an error when cacert is not provided for the SSL scheme", runType, dir), CheckFn(runType,
						"must specify private key, certificate, and CA certificate for 'ssl' scheme",
						"-"+dir+"-address=ssl://1.2.3.4:444",
						"-"+dir+"-client-privkey=/bar/baz/foo",
						"-"+dir+"-client-cert=/bar/baz/foo"))

					It(fmt.Sprintf("(%d/%s) returns an error when certs are provided for the TCP scheme", runType, dir), CheckFn(runType,
						"certificate or key given; perhaps you mean to use the 'ssl' scheme?",
						"-"+dir+"-address=tcp://1.2.3.4:444",
						"-"+dir+"-client-privkey=/bar/baz/foo"))
				}
			}
		})
	})
})
