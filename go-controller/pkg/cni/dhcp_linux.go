// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

// DHCP IPAM support for localnet UDNs (OKEP-6224): delegation to the dhcp
// CNI plugin daemon for pods, one-shot DORA for KubeVirt VMs (including the
// VFIO driver handoff) and result/annotation reporting.

package cni

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/invoke"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mdlayher/packet"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// nclient4 doubles the timeout per retry, so these values bound the
	// worst-case DORA at 2*(10s+20s) = 60s, half the 2-minute CNI request
	// deadline, leaving room for cleanup when no DHCP server answers.
	vmDHCPTimeout = 10 * time.Second
	vmDHCPRetries = 2
)

// vfioHandoffJournalDir holds one journal file per PCI device undergoing the
// VFIO DHCP driver handoff (runLocalnetVFIODHCP). The journal is written
// even before the first destructive driver operation and removed once the handoff
// concludes with the device back on vfio-pci, so its presence marks an
// interrupted handoff regardless of which intermediate driver state a crash
// froze.
var vfioHandoffJournalDir = "/var/run/ovn-kubernetes/cni/vfio-handoff"

const (
	mlx5CoreDriver = "mlx5_core"
	vfioPCIDriver  = "vfio-pci"

	// pciVendorMellanox is the PCI vendor ID of NVIDIA/Mellanox NICs, as read
	// from the sysfs vendor attribute.
	pciVendorMellanox = "0x15b3"
)

// vfKernelDriverByVendor maps PCI vendor IDs to the kernel VF netdev driver
// used for the temporary VFIO→kernel driver handoff. Only vendors validated
// with this flow are listed — the handoff refuses unknown vendors before
// touching the driver binding (OKEP-6224: an unknown/unsupported NIC vendor
// must fail the CNI request).
var vfKernelDriverByVendor = map[string]string{
	pciVendorMellanox: mlx5CoreDriver, // NVIDIA/Mellanox ConnectX
}

// sysfs PCI roots; vars so unit tests can point them at a fake sysfs tree.
var (
	sysBusPCIDevices = "/sys/bus/pci/devices"
	sysBusPCIDrivers = "/sys/bus/pci/drivers"
)

func getPCIDeviceDriver(deviceID string) (string, error) {
	driverPath := filepath.Join(sysBusPCIDevices, deviceID, "driver")
	driverTarget, err := os.Readlink(driverPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read driver for PCI device %s: %w", deviceID, err)
	}
	return filepath.Base(driverTarget), nil
}

// getPCIDeviceVendor returns the device's PCI vendor ID as sysfs reports it
// (e.g. "0x15b3" for Mellanox).
func getPCIDeviceVendor(deviceID string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(sysBusPCIDevices, deviceID, "vendor"))
	if err != nil {
		return "", fmt.Errorf("failed to read PCI vendor of device %s: %w", deviceID, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// getVfKernelDriverForDevice returns the kernel netdev driver for the VF's
// hardware, looked up by PCI vendor ID. An unknown vendor must fail here,
// while the VF is still bound to vfio-pci, rather than unbind it and force
// a wrong driver.
func getVfKernelDriverForDevice(deviceID string) (string, error) {
	vendor, err := getPCIDeviceVendor(deviceID)
	if err != nil {
		return "", err
	}
	driver, ok := vfKernelDriverByVendor[vendor]
	if !ok {
		return "", fmt.Errorf("cannot run DHCP on VFIO device %s: unsupported NIC vendor %s, "+
			"no kernel driver known for the DHCP driver handoff (supported: %s NVIDIA/Mellanox → %s)",
			deviceID, vendor, pciVendorMellanox, mlx5CoreDriver)
	}
	return driver, nil
}

func writePCIDeviceDriverOverride(deviceID, driver string) error {
	overridePath := filepath.Join(sysBusPCIDevices, deviceID, "driver_override")
	overrideValue := []byte(driver)
	if driver == "" {
		overrideValue = []byte("\n")
	}
	if err := os.WriteFile(overridePath, overrideValue, 0o644); err != nil {
		return fmt.Errorf("failed to write driver override for PCI device %s: %w", deviceID, err)
	}
	return nil
}

// healInterruptedVFIOHandoff rebinds the device to vfio-pci when a previous
// CNI ADD died mid driver-handoff, detected by the journal that
// runLocalnetVFIODHCP writes before its first driver operation. The journal
// covers every crash state
//  1. bound to the kernel driver (sysfs alone cannot tell it from a plain netdev VF,
//
// so the retried ADD would silently plumb the VM's VF as a netdev, which is harmless).
//  2. bound to nothing (the retried ADD would wedge forever ie. IsVFIO=false yet no netdev).
//  3. No journal means every non-DHCP attachment costs a single stat.
func healInterruptedVFIOHandoff(deviceID string) error {
	if _, err := os.Stat(vfioHandoffJournalPath(deviceID)); err != nil {
		if os.IsNotExist(err) {
			// no handoff in flight, a regular VF and no need to repair
			return nil
		}
		return fmt.Errorf("failed to check the VFIO handoff journal of device %s: %w", deviceID, err)
	}
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return err
	}
	if currentDriver != vfioPCIDriver {
		klog.Warningf("Device %s is bound to %q with a VFIO handoff journal present, "+
			"repairing an interrupted VFIO DHCP handoff", deviceID, currentDriver)
		if err := dhcpOps.BindPCIDeviceDriver(deviceID, vfioPCIDriver); err != nil {
			// the journal is deliberately kept, the next attempt repairs again
			return fmt.Errorf("failed to restore device %s to %s after an interrupted handoff: %w",
				deviceID, vfioPCIDriver, err)
		}
	}
	// The crash may have hit after bindPCIDeviceDriver cleared driver_override
	// but before the caller re-wrote the vfio-pci pin, so restore the pin
	// unconditionally (a plain idempotent sysfs write when it survived).
	// Recovery is complete only once the pin is durable: on failure keep the
	// journal so the next attempt repairs again.
	if err := writePCIDeviceDriverOverride(deviceID, vfioPCIDriver); err != nil {
		return fmt.Errorf("failed to restore the %s pin on device %s after the repair: %w",
			vfioPCIDriver, deviceID, err)
	}
	removeVFIOHandoffJournal(deviceID)
	return nil
}

// bindPCIDeviceDriver rebinds a PCI device to the given driver via sysfs.
// Note: it always ends with driver_override cleared. Callers that rely on a
// persistent pin (the VFIO DHCP handoff and its crash repair) re-writes the
// pin after each call.
func bindPCIDeviceDriver(deviceID, driver string) error {
	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return err
	}
	if currentDriver == driver {
		return nil
	}

	if err := writePCIDeviceDriverOverride(deviceID, driver); err != nil {
		return err
	}

	if currentDriver != "" {
		unbindPath := filepath.Join(sysBusPCIDevices, deviceID, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(deviceID), 0o644); err != nil {
			_ = writePCIDeviceDriverOverride(deviceID, "")
			return fmt.Errorf("failed to unbind PCI device %s from driver %s: %w", deviceID, currentDriver, err)
		}
	}

	bindPath := filepath.Join(sysBusPCIDrivers, driver, "bind")
	if err := os.WriteFile(bindPath, []byte(deviceID), 0o644); err != nil {
		bindErr := fmt.Errorf("failed to bind PCI device %s to driver %s: %w", deviceID, driver, err)
		// The bind failed after the unbind. Rebind the original driver
		// rather than leave the device driverless (the override must name
		// it first ie. vfio-pci only claims devices via driver_override).
		// Recovery failures are joined into the returned error.
		if currentDriver != "" {
			if rerr := writePCIDeviceDriverOverride(deviceID, currentDriver); rerr != nil {
				bindErr = errors.Join(bindErr, fmt.Errorf("failed to restore driver override to %s: %w", currentDriver, rerr))
			} else if rerr := os.WriteFile(filepath.Join(sysBusPCIDrivers, currentDriver, "bind"),
				[]byte(deviceID), 0o644); rerr != nil {
				bindErr = errors.Join(bindErr, fmt.Errorf("failed to restore PCI device to driver %s: %w", currentDriver, rerr))
			}
		}
		if oerr := writePCIDeviceDriverOverride(deviceID, ""); oerr != nil {
			bindErr = errors.Join(bindErr, oerr)
		}
		return bindErr
	}

	if err := writePCIDeviceDriverOverride(deviceID, ""); err != nil {
		return err
	}
	return nil
}

