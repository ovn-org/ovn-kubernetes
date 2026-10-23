// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/moby/sys/userns"
	"golang.org/x/sys/unix"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"sigs.k8s.io/knftables"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// udn-isolation chain contains rules for udn isolation from the host side.
	UDNIsolationChain = "udn-isolation"
	// nftables set names
	nftablesUDNOpenPortsv4     = "udn-open-ports-v4"
	nftablesUDNOpenPortsv6     = "udn-open-ports-v6"
	nftablesUDNOpenPortsICMPv4 = "udn-open-ports-icmp-v4"
	nftablesUDNOpenPortsICMPv6 = "udn-open-ports-icmp-v6"
	nftablesUDNPodIPsv4        = "udn-pod-default-ips-v4"
	nftablesUDNPodIPsv6        = "udn-pod-default-ips-v6"
	// name of the cgroup directory kubelet runs under when it is managed by systemd.
	kubeletCgroupName = "kubelet.service"
	// throwaway chain used to check kernel support for the socket cgroupv2 match. It
	// only exists inside the checking transaction.
	socketMatchProbeChain = "udn-isolation-socket-probe"
)

// UDNHostIsolationManager manages the host isolation for user defined networks.
// It uses nftables chain "udn-isolation" to only allow connection to primary UDN pods from kubelet.
// It also listens to systemd events to re-apply the rules after kubelet restart as cgroup matching is used.
type UDNHostIsolationManager struct {
	nft               knftables.Interface
	ipv4, ipv6        bool
	podController     controller.Controller
	podLister         corelisters.PodLister
	kubeletCgroupPath string
	nodeName          string
	recorder          record.EventRecorder

	udnPodIPsv4 *nftPodElementsSet
	udnPodIPsv6 *nftPodElementsSet

	udnOpenPortsv4 *nftPodElementsSet
	udnOpenPortsv6 *nftPodElementsSet

	udnOpenPortsICMPv4 *nftPodElementsSet
	udnOpenPortsICMPv6 *nftPodElementsSet
}

func NewUDNHostIsolationManager(ipv4, ipv6 bool, podInformer coreinformers.PodInformer, nodeName string, recorder record.EventRecorder) *UDNHostIsolationManager {
	m := &UDNHostIsolationManager{
		podLister:          podInformer.Lister(),
		ipv4:               ipv4,
		ipv6:               ipv6,
		nodeName:           nodeName,
		recorder:           recorder,
		udnPodIPsv4:        newNFTPodElementsSet(nftablesUDNPodIPsv4, false),
		udnPodIPsv6:        newNFTPodElementsSet(nftablesUDNPodIPsv6, false),
		udnOpenPortsv4:     newNFTPodElementsSet(nftablesUDNOpenPortsv4, true),
		udnOpenPortsv6:     newNFTPodElementsSet(nftablesUDNOpenPortsv6, true),
		udnOpenPortsICMPv4: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv4, false),
		udnOpenPortsICMPv6: newNFTPodElementsSet(nftablesUDNOpenPortsICMPv6, false),
	}
	controllerConfig := &controller.ControllerConfig[corev1.Pod]{
		Informer:       podInformer.Informer(),
		Lister:         podInformer.Lister().List,
		ObjNeedsUpdate: podNeedsUpdate,
		Reconcile:      m.reconcilePod,
		Threadiness:    1,
	}
	m.podController = controller.NewController[corev1.Pod]("udn-host-isolation-manager", controllerConfig)
	return m
}

// findKubeletCgroupPath looks for the kubelet cgroup under the given cgroup root and
// returns it relative to that root.
// kind cluster uses "kubelet.slice/kubelet.service", while OCP cluster uses "system.slice/kubelet.service".
// as long as ovn-k node is running as a privileged container, we can access the host cgroup directory.
func findKubeletCgroupPath(cgroupRoot string) (string, error) {
	var cgroupPath string
	err := filepath.WalkDir(cgroupRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == kubeletCgroupName {
			relPath, err := filepath.Rel(cgroupRoot, path)
			if err != nil {
				return err
			}
			cgroupPath = relPath
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to walk %s: %w", cgroupRoot, err)
	}
	if cgroupPath == "" {
		return "", fmt.Errorf("no %q directory found under %s", kubeletCgroupName, cgroupRoot)
	}
	return cgroupPath, nil
}

// socketCgroupv2MatchSupported reports whether the kernel accepts the socket cgroupv2
// match that lets kubelet reach primary UDN pods. The match is loaded in a throwaway
// chain that is created and removed in a single transaction, so nothing is left behind
// whether or not it is supported. A kernel built without CONFIG_NFT_SOCKET has no socket
// expression at all and rejects the rule.
// The check uses the same match the isolation rules install, so support is decided for
// the exact rule rather than an approximation of it.
func (m *UDNHostIsolationManager) socketCgroupv2MatchSupported(cgroupPath string) error {
	tx := m.nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     socketMatchProbeChain,
		Comment:  knftables.PtrTo("Transient chain, checks kernel support for the socket cgroupv2 match"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})
	tx.Add(&knftables.Rule{
		Chain: socketMatchProbeChain,
		Rule:  knftables.Concat(kubeletCgroupMatch(cgroupPath), "accept"),
	})
	tx.Delete(&knftables.Chain{
		Name: socketMatchProbeChain,
	})
	return m.nft.Run(context.TODO(), tx)
}

