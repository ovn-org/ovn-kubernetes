// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	gotesting "testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	fakecore "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ipallocator "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/ip"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/controller/udnenabledsvc"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type failSecondPodPortDeleteClient struct {
	libovsdbclient.Client
	portDeletes int
}

// Transact fails the second port deletion, regardless of NAD iteration order.
func (c *failSecondPodPortDeleteClient) Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	deletesPort := false
	for _, op := range ops {
		if op.Op == ovsdb.OperationDelete && op.Table == nbdb.LogicalSwitchPortTable {
			deletesPort = true
			if c.portDeletes > 0 {
				return nil, fmt.Errorf("injected second NAD cleanup failure")
			}
		}
	}
	results, err := c.Client.Transact(ctx, ops...)
	if err == nil && deletesPort {
		c.portDeletes++
	}
	return results, err
}

var _ = Describe("BaseUserDefinedNetworkController", func() {
	var (
		nad = ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)
	)
	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
	})

	const (
		deleteTestNetworkName  = "bluenet"
		deleteTestNADNamespace = "greenamespace"
		deleteTestNADName      = "rednad"
		deleteTestPodName      = "pod-a"
		deleteTestNodeName     = "node-a"
		deleteTestNADKey       = deleteTestNADNamespace + "/" + deleteTestNADName
	)

	addSecondaryPodNetworkAnnotationForNADWithIP := func(pod *corev1.Pod, nadKey, podIP string) {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		mac, err := net.ParseMAC("0a:58:64:80:00:03")
		Expect(err).NotTo(HaveOccurred())
		ip, ipNet, err := net.ParseCIDR(podIP)
		Expect(err).NotTo(HaveOccurred())
		ipNet.IP = ip
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
			MAC:      mac,
			IPs:      []*net.IPNet{ipNet},
			Role:     types.NetworkRoleSecondary,
			TunnelID: int(ip[len(ip)-1]),
		}, nadKey)
		Expect(err).NotTo(HaveOccurred())
	}
	setSecondaryPodNetworkForNADWithIP := func(pod *corev1.Pod, nadKey, podIP string) {
		pod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: nadKey}
		addSecondaryPodNetworkAnnotationForNADWithIP(pod, nadKey, podIP)
	}
	setSecondaryPodNetworkWithIP := func(pod *corev1.Pod, podIP string) {
		setSecondaryPodNetworkForNADWithIP(pod, deleteTestNADKey, podIP)
	}
	setSecondaryPodNetwork := func(pod *corev1.Pod) {
		setSecondaryPodNetworkWithIP(pod, "100.128.0.3/16")
	}
	newDeleteTestNode := func() *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: deleteTestNodeName,
			Annotations: map[string]string{
				util.OvnNodeChassisID: chassisIDForNode(deleteTestNodeName),
			},
		}}
	}

	It("allows the retry framework to process unscheduled UDN pods", func() {
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, "", "")
		retryEligibilityChecks := map[string]func(interface{}) bool{
			"layer3":   (&Layer3UserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
			"layer2":   (&layer2UserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
			"localnet": (&LocalnetUserDefinedNetworkControllerEventHandler{objType: factory.PodType}).IsResourceScheduled,
		}
		for topology, isResourceScheduled := range retryEligibilityChecks {
			Expect(isResourceScheduled(pod)).To(BeTrue(), topology)
		}
	})

	It("only reports bootstrap pods without retained network state as empty", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		otherNAD := ovntest.GenerateNAD("othernet", "othernad", deleteTestNADNamespace,
			types.Layer3Topology, "10.50.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		ovntest.AnnotateNADWithNetworkID("4", otherNAD)
		unrelated := ovntest.NewPod("unrelated-ns", "unrelated", deleteTestNodeName, "")
		otherNetwork := ovntest.NewPod(deleteTestNADNamespace, "other-network", deleteTestNodeName, "")
		setSecondaryPodNetworkForNADWithIP(otherNetwork, deleteTestNADNamespace+"/othernad", "10.50.0.3/16")
		reserved := ovntest.NewPod(deleteTestNADNamespace, "reserved", deleteTestNodeName, "")
		setSecondaryPodNetwork(reserved)
		detached := ovntest.NewPod(deleteTestNADNamespace, "detached", deleteTestNodeName, "")
		addSecondaryPodNetworkAnnotationForNADWithIP(detached, deleteTestNADKey, "100.128.0.4/16")
		malformed := ovntest.NewPod(deleteTestNADNamespace, "malformed", deleteTestNodeName, "")
		malformed.Annotations = map[string]string{types.OvnPodAnnotationName: "invalid"}
		unknownNAD := ovntest.NewPod(deleteTestNADNamespace, "unknown-nad", deleteTestNodeName, "")
		addSecondaryPodNetworkAnnotationForNADWithIP(unknownNAD, "ns/gone", "100.128.0.5/16")
		partial := ovntest.NewPod(deleteTestNADNamespace, "partial", deleteTestNodeName, "")
		portOnly := ovntest.NewPod("orphan-ns", "port-only", deleteTestNodeName, "")
		pods := []*corev1.Pod{unrelated, otherNetwork, reserved, detached, malformed, unknownNAD, partial, portOnly}
		objects := []runtime.Object{newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad, *otherNAD}}}
		for _, pod := range pods {
			pod.UID = k8stypes.UID(pod.Name)
			objects = append(objects, pod)
		}
		// A port outside the local switch is not removed by the startup sweep.
		// Missing UID/NAD metadata must not let bootstrap forget its cleanup.
		legacyPort := &nbdb.LogicalSwitchPort{
			UUID: "legacy-port-UUID", Name: "legacy-port",
			ExternalIDs: map[string]string{"pod": "true", "namespace": portOnly.Namespace,
				types.NetworkExternalID: deleteTestNetworkName},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{Name: switchName},
			&nbdb.LogicalSwitch{Name: "other-switch", Ports: []string{legacyPort.UUID}}, legacyPort,
		}}, objects...)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Eventually(func() string {
			return bnc.networkManager.GetNetworkNameForNADKey(deleteTestNADNamespace + "/othernad")
		}).Should(Equal("othernet"))
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())
		bnc.rememberIncompletePodPorts(partial, map[string]*lpInfo{
			deleteTestNADKey: {podUID: partial.UID, appliedNetworkName: deleteTestNetworkName},
		})

		emptyPods, err := bnc.SyncPods(pods)
		Expect(err).NotTo(HaveOccurred())
		Expect(emptyPods.UnsortedList()).To(ConsistOf(unrelated.UID, otherNetwork.UID))
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(reserved, deleteTestNADKey),
			ovntest.MustParseIPNets("100.128.0.3/16"))).To(BeTrue(), "an annotation can restore IPAM without an LSP")
	})

	testDesiredNADPortReconcile := func(preserveDesiredPort bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADName := "blue-nad"
		alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)

		oldPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		oldPod.UID = "stable-pod-uid"
		setSecondaryPodNetworkForNADWithIP(oldPod, deleteTestNADKey, "100.128.0.3/16")
		if preserveDesiredPort {
			oldPod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + alternateNADKey
			addSecondaryPodNetworkAnnotationForNADWithIP(oldPod, alternateNADKey, "100.128.0.4/16")
		}
		newPod := oldPod.DeepCopy()
		setSecondaryPodNetworkForNADWithIP(newPod, alternateNADKey, "100.128.0.4/16")

		oldPortName := util.GetUserDefinedNetworkLogicalPortName(oldPod.Namespace, oldPod.Name, deleteTestNADKey)
		newPortName := util.GetUserDefinedNetworkLogicalPortName(newPod.Namespace, newPod.Name, alternateNADKey)
		oldLSP := &nbdb.LogicalSwitchPort{
			UUID:      oldPortName + "-UUID",
			Name:      oldPortName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(oldPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              oldPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID", Name: switchName, Ports: []string{oldLSP.UUID},
		}
		nbData := []libovsdbtest.TestData{logicalSwitch, oldLSP}
		if preserveDesiredPort {
			desiredLSP := &nbdb.LogicalSwitchPort{
				UUID:      newPortName + "-preserved-UUID",
				Name:      newPortName,
				Addresses: []string{"0a:58:64:80:00:03 100.128.0.4"},
				Options:   map[string]string{"iface-id-ver": string(oldPod.UID)},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              oldPod.Namespace,
					types.NetworkExternalID:  deleteTestNetworkName,
					types.NADExternalID:      alternateNADKey,
					types.TopologyExternalID: types.Layer2Topology,
				},
			}
			logicalSwitch.Ports = append(logicalSwitch.Ports, desiredLSP.UUID)
			nbData = append(nbData, desiredLSP)
		}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: nbData},
			newPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		persistedOldLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: oldPortName})
		Expect(err).NotTo(HaveOccurred())
		appliedPortInfo := controller.bnc.logicalPortCache.add(oldPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, persistedOldLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		appliedState := map[string]*lpInfo{deleteTestNADKey: appliedPortInfo}
		var preservedDesiredUUID string
		if preserveDesiredPort {
			persistedDesiredLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient,
				&nbdb.LogicalSwitchPort{Name: newPortName})
			Expect(err).NotTo(HaveOccurred())
			appliedState[alternateNADKey] = controller.bnc.logicalPortCache.add(oldPod, switchName, alternateNADKey,
				deleteTestNetworkName, persistedDesiredLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:03"),
				ovntest.MustParseIPNets("100.128.0.4/16"))
			preservedDesiredUUID = persistedDesiredLSP.UUID
		}

		expected, err := controller.bnc.PodExpectedOnNetwork(newPod)
		Expect(err).NotTo(HaveOccurred())
		Expect(expected).To(BeTrue())
		state, err := controller.bnc.ReconcilePod(oldPod, newPod, appliedState)
		Expect(err).NotTo(HaveOccurred())

		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: oldPortName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
		persistedDesiredLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: newPortName})
		Expect(err).NotTo(HaveOccurred())
		stateMap, ok := state.(map[string]*lpInfo)
		Expect(ok).To(BeTrue())
		Expect(stateMap).To(HaveKey(alternateNADKey))
		Expect(stateMap).NotTo(HaveKey(deleteTestNADKey))
		if preserveDesiredPort {
			Expect(persistedDesiredLSP.UUID).To(Equal(preservedDesiredUUID))
			Expect(stateMap[alternateNADKey].uuid).To(Equal(persistedDesiredLSP.UUID))
		}
	}

	It("replaces a stale NAD port with the newly desired NAD port", func() {
		testDesiredNADPortReconcile(false)
	})

	It("removes only stale NAD ports while preserving an already-applied desired port", func() {
		testDesiredNADPortReconcile(true)
	})

	DescribeTable("cleans ports committed by an unsuccessful reconcile", func(hadAppliedState, invalidateCache, deletePod bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nadBKey := deleteTestNADNamespace + "/blue-nad"
		nadCKey := deleteTestNADNamespace + "/pending-nad"
		var nads []nadapi.NetworkAttachmentDefinition
		for _, name := range []string{deleteTestNADName, "blue-nad", "pending-nad"} {
			nad := ovntest.GenerateNAD(deleteTestNetworkName, name, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			nads = append(nads, *nad)
		}
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.4")
		pod.UID = "stable-pod-uid"
		setSecondaryPodNetworkForNADWithIP(pod, nadBKey, "100.128.0.4/16")
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			pod, newDeleteTestNode(), &nadapi.NetworkAttachmentDefinitionList{Items: nads},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(&nads[0])).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(controller.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		var appliedPod *corev1.Pod
		var state interface{}
		var err error
		if hadAppliedState {
			state, err = controller.ReconcilePod(nil, pod, nil)
			Expect(err).NotTo(HaveOccurred())
			appliedPod = pod
		}
		failedPod := pod.DeepCopy()
		failedPod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + nadBKey + "," + nadCKey
		addSecondaryPodNetworkAnnotationForNADWithIP(failedPod, deleteTestNADKey, "100.128.0.3/16")
		// A and B commit, but C has no cluster-manager IP annotation yet.
		_, err = controller.ReconcilePod(appliedPod, failedPod, state)
		Expect(err).To(MatchError(ContainSubstring("might have not allocated it yet")))
		portA := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, deleteTestNADKey)
		portB := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadBKey)
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portA})
		Expect(err).NotTo(HaveOccurred(), "the failed reconcile actually committed A")
		previousB, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portB})
		Expect(err).NotTo(HaveOccurred())
		if invalidateCache {
			controller.logicalPortCache.invalidatePodForNetwork(pod, deleteTestNetworkName)
		}

		if deletePod {
			// Neither this annotation nor the successful snapshot includes A.
			_, err = controller.ReconcilePod(pod, nil, state)
		} else {
			state, err = controller.ReconcilePod(appliedPod, pod, state)
		}
		Expect(err).NotTo(HaveOccurred())
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portA})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(), "abandoned A must be removed")
		currentB, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portB})
		if deletePod {
			Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
		} else {
			Expect(err).NotTo(HaveOccurred())
			Expect(currentB.UUID).To(Equal(previousB.UUID), "a still-desired port must be preserved")
			Expect(state.(map[string]*lpInfo)).To(HaveLen(1))
			Expect(controller.podPortsForReconcile(pod, state.(map[string]*lpInfo))).To(HaveLen(1),
				"the next reconcile must not rediscover an already-retired cache entry")
		}
		_, pending := controller.incompletePodPorts.Load(podReconcileKey(pod))
		Expect(pending).To(BeFalse(), "successful reconciliation retires partial-state tracking")
	},
		Entry("after a failed first add", false, false, false),
		Entry("after a failed update", true, false, false),
		Entry("after cache invalidation", false, true, false),
		Entry("after a failed update and cache invalidation", true, true, false),
		Entry("on deletion after a failed first add", false, false, true),
		Entry("on deletion after a failed update", true, false, true),
		Entry("on deletion after cache invalidation", false, true, true),
		Entry("on deletion after a failed update and cache invalidation", true, true, true),
	)

	It("does not merge another UID or network's cached ports into partial reconciliation", func() {
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{})
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "")
		pod.UID = "old-uid"
		replacement := pod.DeepCopy()
		replacement.UID = "replacement-uid"
		controller.logicalPortCache.add(pod, "switch", "old-nad", deleteTestNetworkName, "old-port", nil, nil)
		controller.logicalPortCache.add(replacement, "switch", "new-nad", deleteTestNetworkName, "new-port", nil, nil)
		controller.logicalPortCache.add(pod, "switch", "other-nad", "other-network", "other-port", nil, nil)
		oldPorts := controller.podPortsForReconcile(pod, nil)
		Expect(oldPorts).To(HaveLen(1))
		Expect(oldPorts).To(HaveKey("old-nad"))
		controller.incompletePodPorts.Store(podReconcileKey(pod), oldPorts)
		newPorts := controller.podPortsForReconcile(replacement, nil)
		Expect(newPorts).To(HaveLen(1))
		Expect(newPorts).To(HaveKey("new-nad"))
	})

	It("uses a valid pod annotation directly when the applied cache is empty", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		setSecondaryPodNetwork(pod)

		lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:        lspName + "-UUID",
			Name:        lspName,
			Addresses:   []string{"0a:58:64:80:00:03 100.128.0.3"},
			ExternalIDs: map[string]string{"legacy": "true"},
		}
		// If this path queries discovery, this matching malformed row produces
		// an error because it has no NAD external ID. A valid annotation must be
		// sufficient to delete lsp without consulting this row.
		poisonDiscoveryLSP := &nbdb.LogicalSwitchPort{
			UUID: "poison-discovery-UUID",
			Name: "poison-discovery_" + util.GetLogicalPortName(pod.Namespace, pod.Name),
			ExternalIDs: map[string]string{
				"pod":                   "true",
				"namespace":             pod.Namespace,
				types.NetworkExternalID: deleteTestNetworkName,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp, poisonDiscoveryLSP}},
			pod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.getPortInfoForUserDefinedNetwork(pod)).To(BeNil())

		Expect(controller.bnc.removePodForUserDefinedNetwork(pod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	It("deletes an IPAM-less localnet static-IP port with malformed annotation and no cache", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.LocalnetTopology, "", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 192.0.2.25"},
			Options:   map[string]string{"iface-id-ver": "some-other-uid"},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.LocalnetTopology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLocalnetSwitch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		// The deleted pod is intentionally absent from the informer. Stale state
		// is safe to remove regardless of the port's iface-id-ver.
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	It("deletes a discovered Layer3 port after its switch IPAM state is gone", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(deletedPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer3Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}
		// The indexed bucket can also contain other pods and non-pod ports.
		// Neither those nor rows in another namespace/network belong to this delete.
		var unrelatedPorts []*nbdb.LogicalSwitchPort
		for _, kind := range []string{"other-pod", "non-pod", "other-namespace", "other-network"} {
			unrelated := lsp.DeepCopy()
			unrelated.UUID = kind + "-UUID"
			unrelated.Name = kind + "_" + util.GetLogicalPortName(deletedPod.Namespace, deletedPod.Name)
			delete(unrelated.ExternalIDs, types.NADExternalID)
			switch kind {
			case "other-pod":
				unrelated.Name += "-other"
			case "non-pod":
				unrelated.ExternalIDs["pod"] = "false"
			case "other-namespace":
				unrelated.ExternalIDs["namespace"] = "other-namespace"
			case "other-network":
				unrelated.ExternalIDs[types.NetworkExternalID] = "other-network"
			}
			unrelatedPorts = append(unrelatedPorts, unrelated)
			logicalSwitch.Ports = append(logicalSwitch.Ports, unrelated.UUID)
		}
		data := []libovsdbtest.TestData{logicalSwitch, lsp}
		for _, port := range unrelatedPorts {
			data = append(data, port)
		}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: data},
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.lsManager.GetSwitchSubnets(switchName)).To(BeNil())

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
		for _, port := range unrelatedPorts {
			_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: port.Name})
			Expect(err).NotTo(HaveOccurred(), port.Name)
		}
	})

	It("deletes a valid discovered port while retaining a retry for a malformed LSP", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		validLSPName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		validLSP := &nbdb.LogicalSwitchPort{
			UUID:      validLSPName + "-UUID",
			Name:      validLSPName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(deletedPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		malformedLSP := &nbdb.LogicalSwitchPort{
			UUID: "malformed-port-UUID",
			Name: "malformed_" + util.GetLogicalPortName(deletedPod.Namespace, deletedPod.Name),
			ExternalIDs: map[string]string{
				"pod":                   "true",
				"namespace":             deletedPod.Namespace,
				types.NetworkExternalID: deleteTestNetworkName,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID", Name: switchName,
			Ports: []string{validLSP.UUID, malformedLSP.UUID},
		}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, validLSP, malformedLSP}},
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())
		persistedMalformedLSPBeforeDelete, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient,
			&nbdb.LogicalSwitchPort{Name: malformedLSP.Name})
		Expect(err).NotTo(HaveOccurred())

		err = controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)
		Expect(err).To(MatchError(ContainSubstring("has no NAD external ID")))
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: validLSP.Name})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
		persistedMalformedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient,
			&nbdb.LogicalSwitchPort{Name: malformedLSP.Name})
		Expect(err).NotTo(HaveOccurred())
		Expect(persistedMalformedLSP.UUID).To(Equal(persistedMalformedLSPBeforeDelete.UUID))
	})

	It("preserves replacement and unclassified cache state after partial discovery", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "current-pod-uid"
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		currentLSPName := util.GetUserDefinedNetworkLogicalPortName(currentPod.Namespace, currentPod.Name, deleteTestNADKey)
		currentLSP := &nbdb.LogicalSwitchPort{
			UUID:      currentLSPName + "-UUID",
			Name:      currentLSPName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(currentPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              currentPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		malformedLSP := &nbdb.LogicalSwitchPort{
			UUID: "malformed-replacement-port-UUID",
			Name: "malformed_" + util.GetLogicalPortName(currentPod.Namespace, currentPod.Name),
			ExternalIDs: map[string]string{
				"pod":                   "true",
				"namespace":             currentPod.Namespace,
				types.NetworkExternalID: deleteTestNetworkName,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID", Name: switchName,
			Ports: []string{currentLSP.UUID, malformedLSP.UUID},
		}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, currentLSP, malformedLSP}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		persistedCurrentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient,
			&nbdb.LogicalSwitchPort{Name: currentLSP.Name})
		Expect(err).NotTo(HaveOccurred())
		currentPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, persistedCurrentLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		unclassifiedNADKey := deleteTestNADNamespace + "/unclassified-nad"
		unclassifiedPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, unclassifiedNADKey,
			deleteTestNetworkName, "unclassified-cache-UUID", ovntest.MustParseMAC("0a:58:64:80:00:05"),
			ovntest.MustParseIPNets("100.128.0.5/16"))

		err = controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey:   currentPortInfo,
			unclassifiedNADKey: unclassifiedPortInfo,
		})
		Expect(err).To(MatchError(ContainSubstring("has no NAD external ID")))
		actualCurrentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient,
			&nbdb.LogicalSwitchPort{Name: currentLSP.Name})
		Expect(err).NotTo(HaveOccurred())
		Expect(actualCurrentLSP.UUID).To(Equal(persistedCurrentLSP.UUID))

		cachedCurrentPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedCurrentPortInfo.uuid).To(Equal(currentPortInfo.uuid))
		Expect(cachedCurrentPortInfo.expires.IsZero()).To(BeTrue())
		cachedUnclassifiedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, unclassifiedNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedUnclassifiedPortInfo.uuid).To(Equal(unclassifiedPortInfo.uuid))
		Expect(cachedUnclassifiedPortInfo.expires.IsZero()).To(BeTrue())
	})

	DescribeTable("classifies same-name replacement ports without retrying on a missing iface-id-ver",
		func(ifaceIDVer string, currentWantsNetwork, expectDeleted bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			currentPod.UID = "new-pod-uid"
			if currentWantsNetwork {
				setSecondaryPodNetwork(currentPod)
			}
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
			lsp := &nbdb.LogicalSwitchPort{
				UUID:      lspName + "-UUID",
				Name:      lspName,
				Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              deletedPod.Namespace,
					types.NetworkExternalID:  deleteTestNetworkName,
					types.NADExternalID:      deleteTestNADKey,
					types.TopologyExternalID: types.Layer2Topology,
				},
			}
			if ifaceIDVer != "" {
				lsp.Options = map[string]string{"iface-id-ver": ifaceIDVer}
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

			fakeOVN := NewFakeOVN(false, deleteTestNodeName)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: {uuid: "stale-cache-UUID", logicalSwitch: switchName, appliedNetworkName: deleteTestNetworkName},
			})).To(Succeed())

			_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			if expectDeleted {
				Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		},
		Entry("deletes a port owned by the deleted UID", "deleted-pod-uid", true, true),
		Entry("preserves a port owned by the current UID", "new-pod-uid", false, false),
		Entry("preserves a desired legacy port", "", true, false),
		Entry("deletes an undesired legacy port", "", false, true),
	)

	It("retries deletion of an ambiguous legacy port until replacement attachments are known", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		currentPod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: "["}
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		err := controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)
		Expect(err).To(MatchError(ContainSubstring("cannot determine current network attachments for replacement pod")))
		persistedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		Expect(persistedLSP.Name).To(Equal(lsp.Name))
		Expect(persistedLSP.Addresses).To(Equal(lsp.Addresses))
		Expect(persistedLSP.ExternalIDs).To(Equal(lsp.ExternalIDs))

		updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(currentPod.Namespace).Get(
			context.TODO(), currentPod.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		delete(updatedPod.Annotations, nadapi.NetworkAttachmentAnnot)
		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(updatedPod.Namespace).Update(
			context.TODO(), updatedPod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			informerPod, err := controller.bnc.watchFactory.GetPod(currentPod.Namespace, currentPod.Name)
			if err != nil {
				return nadapi.NetworkAttachmentAnnot
			}
			return informerPod.Annotations[nadapi.NetworkAttachmentAnnot]
		}).Should(BeEmpty())

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	})

	DescribeTable("expires cache-only applied state when a same-name replacement has no LSP", func(withPolicy bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName}
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		stalePortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, "deleted-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeFalse())
		if withPolicy {
			policy := getPortNetworkPolicy("replacement-policy", currentPod.Namespace, "role", "selected", 80)
			policy.Spec.PodSelector = metav1.LabelSelector{}
			np := NewNetworkPolicy(policy)
			selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
			Expect(err).NotTo(HaveOccurred())
			np.localPodSelector = selector
			controller.bnc.networkPolicies.Store(np.getKey(), np)
			controller.bnc.addNetworkPolicyToNamespaceIndex(np)
		}

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: stalePortInfo,
		})).To(Succeed())

		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(stalePortInfo.uuid))
		Expect(cachedPortInfo.expires.IsZero()).To(BeFalse())
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeTrue())
		if withPolicy {
			// Normal replacement setup must still retry until its port is available.
			Expect(controller.bnc.reconcileNetworkPolicyPortGroups(currentPod)).NotTo(Succeed())
		}
	},
		Entry("without a selecting policy", false),
		Entry("with a selecting policy", true),
	)

	It("preserves a newer cache entry when a stale delete snapshot has no LSP", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName}
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]

		currentPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey,
			deleteTestNetworkName, "current-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ovntest.MustParseIPNets("100.128.0.3/16"))
		stalePortInfo := &lpInfo{
			uuid:               "deleted-port-UUID",
			logicalSwitch:      switchName,
			appliedNetworkName: deleteTestNetworkName,
			mac:                ovntest.MustParseMAC("0a:58:64:80:00:03"),
			ips:                ovntest.MustParseIPNets("100.128.0.3/16"),
		}

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: stalePortInfo,
		})).To(Succeed())

		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(currentPortInfo.uuid))
		Expect(cachedPortInfo.expires.IsZero()).To(BeTrue())
		Expect(controller.bnc.shouldEnsurePodForUserDefinedNetwork(currentPod)).To(BeFalse())
	})

	DescribeTable("releases cache-only Layer3 IPAM state only when a replacement does not use the IP",
		func(replacementIP string, expectReleased bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, replacementIP)
			currentPod.UID = "new-pod-uid"
			setSecondaryPodNetworkWithIP(currentPod, replacementIP+"/16")
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			fakeOVN := NewFakeOVN(false, deleteTestNodeName)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			switchName := controller.bnc.GetNetworkScopedSwitchName(deleteTestNodeName)
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
				ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

			staleIPs := ovntest.MustParseIPNets("100.128.0.3/16")
			owner := deletedPod
			if !expectReleased {
				owner = currentPod
			}
			Expect(controller.bnc.lsManager.AllocateIPs(switchName, controller.bnc.podIPOwner(owner, deleteTestNADKey), staleIPs)).To(Succeed())
			stalePortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, "deleted-port-UUID", ovntest.MustParseMAC("0a:58:64:80:00:03"), staleIPs)

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: stalePortInfo,
			})).To(Succeed())

			err := controller.bnc.lsManager.AllocateIPs(switchName, "test-owner", staleIPs)
			if expectReleased {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
			}
		},
		Entry("when the replacement uses a different IP", "100.128.0.4", true),
		Entry("but preserves it when the replacement uses the same IP", "100.128.0.3", false),
	)

	DescribeTable("preserves reused IPs after partial multi-NAD pod cleanup", func(reattach bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode, config.IPv6Mode = true, true
		const networkName, namespace, nodeName = "bluenet", "greenamespace", "node-a"
		nadKeys := []string{namespace + "/rednad", namespace + "/orangenad"}
		nads := &nadapi.NetworkAttachmentDefinitionList{}
		for _, name := range []string{"rednad", "orangenad"} {
			nad := ovntest.GenerateNAD(networkName, name, namespace,
				types.Layer3Topology, "100.128.0.0/16,fd00::/48", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			nads.Items = append(nads.Items, *nad)
		}
		podA := ovntest.NewPod(namespace, "pod-a", nodeName, "10.128.0.3")
		podA.UID = "pod-a-uid"
		podA.Status.Phase = corev1.PodRunning
		podA.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: nadKeys[0] + "," + nadKeys[1]}
		var err error
		for i, nadKey := range nadKeys {
			ips := ovntest.MustParseIPNets(fmt.Sprintf("100.128.0.%d/24", i+3), fmt.Sprintf("fd00::%d/64", i+3))
			podA.Annotations, err = util.MarshalPodAnnotation(podA.Annotations, &util.PodAnnotation{
				MAC: util.IPAddrToHWAddr(ips[0].IP), IPs: ips, Role: types.NetworkRoleSecondary,
			}, nadKey)
			Expect(err).NotTo(HaveOccurred())
		}

		switchName := util.GetUserDefinedNetworkPrefix(networkName) + nodeName
		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			podA, ovntest.NewNamespace(namespace), newNode(nodeName, "192.0.2.10/24"), nads,
		)
		DeferCleanup(fakeOVN.shutdown)
		bnc := fakeOVN.userDefinedNetworkControllers[networkName].bnc
		for _, nadKey := range nadKeys {
			Eventually(func() string {
				return bnc.networkManager.GetNetworkNameForNADKey(nadKey)
			}).Should(Equal(networkName))
		}
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/24", "fd00::/64"), nil, nil)).To(Succeed())
		podPorts := func(pod *corev1.Pod) ([]*nbdb.LogicalSwitchPort, error) {
			return libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(lsp *nbdb.LogicalSwitchPort) bool {
				return lsp.Options["iface-id-ver"] == string(pod.UID)
			})
		}
		deletePod := func(pod *corev1.Pod, state map[string]*lpInfo) error {
			Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
			Eventually(func() error {
				_, err := fakeOVN.watcher.GetPod(namespace, pod.Name)
				return err
			}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"))
			_, err := bnc.ReconcilePod(pod, nil, state)
			if err != nil {
				return err
			}
			return nil
		}

		By("programming both of pod A's attachments through the controller")
		Expect(bnc.ensurePodForUserDefinedNetwork(podA, true, "ErrorAddingResource")).To(Succeed())
		podA, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), podA.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		portInfoMap, err := bnc.logicalPortCache.getAll(podA)
		Expect(err).NotTo(HaveOccurred())
		Expect(portInfoMap).To(HaveLen(2))
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(2))

		By("releasing one attachment before cleanup of the second attachment fails")
		bnc.nbClient = &failSecondPodPortDeleteClient{Client: fakeOVN.nbClient}
		err = bnc.removePodForUserDefinedNetwork(podA, portInfoMap)
		Expect(err).To(MatchError(ContainSubstring("injected second NAD cleanup failure")))
		bnc.nbClient = fakeOVN.nbClient
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(1))
		remainingPorts, err := podPorts(podA)
		Expect(err).NotTo(HaveOccurred())
		var releasedNAD string
		for nadKey, info := range portInfoMap {
			if info.uuid != remainingPorts[0].UUID {
				releasedNAD = nadKey
			}
		}
		Expect(releasedNAD).NotTo(BeEmpty())
		annotationA, err := util.UnmarshalPodAnnotation(podA.Annotations, releasedNAD)
		Expect(err).NotTo(HaveOccurred())

		By("allocating the freed IPs and programming a logical port for pod B")
		podB := ovntest.NewPod(namespace, "pod-b", nodeName, "10.128.0.4")
		podB.UID = "pod-b-uid"
		podB.Status.Phase = corev1.PodRunning
		podB.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: releasedNAD}
		podB, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Create(context.Background(), podB, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (*corev1.Pod, error) { return fakeOVN.watcher.GetPod(namespace, podB.Name) }).ShouldNot(BeNil())
		Expect(bnc.ensurePodForUserDefinedNetwork(podB, true, "ErrorAddingResource")).To(Succeed())
		podB, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), podB.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		annotationB, err := util.UnmarshalPodAnnotation(podB.Annotations, releasedNAD)
		Expect(err).NotTo(HaveOccurred())
		Expect(annotationB.IPs).To(Equal(annotationA.IPs))
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podB) }).Should(HaveLen(1))
		ownerPorts, err := podPorts(podB)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerPorts[0].Addresses).To(ConsistOf(annotationB.MAC.String() + " " + util.JoinIPNetIPs(annotationB.IPs, " ")))

		By("rejecting pod A's stale reattachment without creating a duplicate-IP logical port")
		err = bnc.ensurePodForUserDefinedNetwork(podA, true, "ErrorAddingResource")
		Expect(err).To(HaveOccurred())
		Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
		Expect(podPorts(podA)).To(HaveLen(1))

		if reattach {
			By("keeping pod B's reservation until its cleanup completes")
			Expect(deletePod(podB, nil)).To(Succeed())
			By("reattaching pod A only after it can reacquire the IPs")
			Expect(bnc.ensurePodForUserDefinedNetwork(podA, true, "ErrorAddingResource")).To(Succeed())
			Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(HaveLen(2))
			portInfoMap, err = bnc.logicalPortCache.getAll(podA)
			Expect(err).NotTo(HaveOccurred())
			Expect(deletePod(podA, portInfoMap)).To(Succeed())
		} else {
			By("retrying pod A's cleanup without releasing pod B's reservation")
			Expect(deletePod(podA, portInfoMap)).To(Succeed())
			for _, ip := range annotationB.IPs {
				Expect(ipallocator.IsErrAllocated(bnc.lsManager.AllocateIPs(switchName, "test-owner", []*net.IPNet{ip}))).To(
					BeTrue(), "pod A's cleanup retry must preserve pod B's reservation for %s", ip)
			}
			Expect(podPorts(podB)).To(Equal(ownerPorts))
			Expect(deletePod(podB, nil)).To(Succeed())
		}

		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podA) }).Should(BeEmpty())
		Eventually(func() ([]*nbdb.LogicalSwitchPort, error) { return podPorts(podB) }).Should(BeEmpty())
		for _, info := range portInfoMap {
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", info.ips)).To(Succeed(), "final cleanup must release every attachment's IPs")
		}
	},
		Entry("cleanup retries preserve the new owner's IPs", false),
		Entry("reattachment requires successful IP reacquisition", true),
	)

	DescribeTable("restores ownership after a completed pod's IPs are reused", func(defaultNetwork, completedOwner, ownerFirst bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.IPv4Mode, config.IPv6Mode = true, true
		const namespace, nodeName, networkName = "greenamespace", "node-a", "bluenet"
		nadKey, switchName := namespace+"/rednad", util.GetUserDefinedNetworkPrefix(networkName)+nodeName
		role := types.NetworkRoleSecondary
		ips := ovntest.MustParseIPNets("100.128.0.3/24", "fd00::3/64")
		subnets := ovntest.MustParseIPNets("100.128.0.0/24", "fd00::/64")
		if defaultNetwork {
			nadKey, switchName, role = types.DefaultNetworkName, nodeName, types.NetworkRolePrimary
			ips = ovntest.MustParseIPNets("10.128.1.3/24", "fd00::3/64")
			subnets = ovntest.MustParseIPNets("10.128.1.0/24", "fd00::/64")
		}
		annotation := &util.PodAnnotation{IPs: ips, MAC: util.IPAddrToHWAddr(ips[0].IP), Role: role}
		stale := ovntest.NewPod(namespace, "old-pod", nodeName, ips[0].IP.String())
		stale.Status.Phase = corev1.PodRunning
		stale.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(100, 0))}}
		stale.Annotations = map[string]string{}
		if !defaultNetwork {
			stale.Annotations[nadapi.NetworkAttachmentAnnot] = nadKey
		}
		var err error
		stale.Annotations, err = util.MarshalPodAnnotation(stale.Annotations, annotation, nadKey)
		Expect(err).NotTo(HaveOccurred())
		nad := ovntest.GenerateNAD(networkName, "rednad", namespace, types.Layer3Topology,
			"100.128.0.0/16,fd00::/48", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		fakeOVN := NewFakeOVN(true, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{Name: switchName},
		}}, stale, ovntest.NewNamespace(namespace), newNode(nodeName, "192.0.2.10/24"),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}})
		DeferCleanup(fakeOVN.shutdown)
		var bnc *BaseNetworkController
		var syncPods func([]interface{}) error
		var removePod func(*corev1.Pod) error
		var addPod func(*corev1.Pod) error
		if defaultNetwork {
			bnc = &fakeOVN.controller.BaseNetworkController
			syncPods = fakeOVN.controller.syncPods
			removePod = func(p *corev1.Pod) error { return fakeOVN.controller.removePod(p, nil) }
			addPod = fakeOVN.controller.addLogicalPort
			Expect(fakeOVN.controller.WatchNamespaces()).To(Succeed())
		} else {
			controller := fakeOVN.userDefinedNetworkControllers[networkName].bnc
			bnc = &controller.BaseNetworkController
			syncPods = controller.syncPodsForUserDefinedNetwork
			removePod = func(p *corev1.Pod) error { return controller.removePodForUserDefinedNetwork(p, nil) }
			addPod = func(p *corev1.Pod) error {
				return controller.ensurePodForUserDefinedNetwork(p, true, "ErrorAddingResource")
			}
			Eventually(func() string { return bnc.networkManager.GetNetworkNameForNADKey(nadKey) }).Should(Equal(networkName))
		}
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, subnets, nil, nil)).To(Succeed())
		By("completing the first pod and releasing its allocation without removing its annotation")
		Expect(addPod(stale)).To(Succeed())
		stale.Status.Phase = corev1.PodSucceeded
		stale, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).UpdateStatus(context.Background(), stale, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(removePod(stale)).To(Succeed())
		By("allocating the freed addresses to a different pod")
		owner := ovntest.NewPod(namespace, "owner", nodeName, "")
		owner.Status.Phase = corev1.PodRunning
		owner.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Unix(200, 0))}}
		if !defaultNetwork {
			owner.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: nadKey}
		}
		owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Create(context.Background(), owner, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() (*corev1.Pod, error) { return bnc.watchFactory.GetPod(namespace, owner.Name) }).ShouldNot(BeNil())
		Expect(addPod(owner)).To(Succeed())
		owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Get(context.Background(), owner.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		ownerAnnotation, err := util.UnmarshalPodAnnotation(owner.Annotations, nadKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(ownerAnnotation.IPs).To(Equal(ips))
		if completedOwner {
			owner.Status.Phase = corev1.PodSucceeded
			owner, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).UpdateStatus(context.Background(), owner, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		By("restarting IPAM before the second pod is cleaned up")
		// No pod handlers are running in this test. Keep the API and OVN
		// state from the lifecycle above, but rebuild the in-memory allocator.
		bnc.lsManager = logicalswitchmanager.NewLogicalSwitchManager()
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, subnets, nil, nil)).To(Succeed())
		Expect(syncPods([]interface{}{stale, owner})).To(Succeed())
		Expect(syncPods([]interface{}{owner, stale})).To(Succeed(), "startup retries must preserve the selected owner")
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(owner, nadKey), ips)).To(BeTrue())
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(stale, nadKey), ips)).To(BeFalse())
		first, last := stale, owner
		if ownerFirst {
			first, last = owner, stale
		}
		By("cleaning up both pods without releasing the current owner early or leaking its allocation")
		Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespace).Delete(context.Background(), owner.Name, metav1.DeleteOptions{})).To(Succeed())
		Expect(removePod(first)).To(Succeed())
		if !ownerFirst {
			Expect(ipallocator.IsErrAllocated(bnc.lsManager.AllocateIPs(switchName, "test-owner", ips))).To(BeTrue())
		}
		Expect(removePod(last)).To(Succeed())
		Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", ips)).To(Succeed(), "final cleanup must not leak the reservation")
	},
		Entry("default, running owner, stale first", true, false, false),
		Entry("default, running owner, owner first", true, false, true),
		Entry("default, completed owner, stale first", true, true, false),
		Entry("default, completed owner, owner first", true, true, true),
		Entry("UDN, running owner, stale first", false, false, false),
		Entry("UDN, running owner, owner first", false, false, true),
		Entry("UDN, completed owner, stale first", false, true, false),
		Entry("UDN, completed owner, owner first", false, true, true),
	)

	DescribeTable("protects reused IPs after detaching a NAD", func(deletePod bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADName := "blue-nad"
		alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)

		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		pod.UID = "pod-uid"
		setSecondaryPodNetworkForNADWithIP(pod, deleteTestNADKey, "100.128.0.3/16")
		pod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + alternateNADKey
		addSecondaryPodNetworkAnnotationForNADWithIP(pod, alternateNADKey, "100.128.0.4/16")

		detachedPod := pod.DeepCopy()
		detachedPod.Annotations[nadapi.NetworkAttachmentAnnot] = alternateNADKey

		otherPod := ovntest.NewPod(deleteTestNADNamespace, "pod-b", deleteTestNodeName, "100.128.0.3")
		otherPod.UID = "other-pod-uid"
		setSecondaryPodNetworkForNADWithIP(otherPod, deleteTestNADKey, "100.128.0.3/16")

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			pod,
			otherPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		state, err := bnc.ReconcilePod(nil, pod, nil)
		Expect(err).NotTo(HaveOccurred())
		state, err = bnc.ReconcilePod(pod, detachedPod, state)
		Expect(err).NotTo(HaveOccurred())
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(pod, deleteTestNADKey),
			ovntest.MustParseIPNets("100.128.0.3/16"))).To(BeFalse())

		_, err = bnc.ReconcilePod(nil, otherPod, nil)
		Expect(err).NotTo(HaveOccurred(), "another pod should be able to reserve the released IP")

		if deletePod {
			_, err = bnc.ReconcilePod(detachedPod, nil, state)
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(otherPod, deleteTestNADKey),
				ovntest.MustParseIPNets("100.128.0.3/16"))).To(BeTrue(),
				"deleting the original pod must not release the other pod's allocation")
			return
		}

		_, err = bnc.ReconcilePod(detachedPod, pod, state)
		Expect(err).To(HaveOccurred())
		Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{
			Name: bnc.GetLogicalPortName(pod, deleteTestNADKey),
		})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(),
			"reattachment must not create a second LSP with the other pod's IP")
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(otherPod, deleteTestNADKey),
			ovntest.MustParseIPNets("100.128.0.3/16"))).To(BeTrue())
	},
		Entry("when the detached NAD is selected again", false),
		Entry("when the original pod is deleted", true),
	)

	DescribeTable("validates detached NAD ownership after controller restart", func(deletePod, reselectBeforeRestart bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADName := "blue-nad"
		alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)
		nads := []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}

		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		pod.UID = "pod-uid"
		setSecondaryPodNetworkForNADWithIP(pod, deleteTestNADKey, "100.128.0.3/16")
		pod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + alternateNADKey
		addSecondaryPodNetworkAnnotationForNADWithIP(pod, alternateNADKey, "100.128.0.4/16")

		detachedPod := pod.DeepCopy()
		detachedPod.Annotations[nadapi.NetworkAttachmentAnnot] = alternateNADKey

		otherPod := ovntest.NewPod(deleteTestNADNamespace, "pod-b", deleteTestNodeName, "100.128.0.3")
		otherPod.UID = "other-pod-uid"
		setSecondaryPodNetworkForNADWithIP(otherPod, deleteTestNADKey, "100.128.0.3/16")

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			pod,
			otherPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: nads},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		state, err := bnc.ReconcilePod(nil, pod, nil)
		Expect(err).NotTo(HaveOccurred())
		state, err = bnc.ReconcilePod(pod, detachedPod, state)
		Expect(err).NotTo(HaveOccurred())
		_, err = bnc.ReconcilePod(nil, otherPod, nil)
		Expect(err).NotTo(HaveOccurred(), "another pod should be able to reserve the released IP")

		bootstrapPod := detachedPod
		if reselectBeforeRestart {
			bootstrapPod = pod
			_, err = bnc.ReconcilePod(detachedPod, pod, state)
			Expect(err).To(HaveOccurred(), "ownership should reject the conflicting reattachment")
			Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
		}

		// Recreate all controller-local state while preserving the API and NBDB
		// state. Ownership must be reconstructed from durable state.
		logicalSwitch, err := libovsdbops.GetLogicalSwitch(fakeOVN.nbClient, &nbdb.LogicalSwitch{Name: switchName})
		Expect(err).NotTo(HaveOccurred())
		ports, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(*nbdb.LogicalSwitchPort) bool { return true })
		Expect(err).NotTo(HaveOccurred())
		dbData := []libovsdbtest.TestData{logicalSwitch}
		for _, port := range ports {
			dbData = append(dbData, port)
		}

		restartedOVN := NewFakeOVN(false, deleteTestNodeName)
		restartedOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: dbData},
			bootstrapPod,
			otherPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: nads},
		)
		DeferCleanup(restartedOVN.shutdown)
		Expect(restartedOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc = restartedOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())
		_, syncErr := bnc.SyncPods([]*corev1.Pod{bootstrapPod, otherPod})
		Expect(syncErr).NotTo(HaveOccurred())

		state, err = bnc.ReconcilePod(nil, bootstrapPod, nil)
		if reselectBeforeRestart {
			Expect(err).To(HaveOccurred(), "a selected NAD must re-establish ownership after restart")
			Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
			_, lspErr := libovsdbops.GetLogicalSwitchPort(bnc.nbClient, &nbdb.LogicalSwitchPort{
				Name: bnc.GetLogicalPortName(pod, deleteTestNADKey),
			})
			Expect(errors.Is(lspErr, libovsdbclient.ErrNotFound)).To(BeTrue(),
				"the stale annotation must not be programmed on a second LSP")
			return
		}
		Expect(err).NotTo(HaveOccurred())

		if deletePod {
			_, err = bnc.ReconcilePod(detachedPod, nil, state)
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner",
				ovntest.MustParseIPNets("100.128.0.3/16"))).To(MatchError(ipallocator.ErrAllocatedByOther),
				"deleting the stale pod must preserve the current owner's allocation")
			return
		}

		_, err = bnc.ReconcilePod(detachedPod, pod, state)
		Expect(err).To(HaveOccurred(), "reattachment must reject the current owner's allocation")
		Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
		_, lspErr := libovsdbops.GetLogicalSwitchPort(bnc.nbClient, &nbdb.LogicalSwitchPort{
			Name: bnc.GetLogicalPortName(pod, deleteTestNADKey),
		})
		Expect(errors.Is(lspErr, libovsdbclient.ErrNotFound)).To(BeTrue(),
			"reattachment must not create a second LSP with the current owner's IP")
	},
		Entry("before reattaching a detached NAD", false, false),
		Entry("before deleting a pod with a stale detached-NAD annotation", true, false),
		Entry("after the detached NAD was selected again", false, true),
	)

	DescribeTable("finishes a pending NAD detach after restart", func(fullyDetached bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADKey := deleteTestNADNamespace + "/blue-nad"
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, "blue-nad", deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)
		pod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "")
		setSecondaryPodNetworkForNADWithIP(pod, deleteTestNADKey, "100.128.0.3/16")
		pod.Annotations[nadapi.NetworkAttachmentAnnot] = alternateNADKey
		addSecondaryPodNetworkAnnotationForNADWithIP(pod, alternateNADKey, "100.128.0.4/16")
		if fullyDetached {
			delete(pod.Annotations, nadapi.NetworkAttachmentAnnot)
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		oldPort := &nbdb.LogicalSwitchPort{
			UUID: "old-port-UUID", Name: util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, deleteTestNADKey),
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(pod.UID)},
			ExternalIDs: map[string]string{
				"pod": "true", "namespace": pod.Namespace,
				types.NetworkExternalID: deleteTestNetworkName, types.NADExternalID: deleteTestNADKey,
				types.TopologyExternalID: types.Layer3Topology,
			},
		}
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
			&nbdb.LogicalSwitch{Name: switchName, Ports: []string{oldPort.UUID}}, oldPort,
		}}, pod, newDeleteTestNode(), &nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}})
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())
		_, syncErr := bnc.SyncPods([]*corev1.Pod{pod})
		Expect(syncErr).NotTo(HaveOccurred())
		oldIPs := ovntest.MustParseIPNets("100.128.0.3/16")
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(pod, deleteTestNADKey), oldIPs)).To(BeTrue())
		var err error
		if fullyDetached {
			_, err = bnc.ReconcilePod(pod, nil, nil)
		} else {
			_, err = bnc.ReconcilePod(nil, pod, nil)
		}
		Expect(err).NotTo(HaveOccurred())
		_, err = libovsdbops.GetLogicalSwitchPort(bnc.nbClient, &nbdb.LogicalSwitchPort{Name: oldPort.Name})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
		Expect(bnc.lsManager.AllocateIPs(switchName, "new-owner", oldIPs)).To(Succeed())
	},
		Entry("while another NAD remains selected", false),
		Entry("when the whole network was deselected", true),
	)

	DescribeTable("protects post-restart allocations before their pod annotations are published", func(deletePod bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADName := "blue-nad"
		alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)
		nads := []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}

		// This is the API state after a controller restart: the old pod is no
		// longer attached to NAD A, but its immutable network annotation still
		// contains A's released address.
		oldPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "")
		oldPod.UID = "old-pod-uid"
		oldPod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: alternateNADKey}
		addSecondaryPodNetworkAnnotationForNADWithIP(oldPod, deleteTestNADKey, "100.128.0.3/16")
		addSecondaryPodNetworkAnnotationForNADWithIP(oldPod, alternateNADKey, "100.128.0.4/16")

		newOwner := ovntest.NewPod(deleteTestNADNamespace, "new-owner", deleteTestNodeName, "")
		newOwner.UID = "new-owner-uid"
		newOwner.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: deleteTestNADKey}

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			oldPod,
			newOwner,
			newNode(deleteTestNodeName, "192.0.2.10/24"),
			&nadapi.NetworkAttachmentDefinitionList{Items: nads},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())
		_, syncErr := bnc.SyncPods([]*corev1.Pod{oldPod, newOwner})
		Expect(syncErr).NotTo(HaveOccurred())

		state, err := bnc.ReconcilePod(nil, oldPod, nil)
		Expect(err).NotTo(HaveOccurred())

		reattachedPod := oldPod.DeepCopy()
		reattachedPod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + alternateNADKey
		if !deletePod {
			_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(oldPod.Namespace).Update(
				context.Background(), reattachedPod, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() string {
				pod, getErr := bnc.watchFactory.GetPod(oldPod.Namespace, oldPod.Name)
				if getErr != nil {
					return ""
				}
				return pod.Annotations[nadapi.NetworkAttachmentAnnot]
			}).Should(Equal(deleteTestNADKey + "," + alternateNADKey))
		}

		// Different pod keys reconcile concurrently. Pause the new owner's real
		// reconcile after IPAM reserves the old address but before the API patch
		// makes that ownership observable through the informer.
		patchStarted := make(chan struct{})
		patchContinue := make(chan struct{})
		var patchStartedOnce, patchContinueOnce sync.Once
		fakeOVN.fakeClient.KubeClient.(*fakecore.Clientset).PrependReactor("patch", "pods",
			func(action ktesting.Action) (bool, runtime.Object, error) {
				patch := action.(ktesting.PatchAction)
				if patch.GetName() != newOwner.Name {
					return false, nil, nil
				}
				patchStartedOnce.Do(func() { close(patchStarted) })
				<-patchContinue
				return false, nil, nil
			})

		ownerFinished := make(chan struct{})
		var ownerErr error
		go func() {
			defer close(ownerFinished)
			_, ownerErr = bnc.ReconcilePod(nil, newOwner, nil)
		}()
		finishOwner := func() {
			patchContinueOnce.Do(func() { close(patchContinue) })
			select {
			case <-ownerFinished:
			case <-time.After(5 * time.Second):
				Fail("new owner reconcile did not finish")
			}
			Expect(ownerErr).NotTo(HaveOccurred())
		}
		DeferCleanup(finishOwner)

		select {
		case <-patchStarted:
		case <-ownerFinished:
			Fail(fmt.Sprintf("new owner returned before its annotation patch: %v", ownerErr))
		case <-time.After(5 * time.Second):
			Fail("new owner did not reach its annotation patch")
		}
		ownerIPs := ovntest.MustParseIPNets("100.128.0.3/16")
		Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", ownerIPs)).To(MatchError(ipallocator.ErrAllocatedByOther),
			"the new owner's reservation must already exist")
		cachedOwner, err := bnc.watchFactory.GetPod(newOwner.Namespace, newOwner.Name)
		Expect(err).NotTo(HaveOccurred())
		_, err = util.UnmarshalPodAnnotation(cachedOwner.Annotations, deleteTestNADKey)
		Expect(util.IsAnnotationNotSetError(err)).To(BeTrue(), "the new owner's annotation is still unpublished")

		if deletePod {
			_, err = bnc.ReconcilePod(oldPod, nil, state)
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", ownerIPs)).To(MatchError(ipallocator.ErrAllocatedByOther),
				"stale deletion must preserve the unpublished allocation")
			finishOwner()
			return
		}

		_, err = bnc.ReconcilePod(oldPod, reattachedPod, state)
		Expect(errors.Is(err, ipallocator.ErrAllocatedByOther)).To(BeTrue(),
			"stale reattachment must not use the unpublished allocation")
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{
			Name: bnc.GetLogicalPortName(oldPod, deleteTestNADKey),
		})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(),
			"stale reattachment must not create a duplicate LSP")
		finishOwner()
	},
		Entry("during stale-pod reattachment", false),
		Entry("during stale-pod deletion", true),
	)

	DescribeTable("protects bootstrap-restored allocations until owner teardown", func(deletePod, ownerCompleted bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		alternateNADName := "blue-nad"
		alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
		nadA := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		nadB := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nadA)
		ovntest.AnnotateNADWithNetworkID("3", nadB)

		oldPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "")
		oldPod.UID = "old-pod-uid"
		setSecondaryPodNetworkForNADWithIP(oldPod, deleteTestNADKey, "100.128.0.3/16")
		addSecondaryPodNetworkAnnotationForNADWithIP(oldPod, alternateNADKey, "100.128.0.4/16")
		oldPod.Annotations[nadapi.NetworkAttachmentAnnot] = alternateNADKey

		ownerPod := ovntest.NewPod(deleteTestNADNamespace, "owner-pod", deleteTestNodeName, "")
		ownerPod.UID = "owner-pod-uid"
		ownerPod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: deleteTestNADKey}
		setSecondaryPodNetworkForNADWithIP(ownerPod, deleteTestNADKey, "100.128.0.3/16")

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			oldPod,
			ownerPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nadA, *nadB}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nadA)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		// Bootstrap restores the selected owner's reservation before ordinary
		// reconciliation. Keep that ownership while the owner's desired state
		// has changed but its actual teardown is still queued.
		_, syncErr := bnc.SyncPods([]*corev1.Pod{oldPod, ownerPod})
		Expect(syncErr).NotTo(HaveOccurred())
		state, err := bnc.ReconcilePod(nil, oldPod, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = bnc.ReconcilePod(nil, ownerPod, nil)
		Expect(err).NotTo(HaveOccurred())

		retiringOwner := ownerPod.DeepCopy()
		if ownerCompleted {
			retiringOwner.Status.Phase = corev1.PodSucceeded
			_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(ownerPod.Namespace).UpdateStatus(
				context.Background(), retiringOwner, metav1.UpdateOptions{})
		} else {
			delete(retiringOwner.Annotations, nadapi.NetworkAttachmentAnnot)
			_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(ownerPod.Namespace).Update(
				context.Background(), retiringOwner, metav1.UpdateOptions{})
		}
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			pod, err := bnc.watchFactory.GetPod(ownerPod.Namespace, ownerPod.Name)
			if err != nil {
				return false
			}
			if ownerCompleted {
				return util.PodCompleted(pod)
			}
			return pod.Annotations[nadapi.NetworkAttachmentAnnot] == ""
		}).Should(BeTrue())

		ownerIPs := ovntest.MustParseIPNets("100.128.0.3/16")
		if deletePod {
			_, err = bnc.ReconcilePod(oldPod, nil, state)
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner", ownerIPs)).To(MatchError(ipallocator.ErrAllocatedByOther),
				"stale deletion must preserve a bootstrap-restored owner until its teardown")
			return
		}

		reattachedPod := oldPod.DeepCopy()
		reattachedPod.Annotations[nadapi.NetworkAttachmentAnnot] = deleteTestNADKey + "," + alternateNADKey
		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(oldPod.Namespace).Update(
			context.Background(), reattachedPod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			pod, err := bnc.watchFactory.GetPod(oldPod.Namespace, oldPod.Name)
			if err != nil {
				return ""
			}
			return pod.Annotations[nadapi.NetworkAttachmentAnnot]
		}).Should(Equal(deleteTestNADKey + "," + alternateNADKey))

		_, err = bnc.ReconcilePod(oldPod, reattachedPod, state)
		Expect(errors.Is(err, ipallocator.ErrAllocatedByOther)).To(BeTrue(),
			"stale reattachment must wait for the bootstrap-restored owner's teardown")
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{
			Name: bnc.GetLogicalPortName(oldPod, deleteTestNADKey),
		})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(),
			"stale reattachment must not create a duplicate LSP")
	},
		Entry("during stale-pod reattachment while owner is detaching", false, false),
		Entry("during stale-pod deletion while owner is detaching", true, false),
		Entry("during stale-pod reattachment while owner is completed", false, true),
		Entry("during stale-pod deletion while owner is completed", true, true),
	)

	It("retires a stale bootstrap claimant after successful teardown", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)

		stalePod := ovntest.NewPod(deleteTestNADNamespace, "stale-pod", deleteTestNodeName, "")
		stalePod.UID = "stale-pod-uid"
		setSecondaryPodNetworkWithIP(stalePod, "100.128.0.3/16")
		stalePod.Status.Phase = corev1.PodSucceeded
		ownerPod := ovntest.NewPod(deleteTestNADNamespace, "owner-pod", deleteTestNodeName, "")
		ownerPod.UID = "owner-pod-uid"
		setSecondaryPodNetworkWithIP(ownerPod, "100.128.0.3/16")

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		ownerPort := &nbdb.LogicalSwitchPort{
			UUID:      "owner-port-UUID",
			Name:      util.GetUserDefinedNetworkLogicalPortName(ownerPod.Namespace, ownerPod.Name, deleteTestNADKey),
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options: map[string]string{
				"iface-id-ver":      string(ownerPod.UID),
				"requested-chassis": deleteTestNodeName,
			},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              ownerPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer3Topology,
			},
		}
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{Name: switchName, Ports: []string{ownerPort.UUID}},
				ownerPort,
			}},
			stalePod,
			ownerPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		// Startup restores the applied owner. Cleaning up the stale annotation
		// must not disturb that reservation or prevent its eventual release.
		_, syncErr := bnc.SyncPods([]*corev1.Pod{stalePod, ownerPod})
		Expect(syncErr).NotTo(HaveOccurred())
		_, err := bnc.ReconcilePod(stalePod, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		ownerState, err := bnc.ReconcilePod(nil, ownerPod, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(ownerPod.Namespace).Delete(
			context.Background(), ownerPod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() error {
			_, err := bnc.watchFactory.GetPod(ownerPod.Namespace, ownerPod.Name)
			return err
		}).Should(HaveOccurred())
		_, err = bnc.ReconcilePod(ownerPod, nil, ownerState)
		Expect(err).NotTo(HaveOccurred())
		Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner",
			ovntest.MustParseIPNets("100.128.0.3/16"))).To(Succeed())
	})

	DescribeTable("preserves a completed pod's applied startup allocation", func(deleteStalePod bool) {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)

		stalePod := ovntest.NewPod(deleteTestNADNamespace, "stale-pod", deleteTestNodeName, "")
		stalePod.UID = "stale-pod-uid"
		setSecondaryPodNetworkWithIP(stalePod, "100.128.0.3/16")
		ownerPod := ovntest.NewPod(deleteTestNADNamespace, "owner-pod", deleteTestNodeName, "")
		ownerPod.UID = "owner-pod-uid"
		setSecondaryPodNetworkWithIP(ownerPod, "100.128.0.3/16")
		ownerPod.Status.Phase = corev1.PodSucceeded

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		ownerPort := &nbdb.LogicalSwitchPort{
			UUID:      "owner-port-UUID",
			Name:      util.GetUserDefinedNetworkLogicalPortName(ownerPod.Namespace, ownerPod.Name, deleteTestNADKey),
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options: map[string]string{
				"iface-id-ver":      string(ownerPod.UID),
				"requested-chassis": deleteTestNodeName,
			},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              ownerPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer3Topology,
			},
		}
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{Name: switchName, Ports: []string{ownerPort.UUID}},
				ownerPort,
			}},
			stalePod,
			ownerPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		// A running pod's stale reselected annotation must not outrank the
		// completed pod's UID-tagged LSP while that owner's teardown is pending.
		_, syncErr := bnc.SyncPods([]*corev1.Pod{stalePod, ownerPod})
		Expect(syncErr).NotTo(HaveOccurred())
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(ownerPod, deleteTestNADKey),
			ovntest.MustParseIPNets("100.128.0.3/16"))).To(BeTrue())

		if deleteStalePod {
			_, err := bnc.ReconcilePod(stalePod, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: ownerPort.Name})
			Expect(err).NotTo(HaveOccurred())
			Expect(bnc.lsManager.AllocateIPs(switchName, "test-owner",
				ovntest.MustParseIPNets("100.128.0.3/16"))).To(MatchError(ipallocator.ErrAllocatedByOther))
			return
		}

		_, err := bnc.ReconcilePod(nil, stalePod, nil)
		Expect(errors.Is(err, ipallocator.ErrAllocatedByOther)).To(BeTrue())
		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{
			Name: bnc.GetLogicalPortName(stalePod, deleteTestNADKey),
		})
		Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue())
	},
		Entry("while rejecting stale reattachment", false),
		Entry("while deleting the stale pod", true),
	)

	It("does not allocate or annotate across a same-name pod replacement", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)

		oldPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "")
		oldPod.UID = "old-pod-uid"
		oldPod.Annotations = map[string]string{nadapi.NetworkAttachmentAnnot: deleteTestNADKey}
		replacementPod := oldPod.DeepCopy()
		replacementPod.UID = "replacement-pod-uid"

		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{&nbdb.LogicalSwitch{Name: switchName}}},
			replacementPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		bnc := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName].bnc
		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName,
			ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

		// The pod can be replaced after a worker captured the old object but
		// before the annotation allocator reads the lister. That stale attempt
		// must abort instead of allocating for, and patching, the new UID.
		_, err := bnc.ReconcilePod(nil, oldPod, nil)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		_, err = bnc.ReconcilePod(nil, replacementPod, nil)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			pod, err := bnc.watchFactory.GetPod(replacementPod.Namespace, replacementPod.Name)
			if err != nil || pod.UID != replacementPod.UID {
				return false
			}
			_, err = util.UnmarshalPodAnnotation(pod.Annotations, deleteTestNADKey)
			return err == nil
		}).Should(BeTrue())

		allocatedIPs := ovntest.MustParseIPNets("100.128.0.3/16")
		Expect(bnc.lsManager.OwnsIPs(switchName, bnc.podIPOwner(replacementPod, deleteTestNADKey), allocatedIPs)).To(BeTrue())
	})

	DescribeTable("releases stale Layer3 IPAM state without deleting a replacement-owned LSP",
		func(replacementIP string, expectReleased bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer3Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)
			currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, replacementIP)
			currentPod.UID = "new-pod-uid"
			setSecondaryPodNetworkWithIP(currentPod, replacementIP+"/16")
			deletedPod := currentPod.DeepCopy()
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

			lspName := util.GetUserDefinedNetworkLogicalPortName(currentPod.Namespace, currentPod.Name, deleteTestNADKey)
			currentLSP := &nbdb.LogicalSwitchPort{
				UUID:      lspName + "-current-UUID",
				Name:      lspName,
				Addresses: []string{"0a:58:64:80:00:04 " + replacementIP},
				Options:   map[string]string{"iface-id-ver": string(currentPod.UID)},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              currentPod.Namespace,
					types.NetworkExternalID:  deleteTestNetworkName,
					types.NADExternalID:      deleteTestNADKey,
					types.TopologyExternalID: types.Layer3Topology,
				},
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + deleteTestNodeName
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{currentLSP.UUID}}

			fakeOVN := NewFakeOVN(false, deleteTestNodeName)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, currentLSP}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			Expect(controller.bnc.lsManager.AddOrUpdateSwitch(switchName,
				ovntest.MustParseIPNets("100.128.0.0/16"), nil, nil)).To(Succeed())

			staleIPs := ovntest.MustParseIPNets("100.128.0.3/16")
			owner := deletedPod
			if !expectReleased {
				owner = currentPod
			}
			Expect(controller.bnc.lsManager.AllocateIPs(switchName, controller.bnc.podIPOwner(owner, deleteTestNADKey), staleIPs)).To(Succeed())
			currentIPs := ovntest.MustParseIPNets(replacementIP + "/16")
			if replacementIP != "100.128.0.3" {
				Expect(controller.bnc.lsManager.AllocateIPs(switchName, controller.bnc.podIPOwner(currentPod, deleteTestNADKey), currentIPs)).To(Succeed())
			}
			persistedCurrentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			Expect(err).NotTo(HaveOccurred())
			currentPortInfo := controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, persistedCurrentLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:04"), currentIPs)
			stalePortInfo := &lpInfo{
				name:               lspName,
				uuid:               "deleted-port-UUID",
				logicalSwitch:      switchName,
				appliedNetworkName: deleteTestNetworkName,
				mac:                ovntest.MustParseMAC("0a:58:64:80:00:03"),
				ips:                staleIPs,
			}

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: stalePortInfo,
			})).To(Succeed())

			actualLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
			Expect(err).NotTo(HaveOccurred())
			Expect(actualLSP.UUID).To(Equal(persistedCurrentLSP.UUID))
			cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(cachedPortInfo.uuid).To(Equal(currentPortInfo.uuid))
			Expect(cachedPortInfo.expires.IsZero()).To(BeTrue())

			err = controller.bnc.lsManager.AllocateIPs(switchName, "test-owner", staleIPs)
			if expectReleased {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(ipallocator.IsErrAllocated(err)).To(BeTrue())
			}
		},
		Entry("when the replacement uses a different IP", "100.128.0.4", true),
		Entry("but preserves the allocation when the replacement uses the same IP", "100.128.0.3", false),
	)

	DescribeTable("discovers all durable pod ports when the applied cache is partial",
		func(malformedAnnotation, staleCachedUUID bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			alternateNADName := "blue-nad"
			alternateNADKey := deleteTestNADNamespace + "/" + alternateNADName
			primaryNAD := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			alternateNAD := ovntest.GenerateNAD(deleteTestNetworkName, alternateNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", primaryNAD)
			ovntest.AnnotateNADWithNetworkID("3", alternateNAD)

			deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Annotations = map[string]string{}
			if malformedAnnotation {
				deletedPod.Annotations[types.OvnPodAnnotationName] = "not-json"
			}

			newLSP := func(nadKey, ip string) *nbdb.LogicalSwitchPort {
				portName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, nadKey)
				return &nbdb.LogicalSwitchPort{
					UUID:      portName + "-UUID",
					Name:      portName,
					Addresses: []string{"0a:58:64:80:00:03 " + ip},
					Options:   map[string]string{"iface-id-ver": string(deletedPod.UID)},
					ExternalIDs: map[string]string{
						"pod":                    "true",
						"namespace":              deletedPod.Namespace,
						types.NetworkExternalID:  deleteTestNetworkName,
						types.NADExternalID:      nadKey,
						types.TopologyExternalID: types.Layer2Topology,
					},
				}
			}
			primaryLSP := newLSP(deleteTestNADKey, "100.128.0.3")
			alternateLSP := newLSP(alternateNADKey, "100.128.0.4")
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID: switchName + "-UUID", Name: switchName,
				Ports: []string{primaryLSP.UUID, alternateLSP.UUID},
			}

			fakeOVN := NewFakeOVN(false, deleteTestNodeName)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, primaryLSP, alternateLSP}},
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*primaryNAD, *alternateNAD}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(primaryNAD)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			persistedPrimaryLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: primaryLSP.Name})
			Expect(err).NotTo(HaveOccurred())
			cachedUUID := persistedPrimaryLSP.UUID
			if staleCachedUUID {
				cachedUUID = "stale-cache-UUID"
			}
			partialPortInfo := controller.bnc.logicalPortCache.add(deletedPod, switchName, deleteTestNADKey,
				deleteTestNetworkName, cachedUUID, ovntest.MustParseMAC("0a:58:64:80:00:03"),
				ovntest.MustParseIPNets("100.128.0.3/16"))

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
				deleteTestNADKey: partialPortInfo,
			})).To(Succeed())
			for _, portName := range []string{primaryLSP.Name, alternateLSP.Name} {
				_, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
				Expect(errors.Is(err, libovsdbclient.ErrNotFound)).To(BeTrue(), portName)
			}
		},
		Entry("when the OVN pod annotation is missing and the cached UUID is current", false, false),
		Entry("when the OVN pod annotation is malformed and the cached UUID is current", true, false),
		Entry("when the OVN pod annotation is missing and the cached UUID is stale", false, true),
		Entry("when the OVN pod annotation is malformed and the cached UUID is stale", true, true),
	)

	It("does not clear replacement cache or policy membership from a stale delete snapshot", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		ovntest.AnnotateNADWithNetworkID("3", nad)
		currentPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
		currentPod.UID = "new-pod-uid"
		currentPod.Labels = map[string]string{"role": "selected"}
		setSecondaryPodNetwork(currentPod)
		deletedPod := currentPod.DeepCopy()
		deletedPod.UID = "deleted-pod-uid"
		deletedPod.Annotations = map[string]string{types.OvnPodAnnotationName: "not-json"}

		lspName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspName + "-current-UUID",
			Name:      lspName,
			Addresses: []string{"0a:58:64:80:00:03 100.128.0.3"},
			Options:   map[string]string{"iface-id-ver": string(currentPod.UID)},
			ExternalIDs: map[string]string{
				"pod":                    "true",
				"namespace":              deletedPod.Namespace,
				types.NetworkExternalID:  deleteTestNetworkName,
				types.NADExternalID:      deleteTestNADKey,
				types.TopologyExternalID: types.Layer2Topology,
			},
		}
		switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
		logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}
		policyPortGroup := &nbdb.PortGroup{UUID: "policy-a-UUID", Name: "policy-a", Ports: []string{lsp.UUID}}

		fakeOVN := NewFakeOVN(false, deleteTestNodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp, policyPortGroup}},
			currentPod,
			newDeleteTestNode(),
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
		currentLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.logicalPortCache.add(currentPod, switchName, deleteTestNADKey, deleteTestNetworkName,
			currentLSP.UUID, ovntest.MustParseMAC("0a:58:64:80:00:03"), ovntest.MustParseIPNets("100.128.0.3/16"))

		policyObject := getPortNetworkPolicy("policy-a", currentPod.Namespace, "role", "selected", 80)
		np := NewNetworkPolicy(policyObject)
		np.portGroupName = policyPortGroup.Name
		np.localPodSelector, err = metav1.LabelSelectorAsSelector(&policyObject.Spec.PodSelector)
		Expect(err).NotTo(HaveOccurred())
		np.localPods.Store(lspName, currentLSP.UUID)
		np.localPodPorts.Store(localPolicyPodKey(currentPod), map[string]string{lspName: currentLSP.UUID})
		controller.bnc.networkPolicies.Store(np.getKey(), np)
		controller.bnc.addNetworkPolicyToNamespaceIndex(np)

		Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, map[string]*lpInfo{
			deleteTestNADKey: {uuid: "stale-cache-UUID", logicalSwitch: switchName, appliedNetworkName: deleteTestNetworkName},
		})).To(Succeed())

		_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
		Expect(err).NotTo(HaveOccurred())
		actualPolicyPortGroup, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: policyPortGroup.Name})
		Expect(err).NotTo(HaveOccurred())
		Expect(actualPolicyPortGroup.Ports).To(ConsistOf(currentLSP.UUID))
		cachedPortInfo, err := controller.bnc.logicalPortCache.get(currentPod, deleteTestNADKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(cachedPortInfo.uuid).To(Equal(currentLSP.UUID))
		policyPorts, found := np.localPodPorts.Load(localPolicyPodKey(currentPod))
		Expect(found).To(BeTrue())
		Expect(policyPorts).To(Equal(map[string]string{lspName: currentLSP.UUID}))
		policyPortUUID, found := np.localPods.Load(lspName)
		Expect(found).To(BeTrue())
		Expect(policyPortUUID).To(Equal(currentLSP.UUID))
	})

	DescribeTable("clears stale policy membership for an ineligible same-name replacement",
		func(hostNetwork, unscheduled bool) {
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			nad := ovntest.GenerateNAD(deleteTestNetworkName, deleteTestNADName, deleteTestNADNamespace,
				types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
			ovntest.AnnotateNADWithNetworkID("3", nad)

			deletedPod := ovntest.NewPod(deleteTestNADNamespace, deleteTestPodName, deleteTestNodeName, "100.128.0.3")
			deletedPod.UID = "deleted-pod-uid"
			deletedPod.Labels = map[string]string{"role": "selected"}
			setSecondaryPodNetwork(deletedPod)
			currentPod := deletedPod.DeepCopy()
			currentPod.UID = "new-pod-uid"
			currentPod.Spec.HostNetwork = hostNetwork
			if unscheduled {
				currentPod.Spec.NodeName = ""
			}

			portName := util.GetUserDefinedNetworkLogicalPortName(deletedPod.Namespace, deletedPod.Name, deleteTestNADKey)
			lsp := &nbdb.LogicalSwitchPort{
				UUID:        portName + "-UUID",
				Name:        portName,
				Addresses:   []string{"0a:58:64:80:00:03 100.128.0.3"},
				ExternalIDs: map[string]string{"legacy-unowned-test-port": "true"},
			}
			switchName := util.GetUserDefinedNetworkPrefix(deleteTestNetworkName) + types.OVNLayer2Switch
			logicalSwitch := &nbdb.LogicalSwitch{UUID: switchName + "-UUID", Name: switchName, Ports: []string{lsp.UUID}}

			fakeOVN := NewFakeOVN(false, deleteTestNodeName)
			fakeOVN.startWithDBSetup(
				libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch, lsp}},
				currentPod,
				newDeleteTestNode(),
				&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller := fakeOVN.userDefinedNetworkControllers[deleteTestNetworkName]
			persistedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: portName})
			Expect(err).NotTo(HaveOccurred())

			policyObject := getPortNetworkPolicy("ineligible-replacement-policy", currentPod.Namespace, "role", "selected", 80)
			np := NewNetworkPolicy(policyObject)
			np.portGroupName = "ineligible-replacement-policy"
			np.localPodSelector, err = metav1.LabelSelectorAsSelector(&policyObject.Spec.PodSelector)
			Expect(err).NotTo(HaveOccurred())
			Expect(controller.bnc.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(Succeed())
			Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOVN.nbClient, &nbdb.PortGroup{Name: np.portGroupName})).To(Succeed())
			policyOps, err := libovsdbops.AddPortsToPortGroupOps(fakeOVN.nbClient, nil, np.portGroupName, persistedLSP.UUID)
			Expect(err).NotTo(HaveOccurred())
			portMembership := map[string]string{portName: persistedLSP.UUID}
			Expect(controller.bnc.denyPGAddPorts(np, portMembership, policyOps)).To(Succeed())
			np.setLocalPortsForPod(deletedPod, portMembership)
			controller.bnc.networkPolicies.Store(np.getKey(), np)
			controller.bnc.addNetworkPolicyToNamespaceIndex(np)

			Expect(controller.bnc.removePodForUserDefinedNetwork(deletedPod, nil)).To(Succeed())

			Expect(np.getLocalPortsForPod(currentPod)).To(BeEmpty())
			_, found := np.localPods.Load(portName)
			Expect(found).To(BeFalse())
			for _, pgName := range []string{
				np.portGroupName,
				controller.bnc.defaultDenyPortGroupName(currentPod.Namespace, libovsdbutil.ACLIngress),
				controller.bnc.defaultDenyPortGroupName(currentPod.Namespace, libovsdbutil.ACLEgress),
			} {
				pg, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				Expect(err).NotTo(HaveOccurred())
				Expect(pg.Ports).To(BeEmpty(), pgName)
			}
			sharedPGs, found := controller.bnc.sharedNetpolPortGroups.Load(currentPod.Namespace)
			Expect(found).To(BeTrue())
			Expect(sharedPGs.ingressPortToPolicies).NotTo(HaveKey(portName))
			Expect(sharedPGs.egressPortToPolicies).NotTo(HaveKey(portName))
			Expect(sharedPGs.ingressPortToAssertedUUID).NotTo(HaveKey(portName))
			Expect(sharedPGs.egressPortToAssertedUUID).NotTo(HaveKey(portName))
		},
		Entry("when the replacement uses HostNetwork", true, false),
		Entry("when the replacement is unscheduled", false, true),
	)

	type dhcpTest struct {
		vmName                string
		ips                   []string
		dns                   []string
		expectedDHCPv4Options *nbdb.DHCPOptions
		expectedDHCPv6Options *nbdb.DHCPOptions
	}
	DescribeTable("with layer2 primary UDN when configuring DHCP", func(t dhcpTest) {
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		fakeOVN := NewFakeOVN(true, "worker1")
		lsp := &nbdb.LogicalSwitchPort{
			Name: "vm-port",
			UUID: "vm-port-UUID",
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  "layer2-switch-UUID",
			Name:  "layer2-switch",
			Ports: []string{lsp.UUID},
		}

		initialDB := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
			},
		}
		fakeOVN.startWithDBSetup(
			initialDB,
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
					},
				},
			},
			&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "kube-system",
					Name:      "kube-dns",
				},
				Spec: corev1.ServiceSpec{
					ClusterIPs: t.dns,
				},
			},
		)
		defer fakeOVN.shutdown()

		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "dummy",
				Labels: map[string]string{
					kubevirtv1.AppLabel:                "virt-launcher",
					kubevirtv1.VirtualMachineNameLabel: t.vmName,
				},
				Annotations: map[string]string{
					kubevirtv1.DomainAnnotation: t.vmName,
				},
			},
		}
		ips, err := util.ParseIPNets(t.ips)
		Expect(err).ToNot(HaveOccurred())
		podAnnotation := &util.PodAnnotation{
			IPs: ips,
		}
		Expect(controller.bnc.ensureDHCP(pod, podAnnotation, lsp)).To(Succeed())
		expectedDB := []libovsdbtest.TestData{}

		By("asserting the OVN entities provisioned in the NBDB are the expected ones")
		expectedLSP := lsp.DeepCopy()
		if t.expectedDHCPv4Options != nil {
			t.expectedDHCPv4Options.UUID = "vm1-dhcpv4-UUID"
			expectedLSP.Dhcpv4Options = &t.expectedDHCPv4Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv4Options)
		}
		if t.expectedDHCPv6Options != nil {
			t.expectedDHCPv6Options.UUID = "vm1-dhcpv6-UUID"
			expectedLSP.Dhcpv6Options = &t.expectedDHCPv6Options.UUID
			expectedDB = append(expectedDB, t.expectedDHCPv6Options)
		}
		// Refresh logical switch to have the propert ports uuid
		obtainedLogicalSwitches := []*nbdb.LogicalSwitch{}
		Expect(fakeOVN.nbClient.List(context.Background(), &obtainedLogicalSwitches)).To(Succeed())
		expectedDB = append(expectedDB,
			obtainedLogicalSwitches[0],
			expectedLSP,
		)
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedDB))

	},
		Entry("for ipv4 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100"},
			ips:    []string{"192.168.100.4/24"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
		}),
		Entry("for ipv6 singlestack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"2015:100:200::10"},
			ips:    []string{"2010:100:200::2/60"},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
		Entry("for dualstack", dhcpTest{
			vmName: "vm1",
			dns:    []string{"10.96.0.100", "2015:100:200::10"},
			ips:    []string{"192.168.100.4/24", "2010:100:200::2/60"},
			expectedDHCPv4Options: &nbdb.DHCPOptions{
				Cidr: "192.168.100.0/24",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/cidr":             "192.168.100.0/24",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:192.168.100.0/24",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
					"k8s.ovn.org/name":             "foo/vm1",
				},
				Options: map[string]string{
					"lease_time": "3500",
					"server_mac": "0a:58:a9:fe:01:01",
					"hostname":   "\"vm1\"",
					"mtu":        "1300",
					"dns_server": "10.96.0.100",
					"server_id":  "169.254.1.1",
				},
			},
			expectedDHCPv6Options: &nbdb.DHCPOptions{
				Cidr: "2010:100:200::/60",
				ExternalIDs: map[string]string{
					"k8s.ovn.org/name":             "foo/vm1",
					"k8s.ovn.org/cidr":             "2010.100.200../60",
					"k8s.ovn.org/id":               "bluenet-network-controller:VirtualMachine:foo/vm1:2010.100.200../60",
					"k8s.ovn.org/zone":             "local",
					"k8s.ovn.org/owner-controller": "bluenet-network-controller",
					"k8s.ovn.org/owner-type":       "VirtualMachine",
				},
				Options: map[string]string{
					"server_id":  "0a:58:6d:6d:c1:50",
					"fqdn":       "\"vm1\"",
					"dns_server": "2015:100:200::10",
				},
			},
		}),
	)
	Context("enableSourceLSPFailedLiveMigration", func() {
		const (
			vmName        = "test-vm"
			nadKey        = "awips/mgmt"
			localNodeName = "node-local"
		)

		newVirtLauncherPod := func(name, nodeName string, phase corev1.PodPhase, annotations map[string]string) *corev1.Pod {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[kubevirtv1.DomainAnnotation] = vmName
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "awips",
					Labels: map[string]string{
						kubevirtv1.AppLabel:                "virt-launcher",
						kubevirtv1.VirtualMachineNameLabel: vmName,
					},
					CreationTimestamp: metav1.Time{Time: time.Now()},
					Annotations:       annotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "kubevirt.io/v1",
						Kind:       "VirtualMachineInstance",
						Name:       vmName,
					}},
				},
				Spec: corev1.PodSpec{
					NodeName: nodeName,
				},
				Status: corev1.PodStatus{
					Phase: phase,
				},
			}
			return pod
		}

		setupControllerWithDBSetup := func(dbSetup *libovsdbtest.TestSetup, pods ...*corev1.Pod) (*BaseUserDefinedNetworkController, *FakeOVN) {
			localnetNAD := ovntest.GenerateNAD("mgmt", "mgmt", "awips",
				types.LocalnetTopology, "", types.NetworkRoleSecondary)

			fakeOVN := NewFakeOVN(false, localNodeName)
			objs := []runtime.Object{}
			for _, p := range pods {
				objs = append(objs, p)
			}
			if dbSetup != nil {
				fakeOVN.startWithDBSetup(*dbSetup, objs...)
			} else {
				fakeOVN.start(objs...)
			}
			DeferCleanup(fakeOVN.shutdown)

			Expect(fakeOVN.NewUserDefinedNetworkController(localnetNAD)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["mgmt"]
			Expect(ok).To(BeTrue())

			controller.bnc.nodeName = localNodeName

			return controller.bnc, fakeOVN
		}

		setupController := func(pods ...*corev1.Pod) *BaseUserDefinedNetworkController {
			bnc, _ := setupControllerWithDBSetup(nil, pods...)
			return bnc
		}

		It("should skip source LSP re-enable when source pod is on a remote node", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			// Target pod is local, failed (completed) — triggers LiveMigrationFailed detection
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			// Make target created after source so DiscoverLiveMigrationStatus picks it as target
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			// Call with empty IPs (IPAM-less localnet) — this would fail without the locality guard
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod is on a remote node")
		})

		It("should skip source LSP re-enable when source pod LSP is not local", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePod := newVirtLauncherPod("virt-launcher-"+vmName+"-source", "node-remote", corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodSucceeded, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			bnc := setupController(sourcePod, targetPod)

			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, "", nil)
			Expect(err).NotTo(HaveOccurred(), "should not error when source pod LSP is not local")
		})

		It("should re-enable source LSP when source pod is local and migration failed", func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true

			sourcePodName := "virt-launcher-" + vmName + "-source"
			sourcePod := newVirtLauncherPod(sourcePodName, localNodeName, corev1.PodRunning, nil)
			targetPod := newVirtLauncherPod("virt-launcher-"+vmName+"-target", localNodeName, corev1.PodFailed, nil)
			targetPod.CreationTimestamp = metav1.Time{Time: sourcePod.CreationTimestamp.Add(time.Second)}

			// Build LSP and switch names matching what the controller will compute:
			//   LSP name: GetUserDefinedNetworkLogicalPortName(namespace, podName, nadKey)
			//   Switch name: GetNetworkScopedSwitchName(OVNLocalnetSwitch)
			sourceLSPName := util.GetUserDefinedNetworkLogicalPortName(sourcePod.Namespace, sourcePodName, nadKey)
			sourceLSP := &nbdb.LogicalSwitchPort{
				UUID:    sourceLSPName + "-UUID",
				Name:    sourceLSPName,
				Enabled: ptr.To(false),
			}
			switchName := util.GetUserDefinedNetworkPrefix("mgmt") + types.OVNLocalnetSwitch
			logicalSwitch := &nbdb.LogicalSwitch{
				UUID:  switchName + "-UUID",
				Name:  switchName,
				Ports: []string{sourceLSP.UUID},
			}

			dbSetup := &libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					sourceLSP,
				},
			}

			bnc, fakeOVN := setupControllerWithDBSetup(dbSetup, sourcePod, targetPod)

			mac := "0a:58:0a:80:00:05"
			ips := []string{"10.128.0.5/24"}
			err := bnc.enableSourceLSPFailedLiveMigration(targetPod, nadKey, mac, ips)
			Expect(err).NotTo(HaveOccurred(), "should re-enable source LSP without error")

			// Verify the LSP was updated: Enabled=true and addresses set
			expectedLSP := &nbdb.LogicalSwitchPort{
				UUID:      sourceLSP.UUID,
				Name:      sourceLSPName,
				Enabled:   ptr.To(true),
				Addresses: []string{mac + " 10.128.0.5"},
			}
			expectedSwitch := logicalSwitch.DeepCopy()
			Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(expectedSwitch, expectedLSP))
		})
	})

	It("should not fail to sync pods if namespace is gone", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false, "worker1")
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager instead of a fake one, so getActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForNode("worker1", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// simulate that we listed the pod, but namespace was deleted after
		podWithNoNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "doesnotexist",
				Name:      "dummy",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podWithNoNamespace)

		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should not fail to sync pods if namespace has primary UDN label but NAD not ready", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		fakeOVN := NewFakeOVN(false, "worker1")
		// Create namespace with primary UDN label but no NAD
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-namespace",
				Labels: map[string]string{
					types.RequiredUDNNamespaceLabel: "",
				},
			},
		}
		fakeOVN.start(
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker1",
					Annotations: map[string]string{
						"k8s.ovn.org/network-ids": `{"other": "3"}`,
					},
				},
			},
			namespace,
		)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		// inject a real networkManager so GetActiveNetworkForNamespace will get called
		nadController, err := networkmanager.NewForNode("worker1", nil, fakeOVN.watcher)
		Expect(err).NotTo(HaveOccurred())
		controller.bnc.networkManager = nadController.Interface()

		// Pod in namespace with primary UDN label but no NAD causes InvalidPrimaryNetworkError
		podInLabeledNamespace := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-pod",
			},
		}

		var initialPodList []interface{}
		initialPodList = append(initialPodList, podInLabeledNamespace)

		// Should skip pod without error when GetActiveNetworkForNamespace returns InvalidPrimaryNetworkError
		err = controller.bnc.syncPodsForUserDefinedNetwork(initialPodList)
		Expect(err).NotTo(HaveOccurred())
	})

	It("picks up an already-running remote pod when its namespace newly joins a running primary UDN", func() {
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		const (
			localNode    = "node-local"
			newNamespace = "greenamespace"
		)
		// bluenet is already running, serving a different namespace
		initialNad := ovntest.GenerateNAD("bluenet", "initialnad", "bluenamespace",
			types.Layer3Topology, "100.128.0.0/16", types.NetworkRolePrimary)

		remotePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: newNamespace, Name: "remote-running"},
			Spec:       corev1.PodSpec{NodeName: "node-remote"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		namespaceObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: newNamespace}}

		fakeOVN := NewFakeOVN(false, localNode)
		fakeOVN.start(newNode(localNode, "192.168.126.202/24"), namespaceObj, remotePod)
		DeferCleanup(fakeOVN.shutdown)

		Expect(fakeOVN.NewUserDefinedNetworkController(initialNad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		bnc := controller.bnc
		Expect(bnc.WatchPods()).To(Succeed())

		// bluenet now also picks up greenamespace, where remotePod is already
		// Running. The network reconcile must enqueue it through the shared pod
		// reconciler even though it is no longer Pending.
		afterInfo := util.NewMutableNetInfo(bnc.GetNetInfo())
		afterInfo.AddNADs(util.GetNADName(newNamespace, "rednad"))
		Expect(bnc.reconcile(afterInfo, func(string) {})).To(Succeed())
	})

	Context("when shouldFilterNamespace does not filter a pod because the namespace is missing from the informer", func() {
		const (
			missingNamespace = "greenamespace"
			podName          = "dummy"
			localNode        = "worker1"
		)

		var (
			bnc     *BaseUserDefinedNetworkController
			fakeOVN *FakeOVN
			pod     *corev1.Pod
		)

		BeforeEach(func() {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.IPv4Mode = true
			pod = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "dummy-pod-uid",
					Namespace: missingNamespace,
					Name:      podName,
				},
				Spec: corev1.PodSpec{
					NodeName: localNode,
				},
			}
			fakeOVN = NewFakeOVN(false, localNode)
			fakeOVN.start(
				&corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: localNode,
						Annotations: map[string]string{
							"k8s.ovn.org/network-ids": `{"bluenet": "3"}`,
							util.OvnNodeChassisID:     chassisIDForNode(localNode),
						},
					},
				},
				pod,
			)
			DeferCleanup(fakeOVN.shutdown)
			Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
			controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
			Expect(ok).To(BeTrue())
			bnc = controller.bnc
			Expect(bnc.shouldFilterNamespace(missingNamespace)).To(BeFalse())
		})

		DescribeTable("initializes and cleans up a namespace when its UDN pod arrives first", func(namespaceReadyDuringRetry bool) {
			config.OVNKubernetesFeature.EnableEgressFirewall = true
			err := bnc.ensurePodForUserDefinedNetwork(pod, true, "ErrorAddingResource")
			Expect(err).To(MatchError(ContainSubstring("failed to get primary network namespace NAD")))
			Expect(err).To(MatchError(apierrors.IsNotFound, "IsNotFound"))

			namespace, err := fakeOVN.fakeClient.KubeClient.CoreV1().Namespaces().Create(
				context.Background(),
				newUDNNamespace(missingNamespace),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			nadToCreate := nad.DeepCopy()
			ovntest.AnnotateNADWithNetworkID("3", nadToCreate)
			_, err = fakeOVN.fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nadToCreate.Namespace).Create(
				context.Background(), nadToCreate, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nadKey := util.GetNADName(nad.Namespace, nad.Name)
			nodeSubnet := ovntest.MustParseIPNet("100.128.0.0/24")
			switchName := bnc.GetNetworkScopedSwitchName(localNode)
			Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, []*net.IPNet{nodeSubnet}, nil, nil)).To(Succeed())
			Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, &nbdb.LogicalSwitch{Name: switchName})).To(Succeed())

			Eventually(func() (string, error) {
				return bnc.networkManager.GetPrimaryNADForNamespace(missingNamespace)
			}).WithTimeout(5 * time.Second).Should(Equal(nadKey))
			pgName := bnc.getNamespacePortGroupName(missingNamespace)
			Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace), "pod setup must not initialize namespace state")
			lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
			initializeNamespace := func() {
				Expect(bnc.AddNamespaceForUserDefinedNetwork(namespace)).To(Succeed())
				Eventually(func() error {
					_, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
					return err
				}).WithTimeout(5 * time.Second).Should(Succeed())
			}
			if namespaceReadyDuringRetry {
				// Run the namespace handler only after the pod's first lookup has
				// observed the missing group. The same pod setup call must recover.
				bnc.nbClient = &namespacePortGroupLookupClient{
					Client: fakeOVN.nbClient, afterFirstLookup: initializeNamespace,
				}
			} else {
				Expect(bnc.ensurePodForUserDefinedNetwork(pod, true, "ErrorAddingResource")).To(MatchError(ContainSubstring("failed to add pod port")))
				_, err = libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				Expect(err).To(MatchError(libovsdbclient.ErrNotFound))
				Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace))
				_, err = libovsdbops.GetLogicalSwitchPort(fakeOVN.nbClient, &nbdb.LogicalSwitchPort{Name: lspName})
				Expect(err).To(MatchError(libovsdbclient.ErrNotFound), "failed membership must not commit the pod port")
				Eventually(func() bool {
					currentPod, err := fakeOVN.watcher.GetPod(pod.Namespace, pod.Name)
					if err != nil {
						return false
					}
					annotation, err := util.UnmarshalPodAnnotation(currentPod.Annotations, nadKey)
					return err == nil && util.IsValidPodAnnotation(annotation)
				}).WithTimeout(5 * time.Second).Should(BeTrue())
				initializeNamespace()
			}

			Expect(bnc.ensurePodForUserDefinedNetwork(pod, true, "ErrorAddingResource")).To(Succeed())
			bnc.nbClient = fakeOVN.nbClient
			nsInfo, unlock := bnc.getNamespaceLocked(missingNamespace, true)
			Expect(nsInfo).NotTo(BeNil())
			initializedPGName := nsInfo.portGroupName
			unlock()
			Expect(initializedPGName).To(Equal(pgName))

			updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Get(
				context.Background(), pod.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			podAnnotation, err := util.UnmarshalPodAnnotation(updatedPod.Annotations, nadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(podAnnotation.IPs).To(HaveLen(1))
			Expect(nodeSubnet.Contains(podAnnotation.IPs[0].IP)).To(BeTrue())
			Expect(podAnnotation.MAC).NotTo(BeEmpty())
			Expect(podAnnotation.Role).To(Equal(types.NetworkRolePrimary))

			lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(HaveLen(1))
			Expect(lsps[0].Addresses).To(ConsistOf(fmt.Sprintf("%s %s", podAnnotation.MAC, podAnnotation.IPs[0].IP)))
			Eventually(func() (*nbdb.PortGroup, error) {
				return libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
			}).WithTimeout(5 * time.Second).Should(HaveField("Ports", ConsistOf(lsps[0].UUID)))

			By("letting namespace teardown remove the group before pod cleanup")
			Expect(bnc.deleteNamespaceForUserDefinedNetwork(namespace)).To(Succeed())
			Expect(bnc.namespaces).NotTo(HaveKey(missingNamespace))
			Eventually(func() error {
				_, err := libovsdbops.GetPortGroup(fakeOVN.nbClient, &nbdb.PortGroup{Name: pgName})
				return err
			}).WithTimeout(5 * time.Second).Should(MatchError(libovsdbclient.ErrNotFound))
			Expect(bnc.removePodForUserDefinedNetwork(updatedPod, nil)).To(Succeed())
			Eventually(fakeOVN.nbClient).WithTimeout(5 * time.Second).Should(libovsdbtest.HaveDataIgnoringUUIDs(
				&nbdb.LogicalSwitch{Name: switchName},
			))
		},
			Entry("namespace becomes ready within the local retry", true),
			Entry("missing namespace handler falls back to a later pod retry", false),
		)

		It("removePod deletes this network's logical port while the namespace is missing", func() {
			nadKey := util.GetNADName(nad.Namespace, nad.Name)
			bnc.networkManager = &nadKeyNameOverlay{
				Interface:    bnc.networkManager,
				nadToNetwork: map[string]string{nadKey: bnc.GetNetworkName()},
			}

			annotations, err := util.MarshalPodAnnotation(nil, &util.PodAnnotation{
				MAC:  net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0x00, 0x05},
				IPs:  ovntest.MustParseIPNets("100.128.0.5/24"),
				Role: types.NetworkRolePrimary,
			}, nadKey)
			Expect(err).NotTo(HaveOccurred())
			pod.Annotations = annotations

			switchName := bnc.GetNetworkScopedSwitchName(localNode)
			lspName := util.GetUserDefinedNetworkLogicalPortName(pod.Namespace, pod.Name, nadKey)
			ls := &nbdb.LogicalSwitch{Name: switchName}
			lsp := &nbdb.LogicalSwitchPort{Name: lspName}
			Expect(libovsdbops.CreateOrUpdateLogicalSwitch(fakeOVN.nbClient, ls)).To(Succeed())
			Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(fakeOVN.nbClient, ls, lsp)).To(Succeed())

			lsps, err := libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(HaveLen(1))
			Expect(lsps[0]).To(Equal(lsp))

			Expect(bnc.removePodForUserDefinedNetwork(pod, map[string]*lpInfo{
				nadKey: {
					name:          lspName,
					uuid:          lsp.UUID,
					logicalSwitch: switchName,
				},
			})).To(Succeed())
			lsps, err = libovsdbops.FindLogicalSwitchPortWithPredicate(fakeOVN.nbClient, func(p *nbdb.LogicalSwitchPort) bool {
				return p.Name == lspName
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lsps).To(BeEmpty())
		})
	})

	It("removes a cached pod port after its NAD mapping is deleted", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRoleSecondary)
		netInfo, err := util.ParseNADInfo(layer2NAD)
		Expect(err).NotTo(HaveOccurred())

		const (
			nodeName      = "worker1"
			podName       = "pod1"
			nadKey        = "greenamespace/rednad"
			foreignNADKey = "greenamespace/orangenad"
			foreignSwitch = "orangenet_ovn-layer2-switch"
		)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "greenamespace",
				Name:      podName,
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
		podIPs, err := util.ParseIPNets([]string{"100.128.0.2/16"})
		Expect(err).NotTo(HaveOccurred())
		podMAC, err := net.ParseMAC("0a:58:64:80:00:02")
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, nadKey)
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, foreignNADKey)
		Expect(err).NotTo(HaveOccurred())

		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		lspName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, nadKey)
		foreignLSPName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, foreignNADKey)
		lsp := &nbdb.LogicalSwitchPort{UUID: lspName + "-UUID", Name: lspName}
		foreignLSP := &nbdb.LogicalSwitchPort{
			UUID: foreignLSPName + "-UUID",
			Name: foreignLSPName,
		}
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID:  switchName + "-UUID",
			Name:  switchName,
			Ports: []string{lsp.UUID},
		}
		foreignLogicalSwitch := &nbdb.LogicalSwitch{
			UUID:  foreignSwitch + "-UUID",
			Name:  foreignSwitch,
			Ports: []string{foreignLSP.UUID},
		}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				logicalSwitch,
				lsp,
				foreignLogicalSwitch,
				foreignLSP,
			},
		}, pod)
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		controller.bnc.networkManager = (&networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{},
			NADNetworks:     map[string]util.NetInfo{},
		}).Interface()

		fakeOVN.portCache.add(
			pod, switchName, nadKey, netInfo.GetNetworkName(), lsp.UUID, podMAC, podIPs)
		fakeOVN.portCache.add(
			pod, foreignSwitch, foreignNADKey, "orangenet", foreignLSP.UUID, podMAC, podIPs)
		portInfoMap := controller.bnc.getPortInfoForUserDefinedNetwork(pod)
		Expect(portInfoMap).To(HaveKey(nadKey))
		Expect(portInfoMap).NotTo(HaveKey(foreignNADKey))
		Expect(controller.bnc.removePodForUserDefinedNetwork(
			pod, portInfoMap)).To(Succeed())

		expectedSwitch := logicalSwitch.DeepCopy()
		expectedSwitch.Ports = nil
		Expect(fakeOVN.nbClient).To(libovsdbtest.HaveData(
			expectedSwitch,
			foreignLogicalSwitch,
			foreignLSP,
		))
	})

	It("processes a primary UDN pod delete while its namespace exists after the NAD mapping is deleted", func() {
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		layer2NAD := ovntest.GenerateNAD("bluenet", "rednad", "greenamespace",
			types.Layer2Topology, "100.128.0.0/16", types.NetworkRolePrimary)
		netInfo, err := util.ParseNADInfo(layer2NAD)
		Expect(err).NotTo(HaveOccurred())

		const (
			nodeName = "worker1"
			podName  = "pod1"
			nadKey   = "greenamespace/rednad"
		)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "greenamespace",
				Name:      podName,
			},
			Spec: corev1.PodSpec{NodeName: nodeName},
		}
		podIPs, err := util.ParseIPNets([]string{"100.128.0.2/16"})
		Expect(err).NotTo(HaveOccurred())
		podMAC, err := net.ParseMAC("0a:58:64:80:00:02")
		Expect(err).NotTo(HaveOccurred())
		pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations,
			&util.PodAnnotation{IPs: podIPs, MAC: podMAC}, nadKey)
		Expect(err).NotTo(HaveOccurred())

		switchName := netInfo.GetNetworkScopedSwitchName(types.OVNLayer2Switch)
		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID",
			Name: switchName,
		}
		lspName := util.GetUserDefinedNetworkLogicalPortName(
			pod.Namespace, pod.Name, nadKey)
		lsp := &nbdb.LogicalSwitchPort{UUID: lspName + "-UUID", Name: lspName}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{logicalSwitch},
		})
		DeferCleanup(fakeOVN.shutdown)
		Expect(fakeOVN.NewUserDefinedNetworkController(layer2NAD)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers["bluenet"]
		Expect(ok).To(BeTrue())
		fakeNetworkManager := &networkmanager.FakeNetworkManager{
			PrimaryNetworks: map[string]util.NetInfo{pod.Namespace: netInfo},
			NADNetworks:     map[string]util.NetInfo{},
		}
		controller.bnc.networkManager = fakeNetworkManager.Interface()
		Expect(controller.bnc.WatchPods()).To(Succeed())

		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).
			Create(context.Background(), pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := fakeOVN.watcher.GetPod(pod.Namespace, pod.Name)
			return err
		}).Should(Succeed())

		Expect(libovsdbops.CreateOrUpdateLogicalSwitchPortsOnSwitch(
			fakeOVN.nbClient, &nbdb.LogicalSwitch{Name: switchName}, lsp)).To(Succeed())
		fakeOVN.portCache.add(pod, switchName, nadKey, netInfo.GetNetworkName(), lsp.UUID, podMAC, podIPs)
		_, err = fakeNetworkManager.GetPrimaryNADForNamespace(pod.Namespace)
		Expect(err).To(MatchError(util.IsInvalidPrimaryNetworkError, "IsInvalidPrimaryNetworkError"))
		Expect(controller.bnc.FilterOutResource(factory.PodType, pod)).To(BeTrue())

		Expect(fakeOVN.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).
			Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(Succeed())
		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(logicalSwitch))
	})

})

