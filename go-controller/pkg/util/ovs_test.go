package util

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/urfave/cli"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Util OVS/OVN command tests", func() {
	var (
		app   *cli.App
		fexec *ovntest.FakeExec
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		PrepareTestConfig()

		fexec = ovntest.NewFakeExec()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
	})

	Context("when running ovn-nbctl in daemon mode", func() {
		var tmpDir string

		BeforeEach(func() {
			var err error

			tmpDir, err = ioutil.TempDir("", "ovsutil_test")
			Expect(err).NotTo(HaveOccurred())

			ovsRunDir = filepath.Join(tmpDir, ovsRunDir)
			err = os.MkdirAll(ovsRunDir, 0700)
			Expect(err).NotTo(HaveOccurred())

			ovnRunDir = filepath.Join(tmpDir, ovnRunDir)
			err = os.MkdirAll(ovnRunDir, 0700)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			err := os.Unsetenv("OVN_NB_DAEMON")
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns the right ovn-nbctl daemon args and environment when OVN_NB_DAEMON is set", func() {
			otherDir := filepath.Join(tmpDir, "foo", "bar")
			err := os.MkdirAll(otherDir, 0700)
			Expect(err).NotTo(HaveOccurred())

			nbctlSock := filepath.Join(otherDir, "ovn-nbctl.101.ctl")
			err = ioutil.WriteFile(nbctlSock, []byte(""), 0644)
			Expect(err).NotTo(HaveOccurred())

			err = os.Setenv("OVN_NB_DAEMON", nbctlSock)
			Expect(err).NotTo(HaveOccurred())

			app.Action = func(ctx *cli.Context) error {
				err = SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				args, env := getNbctlArgsAndEnv(10, "foo", "bar")
				Expect(args).To(Equal([]string{"--timeout=10", "foo", "bar"}))
				Expect(env).To(Equal([]string{"OVN_NB_DAEMON=" + nbctlSock}))

				return nil
			}
			err = app.Run([]string{app.Name, "-nbctl-daemon-mode"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns the right ovn-nbctl daemon args and environment when the nbctl files are in /var/run/openvswitch", func() {
			app.Action = func(ctx *cli.Context) error {
				nbctlPidfile := filepath.Join(ovsRunDir, "ovn-nbctl.pid")
				err := ioutil.WriteFile(nbctlPidfile, []byte("101"), 0644)
				Expect(err).NotTo(HaveOccurred())

				nbctlSock := filepath.Join(ovsRunDir, "ovn-nbctl.101.ctl")
				err = ioutil.WriteFile(nbctlSock, []byte(""), 0644)
				Expect(err).NotTo(HaveOccurred())

				err = SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				args, env := getNbctlArgsAndEnv(10, "foo", "bar")
				Expect(args).To(Equal([]string{"--timeout=10", "foo", "bar"}))
				Expect(env).To(Equal([]string{"OVN_NB_DAEMON=" + nbctlSock}))

				return nil
			}
			err := app.Run([]string{app.Name, "-nbctl-daemon-mode"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns the right ovn-nbctl daemon args and environment when the nbctl files are in /var/run/ovn", func() {
			app.Action = func(ctx *cli.Context) error {
				nbctlPidfile := filepath.Join(ovnRunDir, "ovn-nbctl.pid")
				err := ioutil.WriteFile(nbctlPidfile, []byte("101"), 0644)
				Expect(err).NotTo(HaveOccurred())

				nbctlSock := filepath.Join(ovnRunDir, "ovn-nbctl.101.ctl")
				err = ioutil.WriteFile(nbctlSock, []byte(""), 0644)
				Expect(err).NotTo(HaveOccurred())

				err = SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				args, env := getNbctlArgsAndEnv(10, "foo", "bar")
				Expect(args).To(Equal([]string{"--timeout=10", "foo", "bar"}))
				Expect(env).To(Equal([]string{"OVN_NB_DAEMON=" + nbctlSock}))

				return nil
			}
			err := app.Run([]string{app.Name, "-nbctl-daemon-mode"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("falls back to non-daemon mode if pidfile cannot be found", func() {
			app.Action = func(ctx *cli.Context) error {
				err := SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				args, env := getNbctlArgsAndEnv(10, "foo", "bar")
				Expect(args).To(Equal([]string{"--timeout=10", "foo", "bar"}))
				Expect(len(env)).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name, "-nbctl-daemon-mode"})
			Expect(err).NotTo(HaveOccurred())
		})

		It("falls back to non-daemon mode if control socket cannot be found", func() {
			app.Action = func(ctx *cli.Context) error {
				nbctlPidfile := filepath.Join(ovnRunDir, "ovn-nbctl.pid")
				err := ioutil.WriteFile(nbctlPidfile, []byte("101"), 0644)
				Expect(err).NotTo(HaveOccurred())

				err = SetExec(fexec)
				Expect(err).NotTo(HaveOccurred())

				_, err = config.InitConfig(ctx, fexec, nil)
				Expect(err).NotTo(HaveOccurred())

				args, env := getNbctlArgsAndEnv(10, "foo", "bar")
				Expect(args).To(Equal([]string{"--timeout=10", "foo", "bar"}))
				Expect(len(env)).To(Equal(0))

				return nil
			}
			err := app.Run([]string{app.Name, "-nbctl-daemon-mode"})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