func (pr *PodRequest) shouldRunLocalnetVFIODriverHandoff() bool {
	return pr != nil &&
		pr.CNIConf != nil &&
		pr.CNIConf.DeviceID != "" &&
		pr.IsVFIO &&
		pr.netName != types.DefaultNetworkName &&
		pr.nadName != types.DefaultNetworkName &&
		pr.CNIConf.Topology == types.LocalnetTopology
}

// DHCPOps abstracts the DHCP IPAM operations so unit tests can stub them
// without exec'ing the dhcp plugin binary, opening raw DHCP sockets, or
// entering a network namespace.
type DHCPOps interface {
	// keepDefaultRoutes preserves DHCP-provided default routes in the
	// applied result, for attachments the user designated as the pod's
	// default-route owner via the selection element's default-route key
	ExecAdd(pr *PodRequest, keepDefaultRoutes bool) (*current.Result, error)
	ExecDel(pr *PodRequest) error
	DoOneShot(pr *PodRequest) (*current.Result, error)
	ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error
	BindPCIDeviceDriver(deviceID, driver string) error
	AcquireLease(ctx context.Context, link netlink.Link) (*current.Result, *nclient4.Lease, error)
	ReleaseLease(link netlink.Link, lease *nclient4.Lease, hwAddr net.HardwareAddr) error
}

type defaultDHCPOps struct{}

var dhcpOps DHCPOps = &defaultDHCPOps{}

func (*defaultDHCPOps) ExecAdd(pr *PodRequest, keepDefaultRoutes bool) (*current.Result, error) {
	return pr.execDHCPAdd(keepDefaultRoutes)
}
func (*defaultDHCPOps) ExecDel(pr *PodRequest) error                      { return pr.execDHCPDel() }
func (*defaultDHCPOps) DoOneShot(pr *PodRequest) (*current.Result, error) { return pr.doOneShotDHCP() }
func (*defaultDHCPOps) ApplyResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	return applyDHCPResult(netnsPath, ifName, dhcpResult)
}
func (*defaultDHCPOps) BindPCIDeviceDriver(deviceID, driver string) error {
	return bindPCIDeviceDriver(deviceID, driver)
}

func (*defaultDHCPOps) AcquireLease(ctx context.Context, link netlink.Link) (*current.Result, *nclient4.Lease, error) {
	return acquireDHCPLease(ctx, link)
}

func (*defaultDHCPOps) ReleaseLease(link netlink.Link, lease *nclient4.Lease, hwAddr net.HardwareAddr) error {
	return releaseDHCPLease(link, lease, hwAddr)
}

// dhcpPluginExec overrides the exec environment handed to the CNI invoke API
// for the delegated dhcp plugin; nil selects the library default. Unit tests
// inject a fake to avoid exec'ing the plugin binary.
var dhcpPluginExec invoke.Exec

func getDHCPPluginPath(cniPath string) (string, error) {
	if dhcpPluginExec != nil {
		return dhcpPluginExec.FindInPath(types.IPAMTypeDHCP, filepath.SplitList(cniPath))
	}
	return invoke.FindInPath(types.IPAMTypeDHCP, filepath.SplitList(cniPath))
}