// kubeletCgroupPathToUse returns the cgroup path to match kubelet traffic on, relative
// to the cgroup root. A configured path is used as given, which is the only option on
// hosts that do not run kubelet under systemd. Otherwise the kubelet.service cgroup is
// looked up, which keeps hosts that do work without configuration.
func (m *UDNHostIsolationManager) kubeletCgroupPathToUse(cgroupRoot string) (string, error) {
	if configured := config.OvnKubeNode.KubeletCgroupPath; configured != "" {
		info, err := os.Stat(filepath.Join(cgroupRoot, configured))
		if err != nil {
			return "", fmt.Errorf("configured kubelet cgroup path %q is not present under %s: %w",
				configured, cgroupRoot, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("configured kubelet cgroup path %q under %s is not a cgroup directory",
				configured, cgroupRoot)
		}
		klog.Infof("Using configured kubelet cgroup path: %s", configured)
		return configured, nil
	}
	return findKubeletCgroupPath(cgroupRoot)
}

// setKubeletCgroupPath stores the cgroup kubelet runs under in m.kubeletCgroupPath.
// When it cannot be resolved, kubelet probes to primary UDN pods are reported as
// unsupported and the path is left empty, which leaves the kubelet rule out of the
// isolation chain. Host isolation itself is unaffected.
func (m *UDNHostIsolationManager) setKubeletCgroupPath(cgroupRoot string) {
	if !hostUsesCgroupv2() {
		m.reportKubeletProbesUnsupported("the node uses cgroup v1")
		return
	}
	path, err := m.kubeletCgroupPathToUse(cgroupRoot)
	if err != nil {
		m.reportKubeletProbesUnsupported(err.Error())
		return
	}
	if err := m.socketCgroupv2MatchSupported(path); err != nil {
		m.reportKubeletProbesUnsupported(fmt.Sprintf("the kernel does not support the nftables socket cgroupv2 match: %v", err))
		return
	}
	klog.Infof("Found kubelet cgroup path: %s", path)
	klog.Warningf("Every process in cgroup %s is allowed to reach primary UDN pods", path)
	m.kubeletCgroupPath = path
}

// reportKubeletProbesUnsupported logs and reports on the node that kubelet probes to
// primary UDN pods won't be allowed. Host isolation itself is unaffected.
func (m *UDNHostIsolationManager) reportKubeletProbesUnsupported(reason string) {
	message := fmt.Sprintf("Kubelet probes to primary UDN pods are not supported on node %s: %s. "+
		"Use the %s pod annotation to allow the probe ports from the host.",
		m.nodeName, reason, util.UDNOpenPortsAnnotationName)
	klog.Warning(message)
	m.recordNodeWarning("UDNKubeletProbesNotSupported", message)
}

func (m *UDNHostIsolationManager) recordNodeWarning(reason, message string) {
	nodeRef := &corev1.ObjectReference{
		Kind: "Node",
		Name: m.nodeName,
	}
	m.recorder.Eventf(nodeRef, kapi.EventTypeWarning, reason, "%s", message)
}

// Start must be called on node setup.
func (m *UDNHostIsolationManager) Start(ctx context.Context) error {
	klog.Infof("Starting UDN host isolation manager")
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}
	m.nft = nft

	// A failure to resolve the kubelet cgroup is not fatal: host isolation is still set
	// up, only the rule that lets kubelet reach primary UDN pods is left out.
	m.setKubeletCgroupPath(unifiedMountpoint)

	if err = m.setupUDNIsolationFromHost(); err != nil {
		return fmt.Errorf("failed to setup UDN host isolation: %w", err)
	}
	// The tracker only exists to re-resolve the kubelet cgroup match, so it is pointless
	// without one.
	if m.kubeletCgroupPath != "" {
		err = m.runKubeletRestartTracker(ctx)
		switch {
		case errors.Is(err, errNoSystemdUnit):
			// expected on a host that does not run kubelet under systemd, where there
			// is no unit to watch. The match stays in place but is not refreshed.
			message := fmt.Sprintf("Kubelet restarts are not tracked on node %s: %v. "+
				"Kubelet probes to primary UDN pods will stop working if kubelet is restarted.",
				m.nodeName, err)
			klog.Warning(message)
			m.recordNodeWarning("UDNKubeletRestartTrackingUnavailable", message)
		case err != nil:
			return fmt.Errorf("failed to run kubelet restart tracker: %w", err)
		}
	}
	return controller.StartWithInitialSync(m.podInitialSync, m.podController)
}