func TestAdvertisedSharedGatewaySNATUsesLiveAllowedExtIPSets(t *gotesting.T) {
	for _, outboundSNAT := range []string{types.NoOverlaySNATDisabled, types.NoOverlaySNATEnabled} {
		t.Run(outboundSNAT, func(t *gotesting.T) {
			bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, outboundSNAT, config.GatewayModeShared)
			seedAdvertisedSNATAddressSets(t, asf)

			expectAdvertisedSNATUsesLiveAllowedExtIPs(t, bsnc, asf, localPodSubnets)
		})
	}
}

func TestAdvertisedSharedGatewaySNATFailsWithoutAllowedExtIPsForFamily(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeShared)
	config.IPv6Mode = false
	seedAdvertisedSNATAddressSets(t, asf)

	g := NewWithT(t)
	_, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).To(MatchError(ContainSubstring(
		"failed to build allowed_ext_ips SNAT for advertised network bluenet, subnet ae70::/64: no address set UUID for IPv6",
	)))
}

func TestAdvertisedLocalGatewaySNATUsesDestinationMatch(t *gotesting.T) {
	bsnc, asf, localPodSubnets := newAdvertisedSNATTestController(t, types.NoOverlaySNATDisabled, config.GatewayModeLocal)
	seedAdvertisedSNATAddressSets(t, asf)

	expectAdvertisedSNATUsesDestinationMatch(t, bsnc, asf, localPodSubnets)
}

