package ovn

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sort"
	"time"

	"github.com/onsi/ginkgo/v2"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/utils/net"
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

func newNetworkPolicy(name, namespace string, podSelector metav1.LabelSelector, ingress []knet.NetworkPolicyIngressRule,
	egress []knet.NetworkPolicyEgressRule, policyTypes ...knet.PolicyType) *knet.NetworkPolicy {
	policy := &knet.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			UID:       apimachinerytypes.UID(namespace),
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"name": name,
			},
		},
		Spec: knet.NetworkPolicySpec{
			PodSelector: podSelector,
			PolicyTypes: policyTypes,
			Ingress:     ingress,
			Egress:      egress,
		},
	}
	if policyTypes == nil {
		if len(ingress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, knet.PolicyTypeIngress)
		}
		if len(egress) > 0 {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, knet.PolicyTypeEgress)
		}
	}
	return policy
}

func getFakeBaseController(netInfo util.NetInfo) *BaseNetworkController {
	return &BaseNetworkController{
		controllerName:      getNetworkControllerName(netInfo.GetNetworkName()),
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
	}
}

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
	egressAllowACL := libovsdbops.BuildACL(
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
	egressAllowACL.UUID = aclIDs.String() + "-UUID"

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
	ingressAllowACL := libovsdbops.BuildACL(
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
	ingressAllowACL.UUID = aclIDs.String() + "-UUID"

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
		[]*nbdb.ACL{egressDenyACL, egressAllowACL},
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	var ingressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeIngress {
		ingressDenyPorts = lsps
	}
	ingressDenyPG := libovsdbutil.BuildPortGroup(
		fakeController.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLIngress),
		ingressDenyPorts,
		[]*nbdb.ACL{ingressDenyACL, ingressAllowACL},
	)
	ingressDenyPG.UUID = ingressDenyPG.Name + "-UUID"

	return []libovsdbtest.TestData{
		egressDenyACL,
		egressAllowACL,
		ingressDenyACL,
		ingressAllowACL,
		egressDenyPG,
		ingressDenyPG,
	}
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

func getMultinetNsAddrSetHashNames(ns, controllerName string) (string, string) {
	return addressset.GetHashNamesForAS(getNamespaceAddrSetDbIDs(ns, controllerName))
}

// getGressACLs can only handle tcpPeerPorts for policies built with getPortNetworkPolicy, i.e. peer should only
// have `Ports` filled, but no `To`/`From`.
func getGressACLs(gressIdx int, peers []knet.NetworkPolicyPeer, policyType knet.PolicyType,
	params *netpolDataParams) []*nbdb.ACL {
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
	for _, nsName := range params.peerNamespaces {
		hashedASName, _ := getMultinetNsAddrSetHashNames(nsName, controllerName)
		hashedASNames = append(hashedASNames, hashedASName)
	}
	ipBlocks := []string{}
	for _, peer := range peers {
		// follow the algorithm from setupGressPolicy
		if (peer.NamespaceSelector != nil || peer.PodSelector != nil) && !useNamespaceAddrSet(peer) {
			podSelector := peer.PodSelector
			if podSelector == nil {
				// nil pod selector is equivalent to empty pod selector, which selects all
				podSelector = &metav1.LabelSelector{}
			}
			peerIndex := getPodSelectorAddrSetDbIDs(getPodSelectorKey(podSelector, peer.NamespaceSelector, namespace), controllerName)
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
		action := nbdb.ACLActionAllowRelated
		if params.statelessNetPol {
			action = nbdb.ACLActionAllowStateless
		}
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
	for i, ipBlock := range ipBlocks {
		match := fmt.Sprintf("ip4.%s == %s && %s == @%s", ipDir, ipBlock, portDir, pgName)
		dbIDs := gp.getNetpolACLDbIDs(i, libovsdbutil.UnspecifiedL4Protocol)
		acl := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			match,
			nbdb.ACLActionAllowRelated,
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
		acl := libovsdbops.BuildACL(
			libovsdbutil.GetACLName(dbIDs),
			direction,
			types.DefaultAllowPriority,
			fmt.Sprintf("ip4 && tcp && tcp.dst==%d && %s == @%s", v, portDir, pgName),
			nbdb.ACLActionAllowRelated,
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
	return acls
}

type netpolDataParams struct {
	networkPolicy    *knet.NetworkPolicy
	localPortUUIDs   []string
	peerNamespaces   []string
	tcpPeerPorts     []int32
	allowLogSeverity nbdb.ACLSeverity
	denyLogSeverity  nbdb.ACLSeverity
	statelessNetPol  bool
	netInfo          util.NetInfo
}

func getPolicyData(params *netpolDataParams) []libovsdbtest.TestData {
	acls := []*nbdb.ACL{}

	for i, ingress := range params.networkPolicy.Spec.Ingress {
		acls = append(acls, getGressACLs(i, ingress.From, knet.PolicyTypeIngress, params)...)
	}
	for i, egress := range params.networkPolicy.Spec.Egress {
		acls = append(acls, getGressACLs(i, egress.To, knet.PolicyTypeEgress, params)...)
	}

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

	data := []libovsdbtest.TestData{}
	for _, acl := range acls {
		data = append(data, acl)
	}
	data = append(data, pg)
	return data
}

func newNetpolDataParams(networkPolicy *knet.NetworkPolicy) *netpolDataParams {
	return &netpolDataParams{
		networkPolicy:    networkPolicy,
		localPortUUIDs:   nil,
		peerNamespaces:   nil,
		tcpPeerPorts:     nil,
		allowLogSeverity: "",
		denyLogSeverity:  "",
		statelessNetPol:  false,
		netInfo:          &util.DefaultNetInfo{},
	}
}

func (p *netpolDataParams) withPeerNamespaces(peerNamespaces ...string) *netpolDataParams {
	p.peerNamespaces = peerNamespaces
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

func getMatchLabelsNetworkPolicy(policyName, netpolNamespace, peerNamespace, peerPodName string, ingress, egress bool) *knet.NetworkPolicy {
	netPolPeer := knet.NetworkPolicyPeer{}
	if peerPodName != "" {
		netPolPeer.PodSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerPodName,
			},
		}
	}
	if peerNamespace != "" {
		netPolPeer.NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"name": peerNamespace,
			},
		}
	}
	var ingressRules []knet.NetworkPolicyIngressRule
	if ingress {
		ingressRules = []knet.NetworkPolicyIngressRule{
			{
				From: []knet.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	var egressRules []knet.NetworkPolicyEgressRule
	if egress {
		egressRules = []knet.NetworkPolicyEgressRule{
			{
				To: []knet.NetworkPolicyPeer{netPolPeer},
			},
		}
	}
	return newNetworkPolicy(policyName, netpolNamespace, metav1.LabelSelector{}, ingressRules, egressRules)
}

func getPortNetworkPolicy(policyName, namespace, labelName, labelVal string, tcpPort int32) *knet.NetworkPolicy {
	tcpProtocol := v1.ProtocolTCP
	return newNetworkPolicy(policyName, namespace,
		metav1.LabelSelector{
			MatchLabels: map[string]string{
				labelName: labelVal,
			},
		},
		[]knet.NetworkPolicyIngressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
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

// buildNetworkPolicyPeerAddressSet builds the addresssets for the networkpolicy peer provided
func buildNetworkPolicyPeerAddressSets(namespaceName string, peer knet.NetworkPolicyPeer, ips ...string) (*nbdb.AddressSet, *nbdb.AddressSet) {
	asName := getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespaceName)
	dbIDs := getPodSelectorAddrSetDbIDs(asName, DefaultNetworkControllerName)
	return addressset.GetTestDbAddrSets(dbIDs, ips)
}

// buildNetworkPolicyAddressSets builds all the addresssets for all the network policy peers of a network policy
// the limitation is that all the addressSets must be empty
func buildNetworkPolicyAddressSets(networkPolicy *knet.NetworkPolicy) []libovsdb.TestData {
	addressSets := []libovsdb.TestData{}
	for _, egress := range networkPolicy.Spec.Egress {
		for _, peer := range egress.To {
			if peer.PodSelector != nil {
				peerASv4, peerASv6 := buildNetworkPolicyPeerAddressSets(networkPolicy.Namespace, peer)
				if config.IPv4Mode {
					addressSets = append(addressSets, peerASv4)
				}
				if config.IPv6Mode {
					addressSets = append(addressSets, peerASv6)
				}
			}
		}
	}
	for _, ingress := range networkPolicy.Spec.Ingress {
		for _, peer := range ingress.From {
			if peer.PodSelector != nil {
				peerASv4, peerASv6 := buildNetworkPolicyPeerAddressSets(networkPolicy.Namespace, peer)
				if config.IPv4Mode {
					addressSets = append(addressSets, peerASv4)
				}
				if config.IPv6Mode {
					addressSets = append(addressSets, peerASv6)
				}
			}
		}
	}
	return addressSets
}

func getTestPod(namespace, nodeName string) testPod {
	return newTPod(
		nodeName,
		"10.128.1.0/24",
		"10.128.1.2",
		"10.128.1.1",
		"myPod",
		"10.128.1.3",
		"0a:58:0a:80:01:03",
		namespace,
	)
}

var _ = ginkgo.Describe("OVN NetworkPolicy Operations", func() {
	const (
		namespaceName1        = "namespace1"
		namespaceName2        = "namespace2"
		netPolicyName1        = "networkpolicy1"
		netPolicyName2        = "networkpolicy2"
		nodeName              = "node1"
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
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN(false)

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

	startOvnWithHostNetPods := func(dbSetup libovsdbtest.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string, hostNetPods bool) {
		var podsList []v1.Pod
		for _, testPod := range pods {
			knetPod := newPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			if hostNetPods {
				knetPod.Spec.HostNetwork = true
			}
			podsList = append(podsList, *knetPod)
		}
		fakeOvn.startWithDBSetup(dbSetup,
			&v1.NamespaceList{
				Items: namespaces,
			},
			&v1.NodeList{
				Items: []v1.Node{
					*newNode(nodeName, "192.168.126.202/24"),
				},
			},
			&v1.PodList{
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
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	startOvn := func(dbSetup libovsdbtest.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string) {
		startOvnWithHostNetPods(dbSetup, namespaces, networkPolicies, pods, podLabels, false)
	}

	getUpdatedInitialDB := func(tPods []testPod) []libovsdbtest.TestData {
		updatedSwitchAndPods := getDefaultNetExpectedPodsAndSwitches(tPods, []string{nodeName})
		return append(getHairpinningACLsV4AndPortGroup(), updatedSwitchAndPods...)
	}

	ginkgo.Context("on startup", func() {
		ginkgo.It("creates default hairpinning ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
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
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				// network policy with peer selector
				networkPolicy1 := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", "podName", true, true)
				// network policy with ipBlock (won't have any address sets)
				networkPolicy2 := newNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
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
				initialData = append(initialData, policy1Db...)

				policy2Db := getPolicyData(newNetpolDataParams(networkPolicy2))
				initialData = append(initialData, policy2Db...)

				defaultDenyDb := getDefaultDenyData(newNetpolDataParams(networkPolicy1))
				initialData = append(initialData, defaultDenyDb...)

				// start ovn with no objects, expect stale port db entries to be cleaned up
				startOvn(libovsdbtest.TestSetup{NBData: initialData}, nil, nil, nil, nil)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(afterCleanup))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an existing networkPolicy with empty db", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, nil)
				// add namespaces to initial Database
				initialDB.NBData = append(initialDB.NBData, namespace1AddressSetv4, namespace2AddressSetv4)

				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))
				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an ingress networkPolicy updating an existing ACL", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, nil)
				// add namespaces to initial Database
				initialDB.NBData = append(initialDB.NBData, namespace1AddressSetv4, namespace2AddressSetv4)

				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)

				// network policy without peer namespace
				initialData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)

				startOvn(libovsdbtest.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check peer namespace was added
				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name),
					initialDB.NBData)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod selector in its own namespace from empty db", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				// network policy with peer pod selector
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).withLocalPortUUIDs(nPodTest.portUUID),
					getUpdatedInitialDB([]testPod{nPodTest}))
				expectedData = append(expectedData, netpolASv4, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an existing networkPolicy with a pod and namespace selector in another namespace from empty db", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				// network policy with peer pod and namespace selector
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, []string{nPodTest.podIP})
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(networkPolicy.Namespace, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{nPodTest}))
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4, netpolASv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles existing networkPolicies with equivalent rules", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				peer := knet.NetworkPolicyPeer{
					IPBlock: &knet.IPBlock{
						CIDR: "1.1.1.1",
					},
				}
				// equivalent rules in one peer
				networkPolicy1 := newNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer, peer},
					}}, nil)
				// equivalent rules in different peers
				networkPolicy2 := newNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
						{
							From: []knet.NetworkPolicyPeer{peer},
						},
					}, nil)
				initialData := initialDB.NBData
				initialData = append(initialData, namespace1AddressSetv4)
				gressPolicy1ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy1))
				gressPolicy2ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy2))
				defaultDenyExpectedData := getDefaultDenyDataMultiplePolicies([]*knet.NetworkPolicy{networkPolicy1, networkPolicy2})
				initialData = append(initialData, gressPolicy1ExpectedData...)
				initialData = append(initialData, gressPolicy2ExpectedData...)
				initialData = append(initialData, defaultDenyExpectedData...)

				// start with the updated network policy, but previous-version db
				networkPolicy1Updated := newNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer},
					}}, nil)

				startOvn(libovsdbtest.TestSetup{NBData: initialData}, []v1.Namespace{namespace1},
					[]knet.NetworkPolicy{*networkPolicy1Updated, *networkPolicy2},
					nil, nil)

				// check the initial data is updated, one acl should be removed
				gressUpdatedPolicy1ExpectedData := getPolicyData(newNetpolDataParams(networkPolicy1Updated))
				finalData := initialDB.NBData
				finalData = append(finalData, namespace1AddressSetv4)
				finalData = append(finalData, gressUpdatedPolicy1ExpectedData...)
				finalData = append(finalData, gressPolicy2ExpectedData...)
				finalData = append(finalData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(finalData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.DescribeTable("correctly uses namespace and shared peer selector address sets",
			func(peer knet.NetworkPolicyPeer, peerNamespaces []string) {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, nil)

				netpol := newNetworkPolicy("netpolName", namespace1.Name, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{peer},
					},
				}, nil)
				initialDB.NBData = append(initialDB.NBData, namespace1AddressSetv4, namespace2AddressSetv4)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*netpol}, nil, nil)

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(netpol).withPeerNamespaces(peerNamespaces...),
					initialDB.NBData)
				// if peer.PodSelector == nil then the network policy ACL will use the AddressSet of the namespace selected
				if peer.PodSelector != nil {
					netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, peer)
					expectedData = append(expectedData, netpolASv4)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
			},
			ginkgo.Entry("empty pod selector => use pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{},
				}, nil),
			ginkgo.Entry("namespace selector with nil pod selector => use a set of selected namespace address sets",
				knet.NetworkPolicyPeer{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, []string{namespaceName2}),
			ginkgo.Entry("empty namespace and pod selector => use all pods shared address set",
				knet.NetworkPolicyPeer{
					PodSelector:       &metav1.LabelSelector{},
					NamespaceSelector: &metav1.LabelSelector{},
				}, nil),
			ginkgo.Entry("pod selector with nil namespace => use static namespace+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
				}, nil),
			ginkgo.Entry("pod selector with namespace selector => use namespace selector+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, nil),
			ginkgo.Entry("pod selector with empty namespace selector => use global pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
					NamespaceSelector: &metav1.LabelSelector{},
				}, nil),
		)

		ginkgo.It("correctly creates and deletes a networkpolicy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				initialDB.NBData = append(initialDB.NBData, namespace1AddressSetv4)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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
				expectedData = append(expectedData, namespace1AddressSetv4)
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
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly retries creating a network policy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
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
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
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
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
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
				expectedData = append(expectedData, namespace1AddressSetv4)
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
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)
				// create networkPolicy, check db
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID).
					withPeerNamespaces(namespace2.Name)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, nil)
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
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
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted namespace referenced by a networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withPeerNamespaces(namespace2.Name)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := initialDB.NBData

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, nil)
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// acls will be deleted, since no namespaces are selected at this point.
				expectedData = initialDB.NBData
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
				updatedGressPolicyExpectedData := getPolicyData(newNetpolDataParams(networkPolicy))
				expectedData = append(expectedData, updatedGressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID),
					getUpdatedInitialDB([]testPod{nPodTest}))

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				expectedData = append(expectedData, netpolASv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				netpolASv4.Addresses = nil
				namespace1AddressSetv4.Addresses = nil
				expectedData = append(expectedData, netpolASv4, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in another namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)

				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{nPodTest}))
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				expectedData = append(expectedData, netpolASv4)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{}))
				// After deleting the pod all address sets should be empty
				netpolASv4.Addresses = nil
				namespace2AddressSetv4.Addresses = nil
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4, netpolASv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles an updated namespace label", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				nPodTest := getTestPod(namespace2.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					nPodTest.namespace, nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					getUpdatedInitialDB([]testPod{nPodTest}))
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace2.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				expectedData = append(expectedData, netpolASv4)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				namespace2AddressSetv4, _ := buildNamespaceAddressSets(namespace2.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4, namespace2AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Update namespace labels
				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// After updating the namespace all address sets should be empty
				netpolASv4.Addresses = nil
				namespace1AddressSetv4.Addresses = nil

				// db data should reflect the empty addressets
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a deleted networkpolicy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				expectedData = append(expectedData, netpolASv4)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("retries a deleted network policy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeTrue())
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				dataParams := newNetpolDataParams(networkPolicy).
					withLocalPortUUIDs(nPodTest.portUUID)
				gressPolicyExpectedData := getPolicyData(dataParams)
				defaultDenyExpectedData := getDefaultDenyData(dataParams)
				expectedData := getUpdatedInitialDB([]testPod{nPodTest})
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Ingress[0].From[0], nPodTest.podIP)
				expectedData = append(expectedData, netpolASv4)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
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
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, namespace1AddressSetv4)
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
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
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
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("reconciles a policy isolating for egress and processing ingress rules", func() {
			// Even though a policy might isolate for egress only, we need to
			// process the ingress rules in case an additional policy isolating
			// for ingress is added in the future.
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				tcpProtocol := v1.Protocol(v1.ProtocolTCP)
				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
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
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = getUpdatedInitialDB([]testPod{nPodTest})
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("can reconcile network policy with long name", func() {
			app.Action = func(ctx *cli.Context) error {
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
				longNamespace := *newNamespace(longNameSpaceName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, longNamespace.Name, labelName, labelVal, portNum)

				startOvn(initialDB, []v1.Namespace{longNamespace}, []knet.NetworkPolicy{*networkPolicy1},
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
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				startOvn(initialDB, []v1.Namespace{namespace1}, nil, nil, nil)

				peer := knet.NetworkPolicyPeer{
					IPBlock: &knet.IPBlock{
						CIDR: "1.1.1.1",
					},
				}
				// equivalent rules in one peer
				networkPolicy1 := newNetworkPolicy(netPolicyName1, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{peer, peer},
					}}, nil)
				// equivalent rules in different peers
				networkPolicy2 := newNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
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
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))
				// create second netpol
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check db
				expectedData = getNamespaceWithMultiplePoliciesExpectedData(
					[]*knet.NetworkPolicy{networkPolicy1, networkPolicy2}, initialDB.NBData)
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("references all namespace address sets for empty namespace selector, even if they don't have pods (HostNetworkNamespace)", func() {
			app.Action = func(ctx *cli.Context) error {
				hostNamespaceName := "host-network"
				hostNamespace := *newNamespace(hostNamespaceName)

				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := newNetworkPolicy(netPolicyName1, namespaceName1, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{},
						}},
					},
				}, nil)
				startOvn(initialDB, []v1.Namespace{namespace1, hostNamespace}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				// emulate namespace handler adding management IPs for this address set
				// we could let controller do that, but that would require adding nodes with their annotations
				dbIDs := libovsdbops.NewDbObjectIDs(libovsdbops.AddressSetNamespace, fakeOvn.controller.controllerName,
					map[libovsdbops.ExternalIDKey]string{
						libovsdbops.ObjectNameKey: hostNamespaceName,
					})
				// random set of IPs
				as, err := fakeOvn.controller.addressSetFactory.GetAddressSet(dbIDs)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// emulate management IP being added to the hostNetwork address set, but no pods in that namespace
				as.AddAddresses([]string{"10.244.0.2"})

				// create networkPolicy, check db
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withLocalPortUUIDs(nPodTest.portUUID).
						withPeerNamespaces(hostNamespace.Name, namespaceName1),
					getUpdatedInitialDB([]testPod{nPodTest}))
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, []string{nPodTest.podIP})
				hostNamespaceAddressSetv4, _ := buildNamespaceAddressSets(hostNamespaceName, []string{"10.244.0.2"})
				expectedData = append(expectedData, namespace1AddressSetv4, hostNamespaceAddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("cleans up retryFramework resources", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Labels = map[string]string{"name": "label1"}
				networkPolicy := newNetworkPolicy(netPolicyName2, namespace1.Name, metav1.LabelSelector{},
					[]knet.NetworkPolicyIngressRule{{
						From: []knet.NetworkPolicyPeer{{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: namespace1.Labels,
							}},
						}},
					}, nil)
				startOvn(initialDB, []v1.Namespace{namespace1}, nil, nil, nil)

				// let the system settle down before counting goroutines
				time.Sleep(100 * time.Millisecond)
				goroutinesNumInit := runtime.NumGoroutine()
				fmt.Printf("goroutinesNumInit %v", goroutinesNumInit)
				// network policy will create 1 watchFactory for local pods selector, and 1 peer namespace selector
				// that gives us 2 retryFrameworks, so 2 periodicallyRetryResources goroutines.
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() int {
					return runtime.NumGoroutine()
				}).Should(gomega.Equal(goroutinesNumInit + 2))

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// expect goroutines number to get back
				gomega.Eventually(func() int {
					return runtime.NumGoroutine()
				}).Should(gomega.Equal(goroutinesNumInit))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly creates networkpolicy targeting hostNetwork pods with non-nil podSelector", func() {
			// check useNamespaceAddrSet function comments to explain this behaviour
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Labels = map[string]string{labelName: labelVal}
				nPodTest := getTestPod(namespace1.Name, nodeName)

				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
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

				startOvnWithHostNetPods(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil, true)

				ginkgo.By("Check networkPolicy includes hostNetwork")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// namespaced address set won't have hostNet pod ip
				// netpol peer address set will

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy),
					initialDB.NBData)
				netpolASv4, _ := buildNetworkPolicyPeerAddressSets(namespace1.Name, networkPolicy.Spec.Egress[0].To[0], nPodTest.podIP)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				expectedData = append(expectedData, netpolASv4, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("correctly creates networkpolicy ignoring hostNetwork pods with nil podSelector", func() {
			// check useNamespaceAddrSet function comments to explain this behaviour
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace1.Labels = map[string]string{labelName: labelVal}
				nPodTest := getTestPod(namespace1.Name, nodeName)

				networkPolicy := newNetworkPolicy(netPolicyName1, namespace1.Name,
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

				startOvnWithHostNetPods(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil, true)

				ginkgo.By("Check networkPolicy doesn't select hostNetwork pods")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := getNamespaceWithSinglePolicyExpectedData(
					newNetpolDataParams(networkPolicy).
						withPeerNamespaces(namespace1.Name),
					initialDB.NBData)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespace1.Name, nil)
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})
	})

	ginkgo.Context("ACL logging for network policies", func() {

		var originalNamespace v1.Namespace

		updateNamespaceACLLogSeverity := func(namespaceToUpdate *v1.Namespace, desiredDenyLogLevel string, desiredAllowLogLevel string) error {
			ginkgo.By("updating the namespace's ACL logging severity")
			updatedLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, desiredDenyLogLevel, desiredAllowLogLevel)
			namespaceToUpdate.Annotations[util.AclLoggingAnnotation] = updatedLogSeverity

			_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceToUpdate, metav1.UpdateOptions{})
			return err
		}

		ginkgo.BeforeEach(func() {
			originalACLLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice)
			originalNamespace = *newNamespace(namespaceName1)
			originalNamespace.Annotations = map[string]string{util.AclLoggingAnnotation: originalACLLogSeverity}
		})

		ginkgo.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...*knet.NetworkPolicy) {
			ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
			initialDenyAllPolicy := newNetworkPolicy("emptyPol", namespaceName1, metav1.LabelSelector{}, nil, nil)
			// originalACLLogSeverity.Deny == nbdb.ACLSeverityAlert
			initialExpectedData := getDefaultDenyData(newNetpolDataParams(initialDenyAllPolicy).
				withDenyLogSeverity(nbdb.ACLSeverityAlert))
			initialExpectedData = append(initialExpectedData,
				// no gress policies defined, return only port group
				getPolicyData(newNetpolDataParams(initialDenyAllPolicy).
					withAllowLogSeverity(nbdb.ACLSeverityNotice))...)
			initialExpectedData = append(initialExpectedData, initialDB.NBData...)

			app.Action = func(ctx *cli.Context) error {
				startOvn(initialDB, []v1.Namespace{originalNamespace}, []knet.NetworkPolicy{*initialDenyAllPolicy},
					nil, nil)
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(originalNamespace.Name, nil)
				initialExpectedData = append(initialExpectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(initialExpectedData...))

				var createsPoliciesData []libovsdbtest.TestData
				// create network policies for given Entry
				for i := range networkPolicies {
					ginkgo.By("Creating new network policy")
					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicies[i].GetNamespace()).
						Create(context.TODO(), networkPolicies[i], metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					netpolAS := buildNetworkPolicyAddressSets(networkPolicies[i])
					createsPoliciesData = append(createsPoliciesData, netpolAS...)

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
					netpolAS := buildNetworkPolicyAddressSets(networkPolicies[i])
					expectedData = append(expectedData, netpolAS...)
					expectedData = append(expectedData,
						getPolicyData(newNetpolDataParams(networkPolicies[i]).
							withAllowLogSeverity(updatedLogSeverity))...)
				}
				expectedData = append(expectedData, initialDB.NBData...)

				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		},
			ginkgo.Entry("when the namespace features a network policy with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)),
			ginkgo.Entry("when the namespace features *multiple* network policies with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false),
				getMatchLabelsNetworkPolicy(netPolicyName2, namespaceName1, namespaceName2, "", false, true)),
			ginkgo.Entry("when the namespace features a network policy with *multiple* rules",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "tiny-winy-pod", true, false)))

		ginkgo.It("policies created after namespace logging level updates inherit updated logging level", func() {
			app.Action = func(ctx *cli.Context) error {
				startOvn(initialDB, []v1.Namespace{originalNamespace}, nil, nil, nil)
				desiredLogSeverity := nbdb.ACLSeverityDebug
				// update namespace log severity
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, desiredLogSeverity, desiredLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				newPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)
				ginkgo.By("Creating new network policy")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(namespaceName1).
					Create(context.TODO(), newPolicy, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should have managed to create a new network policy")

				expectedData := initialDB.NBData
				expectedData = append(expectedData, getPolicyData(newNetpolDataParams(newPolicy).
					withAllowLogSeverity(desiredLogSeverity))...)
				expectedData = append(expectedData, getDefaultDenyData(newNetpolDataParams(newPolicy).
					withDenyLogSeverity(desiredLogSeverity))...)

				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespaceName1, nil)
				expectedData = append(expectedData, namespace1AddressSetv4)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				return nil
			}
			gomega.Expect(app.Run([]string{app.Name})).To(gomega.Succeed())
		})

		ginkgo.It("creates stateless OVN ACLs based off of the annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				networkPolicy.Annotations = map[string]string{
					ovnStatelessNetPolAnnotationName: "true",
				}
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
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
				namespace1AddressSetv4, _ := buildNamespaceAddressSets(namespaceName1, []string{nPodTest.podIP})
				expectedData = append(expectedData, namespace1AddressSetv4)
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
		controllerName = DefaultNetworkControllerName
	)

	getFakeController := func(nbClient libovsdbclient.Client) *DefaultNetworkController {
		controller := getFakeController(DefaultNetworkControllerName)
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

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName         string = "pg-name"
			controllerName        = DefaultNetworkControllerName
		)
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		asFactory = addressset.NewFakeAddressSetFactory(controllerName)
		config.IPv4Mode = true
		config.IPv6Mode = false
		asIDs := getPodSelectorAddrSetDbIDs("test_name", DefaultNetworkControllerName)
		gp := newGressPolicy(knet.PolicyTypeIngress, 0, "testing", "policy", controllerName,
			false, &util.DefaultNetInfo{})
		gp.hasPeerSelector = true
		gp.addPeerAddressSets(addressset.GetHashNamesForAS(asIDs))

		one := getNamespaceAddrSetDbIDs("ns1", controllerName)
		two := getNamespaceAddrSetDbIDs("ns2", controllerName)
		three := getNamespaceAddrSetDbIDs("ns3", controllerName)
		four := getNamespaceAddrSetDbIDs("ns4", controllerName)
		five := getNamespaceAddrSetDbIDs("ns5", controllerName)
		six := getNamespaceAddrSetDbIDs("ns6", controllerName)
		for _, addrSetID := range []*libovsdbops.DbObjectIDs{one, two, three, four, five, six} {
			_, err := asFactory.EnsureAddressSet(addrSetID)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		defaultAclLogging := libovsdbutil.ACLLoggingLevels{
			Allow: nbdb.ACLSeverityInfo,
			Deny:  nbdb.ACLSeverityInfo,
		}

		gomega.Expect(gp.addNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected := buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one}, &defaultAclLogging)
		actual, _ := gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdbtest.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())
	})
})