// execDHCPAdd delegates to the DHCP IPAM plugin to obtain IP configuration
// for the pod interface, applies the result (IPs, routes, gateway) inside
// the container netns, and returns the DHCP result for the caller to merge
// into the CNI result and is reported via the pod-networks annotation.
//
// The plugin daemon presents containerID/network-name/ifName as the DHCP
// client-id, so every sandbox is a distinct DHCP client: after a sandbox
// recreation the pod may be assigned a different IP even though its MAC is
// unchanged (servers often re-offer the same address by MAC affinity, but
// that is not guaranteed).
func (pr *PodRequest) execDHCPAdd(keepDefaultRoutes bool) (*current.Result, error) {
	dhcpConfBytes, err := pr.buildDHCPConf()
	if err != nil {
		return nil, err
	}

	cniPath := getCNIPath()
	pluginPath, err := getDHCPPluginPath(cniPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find dhcp plugin in CNI_PATH (%s): %v", cniPath, err)
	}

	// Build explicit CNI args since the server doesn't have kubelet's env
	cniArgs := &invoke.Args{
		Command:     "ADD",
		ContainerID: pr.SandboxID,
		NetNS:       pr.Netns,
		IfName:      pr.IfName,
		Path:        cniPath,
	}

	ipamResult, err := invoke.ExecPluginWithResult(pr.ctx, pluginPath, dhcpConfBytes, cniArgs, dhcpPluginExec)
	if err != nil {
		return nil, fmt.Errorf("failed to execute DHCP plugin ADD: %v", err)
	}

	dhcpResult, err := current.GetResult(ipamResult)
	if err != nil {
		return nil, fmt.Errorf("failed to parse DHCP result: %v", err)
	}

	if len(dhcpResult.IPs) == 0 {
		return nil, fmt.Errorf("no IP addresses returned by DHCP plugin")
	}

	if keepDefaultRoutes {
		klog.Infof("DHCP: keeping DHCP-provided default route(s) for pod %s/%s iface %s: "+
			"the attachment requested the default route",
			pr.PodNamespace, pr.PodName, pr.IfName)
	} else if dropped := filterOutDefaultRoutes(dhcpResult); len(dropped) > 0 {
		klog.Infof("DHCP: dropped default route(s) %v for pod %s/%s iface %s: "+
			"the pod's primary network owns the default route",
			dropped, pr.PodNamespace, pr.PodName, pr.IfName)
	}

	klog.Infof("DHCP IPAM for pod %s/%s on %s: IPs=%v",
		pr.PodNamespace, pr.PodName, pr.IfName, dhcpResult.IPs)

	// Apply DHCP-obtained IPs and routes to the interface inside the container
	// netns. No lease release on failure: the DEL the runtime issues for the
	// failed sandbox releases the lease.
	if err := dhcpOps.ApplyResult(pr.Netns, pr.IfName, dhcpResult); err != nil {
		return nil, fmt.Errorf("failed to apply DHCP result to %s: %v", pr.IfName, err)
	}
	return dhcpResult, nil
}

// allocateDHCPMACAnnotation returns the MAC-only pod-networks entry that
// bootstraps a DHCP IPAM attachment; the CNI is the entry's single writer.
//
// When the pod already has an entry, its MAC is kept but its lease is
// dropped. The MAC is the pod's stable interface identity (and, for VMs,
// its DHCP client-id, so the guest re-acquires the same lease). The old
// lease must go because ovnkube-controller pins it in the LSP port
// security, which would drop the unicast DHCP replies if the server now
// assigns a different address; once the cleared entry is written, the
// controller relaxes the port to MAC-only and the exchange can proceed.
//
// When the pod has no entry yet, a new MAC is used: the MacRequest from
// the pod's network selection when set, a random one otherwise.
func (pr *PodRequest) allocateDHCPMACAnnotation(pod *corev1.Pod) (podAnnotation *util.PodAnnotation, needsWrite bool, err error) {
	if a, err := util.UnmarshalPodAnnotation(pod.Annotations, pr.nadKey); err == nil && util.IsValidPodAnnotation(a) {
		macOnlyAnnotation := &util.PodAnnotation{MAC: a.MAC, Role: a.Role, IPAMMode: a.IPAMMode}
		return macOnlyAnnotation, !reflect.DeepEqual(a, macOnlyAnnotation), nil
	}

	var mac net.HardwareAddr
	nse, err := util.GetK8sPodNetworkSelection(pod, pr.nadKey)
	if err != nil {
		return nil, false, fmt.Errorf("failed to get the network selection element of pod %s/%s for NAD key %s: %w",
			pr.PodNamespace, pr.PodName, pr.nadKey, err)
	}
	if nse != nil && nse.MacRequest != "" {
		mac, err = net.ParseMAC(nse.MacRequest)
		if err != nil {
			return nil, false, fmt.Errorf("failed to parse the requested MAC %q of pod %s/%s: %w",
				nse.MacRequest, pr.PodNamespace, pr.PodName, err)
		}
	} else {
		mac, err = util.GenerateRandMAC()
		if err != nil {
			return nil, false, fmt.Errorf("failed to generate a MAC for pod %s/%s: %w",
				pr.PodNamespace, pr.PodName, err)
		}
	}

	return &util.PodAnnotation{
		MAC:      mac,
		Role:     types.NetworkRoleSecondary,
		IPAMMode: types.IPAMTypeDHCP,
	}, true, nil
}

