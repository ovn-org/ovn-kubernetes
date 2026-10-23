// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	deploymentconfigapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

// These tests cover the MACBindingController (OKEP-6691), which is only active
// when ovnkube-node runs with --disable-udn-arp-ndp-flood.
//
// With that flag set, ARP replies / neighbour advertisements carrying the shared
// breth0 MAC are no longer flooded to every CUDN patch port: they reach only the
// default cluster network (CDN) gateway router. UDN gateway routers therefore
// cannot learn external neighbours themselves. Instead the controller mirrors
// the SBDB MAC_Binding rows learned on the CDN external port
// (rtoe-GR_<node>) onto every tracked primary UDN external port
// (rtoe-GR_cluster_udn_<cudn>_<node>) in the same zone.
//
// Node IPs are deliberately excluded from that mirroring; they are written as
// NBDB Static_MAC_Binding rows instead (see the "node IPs" spec below).
//
// Nothing ever deletes a mirrored row explicitly: it expires through the OVN
// logical router option mac_binding_age_threshold, which ovn-kubernetes sets to
// types.GRMACBindingAgeThreshold (300) on every gateway router.

const (
	noFloodHostnameKey = "kubernetes.io/hostname"

	// pid files for the background pings started inside the client pods.
	noFloodPingPidFileV4   = "/tmp/noflood-ping4.pid"
	noFloodPingPidFileV6   = "/tmp/noflood-ping6.pid"
	noFloodPingPidFileGlob = "/tmp/noflood-ping*.pid"

	// Stopping the traffic does not start a plain mac_binding_age_threshold
	// (300s) countdown. Once a binding enters the last 3/16 of its lifetime
	// (~56s) OVN re-ARPs the neighbour, and an external container that is still
	// up answers, so the row keeps being refreshed for roughly one more full
	// threshold before it finally goes stale. Measured on kind: the last
	// refresh landed 259s after the pings stopped and the row disappeared at
	// 573s. Allow a wide margin on top of that for slow CI.
	noFloodAgeOutTimeout = 900 * time.Second

	// How long we hold the bindings under sustained traffic to prove the
	// controller does not churn or prematurely drop them. Comfortably longer
	// than the ~56s refresh interval, so the controller's refresh-mirror path
	// is exercised a couple of times.
	noFloodStabilityDuration = 120 * time.Second

	// After an ovnkube-node restart the databases in that pod come back empty
	// and everything has to be relearned and re-mirrored.
	noFloodReconvergeTimeout = 300 * time.Second

	// A gateway router only notices a neighbour's new MAC on its next refresh
	// probe, roughly every 56s, and in the worst case the binding was refreshed
	// just before the change.
	noFloodMACChangeTimeout = 600 * time.Second
)

// macBindingRow is one row of either the SBDB MAC_Binding table or the NBDB
// Static_MAC_Binding table.
type macBindingRow struct {
	logicalPort        string
	ip                 string
	mac                string
	overrideDynamicMAC bool
}

// listMACBindingRows runs an ovn-{n,s}bctl list command that emits bare CSV and
// parses the rows. Filtering happens in Go rather than through `find ip=...` so
// the test does not have to deal with quoting IPv6 addresses inside a shell
// string, which is error prone.
func listMACBindingRows(ovnkPod *v1.Pod, cmd string, columns int) ([]macBindingRow, error) {
	out, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%q failed: %w", cmd, err)
	}
	var rows []macBindingRow
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < columns {
			return nil, fmt.Errorf("unexpected output line %q from %q", line, cmd)
		}
		row := macBindingRow{logicalPort: fields[0], ip: fields[1], mac: fields[2]}
		if columns > 3 {
			row.overrideDynamicMAC = fields[3] == "true"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// listDynamicMACBindings returns the SBDB MAC_Binding rows of the zone that
// ovnkPod belongs to.
func listDynamicMACBindings(ovnkPod *v1.Pod) ([]macBindingRow, error) {
	return listMACBindingRows(ovnkPod,
		"ovn-sbctl --format=csv --no-headings --data=bare --columns=logical_port,ip,mac list MAC_Binding", 3)
}

// listStaticMACBindings returns the NBDB Static_MAC_Binding rows of the zone
// that ovnkPod belongs to.
func listStaticMACBindings(ovnkPod *v1.Pod) ([]macBindingRow, error) {
	return listMACBindingRows(ovnkPod,
		"ovn-nbctl --format=csv --no-headings --data=bare --columns=logical_port,ip,mac,override_dynamic_mac list Static_MAC_Binding", 4)
}

// purgeDynamicMACBindings destroys every MAC_Binding row holding one of ips.
// Dynamic rows only ever disappear through aging, and the infra provider recycles
// external container IPs, so a run started within mac_binding_age_threshold of the
// previous one would otherwise inherit its bindings.
func purgeDynamicMACBindings(ovnkPod *v1.Pod, ips []string) {
	conds := make([]string, 0, len(ips))
	for _, ip := range ips {
		conds = append(conds, fmt.Sprintf("$2==%q", ip))
	}
	cmd := fmt.Sprintf(`for u in $(ovn-sbctl --format=csv --no-headings --data=bare --columns=_uuid,ip list MAC_Binding | awk -F, '%s{print $1}'); do ovn-sbctl --if-exists destroy MAC_Binding $u; done`,
		strings.Join(conds, "||"))
	_, err := e2epodoutput.RunHostCmdWithRetries(ovnkPod.Namespace, ovnkPod.Name, cmd, framework.Poll, 30*time.Second)
	Expect(err).NotTo(HaveOccurred(), "must purge stale MAC_Bindings for %v", ips)
}

// macBindingPortsForIP returns logical port -> MAC for every row holding ip.
func macBindingPortsForIP(rows []macBindingRow, ip string) map[string]string {
	ports := map[string]string{}
	for _, row := range rows {
		if row.ip == ip {
			ports[row.logicalPort] = row.mac
		}
	}
	return ports
}

// cudnGatewayRouterExtPortName returns the external (rtoe-) logical router port
// of the CUDN gateway router on the given node - the mirroring target.
func cudnGatewayRouterExtPortName(cudnName, nodeName string) string {
	return ovntypes.GWRouterToExtSwitchPrefix + cudnGatewayRouterName(cudnName, nodeName)
}

// defaultGatewayRouterExtPortName returns the external (rtoe-) logical router
// port of the default cluster network gateway router - the mirroring source.
func defaultGatewayRouterExtPortName(nodeName string) string {
	return ovntypes.GWRouterToExtSwitchPrefix + ovntypes.GWRouterPrefix + nodeName
}

// externalContainerMAC returns the MAC of the external container's primary
// interface, which is the MAC the gateway routers are expected to learn.
func externalContainerMAC(c infraapi.ExternalContainer) string {
	iface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
	out, err := infraprovider.Get().ExecExternalContainerCommand(c, []string{"cat", "/sys/class/net/" + iface + "/address"})
	Expect(err).NotTo(HaveOccurred(), "must read MAC of external container %s", c.GetName())
	mac := strings.ToLower(strings.TrimSpace(out))
	Expect(mac).NotTo(BeEmpty(), "external container %s must have a MAC", c.GetName())
	return mac
}

// startBackgroundPing starts a detached ping inside the pod and records its pid
// so it can be stopped later. The correct binary is picked per IP family.
func startBackgroundPing(namespace, podName, ip, pidFile string) {
	ping := "ping"
	if utilnet.IsIPv6String(ip) {
		ping = "ping6"
	}
	_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
		fmt.Sprintf("%s -q -i 1 %s >/dev/null 2>&1 & echo $! > %s", ping, ip, pidFile))
	Expect(err).NotTo(HaveOccurred(), "must start background ping to %s from %s/%s", ip, namespace, podName)
}