func (m *UDNHostIsolationManager) Stop() {
	controller.Stop(m.podController)
}

// CleanupUDNHostIsolation removes all nftables chains and sets created by UDNHostIsolationManager.
func CleanupUDNHostIsolation() error {
	nft, err := nodenft.GetNFTablesHelper()
	if err != nil {
		return fmt.Errorf("failed getting nftables helper: %w", err)
	}
	tx := nft.NewTransaction()
	safeDelete(tx, &knftables.Chain{
		Name: UDNIsolationChain,
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNPodIPsv4,
		Type: "ipv4_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNPodIPsv6,
		Type: "ipv6_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsv4,
		Type: "ipv4_addr . inet_proto . inet_service",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsv6,
		Type: "ipv6_addr . inet_proto . inet_service",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsICMPv4,
		Type: "ipv4_addr",
	})
	safeDelete(tx, &knftables.Set{
		Name: nftablesUDNOpenPortsICMPv6,
		Type: "ipv6_addr",
	})
	return nft.Run(context.TODO(), tx)
}

func (m *UDNHostIsolationManager) setupUDNIsolationFromHost() error {
	tx := m.nft.NewTransaction()
	tx.Add(&knftables.Chain{
		Name:     UDNIsolationChain,
		Comment:  knftables.PtrTo("Host isolation for user defined networks"),
		Type:     knftables.PtrTo(knftables.FilterType),
		Hook:     knftables.PtrTo(knftables.OutputHook),
		Priority: knftables.PtrTo(knftables.FilterPriority),
	})
	tx.Flush(&knftables.Chain{
		Name: UDNIsolationChain,
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsv4,
		Comment: knftables.PtrTo("default network open ports of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsv6,
		Comment: knftables.PtrTo("default network open ports of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr . inet_proto . inet_service",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsICMPv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks that allow ICMP (IPv4)"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNOpenPortsICMPv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks that allow ICMP (IPv6)"),
		Type:    "ipv6_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv4,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv4)"),
		Type:    "ipv4_addr",
	})
	tx.Add(&knftables.Set{
		Name:    nftablesUDNPodIPsv6,
		Comment: knftables.PtrTo("default network IPs of pods in user defined networks (IPv6)"),
		Type:    "ipv6_addr",
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not setup nftables rules for UDN from host isolation: %v", err)
	}
	return nil
}

// cgroupv2Level returns the "level" argument of the nftables socket cgroupv2 match for
// the given cgroup path. The match compares the cgroup ancestor at that level, so the
// level has to be the depth of the path, e.g. 2 for "kubelet.slice/kubelet.service".
func cgroupv2Level(cgroupPath string) string {
	return fmt.Sprintf("level %d", strings.Count(cgroupPath, "/")+1)
}

// quoteCgroupPath renders the cgroup path as a quoted nftables string. Cgroup directory
// names may contain characters nft would otherwise read as syntax, most importantly "@",
// which starts a set reference. Backslashes and quotes are escaped so a path cannot
// terminate the string early.
func quoteCgroupPath(cgroupPath string) string {
	escaped := strings.ReplaceAll(cgroupPath, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// kubeletCgroupMatch returns the nftables match that selects traffic originating from
// the kubelet cgroup. Both the isolation rules and the kernel support check build the
// match here, so what is checked is exactly what is installed.
func kubeletCgroupMatch(cgroupPath string) []string {
	return []string{"socket", "cgroupv2", cgroupv2Level(cgroupPath), quoteCgroupPath(cgroupPath)}
}

func (m *UDNHostIsolationManager) addRules(tx *knftables.Transaction) {
	if m.ipv4 {
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", ".", "meta l4proto", ".", "th dport",
				"@", nftablesUDNOpenPortsv4, "accept",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", "@", nftablesUDNOpenPortsICMPv4, "meta l4proto", "icmp",
				"accept",
			),
		})

		if m.kubeletCgroupPath != "" {
			tx.Add(&knftables.Rule{
				Chain: UDNIsolationChain,
				Rule: knftables.Concat(
					kubeletCgroupMatch(m.kubeletCgroupPath),
					"ip", "daddr", "@", nftablesUDNPodIPsv4, "accept"),
			})
		}

		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip", "daddr", "@", nftablesUDNPodIPsv4, "drop"),
		})
	}
	if m.ipv6 {
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", ".", "meta l4proto", ".", "th dport",
				"@", nftablesUDNOpenPortsv6, "accept",
			),
		})
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", "@", nftablesUDNOpenPortsICMPv6, "meta l4proto", "icmpv6",
				"accept",
			),
		})
		if m.kubeletCgroupPath != "" {
			tx.Add(&knftables.Rule{
				Chain: UDNIsolationChain,
				Rule: knftables.Concat(
					kubeletCgroupMatch(m.kubeletCgroupPath),
					"ip6", "daddr", "@", nftablesUDNPodIPsv6, "accept"),
			})
		}
		tx.Add(&knftables.Rule{
			Chain: UDNIsolationChain,
			Rule: knftables.Concat(
				"ip6", "daddr", "@", nftablesUDNPodIPsv6, "drop"),
		})
	}
}

