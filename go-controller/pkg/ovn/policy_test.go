// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	utilnet "k8s.io/utils/net"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/addresssetmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func getFakeController(controllerName string) *DefaultNetworkController {
	controller := &DefaultNetworkController{
		BaseNetworkController: BaseNetworkController{
			controllerName:      controllerName,
			ReconcilableNetInfo: &util.DefaultNetInfo{},
		},
	}
	return controller
}

func getFakeBaseController(netInfo util.NetInfo) *BaseNetworkController {
	return &BaseNetworkController{
		controllerName:      getNetworkControllerName(netInfo.GetNetworkName()),
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
	}
}

var _ = ginkgo.Describe("network policy scale metrics", func() {
	ginkgo.It("uses the shared scale-metrics gate", func() {
		savedEnableScaleMetrics := config.Metrics.EnableScaleMetrics
		ginkgo.DeferCleanup(func() {
			config.Metrics.EnableScaleMetrics = savedEnableScaleMetrics
		})

		config.Metrics.EnableScaleMetrics = true
		primaryNetInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "primary-udn"},
			Role:     types.NetworkRolePrimary,
			Topology: types.Layer3Topology,
			NADName:  "ns/primary-udn",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		secondaryNetInfo, err := util.NewNetInfo(&ovncnitypes.NetConf{
			NetConf:  cnitypes.NetConf{Name: "secondary-udn"},
			Role:     types.NetworkRoleSecondary,
			Topology: types.Layer3Topology,
			NADName:  "ns/secondary-udn",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		gomega.Expect(getFakeBaseController(&util.DefaultNetInfo{}).networkPolicyMetricsEnabled()).To(gomega.BeTrue())
		gomega.Expect(getFakeBaseController(primaryNetInfo).networkPolicyMetricsEnabled()).To(gomega.BeTrue())
		gomega.Expect(getFakeBaseController(secondaryNetInfo).networkPolicyMetricsEnabled()).To(gomega.BeFalse())

		config.Metrics.EnableScaleMetrics = false
		gomega.Expect(getFakeBaseController(primaryNetInfo).networkPolicyMetricsEnabled()).To(gomega.BeFalse())
	})
})

// getDefaultDenyData builds namespace-owned port groups, considering the same ports are selected for ingress
// and egress
func getDefaultDenyDataHelper(policyTypeIngress, policyTypeEgress bool, params *netpolDataParams) []libovsdbtest.TestData {
	namespace := params.networkPolicy.Namespace
	denyLogSeverity := params.denyLogSeverity
	fakeController := getFakeBaseController(params.netInfo)
	egressPGName := fakeController.defaultDenyPortGroupName(namespace, libovsdbutil.ACLEgress)
	shouldBeLogged := denyLogSeverity != ""
	aclIDs := fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLEgress, defaultDenyACL)
	egressDenyACL := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(aclIDs),
		nbdb.ACLDirectionFromLport,
		types.DefaultDenyPriority,
		"inport == @"+egressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	egressDenyACL.UUID = aclIDs.String() + "-UUID"

	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLEgress, arpAllowACL)
	egressARPAllowACL := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(aclIDs),
		nbdb.ACLDirectionFromLport,
		types.DefaultAllowPriority,
		"inport == @"+egressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	egressARPAllowACL.UUID = aclIDs.String() + "-UUID"

	testData := []libovsdbtest.TestData{
		egressDenyACL,
		egressARPAllowACL,
	}
	egressACLs := []*nbdb.ACL{egressDenyACL, egressARPAllowACL}

	if config.OVNKubernetesFeature.AllowICMPNetworkPolicy {
		aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLEgress, icmpAllowACL)
		egressICMPAllowACL := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(aclIDs),
			nbdb.ACLDirectionFromLport,
			types.DefaultAllowPriority,
			"inport == @"+egressPGName+" && "+icmpAllowPolicyMatch,
			nbdb.ACLActionAllow,
			types.OvnACLLoggingMeter,
			"",
			false,
			aclIDs.GetExternalIDs(),
			map[string]string{
				"apply-after-lb": "true",
			},
			types.DefaultACLTier,
		)
		egressICMPAllowACL.UUID = aclIDs.String() + "-UUID"
		testData = append(testData, egressICMPAllowACL)
		egressACLs = append(egressACLs, egressICMPAllowACL)

	}

	ingressPGName := fakeController.defaultDenyPortGroupName(namespace, libovsdbutil.ACLIngress)
	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLIngress, defaultDenyACL)
	ingressDenyACL := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(aclIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultDenyPriority,
		"outport == @"+ingressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	ingressDenyACL.UUID = aclIDs.String() + "-UUID"

	aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLIngress, arpAllowACL)
	ingressARPAllowACL := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(aclIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		"outport == @"+ingressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	ingressARPAllowACL.UUID = aclIDs.String() + "-UUID"

	ingressACLs := []*nbdb.ACL{ingressDenyACL, ingressARPAllowACL}
	if config.OVNKubernetesFeature.AllowICMPNetworkPolicy {
		aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, libovsdbutil.ACLIngress, icmpAllowACL)
		ingressICMPAllowACL := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(aclIDs),
			nbdb.ACLDirectionToLport,
			types.DefaultAllowPriority,
			"outport == @"+ingressPGName+" && "+icmpAllowPolicyMatch,
			nbdb.ACLActionAllow,
			types.OvnACLLoggingMeter,
			"",
			false,
			aclIDs.GetExternalIDs(),
			nil,
			types.DefaultACLTier,
		)
		ingressICMPAllowACL.UUID = aclIDs.String() + "-UUID"
		ingressACLs = append(ingressACLs, ingressICMPAllowACL)
	}

	for _, acl := range ingressACLs {
		testData = append(testData, acl)
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range params.localPortUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	var egressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeEgress {
		egressDenyPorts = lsps
	}

	egressDenyPG := libovsdbutil.BuildPortGroup(
		fakeController.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLEgress),
		egressDenyPorts,
		egressACLs,
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	var ingressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeIngress {
		ingressDenyPorts = lsps
	}
	ingressDenyPG := libovsdbutil.BuildPortGroup(
		fakeController.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLIngress),
		ingressDenyPorts,
		ingressACLs,
	)
	ingressDenyPG.UUID = ingressDenyPG.Name + "-UUID"

	return append(testData, egressDenyPG, ingressDenyPG)
}

func getDefaultDenyData(params *netpolDataParams) []libovsdbtest.TestData {
	policyTypeIngress, policyTypeEgress := getPolicyType(params.networkPolicy)
	return getDefaultDenyDataHelper(policyTypeIngress, policyTypeEgress, params)
}

// getDefaultDenyDataMultiplePolicies considers that all networkPolicies belong to the same namespace,
// networkPolicies list should not be empty, ingress and egress default deny groups will have the same ports.
func getDefaultDenyDataMultiplePolicies(networkPolicies []*knet.NetworkPolicy) []libovsdbtest.TestData {
	var policyTypeIngress, policyTypeEgress bool
	for _, policy := range networkPolicies {
		ingress, egress := getPolicyType(policy)
		policyTypeIngress = policyTypeIngress || ingress
		policyTypeEgress = policyTypeEgress || egress
	}
	return getDefaultDenyDataHelper(policyTypeIngress, policyTypeEgress, newNetpolDataParams(networkPolicies[0]))
}