func newAdvertisedSNATTestController(
	t *gotesting.T,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	return newAdvertisedSNATTestControllerForTopology(
		t,
		types.Layer3Topology,
		"100.128.0.0/16/24,ae70::/60/64",
		outboundSNAT,
		gatewayMode,
		ovntest.MustParseIPNets("100.128.0.0/24", "ae70::/64"),
	)
}

func newAdvertisedSNATTestControllerForTopology(
	t *gotesting.T,
	topology string,
	cidrs string,
	outboundSNAT string,
	gatewayMode config.GatewayMode,
	localPodSubnets []*net.IPNet,
) (*BaseUserDefinedNetworkController, *addressset.FakeAddressSetFactory, []*net.IPNet) {
	t.Helper()
	RegisterTestingT(t)
	if err := config.PrepareTestConfig(); err != nil {
		t.Fatalf("failed to prepare test config: %v", err)
	}
	t.Cleanup(func() {
		_ = config.PrepareTestConfig()
	})
	config.IPv4Mode = true
	config.IPv6Mode = true
	config.Gateway.Mode = gatewayMode
	config.Gateway.V4MasqueradeSubnet = "169.254.0.0/16"
	config.Gateway.V6MasqueradeSubnet = "fd69::/112"

	const (
		networkName = "bluenet"
		nadName     = "rednad"
		namespace   = "greenamespace"
	)
	nad := ovntest.GenerateNADWithConfig(nadName, namespace, fmt.Sprintf(`
{
        "cniVersion": "1.1.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": %q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q,
        "transport": %q,
        "outboundSNAT": %q
}
`,
		networkName,
		topology,
		cidrs,
		fmt.Sprintf("%s/%s", namespace, nadName),
		types.NetworkRolePrimary,
		types.NetworkTransportNoOverlay,
		outboundSNAT,
	))
	ovntest.AnnotateNADWithNetworkID("3", nad)
	netInfo, err := util.ParseNADInfo(nad)
	if err != nil {
		t.Fatalf("failed to parse NAD: %v", err)
	}

	controllerName := getNetworkControllerName(netInfo.GetNetworkName())
	asf := addressset.NewFakeAddressSetFactory(controllerName)
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker1",
			Annotations: map[string]string{
				util.OVNNodeHostCIDRs: `["192.168.126.11/24","fd00::11/64"]`,
			},
		},
	}
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{node}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}
	if err := watchFactory.Start(); err != nil {
		t.Fatalf("failed to start watch factory: %v", err)
	}
	t.Cleanup(watchFactory.Shutdown)

	nbClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(libovsdbtest.TestSetup{})
	if err != nil {
		t.Fatalf("failed to create libovsdb test harness: %v", err)
	}
	t.Cleanup(libovsdbCleanup.Cleanup)
	addressSetManager := addresssetmanager.NewAddressSetManager(
		watchFactory.PodCoreInformer(),
		watchFactory.NamespaceInformer(),
		watchFactory.NodeCoreInformer(),
		nbClient,
		networkmanager.Default().Interface().GetNetworkNameForNADKey,
	)
	return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				controllerName:      controllerName,
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
				addressSetFactory:   asf,
				addressSetManager:   addressSetManager,
			},
		},
		asf,
		localPodSubnets
}

