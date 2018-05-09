package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openvswitch/ovn-kubernetes/go-controller/cmd/ovn-k8s-cni-overlay/app"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

func argString2Map(args string) (map[string]string, error) {
	argsMap := make(map[string]string)

	pairs := strings.Split(args, ";")
	for _, pair := range pairs {
		kv := strings.Split(pair, "=")
		if len(kv) != 2 {
			return nil, fmt.Errorf("ARGS: invalid pair %q", pair)
		}
		keyString := kv[0]
		valueString := kv[1]
		argsMap[keyString] = valueString
	}

	return argsMap, nil
}

var minRsrc = resource.MustParse("1k")
var maxRsrc = resource.MustParse("1P")

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidthResources(podAnnotations map[string]string) (int64, int64, error) {
	ingress := int64(-1)
	egress := int64(-1)
	str, found := podAnnotations["kubernetes.io/ingress-bandwidth"]
	if found {
		ingressVal, err := resource.ParseQuantity(str)
		if err != nil {
			return -1, -1, err
		}
		if err := validateBandwidthIsReasonable(&ingressVal); err != nil {
			return -1, -1, err
		}
		ingress = ingressVal.Value()
	}
	str, found = podAnnotations["kubernetes.io/egress-bandwidth"]
	if found {
		egressVal, err := resource.ParseQuantity(str)
		if err != nil {
			return -1, -1, err
		}
		if err := validateBandwidthIsReasonable(&egressVal); err != nil {
			return -1, -1, err
		}
		egress = egressVal.Value()
	}
	return ingress, egress, nil
}

func initConfig(ctx *cli.Context, args *skel.CmdArgs) (*config.OVNNetConf, error) {
	conf, err := config.ReadCNIConfig(args.StdinData)
	if err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	if _, err := config.InitConfigWithPath(ctx, conf.ConfigFilePath, &config.Defaults{
		K8sAPIServer: true,
		K8sToken:     true,
		K8sCert:      true,
	}); err != nil {
		return nil, err
	}

	return conf, nil
}

func cmdAdd(ctx *cli.Context, args *skel.CmdArgs) error {
	conf, err := initConfig(ctx, args)
	if err != nil {
		return err
	}

	argsMap, err := argString2Map(args.Args)
	if err != nil {
		return err
	}

	namespace := argsMap["K8S_POD_NAMESPACE"]
	podName := argsMap["K8S_POD_NAME"]
	if namespace == "" || podName == "" {
		return fmt.Errorf("required CNI variable missing")
	}

	clientset, err := util.NewClientset(&config.Kubernetes)
	if err != nil {
		return fmt.Errorf("Could not create clientset for kubernetes: %v", err)
	}
	kubecli := &kube.Kube{KClient: clientset}

	// Get the IP address and MAC address from the API server.
	// Exponential back off ~32 seconds + 7* t(api call)
	var annotationBackoff = wait.Backoff{Duration: 1 * time.Second, Steps: 7, Factor: 1.5, Jitter: 0.1}
	var annotation map[string]string
	if err = wait.ExponentialBackoff(annotationBackoff, func() (bool, error) {
		annotation, err = kubecli.GetAnnotationsOnPod(namespace, podName)
		if err != nil {
			// TODO: check if err is non recoverable
			logrus.Warningf("Error while obtaining pod annotations - %v", err)
			return false, nil
		}
		if _, ok := annotation["ovn"]; ok {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("failed to get pod annotation - %v", err)
	}

	ovnAnnotation, ok := annotation["ovn"]
	if !ok {
		return fmt.Errorf("failed to get ovn annotation from pod")
	}

	var ovnAnnotatedMap map[string]string
	err = json.Unmarshal([]byte(ovnAnnotation), &ovnAnnotatedMap)
	if err != nil {
		return fmt.Errorf("unmarshal ovn annotation failed")
	}

	ipAddress := ovnAnnotatedMap["ip_address"]
	macAddress := ovnAnnotatedMap["mac_address"]
	gatewayIP := ovnAnnotatedMap["gateway_ip"]

	if ipAddress == "" || macAddress == "" || gatewayIP == "" {
		return fmt.Errorf("failed in pod annotation key extract")
	}

	ingress, egress, err := extractPodBandwidthResources(annotation)
	if err != nil {
		return fmt.Errorf("failed to parse bandwidth request: %v", err)
	}

	var interfacesArray []*current.Interface
	interfacesArray, err = app.ConfigureInterface(args, namespace, conf,
		podName, macAddress, ipAddress, gatewayIP, config.Default.MTU,
		ingress, egress)
	if err != nil {
		return err
	}

	// Build the result structure to pass back to the runtime
	addr, addrNet, err := net.ParseCIDR(ipAddress)
	if err != nil {
		return fmt.Errorf("failed to parse IP address %q: %v", ipAddress, err)
	}
	ipVersion := "6"
	if addr.To4() != nil {
		ipVersion = "4"
	}
	result := &current.Result{
		Interfaces: interfacesArray,
		IPs: []*current.IPConfig{
			{
				Version:   ipVersion,
				Interface: current.Int(1),
				Address:   net.IPNet{IP: addr, Mask: addrNet.Mask},
				Gateway:   net.ParseIP(gatewayIP),
			},
		},
	}

	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	// argsMap is used only on Windows. Ignore the error, DEL should be idempotent
	argsMap, _ := argString2Map(args.Args)

	return app.PlatformSpecificCleanup(args, argsMap)
}

func main() {
	c := cli.NewApp()
	c.Name = "ovn-k8s-cni-overlay"
	c.Usage = "a CNI plugin to set up or tear down a container's network with OVN"
	c.Version = "0.0.2"
	c.Flags = config.Flags
	c.Action = func(ctx *cli.Context) error {
		skel.PluginMain(
			func(args *skel.CmdArgs) error {
				return cmdAdd(ctx, args)
			},
			cmdDel,
			version.All)
		return nil
	}

	if err := c.Run(os.Args); err != nil {
		// Print the error to stdout in conformance with the CNI spec
		e, ok := err.(*types.Error)
		if !ok {
			e = &types.Error{Code: 100, Msg: err.Error()}
		}
		e.Print()
	}
}
