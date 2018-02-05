package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/Sirupsen/logrus"

	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	ovncluster "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
	ovnfactory "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	util "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

func main() {
	// auth flags
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	server := flag.String("apiserver", "https://localhost:8443", "URL to the Kubernetes apiserver")
	rootCAFile := flag.String("ca-cert", "", "CA cert for the Kubernetes api server")
	token := flag.String("token", "", "Bearer token to use for establishing ovn infrastructure")
	clusterSubnet := flag.String("cluster-subnet", "11.11.0.0/16", "Cluster wide IP subnet to use")
	clusterServicesSubnet := flag.String("service-cluster-ip-range", "",
		"A CIDR notation IP range from which k8s assigns service cluster "+
			"IPs. This should be the same as the one provided for "+
			"kube-apiserver \"-service-cluster-ip-range\" option.")

	// IP address and port of the northbound API server
	ovnNorth := flag.String("ovn-north-db", "", "IP address and port of the OVN northbound API (eg, ssl://1.2.3.4:6641).  Leave empty to use a local unix socket.")

	// SSL-related flags for securing the northbound API server
	ovnNorthServerPrivKey := flag.String("ovn-north-server-privkey", "", "Private key that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.")
	ovnNorthServerCert := flag.String("ovn-north-server-cert", "", "Server certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.")
	ovnNorthServerCACert := flag.String("ovn-north-server-cacert", "", "CA certificate that the OVN northbound API should use for securing the API.  Leave empty to use local unix socket.")

	// SSL-related flags for clients connecting to the northbound API
	ovnNorthClientPrivKey := flag.String("ovn-north-client-privkey", "", "Private key that the client should use for talking to the OVN database.  Leave empty to use local unix socket.")
	ovnNorthClientCert := flag.String("ovn-north-client-cert", "", "Client certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.")
	ovnNorthClientCACert := flag.String("ovn-north-client-cacert", "", "CA certificate that the client should use for talking to the OVN database.  Leave empty to use local unix socket.")

	// IP address and port of the southbound database server
	ovnSouth := flag.String("ovn-south-db", "", "IP address and port of the OVN southbound database (eg, ssl://1.2.3.4:6642).")

	// SSL-related flags for securing the southbound database server
	ovnSouthServerPrivKey := flag.String("ovn-south-server-privkey", "", "Private key that the OVN southbound database should use for securing the API.")
	ovnSouthServerCert := flag.String("ovn-south-server-cert", "", "Server certificate that the OVN southbound database should use for securing the API.")
	ovnSouthServerCACert := flag.String("ovn-south-server-cacert", "", "CA certificate that the OVN southbound database should use for securing the API.")

	// SSL-related flags for clients connecting to the southbound database
	ovnSouthClientPrivKey := flag.String("ovn-south-client-privkey", "", "Private key that the client should use for talking to the OVN database.")
	ovnSouthClientCert := flag.String("ovn-south-client-cert", "", "Client certificate that the client should use for talking to the OVN database.")
	ovnSouthClientCACert := flag.String("ovn-south-client-cacert", "", "CA certificate that the client should use for talking to the OVN database.")

	// mode flags
	netController := flag.Bool("net-controller", false, "Flag to start the central controller that watches pods/services/policies")
	master := flag.String("init-master", "", "initialize master, requires the hostname as argument")
	node := flag.String("init-node", "", "initialize node, requires the name that node is registered with in kubernetes cluster")

	// log flags
	verbose := flag.Int("loglevel", 4,
		"loglevels 5=debug, 4=info, 3=warn, 2=error, 1=fatal")
	logFile := flag.String("logfile", "",
		"logfile name (with path) for ovnkube to write to.")

	// gateway flags
	gatewayInit := flag.Bool("init-gateways", false,
		"initialize a gateway in the minion. Only useful with \"init-node\"")
	gatewayIntf := flag.String("gateway-interface", "",
		"The interface in minions that will be the gateway interface. "+
			"If none specified, then the node's interface on which the "+
			"default gateway is configured will be used as the gateway "+
			"interface. Only useful with \"init-gateways\"")
	gatewayNextHop := flag.String("gateway-nexthop", "",
		"The external default gateway which is used as a next hop by "+
			"OVN gateway.  This is many times just the default gateway "+
			"of the node in question. If not specified, the default gateway"+
			"configured in the node is used. Only useful with "+
			"\"init-gateways\"")
	gatewaySpareIntf := flag.Bool("gateway-spare-interface", false,
		"If true, assumes that \"gateway-interface\" provided can be "+
			"exclusively used for the OVN gateway.  When true, only OVN"+
			"related traffic can flow through this interface")

	// Enable nodeport
	nodePortEnable := flag.Bool("nodeport", false,
		"Setup nodeport based ingress on gateways.")

	flag.Parse()

	// Process log flags
	logrus.SetLevel(logrus.Level(*verbose))
	logrus.SetOutput(os.Stderr)
	if *logFile != "" {
		file, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY,
			0660)
		if err != nil {
			logrus.Errorf("failed to open logfile %s (%v). Ignoring..",
				*logFile, err)
		} else {
			defer file.Close()
			logrus.SetOutput(file)
		}
	}

	// Process auth flags
	var config *restclient.Config
	var err error
	if *kubeconfig != "" {
		// uses the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else if *server != "" && *token != "" && ((*rootCAFile != "") || !strings.HasPrefix(*server, "https")) {
		config, err = util.CreateConfig(*server, *token, *rootCAFile)
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
	factory := ovnfactory.NewDefaultFactory(clientset)
	clusterController := factory.CreateClusterController()

	if *master != "" || *node != "" {
		clusterController.KubeServer = *server
		clusterController.CACert = *rootCAFile
		clusterController.Token = *token
		clusterController.HostSubnetLength = 8
		clusterController.GatewayInit = *gatewayInit
		clusterController.GatewayIntf = *gatewayIntf
		clusterController.GatewayNextHop = *gatewayNextHop
		clusterController.GatewaySpareIntf = *gatewaySpareIntf
		_, clusterController.ClusterIPNet, err = net.ParseCIDR(*clusterSubnet)
		if err != nil {
			panic(err.Error)
		}

		if *clusterServicesSubnet != "" {
			var servicesSubnet *net.IPNet
			_, servicesSubnet, err = net.ParseCIDR(
				*clusterServicesSubnet)
			if err != nil {
				panic(err.Error)
			}
			clusterController.ClusterServicesSubnet = servicesSubnet.String()
		}

		clusterController.NorthDBClientAuth, err = ovncluster.NewOvnDBAuth(*ovnNorth, *ovnNorthClientPrivKey, *ovnNorthClientCert, *ovnNorthClientCACert, false)
		if err != nil {
			panic(err.Error())
		}
		clusterController.SouthDBClientAuth, err = ovncluster.NewOvnDBAuth(*ovnSouth, *ovnSouthClientPrivKey, *ovnSouthClientCert, *ovnSouthClientCACert, false)
		if err != nil {
			panic(err.Error())
		}
	}

	if *master != "" {
		clusterController.NorthDBServerAuth, err = ovncluster.NewOvnDBAuth(*ovnNorth, *ovnNorthServerPrivKey, *ovnNorthServerCert, *ovnNorthServerCACert, true)
		if err != nil {
			panic(err.Error())
		}
		clusterController.SouthDBServerAuth, err = ovncluster.NewOvnDBAuth(*ovnSouth, *ovnSouthServerPrivKey, *ovnSouthServerCert, *ovnSouthServerCACert, true)
		if err != nil {
			panic(err.Error())
		}
	}

	ovnController := factory.CreateOvnController()

	if *node != "" {
		if *token == "" {
			panic("Cannot initialize node without service account 'token'. Please provide one with --token argument")
		}
		clusterController.NodePortEnable = *nodePortEnable

		err := clusterController.StartClusterNode(*node)
		if err != nil {
			logrus.Errorf(err.Error())
			panic(err.Error())
		}
	}
	if *master != "" {
		// run the cluster controller to init the master
		err := clusterController.StartClusterMaster(*master)
		if err != nil {
			logrus.Errorf(err.Error())
			panic(err.Error())
		}
	}
	if *netController {
		ovnController.NodePortEnable = *nodePortEnable
		ovnController.Run()
	}
	if *master != "" || *netController {
		// run forever
		select {}
	}
}
