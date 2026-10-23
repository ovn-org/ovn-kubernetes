// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"fmt"
	"net"
	"strings"

	current "github.com/containernetworking/cni/pkg/types/100"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kubevirt"
	ovs "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var (
	minRsrc           = resource.MustParse("1k")
	maxRsrc           = resource.MustParse("1P")
	BandwidthNotFound = &notFoundError{}
)

const dpuNotReadyMsg = "DPU Not Ready"

type direction int

func (d direction) String() string {
	if d == Egress {
		return "egress"
	}
	return "ingress"
}

const (
	Egress direction = iota
	Ingress
)

type notFoundError struct{}

func (*notFoundError) Error() string {
	return "not found"
}

func validateBandwidthIsReasonable(rsrc *resource.Quantity) error {
	if rsrc.Value() < minRsrc.Value() {
		return fmt.Errorf("resource is unreasonably small (< 1kbit)")
	}
	if rsrc.Value() > maxRsrc.Value() {
		return fmt.Errorf("resoruce is unreasonably large (> 1Pbit)")
	}
	return nil
}

func extractPodBandwidth(podAnnotations map[string]string, dir direction) (int64, error) {
	annotation := "kubernetes.io/ingress-bandwidth"
	if dir == Egress {
		annotation = "kubernetes.io/egress-bandwidth"
	}

	str, found := podAnnotations[annotation]
	if !found {
		return 0, BandwidthNotFound
	}
	bwVal, err := resource.ParseQuantity(str)
	if err != nil {
		return 0, err
	}
	if err := validateBandwidthIsReasonable(&bwVal); err != nil {
		return 0, err
	}
	return bwVal.Value(), nil
}

func (pr *PodRequest) String() string {
	return fmt.Sprintf("[%s/%s %s network %s NAD %s NAD key %s]", pr.PodNamespace, pr.PodName, pr.SandboxID, pr.netName, pr.nadName, pr.nadKey)
}