// stopBackgroundPings kills every ping started by startBackgroundPing in the pod
// and asserts none survives - a surviving ping would keep the MAC bindings
// refreshed and silently invalidate the age-out specs. Both families are stopped
// together because the survivor check counts "ping" and "ping6" alike.
func stopBackgroundPings(namespace, podName string) {
	_, err := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
		fmt.Sprintf("kill $(cat %s) 2>/dev/null; rm -f %s; true", noFloodPingPidFileGlob, noFloodPingPidFileGlob))
	Expect(err).NotTo(HaveOccurred(), "must stop background pings in %s/%s", namespace, podName)
	Eventually(func() string {
		out, _ := e2ekubectl.RunKubectl(namespace, "exec", podName, "--", "/bin/sh", "-c",
			"ps -o args= -A 2>/dev/null | grep -c '^ping' || true")
		return strings.TrimSpace(out)
	}, 30*time.Second, 2*time.Second).Should(Equal("0"), "no ping must survive in %s/%s", namespace, podName)
}

// noFloodGatewayConfig is the subset of the k8s.ovn.org/l3-gateway-config node
// annotation this test needs.
type noFloodGatewayConfig struct {
	MACAddress  string   `json:"mac-address"`
	IPAddresses []string `json:"ip-addresses"`
}

func noFloodGatewayConfigOf(node *v1.Node) noFloodGatewayConfig {
	raw, ok := node.Annotations["k8s.ovn.org/l3-gateway-config"]
	Expect(ok).To(BeTrue(), "node %s must have a l3-gateway-config annotation", node.Name)
	var cfg map[string]noFloodGatewayConfig
	Expect(json.Unmarshal([]byte(raw), &cfg)).To(Succeed(), "l3-gateway-config of node %s must parse", node.Name)
	def, ok := cfg["default"]
	Expect(ok).To(BeTrue(), "node %s must have a default l3-gateway-config entry", node.Name)
	return def
}