func seedAdvertisedSNATAddressSets(t *gotesting.T, asf addressset.AddressSetFactory) {
	t.Helper()
	nodeIPsASIDs := getClusterNodeIPsAddrSetDbIDsForTest()
	if _, err := asf.NewAddressSet(nodeIPsASIDs, []string{"192.168.126.11", "fd00::11"}); err != nil {
		t.Fatalf("failed to create node IP address set: %v", err)
	}

	svcIPsASIDs := udnenabledsvc.GetAddressSetDBIDs()
	if _, err := asf.NewAddressSet(svcIPsASIDs, []string{"10.96.0.10", "fd02::10"}); err != nil {
		t.Fatalf("failed to create UDN-enabled service address set: %v", err)
	}
}

func expectAdvertisedSNATUsesLiveAllowedExtIPs(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(4))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	nodeIPv4ASUUID, nodeIPv6ASUUID := nodeIPsAS.GetASUUID()
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPv4ASUUID, svcIPv6ASUUID := svcIPsAS.GetASUUID()

	actualAllowedExtIPsByLogicalIP := map[string][]string{}
	for _, snat := range snats {
		g.Expect(snat.Match).To(Equal(""))
		g.Expect(snat.AllowedExtIPs).NotTo(BeNil())
		g.Expect(snat.ExemptedExtIPs).To(BeNil())
		actualAllowedExtIPsByLogicalIP[snat.LogicalIP] = append(
			actualAllowedExtIPsByLogicalIP[snat.LogicalIP],
			*snat.AllowedExtIPs,
		)
	}
	g.Expect(actualAllowedExtIPsByLogicalIP["100.128.0.0/24"]).To(ConsistOf(nodeIPv4ASUUID, svcIPv4ASUUID))
	g.Expect(actualAllowedExtIPsByLogicalIP["ae70::/64"]).To(ConsistOf(nodeIPv6ASUUID, svcIPv6ASUUID))
}