// getGressACLsAndAddrSets can only handle tcpPeerPorts for policies built with getPortNetworkPolicy, i.e. peer should only
// have `Ports` filled, but no `To`/`From`.
func getGressACLsAndAddrSets(gressIdx int, peers []knet.NetworkPolicyPeer, policyType knet.PolicyType,
	params *netpolDataParams) ([]*nbdb.ACL, []*nbdb.AddressSet) {
	namespace := params.networkPolicy.Namespace
	fakeController := getFakeBaseController(params.netInfo)
	pgName := fakeController.getNetworkPolicyPGName(namespace, params.networkPolicy.Name)
	controllerName := getNetworkControllerName(params.netInfo.GetNetworkName())
	shouldBeLogged := params.allowLogSeverity != ""
	var options map[string]string
	var direction string
	var portDir string
	var ipDir string
	acls := []*nbdb.ACL{}
	addressSets := []*nbdb.AddressSet{}
	if policyType == knet.PolicyTypeEgress {
		options = map[string]string{
			"apply-after-lb": "true",
		}
		direction = nbdb.ACLDirectionFromLport
		portDir = "inport"
		ipDir = "dst"
	} else {
		direction = nbdb.ACLDirectionToLport
		portDir = "outport"
		ipDir = "src"
	}
	hashedASNames := []string{}
	ipBlocks := []string{}
	for _, peer := range peers {
		// follow the algorithm from setupGressPolicy
		if peer.NamespaceSelector != nil || peer.PodSelector != nil {
			podSelector := peer.PodSelector
			if podSelector == nil {
				// nil pod selector is equivalent to empty pod selector, which selects all
				podSelector = &metav1.LabelSelector{}
			}
			legacyNetpolMode := useNamespaceAddrSet(peer)
			peerIndex := addresssetmanager.GetPodSelectorAddrSetDbIDs(podSelector, peer.NamespaceSelector, nil, namespace, controllerName, legacyNetpolMode)
			netpolASv4, _ := addressset.GetTestDbAddrSets(peerIndex, params.peerIPs)
			addressSets = append(addressSets, netpolASv4)
			asv4, _ := addressset.GetHashNamesForAS(peerIndex)
			hashedASNames = append(hashedASNames, asv4)
		}
		if peer.IPBlock != nil {
			ipBlocks = append(ipBlocks, peer.IPBlock.CIDR)
		}
	}
	gp := gressPolicy{
		policyNamespace: namespace,
		policyName:      params.networkPolicy.Name,
		policyType:      policyType,
		idx:             gressIdx,
		controllerName:  controllerName,
	}
	if len(hashedASNames) > 0 {
		gressAsMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.%s == {%s} && %s == @%s", ipDir, gressAsMatch, portDir, pgName)
		action := allowAction(params.statelessNetPol)
		dbIDs := gp.getNetpolACLDbIDs(emptyIdx, libovsdbutil.UnspecifiedL4Protocol)
		acl := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			match,
			action,
			types.OvnACLLoggingMeter,
			params.allowLogSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
			types.DefaultACLTier,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	if len(ipBlocks) > 0 {
		var ipBlockMatches []string
		for _, ipBlock := range ipBlocks {
			ipVersion := "ip4"
			if utilnet.IsIPv6CIDRString(ipBlock) {
				ipVersion = "ip6"
			}
			ipBlockMatches = append(ipBlockMatches, fmt.Sprintf("%s.%s == %s", ipVersion, ipDir, ipBlock))
		}
		var match string
		if len(ipBlockMatches) == 1 {
			match = ipBlockMatches[0]
		} else {
			match = fmt.Sprintf("(%s)", strings.Join(ipBlockMatches, " || "))
		}
		match = fmt.Sprintf("%s && %s == @%s", match, portDir, pgName)
		action := allowAction(params.statelessNetPol)
		dbIDs := gp.getNetpolACLDbIDs(ipBlockCombinedIdx, libovsdbutil.UnspecifiedL4Protocol)
		acl := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			match,
			action,
			types.OvnACLLoggingMeter,
			params.allowLogSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
			types.DefaultACLTier,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	for _, v := range params.tcpPeerPorts {
		dbIDs := gp.getNetpolACLDbIDs(emptyIdx, "tcp")
		action := allowAction(params.statelessNetPol)
		acl := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			fmt.Sprintf("ip4 && tcp && tcp.dst==%d && %s == @%s", v, portDir, pgName),
			action,
			types.OvnACLLoggingMeter,
			params.allowLogSeverity,
			shouldBeLogged,
			dbIDs.GetExternalIDs(),
			options,
			types.DefaultACLTier,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	return acls, addressSets
}

func allowAction(statelessNetPol bool) nbdb.ACLAction {
	if statelessNetPol {
		return nbdb.ACLActionAllowStateless
	}
	return nbdb.ACLActionAllowRelated
}

type netpolDataParams struct {
	networkPolicy    *knet.NetworkPolicy
	localPortUUIDs   []string
	peerIPs          []string
	tcpPeerPorts     []int32
	allowLogSeverity nbdb.ACLSeverity
	denyLogSeverity  nbdb.ACLSeverity
	statelessNetPol  bool
	netInfo          util.NetInfo
}

func getPolicyData(params *netpolDataParams) []libovsdbtest.TestData {
	acls := []*nbdb.ACL{}
	addrSets := []*nbdb.AddressSet{}
	for i, ingress := range params.networkPolicy.Spec.Ingress {
		newACLs, newAddrSets := getGressACLsAndAddrSets(i, ingress.From, knet.PolicyTypeIngress, params)
		acls = append(acls, newACLs...)
		// use shared address sets, dedup
		for _, newAddrSet := range newAddrSets {
			found := false
			for _, addrSet := range addrSets {
				if newAddrSet.UUID == addrSet.UUID {
					found = true
					break
				}
			}
			if !found {
				addrSets = append(addrSets, newAddrSet)
			}
		}
	}
	for i, egress := range params.networkPolicy.Spec.Egress {
		newACLs, newAddrSets := getGressACLsAndAddrSets(i, egress.To, knet.PolicyTypeEgress, params)
		acls = append(acls, newACLs...)
		// use shared address sets, dedup
		for _, newAddrSet := range newAddrSets {
			found := false
			for _, addrSet := range addrSets {
				if newAddrSet.UUID == addrSet.UUID {
					found = true
					break
				}
			}
			if !found {
				addrSets = append(addrSets, newAddrSet)
			}
		}
	}

	pg := getPolicyPortGroup(params, acls)

	data := []libovsdbtest.TestData{}
	for _, addrSet := range addrSets {
		data = append(data, addrSet)
	}
	for _, acl := range acls {
		data = append(data, acl)
	}
	data = append(data, pg)
	return data
}

func getPolicyPortGroup(params *netpolDataParams, acls []*nbdb.ACL) *nbdb.PortGroup {
	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range params.localPortUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	fakeController := getFakeBaseController(params.netInfo)
	pgDbIDs := fakeController.getNetworkPolicyPortGroupDbIDs(params.networkPolicy.Namespace, params.networkPolicy.Name)
	pg := libovsdbutil.BuildPortGroup(
		pgDbIDs,
		lsps,
		acls,
	)
	pg.UUID = pg.Name + "-UUID"

	return pg
}

func newNetpolDataParams(networkPolicy *knet.NetworkPolicy) *netpolDataParams {
	return &netpolDataParams{
		networkPolicy:    networkPolicy,
		localPortUUIDs:   nil,
		peerIPs:          nil,
		tcpPeerPorts:     nil,
		allowLogSeverity: "",
		denyLogSeverity:  "",
		statelessNetPol:  false,
		netInfo:          &util.DefaultNetInfo{},
	}
}

func (p *netpolDataParams) withPeerIPs(peerIPs ...string) *netpolDataParams {
	p.peerIPs = peerIPs
	return p
}

func (p *netpolDataParams) withLocalPortUUIDs(localPortUUIDs ...string) *netpolDataParams {
	p.localPortUUIDs = localPortUUIDs
	return p
}

func (p *netpolDataParams) withTCPPeerPorts(tcpPeerPorts ...int32) *netpolDataParams {
	p.tcpPeerPorts = tcpPeerPorts
	return p
}

func (p *netpolDataParams) withAllowLogSeverity(allowLogSeverity nbdb.ACLSeverity) *netpolDataParams {
	p.allowLogSeverity = allowLogSeverity
	return p
}

func (p *netpolDataParams) withDenyLogSeverity(denyLogSeverity nbdb.ACLSeverity) *netpolDataParams {
	p.denyLogSeverity = denyLogSeverity
	return p
}

func (p *netpolDataParams) withStateless(statelessNetPol bool) *netpolDataParams {
	p.statelessNetPol = statelessNetPol
	return p
}

func (p *netpolDataParams) withNetInfo(netInfo util.NetInfo) *netpolDataParams {
	p.netInfo = netInfo
	return p
}

func getNamespaceWithSinglePolicyExpectedData(params *netpolDataParams, initialDB []libovsdbtest.TestData) []libovsdbtest.TestData {
	gressPolicyExpectedData := getPolicyData(params)
	defaultDenyExpectedData := getDefaultDenyData(params)
	expectedData := initialDB
	expectedData = append(expectedData, gressPolicyExpectedData...)
	expectedData = append(expectedData, defaultDenyExpectedData...)
	return expectedData
}

// getNamespaceWithMultiplePoliciesExpectedData considers that all networkPolicies belong to the same namespace,
// networkPolicies list should not be empty, all listed network policies are expected to have the same localPortUUIDs,
// peerNamespaces, and tcpPeerPorts.
func getNamespaceWithMultiplePoliciesExpectedData(networkPolicies []*knet.NetworkPolicy,
	initialDB []libovsdbtest.TestData) []libovsdbtest.TestData {
	expectedData := initialDB
	for _, policy := range networkPolicies {
		gressPolicyExpectedData := getPolicyData(newNetpolDataParams(policy))
		expectedData = append(expectedData, gressPolicyExpectedData...)
	}
	defaultDenyExpectedData := getDefaultDenyDataMultiplePolicies(networkPolicies)
	expectedData = append(expectedData, defaultDenyExpectedData...)
	return expectedData
}

func getHairpinningACLsV4AndPortGroup() []libovsdbtest.TestData {
	return getHairpinningACLsV4AndPortGroupForNetwork(&util.DefaultNetInfo{}, nil)
}

func getHairpinningACLsV4AndPortGroupForNetwork(netInfo util.NetInfo, ports []string) []libovsdbtest.TestData {
	controllerName := getNetworkControllerName(netInfo.GetNetworkName())
	clusterPortGroup := newNetworkClusterPortGroup(netInfo)
	fakeController := getFakeController(controllerName)
	egressIDs := fakeController.getNetpolDefaultACLDbIDs("Egress")
	egressACL := libovsdbops.BuildACL(
		"",
		nbdb.ACLDirectionFromLport,
		types.DefaultAllowPriority,
		fmt.Sprintf("%s.src == %s", "ip4", config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP.String()),
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		egressIDs.GetExternalIDs(),
		map[string]string{
			"apply-after-lb": "true",
		},
		types.DefaultACLTier,
	)
	egressACL.UUID = fmt.Sprintf("hp-egress-%s", controllerName)
	ingressIDs := fakeController.getNetpolDefaultACLDbIDs("Ingress")
	ingressACL := libovsdbops.BuildACL(
		"",
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		fmt.Sprintf("%s.src == %s", "ip4", config.Gateway.MasqueradeIPs.V4OVNServiceHairpinMasqueradeIP.String()),
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		ingressIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier,
	)
	ingressACL.UUID = fmt.Sprintf("hp-ingress-%s", controllerName)
	clusterPortGroup.ACLs = []string{egressACL.UUID, ingressACL.UUID}
	clusterPortGroup.Ports = ports
	return []libovsdbtest.TestData{egressACL, ingressACL, clusterPortGroup}
}

func getPortNetworkPolicy(policyName, namespace, labelName, labelVal string, tcpPort int32) *knet.NetworkPolicy {
	tcpProtocol := corev1.ProtocolTCP
	return ovntest.NewTestNetworkPolicy(policyName, namespace,
		metav1.LabelSelector{
			MatchLabels: map[string]string{
				labelName: labelVal,
			},
		},
		[]knet.NetworkPolicyIngressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port: &intstr.IntOrString{IntVal: tcpPort},
			}},
		}},
		[]knet.NetworkPolicyEgressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
			}},
		}},
	)
}

