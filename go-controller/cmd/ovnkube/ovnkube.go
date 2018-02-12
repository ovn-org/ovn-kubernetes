package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	ovncluster "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovnkube"
	c.Usage = "run ovnkube to start master, node, and gateway services"
	c.Version = "0.0.1"
	c.Flags = []cli.Flag{
		// Kubernetes-related options
		cli.StringFlag{
			Name:  "kubeconfig",
			Usage: "absolute path to the kubeconfig file",
		},
		cli.StringFlag{
			Name:  "apiserver",
			Value: "https://localhost:8443",
			Usage: "URL to the Kubernetes apiserver",
		},
		cli.StringFlag{
			Name:  "ca-cert",
			Usage: "CA cert for the Kubernetes api server",
		},
		cli.StringFlag{
			Name:  "token",
			Usage: "Bearer token to use for establishing ovn infrastructure",
		},
		cli.StringFlag{
			Name:  "cluster-subnet",
			Value: "11.11.0.0/16",
			Usage: "Cluster wide IP subnet to use",
		},
		cli.StringFlag{
			Name: "service-cluster-ip-range",
			Usage: "A CIDR notation IP range from which k8s assigns " +
				"service cluster IPs. This should be the same as the one " +
				"provided for kube-apiserver \"-service-cluster-ip-range\" " +
				"option.",
		},

		// OVN northbound database options
		cli.StringFlag{
			Name:  "ovn-north-db",
			Usage: "IP address and port of the OVN northbound API (eg, ssl://1.2.3.4:6641).  Leave empty to use a local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-server-privkey",
			Usage: "Private key that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-server-cert",
			Usage: "Server certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-server-cacert",
			Usage: "CA certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-client-privkey",
			Usage: "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-client-cert",
			Usage: "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-north-client-cacert",
			Usage: "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},

		// OVN southbound database options
		cli.StringFlag{
			Name:  "ovn-south-db",
			Usage: "IP address and port of the OVN southbound API (eg, ssl://1.2.3.4:6642).  Leave empty to use a local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-server-privkey",
			Usage: "Private key that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-server-cert",
			Usage: "Server certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-server-cacert",
			Usage: "CA certificate that the OVN southbound API should use for securing the API.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-client-privkey",
			Usage: "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-client-cert",
			Usage: "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},
		cli.StringFlag{
			Name:  "ovn-south-client-cacert",
			Usage: "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.",
		},

		// Mode flags
		cli.BoolFlag{
			Name:  "net-controller",
			Usage: "Flag to start the central controller that watches pods/services/policies",
		},
		cli.StringFlag{
			Name:  "init-master",
			Usage: "initialize master, requires the hostname as argument",
		},
		cli.StringFlag{
			Name:  "init-node",
			Usage: "initialize node, requires the name that node is registered with in kubernetes cluster",
		},

		// Log flags
		cli.IntFlag{
			Name:  "loglevel",
			Usage: "loglevels 5=debug, 4=info, 3=warn, 2=error, 1=fatal",
			Value: 4,
		},
		cli.StringFlag{
			Name:  "logfile",
			Usage: "logfile name (with path) for ovnkube to write to.",
		},

		// Gateway flags
		cli.BoolFlag{
			Name:  "init-gateways",
			Usage: "initialize a gateway in the minion. Only useful with \"init-node\"",
		},
		cli.StringFlag{
			Name: "gateway-interface",
			Usage: "The interface in minions that will be the gateway interface. " +
				"If none specified, then the node's interface on which the " +
				"default gateway is configured will be used as the gateway " +
				"interface. Only useful with \"init-gateways\"",
		},
		cli.StringFlag{
			Name: "gateway-nexthop",
			Usage: "The external default gateway which is used as a next hop by " +
				"OVN gateway.  This is many times just the default gateway " +
				"of the node in question. If not specified, the default gateway" +
				"configured in the node is used. Only useful with " +
				"\"init-gateways\"",
		},
		cli.BoolFlag{
			Name: "gateway-spare-interface",
			Usage: "If true, assumes that \"gateway-interface\" provided can be " +
				"exclusively used for the OVN gateway.  When true, only OVN" +
				"related traffic can flow through this interface",
		},

		cli.BoolFlag{
			Name:  "nodeport",
			Usage: "Setup nodeport based ingress on gateways.",
		},
	}
	c.Action = func(c *cli.Context) error {
		return runOvnKube(c)
	}

	if err := c.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func runOvnKube(ctx *cli.Context) error {
	// Process log flags
	logrus.SetLevel(logrus.Level(ctx.Int("loglevel")))
	logrus.SetOutput(os.Stderr)
	logFile := ctx.String("logfile")
	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY,
			0660)
		if err != nil {
			logrus.Errorf("failed to open logfile %s (%v). Ignoring..",
				logFile, err)
		} else {
			defer file.Close()
			logrus.SetOutput(file)
		}
	}

	// Process auth flags
	var config *restclient.Config
	var err error

	kubeconfig := ctx.String("kubeconfig")
	server := ctx.String("apiserver")
	rootCAFile := ctx.String("ca-cert")
	token := ctx.String("token")
	if kubeconfig != "" {
		// uses the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else if server != "" && token != "" && ((rootCAFile != "") || !strings.HasPrefix(server, "https")) {
		config, err = util.CreateConfig(server, token, rootCAFile)
	} else {
		err = fmt.Errorf("Provide kubeconfig file or give server/token/tls credentials")
	}
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// create factory and start the controllers asked for
	stopChan := make(chan struct{})
	factory, err := factory.NewWatchFactory(clientset, stopChan)
	if err != nil {
		panic(err.Error)
	}

	netController := ctx.Bool("net-controller")
	master := ctx.String("init-master")
	node := ctx.String("init-node")
	nodePortEnable := ctx.Bool("nodeport")

	if master != "" || node != "" {
		clusterController := ovncluster.NewClusterController(clientset, factory)
		clusterController.KubeServer = server
		clusterController.CACert = rootCAFile
		clusterController.Token = token
		clusterController.HostSubnetLength = 8
		clusterController.GatewayInit = ctx.Bool("init-gateways")
		clusterController.GatewayIntf = ctx.String("gateway-interface")
		clusterController.GatewayNextHop = ctx.String("gateway-nexthop")
		clusterController.GatewaySpareIntf = ctx.Bool("gateway-spare-interface")
		_, clusterController.ClusterIPNet, err = net.ParseCIDR(ctx.String("cluster-subnet"))
		if err != nil {
			panic(err.Error)
		}

		clusterServicesSubnet := ctx.String("service-cluster-ip-range")
		if clusterServicesSubnet != "" {
			var servicesSubnet *net.IPNet
			_, servicesSubnet, err = net.ParseCIDR(
				clusterServicesSubnet)
			if err != nil {
				panic(err.Error)
			}
			clusterController.ClusterServicesSubnet = servicesSubnet.String()
		}

		ovnNorth := ctx.String("ovn-north-db")
		ovnNorthClientPrivKey := ctx.String("ovn-north-client-privkey")
		ovnNorthClientCert := ctx.String("ovn-north-client-cert")
		ovnNorthClientCACert := ctx.String("ovn-north-client-cacert")
		clusterController.NorthDBClientAuth, err = ovncluster.NewOvnDBAuth(ovnNorth, ovnNorthClientPrivKey, ovnNorthClientCert, ovnNorthClientCACert, false)
		if err != nil {
			panic(err.Error())
		}

		ovnSouth := ctx.String("ovn-south-db")
		ovnSouthClientPrivKey := ctx.String("ovn-south-client-privkey")
		ovnSouthClientCert := ctx.String("ovn-south-client-cert")
		ovnSouthClientCACert := ctx.String("ovn-south-client-cacert")
		clusterController.SouthDBClientAuth, err = ovncluster.NewOvnDBAuth(ovnSouth, ovnSouthClientPrivKey, ovnSouthClientCert, ovnSouthClientCACert, false)
		if err != nil {
			panic(err.Error())
		}

		if node != "" {
			if token == "" {
				panic("Cannot initialize node without service account 'token'. Please provide one with --token argument")
			}

			err := clusterController.StartClusterNode(node)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}
		clusterController.NodePortEnable = nodePortEnable

		if master != "" {
			if runtime.GOOS == "windows" {
				panic("Windows is not supported as master node")
			}
			ovnNorthServerPrivKey := ctx.String("ovn-north-server-privkey")
			ovnNorthServerCert := ctx.String("ovn-north-server-cert")
			ovnNorthServerCACert := ctx.String("ovn-north-server-cacert")
			clusterController.NorthDBServerAuth, err = ovncluster.NewOvnDBAuth(ovnNorth, ovnNorthServerPrivKey, ovnNorthServerCert, ovnNorthServerCACert, true)
			if err != nil {
				panic(err.Error())
			}

			ovnSouthServerPrivKey := ctx.String("ovn-south-server-privkey")
			ovnSouthServerCert := ctx.String("ovn-south-server-cert")
			ovnSouthServerCACert := ctx.String("ovn-south-server-cacert")
			clusterController.SouthDBServerAuth, err = ovncluster.NewOvnDBAuth(ovnSouth, ovnSouthServerPrivKey, ovnSouthServerCert, ovnSouthServerCACert, true)
			if err != nil {
				panic(err.Error())
			}

			// run the cluster controller to init the master
			err := clusterController.StartClusterMaster(master)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
		}
	}
	if netController {
		ovnController := ovn.NewOvnController(clientset, factory)
		ovnController.NodePortEnable = nodePortEnable
		ovnController.Run()
	}
	if master != "" || netController {
		// run forever
		select {}
	}

	return nil
}