func expectAdvertisedSNATUsesDestinationMatch(
	t *gotesting.T,
	bsnc *BaseUserDefinedNetworkController,
	asf addressset.AddressSetFactory,
	localPodSubnets []*net.IPNet,
) {
	t.Helper()
	g := NewWithT(t)

	snats, err := bsnc.buildUDNEgressSNAT(localPodSubnets, "rtos-bluenet-worker1", true)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(snats).To(HaveLen(2))

	nodeIPsAS, err := asf.GetAddressSet(getClusterNodeIPsAddrSetDbIDsForTest())
	g.Expect(err).NotTo(HaveOccurred())
	svcIPsAS, err := asf.GetAddressSet(udnenabledsvc.GetAddressSetDBIDs())
	g.Expect(err).NotTo(HaveOccurred())

	dstMac := util.IPAddrToHWAddr(bsnc.GetNodeManagementIP(localPodSubnets[0]).IP)
	dstMacMatch := getMasqueradeManagementIPSNATMatch(dstMac.String())
	v4Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv4, nodeIPsAS, svcIPsAS)
	v6Match := getClusterNodesDestinationBasedSNATMatch(utilnet.IPv6, nodeIPsAS, svcIPsAS)

	g.Expect(snats[0].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v4Match)))
	g.Expect(snats[0].AllowedExtIPs).To(BeNil())
	g.Expect(snats[0].ExemptedExtIPs).To(BeNil())

	g.Expect(snats[1].Match).To(Equal(fmt.Sprintf("%s && %s", dstMacMatch, v6Match)))
	g.Expect(snats[1].AllowedExtIPs).To(BeNil())
	g.Expect(snats[1].ExemptedExtIPs).To(BeNil())
}

