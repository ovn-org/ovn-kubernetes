package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"text/template"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"gopkg.in/fsnotify/fsnotify.v1"

	hocontroller "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	ovncluster "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kexec "k8s.io/utils/exec"
)

const (
	// CustomAppHelpTemplate helps in grouping options to ovnkube
	CustomAppHelpTemplate = `NAME:
   {{.Name}} - {{.Usage}}

USAGE:
   {{.HelpName}} [global options]

VERSION:
   {{.Version}}{{if .Description}}

DESCRIPTION:
   {{.Description}}{{end}}

COMMANDS:{{range .VisibleCategories}}{{if .Name}}

   {{.Name}}:{{end}}{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}

GLOBAL OPTIONS:{{range $title, $category := getFlagsByCategory}}
   {{upper $title}}
   {{range $index, $option := $category}}{{if $index}}
   {{end}}{{$option}}{{end}}
   {{end}}`
)

func getFlagsByCategory() map[string][]cli.Flag {
	m := map[string][]cli.Flag{}
	m["Generic Options"] = config.CommonFlags
	m["CNI Options"] = config.CNIFlags
	m["K8s-related Options"] = config.K8sFlags
	m["OVN Northbound DB Options"] = config.OvnNBFlags
	m["OVN Southbound DB Options"] = config.OvnSBFlags
	m["OVN Gateway Options"] = config.OVNGatewayFlags
	m["Master HA Options"] = config.MasterHAFlags

	return m
}

// borrowed from cli packages' printHelpCustom()
func printOvnKubeHelp(out io.Writer, templ string, data interface{}, customFunc map[string]interface{}) {
	funcMap := template.FuncMap{
		"join":               strings.Join,
		"upper":              strings.ToUpper,
		"getFlagsByCategory": getFlagsByCategory,
	}
	for key, value := range customFunc {
		funcMap[key] = value
	}

	w := tabwriter.NewWriter(out, 1, 8, 2, ' ', 0)
	t := template.Must(template.New("help").Funcs(funcMap).Parse(templ))
	err := t.Execute(w, data)
	if err == nil {
		_ = w.Flush()
	}
}

func main() {
	cli.HelpPrinterCustom = printOvnKubeHelp
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = config.Version
	c.CustomAppHelpTemplate = CustomAppHelpTemplate
	c.Flags = config.CommonFlags
	c.Flags = append(c.Flags, config.CNIFlags...)
	c.Flags = append(c.Flags, config.K8sFlags...)
	c.Flags = append(c.Flags, config.OvnNBFlags...)
	c.Flags = append(c.Flags, config.OvnSBFlags...)
	c.Flags = append(c.Flags, config.OVNGatewayFlags...)
	c.Flags = append(c.Flags, config.MasterHAFlags...)
	c.Flags = append(c.Flags, hocontroller.GetHybridOverlayCLIFlags([]cli.Flag{
		cli.BoolFlag{
			Name:  "enable-hybrid-overlay",
			Usage: "Enables hybrid overlay operation (requires --init-master and/or --init-node)",
		}})...)

	c.Action = func(c *cli.Context) error {
		return runOvnKube(c)
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func delPidfile(pidfile string) {
	if pidfile != "" {
		if _, err := os.Stat(pidfile); err == nil {
			if err := os.Remove(pidfile); err != nil {
				logrus.Errorf("%s delete failed: %v", pidfile, err)
			}
		}
	}
}

func setupPIDFile(pidfile string) error {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		delPidfile(pidfile)
		os.Exit(1)
	}()

	// need to test if already there
	_, err := os.Stat(pidfile)

	// Create if it doesn't exist, else exit with error
	if os.IsNotExist(err) {
		if err := ioutil.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
			logrus.Errorf("failed to write pidfile %s (%v). Ignoring..", pidfile, err)
		}
	} else {
		// get the pid and see if it exists
		pid, err := ioutil.ReadFile(pidfile)
		if err != nil {
			return fmt.Errorf("pidfile %s exists but can't be read: %v", pidfile, err)
		}
		_, err1 := os.Stat("/proc/" + string(pid[:]) + "/cmdline")
		if os.IsNotExist(err1) {
			// Left over pid from dead process
			if err := ioutil.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
				logrus.Errorf("failed to write pidfile %s (%v). Ignoring..", pidfile, err)
			}
		} else {
			return fmt.Errorf("pidfile %s exists and ovnkube is running", pidfile)
		}
	}

	return nil
}