// updateDHCPAndDPUAnnotations writes the DHCP pod-networks entry and, on DPU
// hosts, the DPU connection details to the pod in a single API update.
// Writing both atomically guarantees the DPU, which starts plumbing as soon as
// the connection details appear, never sees the pod without its MAC.
// Pods without a DHCP entry are not routed here, their connection details
// are written by the DPU flow's own updatePodDPUConnDetailsWithRetry.
func (pr *PodRequest) updateDHCPAndDPUAnnotations(clientset *ClientSet, kubecli kube.Interface, pod *corev1.Pod,
	dhcpEntry *util.PodAnnotation, dpuConnDetails *util.DPUConnectionDetails) error {
	// Every attempt of the retry loop below re-fetches the pod by
	// namespace/name. If the pod is deleted and recreated with the same name
	// mid-request, an attempt can come back holding the new pod instance and
	// would stamp this request's annotations onto it and the new pod's own ADD
	// would then reuse this stale MAC instead of minting its own. Guard each
	// attempt by comparing the fetched pod's UID against the UID the runtime
	// established by the initial UID-aware lookup in cmdAdd: a mismatch means
	// the wrong pod instance, so fail the ADD
	// without writing rather than retry.
	updateFn := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		if pr.PodUID != "" && string(pod.UID) != pr.PodUID {
			return nil, nil, fmt.Errorf("pod %s/%s was replaced while staging CNI annotations: "+
				"expected UID %q, found UID %q", pr.PodNamespace, pr.PodName, pr.PodUID, pod.UID)
		}
		if dhcpEntry != nil {
			// The DHCP entry clears the previous sandbox's lease. If this
			// request's netns is gone, kubelet has abandoned it and a newer
			// sandbox may already have patched its fresh lease, which this
			// superseded request must not wipe out.
			if _, err := os.Stat(pr.Netns); err != nil {
				return nil, nil, fmt.Errorf("sandbox %s of pod %s/%s was superseded (netns %q is gone), "+
					"refusing to stage the DHCP annotation: %w",
					pr.SandboxID, pr.PodNamespace, pr.PodName, pr.Netns, err)
			}
			annotations, err := util.MarshalPodAnnotation(pod.Annotations, dhcpEntry, pr.nadKey)
			if err != nil {
				return nil, nil, err
			}
			pod.Annotations = annotations
		}
		if dpuConnDetails != nil {
			annotations, err := util.MarshalPodDPUConnDetails(pod.Annotations, dpuConnDetails, pr.nadKey)
			if err != nil {
				// a retried ADD already carrying these details is not an
				// error, keep going so a DHCP entry staged above still lands.
				// If nothing changed at all, the update is a no-op with no
				// API write.
				if !util.IsAnnotationAlreadySetError(err) {
					return nil, nil, err
				}
			} else {
				pod.Annotations = annotations
			}
		}
		return pod, nil, nil
	}
	if err := util.UpdatePodWithRetryOrRollback(pr.ctx, clientset.podLister, kubecli, pod, updateFn); err != nil {
		return fmt.Errorf("failed to update the DHCP and DPU annotations of pod %s/%s: %w",
			pr.PodNamespace, pr.PodName, err)
	}
	return nil
}

// updatePodNetworksAnnotationWithDHCPResult patches the pod's
// k8s.ovn.org/pod-networks entry for this NAD with the DHCP-learned
// ip_addresses/gateway_ips. The IPs of a DHCP entry are owned by the external
// DHCP server and on a repeat CNI ADD (sandbox recreation) with a new lease, they
// are overwritten with the newly learned ones.
func (pr *PodRequest) updatePodNetworksAnnotationWithDHCPResult(clientset *ClientSet, dhcpResult *current.Result) error {
	pod, err := clientset.getPod(pr.PodNamespace, pr.PodName)
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", pr.PodNamespace, pr.PodName, err)
	}

	updateFn := func(pod *corev1.Pod) (*corev1.Pod, func(), error) {
		// The lease belongs to this request's sandbox: if that sandbox's
		// netns is gone, kubelet has torn it down. Since kubelet
		// completes the old sandbox's DEL before creating a new one, a newer
		// sandbox may already have patched its fresh lease, which this
		// abandoned request (nothing cancels an in-flight ADD when kubelet
		// times it out and moves on) must not overwrite with its stale one.
		// The check sits INSIDE updateFn so every retry attempt re-runs it:
		// a write conflict with the newer sandbox's patch forces a retry and
		// deterministically lands here after the netns is gone.
		if _, err := os.Stat(pr.Netns); err != nil {
			return nil, nil, fmt.Errorf("sandbox %s of pod %s/%s was superseded (netns %q is gone), "+
				"refusing to patch a stale DHCP lease: %w",
				pr.SandboxID, pr.PodNamespace, pr.PodName, pr.Netns, err)
		}
		// guard against the pod having been deleted and recreated since this
		// CNI ADD started; never report this sandbox's lease on a new instance
		if pr.PodUID != "" && string(pod.UID) != pr.PodUID {
			return nil, nil, fmt.Errorf("pod %s/%s UID %q does not match CNI request UID %q",
				pr.PodNamespace, pr.PodName, pod.UID, pr.PodUID)
		}
		podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, pr.nadKey)
		if err != nil {
			return nil, nil, fmt.Errorf("no pod-networks entry for NAD key %s: %w", pr.nadKey, err)
		}
		podAnnotation.IPs = nil
		podAnnotation.Gateways = nil
		for _, ipc := range dhcpResult.IPs {
			addr := ipc.Address
			podAnnotation.IPs = append(podAnnotation.IPs, &addr)
			if ipc.Gateway != nil {
				podAnnotation.Gateways = append(podAnnotation.Gateways, ipc.Gateway)
			}
		}
		annotations, err := util.MarshalPodAnnotation(pod.Annotations, podAnnotation, pr.nadKey)
		if err != nil {
			if util.IsAnnotationAlreadySetError(err) {
				// repeat CNI ADD with the same lease, nothing to update
				return pod, nil, nil
			}
			return nil, nil, err
		}
		pod.Annotations = annotations
		return pod, nil, nil
	}
	return util.UpdatePodWithRetryOrRollback(pr.ctx, clientset.podLister, &kube.Kube{KClient: clientset.kclient}, pod, updateFn)
}

// execDHCPDel releases the DHCP lease by executing the DHCP plugin's DEL command.
func (pr *PodRequest) execDHCPDel() error {
	dhcpConfBytes, err := pr.buildDHCPConf()
	if err != nil {
		return err
	}

	cniPath := getCNIPath()
	pluginPath, err := getDHCPPluginPath(cniPath)
	if err != nil {
		return fmt.Errorf("failed to find dhcp plugin in CNI_PATH (%s): %v", cniPath, err)
	}

	cniArgs := &invoke.Args{
		Command:     "DEL",
		ContainerID: pr.SandboxID,
		NetNS:       pr.Netns,
		IfName:      pr.IfName,
		Path:        cniPath,
	}

	return invoke.ExecPluginWithoutResult(pr.ctx, pluginPath, dhcpConfBytes, cniArgs, dhcpPluginExec)
}

// buildDHCPConf constructs the JSON network config that the DHCP IPAM plugin expects.
// The cniVersion comes from the UDN-generated NAD (config.CNISpecVersion,
// currently "1.1.0"), and the delegated dhcp plugin rejects any spec version
// its vendored cni library does not support. Spec 1.1.0 requires cni >= v1.2.0,
// first shipped in containernetworking/plugins v1.6.0 and so both the dhcp
// binary and the dhcp daemon on the node must be v1.6.0 or newer.
func (pr *PodRequest) buildDHCPConf() ([]byte, error) {
	conf := map[string]any{
		"cniVersion": pr.CNIConf.CNIVersion,
		"name":       pr.CNIConf.Name,
		"type":       "ovn-k8s-cni-overlay",
		"ipam": map[string]any{
			"type": types.IPAMTypeDHCP,
		},
	}
	return json.Marshal(conf)
}