func getTestPod(namespace, nodeName string) testPod {
	return getTestPodAdvanced(namespace, nodeName, "10.128.1.3", false)
}

func getTestPodAdvanced(namespace, nodeName, podIP string, hostNetwork bool) testPod {
	ip := net.ParseIP(podIP).To4()
	mac := net.HardwareAddr{
		0x02, 0x00,
		ip[0], ip[1], ip[2], ip[3],
	}
	tPod := newTPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod"+strings.ReplaceAll(podIP, ".", "-"),
		podIP,
		mac.String(),
		namespace,
	)
	tPod.hostNetwork = hostNetwork
	return tPod
}

var _ = ginkgo.Describe("OVN NetworkPolicy Operations", func() {
	const (
		namespaceName1        = "namespace1"
		namespaceName2        = "namespace2"
		netPolicyName1        = "networkpolicy1"
		netPolicyName2        = "networkpolicy2"
		nodeName              = "node1"
		node1Subnet           = "10.244.0.0/24"
		node1MgmtIP           = "10.244.0.2"
		labelName      string = "pod-name"
		labelVal       string = "server"
		portNum        int32  = 81
	)
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup

		gomegaFormatMaxLength int
		logicalSwitch         *nbdb.LogicalSwitch
		clusterPortGroup      *nbdb.PortGroup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(false, nodeName)

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		logicalSwitch = &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
		clusterPortGroup = newClusterPortGroup()
		initialData := getHairpinningACLsV4AndPortGroup()
		initialData = append(initialData, logicalSwitch)
		initialDB = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	startOvn := func(dbSetup libovsdbtest.TestSetup, namespaces []corev1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string) {
		var podsList []corev1.Pod
		for _, testPod := range pods {
			knetPod := ovntest.NewPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			knetPod.Annotations = map[string]string{types.OvnPodAnnotationName: fmt.Sprintf(`{"default":{"mac_address":"%s","ip_address":"%s/24","role":"infrastructure-locked"}}`, testPod.podMAC, testPod.podIP)}
			if testPod.hostNetwork {
				knetPod.Spec.HostNetwork = true
			}
			podsList = append(podsList, *knetPod)
		}
		testNode := newNode(nodeName, "192.168.126.202/24")
		testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
		fakeOvn.startWithDBSetup(dbSetup,
			&corev1.NamespaceList{
				Items: namespaces,
			},
			&corev1.NodeList{
				Items: []corev1.Node{
					*testNode,
				},
			},
			&corev1.PodList{
				Items: podsList,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
		)
		for _, testPod := range pods {
			testPod.populateLogicalSwitchCache(fakeOvn)
		}
		var err error
		if namespaces != nil {
			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		if pods != nil {
			err = fakeOvn.controller.WatchPods()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		//gomega.Expect(fakeOvn.controller.addressSetManager.Start()).To(gomega.Succeed())
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	getUpdatedInitialDB := func(tPods []testPod) []libovsdbtest.TestData {
		updatedSwitchAndPods := getDefaultNetExpectedPodsAndSwitches(tPods, []string{nodeName})
		return append(getHairpinningACLsV4AndPortGroup(), updatedSwitchAndPods...)
	}

	ginkgo.Context("on startup", func() {
		ginkgo.It("creates default hairpinning ACLs", func() {
			app.Action = func(*cli.Context) error {
				clusterPortGroup = newClusterPortGroup()
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						logicalSwitch,
						clusterPortGroup,
					},
				}
				startOvn(initialDB, nil, nil, nil, nil)

				hairpinningACLs := getHairpinningACLsV4AndPortGroup()
				expectedData := []libovsdbtest.TestData{logicalSwitch}
				expectedData = append(expectedData, hairpinningACLs...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))
				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("deletes stale port groups", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				// network policy with peer selector, has an address set
				networkPolicy1 := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", "podName", true, true)
				// network policy with ipBlock (won't have any address sets)
				networkPolicy2 := ovntest.NewTestNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{{
							IPBlock: &knet.IPBlock{
								CIDR: "1.1.1.0/24",
							}},
						}},
					}, nil)

				initialData := initialDB.NBData
				afterCleanup := initialDB.NBData
				policy1Db := getPolicyData(newNetpolDataParams(networkPolicy1))
				policy1PeerAddrSet := policy1Db[0]
				initialData = append(initialData, policy1Db...)

				policy2Db := getPolicyData(newNetpolDataParams(networkPolicy2))
				initialData = append(initialData, policy2Db...)

				defaultDenyDb := getDefaultDenyData(newNetpolDataParams(networkPolicy1))
				initialData = append(initialData, defaultDenyDb...)

				// start ovn with no objects, expect stale port db entries to be cleaned up
				// peer selector address set will stay until the next restart
				startOvn(libovsdbtest.TestSetup{NBData: initialData}, nil, nil, nil, nil)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(append(afterCleanup, policy1PeerAddrSet)))

				// now restart address set manager
				fakeOvn.controller.addressSetManager.Stop()
				gomega.Expect(fakeOvn.controller.addressSetManager.Start()).To(gomega.Succeed())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(afterCleanup))
				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.DescribeTable("reconciles an existing networkPolicy with empty db",
			func(allowICMPNetworkPolicy bool) {
				app.Action = func(*cli.Context) error {
					config.OVNKubernetesFeature.AllowICMPNetworkPolicy = allowICMPNetworkPolicy
					namespace1 := *ovntest.NewNamespace(namespaceName1)
					namespace2 := *ovntest.NewNamespace(namespaceName2)
					// add namespaces to initial Database

					networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
						namespace2.Name, "", true, true)
					startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
						nil, nil)

					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
						Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					expectedData := getNamespaceWithSinglePolicyExpectedData(
						newNetpolDataParams(networkPolicy),
						initialDB.NBData)
					gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))
					return nil
				}

				gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
			},
			ginkgo.Entry("with allow ICMP network policy disabled", false),
			ginkgo.Entry("with allow ICMP network policy enabled", true),
		)

		ginkgo.It("reconciles an ingress networkPolicy updating an existing ACL", func() {
			app.Action = func(*cli.Context) error {

				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				// add namespaces to initial Database

				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)

				// network policy without peer namespace
				initialData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)

				startOvn(libovsdbtest.TestSetup{NBData: initialData}, []corev1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check peer namespace was added
				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod selector in its own namespace from empty db", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				// network policy with peer pod selector
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withLocalPortUUIDs(nPodTest.portUUID).withPeerIPs(nPodTest.podIP),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod and namespace selector in another namespace from empty db", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				// network policy with peer pod and namespace selector
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerIPs(nPodTest.podIP),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles existing networkPolicies with equivalent rules", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				peer := knet.NetworkPolicyPeer{
					IPBlock: &knet.IPBlock{
						CIDR: "1.1.1.1",
					},
				}
				// equivalent rules in one peer
				networkPolicy1 := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer, peer},
					}}, nil)
				// equivalent rules in different peers
				networkPolicy2 := ovntest.NewTestNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
					}, nil)
				initialData := initialDB.NBData
				gressPolicy1ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy1))
				gressPolicy2ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy2))
				defaultDenyExpectedData := getDefaultDenyDataMultiplePolicies([]*knet.NetworkPolicy{networkPolicy1, networkPolicy2})
				initialData = append(initialData, gressPolicy1ExpectedData...)
				initialData = append(initialData, gressPolicy2ExpectedData...)
				initialData = append(initialData, defaultDenyExpectedData...)

				// start with the updated network policy, but previous-version db
				networkPolicy1Updated := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer},
					}}, nil)

				startOvn(libovsdbtest.TestSetup{NBData: initialData}, []corev1.Namespace{namespace1},
					[]knet.NetworkPolicy{*networkPolicy1Updated, *networkPolicy2},
					nil, nil)

				// check the initial data is updated, one acl should be removed
				gressUpdatedPolicy1ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy1Updated))
				finalData := initialDB.NBData
				finalData = append(finalData, gressUpdatedPolicy1ExpectedData...)
				finalData = append(finalData, gressPolicy2ExpectedData...)
				finalData = append(finalData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles existing networkPolicies with has legacy ipBlock ACLs", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				peer := knet.NetworkPolicyPeer{
					IPBlock: &knet.IPBlock{
						CIDR: "1.1.1.1",
					},
				}
				// equivalent rules in one peer
				networkPolicy1 := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer, peer},
					}}, nil)
				// equivalent rules in different peers
				networkPolicy2 := ovntest.NewTestNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
					}, nil)
				initialData := initialDB.NBData
				defaultDenyExpectedData := getDefaultDenyDataMultiplePolicies([]*knet.NetworkPolicy{networkPolicy1, networkPolicy2})
				initialData = append(initialData, defaultDenyExpectedData...)

				// NetworkPolicy 1 contains a single gress policy that previously
				// created one legacy ACL per ipBlock. Simulate two legacy ACLs
				// corresponding to ipBlock indexes 0 and 1 of the gress policy.
				// ACL1 => libovsdbops.GressIdxKey: 0, libovsdbops.IpBlockIndexKey: 0
				// ACL2 => libovsdbops.GressIdxKey: 0, libovsdbops.IpBlockIndexKey: 1
				netInfo := &util.DefaultNetInfo{}
				fakeController := getFakeBaseController(netInfo)
				controllerName := getNetworkControllerName(netInfo.GetNetworkName())
				pgName1 := fakeController.getNetworkPolicyPGName(namespace1.Name, networkPolicy1.Name)
				gp1 := gressPolicy{
					policyNamespace: networkPolicy1.Namespace,
					policyName:      networkPolicy1.Name,
					policyType:      knet.PolicyTypeIngress,
					idx:             0,
					controllerName:  controllerName,
				}
				var legacyACLPolicy1 []*nbdb.ACL
				for idx := 0; idx < 2; idx++ {
					legacyACLIDs := gp1.getNetpolACLDbIDs(idx, libovsdbutil.UnspecifiedL4Protocol)
					legacyACL := libovsdbops.BuildACL(
						libovsdbutil.GetACLName(legacyACLIDs),
						nbdb.ACLDirectionToLport,
						types.DefaultAllowPriority,
						fmt.Sprintf("ip4.src == 1.1.1.1 && outport == @%s", pgName1),
						nbdb.ACLActionAllowRelated,
						types.OvnACLLoggingMeter,
						"",
						false,
						legacyACLIDs.GetExternalIDs(),
						nil,
						types.DefaultACLTier,
					)
					legacyACL.UUID = legacyACLIDs.String() + "-UUID"
					initialData = append(initialData, legacyACL)
					legacyACLPolicy1 = append(legacyACLPolicy1, legacyACL)
				}
				pgNetworkPolicy1 := getPolicyPortGroup(newNetpolDataParams(networkPolicy1), legacyACLPolicy1)
				initialData = append(initialData, pgNetworkPolicy1)

				// NetworkPolicy 2 contains two gress policies, each with one legacy
				// ACL per ipBlock. Simulate two legacy ACL corresponding to gress
				// policy indexes 0 and 1, respectively.
				// ACL1 => libovsdbops.GressIdxKey: 0, libovsdbops.IpBlockIndexKey: 0
				// ACL2 => libovsdbops.GressIdxKey: 1, libovsdbops.IpBlockIndexKey: 0
				pgName2 := fakeController.getNetworkPolicyPGName(namespace1.Name, networkPolicy2.Name)
				firstgp2 := gressPolicy{
					policyNamespace: networkPolicy2.Namespace,
					policyName:      networkPolicy2.Name,
					policyType:      knet.PolicyTypeIngress,
					idx:             0,
					controllerName:  controllerName,
				}
				secondgp2 := gressPolicy{
					policyNamespace: networkPolicy2.Namespace,
					policyName:      networkPolicy2.Name,
					policyType:      knet.PolicyTypeIngress,
					idx:             1,
					controllerName:  controllerName,
				}
				legacyACLID := firstgp2.getNetpolACLDbIDs(0, libovsdbutil.UnspecifiedL4Protocol)
				legacyACL := libovsdbops.BuildACL(
					libovsdbutil.GetACLName(legacyACLID),
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					fmt.Sprintf("ip4.src == 1.1.1.1 && outport == @%s", pgName2),
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					legacyACLID.GetExternalIDs(),
					nil,
					types.DefaultACLTier,
				)
				legacyACL.UUID = legacyACLID.String() + "-UUID"
				initialData = append(initialData, legacyACL)

				legacyACLID2 := secondgp2.getNetpolACLDbIDs(0, libovsdbutil.UnspecifiedL4Protocol)
				legacyACL2 := libovsdbops.BuildACL(
					libovsdbutil.GetACLName(legacyACLID2),
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					fmt.Sprintf("ip4.src == 1.1.1.1 && outport == @%s", pgName2),
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					legacyACLID2.GetExternalIDs(),
					nil,
					types.DefaultACLTier,
				)
				legacyACL2.UUID = legacyACLID2.String() + "-UUID"
				initialData = append(initialData, legacyACL2)
				pgNetworkPolicy2 := getPolicyPortGroup(newNetpolDataParams(networkPolicy2), []*nbdb.ACL{legacyACL, legacyACL2})
				initialData = append(initialData, pgNetworkPolicy2)

				startOvn(libovsdbtest.TestSetup{NBData: initialData}, []corev1.Namespace{namespace1},
					[]knet.NetworkPolicy{*networkPolicy1, *networkPolicy2},
					nil, nil)

				// check the initial data is updated and all legacy ACLs should be cleaned up
				gressPolicy1ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy1))
				gressPolicy2ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy2))
				finalData := initialDB.NBData
				finalData = append(finalData, gressPolicy1ExpectedData...)
				finalData = append(finalData, gressPolicy2ExpectedData...)
				finalData = append(finalData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})

	ginkgo.Context("during execution", func() {
		const ns1PodIP = "10.128.1.3"
		const ns1HostnetPodIP = "10.0.1.1"
		const ns2PodIP = "10.128.1.4"
		ginkgo.DescribeTable("correctly uses shared peer selector address sets",
			func(peer knet.NetworkPolicyPeer, peerIPs []string) {
				config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
				hostNetNamespace := *ovntest.NewNamespace(config.Kubernetes.HostNetworkNamespace)
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				ns1Pod := getTestPodAdvanced(namespace1.Name, nodeName, ns1PodIP, false)
				ns1HosnetPod := getTestPodAdvanced(namespace1.Name, nodeName, ns1HostnetPodIP, true)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				ns2Pod := getTestPodAdvanced(namespace2.Name, nodeName, ns2PodIP, false)

				netpol := ovntest.NewTestNetworkPolicy("netpolName", namespace1.Name, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{peer},
					},
				}, nil)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2, hostNetNamespace}, []knet.NetworkPolicy{*netpol},
					[]testPod{ns1Pod, ns1HosnetPod, ns2Pod}, nil)

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(netpol).withPeerIPs(peerIPs...).withLocalPortUUIDs(ns1Pod.portUUID),
					getUpdatedInitialDB([]testPod{ns1Pod, ns2Pod}))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
			},
			ginkgo.Entry("empty pod selector => use pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{},
				}, []string{ns1PodIP, ns1HostnetPodIP}),
			ginkgo.Entry("namespace selector with nil pod selector => use pod selector + legacyNetpolMode=true",
				knet.NetworkPolicyPeer{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName1,
						},
					},
				}, []string{ns1PodIP}),
			ginkgo.Entry("empty namespace and pod selector => use all pods shared address set",
				knet.NetworkPolicyPeer{
					PodSelector:       &metav1.LabelSelector{},
					NamespaceSelector: &metav1.LabelSelector{},
				}, []string{ns1PodIP, ns1HostnetPodIP, ns2PodIP}),
			ginkgo.Entry("pod selector with nil namespace => use static namespace+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
				}, []string{}),
			ginkgo.Entry("pod selector with namespace selector => use namespace selector+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, []string{ns2PodIP}),
			ginkgo.Entry("pod selector with empty namespace selector => use global pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
					NamespaceSelector: &metav1.LabelSelector{},
				}, []string{}),
			ginkgo.Entry("empty namespace selector matches on HostNetworkNamespace IPs + legacyNetpolMode=true",
				knet.NetworkPolicyPeer{
					NamespaceSelector: &metav1.LabelSelector{},
				}, []string{ns1PodIP, ns2PodIP, node1MgmtIP}),
		)

		ginkgo.It("correctly creates and deletes a networkpolicy allowing a port to a local pod", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).
					withTCPPeerPorts(portNum)
				gressPolicy1ExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy2).
					withLocalPortUUIDs(nPodTest.portUUID).
					withTCPPeerPorts(portNum + 1))
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDataWithPolicy2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("pod reconcile updates existing network policy local pod membership", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				pod := ovntest.NewPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, nPodTest)

				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespace1}},
					&corev1.NodeList{Items: []corev1.Node{*testNode}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				np := NewNetworkPolicy(networkPolicy)
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, getDefaultDenyData(dataParams)...)
				policyPG := libovsdbutil.BuildPortGroup(
					pgDbIDs,
					[]*nbdb.LogicalSwitchPort{{UUID: nPodTest.portUUID}},
					nil,
				)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				pod.Labels = map[string]string{labelName: "does-not-match"}
				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(networkPolicy))...)
				policyPG = libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("restores exact default-deny references and asserted UUIDs when add or delete OVN operations fail", func() {
			app.Action = func(*cli.Context) error {
				fakeOvn.startWithDBSetup(initialDB)

				namespace := namespaceName1
				portName := util.GetLogicalPortName(namespace, "pod")
				absentPortName := util.GetLogicalPortName(namespace, "absent-pod")
				freshPortUUID := "fresh-port-uuid"
				stalePortUUID := "stale-port-uuid"
				absentPortUUID := "absent-port-uuid"
				policy1 := NewNetworkPolicy(getPortNetworkPolicy(netPolicyName1, namespace, labelName, labelVal, portNum))
				policy2 := NewNetworkPolicy(getPortNetworkPolicy(netPolicyName2, namespace, labelName, labelVal, portNum+1))
				expectedPolicyKeys := sets.New(policy1.getKey(), policy2.getKey())
				sharedPGs := &defaultDenyPortGroups{
					ingressPortToPolicies:     map[string]sets.Set[string]{portName: expectedPolicyKeys.Clone()},
					egressPortToPolicies:      map[string]sets.Set[string]{portName: expectedPolicyKeys.Clone()},
					ingressPortToAssertedUUID: map[string]string{portName: freshPortUUID},
					egressPortToAssertedUUID:  map[string]string{portName: freshPortUUID},
					policies: map[string]bool{
						policy1.getKey(): true,
						policy2.getKey(): true,
					},
				}
				fakeOvn.controller.sharedNetpolPortGroups.Store(namespace, sharedPGs)

				expectExactState := func() {
					gomega.Expect(sharedPGs.ingressPortToPolicies).To(gomega.Equal(
						map[string]sets.Set[string]{portName: expectedPolicyKeys.Clone()}))
					gomega.Expect(sharedPGs.egressPortToPolicies).To(gomega.Equal(
						map[string]sets.Set[string]{portName: expectedPolicyKeys.Clone()}))
					gomega.Expect(sharedPGs.ingressPortToAssertedUUID).To(gomega.Equal(
						map[string]string{portName: freshPortUUID}))
					gomega.Expect(sharedPGs.egressPortToAssertedUUID).To(gomega.Equal(
						map[string]string{portName: freshPortUUID}))
				}

				invalidOps := []ovsdb.Operation{{
					Op:    ovsdb.OperationDelete,
					Table: "not-a-real-table",
				}}

				// An idempotent add already owns the reference. Inverse rollback
				// would incorrectly remove that pre-existing policy reference.
				err := fakeOvn.controller.denyPGAddPorts(policy1, map[string]string{
					portName:       freshPortUUID,
					absentPortName: absentPortUUID,
				}, invalidOps)
				gomega.Expect(err).To(gomega.HaveOccurred())
				expectExactState()

				// This policy's recorded UUID is stale while another policy keeps
				// the current UUID asserted. Inverse rollback would overwrite the
				// current asserted UUID with the stale value.
				err = fakeOvn.controller.denyPGDeletePorts(policy1, map[string]string{
					portName:       stalePortUUID,
					absentPortName: absentPortUUID,
				}, false, invalidOps)
				gomega.Expect(err).To(gomega.HaveOccurred())
				expectExactState()

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("pod policy membership cleanup succeeds after logical port delete", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				pod := ovntest.NewPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, nPodTest)

				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespace1}},
					&corev1.NodeList{Items: []corev1.Node{*testNode}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				np := NewNetworkPolicy(networkPolicy)
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, loaded := np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())

				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed())
				_, loaded = np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())

				gomega.Expect(fakeOvn.controller.deletePodNetworkPolicyMembership(pod)).To(gomega.Succeed())
				_, loaded = np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeFalse())

				expectedData := getUpdatedInitialDB([]testPod{})
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(networkPolicy))...)
				policyPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("pod policy reconcile preserves recorded membership while its logical port cache entry is transiently unavailable", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				pod := ovntest.NewPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, nPodTest)

				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespace1}},
					&corev1.NodeList{Items: []corev1.Node{*testNode}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				np := NewNetworkPolicy(networkPolicy)
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				appliedPortInfo, err := fakeOvn.controller.logicalPortCache.get(pod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedMembership := map[string]string{appliedPortInfo.name: appliedPortInfo.uuid}
				gomega.Expect(np.getLocalPortsForPod(pod)).To(gomega.Equal(expectedMembership))

				// Cache expiration is a retryable observation, not evidence that this
				// selected pod should be removed from policy port groups.
				fakeOvn.controller.logicalPortCache.remove(pod, types.DefaultNetworkName)
				err = fakeOvn.controller.reconcileNetworkPolicyPortGroups(pod)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("scheduled for removal"))
				gomega.Expect(np.getLocalPortsForPod(pod)).To(gomega.Equal(expectedMembership))

				for _, pgName := range []string{
					np.portGroupName,
					fakeOvn.controller.defaultDenyPortGroupName(networkPolicy.Namespace, libovsdbutil.ACLIngress),
					fakeOvn.controller.defaultDenyPortGroupName(networkPolicy.Namespace, libovsdbutil.ACLEgress),
				} {
					gomega.Expect(PortGroupHasPorts(fakeOvn.controller.nbClient, pgName, []string{appliedPortInfo.uuid})).To(gomega.BeTrue())
				}

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("pod reconcile replaces recorded policy membership of a previous port incarnation", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				pod := ovntest.NewPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, nPodTest)

				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespace1}},
					&corev1.NodeList{Items: []corev1.Node{*testNode}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				np := NewNetworkPolicy(networkPolicy)
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				loadedPortUUID, loaded := np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())
				freshPortUUID := loadedPortUUID.(string)

				// Simulate recorded membership lagging a re-created LSP.
				np.setLocalPortsForPod(pod, map[string]string{nPodTest.portName: "stale-port-uuid"})
				for _, pgName := range []string{
					np.portGroupName,
					fakeOvn.controller.defaultDenyPortGroupName(networkPolicy.Namespace, libovsdbutil.ACLIngress),
					fakeOvn.controller.defaultDenyPortGroupName(networkPolicy.Namespace, libovsdbutil.ACLEgress),
				} {
					gomega.Expect(libovsdbops.DeletePortsFromPortGroup(fakeOvn.controller.nbClient, pgName, freshPortUUID)).To(gomega.Succeed())
				}

				// Reconcile must replace the stale record and reassert PG membership.
				_, err = fakeOvn.controller.ReconcilePod(pod, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				loadedPortUUID, loaded = np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())
				gomega.Expect(loadedPortUUID.(string)).To(gomega.Equal(freshPortUUID))

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, getDefaultDenyData(dataParams)...)
				policyPG := libovsdbutil.BuildPortGroup(
					pgDbIDs,
					[]*nbdb.LogicalSwitchPort{{UUID: nPodTest.portUUID}},
					nil,
				)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("pod reconcile removes policy membership after its node becomes remote", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				pod := ovntest.NewPod(nPodTest.namespace, nPodTest.podName, nPodTest.nodeName, nPodTest.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, nPodTest)

				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB,
					&corev1.NamespaceList{Items: []corev1.Namespace{namespace1}},
					&corev1.NodeList{Items: []corev1.Node{*testNode}},
					&corev1.PodList{Items: []corev1.Pod{*pod}},
				)
				nPodTest.populateLogicalSwitchCache(fakeOvn)
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				np := NewNetworkPolicy(networkPolicy)
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, loaded := np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())

				// Reconcile through the shared-controller integration path. Network
				// relevance must not be gated by local LSP eligibility: the remote pod
				// still has policy state to remove.
				remotePod := pod.DeepCopy()
				remotePod.Spec.NodeName = "remote-node"
				gomega.Expect(fakeOvn.controller.isPodScheduledOnLocalNode(remotePod)).To(gomega.BeFalse())
				expected, err := fakeOvn.controller.PodExpectedOnNetwork(remotePod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(expected).To(gomega.BeTrue())
				state, err := fakeOvn.controller.ReconcilePod(pod, remotePod, fakeOvn.controller.GetPodState(pod))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(state).To(gomega.BeNil())
				stalePortInfo, err := fakeOvn.controller.logicalPortCache.get(remotePod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(stalePortInfo.expires.IsZero()).To(gomega.BeFalse())
				_, loaded = np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeFalse())

				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(networkPolicy))...)
				policyPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Model node-zone cleanup removing the old local LSP. Returning to the
				// local zone must not trust the retired cache snapshot and must recreate
				// both the LSP and its policy membership.
				gomega.Expect(libovsdbops.DeleteLogicalSwitchPorts(fakeOvn.nbClient,
					&nbdb.LogicalSwitch{Name: nPodTest.nodeName},
					&nbdb.LogicalSwitchPort{Name: nPodTest.portName})).To(gomega.Succeed())
				state, err = fakeOvn.controller.ReconcilePod(remotePod, pod, state)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				localPortInfo, ok := state.(*lpInfo)
				gomega.Expect(ok).To(gomega.BeTrue())
				gomega.Expect(localPortInfo.expires.IsZero()).To(gomega.BeTrue())
				recreatedLSP, err := libovsdbops.GetLogicalSwitchPort(fakeOvn.nbClient,
					&nbdb.LogicalSwitchPort{Name: nPodTest.portName})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(localPortInfo.uuid).To(gomega.Equal(recreatedLSP.UUID))
				_, loaded = np.localPods.Load(nPodTest.portName)
				gomega.Expect(loaded).To(gomega.BeTrue())

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("policy bootstrap serializes a concurrent label update behind its bulk snapshot", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []corev1.Namespace{namespace1}, nil,
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				listedPod, err := fakeOvn.controller.watchFactory.GetPod(namespace1.Name, nPodTest.podName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				listedPortInfo, err := fakeOvn.controller.logicalPortCache.get(listedPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				latestPod := listedPod.DeepCopy()
				latestPod.Labels = map[string]string{labelName: "does-not-match"}

				np := NewNetworkPolicy(networkPolicy)
				pgDbIDs := fakeOvn.controller.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
				np.portGroupName = libovsdbutil.GetPortGroupName(pgDbIDs)
				gomega.Expect(fakeOvn.controller.addPolicyToDefaultPortGroups(np, &libovsdbutil.ACLLoggingLevels{})).To(gomega.Succeed())
				gomega.Expect(libovsdbops.CreateOrUpdatePortGroups(fakeOvn.controller.nbClient,
					libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil))).To(gomega.Succeed())
				fakeOvn.controller.networkPolicies.Store(np.getKey(), np)

				fakeOvn.controller.networkPolicies.LockKey(np.getKey())
				npLocked := true
				defer func() {
					if npLocked {
						fakeOvn.controller.networkPolicies.UnlockKey(np.getKey())
					}
				}()
				np.localPodSelector, err = metav1.LabelSelectorAsSelector(&networkPolicy.Spec.PodSelector)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.addNetworkPolicyToNamespaceIndex(np)

				reconcileStarted := make(chan struct{})
				reconcileDone := make(chan error, 1)
				go func() {
					close(reconcileStarted)
					reconcileDone <- fakeOvn.controller.reconcileNetworkPolicyPortGroups(latestPod)
				}()
				<-reconcileStarted
				gomega.Consistently(reconcileDone, 50*time.Millisecond).ShouldNot(gomega.Receive())

				gomega.Expect(fakeOvn.controller.bootstrapLocalPodsForNetworkPolicy(np, []*corev1.Pod{listedPod})).To(gomega.Succeed())
				gomega.Expect(np.getLocalPortsForPod(listedPod)).To(gomega.Equal(map[string]string{
					listedPortInfo.name: listedPortInfo.uuid,
				}))

				fakeOvn.controller.networkPolicies.UnlockKey(np.getKey())
				npLocked = false
				gomega.Eventually(reconcileDone).Should(gomega.Receive(gomega.BeNil()))
				gomega.Expect(np.getLocalPortsForPod(latestPod)).To(gomega.BeEmpty())

				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(networkPolicy))...)
				policyPG := libovsdbutil.BuildPortGroup(pgDbIDs, nil, nil)
				policyPG.UUID = policyPG.Name + "-UUID"
				expectedData = append(expectedData, policyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.DescribeTable("retries policy bootstrap when a selected pod has no logical port", func(completed bool) {
			app.Action = func(*cli.Context) error {
				namespace := *ovntest.NewNamespace(namespaceName1)
				tPod := getTestPod(namespace.Name, nodeName)
				pod := ovntest.NewPod(tPod.namespace, tPod.podName, nodeName, tPod.podIP)
				pod.Labels = map[string]string{labelName: labelVal}
				setPodAnnotations(pod, tPod)
				readyTPod := getTestPodAdvanced(namespace.Name, nodeName, "10.128.1.4", false)
				readyPod := ovntest.NewPod(readyTPod.namespace, readyTPod.podName, nodeName, readyTPod.podIP)
				readyPod.Labels = pod.Labels
				setPodAnnotations(readyPod, readyTPod)
				testNode := newNode(nodeName, "192.168.126.202/24")
				testNode.Annotations["k8s.ovn.org/node-subnets"] = fmt.Sprintf("{\"default\":\"%s\"}", node1Subnet)
				fakeOvn.startWithDBSetup(initialDB, &namespace, testNode, pod, readyPod)
				tPod.populateLogicalSwitchCache(fakeOvn)
				gomega.Expect(fakeOvn.controller.WatchNamespaces()).To(gomega.Succeed())
				_, err := fakeOvn.controller.ReconcilePod(nil, readyPod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				readyPortInfo, err := fakeOvn.controller.logicalPortCache.get(readyPod, types.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.controller.WatchNetworkPolicy()).To(gomega.Succeed())

				policy := getPortNetworkPolicy(netPolicyName1, namespace.Name, labelName, labelVal, portNum)
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(namespace.Name).
					Create(context.Background(), policy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				key := getPolicyKey(policy)
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)
				gomega.Expect(fakeOvn.controller.getNetworkPolicyKeysForNamespace(namespace.Name)).To(gomega.BeEmpty())

				expectedPorts := []string{readyPortInfo.uuid}
				if completed {
					pod.Status.Phase = corev1.PodSucceeded
					_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(namespace.Name).
						UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Eventually(func() bool {
						latest, err := fakeOvn.controller.watchFactory.GetPod(pod.Namespace, pod.Name)
						return err == nil && util.PodCompleted(latest)
					}).Should(gomega.BeTrue())
				} else {
					_, err = fakeOvn.controller.ReconcilePod(nil, pod, nil)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					portInfo, err := fakeOvn.controller.logicalPortCache.get(pod, types.DefaultNetworkName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					expectedPorts = append(expectedPorts, portInfo.uuid)
				}
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryNetworkPolicies)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				pg, err := libovsdbops.GetPortGroup(fakeOvn.nbClient, &nbdb.PortGroup{
					Name: fakeOvn.controller.getNetworkPolicyPGName(policy.Namespace, policy.Name),
				})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(pg.Ports).To(gomega.ConsistOf(expectedPorts))
				if completed {
					_, err := fakeOvn.controller.logicalPortCache.get(pod, types.DefaultNetworkName)
					gomega.Expect(err).To(gomega.HaveOccurred())
				}
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		},
			ginkgo.Entry("the port becomes available", false),
			ginkgo.Entry("the pod completes before retry", true),
		)

		ginkgo.It("correctly retries creating a network policy allowing a port to a local pod", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						withTCPPeerPorts(portNum),
					getUpdatedInitialDB([]testPod{nPodTest}))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)

				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryNetworkPolicies)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				gressPolicy2ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy2).
					withLocalPortUUIDs(nPodTest.portUUID).
					withTCPPeerPorts(portNum + 1))
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedDataWithPolicy2...))
				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly retries recreating a network policy with the same name", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						withTCPPeerPorts(portNum),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Bringing down NBDB")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				ginkgo.By("Delete the first network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)
				// create second networkpolicy with the same name, but different tcp port
				networkPolicy2 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum+1)
				// check retry entry for this policy
				key, err := retry.GetResourceKey(networkPolicy2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ginkgo.By("retry entry: old obj should not be nil, new obj should be nil")
				retry.CheckRetryObjectMultipleFieldsEventually(
					key,
					fakeOvn.controller.retryNetworkPolicies,
					gomega.Not(gomega.BeNil()), // oldObj should not be nil
					gomega.BeNil(),             // newObj should be nil
				)
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				ginkgo.By("Create a new network policy with same name")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy2).
						withLocalPortUUIDs(nPodTest.portUUID).
						withTCPPeerPorts(portNum+1),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				// check the cache no longer has the entry
				key, err = retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy with a local running pod", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)
				// create networkPolicy, check db
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete peer namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// acls will be deleted, since no namespaces are selected at this point.
				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				updatedGressPolicyExpectedData := getPolicyData(newNetpolDataParams(networkPolicy).withLocalPortUUIDs(nPodTest.portUUID))
				expectedData = append(expectedData, updatedGressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := initialDB.NBData

				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// acls will be deleted, since no namespaces are selected at this point.
				expectedData = initialDB.NBData
				updatedGressPolicyExpectedData := getPolicyData(newNetpolDataParams(networkPolicy))
				expectedData = append(expectedData, updatedGressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).withPeerIPs(nPodTest.podIP),
					getUpdatedInitialDB([]testPod{nPodTest}))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in another namespace", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerIPs(nPodTest.podIP),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{}))
				// After deleting the pod all address sets should be empty
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an updated namespace label", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace2 := *ovntest.NewNamespace(namespaceName2)
				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerIPs(nPodTest.podIP),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Update namespace labels
				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// After updating the namespace all address sets should be empty
				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{nPodTest}))
				// db data should reflect the empty addressets
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted networkpolicy", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).withPeerIPs(nPodTest.podIP)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("retries a deleted network policy", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeTrue())
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).withPeerIPs(nPodTest.podIP)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing NP Add to fail
				time.Sleep(config.Default.OVSDBTxnTimeout + time.Second)

				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)
				connCtx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryNetworkPolicies)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				gomega.Eventually(fakeOvn.controller.nbClient).Should(libovsdbtest.HaveData(expectedData))

				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a policy isolating for ingress and processing egress rules", func() {
			// Even though a policy might isolate for ingress only, we need to
			// process the egress rules in case an additional policy isolating
			// for egress is added in the future.
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := corev1.Protocol(corev1.ProtocolTCP)
				networkPolicy := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					nil,
					[]knet.NetworkPolicyEgressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					knet.PolicyTypeIngress,
				)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that isolates a pod for ingress with egress rules")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).
					withTCPPeerPorts(portNum)
				gressPolicy1ExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a policy isolating for egress and processing ingress rules", func() {
			// Even though a policy might isolate for egress only, we need to
			// process the ingress rules in case an additional policy isolating
			// for ingress is added in the future.
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := corev1.Protocol(corev1.ProtocolTCP)
				networkPolicy := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{
						MatchLabels: map[string]string{
							labelName: labelVal,
						},
					},
					[]knet.NetworkPolicyIngressRule{{
						Ports: []knet.NetworkPolicyPort{{
							Port:     &intstr.IntOrString{IntVal: portNum},
							Protocol: &tcpProtocol,
						}},
					}},
					nil,
					knet.PolicyTypeEgress,
				)
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that isolates a pod for egress with ingress rules")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).
					withTCPPeerPorts(portNum)
				gressPolicy1ExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("can reconcile network policy with long name", func() {
			app.Action = func(*cli.Context) error {
				// this problem can be reproduced by starting ovn with existing db rows for network policy
				// from namespace with long name, but WatchNetworkPolicy doesn't return error on initial netpol add,
				// it just puts network policy to retry loop.
				// To check the error message directly, we can explicitly add network policy, then
				// delete NetworkPolicy's resources to pretend controller doesn't know about it.
				// Then on the next addNetworkPolicy call the result should be the same as on restart.
				// Before ACLs were updated to have new DbIDs, defaultDeny acls (arp and default deny)
				// were equivalent, since their names were cropped and only contained namespace name,
				// and externalIDs only had defaultDenyPolicyTypeACLExtIdKey: Egress/Ingress.
				// Now ExternalIDs will always be different, and ACLs won't be equivalent.
				longNameSpaceName := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
				longNamespace := *ovntest.NewNamespace(longNameSpaceName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, longNamespace.Name, labelName, labelVal, portNum)

				startOvn(initialDB, []corev1.Namespace{longNamespace}, []knet.NetworkPolicy{*networkPolicy1},
					nil, map[string]string{labelName: labelVal})

				ginkgo.By("Simulate the initial re-add of all network policies during upgrade and ensure we are stable")
				// pretend controller didn't see this netpol object, all related db rows are still present
				fakeOvn.controller.networkPolicies.Delete(getPolicyKey(networkPolicy1))
				fakeOvn.controller.sharedNetpolPortGroups.Delete(networkPolicy1.Namespace)
				err := fakeOvn.controller.addNetworkPolicy(networkPolicy1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("can handle network policies with equivalent rules", func() {
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				startOvn(initialDB, []corev1.Namespace{namespace1}, nil, nil, nil)

				peer := knet.NetworkPolicyPeer{
					IPBlock: &knet.IPBlock{
						CIDR: "1.1.1.1",
					},
				}
				// equivalent rules in one peer
				networkPolicy1 := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer, peer},
					}}, nil)
				// equivalent rules in different peers
				networkPolicy2 := ovntest.NewTestNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
					}, nil)

				// create first netpol
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Create(context.TODO(), networkPolicy1, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check db
				expectedData := getNamespaceWithMultiplePoliciesExpectedData(
					[]*knet.NetworkPolicy{networkPolicy1}, initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))
				// create second netpol
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check db
				expectedData = getNamespaceWithMultiplePoliciesExpectedData(
					[]*knet.NetworkPolicy{networkPolicy1, networkPolicy2}, initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("references all namespaces pod IP for empty namespace selector, (including HostNetworkNamespace)", func() {
			app.Action = func(*cli.Context) error {
				config.Kubernetes.HostNetworkNamespace = "ovn-host-network"
				hostNamespace := *ovntest.NewNamespace(config.Kubernetes.HostNetworkNamespace)

				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := ovntest.NewTestNetworkPolicy(netPolicyName1, namespaceName1, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{},
						}},
					},
				}, nil)
				startOvn(initialDB, []corev1.Namespace{namespace1, hostNamespace}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				// create networkPolicy, check db
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						// this is added via HostNetworkNamespace
						withPeerIPs(nPodTest.podIP, node1MgmtIP),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("indexes network policies by namespace for pod-driven membership reconciliation", func() {
			startOvn(initialDB, nil, nil, nil, nil)

			bnc := &fakeOvn.controller.BaseNetworkController

			namespacePolicy := &networkPolicy{name: "match", namespace: namespaceName1}
			otherNamespacePolicy := &networkPolicy{name: "other", namespace: namespaceName2}
			bnc.networkPolicies.Store(namespacePolicy.getKey(), namespacePolicy)
			bnc.networkPolicies.Store(otherNamespacePolicy.getKey(), otherNamespacePolicy)
			bnc.addNetworkPolicyToNamespaceIndex(namespacePolicy)
			bnc.addNetworkPolicyToNamespaceIndex(otherNamespacePolicy)

			gomega.Expect(bnc.getNetworkPolicyKeysForNamespace(namespaceName1)).To(gomega.ConsistOf(namespacePolicy.getKey()))
			gomega.Expect(bnc.getNetworkPolicyKeysForNamespace(namespaceName2)).To(gomega.ConsistOf(otherNamespacePolicy.getKey()))

			bnc.deleteNetworkPolicyFromNamespaceIndex(namespacePolicy)
			gomega.Expect(bnc.getNetworkPolicyKeysForNamespace(namespaceName1)).To(gomega.BeEmpty())
			gomega.Expect(bnc.getNetworkPolicyKeysForNamespace(namespaceName2)).To(gomega.ConsistOf(otherNamespacePolicy.getKey()))
		})

		ginkgo.It("correctly creates networkpolicy targeting hostNetwork pods with non-nil podSelector", func() {
			// check useNamespaceAddrSet function comments to explain this behaviour
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace1.Labels = map[string]string{labelName: labelVal}
				nPodTest := getTestPodAdvanced(namespace1.Name, nodeName, "10.128.1.3", true)

				networkPolicy := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{},
					nil,
					[]knet.NetworkPolicyEgressRule{{
						To: []knet.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{labelName: labelVal},
							},
							PodSelector: &metav1.LabelSelector{},
						}},
					}},
					knet.PolicyTypeEgress,
				)

				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				ginkgo.By("Check networkPolicy includes hostNetwork")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// namespaced address set won't have hostNet pod ip
				// netpol peer address set will

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerIPs(nPodTest.podIP),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly creates networkpolicy ignoring hostNetwork pods with nil podSelector", func() {
			// check useNamespaceAddrSet function comments to explain this behaviour
			app.Action = func(*cli.Context) error {
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				namespace1.Labels = map[string]string{labelName: labelVal}
				nPodTest := getTestPodAdvanced(namespace1.Name, nodeName, "10.128.1.3", true)

				networkPolicy := ovntest.NewTestNetworkPolicy(netPolicyName1, namespace1.Name,
					metav1.LabelSelector{},
					nil,
					[]knet.NetworkPolicyEgressRule{{
						To: []knet.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{labelName: labelVal},
							},
						}},
					}},
					knet.PolicyTypeEgress,
				)

				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				ginkgo.By("Check networkPolicy doesn't select hostNetwork pods")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly creates, updates and deletes network policies", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableStatelessNetPol = true
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				startOvn(initialDB, []corev1.Namespace{namespace1}, nil, nil, nil)

				ginkgo.By("Creating network policy")
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withTCPPeerPorts(portNum),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy ingress and egress rules")
				networkPolicy.Spec.Ingress = []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{labelName: labelVal},
						},
					}},
				}}
				networkPolicy.Spec.Egress = []knet.NetworkPolicyEgressRule{{
					To: []knet.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{labelName: labelVal},
						},
					}},
				}}

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy labels")
				networkPolicy.Labels = map[string]string{labelName: labelVal}
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy unrelated annotation")
				networkPolicy.Annotations = map[string]string{"test-annotation": "test-value"}
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Consistently(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy stateless OVN ACLs annotation to true")
				networkPolicy.Annotations = map[string]string{ovnStatelessNetPolAnnotationName: "true"}
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withStateless(true),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy with IPBlock when stateless OVN ACLs annotation is set to true")
				networkPolicy.Spec.Ingress = []knet.NetworkPolicyIngressRule{{
					From: []knet.NetworkPolicyPeer{{
						IPBlock: &knet.IPBlock{
							CIDR: "192.168.1.0/24",
						},
					}},
				}}
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withStateless(true),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Updating network policy stateless OVN ACLs annotation to false")
				networkPolicy.Annotations = map[string]string{ovnStatelessNetPolAnnotationName: "false"}
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Update(context.TODO(), networkPolicy, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withStateless(false),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				ginkgo.By("Deleting network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = initialDB.NBData
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})

	ginkgo.Context("ACL logging for network policies", func() {

		var originalNamespace corev1.Namespace

		updateNamespaceACLLogSeverity := func(namespaceToUpdate *corev1.Namespace, desiredDenyLogLevel string, desiredAllowLogLevel string) error {
			ginkgo.By("updating the namespace's ACL logging severity")
			updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
			namespaceToUpdate.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity

			_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
			return err
		}

		ginkgo.BeforeEach(func() {
			originalACLLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice)
			originalNamespace = *ovntest.NewNamespace(namespaceName1)
			originalNamespace.Annotations = map[string]string{util.AclLoggingAnnotation: originalACLLogSeverity}
		})

		ginkgo.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...*knet.NetworkPolicy) {
			ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
			initialDenyAllPolicy := ovntest.NewTestNetworkPolicy("emptyPol", namespaceName1, metav1.LabelSelector{}, nil, nil)
			// originalACLLogSeverity.Deny == nbdb.ACLSeverityAlert
			initialExpectedData := getDefaultDenyData(newNetpolDataParams(initialDenyAllPolicy).
				withDenyLogSeverity(nbdb.ACLSeverityAlert))
			initialExpectedData = append(initialExpectedData,
				// no gress policies defined, return only port group
				getPolicyData(newNetpolDataParams(initialDenyAllPolicy).
					withAllowLogSeverity(nbdb.ACLSeverityNotice))...)
			initialExpectedData = append(initialExpectedData, initialDB.NBData...)

			app.Action = func(*cli.Context) error {
				startOvn(initialDB, []corev1.Namespace{originalNamespace}, []knet.NetworkPolicy{*initialDenyAllPolicy},
					nil, nil)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(initialExpectedData...))

				var createsPoliciesData []libovsdbtest.TestData
				// create network policies for given Entry
				for i := range networkPolicies {
					ginkgo.By("Creating new network policy")
					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicies[i].GetNamespace()).
						Create(context.TODO(), networkPolicies[i], metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())

					createsPoliciesData = append(createsPoliciesData,
						// originalACLLogSeverity.Allow == nbdb.ACLSeverityNotice
						getPolicyData(newNetpolDataParams(networkPolicies[i]).
							withAllowLogSeverity(nbdb.ACLSeverityNotice))...)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(append(initialExpectedData, createsPoliciesData...)))

				// update namespace log severity
				updatedLogSeverity := nbdb.ACLSeverityDebug
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, updatedLogSeverity, updatedLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				// update expected data log severity
				expectedData := getDefaultDenyData(newNetpolDataParams(initialDenyAllPolicy).
					withDenyLogSeverity(updatedLogSeverity))
				expectedData = append(expectedData,
					// no gress policies defined, return only port group
					getPolicyData(newNetpolDataParams(initialDenyAllPolicy).
						withAllowLogSeverity(updatedLogSeverity))...)
				for i := range networkPolicies {
					expectedData = append(expectedData,
						getPolicyData(newNetpolDataParams(networkPolicies[i]).
							withAllowLogSeverity(updatedLogSeverity))...)
				}
				expectedData = append(expectedData, initialDB.NBData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		},
			ginkgo.Entry("when the namespace features a network policy with a single rule",
				ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)),
			ginkgo.Entry("when the namespace features *multiple* network policies with a single rule",
				ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false),
				ovntest.NewMatchLabelsNetworkPolicy(netPolicyName2, namespaceName1, namespaceName1, "", false, true)),
			ginkgo.Entry("when the namespace features a network policy with *multiple* rules",
				ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "tiny-winy-pod", true, false)))

		ginkgo.It("policies created after namespace logging level updates inherit updated logging level", func() {
			app.Action = func(*cli.Context) error {
				startOvn(initialDB, []corev1.Namespace{originalNamespace}, nil, nil, nil)
				desiredLogSeverity := nbdb.ACLSeverityDebug
				// update namespace log severity
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, desiredLogSeverity, desiredLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				newPolicy := ovntest.NewMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)
				ginkgo.By("Creating new network policy")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(namespaceName1).
					Create(context.TODO(), newPolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should have managed to create a new network policy")

				expectedData := initialDB.NBData
				expectedData = append(expectedData, getPolicyData(newNetpolDataParams(newPolicy).
					withAllowLogSeverity(desiredLogSeverity))...)
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(newPolicy).
					withDenyLogSeverity(desiredLogSeverity))...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("creates stateless OVN ACLs based off of the annotation", func() {
			app.Action = func(*cli.Context) error {
				config.OVNKubernetesFeature.EnableStatelessNetPol = true
				namespace1 := *ovntest.NewNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				networkPolicy.Annotations = map[string]string{
					ovnStatelessNetPolAnnotationName: "true",
				}
				startOvn(initialDB, []corev1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						withTCPPeerPorts(portNum).
						withStateless(true),
					getUpdatedInitialDB([]testPod{nPodTest}))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

	})
})

func asMatch(hashedAddressSets []string) string {
	sort.Strings(hashedAddressSets)
	var match string
	for i, n := range hashedAddressSets {
		if i > 0 {
			match += ", "
		}
		match += fmt.Sprintf("$%s", n)
	}
	return match
}

func getAllowFromNodeExpectedACL(nodeName, mgmtIP string, logicalSwitch *nbdb.LogicalSwitch, controllerName string) *nbdb.ACL {
	var ipFamily = "ip4"
	if utilnet.IsIPv6(ovntest.MustParseIP(mgmtIP)) {
		ipFamily = "ip6"
	}
	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIP)

	dbIDs := getAllowFromNodeACLDbIDs(nodeName, mgmtIP, controllerName)
	nodeACL := libovsdbops.BuildACL(
		libovsdbutil.GetACLName(dbIDs),
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		match,
		nbdb.ACLActionAllowRelated,
		types.OvnACLLoggingMeter,
		"",
		false,
		dbIDs.GetExternalIDs(),
		nil,
		types.DefaultACLTier)
	nodeACL.UUID = dbIDs.String() + "-UUID"
	if logicalSwitch != nil {
		logicalSwitch.ACLs = []string{nodeACL.UUID}
	}
	return nodeACL
}