func (pr *PodRequest) cmdAdd(kubeAuth *KubeAPIAuth, clientset *ClientSet, ovsClient client.Client) (*Response, error) {
	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	kubecli := &kube.Kube{KClient: clientset.kclient}

	pod, _, _, err := getPodWithAnnotations(pr.ctx, clientset, namespace, podName, "", pr.PodUID, false,
		func(*corev1.Pod, string) (*util.PodAnnotation, bool, error) {
			return nil, true, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %v", namespace, podName, err)
	}

	// The runtime may omit the UID, and for static pods it passes a config hash
	// rather than the mirror Pod UID. Pin the rest of ADD to the Pod instance
	// that the UID-aware lookup above accepted.
	pr.PodUID = string(pod.UID)

	// nadKey is only set for default network and primary UDN
	if pr.nadKey == "" {
		nadKey, err := GetCNINADKey(pod, pr.IfName, pr.nadName)
		if err != nil {
			return nil, fmt.Errorf("failed to get NAD key for CNI Add request %v: %v", pr, err)
		}
		pr.nadKey = nadKey
	}

	annotCondFn := isOvnReady
	var dhcpAnnotation *util.PodAnnotation
	var needsDHCPWrite bool
	var dpuConnDetails *util.DPUConnectionDetails
	// On localnet topologies with DHCP IPAM the pod-networks entry is
	// written by the CNI itself, so the wait condition resolves to the
	// locally allocated annotation.
	if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
		// unprivileged mode cannot run the DHCP exchange, so fail now,
		// before any annotation is written
		if config.UnprivilegedMode {
			return nil, fmt.Errorf("dhcp IPAM mode for localnet topology is not supported in unprivileged mode")
		}
		dhcpAnnotation, needsDHCPWrite, err = pr.allocateDHCPMACAnnotation(pod)
		if err != nil {
			return nil, fmt.Errorf("failed to allocate the DHCP pod-networks entry for pod %s/%s: %w",
				namespace, podName, err)
		}
		// Wait until the informer cache reflects the MAC-only entry staged
		// above: the lease patch after the DHCP exchange rebuilds the
		// annotation from the cache on every retry, and a stale base fails
		// its JSON-patch test op. The wait also spans the write itself,
		// which happens between here and the GetPodWithAnnotations call.
		annotCondFn = func(pod *corev1.Pod, _ string) (*util.PodAnnotation, bool, error) {
			a, err := util.UnmarshalPodAnnotation(pod.Annotations, pr.nadKey)
			if err != nil || !util.IsValidPodAnnotation(a) ||
				a.MAC.String() != dhcpAnnotation.MAC.String() || len(a.IPs) > 0 {
				return nil, false, nil
			}
			return dhcpAnnotation, true, nil
		}
	}
	netdevName := ""
	if pr.CNIConf.DeviceID != "" {
		var err error

		if !pr.IsVFIO {
			netdevName, err = util.GetNetdevNameFromDeviceId(pr.CNIConf.DeviceID, pr.deviceInfo)
			if err != nil {
				return nil, fmt.Errorf("failed in cmdAdd while getting Netdevice name: %w", err)
			}
		}
		if config.IsModeDPUHost() {
			// Resolve the DPU connection details so ovnkube-node running on DPU
			// performs the needed network plumbing.
			if dpuConnDetails, err = pr.allocateDPUConnectionDetails(netdevName); err != nil {
				return nil, err
			}
			// Defer default-network DPU readiness gating so the primary UDN annotation/DPU readiness can progress in parallel when present.
		}
		// In the case of SmartNIC (CX5), we store the netdevname in the representor's
		// OVS interface's external_id column. This is done in ConfigureInterface().
	}

	// When a DHCP entry is staged, it and the DPU connection details
	// are written in a single update. When a DPU-only write is needed,
	// it goes through the DPU flow's own writer.
	if needsDHCPWrite {
		if err := pr.updateDHCPAndDPUAnnotations(clientset, kubecli, pod, dhcpAnnotation, dpuConnDetails); err != nil {
			return nil, err
		}
	} else if dpuConnDetails != nil {
		if err := pr.updatePodDPUConnDetailsWithRetry(kubecli, clientset.podLister, pod, dpuConnDetails); err != nil {
			return nil, fmt.Errorf("failed to update the DPU connection details annotation of pod %s/%s: %w",
				pr.PodNamespace, pr.PodName, err)
		}
	}

	// now checks for default network's DPU connection status
	if config.IsModeDPUHost() {
		if pr.CNIConf.DeviceID != "" {
			annotCondFn = isDPUReady(annotCondFn, pr.nadKey)
		}
	}

	// Get the IP address and MAC address of the pod
	// for DPU, ensure connection-details is present
	pod, annotations, podNADAnnotation, err := getPodWithAnnotations(
		pr.ctx, clientset, namespace, podName, pr.nadKey, pr.PodUID, true, annotCondFn)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod annotation: %v", err)
	}

	podInterfaceInfo, err := pr.buildPodInterfaceInfo(annotations, podNADAnnotation, netdevName)
	if err != nil {
		return nil, err
	}
	// get all the Pod interface names of the same nadName. See if this is a pod with multiple secondary UDN of nadName
	podIfNamesOfSameNAD, _ := GetPodIfNamesForNAD(pod, pr.nadName)
	if len(podIfNamesOfSameNAD) > 1 {
		podInterfaceInfo.PodIfNamesOfSameNAD = podIfNamesOfSameNAD
	}

	podInterfaceInfo.SkipIPConfig = kubevirt.IsPodLiveMigratable(pod)

	// On a DHCP IPAM network the annotation's L3 config only reports the
	// previous sandbox's lease, which was released on DEL. Re-applying it on
	// a repeat ADD would program a stale IP and a conflicting default route,
	// failing the ADD forever. Clear it so the DHCP exchange below is the
	// only source of addressing.
	if podNADAnnotation.IPAMMode == types.IPAMTypeDHCP {
		podInterfaceInfo.IPs = nil
		podInterfaceInfo.Gateways = nil
		podInterfaceInfo.Routes = nil
	}

	response := &Response{KubeAuth: kubeAuth}
	if !config.UnprivilegedMode {
		if ovsClient == nil && !config.IsModeDPUHost() {
			return nil, fmt.Errorf("OVS client is required in privileged mode")
		}

		netName := pr.netName
		if pr.CNIConf.PhysicalNetworkName != "" {
			netName = pr.CNIConf.PhysicalNetworkName
		}

		// Skip checking bridge mapping on DPU hosts as OVS is not present
		if config.IsModeDPU() || config.IsModeFull() {
			if err := checkBridgeMapping(ovsClient, pr.CNIConf.Topology, netName); err != nil {
				return nil, fmt.Errorf("failed bridge mapping validation: %w", err)
			}
		}

		response.Result, err = getCNIResult(pr, ovsClient, clientset, podInterfaceInfo)
		if err != nil {
			return nil, err
		}

		// If IPAM mode is DHCP, obtain IP configuration from an external DHCP
		// server. The lease-handling behavior is selected by workload type:
		//   - KubeVirt VMs: perform a one-shot DHCP discovery to learn the IP and
		//     report it via the pod annotation only. The IP is never applied to the
		//     interface (a VFIO VF loses it on rebind; a non-VFIO VM would start
		//     KubeVirt's in-pod dnsmasq). The guest runs its own DHCP client.
		//   - Regular pods: delegate to the DHCP CNI plugin daemon, which applies
		//     the IP and maintains the lease for the pod's lifetime.
		//
		// The two paths differ in lease identity across sandbox recreations:
		//   - KubeVirt VMs: the one-shot exchange presents the annotation MAC
		//     as the DHCP client-id and the lease is never released on DEL
		//     (the guest owns it), so a recreated sandbox re-acquires the
		//     same lease and IP.
		//   - Regular pods: the delegated daemon presents
		//     containerID/network-name/ifName as the client-id and the lease
		//     is released on DEL, so a recreated sandbox is a new DHCP client
		//     and may be assigned a different IP, which ovnkube-controller
		//     absorbs by reprocessing the port on the annotation patch below
		//     (see dhcpPodNetworkUpdated).
		//
		// In both cases the learned IPs are merged into the CNI result (multus
		// network-status) and patched into the k8s.ovn.org/pod-networks
		// annotation so ovnkube-controller programs the logical switch port and
		// IP-based features (MultiNetworkPolicy, NetworkQoS) see the address.
		if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
			var dhcpResult *current.Result
			if kubevirt.IsPodOwnedByVirtualMachine(pod) {
				dhcpResult, err = dhcpOps.DoOneShot(pr)
				if err != nil {
					return nil, fmt.Errorf("VM DHCP discovery failed for pod %s/%s: %v",
						pr.PodNamespace, pr.PodName, err)
				}
			} else {
				// A VFIO device exposes no netdev in the pod netns for the
				// DORA exchange. Reject here instead of failing it in the DHCP plugin.
				if pr.IsVFIO {
					return nil, fmt.Errorf("dhcp IPAM mode is not supported for VFIO device %s on regular pod %s/%s: "+
						"DHCP with VFIO is only supported for KubeVirt VM pods",
						pr.CNIConf.DeviceID, pr.PodNamespace, pr.PodName)
				}
				// An attachment carrying the default-route key owns the pod's
				// default route (the primary network yields it, see
				// allocator/pod), so keep the DHCP-provided default routes
				// instead of filtering them out
				nse, err := util.GetK8sPodNetworkSelection(pod, pr.nadKey)
				if err != nil {
					return nil, fmt.Errorf("failed to get the network selection of pod %s/%s for NAD %s: %w",
						pr.PodNamespace, pr.PodName, pr.nadKey, err)
				}
				defaultRouteRequested := nse != nil && len(nse.GatewayRequest) > 0
				dhcpResult, err = dhcpOps.ExecAdd(pr, defaultRouteRequested)
				if err != nil {
					return nil, fmt.Errorf("DHCP IPAM ADD failed for pod %s/%s: %v",
						pr.PodNamespace, pr.PodName, err)
				}
			}
			mergeDHCPResultIntoCNIResult(dhcpResult, response.Result)
			if err := pr.updatePodNetworksAnnotationWithDHCPResult(clientset, dhcpResult); err != nil {
				return nil, fmt.Errorf("failed to report DHCP IPs in pod-networks annotation for pod %s/%s: %v",
					pr.PodNamespace, pr.PodName, err)
			}
		}
	} else {
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

func (pr *PodRequest) cmdDel(clientset *ClientSet, ovsClient client.Client) (*Response, error) {
	if !config.UnprivilegedMode && ovsClient == nil && !config.IsModeDPUHost() {
		return nil, fmt.Errorf("OVS client is required in privileged mode")
	}

	// assume success case, return an empty Result
	response := &Response{}
	response.Result = &current.Result{}

	namespace := pr.PodNamespace
	podName := pr.PodName
	if namespace == "" || podName == "" {
		return nil, fmt.Errorf("required CNI variable missing")
	}

	pod, err := clientset.getPod(pr.PodNamespace, pr.PodName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get pod %s/%s: %w", pr.PodNamespace, pr.PodName, err)
		}
	}

	if pod != nil && pr.netName != types.DefaultNetworkName {
		nadKey, err := GetCNINADKey(pod, pr.IfName, pr.nadName)
		if err != nil {
			return nil, err
		}
		pr.nadKey = nadKey
	} else {
		pr.nadKey = pr.nadName
	}

	// Release the DHCP lease before the teardown below removes the transmit
	// path. The dhcp plugin daemon tracks its own leases and no-ops for
	// containers it never served, so it is safe to always invoke it; a
	// missing plugin binary means no lease exists (ADD hard-requires it) and
	// a VFIO device's lease is owned by the guest. Releases are best-effort:
	// when one fails, the server simply expires the lease once its time
	// runs out.
	if !config.UnprivilegedMode && pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP && !pr.IsVFIO {
		if _, err := getDHCPPluginPath(getCNIPath()); err != nil {
			klog.V(5).Infof("DHCP: plugin not found in CNI_PATH, no lease to release for pod %s/%s: %v",
				pr.PodNamespace, pr.PodName, err)
		} else {
			klog.Infof("DHCP: releasing lease for pod %s/%s iface %s netns %s container %s",
				pr.PodNamespace, pr.PodName, pr.IfName, pr.Netns, pr.SandboxID)
			if delErr := dhcpOps.ExecDel(pr); delErr != nil {
				klog.Warningf("DHCP: failed to release the lease of pod %s/%s (sandbox %s, iface %s), "+
					"it expires on the server instead: %v",
					pr.PodNamespace, pr.PodName, pr.SandboxID, pr.IfName, delErr)
			} else {
				klog.Infof("DHCP: released lease for pod %s/%s", pr.PodNamespace, pr.PodName)
			}
		}
	}

	netdevName := ""
	if pr.CNIConf.DeviceID != "" {
		if config.IsModeDPUHost() {
			var dpuCD *util.DPUConnectionDetails
			if pod == nil {
				// no need to update DPU connection-details annotation if pod is already removed
				klog.Warningf("Failed to get pod %s/%s: %v", pr.PodNamespace, pr.PodName, err)
			} else {
				dpuCD, err = util.UnmarshalPodDPUConnDetails(pod.Annotations, pr.nadKey)
				if err != nil {
					klog.Warningf("Failed to get DPU connection details annotation for pod %s/%s NAD key %s: %v", pr.PodNamespace,
						pr.PodName, pr.nadKey, err)
				}
			}
			if dpuCD == nil {
				if !util.IsSimulatedDPU() {
					// a PCI VF returns to the initial network namespace when the
					// pod namespace is removed, nothing to do
					return response, nil
				}
				// A simulated device is a veth and is destroyed with the pod namespace unless it is moved back first.
				netdevName = pr.CNIConf.DeviceID
			} else {
				// check if this cmdDel is meant for the current sandbox, if not, directly return
				if dpuCD.SandboxId != pr.SandboxID {
					klog.Infof("The cmdDel request for sandbox %s is not meant for the currently configured "+
						"pod %s/%s on NAD key %s with sandbox %s. Ignoring this request.",
						pr.SandboxID, namespace, podName, pr.nadKey, dpuCD.SandboxId)
					return response, nil
				}

				netdevName = dpuCD.VfNetdevName
				if pr.netName == types.DefaultNetworkName {
					// if this is the default network name, remove the whole DPU connection-details annotation,
					// including the primary UDN connection-details if any
					updatePodAnnotationNoRollback := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
						delete(pod.Annotations, util.DPUConnectionDetailsAnnot)
						return pod, nil, nil
					}

					err = util.UpdatePodWithRetryOrRollback(
						clientset.podLister,
						&kube.Kube{KClient: clientset.kclient},
						pod,
						updatePodAnnotationNoRollback,
					)
				} else {
					// Delete the DPU connection-details annotation for this NAD
					err = pr.updatePodDPUConnDetailsWithRetry(&kube.Kube{KClient: clientset.kclient}, clientset.podLister, pod, nil)
				}
				// not an error if pod has already been deleted
				if err != nil && !apierrors.IsNotFound(err) {
					return nil, fmt.Errorf("failed to cleanup the DPU connection details annotation for NAD key %s: %v", pr.nadKey, err)
				}
			}
		} else {
			// Find the hostInterface name
			ovsIfNames, err := findPodInterfaces(ovsClient, pr.SandboxID, pr.IfName, pr.netName, pr.nadKey)
			if err != nil || len(ovsIfNames) != 1 {
				klog.Warningf("Couldn't find the OVS interface for pod %s/%s NAD key %s: %v",
					pr.PodNamespace, pr.PodName, pr.nadKey, err)
			} else {
				out, err := getInterfaceExternalID(ovsClient, ovsIfNames[0], "vf-netdev-name")
				if err != nil {
					klog.Warningf("Couldn't find the original Netdev name from OVS interface %s for pod %s/%s: %v",
						ovsIfNames[0], pr.PodNamespace, pr.PodName, err)
				} else {
					netdevName = out
				}
			}
		}
	}

	podInterfaceInfo := &PodInterfaceInfo{
		IsDPUHostMode:  config.IsModeDPUHost(),
		IsSimulatedDPU: util.IsSimulatedDPU(),
		NetdevName:     netdevName,
	}
	if !config.UnprivilegedMode {
		err := podRequestInterfaceOps.UnconfigureInterface(pr, ovsClient, podInterfaceInfo, clientset.podLister, pod)
		if err != nil {
			return nil, err
		}
	} else {
		// pass the isDPU flag and vfNetdevName back to cniShim
		if pr.CNIConf.IPAM.Type == types.IPAMTypeDHCP {
			klog.Warningf("DHCP lease release skipped for pod %s/%s: not supported in unprivileged mode",
				pr.PodNamespace, pr.PodName)
		}
		response.Result = nil
		response.PodIFInfo = podInterfaceInfo
	}
	return response, nil
}

