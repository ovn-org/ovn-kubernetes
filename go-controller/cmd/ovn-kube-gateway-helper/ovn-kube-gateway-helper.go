package main

import (
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	utilwait "k8s.io/apimachinery/pkg/util/wait"
	informerfactory "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	kapi "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	conntrackZone = 64000

	// TCP is the constant string for the string "TCP"
	TCP = "TCP"

	// UDP is the constant string for the string "UDP"
	UDP = "UDP"

	// ADD is the constant string for adding service event
	ADD = "ADD"

	// DEL is the constant string for deleting service event
	DEL = "DEL"
)

func addConntrackRules(physicalBridge string, brIntOfport, physicalInterfaceOfport int) error {
	// table 0, packets coming from pods headed externally. Commit connections
	// so that reverse direction goes back to the pods
	stdout, _, err := util.RunOVSOfctl(
		"add-flow", physicalBridge,
		fmt.Sprintf("priority=100, in_port=%d, ip, actions=ct(commit, zone=%d), output:%d",
			brIntOfport, conntrackZone, physicalInterfaceOfport))
	if err != nil {
		return fmt.Errorf("table 0, add flow for packets from pods haeded externally failed: %q", stdout)
	}

	// table 0, packets coming from external. Send it through conntrack and
	// resubmit to table 1 to know the state of the connection
	stdout, _, err = util.RunOVSOfctl(
		"add-flow", physicalBridge,
		fmt.Sprintf("priority=50, in_port=%d, ip, actions=ct(zone=%d, table=1)",
			physicalInterfaceOfport, conntrackZone))
	if err != nil {
		return fmt.Errorf("table 0, add flow for packets coming from external failed: %q", stdout)
	}

	// table 1, established and related connections go to pod
	stdout, _, err = util.RunOVSOfctl(
		"add-flow", physicalBridge,
		fmt.Sprintf("priority=100, table=1, ct_state=+trk+est, actions=output:%d", brIntOfport))
	if err != nil {
		return fmt.Errorf("table 1, add flow for established and related connections go to pod failed: %q", stdout)
	}

	// All other connections get the 'NORMAL' treatment
	stdout, _, err = util.RunOVSOfctl(
		"add-flow", physicalBridge,
		"priority=0, table=1, actions=NORMAL")
	if err != nil {
		return fmt.Errorf("table 1, add flow for other connections failed: %q", stdout)
	}

	return nil
}

func syncServices(kubecli *kube.Kube, physicalBridge string, physicalInterfaceOfport int) error {
	existingNamespaces, err := kubecli.GetNamespaces()
	if err != nil {
		return fmt.Errorf("get namespaces failed: %v", err)
	}

	nodePorts := make(map[string]bool)
	for _, namespace := range existingNamespaces.Items {
		var existingServices *kapi.ServiceList

		existingServices, err = kubecli.GetServices(namespace.ObjectMeta.Name)
		if err != nil {
			return fmt.Errorf("get services failed: %v", err)
		}

		for _, service := range existingServices.Items {
			if service.Spec.Type != kapi.ServiceTypeNodePort || len(service.Spec.Ports) == 0 {
				continue
			}

			for _, svcPort := range service.Spec.Ports {
				port := svcPort.NodePort
				if port == 0 {
					continue
				}

				prot := svcPort.Protocol
				if prot != TCP && prot != UDP {
					continue
				}
				protocol := strings.ToLower(string(prot))
				nodePortKey := fmt.Sprintf("%s_%d", protocol, port)
				nodePorts[nodePortKey] = true
			}
		}
	}

	// Possibility that the daemon was restarted. We should find out any additional
	// node_ports which are stale and delete them
	stdout, _, err := util.RunOVSOfctl(
		"dump-flows", physicalBridge,
		fmt.Sprintf("in_port=%d", physicalInterfaceOfport))
	if err != nil {
		return fmt.Errorf("dump flows failed: %q", stdout)
	}
	flows := strings.Split(stdout, "\n")

	re, err := regexp.Compile(`tp_dst=(.*?)[, ]`)
	if err != nil {
		return fmt.Errorf("regexp compile failed: %v", err)
	}

	for _, flow := range flows {
		group := re.FindStringSubmatch(flow)
		if group == nil {
			continue
		}

		var key string
		if strings.Contains(flow, "tcp") {
			key = fmt.Sprintf("tcp_%s", group[1])
		} else if strings.Contains(flow, "udp") {
			key = fmt.Sprintf("udp_%s", group[1])
		} else {
			continue
		}

		if _, ok := nodePorts[key]; !ok {
			pair := strings.Split(key, "_")
			protocol, port := pair[0], pair[1]
			protocolDst := fmt.Sprintf("%s_dst", protocol)

			stdout, _, err := util.RunOVSOfctl(
				"del-flows", physicalBridge,
				fmt.Sprintf("in_port=%d, %s, %s=%s",
					physicalInterfaceOfport, protocol, protocolDst, port))
			if err != nil {
				logrus.Errorf("del flow of %s failed: %q", physicalBridge, stdout)
			}
		}
	}

	return nil
}

