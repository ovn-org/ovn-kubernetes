package ovn

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/urfave/cli/v2"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
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
	utilpointer "k8s.io/utils/pointer"
)

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

func getDefaultDenyData(networkPolicy *knet.NetworkPolicy, ports []string,
	denyLogSeverity nbdb.ACLSeverity, stale bool) []libovsdb.TestData {
	egressPGName := defaultDenyPortGroupName(networkPolicy.Namespace, egressDefaultDenySuffix)
	policyTypeIngress, policyTypeEgress := getPolicyType(networkPolicy)
	shouldBeLogged := denyLogSeverity != ""
	aclName := getDefaultDenyPolicyACLName(networkPolicy.Namespace, lportEgressAfterLB)
	egressDenyACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultDenyPriority,
		"inport == @"+egressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressDenyACL.UUID = aclName + "-egressDenyACL-UUID"

	aclName = getARPAllowACLName(networkPolicy.Namespace)
	egressAllowACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionFromLport,
		types.DefaultAllowPriority,
		"inport == @"+egressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressAllowACL.UUID = aclName + "-egressAllowACL-UUID"

	ingressPGName := defaultDenyPortGroupName(networkPolicy.Namespace, ingressDefaultDenySuffix)
	aclName = getDefaultDenyPolicyACLName(networkPolicy.Namespace, lportIngress)
	ingressDenyACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultDenyPriority,
		"outport == @"+ingressPGName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denyLogSeverity,
		shouldBeLogged,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressDenyACL.UUID = aclName + "-ingressDenyACL-UUID"

	aclName = getARPAllowACLName(networkPolicy.Namespace)
	ingressAllowACL := libovsdbops.BuildACL(
		aclName,
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		"outport == @"+ingressPGName+" && "+arpAllowPolicyMatch,
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		"",
		false,
		map[string]string{
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressAllowACL.UUID = aclName + "-ingressAllowACL-UUID"

	if stale {
		getStaleDefaultACL([]*nbdb.ACL{egressDenyACL, egressAllowACL})
		getStaleDefaultACL([]*nbdb.ACL{ingressDenyACL, ingressAllowACL})
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range ports {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	var egressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeEgress {
		egressDenyPorts = lsps
	}
	egressDenyPG := libovsdbops.BuildPortGroup(
		egressPGName,
		egressPGName,
		egressDenyPorts,
		[]*nbdb.ACL{egressDenyACL, egressAllowACL},
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	var ingressDenyPorts []*nbdb.LogicalSwitchPort
	if policyTypeIngress {
		ingressDenyPorts = lsps
	}
	ingressDenyPG := libovsdbops.BuildPortGroup(
		ingressPGName,
		ingressPGName,
		ingressDenyPorts,
		[]*nbdb.ACL{ingressDenyACL, ingressAllowACL},
	)
	ingressDenyPG.UUID = ingressDenyPG.Name + "-UUID"

	return []libovsdb.TestData{
		egressDenyACL,
		egressAllowACL,
		ingressDenyACL,
		ingressAllowACL,
		egressDenyPG,
		ingressDenyPG,
	}
}

func getStaleDefaultACL(acls []*nbdb.ACL) []*nbdb.ACL {
	for _, acl := range acls {
		acl.Options = nil
		acl.Direction = nbdb.ACLDirectionToLport
	}
	return acls
}

func getGressACLs(i int, namespace, policyName string, peerNamespaces []string, tcpPeerPorts []int32,
	peers []knet.NetworkPolicyPeer, logSeverity nbdb.ACLSeverity, policyType knet.PolicyType, stale bool) []*nbdb.ACL {
	aclName := getGressPolicyACLName(namespace, policyName, i)
	pgName, _ := getNetworkPolicyPGName(namespace, policyName)
	shouldBeLogged := logSeverity != ""
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
	for _, nsName := range peerNamespaces {
		hashedASName, _ := getNsAddrSetHashNames(nsName)
		hashedASNames = append(hashedASNames, hashedASName)
	}
	ipBlock := ""
	for _, peer := range peers {
		if peer.PodSelector != nil && len(peerNamespaces) == 0 {
			peerIndex := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
			asv4, _ := addressset.GetHashNamesForAS(peerIndex)
			hashedASNames = append(hashedASNames, asv4)
		}
		if peer.IPBlock != nil {
			ipBlock = peer.IPBlock.CIDR
		}
	}
	if len(hashedASNames) > 0 {
		gressAsMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.%s == {%s}", ipDir, gressAsMatch)
		if policyType == knet.PolicyTypeIngress {
			match = fmt.Sprintf("(%s || (ip4.src == %s && ip4.dst == {%s}))", match, types.V4OVNServiceHairpinMasqueradeIP, gressAsMatch)
		}
		match += fmt.Sprintf(" && %s == @%s", portDir, pgName)
		acl := libovsdbops.BuildACL(
			aclName,
			direction,
			types.DefaultAllowPriority,
			match,
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			map[string]string{
				l4MatchACLExtIdKey:          "None",
				ipBlockCIDRACLExtIdKey:      "false",
				namespaceACLExtIdKey:        namespace,
				policyACLExtIdKey:           policyName,
				policyTypeACLExtIdKey:       string(policyType),
				string(policyType) + "_num": strconv.Itoa(i),
			},
			options,
		)
		acl.UUID = aclName + string(policyType) + "-UUID"
		acls = append(acls, acl)
	}
	if ipBlock != "" {
		match := fmt.Sprintf("ip4.%s == %s && %s == @%s", ipDir, ipBlock, portDir, pgName)
		acl := libovsdbops.BuildACL(
			aclName,
			direction,
			types.DefaultAllowPriority,
			match,
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			map[string]string{
				l4MatchACLExtIdKey:          "None",
				ipBlockCIDRACLExtIdKey:      "true",
				namespaceACLExtIdKey:        namespace,
				policyACLExtIdKey:           policyName,
				policyTypeACLExtIdKey:       string(policyType),
				string(policyType) + "_num": strconv.Itoa(i),
			},
			options,
		)
		acl.UUID = aclName + string(policyType) + "-UUID"
		acls = append(acls, acl)
	}
	for _, v := range tcpPeerPorts {
		acl := libovsdbops.BuildACL(
			aclName,
			direction,
			types.DefaultAllowPriority,
			fmt.Sprintf("ip4 && tcp && tcp.dst==%d && %s == @%s", v, portDir, pgName),
			nbdb.ACLActionAllowRelated,
			types.OvnACLLoggingMeter,
			logSeverity,
			shouldBeLogged,
			map[string]string{
				l4MatchACLExtIdKey:          fmt.Sprintf("tcp && tcp.dst==%d", v),
				ipBlockCIDRACLExtIdKey:      "false",
				namespaceACLExtIdKey:        namespace,
				policyACLExtIdKey:           policyName,
				policyTypeACLExtIdKey:       string(policyType),
				string(policyType) + "_num": strconv.Itoa(i),
			},
			options,
		)
		acl.UUID = fmt.Sprintf("%s-%s-port_%d-UUID", string(policyType), aclName, v)
		acls = append(acls, acl)
	}
	if stale {
		return getStalePolicyACL(acls)
	}
	return acls
}

func getStalePolicyACL(acls []*nbdb.ACL) []*nbdb.ACL {
	for _, acl := range acls {
		acl.Options = nil
		acl.Direction = nbdb.ACLDirectionToLport
	}
	return acls
}

func getPolicyData(networkPolicy *knet.NetworkPolicy, localPortUUIDs []string, peerNamespaces []string,
	tcpPeerPorts []int32, allowLogSeverity nbdb.ACLSeverity, stale bool) []libovsdbtest.TestData {
	acls := []*nbdb.ACL{}

	for i, ingress := range networkPolicy.Spec.Ingress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, tcpPeerPorts, ingress.From, allowLogSeverity, knet.PolicyTypeIngress, stale)...)
	}
	for i, egress := range networkPolicy.Spec.Egress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, tcpPeerPorts, egress.To, allowLogSeverity, knet.PolicyTypeEgress, stale)...)
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range localPortUUIDs {
		lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
	}

	pgName, readableName := getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
	pg := libovsdbops.BuildPortGroup(
		pgName,
		readableName,
		lsps,
		acls,
	)
	pg.UUID = pg.Name + "-UUID"

	data := []libovsdb.TestData{}
	for _, acl := range acls {
		data = append(data, acl)
	}
	data = append(data, pg)
	return data
}

