package main

import (
	"flag"
	"fmt"
	"net"
	"strings"

	"github.com/Sirupsen/logrus"

	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	certutil "k8s.io/client-go/util/cert"

	ovnfactory "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/factory"
	ovncluster "github.com/openvswitch/ovn-kubernetes/go-controller/pkg/cluster"
)

func main() {
	// auth flags
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	server := flag.String("apiserver", "https://localhost:8443", "URL to the Kubernetes apiserver")
	rootCAFile := flag.String("ca-cert", "", "CA cert for the Kubernetes api server")
	token := flag.String("token", "", "Bearer token to use for establishing ovn infrastructure")
	clusterSubnet := flag.String("cluster-subnet", "11.11.0.0/16", "Cluster wide IP subnet to use")

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

	logrus.SetLevel(logrus.DebugLevel)

	flag.Parse()

	// Process auth flags
	var config *restclient.Config
	var err error
	if *kubeconfig != "" {
		// uses the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else if *server != "" && *token != "" && ((*rootCAFile != "") || !strings.HasPrefix(*server, "https")) {
		config, err = CreateConfig(*server, *token, *rootCAFile)
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
		_, clusterController.ClusterIPNet, err = net.ParseCIDR(*clusterSubnet)
		if err != nil {
			panic(err.Error)
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

		err := clusterController.StartClusterNode(*node)
		if err != nil {
			panic(err.Error)
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
		ovnController.Run()
	}
	if *master != "" || *netController {
		// run forever
		select {}
	}
}

// CreateConfig creates the restclient.Config object from token, ca file and the apiserver
// (similar to what is obtainable from kubeconfig file)
func CreateConfig(server, token, rootCAFile string) (*restclient.Config, error) {
	tlsClientConfig := restclient.TLSClientConfig{}
	if rootCAFile != "" {
		if _, err := certutil.NewPool(rootCAFile); err != nil {
			return nil, err
		}
		tlsClientConfig.CAFile = rootCAFile
	}

	return &restclient.Config{
		Host:            server,
		BearerToken:     token,
		TLSClientConfig: tlsClientConfig,
	}, nil
}