func createK8sClientset() (kubernetes.Interface, error) {
	stdout, _, err := util.RunOVSVsctl(
		"--if-exists", "get", "Open_vSwitch",
		".", "external_ids:k8s-api-server")
	if err != nil {
		return nil, fmt.Errorf("failed to get K8S_API_SERVER: %q", stdout)
	}
	k8sAPIServer := strings.Trim(strings.TrimSpace(stdout), "\"")
	if !strings.HasPrefix(k8sAPIServer, "http") {
		k8sAPIServer = fmt.Sprintf("http://%s", k8sAPIServer)
	}

	// TODO: support https
	config, err := clientcmd.BuildConfigFromFlags(k8sAPIServer, "")
	if err != nil {
		return nil, fmt.Errorf("build config from flags failed: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func createPatchPort(physicalBridge string) (int, error) {
	patch1 := fmt.Sprintf("k8s-patch-br-int-%s", physicalBridge)
	patch2 := fmt.Sprintf("k8s-patch-%s-br-int", physicalBridge)
	stdout, _, err := util.RunOVSVsctl(
		"--may-exist", "add-port", "br-int", patch1,
		"--", "set", "interface", patch1, "type=patch",
		fmt.Sprintf("options:peer=%s", patch2))
	if err != nil {
		return 0, fmt.Errorf("set patch port for br-int failed: %q", stdout)
	}

	stdout, _, err = util.RunOVSVsctl(
		"--may-exist", "add-port", physicalBridge, patch2,
		"--", "set", "interface", patch2, "type=patch",
		fmt.Sprintf("options:peer=%s", patch1))
	if err != nil {
		return 0, fmt.Errorf("set patch port for %s failed: %q", physicalBridge, stdout)
	}

	stdout, _, err = util.RunOVSVsctl("get", "interface", patch2, "ofport")
	if err != nil {
		return 0, fmt.Errorf("failed to get ofport of %s: %q", physicalBridge, stdout)
	}
	brIntOfport, err := strconv.Atoi(strings.TrimSuffix(stdout, "\n"))
	if err != nil || brIntOfport <= 0 {
		return 0, fmt.Errorf("patch port creation failed to provide a ofport: %v", err)
	}

	return brIntOfport, nil
}

func handleServiceEvent(obj interface{}, event, physicalBridge string, physicalInterfaceOfport, brIntOfport int) {
	service, ok := obj.(*kapi.Service)
	if !ok {
		logrus.Errorf("couldn't get service from obj")
		return
	}

	if service.Spec.Type != kapi.ServiceTypeNodePort || len(service.Spec.Ports) == 0 {
		return
	}

	for _, svcPort := range service.Spec.Ports {
		port := svcPort.NodePort
		if port == 0 {
			continue
		}

		prot := svcPort.Protocol
		if prot != TCP && prot != UDP {
			continue
		}

		protocol := strings.ToLower(string(prot))
		protocolDst := fmt.Sprintf("%s_dst", protocol)

		switch event {
		case ADD:
			stdout, _, err := util.RunOVSOfctl(
				"add-flow", physicalBridge,
				fmt.Sprintf("priority=100, in_port=%d, %s, %s=%d, actions=%d",
					physicalInterfaceOfport, protocol, protocolDst, port, brIntOfport))
			if err != nil {
				logrus.Errorf("add flow to %s failed: %q", physicalBridge, stdout)
			}

		case DEL:
			stdout, _, err := util.RunOVSOfctl(
				"del-flows", physicalBridge,
				fmt.Sprintf("in_port=%d, %s, %s=%d",
					physicalInterfaceOfport, protocol, protocolDst, port))
			if err != nil {
				logrus.Errorf("del flow of %s failed: %q", physicalBridge, stdout)
			}
		}
	}
}

func watchServices(clientset kubernetes.Interface, physicalBridge string, physicalInterfaceOfport, brIntOfport int) {
	// How long is better ?
	resyncInterval := 3 * time.Second
	IFactory := informerfactory.NewSharedInformerFactory(clientset, resyncInterval)
	serviceInformer := IFactory.Core().V1().Services()

	StartServiceWatch := func(handler cache.ResourceEventHandler) {
		serviceInformer.Informer().AddEventHandler(handler)
		go serviceInformer.Informer().Run(utilwait.NeverStop)
	}

	StartServiceWatch(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			handleServiceEvent(obj, ADD, physicalBridge, physicalInterfaceOfport, brIntOfport)
		},
		UpdateFunc: func(old, new interface{}) {

		},
		DeleteFunc: func(obj interface{}) {
			handleServiceEvent(obj, DEL, physicalBridge, physicalInterfaceOfport, brIntOfport)
		},
	})
}