var _ = Describe("dhcpPodNetworkOutOfSync", func() {
	newController := func(ipamType string) *BaseUserDefinedNetworkController {
		netconf := &ovncnitypes.NetConf{
			NetConf: cnitypes.NetConf{
				Name: "localnet-net",
				Type: "ovn-k8s-cni-overlay",
				IPAM: cnitypes.IPAM{Type: ipamType},
			},
			NADName:  "foo-ns/localnet-nad",
			Topology: types.LocalnetTopology,
		}
		netInfo, err := util.NewNetInfo(netconf)
		Expect(err).NotTo(HaveOccurred())
		return &BaseUserDefinedNetworkController{
			BaseNetworkController: BaseNetworkController{
				ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
			},
		}
	}

	type podNetworks map[string]*util.PodAnnotation

	podWithNetworks := func(networks podNetworks) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bar-pod",
				Namespace: "foo-ns",
			},
		}
		for nadKey, entry := range networks {
			var err error
			pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, entry, nadKey)
			Expect(err).NotTo(HaveOccurred())
		}
		return pod
	}

	const nadKey = "foo-ns/localnet-nad"
	var (
		// this network's entry as the CNI first writes it, before the DHCP
		// exchange, and after patching the learned lease into it
		dhcpMACOnly = &util.PodAnnotation{
			MAC:      ovntest.MustParseMAC("0a:58:fd:98:00:01"),
			Role:     types.NetworkRoleSecondary,
			IPAMMode: types.IPAMTypeDHCP,
		}
		dhcpLeased = &util.PodAnnotation{
			IPs:      ovntest.MustParseIPNets("10.1.192.102/24"),
			MAC:      ovntest.MustParseMAC("0a:58:fd:98:00:01"),
			Gateways: ovntest.MustParseIPs("10.1.192.1"),
			Role:     types.NetworkRoleSecondary,
			IPAMMode: types.IPAMTypeDHCP,
		}
	)

	dhcpPodNetworkOutOfSync := func(ipamType string, desiredNetworks podNetworks, applied *util.PodAnnotation) bool {
		bsnc := newController(ipamType)
		portInfo := &lpInfo{}
		if applied != nil {
			portInfo.mac = applied.MAC
			portInfo.ips = applied.IPs
		}
		return bsnc.dhcpPodNetworkOutOfSync(podWithNetworks(desiredNetworks), nadKey, portInfo)
	}

	DescribeTable("re-triggers port processing when desired DHCP state differs from applied state",
		func(ipamType string, desiredNetworks podNetworks, applied *util.PodAnnotation) {
			Expect(dhcpPodNetworkOutOfSync(ipamType, desiredNetworks, applied)).To(BeTrue())
		},
		Entry("when the annotation gains the DHCP-learned IPs",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, dhcpMACOnly),
		// the CNI clears the stale lease at the start of a repeat ADD so the
		// port security is relaxed to MAC-only for the new DHCP exchange
		Entry("when the CNI clears the previous lease on a repeat ADD",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpMACOnly}, dhcpLeased),
		Entry("when the applied cache has no addresses",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, nil),
	)

	DescribeTable("does not re-trigger port processing",
		func(ipamType string, desiredNetworks podNetworks, applied *util.PodAnnotation) {
			Expect(dhcpPodNetworkOutOfSync(ipamType, desiredNetworks, applied)).To(BeFalse())
		},
		Entry("when desired and applied state match",
			types.IPAMTypeDHCP, podNetworks{nadKey: dhcpLeased}, dhcpLeased),
		Entry("on non-DHCP networks",
			"", podNetworks{nadKey: dhcpLeased}, dhcpMACOnly),
		// nothing legitimately removes a DHCP entry from a live pod, and
		// reprocessing the port without its annotation would only churn
		Entry("when this network's entry is removed",
			types.IPAMTypeDHCP, podNetworks{}, dhcpLeased),
	)
})