// getCNIPath returns the CNI plugin binary search path.
// It checks CNI_PATH env var first (set by kubelet or the container runtime),
// then falls back to the standard /opt/cni/bin directory.
func getCNIPath() string {
	if p := os.Getenv("CNI_PATH"); p != "" {
		return p
	}
	return "/opt/cni/bin"
}

// filterOutDefaultRoutes drops default routes from a pod's DHCP result and
// returns the dropped ones. DHCP IPAM networks are always secondary (CRD CEL
// rule), so the pod's primary network already owns the default route;
// installing the DHCP server's default route too would steal the pod's
// egress traffic onto the localnet. Like every other OVN-Kubernetes
// secondary attachment, only specific-prefix routes are honored (option-121
// routes, which pass this filter); a default route on the secondary network
// must be requested explicitly via Multus's default-route annotation instead.
func filterOutDefaultRoutes(dhcpResult *current.Result) []*cnitypes.Route {
	var dropped []*cnitypes.Route
	routes := dhcpResult.Routes[:0]
	for _, route := range dhcpResult.Routes {
		if ones, _ := route.Dst.Mask.Size(); ones == 0 {
			dropped = append(dropped, route)
			continue
		}
		routes = append(routes, route)
	}
	dhcpResult.Routes = routes
	return dropped
}

// applyDHCPResult applies the IP addresses and routes of a DHCP IPAM result
// to the named interface inside the container network namespace.
func applyDHCPResult(netnsPath, ifName string, dhcpResult *current.Result) error {
	netns, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netnsPath, err)
	}
	defer netns.Close()

	return netns.Do(func(_ ns.NetNS) error {
		return applyDHCPResultInNS(ifName, dhcpResult)
	})
}

// applyDHCPResultInNS does the actual interface configuration and must run
// inside the container network namespace. The result's routes are installed
// exactly as handed in, honoring scope (on-link option-121 routes carry
// SCOPE_LINK), priority and table.

// The per-IP Gateway field is not turned into a default route,
// because the dhcp daemon has already translated
// the server's routing options (routers, option-121 static routes) into the
// routes list, applying the RFC 3442 rule that option-121 overrides the
// routers option. Adding "default via Gateway" on top would duplicate a
// default route already in the list, or resurrect the routers option the
// server told us to ignore.
func applyDHCPResultInNS(ifName string, dhcpResult *current.Result) error {
	link, err := util.GetNetLinkOps().LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %v", ifName, err)
	}

	// Add IP addresses from DHCP; EEXIST is tolerated so a repeated ADD
	// stays idempotent
	for _, ipConfig := range dhcpResult.IPs {
		addr := &netlink.Addr{IPNet: &ipConfig.Address}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add IP %s to %s: %v",
				ipConfig.Address.String(), ifName, err)
		}
		klog.Infof("DHCP: applied IP %s to %s", ipConfig.Address.String(), ifName)
	}

	// Route failures are fatal: a pod missing its DHCP-advertised routes
	// must not report a successful ADD. Only EEXIST is tolerated for idempotency.
	for _, route := range dhcpResult.Routes {
		nlRoute := netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       &route.Dst,
			Gw:        route.GW,
			Priority:  route.Priority,
		}
		if route.Table != nil {
			nlRoute.Table = *route.Table
		}
		if route.Scope != nil {
			nlRoute.Scope = netlink.Scope(*route.Scope)
		}
		if err := util.GetNetLinkOps().RouteAdd(&nlRoute); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to add route dst=%s gw=%s scope=%d on %s: %v",
				route.Dst.String(), route.GW, nlRoute.Scope, ifName, err)
		}
		klog.Infof("DHCP: applied route dst=%s gw=%s on %s", route.Dst.String(), route.GW, ifName)
	}

	return nil
}

const (
	netDevPollTimeout  = 5 * time.Second
	netDevPollInterval = 200 * time.Millisecond
)

// getNetdevIndex returns the ifindex of the netdev of PCI device deviceID.
// The kernel creates the netdev asynchronously after a driver bind, and udev
// can rename it at any time, also between listing it and reading its
// ifindex, so the lookup polls until the one netdev and its ifindex are
// read. The ifindex stays the same across renames.
func getNetdevIndex(ctx context.Context, deviceID string) (int, error) {
	netDir := filepath.Join(sysBusPCIDevices, deviceID, "net")
	var index int
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, netDevPollInterval, netDevPollTimeout, true,
		func(context.Context) (bool, error) {
			index, lastErr = readNetdevIndex(netDir)
			return lastErr == nil, nil
		})
	if err != nil {
		return 0, fmt.Errorf("failed to find netdev for device %s: %w", deviceID, errors.Join(err, lastErr))
	}
	return index, nil
}

func readNetdevIndex(netDir string) (int, error) {
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return 0, err
	}
	if len(entries) != 1 {
		return 0, fmt.Errorf("found %d netdevs in %s, expected 1", len(entries), netDir)
	}
	path := filepath.Join(netDir, entries[0].Name(), "ifindex")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	index, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return index, nil
}

// vfioNetdevUp waits for the netdev of PCI device deviceID and brings it up.
// It looks the netdev up by ifindex, and the returned link is used by index
// only, so a udev rename cannot make a later step fail.
func vfioNetdevUp(ctx context.Context, deviceID string) (netlink.Link, error) {
	index, err := getNetdevIndex(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	link, err := util.GetNetLinkOps().LinkByIndex(index)
	if err != nil {
		return nil, fmt.Errorf("failed to look up netdev %d of device %s: %w", index, deviceID, err)
	}
	if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to bring up netdev %s of device %s: %w", link.Attrs().Name, deviceID, err)
	}
	return link, nil
}

// vfioHandoffJournal records who started the handoff, for operator debugging;
// the heal decision needs only the file's existence (the intended owner is
// always vfio-pci, the handoff's precondition).
type vfioHandoffJournal struct {
	IntendedDriver string `json:"intendedDriver"`
	PodNamespace   string `json:"podNamespace"`
	PodName        string `json:"podName"`
	SandboxID      string `json:"sandboxID"`
}