func runOvnKube(ctx *cli.Context) error {
	pidfile := ctx.String("pidfile")
	if pidfile != "" {
		defer delPidfile(pidfile)
		if err := setupPIDFile(pidfile); err != nil {
			return err
		}
	}

	exec := kexec.New()
	configFile, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}

	if err = util.SetExec(exec); err != nil {
		return fmt.Errorf("failed to initialize exec helper: %v", err)
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		return err
	}

	// create factory and start the controllers asked for
	stopChan := make(chan struct{})
	factory, err := factory.NewWatchFactory(clientset, stopChan)
	if err != nil {
		return err
	}

	master := ctx.String("init-master")
	node := ctx.String("init-node")

	cleanupNode := ctx.String("cleanup-node")
	if cleanupNode != "" {
		if master != "" || node != "" {
			return fmt.Errorf("cannot specify cleanup-node together with 'init-node or 'init-master'")
		}

		if err = ovncluster.CleanupClusterNode(cleanupNode); err != nil {
			return err
		}
		return nil
	}

	if master == "" && node == "" {
		return fmt.Errorf("need to run ovnkube in either master and/or node mode")
	}

	// start the prometheus server
	if config.Kubernetes.MetricsBindAddress != "" {
		util.StartMetricsServer(config.Kubernetes.MetricsBindAddress, config.Kubernetes.MetricsEnablePprof)
	}

	// Set up a watch on our config file; if it changes, we exit -
	// (we don't have the ability to dynamically reload config changes).
	if err := watchForChanges(configFile); err != nil {
		return fmt.Errorf("unable to setup configuration watch: %v", err)
	}

	enableHybridOverlay := ctx.Bool("enable-hybrid-overlay")
	if enableHybridOverlay {
		// Since the third address of every cluster subnet is reserved for
		// the hybrid overlay, only allow enabling it for HostSubnets that
		// are a /24 or larger.
		for _, clusterEntry := range config.Default.ClusterSubnets {
			if clusterEntry.HostSubnetLength > 24 {
				return fmt.Errorf("hybrid overlay cannot be used with" +
					" host subnet prefixes smaller than /24.")
			}
		}
	}

	if master != "" {
		if runtime.GOOS == "windows" {
			return fmt.Errorf("Windows is not supported as master node")
		}

		ovn.RegisterMetrics()

		var nodeSelector *metav1.LabelSelector
		var hybridOverlayClusterSubnets []config.CIDRNetworkEntry
		if enableHybridOverlay {
			hybridOverlayClusterSubnets, err = hocontroller.GetHybridOverlayClusterSubnets(ctx)
			if err != nil {
				return err
			}

			nodeSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{v1.LabelOSStable: "linux"},
			}
		}

		// run the HA master controller to init the master
		ovnHAController := ovn.NewHAMasterController(clientset, factory, master, stopChan,
			hybridOverlayClusterSubnets, nodeSelector)
		if err := ovnHAController.StartHAMasterController(); err != nil {
			return err
		}
	}

	if node != "" {
		if config.Kubernetes.Token == "" {
			return fmt.Errorf("cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
		}

		clusterController := ovncluster.NewClusterController(clientset, factory)
		if err := clusterController.StartClusterNode(node); err != nil {
			return err
		}
	}

	if enableHybridOverlay {
		if err := hocontroller.StartHybridOverlay(ctx, master != "", node, clientset, factory); err != nil {
			return err
		}
	}

	// run forever
	select {}
}

// watchForChanges exits if the configuration file changed.
func watchForChanges(configPath string) error {
	if configPath == "" {
		return nil
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Write == fsnotify.Write {
					logrus.Infof("Configuration file %s changed, exiting...", event.Name)
					os.Exit(0)
					return
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logrus.Errorf("fsnotify error %v", err)
			}
		}
	}()

	// Watch all symlinks for changes
	p := configPath
	maxdepth := 100
	for depth := 0; depth < maxdepth; depth++ {
		if err := watcher.Add(p); err != nil {
			return err
		}
		logrus.Infof("Watching config file %s for changes", p)

		stat, err := os.Lstat(p)
		if err != nil {
			return err
		}

		// configmaps are usually symlinks
		if stat.Mode()&os.ModeSymlink > 0 {
			p, err = filepath.EvalSymlinks(p)
			if err != nil {
				return err
			}
		} else {
			break
		}
	}

	return nil
}
