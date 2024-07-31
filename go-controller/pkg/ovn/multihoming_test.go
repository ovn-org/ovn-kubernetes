package ovn

import (
	"net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func (p testPod) addNetwork(
	netName, nadName, nodeSubnet, nodeMgtIP, nodeGWIP, podIP, podMAC, role string,
	tunnelID int,
	routes []util.PodRoute,
) {
	podInfo, ok := p.secondaryPodInfos[netName]
	if !ok {
		podInfo = &secondaryPodInfo{
			nodeSubnet:  nodeSubnet,
			nodeMgtIP:   nodeMgtIP,
			nodeGWIP:    nodeGWIP,
			role:        role,
			routes:      routes,
			allportInfo: map[string]portInfo{},
		}
		p.secondaryPodInfos[netName] = podInfo
	}

	prefixLen, ip := splitPodIPMaskLength(podIP)

	portName := util.GetSecondaryNetworkLogicalPortName(p.namespace, p.podName, nadName)
	podInfo.allportInfo[nadName] = portInfo{
		portUUID:  portName + "-UUID",
		podIP:     ip,
		podMAC:    podMAC,
		portName:  portName,
		tunnelID:  tunnelID,
		prefixLen: prefixLen,
	}
}

func (p testPod) getNetworkPortInfo(netName, nadName string) *portInfo {
	podInfo, ok := p.secondaryPodInfos[netName]
	if !ok {
		return nil
	}
	info, ok := podInfo.allportInfo[nadName]
	if !ok {
		return nil
	}

	return &info
}

func splitPodIPMaskLength(podIP string) (int, string) {
	var prefixLen int
	ip, ipNet, err := net.ParseCIDR(podIP)
	if err != nil || ipNet == nil {
		return 0, podIP // falling back to the test's default - e.g. 24 for v4 / 64 for v6
	}
	prefixLen, _ = ipNet.Mask.Size()
	return prefixLen, ip.String()
}