func (m *UDNHostIsolationManager) updateKubeletCgroup() error {
	tx := m.nft.NewTransaction()
	tx.Flush(&knftables.Chain{
		Name: UDNIsolationChain,
	})
	m.addRules(tx)

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables rule for management port: %v", err)
	}
	return nil
}

// systemdUnitSuffixes are the unit types kubelet can run as. A cgroup path that ends
// in neither is a plain cgroup, such as a slice or an ancestor, which no single unit
// corresponds to.
var systemdUnitSuffixes = []string{".service", ".scope"}

// errNoSystemdUnit is returned when the kubelet cgroup does not belong to a systemd
// unit, which is the case on a host that does not run kubelet under systemd. It is the
// one tracker failure that is expected rather than a sign that something is wrong.
var errNoSystemdUnit = errors.New("cgroup does not belong to a systemd unit")

// kubeletSystemdUnit derives the systemd unit that owns the given kubelet cgroup path.
// The unit is the last component of the path when it names one, e.g. "kubelet.service"
// for "kubelet.slice/kubelet.service".
func kubeletSystemdUnit(cgroupPath string) (string, error) {
	unit := filepath.Base(cgroupPath)
	for _, suffix := range systemdUnitSuffixes {
		if strings.HasSuffix(unit, suffix) {
			return unit, nil
		}
	}
	return "", fmt.Errorf("%q %w", cgroupPath, errNoSystemdUnit)
}

// decodeSystemdUnitName decodes a systemd unit name taken from a D-Bus object path.
// systemd escapes every character outside [A-Za-z0-9] as an underscore followed by two
// hex digits, so "custom-kubelet.service" arrives as "custom_2dkubelet_2eservice".
// Anything that is not a complete escape sequence is left as it is.
func decodeSystemdUnitName(escapedUnit string) string {
	var unitName strings.Builder
	for i := 0; i < len(escapedUnit); i++ {
		if escapedUnit[i] == '_' && i+2 < len(escapedUnit) {
			if decoded, err := strconv.ParseUint(escapedUnit[i+1:i+3], 16, 8); err == nil {
				unitName.WriteByte(byte(decoded))
				i += 2
				continue
			}
		}
		unitName.WriteByte(escapedUnit[i])
	}
	return unitName.String()
}

// handleKubeletRestartSignal re-applies the isolation rules when a signal reports the
// kubelet unit becoming active again. The systemd private connection delivers every
// signal rather than a filtered subset, so each field is checked before use: an
// unexpected shape would otherwise panic the tracker goroutine.
func (m *UDNHostIsolationManager) handleKubeletRestartSignal(signal *dbus.Signal, kubeletUnit string) {
	parts := strings.Split(string(signal.Path), "/")
	if len(parts) < 6 || parts[4] != "unit" {
		return
	}
	if decodeSystemdUnitName(parts[5]) != kubeletUnit || len(signal.Body) < 2 {
		return
	}
	changes, ok := signal.Body[1].(map[string]dbus.Variant)
	if !ok {
		return
	}
	state, exists := changes["ActiveState"]
	if !exists {
		return
	}
	if newState, ok := state.Value().(string); !ok || newState != "active" {
		return
	}
	klog.Info("Kubelet restarted, re-applying isolation")
	if err := m.updateKubeletCgroup(); err != nil {
		klog.Errorf("Failed to re-apply isolation: %v", err)
	}
}