var _ = Describe("localnet DHCP IPAM pod lifecycle", func() {
	const (
		namespaceName = "foo-ns"
		nadName       = "localnet-nad"
		networkName   = "localnet-net"
		nodeName      = "node1"
		chassisID     = "chassis-node1"
		podName       = "bar-pod"
		nadKey        = namespaceName + "/" + nadName
		podMAC        = "0a:58:fd:98:00:01"
		podIP         = "10.1.192.102"

		macOnlyAnnotation = `{"foo-ns/localnet-nad":{"mac_address":"0a:58:fd:98:00:01","role":"secondary","ipam_mode":"dhcp"}}`
		leasedAnnotation  = `{"foo-ns/localnet-nad":{"ip_addresses":["10.1.192.102/24"],"mac_address":"0a:58:fd:98:00:01","gateway_ips":["10.1.192.1"],"role":"secondary","ipam_mode":"dhcp"}}`
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
	})

	It("creates the LSP with the CNI-minted MAC and tightens it after the DHCP lease patch", func() {
		nad := ovntest.GenerateNADWithConfig(nadName, namespaceName, fmt.Sprintf(`
{
        "cniVersion": "1.0.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology": "localnet",
        "netAttachDefName": %q,
        "ipam": {"type": "dhcp"}
}
`, networkName, nadKey))
		ovntest.AnnotateNADWithNetworkID("3", nad)
		netInfo, err := util.ParseNADInfo(nad)
		Expect(err).NotTo(HaveOccurred())
		switchName := netInfo.GetNetworkScopedName(types.OVNLocalnetSwitch)

		logicalSwitch := &nbdb.LogicalSwitch{
			UUID: switchName + "-UUID",
			Name: switchName,
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespaceName,
				Name:      podName,
				UID:       podName,
				Annotations: map[string]string{
					nadapi.NetworkAttachmentAnnot: nadKey,
					types.OvnPodAnnotationName:    macOnlyAnnotation,
				},
			},
			Spec:   corev1.PodSpec{NodeName: nodeName},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		fakeOVN := NewFakeOVN(false, nodeName)
		fakeOVN.startWithDBSetup(
			libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{logicalSwitch}},
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": chassisID},
				},
			},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}},
			pod,
			&nadapi.NetworkAttachmentDefinitionList{Items: []nadapi.NetworkAttachmentDefinition{*nad}},
		)
		DeferCleanup(fakeOVN.shutdown)

		Expect(fakeOVN.NewUserDefinedNetworkController(nad)).To(Succeed())
		controller, ok := fakeOVN.userDefinedNetworkControllers[networkName]
		Expect(ok).To(BeTrue())
		bnc := controller.bnc

		Expect(bnc.lsManager.AddOrUpdateSwitch(switchName, nil, nil, nil)).To(Succeed())

		Expect(bnc.WatchPods()).To(Succeed())

		portName := util.GetUserDefinedNetworkLogicalPortName(namespaceName, podName, nadKey)
		newExpectedLSP := func(addresses string) *nbdb.LogicalSwitchPort {
			return &nbdb.LogicalSwitchPort{
				UUID:         portName + "-UUID",
				Name:         portName,
				Addresses:    []string{addresses},
				PortSecurity: []string{addresses},
				ExternalIDs: map[string]string{
					"pod":                    "true",
					"namespace":              namespaceName,
					types.NetworkExternalID:  networkName,
					types.NADExternalID:      nadKey,
					types.TopologyExternalID: types.LocalnetTopology,
				},
				Options: map[string]string{
					"requested-chassis": chassisID,
					"iface-id-ver":      podName,
				},
			}
		}
		expectedSwitch := &nbdb.LogicalSwitch{
			UUID:  switchName + "-UUID",
			Name:  switchName,
			Ports: []string{portName + "-UUID"},
		}

		By("the pod add creates the LSP with just the MAC Address")
		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedSwitch, newExpectedLSP(podMAC)))

		By("the CNI patches the DHCP-learned IP into the pod-networks annotation and the pod-update event tightens the LSP to MAC+IP")
		updatedPod, err := fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespaceName).
			Get(context.Background(), podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		updatedPod.Annotations[types.OvnPodAnnotationName] = leasedAnnotation
		// the fake clientset doesn't bump ResourceVersion; informers need it
		// to change to deliver the update event
		updatedPod.ResourceVersion = "42"
		_, err = fakeOVN.fakeClient.KubeClient.CoreV1().Pods(namespaceName).
			Update(context.Background(), updatedPod, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(fakeOVN.nbClient).Should(libovsdbtest.HaveData(expectedSwitch, newExpectedLSP(podMAC+" "+podIP)))
	})
})

// nadKeyNameOverlay keeps GetPrimaryNADForNamespace (and thus namespace-informer
// NotFound) on the wrapped manager, while allowing tests to claim a NAD key.
type nadKeyNameOverlay struct {
	networkmanager.Interface
	nadToNetwork map[string]string
}

func (m *nadKeyNameOverlay) GetNetworkNameForNADKey(nadKey string) string {
	if name, ok := m.nadToNetwork[nadKey]; ok {
		return name
	}
	return m.Interface.GetNetworkNameForNADKey(nadKey)
}