func vfioHandoffJournalPath(deviceID string) string {
	return filepath.Join(vfioHandoffJournalDir, deviceID+".json")
}

// writeVFIOHandoffJournal is fatal on failure, without the journal a crash
// mid-handoff is unrecoverable and so the handoff must not start.
func (pr *PodRequest) writeVFIOHandoffJournal(deviceID string) error {
	if err := os.MkdirAll(vfioHandoffJournalDir, 0o700); err != nil {
		return fmt.Errorf("failed to create the VFIO handoff journal dir %q: %w", vfioHandoffJournalDir, err)
	}
	data, err := json.Marshal(vfioHandoffJournal{
		IntendedDriver: vfioPCIDriver,
		PodNamespace:   pr.PodNamespace,
		PodName:        pr.PodName,
		SandboxID:      pr.SandboxID,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal the VFIO handoff journal of device %s: %w", deviceID, err)
	}
	if err := os.WriteFile(vfioHandoffJournalPath(deviceID), data, 0o600); err != nil {
		return fmt.Errorf("failed to write the VFIO handoff journal of device %s: %w", deviceID, err)
	}
	return nil
}

// removeVFIOHandoffJournal is warn-only, a leftover journal with the device
// already on vfio-pci is recognized and cleaned up by the next heal.
func removeVFIOHandoffJournal(deviceID string) {
	if err := os.Remove(vfioHandoffJournalPath(deviceID)); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Failed to remove the VFIO handoff journal of device %s: %v", deviceID, err)
	}
}

// runLocalnetVFIODHCP acquires a DHCP lease for a VFIO VF on a localnet network.
// It temporarily unbinds the VF from vfio-pci and rebinds it to the kernel
// driver matching its hardware (detected from the PCI vendor ID) to get a
// netdev, performs a DHCP DORA exchange to discover the IP, then rebinds to
// vfio-pci. VFs of unsupported NIC vendors are rejected before any driver
// state is touched.
//
// The probe lease is deliberately kept: the guest's DHCP client, presenting
// the same MAC, re-acquires the same IP at boot. It is released only when
// the rebind fails, as no VM will boot to take it over.
func (pr *PodRequest) runLocalnetVFIODHCP() (dhcpResult *current.Result, retErr error) {
	deviceID := pr.CNIConf.DeviceID

	currentDriver, err := getPCIDeviceDriver(deviceID)
	if err != nil {
		return nil, err
	}
	if currentDriver != vfioPCIDriver {
		return nil, fmt.Errorf("pci device %s is not bound to %s, found %q",
			deviceID, vfioPCIDriver, currentDriver)
	}

	// Detect the kernel driver matching this VF's hardware before any driver
	// state is touched. An unsupported vendor must fail cleanly here, not
	// halfway through an unbind/bind cycle with a raw sysfs error.
	kernelDriver, err := getVfKernelDriverForDevice(deviceID)
	if err != nil {
		return nil, err
	}

	klog.Infof("VFIO DHCP: acquiring lease for pod %s/%s device %s via driver %s",
		pr.PodNamespace, pr.PodName, deviceID, kernelDriver)

	var probeLease *nclient4.Lease
	var probeMAC net.HardwareAddr

	// Always restore vfio-pci on the way out, no matter where we fail below.
	// This must be registered before the driver swap is attempted, because the swap
	// unbinds from vfio-pci before binding the kernel driver, so a failure
	// halfway leaves the VF bound to nothing. Rebinding is a no-op when the
	// device is still (or already back) on vfio-pci.
	defer func() {
		if err := dhcpOps.BindPCIDeviceDriver(deviceID, vfioPCIDriver); err != nil {
			if retErr != nil {
				retErr = fmt.Errorf("%w; rebind to %s failed: %v",
					retErr, vfioPCIDriver, err)
			} else {
				retErr = fmt.Errorf("rebind device %s to %s failed: %w",
					deviceID, vfioPCIDriver, err)
			}
			dhcpResult = nil
			// kubelet retries this ADD regardless, and the same-MAC re-acquire makes an unreleased
			// lease self-correcting.
			if probeLease != nil {
				releaseVFIOProbeLease(pr.ctx, deviceID, probeLease, probeMAC)
			}
		} else {
			klog.Infof("VFIO DHCP: device %s rebound to %s",
				deviceID, vfioPCIDriver)
			// Restore the pin (BindPCIDeviceDriver clears driver_override);
			// the pin is how SR-IOV tooling keeps vfio devices bound across
			// reprobes. A failed pin write means the device is not fully
			// handed back yet, so fail this ADD and keep the journal: the
			// retried ADD then restores the pin through healInterruptedVFIOHandoff.
			if err := writePCIDeviceDriverOverride(deviceID, vfioPCIDriver); err != nil {
				pinErr := fmt.Errorf("failed to restore the %s pin on device %s: %w",
					vfioPCIDriver, deviceID, err)
				if retErr != nil {
					retErr = fmt.Errorf("%w; additionally: %v", retErr, pinErr)
				} else {
					retErr = pinErr
				}
				dhcpResult = nil
				return
			}
			// the device is back on vfio-pci with its pin and the handoff is
			// concluded
			removeVFIOHandoffJournal(deviceID)
		}
	}()

	// Record the handoff journal before touching any driver state: it is
	// what lets a retried ADD detect and repair an interrupted handoff, so
	// no journal means no handoff. The deferred rebind above removes it
	// once the device is back on vfio-pci.
	if err := pr.writeVFIOHandoffJournal(deviceID); err != nil {
		return nil, fmt.Errorf("refusing to start the VFIO DHCP handoff for device %s: %w",
			deviceID, err)
	}

	// Unbind from vfio-pci, bind to the hardware-matching kernel driver to
	// get a netdev
	if err := dhcpOps.BindPCIDeviceDriver(deviceID, kernelDriver); err != nil {
		return nil, fmt.Errorf("failed to bind device %s to %s: %w",
			deviceID, kernelDriver, err)
	}
	link, err := vfioNetdevUp(pr.ctx, deviceID)
	if err != nil {
		return nil, err
	}
	probeMAC = link.Attrs().HardwareAddr
	klog.Infof("VFIO DHCP: device %s netdev %s (index %d) MAC %s is up, starting DHCP",
		deviceID, link.Attrs().Name, link.Attrs().Index, probeMAC)

	// The netdev carries the VM's MAC (set on the VF by ConfigureInterface),
	// so the DHCP server sees the guest's identity and leases it the VM's IP.
	dhcpResult, probeLease, err = dhcpOps.AcquireLease(pr.ctx, link)
	if err != nil {
		return nil, fmt.Errorf("DHCP lease acquisition failed on device %s: %w", deviceID, err)
	}

	klog.Infof("VFIO DHCP: pod %s/%s device %s got IP=%s gw=%s",
		pr.PodNamespace, pr.PodName, deviceID,
		dhcpResult.IPs[0].Address.String(), dhcpResult.IPs[0].Gateway)

	return dhcpResult, nil
}