// runKubeletRestartTracker listens to systemd events to re-apply the UDN host isolation rules after kubelet restart.
// cgroupv2 match doesn't actually match cgroup paths, but rather resolves them to numeric cgroup IDs when such
// rules are loaded into kernel, and does not automatically update them in any way afterwards.
// From the patch https://patchwork.ozlabs.org/project/netfilter-devel/patch/1479114761-19534-1-git-send-email-pablo@netfilter.org/#1511797:
// If the cgroup is gone, the filtering policy would not match anymore. You only have to subscribe to events
// and perform an incremental updates to tear down the side of the filtering policy that you don't need anymore.
// If a new cgroup is created, you load the filtering policy for the new cgroup and then add
// processes to that cgroup. You only have to follow the right sequence to avoid problems.
func (m *UDNHostIsolationManager) runKubeletRestartTracker(ctx context.Context) (err error) {
	// the tracker follows a systemd unit, so it can only be used when the kubelet
	// cgroup belongs to one.
	kubeletUnit, err := kubeletSystemdUnit(m.kubeletCgroupPath)
	if err != nil {
		return err
	}

	conn, err := dbus.Dial("unix:path=/run/systemd/private", dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("failed to connect to systemd: %w", err)
	}

	defer func() {
		if err != nil {
			if err := conn.Close(); err != nil {
				klog.Errorf("Error closing dbus connection for UDN isolation: %v", err)
			}
		}
	}()

	// Only use EXTERNAL method, and hardcode the uid (not username)
	// to avoid a username lookup (which requires a dynamically linked
	// libc)
	methods := []dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}

	err = conn.Auth(methods)
	if err != nil {
		return err
	}

	signalChan := make(chan *dbus.Signal, 100)
	conn.Signal(signalChan)

	// run until context is cancelled
	go func() {
		for {
			select {
			case <-ctx.Done():
				if err := conn.Close(); err != nil {
					klog.Errorf("Error closing dbus connection for UDN isolation: %v", err)
				}
				return
			case signal, ok := <-signalChan:
				if !ok || signal == nil {
					// Channel was closed, connection is shutting down
					return
				}
				klog.V(5).Infof("D-Bus event received: %#v", signal)
				m.handleKubeletRestartSignal(signal, kubeletUnit)
			}
		}
	}()

	conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		"type='signal',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged'")

	sigObj := conn.Object("org.freedesktop.systemd1", dbus.ObjectPath("/org/freedesktop/systemd1"))
	if err := sigObj.Call("org.freedesktop.systemd1.Manager.Subscribe", 0).Store(); err != nil {
		return fmt.Errorf("failed to subscribe to systemd: %w", err)
	}

	return nil
}

func (m *UDNHostIsolationManager) podInitialSync() error {
	udnPodIPsv4 := map[string]sets.Set[string]{}
	udnPodIPsv6 := map[string]sets.Set[string]{}
	udnOpenPortsICMPv4 := map[string]sets.Set[string]{}
	udnOpenPortsICMPv6 := map[string]sets.Set[string]{}
	udnOpenPortsv4 := map[string]sets.Set[string]{}
	udnOpenPortsv6 := map[string]sets.Set[string]{}

	pods, err := m.podLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list pods: %v", err)
	}

	for _, pod := range pods {
		podKey, err := cache.MetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Warningf("UDNHostIsolationManager failed to get key for pod %s in namespace %s: %v", pod.Name, pod.Namespace, err)
			continue
		}
		// ignore openPorts parse error in initial sync
		pi, _, err := m.getPodInfo(podKey, pod)
		if err != nil {
			// don't fail because of one pod error on initial sync as it may cause crashloop.
			// expect pod event to come later with correct/updated annotations.
			klog.Warningf("UDNHostIsolationManager failed to get pod info for pod %s/%s on initial sync: %v", pod.Name, pod.Namespace, err)
			continue
		}
		if pi == nil {
			// this pod doesn't need to be updated
			continue
		}

		udnPodIPsv4[podKey] = pi.ipsv4
		udnPodIPsv6[podKey] = pi.ipsv6
		udnOpenPortsICMPv4[podKey] = pi.icmpv4
		udnOpenPortsICMPv6[podKey] = pi.icmpv6
		udnOpenPortsv4[podKey] = pi.openPortsv4
		udnOpenPortsv6[podKey] = pi.openPortsv6
	}
	if err = m.udnPodIPsv4.fullSync(m.nft, udnPodIPsv4); err != nil {
		return err
	}
	if err = m.udnPodIPsv6.fullSync(m.nft, udnPodIPsv6); err != nil {
		return err
	}
	if err = m.udnOpenPortsICMPv4.fullSync(m.nft, udnOpenPortsICMPv4); err != nil {
		return err
	}
	if err = m.udnOpenPortsICMPv6.fullSync(m.nft, udnOpenPortsICMPv6); err != nil {
		return err
	}
	if err = m.udnOpenPortsv4.fullSync(m.nft, udnOpenPortsv4); err != nil {
		return err
	}
	if err = m.udnOpenPortsv6.fullSync(m.nft, udnOpenPortsv6); err != nil {
		return err
	}
	return nil
}