func getAllowFromNodeStaleACL(nodeName, mgmtIP string, logicalSwitch *nbdb.LogicalSwitch, controllerName string) *nbdb.ACL {
	acl := getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName)
	newName := ""
	acl.Name = &newName
	// re-setting the tier to 0 to test that the stale ACL gets updated to 2 eventually
	acl.Tier = types.PrimaryACLTier
	return acl
}

// here only low-level operation are tested (directly calling updateStaleNetpolNodeACLs)
var _ = ginkgo.Describe("OVN AllowFromNode ACL low-level operations", func() {
	var (
		nbCleanup     *libovsdbtest.Context
		logicalSwitch *nbdb.LogicalSwitch
	)

	const (
		nodeName       = "node1"
		ipv4MgmtIP     = "192.168.10.10"
		ipv6MgmtIP     = "fd01::1234"
		controllerName = types.DefaultNetworkControllerName
	)

	getFakeController := func(nbClient libovsdbclient.Client) *DefaultNetworkController {
		controller := getFakeController(types.DefaultNetworkControllerName)
		controller.nbClient = nbClient
		return controller
	}

	ginkgo.BeforeEach(func() {
		nbCleanup = nil
		logicalSwitch = &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
	})

	ginkgo.AfterEach(func() {
		if nbCleanup != nil {
			nbCleanup.Cleanup()
		}
	})

	for _, ipMode := range []string{"ipv4", "ipv6"} {
		var mgmtIP string
		if ipMode == "ipv4" {
			mgmtIP = ipv4MgmtIP
		} else {
			mgmtIP = ipv6MgmtIP
		}
		ginkgo.It(fmt.Sprintf("sync existing ACLs on startup, %s mode", ipMode), func() {
			// mock existing management port
			mgmtPortMAC := "0a:58:0a:01:01:02"
			mgmtPort := &nbdb.LogicalSwitchPort{
				Name:      types.K8sPrefix + nodeName,
				UUID:      types.K8sPrefix + nodeName + "-UUID",
				Type:      "",
				Options:   nil,
				Addresses: []string{mgmtPortMAC + " " + mgmtIP},
			}
			logicalSwitch.Ports = []string{mgmtPort.UUID}
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					getAllowFromNodeStaleACL(nodeName, mgmtIP, logicalSwitch, controllerName),
					logicalSwitch,
					mgmtPort,
				},
			}
			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, net.ParseIP(mgmtIP))

			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdbtest.TestData{
				logicalSwitch,
				mgmtPort,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName), // checks if tier get's updated from 0 to 2
			}
			gomega.Expect(nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})
		ginkgo.It(fmt.Sprintf("adding an existing ACL to the node switch, %s mode", ipMode), func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
					getAllowFromNodeExpectedACL(nodeName, mgmtIP, nil, controllerName),
				},
			}
			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, ovntest.MustParseIP(mgmtIP))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdbtest.TestData{
				logicalSwitch,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName),
			}
			gomega.Expect(nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})

		ginkgo.It(fmt.Sprintf("creating new ACL and adding it to node switch, %s mode", ipMode), func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					logicalSwitch,
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			fakeController := getFakeController(nbClient)
			err = fakeController.addAllowACLFromNode(nodeName, ovntest.MustParseIP(mgmtIP))
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			expectedData := []libovsdbtest.TestData{
				logicalSwitch,
				getAllowFromNodeExpectedACL(nodeName, mgmtIP, logicalSwitch, controllerName),
			}
			gomega.Expect(nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})
	}
})