func eventuallyExpectNoAddressSets(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace string) {
	dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
	fakeOvn.asf.EventuallyExpectNoAddressSet(dbIDs)
}

func eventuallyExpectAddressSetsWithIP(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace, ip string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(dbIDs, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(fakeOvn *FakeOVN, peer knet.NetworkPolicyPeer, namespace string) {
	if peer.PodSelector != nil {
		dbIDs := getPodSelectorAddrSetDbIDs(getPodSelectorKey(peer.PodSelector, peer.NamespaceSelector, namespace), DefaultNetworkControllerName)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(dbIDs)
	}
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
		initialDB libovsdb.TestSetup

		gomegaFormatMaxLength int
		logicalSwitch         *nbdb.LogicalSwitch
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN()

		gomegaFormatMaxLength = format.MaxLength
		format.MaxLength = 0
		logicalSwitch = &nbdb.LogicalSwitch{
			Name: nodeName,
			UUID: nodeName + "_UUID",
		}
		initialDB = libovsdb.TestSetup{
			NBData: []libovsdb.TestData{
				logicalSwitch,
			},
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
		format.MaxLength = gomegaFormatMaxLength
	})

	startOvn := func(dbSetup libovsdb.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy,
		pods []testPod, podLabels map[string]string) {
		var podsList []v1.Pod
		for _, testPod := range pods {
			knetPod := newPod(testPod.namespace, testPod.podName, testPod.nodeName, testPod.podIP)
			if len(podLabels) > 0 {
				knetPod.Labels = podLabels
			}
			podsList = append(podsList, *knetPod)
		}
		fakeOvn.startWithDBSetup(dbSetup,
			&v1.NamespaceList{
				Items: namespaces,
			},
			&v1.PodList{
				Items: podsList,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
		)
		for _, testPod := range pods {
			testPod.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, nodeName))
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

	ginkgo.Context("on startup", func() {
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

				initialData := []libovsdb.TestData{}
				afterCleanup := []libovsdb.TestData{}
				policy1Db := getPolicyData(networkPolicy1, nil, []string{}, nil, "", false)
				initialData = append(initialData, policy1Db...)
				// since acls are not garbage-collected, only port group will be deleted
				afterCleanup = append(afterCleanup, policy1Db[:len(policy1Db)-1]...)

				policy2Db := getPolicyData(networkPolicy2, nil, []string{}, nil, "", false)
				initialData = append(initialData, policy2Db...)
				// since acls are not garbage-collected, only port group will be deleted
				afterCleanup = append(afterCleanup, policy2Db[:len(policy2Db)-1]...)

				defaultDenyDb := getDefaultDenyData(networkPolicy1, nil, "", false)
				initialData = append(initialData, defaultDenyDb...)
				// since acls are not garbage-collected, only port groups will be deleted
				afterCleanup = append(afterCleanup, defaultDenyDb[:len(defaultDenyDb)-2]...)

				// start ovn with no objects, expect stale port db entries to be cleaned up
				startOvn(libovsdb.TestSetup{NBData: initialData}, nil, nil, nil, nil)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(afterCleanup))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy with empty db", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				startOvn(initialDB, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*networkPolicy},
					nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData := initialDB.NBData
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing networkPolicy updating stale ACLs", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				// start with stale ACLs
				gressPolicyInitialData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", true)
				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", true)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// make sure stale ACLs were updated
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an existing networkPolicy updating stale address sets", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", false, true)
				// start with stale ACLs
				staleAddrSetIDs := getStaleNetpolAddrSetDbIDs(networkPolicy.Namespace, networkPolicy.Name,
					"egress", "0", DefaultNetworkControllerName)
				localASName, _ := addressset.GetHashNamesForAS(staleAddrSetIDs)
				peerASName, _ := getNsAddrSetHashNames(namespace2.Name)
				pgName, readableName := getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
				staleACL := libovsdbops.BuildACL(
					"staleACL",
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					fmt.Sprintf("ip4.dst == {$%s, $%s} && inport == @%s", localASName, peerASName, pgName),
					nbdb.ACLActionAllowRelated,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						l4MatchACLExtIdKey:     "None",
						ipBlockCIDRACLExtIdKey: "false",
						namespaceACLExtIdKey:   networkPolicy.Namespace,
						policyACLExtIdKey:      networkPolicy.Name,
						policyTypeACLExtIdKey:  "egress",
						"egress_num":           "0",
					},
					map[string]string{
						"apply-after-lb": "true",
					},
				)
				staleACL.UUID = "staleACL-UUID"
				pg := libovsdbops.BuildPortGroup(
					pgName,
					readableName,
					nil,
					[]*nbdb.ACL{staleACL},
				)
				pg.UUID = pg.Name + "-UUID"

				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", true)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, defaultDenyInitialData...)
				initialData = append(initialData, staleACL, pg)
				_, err := fakeOvn.asf.NewAddressSet(staleAddrSetIDs, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				// make sure stale ACLs were updated
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				// stale acl will be de-refenced, but not garbage collected
				expectedData = append(expectedData, staleACL)
				fakeOvn.asf.ExpectEmptyAddressSet(staleAddrSetIDs)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an ingress networkPolicy updating an existing ACL", func() {
			app.Action = func(ctx *cli.Context) error {

				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)

				// network policy without peer namespace
				gressPolicyInitialData := getPolicyData(networkPolicy, nil, []string{}, nil,
					"", false)
				defaultDenyInitialData := getDefaultDenyData(networkPolicy, nil, "", false)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// check peer namespace was added
				expectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil,
					"", false)
				expectedData = append(expectedData, defaultDenyInitialData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{}, nil,
					"", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("during execution", func() {
		table.DescribeTable("correctly uses namespace and shared peer selector address sets",
			func(peer knet.NetworkPolicyPeer, peerNamespaces []string) {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				netpol := newNetworkPolicy("netpolName", namespace1.Name, metav1.LabelSelector{}, []knet.NetworkPolicyIngressRule{
					{
						From: []knet.NetworkPolicyPeer{peer},
					},
				}, nil)
				startOvn(libovsdb.TestSetup{}, []v1.Namespace{namespace1, namespace2}, []knet.NetworkPolicy{*netpol}, nil, nil)
				expectedData := []libovsdb.TestData{}
				gressPolicyExpectedData := getPolicyData(netpol, nil,
					peerNamespaces, nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(netpol, nil, "", false)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
			},
			table.Entry("for empty pod selector => use netpol namespace address set",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{},
				}, []string{namespaceName1}),
			table.Entry("namespace selector with empty pod selector => use a set of selected namespace address sets",
				knet.NetworkPolicyPeer{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": namespaceName2,
						},
					},
				}, []string{namespaceName2}),
			table.Entry("empty namespace and pod selector => use all pods shared address set",
				knet.NetworkPolicyPeer{
					PodSelector:       &metav1.LabelSelector{},
					NamespaceSelector: &metav1.LabelSelector{},
				}, nil),
			table.Entry("pod selector with nil namespace => use static namespace+pod selector",
				knet.NetworkPolicyPeer{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"name": "podName",
						},
					},
				}, nil),
			table.Entry("pod selector with namespace selector => use namespace selector+pod selector",
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
			table.Entry("pod selector with empty namespace selector => use global pod selector",
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
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false)
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// TODO: test server does not garbage collect ACLs, so we just expect policy2 portgroup to be removed
				expectedDataWithoutPolicy2 := append(expectedData, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithoutPolicy2...))

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				acls = append(acls, gressPolicy2ExpectedData[:len(gressPolicy2ExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls,
					getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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
				time.Sleep(types.OVSDBTimeout + time.Second)
				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)

				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false)
				expectedDataWithPolicy2 := append(expectedData, gressPolicy2ExpectedData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedDataWithPolicy2...))
				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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
				time.Sleep(types.OVSDBTimeout + time.Second)
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
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				ginkgo.By("Create a new network policy with same name")

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false)
				defaultDenyExpectedData2 := getDefaultDenyData(networkPolicy2, []string{nPodTest.portUUID}, "", false)

				expectedData = []libovsdb.TestData{}
				// FIXME(trozet): libovsdb server doesn't remove referenced ACLs to PG when deleting the PG
				// https://github.com/ovn-org/libovsdb/issues/219
				expectedPolicy1ACLs := gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]
				expectedData = append(expectedData, expectedPolicy1ACLs...)
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData2...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				// check the cache no longer has the entry
				key, err = retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("deleting a network policy that failed half-way through creation succeeds", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk") // create with 63 characters
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				egressOptions := map[string]string{
					"apply-after-lb": "true",
				}
				egressPGName := defaultDenyPortGroupName(networkPolicy.Namespace, egressDefaultDenySuffix)
				aclName := getARPAllowACLName(networkPolicy.Namespace)
				leftOverACLFromUpgrade1 := libovsdbops.BuildACL(
					aclName,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+egressPGName+" && (arp)", // invalid ACL match; won't be cleaned up
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACLFromUpgrade1.UUID = *leftOverACLFromUpgrade1.Name + "-egressAllowACL-UUID1"

				aclName = getARPAllowACLName(networkPolicy.Namespace)
				leftOverACLFromUpgrade2 := libovsdbops.BuildACL(
					aclName,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+egressPGName+" && "+arpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACLFromUpgrade2.UUID = *leftOverACLFromUpgrade2.Name + "-egressAllowACL-UUID2"

				initialDB.NBData = append(initialDB.NBData, leftOverACLFromUpgrade1, leftOverACLFromUpgrade2)
				startOvn(initialDB, []v1.Namespace{namespace1}, nil, []testPod{nPodTest},
					map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that applies to a pod and ensuring creation fails")

				err := fakeOvn.controller.addNetworkPolicy(networkPolicy)
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("failed to create Network Policy " +
					"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1: failed to " +
					"create default deny port groups: unexpectedly found multiple results for provided predicate"))

				// ensure the default PGs and ACLs were removed via rollback from add failure
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				// note stale leftovers from previous upgrades won't be cleanedup
				expectedData = append(expectedData, leftOverACLFromUpgrade1, leftOverACLFromUpgrade2)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Deleting the network policy that failed to create and ensuring we don't panic")
				err = fakeOvn.controller.deleteNetworkPolicy(networkPolicy)
				// I0623 policy.go:1285] Deleting network policy networkpolicy1 in namespace namespace1, np is nil: true
				// W0623 policy.go:1315] Unable to delete network policy: namespace1/networkpolicy1 since its not found in cache
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("stale ACLs should be cleaned up or updated at startup via syncNetworkPolicies", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum)
				// This is not yet going to be created
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1)
				// network policy should exist for port group to not be cleaned up
				networkPolicy3 := getPortNetworkPolicy(netPolicyName1, "leftover1", labelName, labelVal, portNum)
				egressPGName := defaultDenyPortGroupName("leftover1", egressDefaultDenySuffix)
				ingressPGName := defaultDenyPortGroupName("leftover1", ingressDefaultDenySuffix)
				egressOptions := map[string]string{
					// older versions of ACLs don't have, should be added by syncNetworkPolicies on startup
					//	"apply-after-lb": "true",
				}
				// ACL1: leftover arp allow ACL egress with old match (arp)
				leftOverACL1FromUpgrade := libovsdbops.BuildACL(
					getARPAllowACLName("leftover1"),
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+egressPGName+" && "+staleArpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACL1FromUpgrade.UUID = *leftOverACL1FromUpgrade.Name + "-egressAllowACL-UUID"
				testOnlyEgressDenyPG := libovsdbops.BuildPortGroup(
					egressPGName,
					egressPGName,
					nil,
					[]*nbdb.ACL{leftOverACL1FromUpgrade},
				)
				testOnlyEgressDenyPG.UUID = testOnlyEgressDenyPG.Name + "-UUID"
				// ACL2: leftover arp allow ACL ingress with old match (arp)
				leftOverACL2FromUpgrade := libovsdbops.BuildACL(
					getARPAllowACLName("leftover1"),
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"outport == @"+ingressPGName+" && "+staleArpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL2FromUpgrade.UUID = *leftOverACL2FromUpgrade.Name + "-ingressAllowACL-UUID"
				testOnlyIngressDenyPG := libovsdbops.BuildPortGroup(
					ingressPGName,
					ingressPGName,
					nil,
					[]*nbdb.ACL{leftOverACL2FromUpgrade},
				)
				testOnlyIngressDenyPG.UUID = testOnlyIngressDenyPG.Name + "-UUID"

				// ACL3: leftover default deny ACL egress with old name (namespace_policyname)
				leftOverACL3FromUpgrade := libovsdbops.BuildACL(
					"youknownothingjonsnowyouknownothingjonsnowyouknownothingjonsnow"+"_"+networkPolicy2.Name,
					nbdb.ACLDirectionFromLport,
					types.DefaultDenyPriority,
					"inport == @"+egressPGName,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					egressOptions,
				)
				leftOverACL3FromUpgrade.UUID = *leftOverACL3FromUpgrade.Name + "-egressDenyACL-UUID"

				// ACL4: leftover default deny ACL ingress with old name (namespace_policyname)
				leftOverACL4FromUpgrade := libovsdbops.BuildACL(
					"shortName"+"_"+networkPolicy2.Name,
					nbdb.ACLDirectionToLport,
					types.DefaultDenyPriority,
					"outport == @"+ingressPGName,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL4FromUpgrade.UUID = *leftOverACL4FromUpgrade.Name + "-ingressDenyACL-UUID"

				initialDB.NBData = append(
					initialDB.NBData,
					leftOverACL1FromUpgrade,
					leftOverACL2FromUpgrade,
					leftOverACL3FromUpgrade,
					leftOverACL4FromUpgrade,
					testOnlyIngressDenyPG,
					testOnlyEgressDenyPG,
				)

				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy1, *networkPolicy3},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gressPolicy1ExpectedData := getPolicyData(networkPolicy1, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false)
				defaultDeny1ExpectedData := getDefaultDenyData(networkPolicy1, []string{nPodTest.portUUID}, "", false)
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDeny1ExpectedData...)
				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false)
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				egressOptions = map[string]string{
					"apply-after-lb": "true",
				}
				leftOverACL3FromUpgrade.Options = egressOptions
				newDefaultDenyEgressACLName := "youknownothingjonsnowyouknownothingjonsnowyouknownothingjonsnow" // trims it according to RFC1123
				newDefaultDenyIngressACLName := getDefaultDenyPolicyACLName("shortName", lportIngress)
				leftOverACL3FromUpgrade.Name = &newDefaultDenyEgressACLName
				leftOverACL4FromUpgrade.Name = &newDefaultDenyIngressACLName
				expectedData = append(expectedData, leftOverACL3FromUpgrade)
				expectedData = append(expectedData, leftOverACL4FromUpgrade)
				testOnlyIngressDenyPG.ACLs = nil // Sync Function should remove stale ACL from PGs
				testOnlyEgressDenyPG.ACLs = nil  // Sync Function should remove stale ACL from PGs
				expectedData = append(expectedData, testOnlyIngressDenyPG)
				// since test server doesn't garbage collect dereferenced acls, they will stay in the test db after they
				// were deleted. Even though they are derefenced from the port group at this point, they will be updated
				// as all the other ACLs.
				// Update deleted leftOverACL1FromUpgrade and leftOverACL2FromUpgrade to match on expected data
				// Once our test server can delete such acls, this part should be deleted
				// start of db hack
				newDefaultDenyLeftoverIngressACLName := getDefaultDenyPolicyACLName("leftover1", lportIngress)
				newDefaultDenyLeftoverEgressACLName := getDefaultDenyPolicyACLName("leftover1", lportEgressAfterLB)
				leftOverACL2FromUpgrade.Name = &newDefaultDenyLeftoverIngressACLName
				leftOverACL1FromUpgrade.Name = &newDefaultDenyLeftoverEgressACLName
				leftOverACL1FromUpgrade.Options = egressOptions
				expectedData = append(expectedData, leftOverACL2FromUpgrade)
				expectedData = append(expectedData, leftOverACL1FromUpgrade)
				// end of db hack
				expectedData = append(expectedData, testOnlyEgressDenyPG)

				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("ACLs with long names and run syncNetworkPolicies", func() {
			app.Action = func(ctx *cli.Context) error {
				longNameSpaceName := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // create with 63 characters
				longNamespace := *newNamespace(longNameSpaceName)
				nPodTest := getTestPod(longNamespace.Name, nodeName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, longNamespace.Name, labelName, labelVal, portNum)
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, longNamespace.Name, labelName, labelVal, portNum+1)

				longLeftOverNameSpaceName := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"  // namespace is >45 characters long
				longLeftOverNameSpaceName2 := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxy1" // namespace is >45 characters long
				// network policy should exist for port group to not be cleaned up
				networkPolicy3 := getPortNetworkPolicy(netPolicyName1, longLeftOverNameSpaceName, labelName, labelVal, portNum)
				egressPGName := defaultDenyPortGroupName(longLeftOverNameSpaceName, egressDefaultDenySuffix)
				ingressPGName := defaultDenyPortGroupName(longLeftOverNameSpaceName, ingressDefaultDenySuffix)
				// ACL1: leftover arp allow ACL egress with old match (arp)
				leftOverACL1FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+egressPGName+" && "+staleArpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					nil,
				)
				leftOverACL1FromUpgrade.UUID = *leftOverACL1FromUpgrade.Name + "-egressAllowACL-UUID"
				testOnlyEgressDenyPG := libovsdbops.BuildPortGroup(
					egressPGName,
					egressPGName,
					nil,
					[]*nbdb.ACL{leftOverACL1FromUpgrade},
				)
				testOnlyEgressDenyPG.UUID = testOnlyEgressDenyPG.Name + "-UUID"
				// ACL2: leftover arp allow ACL ingress with old match (arp)
				leftOverACL2FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"outport == @"+ingressPGName+" && "+staleArpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL2FromUpgrade.UUID = *leftOverACL2FromUpgrade.Name + "-ingressAllowACL-UUID"

				// ACL3: leftover arp allow ACL ingress with new match (arp || nd)
				leftOverACL3FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName+"blah"+"_"+arpAllowPolicySuffix,
					nbdb.ACLDirectionToLport,
					types.DefaultAllowPriority,
					"outport == @"+ingressPGName+" && "+arpAllowPolicyMatch, // new match! this ACL should be left as is!
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL3FromUpgrade.UUID = *leftOverACL3FromUpgrade.Name + "-ingressAllowACL-UUID1"
				testOnlyIngressDenyPG := libovsdbops.BuildPortGroup(
					ingressPGName,
					ingressPGName,
					nil,
					[]*nbdb.ACL{leftOverACL2FromUpgrade, leftOverACL3FromUpgrade},
				)
				testOnlyIngressDenyPG.UUID = testOnlyIngressDenyPG.Name + "-UUID"

				// ACL4: leftover default deny ACL egress with old name (namespace_policyname)
				leftOverACL4FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName2+"_"+networkPolicy2.Name, // we are ok here because test server doesn't impose restrictions
					nbdb.ACLDirectionFromLport,
					types.DefaultDenyPriority,
					"inport == @"+egressPGName,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
					},
					nil,
				)
				leftOverACL4FromUpgrade.UUID = *leftOverACL4FromUpgrade.Name + "-egressDenyACL-UUID"

				// ACL5: leftover default deny ACL ingress with old name (namespace_policyname)
				leftOverACL5FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName2+"_"+networkPolicy2.Name, // we are ok here because test server doesn't impose restrictions
					nbdb.ACLDirectionToLport,
					types.DefaultDenyPriority,
					"outport == @"+ingressPGName,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL5FromUpgrade.UUID = *leftOverACL5FromUpgrade.Name + "-ingressDenyACL-UUID"

				longLeftOverNameSpaceName62 := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghij"
				// ACL6: leftover default deny ACL ingress with old name (namespace_policyname) but namespace is 62 characters long
				leftOverACL6FromUpgrade := libovsdbops.BuildACL(
					longLeftOverNameSpaceName62+"_"+networkPolicy2.Name,
					nbdb.ACLDirectionToLport,
					types.DefaultDenyPriority,
					"outport == @"+ingressPGName,
					nbdb.ACLActionDrop,
					types.OvnACLLoggingMeter,
					nbdb.ACLSeverityInfo,
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
					},
					nil,
				)
				leftOverACL6FromUpgrade.UUID = *leftOverACL6FromUpgrade.Name + "-ingressDenyACL-UUID"

				initialDB.NBData = append(
					initialDB.NBData,
					leftOverACL1FromUpgrade,
					leftOverACL2FromUpgrade,
					leftOverACL3FromUpgrade,
					leftOverACL4FromUpgrade,
					leftOverACL5FromUpgrade,
					leftOverACL6FromUpgrade,
					testOnlyIngressDenyPG,
					testOnlyEgressDenyPG,
				)

				startOvn(initialDB, []v1.Namespace{longNamespace}, []knet.NetworkPolicy{*networkPolicy1, *networkPolicy3},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that applies to a pod")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Get(context.TODO(), networkPolicy1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(longNamespace.Name, []string{nPodTest.podIP})
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{"node1"})...)
				gressPolicy1ExpectedData := getPolicyData(networkPolicy1, []string{nPodTest.portUUID},
					nil, []int32{portNum}, "", false)
				defaultDeny1ExpectedData := getDefaultDenyData(networkPolicy1, []string{nPodTest.portUUID},
					"", false)
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDeny1ExpectedData...)

				// Create a second NP
				ginkgo.By("Creating another policy that references that pod")
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Get(context.TODO(), networkPolicy2.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gressPolicy2ExpectedData := getPolicyData(networkPolicy2, []string{nPodTest.portUUID},
					nil, []int32{portNum + 1}, "", false)
				expectedData = append(expectedData, gressPolicy2ExpectedData...)
				egressOptions := map[string]string{
					"apply-after-lb": "true",
				}
				leftOverACL4FromUpgrade.Options = egressOptions
				leftOverACL3FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName + "blah_ARPall")  // trims it according to RFC1123
				leftOverACL4FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName2 + "_egressDefa") // trims it according to RFC1123
				leftOverACL5FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName2 + "_ingressDef") // trims it according to RFC1123
				leftOverACL6FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName62 + "_")          // name stays the same here since its no-op
				expectedData = append(expectedData, leftOverACL3FromUpgrade)
				expectedData = append(expectedData, leftOverACL4FromUpgrade)
				expectedData = append(expectedData, leftOverACL5FromUpgrade)
				expectedData = append(expectedData, leftOverACL6FromUpgrade)
				testOnlyIngressDenyPG.ACLs = []string{leftOverACL3FromUpgrade.UUID} // Sync Function should remove stale ACL from PGs
				testOnlyEgressDenyPG.ACLs = nil                                     // Sync Function should remove stale ACL from PGs

				// since test server doesn't garbage collect dereferenced acls, they will stay in the test db after they
				// were deleted. Even though they are derefenced from the port group at this point, they will be updated
				// as all the other ACLs.
				// Update deleted leftOverACL1FromUpgrade and leftOverACL2FromUpgrade to match on expected data
				// Once our test server can delete such acls, this part should be deleted
				// start of db hack
				longLeftOverIngressName := longLeftOverNameSpaceName + "_ingressDef"
				longLeftOverEgressName := longLeftOverNameSpaceName + "_egressDefa"
				leftOverACL2FromUpgrade.Name = &longLeftOverIngressName
				leftOverACL1FromUpgrade.Name = &longLeftOverEgressName
				leftOverACL1FromUpgrade.Options = egressOptions
				expectedData = append(expectedData, leftOverACL2FromUpgrade)
				expectedData = append(expectedData, leftOverACL1FromUpgrade)
				// end of db hack
				expectedData = append(expectedData, testOnlyIngressDenyPG)
				expectedData = append(expectedData, testOnlyEgressDenyPG)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// in stable state, after all stale acls were updated and cleanedup invoke sync again to re-enact a master restart
				ginkgo.By("Trigger another syncNetworkPolicies run and ensure nothing has changed in the DB")
				fakeOvn.controller.syncNetworkPolicies([]interface{}{networkPolicy1, networkPolicy2, networkPolicy3})
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Simulate the initial re-add of all network policies during upgrade and ensure we are stable")
				fakeOvn.controller.networkPolicies.Delete(getPolicyKey(networkPolicy1))
				fakeOvn.controller.sharedNetpolPortGroups.Delete(networkPolicy1.Namespace) // reset cache so that we simulate the add that happens during upgrades
				err = fakeOvn.controller.addNetworkPolicy(networkPolicy1)
				// TODO: FIX ME
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(err.Error()).To(gomega.ContainSubstring("failed to create Network Policy " +
					"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1: " +
					"failed to create default deny port groups: unexpectedly found multiple results for provided predicate"))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID},
					[]string{namespace2.Name}, nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete peer namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = []libovsdb.TestData{}
				// acls will be deleted, since no namespaces are selected at this point.
				// since test server doesn't garbage-collect de-referenced acls, they will stay in the db
				// gressPolicyExpectedData[2] is the policy port group
				gressPolicyExpectedData[2].(*nbdb.PortGroup).ACLs = nil
				//gressPolicyExpectedData = getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
				//	nil, "", false)
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = []libovsdb.TestData{}
				// acls will be deleted, since no namespaces are selected at this point.
				// since test server doesn't garbage-collect de-referenced acls, they will stay in the db
				// gressPolicyExpectedData[2] is the policy port group
				gressPolicyExpectedData[2].(*nbdb.PortGroup).ACLs = nil
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod referenced by a networkpolicy in its own namespace", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					"", nPodTest.podName, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, nil)

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				gressPolicyExpectedData = getPolicyData(networkPolicy, nil, []string{}, nil, "", false)
				defaultDenyExpectedData = getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData = []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData1 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData2 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

				// After deleting the pod all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gressPolicyExpectedData := getPolicyData(networkPolicy, nil, []string{}, nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, nil, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Update namespace labels
				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// After updating the namespace all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy.Spec.Ingress[0].From[0],
					networkPolicy.Namespace)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				// db data should stay the same
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy.Spec.Ingress[0].From[0], networkPolicy.Namespace)

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)))

				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				gressPolicyExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{},
					nil, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicyExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

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
				time.Sleep(types.OVSDBTimeout + time.Second)

				// check to see if the retry cache has an entry for this policy
				key, err := retry.GetResourceKey(networkPolicy)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				retry.CheckRetryObjectEventually(key, true, fakeOvn.controller.retryNetworkPolicies)
				connCtx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				retry.SetRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryNetworkPolicies.RequestRetryObjs()

				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy.Spec.Ingress[0].From[0], networkPolicy.Namespace)

				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicyExpectedData[:len(gressPolicyExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)))

				// check the cache no longer has the entry
				retry.CheckRetryObjectEventually(key, false, fakeOvn.controller.retryNetworkPolicies)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil,
					[]int32{portNum}, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				gressPolicy1ExpectedData := getPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", false)
				defaultDenyExpectedData := getDefaultDenyData(networkPolicy, []string{nPodTest.portUUID}, "", false)
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, gressPolicy1ExpectedData...)
				expectedData = append(expectedData, defaultDenyExpectedData...)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				acls := []libovsdb.TestData{}
				acls = append(acls, gressPolicy1ExpectedData[:len(gressPolicy1ExpectedData)-1]...)
				acls = append(acls, defaultDenyExpectedData[:len(defaultDenyExpectedData)-2]...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(acls, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

		table.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...*knet.NetworkPolicy) {
			ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
			initialDenyAllPolicy := newNetworkPolicy("emptyPol", namespaceName1, metav1.LabelSelector{}, nil, nil)
			// originalACLLogSeverity.Deny == nbdb.ACLSeverityAlert
			initialExpectedData := getDefaultDenyData(initialDenyAllPolicy, nil, nbdb.ACLSeverityAlert, false)
			initialExpectedData = append(initialExpectedData,
				// no gress policies defined, return only port group
				getPolicyData(initialDenyAllPolicy, nil, []string{}, nil, nbdb.ACLSeverityNotice, false)...)

			app.Action = func(ctx *cli.Context) error {
				startOvn(libovsdb.TestSetup{}, []v1.Namespace{originalNamespace}, []knet.NetworkPolicy{*initialDenyAllPolicy},
					nil, nil)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialExpectedData...))

				var createsPoliciesData []libovsdb.TestData
				// create network policies for given Entry
				for i := range networkPolicies {
					ginkgo.By("Creating new network policy")
					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicies[i].GetNamespace()).
						Create(context.TODO(), networkPolicies[i], metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					createsPoliciesData = append(createsPoliciesData,
						// originalACLLogSeverity.Allow == nbdb.ACLSeverityNotice
						getPolicyData(networkPolicies[i], nil, []string{}, nil, nbdb.ACLSeverityNotice, false)...)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(append(initialExpectedData, createsPoliciesData...)))

				// update namespace log severity
				updatedLogSeverity := nbdb.ACLSeverityDebug
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, updatedLogSeverity, updatedLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				// update expected data log severity
				expectedData := getDefaultDenyData(initialDenyAllPolicy, nil, updatedLogSeverity, false)
				expectedData = append(expectedData,
					// no gress policies defined, return only port group
					getPolicyData(initialDenyAllPolicy, nil, []string{}, nil, updatedLogSeverity, false)...)
				for i := range networkPolicies {
					expectedData = append(expectedData,
						getPolicyData(networkPolicies[i], nil, []string{}, nil, updatedLogSeverity, false)...)
				}

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		},
			table.Entry("when the namespace features a network policy with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false)),
			table.Entry("when the namespace features *multiple* network policies with a single rule",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "", true, false),
				getMatchLabelsNetworkPolicy(netPolicyName2, namespaceName1, namespaceName2, "", false, true)),
			table.Entry("when the namespace features a network policy with *multiple* rules",
				getMatchLabelsNetworkPolicy(netPolicyName1, namespaceName1, namespaceName2, "tiny-winy-pod", true, false)))

		ginkgo.It("policies created after namespace logging level updates inherit updated logging level", func() {
			app.Action = func(ctx *cli.Context) error {
				startOvn(libovsdb.TestSetup{}, []v1.Namespace{originalNamespace}, nil, nil, nil)
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

				var expectedData []libovsdb.TestData
				expectedData = append(expectedData, getPolicyData(newPolicy, nil, []string{}, nil, desiredLogSeverity, false)...)
				expectedData = append(expectedData, getDefaultDenyData(newPolicy, nil, desiredLogSeverity, false)...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
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

var _ = ginkgo.Describe("OVN NetworkPolicy Low-Level Operations", func() {
	var (
		asFactory *addressset.FakeAddressSetFactory
		nbCleanup *libovsdbtest.Cleanup
	)

	const (
		nodeName   = "node1"
		ipv4MgmtIP = "192.168.10.10"
		ipv6MgmtIP = "fd01::1234"
	)

	ginkgo.BeforeEach(func() {
		nbCleanup = nil
	})

	ginkgo.AfterEach(func() {
		if nbCleanup != nil {
			nbCleanup.Cleanup()
		}
	})

	buildExpectedIngressPeerNSv4ACL := func(gp *gressPolicy, pgName string, asDbIDses []*libovsdbops.DbObjectIDs,
		aclLogging *ACLLoggingLevels) *nbdb.ACL {
		name := getGressPolicyACLName(gp.policyNamespace, gp.policyName, gp.idx)
		hashedASNames := []string{}

		for _, dbIdx := range asDbIDses {
			hashedASName, _ := addressset.GetHashNamesForAS(dbIdx)
			hashedASNames = append(hashedASNames, hashedASName)
		}
		asMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.src == {%s}", asMatch)
		if gp.policyType == knet.PolicyTypeIngress {
			match = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", match, "ip4", types.V4OVNServiceHairpinMasqueradeIP, "ip4", asMatch)
		}
		match += fmt.Sprintf(" && outport == @%s", pgName)
		gpDirection := string(knet.PolicyTypeIngress)
		externalIds := map[string]string{
			l4MatchACLExtIdKey:     "None",
			ipBlockCIDRACLExtIdKey: "false",
			namespaceACLExtIdKey:   gp.policyNamespace,
			policyACLExtIdKey:      gp.policyName,
			policyTypeACLExtIdKey:  gpDirection,
			gpDirection + "_num":   fmt.Sprintf("%d", gp.idx),
		}
		acl := libovsdbops.BuildACL(name, nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match,
			nbdb.ACLActionAllowRelated, types.OvnACLLoggingMeter, aclLogging.Allow, true, externalIds, nil)
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
		gp := newGressPolicy(knet.PolicyTypeIngress, 0, "testing", "policy", controllerName)
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
		defaultAclLogging := ACLLoggingLevels{
			nbdb.ACLSeverityInfo,
			nbdb.ACLSeverityInfo,
		}

		gomega.Expect(gp.addNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected := buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one}, &defaultAclLogging)
		actual, _ := gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, one, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, three, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey), asFactory)).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, two, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, five, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four, six}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs, four}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeTrue())
		expected = buildExpectedIngressPeerNSv4ACL(gp, pgName, []*libovsdbops.DbObjectIDs{
			asIDs}, &defaultAclLogging)
		actual, _ = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four.GetObjectID(libovsdbops.ObjectNameKey))).To(gomega.BeFalse())
	})

	ginkgo.It("Tests AddAllowACLFromNode", func() {
		ginkgo.By("adding an existing ACL to the node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
					&nbdb.ACL{
						Match:     "ip4.src==" + ipv4MgmtIP,
						Priority:  types.DefaultAllowPriority,
						Action:    "allow-related",
						Direction: nbdb.ACLDirectionToLport,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv4MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv4MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})

		ginkgo.By("creating an ipv4 ACL and adding it to node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv4MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv4MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
		ginkgo.By("creating an ipv6 ACL and adding it to node switch", func() {
			initialNbdb := libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name: nodeName,
					},
				},
			}

			var err error
			var nbClient libovsdbclient.Client
			nbClient, nbCleanup, err = libovsdbtest.NewNBTestHarness(initialNbdb, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			err = addAllowACLFromNode(nodeName, ovntest.MustParseIP(ipv6MgmtIP), nbClient)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			testNode, nodeACL := generateAllowFromNodeData(nodeName, ipv6MgmtIP)
			expectedData := []libovsdb.TestData{
				testNode,
				nodeACL,
			}
			gomega.Expect(nbClient).Should(libovsdb.HaveData(expectedData...))
		})
	})
})

func generateAllowFromNodeData(nodeName, mgmtIP string) (nodeSwitch *nbdb.LogicalSwitch, acl *nbdb.ACL) {
	var ipFamily = "ip4"
	if utilnet.IsIPv6(ovntest.MustParseIP(mgmtIP)) {
		ipFamily = "ip6"
	}

	match := fmt.Sprintf("%s.src==%s", ipFamily, mgmtIP)

	nodeACL := libovsdbops.BuildACL(getAllowFromNodeACLName(), nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match, "allow-related", types.OvnACLLoggingMeter, "", false, nil, nil)
	nodeACL.UUID = "nodeACL-UUID"

	testNode := &nbdb.LogicalSwitch{
		UUID: nodeName + "-UUID",
		Name: nodeName,
		ACLs: []string{nodeACL.UUID},
	}

	return testNode, nodeACL
}