func podNeedsUpdate(oldObj, newObj *corev1.Pod) bool {
	if oldObj == nil || newObj == nil {
		return true
	}
	// react to pod IP changes
	return !reflect.DeepEqual(oldObj.Status, newObj.Status) ||
		oldObj.Annotations[types.OvnPodAnnotationName] != newObj.Annotations[types.OvnPodAnnotationName] ||
		oldObj.Annotations[util.UDNOpenPortsAnnotationName] != newObj.Annotations[util.UDNOpenPortsAnnotationName]
}

func (m *UDNHostIsolationManager) reconcilePod(_ context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("UDNHostIsolationManager failed to split meta namespace cache key %s for pod: %v", key, err)
		return nil
	}
	pod, err := m.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Pod was deleted, clean up.
			return m.updateWithPodInfo(key, &podInfo{})
		}
		return fmt.Errorf("failed to fetch pod %s in namespace %s", name, namespace)
	}
	pi, parseErr, err := m.getPodInfo(key, pod)
	if err != nil {
		return err
	}
	if pi == nil {
		// this pod doesn't need to be updated
		return nil
	}
	err = m.updateWithPodInfo(key, pi)
	return errors.Join(err, parseErr)
}

type podInfo struct {
	ipsv4       sets.Set[string]
	ipsv6       sets.Set[string]
	icmpv4      sets.Set[string]
	icmpv6      sets.Set[string]
	openPortsv4 sets.Set[string]
	openPortsv6 sets.Set[string]
}

