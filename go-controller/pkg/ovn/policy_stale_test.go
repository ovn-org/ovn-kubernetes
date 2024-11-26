package ovn

import (
	"context"
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Legacy const, should only be used in sync and tests
const (
	// arpAllowPolicySuffix is the suffix used when creating default ACLs for a namespace
	arpAllowPolicySuffix = "ARPallowPolicy"
)

func getStaleDefaultDenyACL(netpolName, namespace, match string, deny, egress bool) *nbdb.ACL {
	fakeController := getFakeBaseController(&util.DefaultNetInfo{})
	direction := libovsdbutil.ACLEgress
	if !egress {
		direction = libovsdbutil.ACLIngress
	}
	aclIDs := fakeController.getDefaultDenyPolicyACLIDs(namespace, direction, defaultDenyACL)
	priority := types.DefaultDenyPriority
	action := nbdb.ACLActionDrop
	// stale deny name
	name := namespace + "_" + netpolName
	if !deny {
		aclIDs = fakeController.getDefaultDenyPolicyACLIDs(namespace, direction, arpAllowACL)
		priority = types.DefaultAllowPriority
		action = nbdb.ACLActionAllow
		name = getStaleARPAllowACLName(namespace)
	}
	acl := libovsdbops.BuildACL(
		name,
		nbdb.ACLDirectionToLport,
		priority,
		match,
		action,
		types.OvnACLLoggingMeter,
		"",
		false,
		aclIDs.GetExternalIDs(),
		nil,
		types.PrimaryACLTier,
	)
	acl.UUID = aclIDs.String() + "-UUID"
	return acl
}

func getStaleARPAllowACLName(ns string) string {
	return libovsdbutil.JoinACLName(ns, arpAllowPolicySuffix)
}

// getStaleDefaultDenyData builds stale ACLs and port groups for given netpol
func getStaleDefaultDenyData(networkPolicy *knet.NetworkPolicy) []libovsdbtest.TestData {
	namespace := networkPolicy.Namespace
	netpolName := networkPolicy.Name
	fakeController := getFakeBaseController(&util.DefaultNetInfo{})

	egressPGName := fakeController.defaultDenyPortGroupName(namespace, libovsdbutil.ACLEgress)
	egressDenyACL := getStaleDefaultDenyACL(netpolName, namespace, "inport == @"+egressPGName, true, true)
	egressAllowACL := getStaleDefaultDenyACL(netpolName, namespace, "inport == @"+egressPGName+" && "+arpAllowPolicyMatch, false, true)

	ingressPGName := fakeController.defaultDenyPortGroupName(namespace, libovsdbutil.ACLIngress)
	ingressDenyACL := getStaleDefaultDenyACL(netpolName, namespace, "outport == @"+ingressPGName, true, false)
	ingressAllowACL := getStaleDefaultDenyACL(netpolName, namespace, "outport == @"+ingressPGName+" && "+arpAllowPolicyMatch, false, false)

	egressDenyPG := libovsdbutil.BuildPortGroup(
		fakeController.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLEgress),
		nil,
		[]*nbdb.ACL{egressDenyACL, egressAllowACL},
	)
	egressDenyPG.UUID = egressDenyPG.Name + "-UUID"

	ingressDenyPG := libovsdbutil.BuildPortGroup(
		fakeController.getDefaultDenyPolicyPortGroupIDs(namespace, libovsdbutil.ACLIngress),
		nil,
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

// getStalePolicyACLs builds stale ACLs for given peers
func getStalePolicyACLs(gressIdx int, namespace, policyName string, peerNamespaces []string,
	peers []knet.NetworkPolicyPeer, policyType knet.PolicyType, netInfo util.NetInfo) []*nbdb.ACL {
	fakeController := getFakeBaseController(netInfo)
	pgName := fakeController.getNetworkPolicyPGName(namespace, policyName)
	controllerName := netInfo.GetNetworkName() + "-network-controller"
	var portDir string
	var ipDir string
	acls := []*nbdb.ACL{}
	if policyType == knet.PolicyTypeEgress {
		portDir = "inport"
		ipDir = "dst"
	} else {
		portDir = "outport"
		ipDir = "src"
	}
	hashedASNames := []string{}
	for _, nsName := range peerNamespaces {
		hashedASName, _ := getMultinetNsAddrSetHashNames(nsName, controllerName)
		hashedASNames = append(hashedASNames, hashedASName)
	}
	for _, peer := range peers {
		// follow the algorithm from setupGressPolicy
		if !useNamespaceAddrSet(peer) {
			podSelector := peer.PodSelector
			if podSelector == nil {
				// nil pod selector is equivalent to empty pod selector, which selects all
				podSelector = &metav1.LabelSelector{}
			}
			peerIndex := getPodSelectorAddrSetDbIDs(getPodSelectorKey(podSelector, peer.NamespaceSelector, namespace), controllerName)
			asv4, _ := addressset.GetHashNamesForAS(peerIndex)
			hashedASNames = append(hashedASNames, asv4)
		}
	}
	gp := gressPolicy{
		policyNamespace: namespace,
		policyName:      policyName,
		policyType:      policyType,
		idx:             gressIdx,
		controllerName:  controllerName,
	}
	if len(hashedASNames) > 0 {
		gressAsMatch := asMatch(hashedASNames)
		match := fmt.Sprintf("ip4.%s == {%s} && %s == @%s", ipDir, gressAsMatch, portDir, pgName)
		action := nbdb.ACLActionAllowRelated
		dbIDs := gp.getNetpolACLDbIDs(emptyIdx, libovsdbutil.UnspecifiedL4Protocol)
		staleName := namespace + "_" + policyName + "_0"
		// build the most ancient ACL version, which includes
		// - old name
		// - no options for Egress ACLs
		// - wrong direction for egress ACLs
		// - non-nil Severity when Log is false
		acl := libovsdbops.BuildACL(
			staleName,
			nbdb.ACLDirectionToLport,
			types.DefaultAllowPriority,
			match,
			action,
			types.OvnACLLoggingMeter,
			nbdb.ACLSeverityInfo,
			false,
			dbIDs.GetExternalIDs(),
			nil,
			types.PrimaryACLTier,
		)
		acl.UUID = dbIDs.String() + "-UUID"
		acls = append(acls, acl)
	}
	return acls
}

func getStalePolicyData(networkPolicy *knet.NetworkPolicy, peerNamespaces []string) []libovsdbtest.TestData {
	netInfo := &util.DefaultNetInfo{}
	acls := []*nbdb.ACL{}

	for i, ingress := range networkPolicy.Spec.Ingress {
		acls = append(acls, getStalePolicyACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, ingress.From, knet.PolicyTypeIngress, netInfo)...)
	}
	for i, egress := range networkPolicy.Spec.Egress {
		acls = append(acls, getStalePolicyACLs(i, networkPolicy.Namespace, networkPolicy.Name,
			peerNamespaces, egress.To, knet.PolicyTypeEgress, netInfo)...)
	}

	fakeController := getFakeBaseController(netInfo)
	pgDbIDs := fakeController.getNetworkPolicyPortGroupDbIDs(networkPolicy.Namespace, networkPolicy.Name)
	pg := libovsdbutil.BuildPortGroup(
		pgDbIDs,
		nil,
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

var _ = ginkgo.Describe("OVN Stale NetworkPolicy Operations", func() {
	const (
		namespaceName1 = "namespace1"
		namespaceName2 = "namespace2"
		netPolicyName1 = "networkpolicy1"
	)
	var (
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()
		fakeOvn = NewFakeOVN(true)

		initialData := getHairpinningACLsV4AndPortGroup()
		initialDB = libovsdbtest.TestSetup{
			NBData: initialData,
		}
	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	startOvn := func(dbSetup libovsdbtest.TestSetup, namespaces []v1.Namespace, networkPolicies []knet.NetworkPolicy) {
		fakeOvn.startWithDBSetup(dbSetup,
			&v1.NamespaceList{
				Items: namespaces,
			},
			&knet.NetworkPolicyList{
				Items: networkPolicies,
			},
		)
		var err error
		if namespaces != nil {
			err = fakeOvn.controller.WatchNamespaces()
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		}
		err = fakeOvn.controller.WatchNetworkPolicy()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles an existing networkPolicy updating stale ACLs", func() {
			namespace1 := *newNamespace(namespaceName1)
			namespace2 := *newNamespace(namespaceName2)
			networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
				namespace2.Name, "", true, true)
			// start with stale ACLs
			gressPolicyInitialData := getStalePolicyData(networkPolicy, []string{namespace2.Name})
			defaultDenyInitialData := getStaleDefaultDenyData(networkPolicy)
			initialData := initialDB.NBData
			initialData = append(initialData, gressPolicyInitialData...)
			initialData = append(initialData, defaultDenyInitialData...)
			startOvn(libovsdbtest.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
				[]knet.NetworkPolicy{*networkPolicy})

			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

			_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
				Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// make sure stale ACLs were updated
			expectedData := getNamespaceWithSinglePolicyExpectedData(
				newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name),
				initialDB.NBData)
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})

		ginkgo.It("reconciles an existing networkPolicy updating stale ACLs with long names", func() {
			longNamespaceName63 := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk" // longest allowed namespace name
			namespace1 := *newNamespace(longNamespaceName63)
			namespace2 := *newNamespace(namespaceName2)
			networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
				namespace2.Name, "", true, true)
			// start with stale ACLs
			gressPolicyInitialData := getStalePolicyData(networkPolicy, []string{namespace2.Name})
			defaultDenyInitialData := getStaleDefaultDenyData(networkPolicy)
			initialData := initialDB.NBData
			initialData = append(initialData, gressPolicyInitialData...)
			initialData = append(initialData, defaultDenyInitialData...)
			startOvn(libovsdbtest.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
				[]knet.NetworkPolicy{*networkPolicy})

			fakeOvn.asf.ExpectEmptyAddressSet(longNamespaceName63)
			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

			_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
				Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// make sure stale ACLs were updated
			expectedData := getNamespaceWithSinglePolicyExpectedData(
				newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name),
				initialDB.NBData)
			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})

		ginkgo.It("reconciles an existing networkPolicy updating stale address sets", func() {
			namespace1 := *newNamespace(namespaceName1)
			namespace2 := *newNamespace(namespaceName2)
			networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
				namespace2.Name, "", false, true)
			// start with stale ACLs
			staleAddrSetIDs := getStaleNetpolAddrSetDbIDs(networkPolicy.Namespace, networkPolicy.Name,
				"egress", "0", DefaultNetworkControllerName)
			localASName, _ := addressset.GetHashNamesForAS(staleAddrSetIDs)
			peerASName, _ := getDefaultNetNsAddrSetHashNames(namespace2.Name)
			fakeController := getFakeController(DefaultNetworkControllerName)
			pgName := fakeController.getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
			initialData := getPolicyData(newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name))
			staleACL := initialData[0].(*nbdb.ACL)
			staleACL.Match = fmt.Sprintf("ip4.dst == {$%s, $%s} && inport == @%s", localASName, peerASName, pgName)

			defaultDenyInitialData := getStaleDefaultDenyData(networkPolicy)
			initialData = append(initialData, defaultDenyInitialData...)
			initialData = append(initialData, initialDB.NBData...)
			_, err := fakeOvn.asf.NewAddressSet(staleAddrSetIDs, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			startOvn(libovsdbtest.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
				[]knet.NetworkPolicy{*networkPolicy})

			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
			fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

			// make sure stale ACLs were updated
			expectedData := getNamespaceWithSinglePolicyExpectedData(
				newNetpolDataParams(networkPolicy).withPeerNamespaces(namespace2.Name),
				initialDB.NBData)
			fakeOvn.asf.ExpectEmptyAddressSet(staleAddrSetIDs)

			gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
		})
	})
})
