package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultVethMTU = 1400

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetDown(link); err != nil {
		return err
	}
	if err := netlink.LinkSetName(link, newName); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}

	return nil
}

func setupInterface(netns ns.NetNS, containerID, ifName, macAddress, ipAddress, gatewayIP string) (*current.Interface, *current.Interface, error) {
	hostIface := &current.Interface{}
	contIface := &current.Interface{}

	var oldHostVethName string
	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, defaultVethMTU, hostNS)
		if err != nil {
			return err
		}
		hostIface.Mac = hostVeth.HardwareAddr.String()
		contIface.Name = containerVeth.Name

		link, err := netlink.LinkByName(contIface.Name)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", contIface.Name, err)
		}

		hwAddr, err := net.ParseMAC(macAddress)
		if err != nil {
			return fmt.Errorf("failed to parse mac address for %s: %v", contIface.Name, err)
		}
		err = netlink.LinkSetHardwareAddr(link, hwAddr)
		if err != nil {
			return fmt.Errorf("failed to add mac address %s to %s: %v", macAddress, contIface.Name, err)
		}
		contIface.Mac = macAddress
		contIface.Sandbox = netns.Path()

		addr, err := netlink.ParseAddr(ipAddress)
		if err != nil {
			return err
		}
		err = netlink.AddrAdd(link, addr)
		if err != nil {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ipAddress, contIface.Name, err)
		}

		gw := net.ParseIP(gatewayIP)
		if gw == nil {
			return fmt.Errorf("parse ip of gateway failed")
		}
		err = ip.AddRoute(nil, gw, link)
		if err != nil {
			return err
		}

		oldHostVethName = hostVeth.Name

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// rename the host end of veth pair
	hostIface.Name = containerID[:15]
	if err := renameLink(oldHostVethName, hostIface.Name); err != nil {
		return nil, nil, fmt.Errorf("failed to rename %s to %s: %v", oldHostVethName, hostIface.Name, err)
	}

	return hostIface, contIface, nil
}

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

func cmdAdd(args *skel.CmdArgs) error {
	conf := &types.NetConf{}
	if err := json.Unmarshal(args.StdinData, conf); err != nil {
		return fmt.Errorf("failed to load netconf: %v", err)
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

	ovsArgs := []string{
		"--if-exists", "get", "Open_vSwitch",
		".", "external_ids:k8s-api-server",
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to get K8S_API_SERVER")
	}
	k8sAPIServer := strings.Trim(strings.TrimSpace(string(out)), "\"")
	var cfg *rest.Config
	if strings.HasPrefix(k8sAPIServer, "https") {
		// get token and ca cert
		ovsArgs = []string{
			"--if-exists", "get", "Open_vSwitch",
			".", "external_ids:k8s-api-token",
		}
		out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to get K8S_API_TOKEN")
		}
		token := strings.Trim(strings.TrimSpace(string(out)), "\"")
		config.FetchConfig()
		cfg, err = util.CreateConfig(k8sAPIServer, token, config.K8sCACertificate)
	} else if strings.HasPrefix(k8sAPIServer, "http") {
		cfg, err = clientcmd.BuildConfigFromFlags(k8sAPIServer, "")
	} else {
		k8sAPIServer = fmt.Sprintf("http://%s", k8sAPIServer)
		cfg, err = clientcmd.BuildConfigFromFlags(k8sAPIServer, "")
	}
	if err != nil {
		return fmt.Errorf("Failed to get kubeclient: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("Could not create clientset for kubernetes: %v", err)
	}
	kubecli := &kube.Kube{KClient: clientset}

	// Get the IP address and MAC address from the API server.
	// Wait for a maximum of 30 seconds with a retry every 1 second.
	var annotation map[string]string
	for cnt := 0; cnt < 30; cnt++ {
		annotation, err = kubecli.GetAnnotationsOnPod(namespace, podName)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		if _, ok := annotation["ovn"]; ok {
			break
		}
	}

	if annotation == nil {
		return fmt.Errorf("failed to get pod annotation")
	}

	ovnAnnotation, ok := annotation["ovn"]
	if !ok {
		return fmt.Errorf("failed to get annotation of ovn")
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

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	hostIface, contIface, err := setupInterface(netns, args.ContainerID, args.IfName, macAddress, ipAddress, gatewayIP)
	if err != nil {
		return err
	}

	ifaceID := fmt.Sprintf("%s_%s", namespace, podName)

	ovsArgs = []string{
		"add-port", "br-int", hostIface.Name, "--", "set",
		"interface", hostIface.Name,
		fmt.Sprintf("external_ids:attached_mac=%s", macAddress),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:ip_address=%s", ipAddress),
	}
	out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, string(out))
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
		Interfaces: []*current.Interface{hostIface, contIface},
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
	ifaceName := args.ContainerID[:15]
	ovsArgs := []string{
		"del-port", "br-int", ifaceName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no port named") {
		// DEL should be idempotent; don't return an error just log it
		logrus.Warningf("failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
	}

	return nil
}

func main() {
	f, err := os.OpenFile(config.LogPath, os.O_WRONLY|os.O_CREATE, 0755)
	if err == nil {
		defer f.Close()
		logrus.SetOutput(f)
	}
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}