// getPodInfo returns nftables set elements for a pod.
// nil is returned when pod should not be updated.
// empty podInfo will delete the pod from all sets and is returned when nil pod is passed.
// first error is for parsing openPorts annotation, second error is for fetching pod IPs.
// parsing error should not stop the update, as we need to cleanup potentially present rules from the previous config.
func (m *UDNHostIsolationManager) getPodInfo(podKey string, pod *corev1.Pod) (*podInfo, error, error) {
	pi := &podInfo{}
	if pod == nil {
		return pi, nil, nil
	}
	if util.PodWantsHostNetwork(pod) {
		// host network pods can't be isolated by IP
		return nil, nil, nil
	}
	// only add pods with primary UDN
	primaryUDN, err := m.isPodPrimaryUDN(pod)
	if err != nil {
		if util.IsAnnotationNotSetError(err) {
			// pod IPs were not assigned yet, expecting an update event
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("failed to check if pod %s is in primary UDN: %w", podKey, err)
	}
	if !primaryUDN {
		return nil, nil, nil
	}
	podIPs, err := util.DefaultNetworkPodIPs(pod)
	if err != nil {
		// update event should come later with ips
		klog.V(5).Infof("Failed to get default network pod IPs for pod %s: %v", podKey, err)
		return nil, nil, nil
	}
	openPorts, parseErr := util.UnmarshalUDNOpenPortsAnnotation(pod.Annotations)
	pi.ipsv4, pi.ipsv6 = splitIPsPerFamily(podIPs)
	pi.icmpv4, pi.icmpv6, pi.openPortsv4, pi.openPortsv6 = m.getOpenPortSets(pi.ipsv4, pi.ipsv6, openPorts)
	return pi, parseErr, nil
}

// updateWithPodInfo updates the nftables sets with given podInfo for a given pod.
// empty podInfo will delete the pod from all sets.
func (m *UDNHostIsolationManager) updateWithPodInfo(podKey string, pi *podInfo) error {
	tx := m.nft.NewTransaction()
	m.udnPodIPsv4.updatePodElementsTX(podKey, pi.ipsv4, tx)
	m.udnPodIPsv6.updatePodElementsTX(podKey, pi.ipsv6, tx)
	m.udnOpenPortsICMPv4.updatePodElementsTX(podKey, pi.icmpv4, tx)
	m.udnOpenPortsICMPv6.updatePodElementsTX(podKey, pi.icmpv6, tx)
	m.udnOpenPortsv4.updatePodElementsTX(podKey, pi.openPortsv4, tx)
	m.udnOpenPortsv6.updatePodElementsTX(podKey, pi.openPortsv6, tx)

	if tx.NumOperations() == 0 {
		return nil
	}

	err := m.nft.Run(context.TODO(), tx)
	if err != nil {
		return fmt.Errorf("could not update nftables set for UDN pods: %v", err)
	}

	// update internal state only after successful transaction
	m.udnPodIPsv4.updatePodElementsAfterTX(podKey, pi.ipsv4)
	m.udnPodIPsv6.updatePodElementsAfterTX(podKey, pi.ipsv6)
	m.udnOpenPortsICMPv4.updatePodElementsAfterTX(podKey, pi.icmpv4)
	m.udnOpenPortsICMPv6.updatePodElementsAfterTX(podKey, pi.icmpv6)
	m.udnOpenPortsv4.updatePodElementsAfterTX(podKey, pi.openPortsv4)
	m.udnOpenPortsv6.updatePodElementsAfterTX(podKey, pi.openPortsv6)
	return nil
}

func (m *UDNHostIsolationManager) isPodPrimaryUDN(pod *corev1.Pod) (bool, error) {
	podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, types.DefaultNetworkName)
	if err != nil {
		// pod IPs were not assigned yet, should be retried later
		return false, err
	}
	// NetworkRoleInfrastructure means default network is not primary, then UDN must be the primary network
	return podAnnotation.Role == types.NetworkRoleInfrastructure, nil
}

func (m *UDNHostIsolationManager) getOpenPortSets(newV4IPs, newV6IPs sets.Set[string], openPorts []*util.OpenPort) (icmpv4, icmpv6, openPortsv4, openPortsv6 sets.Set[string]) {
	icmpv4 = sets.New[string]()
	icmpv6 = sets.New[string]()
	openPortsv4 = sets.New[string]()
	openPortsv6 = sets.New[string]()

	for _, openPort := range openPorts {
		if openPort.Protocol == "icmp" {
			icmpv4 = newV4IPs
			icmpv6 = newV6IPs
		} else {
			for podIPv4 := range newV4IPs {
				openPortsv4.Insert(joinNFTSlice([]string{podIPv4, openPort.Protocol, fmt.Sprintf("%d", *openPort.Port)}))
			}
			for podIPv6 := range newV6IPs {
				openPortsv6.Insert(joinNFTSlice([]string{podIPv6, openPort.Protocol, fmt.Sprintf("%d", *openPort.Port)}))
			}
		}
	}
	return
}

// nftPodElementsSet is a helper struct to manage an nftables set with pod-owned elements.
// Can be used to store pod IPs, or more complex elements.
type nftPodElementsSet struct {
	setName string
	// podName: set elements
	podElements map[string]sets.Set[string]
	// podIPs may be reused as soon as the pod reaches Terminating state, and delete event may come later.
	// That means a new pod with the same IP may be added before the previous pod is deleted.
	// To avoid deleting newly-added pod IP thinking we are deleting old pod IP, we keep track of re-used set elements.
	elementToPods map[string]sets.Set[string]
	// if a set element is composed of multiple strings
	// set to false to avoid unneeded parsing
	composedValue bool
}

func newNFTPodElementsSet(setName string, composedValue bool) *nftPodElementsSet {
	return &nftPodElementsSet{
		setName:       setName,
		composedValue: composedValue,
		podElements:   make(map[string]sets.Set[string]),
		elementToPods: make(map[string]sets.Set[string]),
	}
}

func (n *nftPodElementsSet) getKey(key string) []string {
	if n.composedValue {
		return splitNFTSlice(key)
	}
	return []string{key}
}

