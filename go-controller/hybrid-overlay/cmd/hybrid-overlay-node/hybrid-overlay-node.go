package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/urfave/cli/v2"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/informer"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
)

var nodeName string

const windowServiceArgName = "windows-service"
const appName = "hybrid-overlay-node"

func main() {
	c := cli.NewApp()
	c.Name = appName
	c.Usage = "a node controller to integrate disparate networks with VXLAN tunnels"
	c.Version = config.Version
	c.Flags = config.GetFlags([]cli.Flag{
		&cli.StringFlag{
			Name:        "node",
			Usage:       "The name of this node in the Kubernetes cluster.",
			Destination: &nodeName,
		},
		&cli.BoolFlag{
			Name:  windowServiceArgName,
			Usage: "Enables hybrid overlay to run as a Windows service. Ignored on Linux.",
		}})

	ctx := context.Background()

	c.Action = func(c *cli.Context) error {
		if err := initForOS(c, ctx); err != nil {
			klog.Exit(err)
		}

		if err := runHybridOverlay(c); err != nil {
			klog.Exit(err)
		}
		return nil
	}

	if err := c.RunContext(ctx, os.Args); err != nil {
		klog.Exit(err)
	}
}

func signalHandler(c context.Context) {
	ctx, cancel := context.WithCancel(c)

	// trap SIGHUP, SIGINT, SIGTERM, SIGQUIT and
	// cancel the context
	exitCh := make(chan os.Signal, 1)
	signal.Notify(exitCh,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	defer func() {
		signal.Stop(exitCh)
		cancel()
	}()
	go func() {
		select {
		case s := <-exitCh:
			klog.Infof("Received signal %s. Shutting down", s)
			cancel()
		case <-ctx.Done():
		}
	}()
}

func runHybridOverlay(ctx *cli.Context) error {
	exec := kexec.New()
	if _, err := config.InitConfig(ctx, exec, nil); err != nil {
		return err
	}

	if err := util.SetExecWithoutOVS(exec); err != nil {
		return err
	}

	if nodeName == "" {
		return fmt.Errorf("missing node name; use the 'node' flag to provide one")
	}

	wg := &sync.WaitGroup{}
	clientCfg := config.Kubernetes
	if config.Kubernetes.BootstrapKubeconfig != "" {
		if err := util.StartNodeCertificateManager(ctx.Context, wg, nodeName, &config.Kubernetes); err != nil {
			return fmt.Errorf("failed to start the node certificate manager: %w", err)
		}

		bootstrapConfig, err := clientcmd.BuildConfigFromFlags("", config.Kubernetes.BootstrapKubeconfig)
		if err != nil {
			return err
		}
		// Copy the APIServer and CAData from the bootstrap kubeconfig
		clientCfg.APIServer = bootstrapConfig.Host
		clientCfg.CAData = bootstrapConfig.CAData
		if bootstrapConfig.CAFile != "" {
			bytes, err := os.ReadFile(bootstrapConfig.CAFile)
			if err != nil {
				return err
			}
			clientCfg.CAData = bytes
		}
	}
	clientset, err := util.NewKubernetesClientset(&clientCfg)
	if err != nil {
		return err
	}

	stopChan := make(chan struct{})
	defer close(stopChan)
	f := informers.NewSharedInformerFactory(clientset, informer.DefaultResyncInterval)

	n, err := controller.NewNode(
		&kube.Kube{KClient: clientset},
		nodeName,
		f.Core().V1().Nodes().Informer(),
		f.Core().V1().Pods().Informer(),
		informer.NewDefaultEventHandler,
		true,
	)
	if err != nil {
		return err
	}

	f.Start(stopChan)
	wg.Add(1)
	go func() {
		defer wg.Done()
		n.Run(stopChan)
	}()

	// run until cancelled
	<-ctx.Context.Done()
	close(stopChan)
	wg.Wait()
	return nil
}