// getCNIResult get result from pod interface info.
// PodInfoGetter is used to check if sandbox is still valid for the current
// instance of the Pod, see checkCancelSandbox for more info. If Pod information
// is not available from the CNI, pass nil to skip this check.
func getCNIResult(pr *PodRequest, ovsClient client.Client, getter PodInfoGetter, podInterfaceInfo *PodInterfaceInfo) (*current.Result, error) {
	interfacesArray, err := podRequestInterfaceOps.ConfigureInterface(pr, ovsClient, getter, podInterfaceInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to configure pod interface: %v", err)
	}

	gateways := map[string]net.IP{}
	for _, gw := range podInterfaceInfo.Gateways {
		if gw.To4() != nil && gateways["4"] == nil {
			gateways["4"] = gw
		} else if gw.To4() == nil && gateways["6"] == nil {
			gateways["6"] = gw
		}
	}

	// Build the result structure to pass back to the runtime
	ips := []*current.IPConfig{}
	for _, ipcidr := range podInterfaceInfo.IPs {
		ip := &current.IPConfig{
			Interface: current.Int(1),
			Address:   *ipcidr,
		}
		var ipVersion string
		if utilnet.IsIPv6CIDR(ipcidr) {
			ipVersion = "6"
		} else {
			ipVersion = "4"
		}
		ip.Gateway = gateways[ipVersion]
		ips = append(ips, ip)
	}

	return &current.Result{
		Interfaces: interfacesArray,
		IPs:        ips,
	}, nil
}