// updatePodElementsTX adds transaction operations to update pod elements in nftables set.
// To update internal struct, updatePodElementsAfterTX must be called if transaction is successful.
func (n *nftPodElementsSet) updatePodElementsTX(namespacedName string, podElements sets.Set[string], tx *knftables.Transaction) {
	if n.podElements[namespacedName].Equal(podElements) {
		return
	}
	// always delete all old elements, then add new elements.
	for existingElem := range n.podElements[namespacedName] {
		if n.elementToPods[existingElem].Len() == 1 {
			// only delete element is it referenced by one pod
			tx.Delete(&knftables.Element{
				Set: n.setName,
				Key: n.getKey(existingElem),
			})
		}
	}
	for newElem := range podElements {
		// adding existing element is a no-op
		tx.Add(&knftables.Element{
			Set: n.setName,
			Key: n.getKey(newElem),
		})
	}
}

func (n *nftPodElementsSet) updatePodElementsAfterTX(namespacedName string, elements sets.Set[string]) {
	for existingElem := range n.podElements[namespacedName] {
		if !elements.Has(existingElem) {
			// element was removed
			n.elementToPods[existingElem].Delete(namespacedName)
			if n.elementToPods[existingElem].Len() == 0 {
				delete(n.elementToPods, existingElem)
			}
		}
	}

	for elem := range elements {
		if n.elementToPods[elem] == nil {
			n.elementToPods[elem] = sets.New[string]()
		}
		n.elementToPods[elem].Insert(namespacedName)
	}
	if len(elements) == 0 {
		delete(n.podElements, namespacedName)
	} else {
		n.podElements[namespacedName] = elements
	}
}

// fullSync should be called on restart to sync all pods elements.
// It flushes existing elements, and adds new elements.
func (n *nftPodElementsSet) fullSync(nft knftables.Interface, podsElements map[string]sets.Set[string]) error {
	tx := nft.NewTransaction()
	tx.Flush(&knftables.Set{
		Name: n.setName,
	})
	for podName, podElements := range podsElements {
		if len(podElements) == 0 {
			continue
		}
		for elem := range podElements {
			tx.Add(&knftables.Element{
				Set: n.setName,
				Key: n.getKey(elem),
			})
			if n.elementToPods[elem] == nil {
				n.elementToPods[elem] = sets.New[string]()
			}
			n.elementToPods[elem].Insert(podName)
		}
		n.podElements[podName] = podElements
	}
	err := nft.Run(context.TODO(), tx)
	if err != nil {
		clear(n.podElements)
		return fmt.Errorf("initial pods sync for UDN host isolation failed: %w", err)
	}
	return nil
}

func splitIPsPerFamily(podIPs []net.IP) (sets.Set[string], sets.Set[string]) {
	newV4IPs := sets.New[string]()
	newV6IPs := sets.New[string]()
	for _, podIP := range podIPs {
		if podIP.To4() != nil {
			newV4IPs.Insert(podIP.String())
		} else {
			newV6IPs.Insert(podIP.String())
		}
	}
	return newV4IPs, newV6IPs
}

func safeDelete(tx *knftables.Transaction, obj knftables.Object) {
	tx.Add(obj)
	tx.Delete(obj)
}

// joinNFTSlice converts nft element key or value (type []string) to string to store in the nftElementStorage.
// The separator is the same as the one used by nft commands, so we know that the parsing is going to be unambiguous.
func joinNFTSlice(k []string) string {
	return strings.Join(k, " . ")
}

// splitNFTSlice converts nftElementStorage key or value string representation back to slice.
func splitNFTSlice(k string) []string {
	return strings.Split(k, " . ")
}

// hostUsesCgroupv2 returns true if host is using cgroup v2, which means we can match on kubelet cgroup path.
// For cgroup v1, kubelet rule will be broken, but host isolation will stay.
func hostUsesCgroupv2() bool {
	return IsCgroup2UnifiedMode()
}

var (
	isUnifiedOnce sync.Once
	isUnified     bool
)

const (
	unifiedMountpoint = "/sys/fs/cgroup"
)

// this function is copied from github.com/opencontainers/runc/libcontainer/cgroups to avoid extra dependencies.
func IsCgroup2UnifiedMode() bool {
	isUnifiedOnce.Do(func() {
		var st unix.Statfs_t
		err := unix.Statfs(unifiedMountpoint, &st)
		if err != nil {
			if os.IsNotExist(err) && userns.RunningInUserNS() {
				// ignore the "not found" error if running in userns
				isUnified = false
				return
			}
			panic(fmt.Sprintf("cannot statfs cgroup root: %s", err))
		}
		isUnified = st.Type == unix.CGROUP2_SUPER_MAGIC
	})
	return isUnified
}