// doOneShotDHCP performs a single DHCP DORA to discover the externally-assigned
// IP for a KubeVirt VM workload and returns it for reporting (CNI result,
// pod-networks annotation and NBDB programming). The IP is never applied to the
// interface:
//   - VFIO passthrough: the VF is rebound to vfio-pci afterwards, so any address
//     configured on the temporary netdev would be discarded.
//   - non-VFIO (l2bridge/managedTap): applying the IP would start KubeVirt's
//     in-pod dnsmasq, which conflicts with the external DHCP server and prevents
//     the guest from renewing its lease.
//
// In both cases the guest runs its own DHCP client after boot and owns the lease
// lifecycle; no lease maintenance is done.
func (pr *PodRequest) doOneShotDHCP() (*current.Result, error) {
	if pr.shouldRunLocalnetVFIODriverHandoff() {
		return pr.runLocalnetVFIODHCP()
	}
	return pr.acquireOneShotDHCPInPodNetns()
}

// acquireOneShotDHCPInPodNetns performs the one-shot DHCP DORA on the pod-netns
// interface for a non-VFIO KubeVirt VM (l2bridge/managedTap binding).
func (pr *PodRequest) acquireOneShotDHCPInPodNetns() (*current.Result, error) {
	netns, err := ns.GetNS(pr.Netns)
	if err != nil {
		return nil, fmt.Errorf("failed to open netns %q: %w", pr.Netns, err)
	}
	defer netns.Close()

	var dhcpResult *current.Result
	if err := netns.Do(func(_ ns.NetNS) error {
		link, err := util.GetNetLinkOps().LinkByName(pr.IfName)
		if err != nil {
			return fmt.Errorf("failed to find interface %s: %w", pr.IfName, err)
		}
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to bring up %s: %w", pr.IfName, err)
		}
		// KubeVirt's bridge binding hands this interface's MAC to the guest,
		// so the lease is keyed to the guest's DHCP identity and is handed
		// over at boot, not leaked. Releasing it here would only open a
		// window for the pool to reassign the IP before the guest boots.
		dhcpResult, _, err = acquireDHCPLease(pr.ctx, link)
		if err != nil {
			return fmt.Errorf("DHCP lease acquisition failed on %s: %w", pr.IfName, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	klog.Infof("VM DHCP: pod %s/%s iface %s discovered IP=%s gw=%s (reported only, not applied)",
		pr.PodNamespace, pr.PodName, pr.IfName,
		dhcpResult.IPs[0].Address.String(), dhcpResult.IPs[0].Gateway)

	return dhcpResult, nil
}

// mergeDHCPResultIntoCNIResult copies the DHCP-discovered IPs and routes into the
// CNI result.
func mergeDHCPResultIntoCNIResult(dhcpResult, result *current.Result) {
	// Point the IPs at the container-side interface (the one with a sandbox
	// path). Defensively leave ips[].interface unset
	// when none is found, as the CNI spec makes it optional.
	var contIfIdx *int
	for i, iface := range result.Interfaces {
		if iface.Sandbox != "" {
			contIfIdx = current.Int(i)
			break
		}
	}
	for _, ipConfig := range dhcpResult.IPs {
		ipConfig.Interface = contIfIdx
		result.IPs = append(result.IPs, ipConfig)
	}
	result.Routes = append(result.Routes, dhcpResult.Routes...)
}

// dhcpClientID returns the RFC 2132 Client-ID (option 61) for the given
// hardware address: a 1-byte hardware type (0x01 = Ethernet) followed by the
// address itself.
func dhcpClientID(hwAddr net.HardwareAddr) []byte {
	return append([]byte{0x01}, hwAddr...)
}

// openRawDHCPConn opens a raw IPv4 socket bound to the netdev with ifindex
// index. Tests replace it, as a raw socket needs privileges.
var openRawDHCPConn = func(index int) (net.PacketConn, error) {
	return packet.Listen(&net.Interface{Index: index}, packet.Datagram, unix.ETH_P_IP, nil)
}

// newDHCPClient creates a raw-socket DHCP client on link. It binds the socket
// to the link's ifindex, not its name, so a udev rename cannot make it fail.
func newDHCPClient(link netlink.Link, opts ...nclient4.ClientOpt) (*nclient4.Client, error) {
	attrs := link.Attrs()
	rawConn, err := openRawDHCPConn(attrs.Index)
	if err != nil {
		return nil, err
	}
	conn := nclient4.NewBroadcastUDPConn(rawConn, &net.UDPAddr{Port: nclient4.ClientPort})
	client, err := nclient4.NewWithConn(conn, attrs.HardwareAddr, opts...)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// acquireDHCPLease performs a DHCP DORA (Discover, Offer, Request, Ack)
// exchange on the given interface and returns the result together with the
// underlying lease.
//
// The Client-ID (option 61) is set to the RFC 2132 MAC form (0x01 + MAC),
// the same as common guest DHCP clients present, so the server keys
// this lease and the guest's later requests identically, and the guest
// renews this very lease instead of being allocated a different IP.
// Additional DHCP options for the exchange may be passed via extraOpts.
func acquireDHCPLease(ctx context.Context, link netlink.Link,
	extraOpts ...dhcpv4.Modifier) (*current.Result, *nclient4.Lease, error) {
	ifName, hwAddr := link.Attrs().Name, link.Attrs().HardwareAddr
	client, err := newDHCPClient(link,
		nclient4.WithTimeout(vmDHCPTimeout),
		nclient4.WithRetry(vmDHCPRetries),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create DHCP client on %s: %w", ifName, err)
	}
	defer client.Close()

	opts := append([]dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(dhcpClientID(hwAddr))),
	}, extraOpts...)
	lease, err := client.Request(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("DHCP request failed on %s: %w", ifName, err)
	}

	if lease.ACK == nil {
		return nil, nil, fmt.Errorf("DHCP lease has no ACK")
	}

	result, err := dhcpACKToCNIResult(ifName, lease.ACK)
	if err != nil {
		return nil, nil, err
	}
	return result, lease, nil
}

// releaseDHCPLease sends a DHCPRELEASE for the lease on the given interface.
// The leased address is configured transiently on link because per RFC 2131
// the release must be unicast from the leased address, and the Client-ID
// (option 61) is included because the server matched the lease on it.
func releaseDHCPLease(link netlink.Link, lease *nclient4.Lease, hwAddr net.HardwareAddr) error {
	ifName := link.Attrs().Name
	clientID := dhcpv4.WithOption(dhcpv4.OptClientIdentifier(dhcpClientID(hwAddr)))
	leasedIP := lease.ACK.YourIPAddr

	if mask := lease.ACK.SubnetMask(); mask != nil {
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: leasedIP, Mask: mask}}
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err == nil {
			defer func() { _ = util.GetNetLinkOps().AddrDel(link, addr) }()
			// a UDP socket bound to the leased address needs no interface
			c, err := nclient4.NewWithConn(nil, link.Attrs().HardwareAddr,
				nclient4.WithUnicast(&net.UDPAddr{IP: leasedIP, Port: nclient4.ClientPort}))
			if err == nil {
				defer c.Close()
				if err := c.Release(lease, clientID); err == nil {
					return nil
				}
			}
		}
	}

	// if the unicast release failed, fall back to a legacy raw-socket
	// release (source 0.0.0.0, L2 broadcast), which lenient servers accept.
	c, err := newDHCPClient(link)
	if err != nil {
		return fmt.Errorf("failed to create DHCP client on %s: %w", ifName, err)
	}
	defer c.Close()
	if err := c.Release(lease, clientID); err != nil {
		return fmt.Errorf("DHCP release failed on %s: %w", ifName, err)
	}
	return nil
}