var _ = ginkgo.Describe("OVN NetworkPolicy Low-Level Operations", func() {
	var asFactory *addressset.FakeAddressSetFactory

	buildExpectedIngressPeerNSv4ACL := func(gp *gressPolicy, pgName string, asDbIDses []*libovsdbops.DbObjectIDs,
		aclLogging *libovsdbutil.ACLLoggingLevels) *nbdb.ACL {
		aclIDs := gp.getNetpolACLDbIDs(emptyIdx, libovsdbutil.UnspecifiedL4Protocol)
		hashedASNames := []string{}

		for _, dbIdx := range asDbIDses {
			hashedASName, _ := addressset.GetHashNamesForAS(dbIdx)
			hashedASNames = append(hashedASNames, hashedASName)
		}
		asMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.src == {%s} && outport == @%s", asMatch, pgName)
		acl := libovsdbops.BuildACL(libovsdbutil.GetACLName(aclIDs), nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match,
			nbdb.ACLActionAllowRelated, types.OvnACLLoggingMeter, aclLogging.Allow, true, aclIDs.GetExternalIDs(), nil,
			types.DefaultACLTier)
		return acl
	}

	getNsPeerDBIDs := func(nsName, controllerName string) *libovsdbops.DbObjectIDs {
		return addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, nsName, controllerName, false)
	}

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName         string = "pg-name"
			controllerName        = types.DefaultNetworkControllerName
		)
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		asFactory = addressset.NewFakeAddressSetFactory(controllerName)
		config.IPv4Mode = true
		config.IPv6Mode = false
		asIDs := addresssetmanager.GetPodSelectorAddrSetDbIDs(&metav1.LabelSelector{}, nil, nil, "nsName", types.DefaultNetworkControllerName, false)
		gp := newGressPolicy(knet.PolicyTypeIngress, 0, "testing", "policy", controllerName,
			false, &util.DefaultNetInfo{})
		gp.hasPeerSelector = true
		gp.addPeerAddressSets(addressset.GetHashNamesForAS(asIDs))

		one := getNsPeerDBIDs("ns1", controllerName)
		two := getNsPeerDBIDs("ns2", controllerName)
		three := getNsPeerDBIDs("ns3", controllerName)
		four := getNsPeerDBIDs("ns4", controllerName)
		five := getNsPeerDBIDs("ns5", controllerName)
		six := getNsPeerDBIDs("ns6", controllerName)
		for _, addrSetID := range []*libovsdbops.DbObjectIDs{one, two, three, four, five, six} {
			_, err := asFactory.EnsureAddressSet(addrSetID)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		defaultAclLogging := libovsdbutil.ACLLoggingLevels{
			Allow: nbdb.ACLSeverityInfo,
			Deny:  nbdb.ACLSeverityInfo,
		}

		gp.addPeerAddressSets(addressset.GetHashNamesForAS(one))
		expected := buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one}, &defaultAclLogging)
		actual, _ := gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gp.addPeerAddressSets(addressset.GetHashNamesForAS(two))
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gp.addPeerAddressSets(addressset.GetHashNamesForAS(three))
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gp.addPeerAddressSets(addressset.GetHashNamesForAS(three))
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gp.addPeerAddressSets(addressset.GetHashNamesForAS(four))
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))
	})
})