var _ = Describe("Network Segmentation: MAC binding mirroring with ARP/NDP flood disabled",
	feature.NetworkSegmentation, func() {

		f := wrappedTestFramework("noflood-macbinding")
		// Namespaces are created by hand in BeforeAll: the UDN ones need the
		// primary-UDN label, and the framework must not garbage collect them
		// between the ordered specs.
		f.SkipNamespaceCreation = true

		// The ordered container is nested so that the framework's own
		// BeforeEach - which populates f.ClientSet - runs before BeforeAll.
		Context("", Ordered, func() {

			var (
				cs          clientset.Interface
				providerCtx infraapi.Context

				trafficNode v1.Node
				ovnkPod     *v1.Pod

				extA, extB       infraapi.ExternalContainer
				extAMAC, extBMAC string
				// IPs actually exercised, one per supported family.
				extAIPs, extBIPs []string

				// every schedulable node other than trafficNode; their zones must
				// stay untouched.
				otherNodes []v1.Node

				cdnNamespace string
				// udnNamespace holds the pinging UDN pod; it is selected by cudnNames[0].
				udnNamespace string
				cudnNames    []string
				// the mirroring source plus the three mirroring targets.
				expectedPorts []string
				// the static bindings the CDN external port carries before any
				// test traffic - the gateway masquerade IPs. The controller must
				// never add to them.
				cdnStaticIPs []string

				cdnPodName = "noflood-cdn-client"
				udnPodName = "noflood-udn-client"
			)

			// macBindingPorts returns logical port -> MAC for ip in the traffic
			// node's zone.
			macBindingPorts := func(ip string) map[string]string {
				rows, err := listDynamicMACBindings(ovnkPod)
				if err != nil {
					framework.Logf("failed to list MAC_Bindings: %v", err)
					return nil
				}
				return macBindingPortsForIP(rows, ip)
			}

			// expectMirroredEverywhere asserts that exactly ports hold ip, all with
			// the same mac.
			expectMirroredEverywhere := func(g Gomega, ip, mac string, ports []string) {
				got := macBindingPorts(ip)
				g.Expect(got).To(HaveLen(len(ports)), "MAC_Bindings for %s: got %v, want exactly %v", ip, got, ports)
				g.Expect(macBindingPortNames(got)).To(ConsistOf(ports), "MAC_Bindings for %s must be on exactly %v, got %v", ip, ports, got)
				for port, gotMAC := range got {
					g.Expect(gotMAC).To(Equal(mac), "MAC_Binding for %s on %s must carry MAC %s", ip, port, mac)
				}
			}

			// ovnkPodOnNode returns the ovnkube-node pod owning the given node's
			// zone, which is how this test reaches that zone's NB/SB databases.
			ovnkPodOnNode := func(nodeName string) *v1.Pod {
				pods, err := cs.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: "spec.nodeName=" + nodeName,
				})
				framework.ExpectNoError(err)
				Expect(pods.Items).To(HaveLen(1), "expected exactly one ovnkube-node pod on %s", nodeName)
				return &pods.Items[0]
			}

			// staticMACBindingIPsOn returns the IPs statically bound on one
			// logical port.
			staticMACBindingIPsOn := func(port string) []string {
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				var ips []string
				for _, row := range static {
					if row.logicalPort == port {
						ips = append(ips, row.ip)
					}
				}
				return ips
			}

			// deleteNamespaceAndWait removes a namespace and blocks until it is
			// really gone. A ClusterUserDefinedNetwork cannot be deleted while a
			// namespace it selects still exists, so every CUDN teardown has to
			// go through this first.
			deleteNamespaceAndWait := func(name string) {
				err := cs.CoreV1().Namespaces().Delete(context.TODO(), name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "must delete namespace %s", name)
				}
				Eventually(func() bool {
					_, err := cs.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, 120*time.Second, 2*time.Second).Should(BeTrue(), "namespace %s must go away", name)
			}

			createCUDN := func(namespace, topology, cidr, role string) string {
				name := randomNetworkMetaName()
				manifest := generateClusterUserDefinedNetworkManifest(&networkAttachmentConfigParams{
					name:      name,
					namespace: namespace,
					topology:  topology,
					cidr:      cidr,
					role:      role,
				}, cs)
				cleanup, err := createManifest("", manifest)
				Expect(err).NotTo(HaveOccurred(), "must create CUDN %s", name)
				DeferCleanup(func() {
					deleteNamespaceAndWait(namespace)
					cleanup()
					_, _ = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", name, "--wait", "--timeout=120s")
				})
				Eventually(clusterUserDefinedNetworkReadyFunc(context.TODO(), f.DynamicClient, name), 60*time.Second, time.Second).
					Should(Succeed(), "CUDN %s must become ready", name)
				return name
			}

			createNamespace := func(labels map[string]string) *v1.Namespace {
				ns, err := cs.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{GenerateName: f.BaseName + "-", Labels: labels},
				}, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "must create namespace")
				DeferCleanup(func() {
					_ = cs.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{})
				})
				return ns
			}

			createPod := func(namespace, name string, attachments ...nadapi.NetworkSelectionElement) {
				e2epod.PodClientNS(f, namespace).CreateSync(context.TODO(), generatePodSpec(podConfiguration{
					name:         name,
					namespace:    namespace,
					containerCmd: []string{"pause"},
					nodeSelector: map[string]string{noFloodHostnameKey: trafficNode.Name},
					isPrivileged: true,
					attachments:  attachments,
				}))
			}

			BeforeAll(func() {
				cs = f.ClientSet
				if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.UDNARPNDPFloodDisabled) {
					Skip("requires ovnkube-node to run with --disable-udn-arp-ndp-flood (DISABLE_UDN_ARP_NDP_FLOOD=true)")
				}

				By("selecting the node that will carry all the test traffic")
				nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
				framework.ExpectNoError(err)
				Expect(nodes.Items).NotTo(BeEmpty(), "need at least one schedulable node")
				trafficNode = nodes.Items[0]
				otherNodes = nodes.Items[1:]
				ovnkPod = ovnkPodOnNode(trafficNode.Name)

				By("creating the two external containers")
				providerCtx = infraprovider.Get().NewTestContext()
				providerNetwork, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "provider primary network must be available")
				for i, target := range []*infraapi.ExternalContainer{&extA, &extB} {
					port := infraprovider.Get().GetExternalContainerPort()
					container, err := providerCtx.CreateExternalContainer(infraapi.ExternalContainer{
						Name:    fmt.Sprintf("noflood-ext-%d-%s", i, rand.String(5)),
						Image:   images.AgnHost(),
						Network: providerNetwork,
						CmdArgs: httpServerContainerCmd(port),
						ExtPort: port,
					})
					framework.ExpectNoError(err, "must create external container %d", i)
					*target = container
				}
				extAMAC, extBMAC = externalContainerMAC(extA), externalContainerMAC(extB)
				for _, spec := range []struct {
					c   infraapi.ExternalContainer
					ips *[]string
				}{{extA, &extAIPs}, {extB, &extBIPs}} {
					if isIPv4Supported(cs) {
						Expect(spec.c.GetIPv4()).NotTo(BeEmpty())
						*spec.ips = append(*spec.ips, spec.c.GetIPv4())
					}
					if isIPv6Supported(cs) {
						Expect(spec.c.GetIPv6()).NotTo(BeEmpty())
						*spec.ips = append(*spec.ips, spec.c.GetIPv6())
					}
				}
				Expect(extAIPs).NotTo(BeEmpty(), "no supported IP family")
				framework.Logf("external container A: ips=%v mac=%s", extAIPs, extAMAC)
				framework.Logf("external container B: ips=%v mac=%s", extBIPs, extBMAC)
				extIPs := append(append([]string{}, extAIPs...), extBIPs...)
				purgeDynamicMACBindings(ovnkPod, extIPs)
				for i := range otherNodes {
					purgeDynamicMACBindings(ovnkPodOnNode(otherNodes[i].Name), extIPs)
				}
				By("creating the CDN namespace and its client pod")
				cdnNs := createNamespace(map[string]string{"e2e-framework": f.BaseName})
				// The framework skipped namespace creation; give it one anyway so
				// its per-spec hooks have something to work with.
				f.Namespace = cdnNs
				cdnNamespace = cdnNs.Name
				createPod(cdnNamespace, cdnPodName)

				By("creating three primary CUDNs, one layer3 and two layer2")
				// Four non overlapping subnet pairs: three for the CUDNs created
				// here and a fourth for the late joining CUDN.
				v4, v6 := allocators.GetNthFirstUDNSubnets(4)
				topologies := []struct {
					topology string
					cidr     string
				}{
					{"layer3", joinStrings(v4[0]+"/24", v6[0]+"/64")},
					{"layer2", joinStrings(v4[1], v6[1])},
					{"layer2", joinStrings(v4[2], v6[2])},
				}
				for i, t := range topologies {
					ns := createNamespace(map[string]string{
						"e2e-framework":           f.BaseName,
						RequiredUDNNamespaceLabel: "",
					}).Name
					name := createCUDN(ns, t.topology, t.cidr, "primary")
					cudnNames = append(cudnNames, name)
					// Every CUDN gets a pod on the traffic node so that its
					// topology - and therefore its rtoe- port - exists there even
					// with dynamic UDN allocation enabled. The first one doubles as
					// the pinging UDN client.
					podName := fmt.Sprintf("noflood-udn-pause-%d", i)
					if i == 0 {
						podName = udnPodName
						udnNamespace = ns
					}
					createPod(ns, podName)
				}

				expectedPorts = []string{defaultGatewayRouterExtPortName(trafficNode.Name)}
				for _, name := range cudnNames {
					expectedPorts = append(expectedPorts, cudnGatewayRouterExtPortName(name, trafficNode.Name))
				}
				framework.Logf("expecting MAC bindings on: %v", expectedPorts)

				cdnStaticIPs = staticMACBindingIPsOn(defaultGatewayRouterExtPortName(trafficNode.Name))
				framework.Logf("CDN gateway router static bindings at rest: %v", cdnStaticIPs)
			})

			It("has no MAC bindings for the external containers before any traffic", func() {
				// IPv4 only: an IPv6 container announces itself on the shared
				// link as soon as it comes up (DAD / unsolicited NA), so the CDN
				// gateway router learns it - and the controller mirrors it -
				// before any test traffic exists. IPv4 has no such chatter.
				if !isIPv4Supported(cs) {
					Skip("baseline requires IPv4: IPv6 neighbour discovery populates the bindings on container start")
				}
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, ip := range []string{extA.GetIPv4(), extB.GetIPv4()} {
					Expect(macBindingPorts(ip)).To(BeEmpty(),
						"no MAC_Binding must exist for untouched external container IP %s", ip)
					Expect(macBindingPortsForIP(static, ip)).To(BeEmpty(),
						"no Static_MAC_Binding must exist for external container IP %s", ip)
				}
			})

			It("mirrors the binding learned by the CDN gateway router to every UDN", func() {
				By("starting a sustained ping from the CDN pod to external container A")
				for _, ip := range extAIPs {
					startBackgroundPing(cdnNamespace, cdnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extAIPs {
					By(fmt.Sprintf("asserting %s is bound to %s on all %d gateway routers", ip, extAMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}
			})

			It("mirrors a binding triggered by a UDN pod to the CDN and every other UDN", func() {
				// The UDN gateway router sends the ARP/NS itself, but with flooding
				// disabled the reply only reaches the CDN gateway router. The UDN
				// can therefore only obtain the binding through the controller.
				By("starting a sustained ping from the UDN pod to external container B")
				for _, ip := range extBIPs {
					startBackgroundPing(udnNamespace, udnPodName, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					By(fmt.Sprintf("asserting %s is bound to %s on all %d gateway routers", ip, extBMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}
			})

			It("binds node IPs statically on the UDNs instead of mirroring them", func() {
				gwCfg := noFloodGatewayConfigOf(&trafficNode)
				var extSubnets []*net.IPNet
				for _, addr := range gwCfg.IPAddresses {
					_, subnet, err := net.ParseCIDR(addr)
					Expect(err).NotTo(HaveOccurred(), "l3-gateway-config ip-address %q must parse", addr)
					extSubnets = append(extSubnets, subnet)
				}

				By("collecting the node IPs that are on-link on the traffic node's external subnets")
				nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				framework.ExpectNoError(err)
				// node IP -> expected MAC (the owning node's gateway MAC)
				onLinkNodeIPs := map[string]string{}
				for i := range nodes.Items {
					node := &nodes.Items[i]
					mac := strings.ToLower(noFloodGatewayConfigOf(node).MACAddress)
					for _, addr := range node.Status.Addresses {
						if addr.Type != v1.NodeInternalIP {
							continue
						}
						ip := net.ParseIP(addr.Address)
						for _, subnet := range extSubnets {
							if subnet.Contains(ip) {
								onLinkNodeIPs[addr.Address] = mac
							}
						}
					}
				}
				Expect(onLinkNodeIPs).NotTo(BeEmpty(), "expected at least one on-link node IP")
				framework.Logf("on-link node IPs: %v", onLinkNodeIPs)

				cdnPort := defaultGatewayRouterExtPortName(trafficNode.Name)
				udnPorts := expectedPorts[1:]

				By("asserting every UDN external port has a static binding for every on-link node IP")
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for ip, mac := range onLinkNodeIPs {
						got := map[string]string{}
						for _, row := range static {
							if row.ip != ip {
								continue
							}
							g.Expect(row.overrideDynamicMAC).To(BeTrue(),
								"static binding for %s on %s must set override_dynamic_mac", ip, row.logicalPort)
							got[row.logicalPort] = row.mac
						}
						g.Expect(macBindingPortNames(got)).To(ConsistOf(udnPorts),
							"node IP %s must be statically bound on exactly the UDN ports %v, got %v", ip, udnPorts, got)
						for port, gotMAC := range got {
							g.Expect(gotMAC).To(Equal(mac), "static binding for %s on %s must carry MAC %s", ip, port, mac)
						}
					}
				}, 120*time.Second, 5*time.Second).Should(Succeed())

				By("asserting the CDN external port only carries the gateway masquerade bindings")
				static, err := listStaticMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, row := range static {
					if row.logicalPort != cdnPort {
						continue
					}
					Expect(onLinkNodeIPs).NotTo(HaveKey(row.ip),
						"the CDN gateway router must never be a static binding target, found %s on %s", row.ip, cdnPort)
				}

				By("asserting node IPs are not mirrored as dynamic bindings on the UDNs")
				dynamic, err := listDynamicMACBindings(ovnkPod)
				Expect(err).NotTo(HaveOccurred())
				for _, row := range dynamic {
					if _, isNodeIP := onLinkNodeIPs[row.ip]; !isNodeIP {
						continue
					}
					Expect(udnPorts).NotTo(ContainElement(row.logicalPort),
						"node IP %s must not be dynamically mirrored onto UDN port %s", row.ip, row.logicalPort)
				}
			})

			It("never adds a static MAC binding to the default network gateway router", func() {
				// The controller owns the UDN external ports only; the CDN port
				// must keep exactly the masquerade bindings the gateway itself
				// programs (config.IsGatewayStaticMACBindingIP).
				got := staticMACBindingIPsOn(defaultGatewayRouterExtPortName(trafficNode.Name))
				Expect(got).To(ConsistOf(cdnStaticIPs),
					"the static bindings of the CDN gateway router must not change while the controller runs")
				for _, ip := range append(append([]string{}, extAIPs...), extBIPs...) {
					Expect(got).NotTo(ContainElement(ip),
						"external container IP %s must never be statically bound on the CDN gateway router", ip)
				}
			})

			It("does not mirror onto a secondary CUDN", func() {
				// shouldTrackNetwork only accepts the default network and primary
				// layer2/layer3 UDNs, so a secondary network must never become a
				// mirroring follower.
				v4, v6 := allocators.GetNthFirstUDNSubnets(5)
				ns := createNamespace(map[string]string{"e2e-framework": f.BaseName}).Name
				secName := createCUDN(ns, "layer2", joinStrings(v4[4], v6[4]), "secondary")
				createPod(ns, "noflood-secondary", nadapi.NetworkSelectionElement{Name: secName, Namespace: ns})
				secPrefix := util.GetUserDefinedNetworkPrefix(ovntypes.CUDNPrefix + secName)

				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(row.logicalPort).NotTo(ContainSubstring(secPrefix),
							"the secondary network must not get static MAC bindings either")
					}
				}, 30*time.Second, 5*time.Second).Should(Succeed())
			})

			It("does not leak the bindings into the other nodes' zones", func() {
				// IPv4 only, for the same reason the baseline spec is: an IPv6
				// container announces itself on the shared link, so every node's
				// CDN gateway router legitimately learns it and mirrors it within
				// its own zone. Nothing makes another node resolve the container's
				// IPv4 address - always_learn_from_arp_request is false, so the
				// broadcast ARP requests this test generates teach nobody.
				if len(otherNodes) == 0 {
					Skip("needs more than one schedulable node")
				}
				if !isIPv4Supported(cs) {
					Skip("cross-zone isolation is only asserted for IPv4")
				}
				for i := range otherNodes {
					node := &otherNodes[i]
					rows, err := listDynamicMACBindings(ovnkPodOnNode(node.Name))
					Expect(err).NotTo(HaveOccurred())
					for _, ip := range []string{extA.GetIPv4(), extB.GetIPv4()} {
						Expect(macBindingPortsForIP(rows, ip)).To(BeEmpty(),
							"the controller is zone local: node %s must hold no MAC binding for %s", node.Name, ip)
					}
				}
			})

			It("keeps the mirrored bindings stable while the traffic lasts", func() {
				// OVN refreshes a binding's timestamp inside the last 3/16 of the
				// age threshold, i.e. roughly every 56s here, so this window both
				// proves the controller does not churn or prematurely drop rows
				// and puts the refresh-mirror path through a few cycles.
				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredEverywhere(g, ip, extAMAC, expectedPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredEverywhere(g, ip, extBMAC, expectedPorts)
					}
				}, noFloodStabilityDuration, 10*time.Second).Should(Succeed())
			})

			It("backfills a CUDN created after the bindings were learned, and reaps it on delete", func() {
				v4, v6 := allocators.GetNthFirstUDNSubnets(4)
				ns := createNamespace(map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				}).Name
				lateName := createCUDN(ns, "layer3", joinStrings(v4[3]+"/24", v6[3]+"/64"), "primary")
				createPod(ns, "noflood-udn-late")
				latePort := cudnGatewayRouterExtPortName(lateName, trafficNode.Name)
				withLate := append(append([]string{}, expectedPorts...), latePort)

				By("asserting the late CUDN is backfilled with the already learned bindings")
				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, withLate)
						}, 120*time.Second, 5*time.Second).Should(Succeed())
					}
				}

				By("deleting the late CUDN and asserting its bindings are reaped")
				deleteNamespaceAndWait(ns)
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", lateName, "--wait", "--timeout=120s")
				Expect(err).NotTo(HaveOccurred())

				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, expectedPorts)
						}, 120*time.Second, 5*time.Second).Should(Succeed())
					}
				}
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(row.logicalPort).NotTo(Equal(latePort),
							"static bindings of the deleted CUDN must be reaped")
					}
				}, 120*time.Second, 5*time.Second).Should(Succeed())
			})

			It("reconverges after ovnkube-node restarts, without leaving orphan static bindings", func() {
				framework.ExpectNoError(restartOVNKubeNodePod(cs, deploymentconfig.Get().OVNKubernetesNamespace(), trafficNode.Name))
				// The pod name changed and every later spec reads the databases
				// through it.
				ovnkPod = ovnkPodOnNode(trafficNode.Name)

				By("asserting the mirrored bindings come back on every gateway router")
				for _, spec := range []struct {
					ips []string
					mac string
				}{{extAIPs, extAMAC}, {extBIPs, extBMAC}} {
					for _, ip := range spec.ips {
						Eventually(func(g Gomega) {
							expectMirroredEverywhere(g, ip, spec.mac, expectedPorts)
						}, noFloodReconvergeTimeout, 5*time.Second).Should(Succeed())
					}
				}

				By("asserting repairStaticMacBindings left no orphans behind")
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(expectedPorts).To(ContainElement(row.logicalPort),
							"static binding for %s survives on %s, which is not a live gateway router port", row.ip, row.logicalPort)
					}
				}, noFloodReconvergeTimeout, 5*time.Second).Should(Succeed())
			})

			It("converges every gateway router onto the new MAC when the external container changes it", func() {
				iface := infraprovider.Get().ExternalContainerPrimaryInterfaceName()
				newMAC := "02:00:00:00:00:a1"
				// Changed in place: bouncing the link would flush the container's
				// global IPv6 address and the test would then be chasing a
				// neighbour that no longer answers.
				_, err := infraprovider.Get().ExecExternalContainerCommand(extA,
					[]string{"ip", "link", "set", "dev", iface, "address", newMAC})
				Expect(err).NotTo(HaveOccurred(), "must change the MAC of external container A")
				Expect(externalContainerMAC(extA)).To(Equal(newMAC))
				extAMAC = newMAC

				// The gateway routers do not learn from the gratuitous ARP - they
				// run with always_learn_from_arp_request=false - so they pick the
				// new MAC up on their next refresh probe, and the controller has
				// to update the followers in place rather than add a second row.
				for _, ip := range extAIPs {
					By(fmt.Sprintf("asserting %s converges to %s on all %d gateway routers", ip, newMAC, len(expectedPorts)))
					Eventually(func(g Gomega) {
						expectMirroredEverywhere(g, ip, newMAC, expectedPorts)
					}, noFloodMACChangeTimeout, 5*time.Second).Should(Succeed())
				}
			})

			It("ages out the CDN-triggered binding on every gateway router once traffic stops", func() {
				// Both pings are stopped here so the two age-out specs share a
				// single 300s mac_binding_age_threshold window instead of running
				// two back to back.
				By("stopping all background pings")
				stopBackgroundPings(cdnNamespace, cdnPodName)
				stopBackgroundPings(udnNamespace, udnPodName)

				for _, ip := range extAIPs {
					By(fmt.Sprintf("waiting for %s to age out of all gateway routers", ip))
					Eventually(func() map[string]string {
						return macBindingPorts(ip)
					}, noFloodAgeOutTimeout, 10*time.Second).Should(BeEmpty(),
						"MAC_Bindings for %s must age out after mac_binding_age_threshold", ip)
				}
			})

			It("ages out the UDN-triggered binding on every gateway router once traffic stops", func() {
				for _, ip := range extBIPs {
					By(fmt.Sprintf("waiting for %s to age out of all gateway routers", ip))
					Eventually(func() map[string]string {
						return macBindingPorts(ip)
					}, noFloodAgeOutTimeout, 10*time.Second).Should(BeEmpty(),
						"MAC_Bindings for %s must age out after mac_binding_age_threshold", ip)
				}
			})
		})
	})