// releaseVFIOProbeLease sends a best-effort DHCPRELEASE for the probe lease
// when the rebind to vfio-pci failed and no VM will boot to take the lease
// over. A failed release is only logged as a retried ADD, presenting the
// same MAC-keyed client-id, re-acquires this same lease, or it expires at
// TTL. When the bind fails, bindPCIDeviceDriver restores the kernel driver,
// whose new netdev starts down, so the release brings it up first and takes
// it down again afterward: the failed ADD must not leave a host interface up
// on the VM's network.
func releaseVFIOProbeLease(ctx context.Context, deviceID string, lease *nclient4.Lease, hwAddr net.HardwareAddr) {
	link, err := vfioNetdevUp(ctx, deviceID)
	if err == nil {
		err = dhcpOps.ReleaseLease(link, lease, hwAddr)
		if downErr := util.GetNetLinkOps().LinkSetDown(link); downErr != nil {
			klog.Warningf("VFIO DHCP: failed to bring down netdev %s of device %s after the lease release: %v",
				link.Attrs().Name, deviceID, downErr)
		}
	}
	if err != nil {
		klog.Warningf("VFIO DHCP: best-effort release of lease %s for device %s failed: %v",
			lease.ACK.YourIPAddr, deviceID, err)
	}
}

// dhcpACKToCNIResult converts a DHCPACK into a CNI result, following the
// upstream dhcp IPAM plugin's behavior:
// - A missing or malformed subnet mask (option 1) is an error rather than a guessed prefix,
// - The first Router (option 3) is the gateway, and per RFC 3442 the option-121
// routes, when present, are the entire route set, the Router only becomes
// a default route without them.
// - The deprecated classful Static Routes option (33) is not parsed.
func dhcpACKToCNIResult(ifName string, ack *dhcpv4.DHCPv4) (*current.Result, error) {
	ip := ack.YourIPAddr
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("DHCP ACK on %s has no IP address", ifName)
	}

	mask := ack.SubnetMask()
	if mask == nil {
		return nil, fmt.Errorf("DHCP option Subnet Mask not found in DHCPACK on %s", ifName)
	}

	// Extract the gateway (option 3) with the library parser, which
	// validates the option layout. Servers may return several routers,
	// so use the first, as standard clients do.
	var gateway net.IP
	if routers := ack.Router(); len(routers) > 0 {
		gateway = routers[0]
	}

	result := &current.Result{
		IPs: []*current.IPConfig{{
			Address: net.IPNet{IP: ip, Mask: mask},
			Gateway: gateway,
		}},
	}

	// Extract classless static routes (option 121). The library parser
	// rejects malformed option data (e.g. a prefix length > 32) as a whole,
	// so a broken option yields no routes instead of bogus ones.
	result.Routes = classlessRoutesToCNIRoutes(ack.ClasslessStaticRoute())
	if len(result.Routes) == 0 && gateway != nil {
		result.Routes = append(result.Routes, &cnitypes.Route{
			Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			GW:  gateway,
		})
	}

	return result, nil
}

// classlessRoutesToCNIRoutes converts RFC 3442 classless routes into CNI
// routes. A 0.0.0.0 router means the destination is on-link, so the route
// gets SCOPE_LINK, as the delegated dhcp IPAM plugin does.
func classlessRoutesToCNIRoutes(routes []*dhcpv4.Route) []*cnitypes.Route {
	var cniRoutes []*cnitypes.Route
	for _, r := range routes {
		route := &cnitypes.Route{Dst: *r.Dest, GW: r.Router}
		if r.Router.IsUnspecified() {
			scope := int(netlink.SCOPE_LINK)
			route.Scope = &scope
		}
		cniRoutes = append(cniRoutes, route)
	}
	return cniRoutes
}
