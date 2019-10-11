package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"text/template"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	ovncluster "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

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

	return m
}

// borrowed from cli packages' printHelpCustom()
func printOvnKubeHelp(out io.Writer, templ string, data interface{}, customFunc map[string]interface{}) {
	funcMap := template.FuncMap{
		"join":               strings.Join,
		"upper":              strings.ToUpper,
		"getFlagsByCategory": getFlagsByCategory,
	}
	if customFunc != nil {
		for key, value := range customFunc {
			funcMap[key] = value
		}
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
			logrus.Errorf("pidfile %s exists but can't be read", pidfile)
			return err
		}
		_, err1 := os.Stat("/proc/" + string(pid[:]) + "/cmdline")
		if os.IsNotExist(err1) {
			// Left over pid from dead process
			if err := ioutil.WriteFile(pidfile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
				logrus.Errorf("failed to write pidfile %s (%v). Ignoring..", pidfile, err)
			}
		} else {
			logrus.Errorf("pidfile %s exists and ovnkube is running", pidfile)
			os.Exit(1)
		}
	}

	return nil
}

func runOvnKube(ctx *cli.Context) error {
	pidfile := ctx.String("pidfile")
	if pidfile != "" {
		defer delPidfile(pidfile)
		err := setupPIDFile(pidfile)
		if err != nil {
			return err
		}
	}
	exec := kexec.New()
	_, err := config.InitConfig(ctx, exec, nil)
	if err != nil {
		return err
	}

	if err = util.SetExec(exec); err != nil {
		logrus.Errorf("Failed to initialize exec helper: %v", err)
		return err
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		panic(err.Error())
	}

	// create factory and start the controllers asked for
	stopChan := make(chan struct{})
	factory, err := factory.NewWatchFactory(clientset, stopChan)
	if err != nil {
		panic(err.Error())
	}

	master := ctx.String("init-master")
	node := ctx.String("init-node")

	cleanupNode := ctx.String("cleanup-node")
	if cleanupNode != "" {
		if master != "" || node != "" {
			panic("Cannot specify cleanup-node together with 'init-node or 'init-master'.")
		}

		if err := ovncluster.CleanupClusterNode(cleanupNode); err != nil {
			logrus.Errorf(err.Error())
			panic(err.Error())
		}
		return nil
	}

	// start the prometheus server
	if config.Kubernetes.MetricsBindAddress != "" {
		ovncluster.StartMetricsServer(config.Kubernetes.MetricsBindAddress)
	}

	clusterController := ovncluster.NewClusterController(clientset, factory)
	if master != "" || node != "" {
		if master != "" {
			if runtime.GOOS == "windows" {
				panic("Windows is not supported as master node")
			}
			// run the cluster controller to init the master
			err := clusterController.StartClusterMaster(master)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
			ovnController := ovn.NewOvnController(clientset, factory)
			if err := ovnController.Run(); err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}

		if node != "" {
			if config.Kubernetes.Token == "" {
				panic("Cannot initialize node without service account 'token'. Please provide one with --k8s-token argument")
			}

			err := clusterController.StartClusterNode(node)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}

		// run forever
		select {}
	}

	return fmt.Errorf("need to run ovnkube in either master and/or node mode")
}