// Tests for MAC binding mirroring with uplink-bound CUDNs.
// This test suite validates that MAC bindings learned by one CUDN's gateway router
// on its uplink are properly mirrored to other CUDNs on the same uplink group.
//
// The test creates multiple CUDNs on the same uplink segment and verifies that:
// 1. When CUDN1 learns an external IP, it mirrors to CUDN2 and CUDN3
// 2. When CUDN2 learns an additional IP, all CUDNs get it
// 3. When CUDN3 initiates traffic, the mirroring covers all three
// 4. Stability and convergence under sustained traffic
// 5. Bindings age out correctly after traffic stops

var _ = Describe("Network Segmentation: MAC binding mirroring for multiple UDNs on same uplink",
	feature.NetworkSegmentation, feature.Uplink, func() {

		f := wrappedTestFramework("noflood-uplink-multi-cudn")
		f.SkipNamespaceCreation = true

		Context("", Ordered, func() {

			var (
				cs          kubernetes.Interface
				ictx        infraapi.Context
				ipFamilySet sets.Set[utilnet.IPFamily]

				trafficNode v1.Node
				ovnkPod     *v1.Pod

				extA, extB, extC          infraapi.ExternalContainer
				extAMAC, extBMAC, extCMAC string
				extAIPs, extBIPs, extCIPs []string

				// Uplink infrastructure
				uplinkName string
				bridgeName string
				nodeIfaces map[string]infraapi.NetworkInterface

				// CUDN names and port names
				cudn1Name, cudn2Name, cudn3Name string
				cudn1Port, cudn2Port, cudn3Port string
				uplinkPorts                     []string

				// Pod names
				pod1Name = "uplink-cudn1-client"
				pod2Name = "uplink-cudn2-client"
				pod3Name = "uplink-cudn3-client"

				// Namespace names for each CUDN
				cudn1Namespace, cudn2Namespace, cudn3Namespace string

				testSuffix string
			)

			macBindingPorts := func(ip string) map[string]string {
				rows, err := listDynamicMACBindings(ovnkPod)
				if err != nil {
					framework.Logf("failed to list MAC_Bindings: %v", err)
					return nil
				}
				return macBindingPortsForIP(rows, ip)
			}

			expectMirroredOnAllPorts := func(g Gomega, ip, mac string, ports []string) {
				got := macBindingPorts(ip)
				g.Expect(got).To(HaveLen(len(ports)), "MAC_Bindings for %s: got %v, want exactly %v", ip, got, ports)
				g.Expect(macBindingPortNames(got)).To(ConsistOf(ports), "MAC_Bindings for %s must be on exactly %v", ip, ports)
				for port, gotMAC := range got {
					g.Expect(gotMAC).To(Equal(mac), "MAC_Binding for %s on %s must carry MAC %s", ip, port, mac)
				}
			}

			BeforeAll(func(ctx SpecContext) {
				cs = f.ClientSet
				if !deploymentconfig.Get().IsConfigurationEnabled(deploymentconfigapi.UDNARPNDPFloodDisabled) {
					Skip("requires --disable-udn-arp-ndp-flood")
				}
				if IsGatewayModeLocal(cs) {
					Skip("Uplink CUDN gateway plumbing is only supported in shared gateway mode")
				}

				By("selecting the traffic node")
				nodes, err := e2enode.GetReadySchedulableNodes(context.TODO(), cs)
				framework.ExpectNoError(err)
				Expect(nodes.Items).NotTo(BeEmpty(), "need at least one schedulable node")
				trafficNode = nodes.Items[0]

				// Get the ovnkube-node pod for this node
				ovnkNs := "ovn-kubernetes"
				ovnkPods, err := cs.CoreV1().Pods(ovnkNs).List(context.TODO(), metav1.ListOptions{
					LabelSelector: "app=ovnkube-node",
					FieldSelector: "spec.nodeName=" + trafficNode.Name,
				})
				framework.ExpectNoError(err)
				Expect(ovnkPods.Items).To(HaveLen(1), "expected exactly one ovnkube-node pod on %s", trafficNode.Name)
				ovnkPod = &ovnkPods.Items[0]

				ipFamilySet = sets.New(getSupportedIPFamiliesSlice(cs)...)
				testSuffix = framework.RandomSuffix()

				By("setting up uplink infrastructure")
				allNodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
				framework.ExpectNoError(err)

				ictx = infraprovider.Get().NewTestContext()

				uplinkAlloc, err := allocators.AllocateBGP(f, ictx)
				framework.ExpectNoError(err)

				_, nodeIfaces = setupUplinkNetwork(
					ictx,
					allNodes.Items,
					ipFamilySet,
					"upnet"+testSuffix,
					[]string{uplinkAlloc.BGPPeerSubnet, uplinkAlloc.BGPPeerSubnet6},
				)

				bridgeName = uplinkBridgeName("upflood" + testSuffix)
				Expect(configureUplinkBridge(ctx, f, ictx, bridgeName, nodeIfaces)).To(Succeed())
				Expect(configureUplinkBridgeDefaultRoutes(ictx, bridgeName, nodeIfaces)).To(Succeed())

				uplinkName = "upflood" + testSuffix
				createUplink(ctx, f, ictx, uplinkName, allNodes.Items, nodeIfaces, bridgeName)
				waitForUplinkStatesResolved(ctx, f, uplinkName, bridgeName, allNodes.Items)

				By("creating three external containers")
				for i, target := range []*infraapi.ExternalContainer{&extA, &extB, &extC} {
					port := infraprovider.Get().GetExternalContainerPort()
					container, err := ictx.CreateExternalContainer(infraapi.ExternalContainer{
						Name:    fmt.Sprintf("upflood-ext-%d-%s", i, framework.RandomSuffix()),
						Image:   images.AgnHost(),
						CmdArgs: httpServerContainerCmd(port),
						ExtPort: port,
					})
					framework.ExpectNoError(err)
					*target = container
				}

				extAMAC, extBMAC, extCMAC = externalContainerMAC(extA), externalContainerMAC(extB), externalContainerMAC(extC)
				for _, spec := range []struct {
					c   infraapi.ExternalContainer
					ips *[]string
				}{{extA, &extAIPs}, {extB, &extBIPs}, {extC, &extCIPs}} {
					if isIPv4Supported(cs) {
						*spec.ips = append(*spec.ips, spec.c.GetIPv4())
					}
					if isIPv6Supported(cs) {
						*spec.ips = append(*spec.ips, spec.c.GetIPv6())
					}
				}

				// Purge stale bindings
				extIPs := append(append(append([]string{}, extAIPs...), extBIPs...), extCIPs...)
				purgeDynamicMACBindings(ovnkPod, extIPs)

				By("creating three CUDNs on the uplink")
				cudn1Name = "upflood-cudn1-" + testSuffix
				ns1 := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, cudn1Name, uplinkName)
				cudn1Namespace = ns1.Name
				createUplinkNetexecPod(ctx, f, cudn1Namespace, pod1Name, trafficNode.Name)
				cudn1Port = cudnGatewayRouterExtPortName(cudn1Name, trafficNode.Name)

				cudn2Name = "upflood-cudn2-" + testSuffix
				ns2 := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, cudn2Name, uplinkName)
				cudn2Namespace = ns2.Name
				createUplinkNetexecPod(ctx, f, cudn2Namespace, pod2Name, trafficNode.Name)
				cudn2Port = cudnGatewayRouterExtPortName(cudn2Name, trafficNode.Name)

				cudn3Name = "upflood-cudn3-" + testSuffix
				ns3 := setupUplinkLayer3CUDN(ctx, f, ictx, ipFamilySet, cudn3Name, uplinkName)
				cudn3Namespace = ns3.Name
				createUplinkNetexecPod(ctx, f, cudn3Namespace, pod3Name, trafficNode.Name)
				cudn3Port = cudnGatewayRouterExtPortName(cudn3Name, trafficNode.Name)

				uplinkPorts = []string{cudn1Port, cudn2Port, cudn3Port}
				framework.Logf("CUDN ports on uplink: %v", uplinkPorts)
			})

			It("mirrors MAC binding when CUDN1 pings external container A", func() {
				By("starting ping from CUDN1 to external container A")
				for _, ip := range extAIPs {
					startBackgroundPing("", pod1Name, ip, noFloodPingPidFile(ip))
				}

				By("verifying binding is mirrored to all CUDN ports")
				for _, ip := range extAIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extAMAC, uplinkPorts)
					}, 120*time.Second, 5*time.Second).Should(Succeed())
				}
			})

			It("adds new bindings when CUDN2 pings both container A and new container B", func() {
				By("starting ping from CUDN2 to containers A and B")
				for _, ip := range extAIPs {
					startBackgroundPing("", pod2Name, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing("", pod2Name, ip, noFloodPingPidFile(ip))
				}

				By("verifying container A bindings remain on all ports")
				for _, ip := range extAIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extAMAC, uplinkPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}

				By("verifying container B bindings are mirrored to all ports")
				for _, ip := range extBIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extBMAC, uplinkPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}
			})

			It("maintains bindings when CUDN3 pings containers A, B, and initiates container C traffic", func() {
				By("starting ping from CUDN3 to containers A and B")
				for _, ip := range extAIPs {
					startBackgroundPing("", pod3Name, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing("", pod3Name, ip, noFloodPingPidFile(ip))
				}

				By("verifying A and B bindings remain on all ports")
				Eventually(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredOnAllPorts(g, ip, extAMAC, uplinkPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredOnAllPorts(g, ip, extBMAC, uplinkPorts)
					}
				}, 90*time.Second, 5*time.Second).Should(Succeed())

				By("CUDN1 initiates traffic to container C")
				for _, ip := range extCIPs {
					startBackgroundPing("", pod1Name, ip, noFloodPingPidFile(ip))
				}

				By("verifying container C bindings are mirrored to all ports")
				for _, ip := range extCIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extCMAC, uplinkPorts)
					}, 90*time.Second, 2*time.Second).Should(Succeed())
				}
			})

			It("maintains stability of all bindings while traffic continues", func() {
				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredOnAllPorts(g, ip, extAMAC, uplinkPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredOnAllPorts(g, ip, extBMAC, uplinkPorts)
					}
					for _, ip := range extCIPs {
						expectMirroredOnAllPorts(g, ip, extCMAC, uplinkPorts)
					}
				}, noFloodStabilityDuration, 10*time.Second).Should(Succeed())
			})

			It("ages out all bindings after traffic stops", func() {
				By("stopping all pings")
				stopBackgroundPings("", pod1Name)
				stopBackgroundPings("", pod2Name)
				stopBackgroundPings("", pod3Name)

				By("waiting for all bindings to age out")
				for _, spec := range []struct {
					ips []string
				}{{extAIPs}, {extBIPs}, {extCIPs}} {
					for _, ip := range spec.ips {
						Eventually(func() map[string]string {
							return macBindingPorts(ip)
						}, noFloodAgeOutTimeout, 10*time.Second).Should(BeEmpty(),
							"MAC_Bindings for %s must age out", ip)
					}
				}
			})

			It("keeps mirroring on remaining CUDNs after CUDN1 is deleted", func() {
				By("restarting pings on CUDN2 and CUDN3 to establish fresh bindings")
				// Stop any existing pings first
				stopBackgroundPings(cudn1Namespace, pod1Name)
				stopBackgroundPings(cudn2Namespace, pod2Name)
				stopBackgroundPings(cudn3Namespace, pod3Name)

				// Clear old bindings
				extIPs := append(append(append([]string{}, extAIPs...), extBIPs...), extCIPs...)
				purgeDynamicMACBindings(ovnkPod, extIPs)

				By("starting pings from CUDN2 and CUDN3 to all external containers")
				for _, ip := range extAIPs {
					startBackgroundPing(cudn2Namespace, pod2Name, ip, noFloodPingPidFile(ip))
					startBackgroundPing(cudn3Namespace, pod3Name, ip, noFloodPingPidFile(ip))
				}
				for _, ip := range extBIPs {
					startBackgroundPing(cudn2Namespace, pod2Name, ip, noFloodPingPidFile(ip))
					startBackgroundPing(cudn3Namespace, pod3Name, ip, noFloodPingPidFile(ip))
				}

				By("verifying bindings are established on all CUDN ports before deletion")
				for _, ip := range extAIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extAMAC, uplinkPorts)
					}, 90*time.Second, 5*time.Second).Should(Succeed())
				}
				for _, ip := range extBIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extBMAC, uplinkPorts)
					}, 90*time.Second, 5*time.Second).Should(Succeed())
				}

				// Remaining ports after CUDN1 deletion
				remainingPorts := []string{cudn2Port, cudn3Port}

				By("deleting CUDN1: pod, CUDN, and namespace")
				// Stop ping on CUDN1
				stopBackgroundPings(cudn1Namespace, pod1Name)

				// Delete the CUDN1 pod
				err := cs.CoreV1().Pods(cudn1Namespace).Delete(context.TODO(), pod1Name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "must delete CUDN1 pod in namespace %s", cudn1Namespace)
				}

				// Delete CUDN1
				_, err = e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudn1Name, "--wait", "--timeout=120s")
				framework.ExpectNoError(err, "must delete CUDN1")

				// Delete CUDN1 namespace
				err = cs.CoreV1().Namespaces().Delete(context.TODO(), cudn1Namespace, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "must delete CUDN1 namespace %s", cudn1Namespace)
				}

				By("verifying pings continue successfully on CUDN2 and CUDN3 without failures")
				// Verify bindings have converged to remaining ports only
				for _, ip := range extAIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extAMAC, remainingPorts)
					}, 60*time.Second, 5*time.Second).Should(Succeed(),
						"binding for %s must converge to remaining CUDN ports after CUDN1 deletion", ip)
				}
				for _, ip := range extBIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extBMAC, remainingPorts)
					}, 60*time.Second, 5*time.Second).Should(Succeed(),
						"binding for %s must converge to remaining CUDN ports after CUDN1 deletion", ip)
				}

				By("verifying pings a new target successfully on CUDN2 and CUDN3 without failures")
				for _, ip := range extCIPs {
					startBackgroundPing(cudn2Namespace, pod2Name, ip, noFloodPingPidFile(ip))
					startBackgroundPing(cudn3Namespace, pod3Name, ip, noFloodPingPidFile(ip))
				}

				for _, ip := range extCIPs {
					Eventually(func(g Gomega) {
						expectMirroredOnAllPorts(g, ip, extCMAC, remainingPorts)
					}, 60*time.Second, 5*time.Second).Should(Succeed(),
						"binding for %s must converge to remaining CUDN ports after CUDN1 deletion", ip)
				}

				By("verifying traffic stability on remaining CUDNs")
				Consistently(func(g Gomega) {
					for _, ip := range extAIPs {
						expectMirroredOnAllPorts(g, ip, extAMAC, remainingPorts)
					}
					for _, ip := range extBIPs {
						expectMirroredOnAllPorts(g, ip, extBMAC, remainingPorts)
					}
					for _, ip := range extCIPs {
						expectMirroredOnAllPorts(g, ip, extCMAC, remainingPorts)
					}
				}, 60*time.Second, 10*time.Second).Should(Succeed(),
					"bindings must remain stable on CUDN2 and CUDN3 after CUDN1 deletion")

				By("stopping pings and verifying cleanup")
				stopBackgroundPings(cudn2Namespace, pod2Name)
				stopBackgroundPings(cudn3Namespace, pod3Name)

				By("asserting no bindings remain on deleted CUDN1 port")
				Eventually(func(g Gomega) {
					static, err := listStaticMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range static {
						g.Expect(row.logicalPort).NotTo(Equal(cudn1Port),
							"static bindings of the deleted CUDN1 must be reaped from port %s", cudn1Port)
					}
					dynamic, err := listDynamicMACBindings(ovnkPod)
					g.Expect(err).NotTo(HaveOccurred())
					for _, row := range dynamic {
						g.Expect(row.logicalPort).NotTo(Equal(cudn1Port),
							"dynamic bindings of the deleted CUDN1 must be reaped from port %s", cudn1Port)
					}
				}, 60*time.Second, 5*time.Second).Should(Succeed())
			})
		})
	})

// noFloodPingPidFile returns the pid file to use for a background ping to ip,
// one per IP family so both can run at the same time.
func noFloodPingPidFile(ip string) string {
	if utilnet.IsIPv6String(ip) {
		return noFloodPingPidFileV6
	}
	return noFloodPingPidFileV4
}

// macBindingPortNames returns the logical port names held by m.
func macBindingPortNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}