func main() {
	var physicalBridge, physicalInterface string

	flag.StringVar(&physicalBridge, "physical-bridge", "", "The OVS bridge via which external connectivity is provided")
	flag.StringVar(&physicalInterface, "physical-interface", "", "The physical interface via which external connectivity is provided")

	flag.Parse()

	clientset, err := createK8sClientset()
	if err != nil {
		logrus.Fatalf("create k8s clientset failed: %v", err)
	}

	// Check whether the bridge exists
	stdout, _, err := util.RunOVSVsctl("find", "bridge", fmt.Sprintf("name=%s", physicalBridge))
	if err != nil {
		logrus.Fatalf("physical bridge %s does not exist: %q", physicalBridge, stdout)
	}

	stdout, _, err = util.RunOVSVsctl("get", "interface", physicalInterface, "ofport")
	if err != nil {
		logrus.Fatalf("failed to get ofport of %s: %q", physicalInterface, stdout)
	}
	physicalInterfaceOfport, err := strconv.Atoi(strings.TrimSuffix(stdout, "\n"))
	if err != nil || physicalInterfaceOfport <= 0 {
		logrus.Fatalf("physical interface %s is invalid", physicalInterface)
	}

	// Create a patch port that connects 'physical-bridge' to br-int and vice-versa
	brIntOfport, err := createPatchPort(physicalBridge)
	if err != nil {
		logrus.Fatalf("create patch port failed: %v", err)
	}

	err = addConntrackRules(physicalBridge, brIntOfport, physicalInterfaceOfport)
	if err != nil {
		logrus.Fatalf("add conntrack rule failed: %v", err)
	}

	err = syncServices(&kube.Kube{KClient: clientset}, physicalBridge, physicalInterfaceOfport)
	if err != nil {
		logrus.Fatalf("sync services failed: %v", err)
	}

	watchServices(clientset, physicalBridge, physicalInterfaceOfport, brIntOfport)

	// Run forever
	select {}
}