func (pr *PodRequest) buildPodInterfaceInfo(annotations map[string]string, podAnnotation *util.PodAnnotation, netDevice string) (*PodInterfaceInfo, error) {
	return PodAnnotation2PodInfo(
		annotations,
		podAnnotation,
		pr.PodUID,
		netDevice,
		pr.nadKey,
		pr.netName,
		pr.CNIConf.MTU,
	)
}

func checkBridgeMapping(ovsClient client.Client, topology string, networkName string) error {
	if topology != types.LocalnetTopology || networkName == types.DefaultNetworkName {
		return nil
	}

	openvSwitch, err := ovs.GetOpenvSwitch(ovsClient)
	if err != nil {
		return fmt.Errorf("failed getting openvswitch: %w", err)
	}

	ovnBridgeMappings := openvSwitch.ExternalIDs["ovn-bridge-mappings"]

	bridgeMappings := strings.Split(ovnBridgeMappings, ",")
	for _, bridgeMapping := range bridgeMappings {
		networkBridgeAssociation := strings.Split(bridgeMapping, ":")
		if len(networkBridgeAssociation) == 2 && networkBridgeAssociation[0] == networkName {
			return nil
		}
	}
	klog.V(5).Infof("Failed to find bridge mapping for network: %q, current OVN bridge-mappings: (%s)", networkName, ovnBridgeMappings)
	return fmt.Errorf("failed to find OVN bridge-mapping for network: %q", networkName)
}
