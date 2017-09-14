package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	kapi "k8s.io/client-go/pkg/api/v1"
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

func setupInterface(netns ns.NetNS, containerID, ifName, macAddress, ipAddress, gatewayIP string) (string, error) {
	var hostIfaceName string

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, containerVeth, err := ip.SetupVeth(ifName, defaultVethMTU, hostNS)
		if err != nil {
			return err
		}

		contIfaceName := containerVeth.Name

		link, err := netlink.LinkByName(contIfaceName)
		if err != nil {
			return fmt.Errorf("failed to lookup %s: %v", contIfaceName, err)
		}

		hwAddr, err := net.ParseMAC(macAddress)
		if err != nil {
			return fmt.Errorf("failed to parse mac address for %s: %v", contIfaceName, err)
		}
		err = netlink.LinkSetHardwareAddr(link, hwAddr)
		if err != nil {
			return fmt.Errorf("failed to add mac address %s to %s: %v", macAddress, contIfaceName, err)
		}

		addr, err := netlink.ParseAddr(ipAddress)
		if err != nil {
			return err
		}
		err = netlink.AddrAdd(link, addr)
		if err != nil {
			return fmt.Errorf("failed to add IP addr %s to %s: %v", ipAddress, contIfaceName, err)
		}

		gw := net.ParseIP(gatewayIP)
		if gw == nil {
			return fmt.Errorf("parse ip of gateway failed")
		}
		err = ip.AddRoute(nil, gw, link)
		if err != nil {
			return err
		}

		hostIfaceName = hostVeth.Name

		return nil
	})
	if err != nil {
		return "", err
	}

	// rename the host end of veth pair
	newHostIfaceName := containerID[:15]
	if err := renameLink(hostIfaceName, newHostIfaceName); err != nil {
		return "", fmt.Errorf("failed to rename %s to %s: %v", hostIfaceName, newHostIfaceName, err)
	}

	return newHostIfaceName, nil
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

// TODO: use a better way to connect to api server
func getAnnotationOnPod(server, namespace, pod string) (map[string]string, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", server, namespace, pod)

	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var apiPod kapi.Pod
	err = json.NewDecoder(res.Body).Decode(&apiPod)
	if err != nil {
		return nil, err
	}

	return apiPod.ObjectMeta.Annotations, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	argsMap, err := argString2Map(args.Args)
	if err != nil {
		return err
	}

	namespace := argsMap["K8S_POD_NAMESPACE"]
	podName := argsMap["K8S_POD_NAME"]
	containerID := argsMap["K8S_POD_INFRA_CONTAINER_ID"]

	if namespace == "" || podName == "" || containerID == "" {
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
	if !strings.HasPrefix(k8sAPIServer, "http") {
		k8sAPIServer = fmt.Sprintf("http://%s", k8sAPIServer)
	}

	// Get the IP address and MAC address from the API server.
	// Wait for a maximum of 3 seconds with a retry every 0.1 second.
	var annotation map[string]string
	for cnt := 0; cnt < 30; cnt++ {
		annotation, err = getAnnotationOnPod(k8sAPIServer, namespace, podName)
		if err != nil {
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

	vethOutside, err := setupInterface(netns, containerID, args.IfName, macAddress, ipAddress, gatewayIP)
	if err != nil {
		return err
	}

	ifaceID := fmt.Sprintf("%s_%s", namespace, podName)

	ovsArgs = []string{
		"add-port", "br-int", vethOutside, "--", "set",
		"interface", vethOutside,
		fmt.Sprintf("external_ids:attached_mac=%s", macAddress),
		fmt.Sprintf("external_ids:iface-id=%s", ifaceID),
		fmt.Sprintf("external_ids:ip_address=%s", ipAddress),
	}
	out, err = exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failure in plugging pod interface: %v\n  %q", err, string(out))
	}

	// TODO: conform with cni specification
	result := fmt.Sprintf("{\"ip_address\":\"%s\", \"mac_address\":\"%s\", \"gateway_ip\": \"%s\"}", ipAddress, macAddress, gatewayIP)
	_, err = os.Stdout.Write([]byte(result))

	return err
}

func cmdDel(args *skel.CmdArgs) error {
	argsMap, err := argString2Map(args.Args)
	if err != nil {
		return err
	}
	containerID := argsMap["K8S_POD_INFRA_CONTAINER_ID"]
	if containerID == "" {
		return fmt.Errorf("required CNI variable missing")
	}

	ifaceName := containerID[:15]
	ovsArgs := []string{
		"del-port", ifaceName,
	}
	out, err := exec.Command("ovs-vsctl", ovsArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete OVS port %s: %v\n  %q", ifaceName, err, string(out))
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.All)
}
