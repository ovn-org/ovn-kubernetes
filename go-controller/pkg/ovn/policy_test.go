package ovn

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

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

	"github.com/urfave/cli/v2"

	v1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilnet "k8s.io/utils/net"
	utilpointer "k8s.io/utils/pointer"
)

func getNetworkPolicyPGName(namespace, name string) (pgName, readablePGName string) {
	readableGroupName := fmt.Sprintf("%s_%s", namespace, name)
	return hashedPortGroup(readableGroupName), readableGroupName
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

func legacyGetDefaultData(networkPolicy *knet.NetworkPolicy, ports []string,
	denyLogSeverity nbdb.ACLSeverity, stale bool) []libovsdb.TestData {
	egressPGName := legacyDefaultDenyPortGroupName(networkPolicy.Namespace, egressDefaultDenySuffix)
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

	ingressPGName := legacyDefaultDenyPortGroupName(networkPolicy.Namespace, ingressDefaultDenySuffix)
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

func getGressACLs(i int, namespace, policyName, pgName string, peerNamespaces []string, tcpPeerPorts []int32,
	logSeverity nbdb.ACLSeverity, policyType knet.PolicyType, stale bool) []*nbdb.ACL {
	aclName := getGressPolicyACLName(namespace, policyName, i)
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
	if peerNamespaces != nil {
		gressAsMatch := asMatch(append(peerNamespaces, getAddressSetName(namespace, policyName, policyType, i)))
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
				namespaceExtIdKey:           namespace,
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
				namespaceExtIdKey:           namespace,
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

func legacyGetPolicyData(networkPolicy *knet.NetworkPolicy, policyPorts []string, peerNamespaces []string,
	tcpPeerPorts []int32, allowLogSeverity nbdb.ACLSeverity, stale bool) []libovsdbtest.TestData {
	acls := []*nbdb.ACL{}

	pgName, _ := getNetworkPolicyPGName(networkPolicy.Namespace, networkPolicy.Name)
	for i := range networkPolicy.Spec.Ingress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name, pgName,
			peerNamespaces, tcpPeerPorts, allowLogSeverity, knet.PolicyTypeIngress, stale)...)
	}
	for i := range networkPolicy.Spec.Egress {
		acls = append(acls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name, pgName,
			peerNamespaces, tcpPeerPorts, allowLogSeverity, knet.PolicyTypeEgress, stale)...)
	}

	lsps := []*nbdb.LogicalSwitchPort{}
	for _, uuid := range policyPorts {
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

type pgArgType struct {
	namespace string
	hashName  string
	name      string

	// leftover acls. TODO: test server does not garbage collect ACLs, so we embed leftover policies here
	// key is either namespace/name for network policy acls or egressDefaultDenySuffix ingressDefaultDenySuffix
	// for default deny acls.
	leftoverAcls     map[string][]*nbdb.ACL
	policyAcls       map[string][]*nbdb.ACL
	policyPorts      []string
	ingressAcls      []*nbdb.ACL
	egressAcls       []*nbdb.ACL
	lspEgressRefCnt  int
	lspIngressRefCnt int
	deleted          bool
}

func getSharedPortGroupPolicyData(pgArgMap map[string]*pgArgType) []libovsdb.TestData {
	data := []libovsdb.TestData{}
	for _, pgArg := range pgArgMap {
		// add left over acls that were not deleted when they are removed from portgroup or when portgroup itself is deleted
		for _, acls := range pgArg.leftoverAcls {
			for _, acl := range acls {
				data = append(data, acl)
			}
		}

		if pgArg.deleted {
			continue
		}

		lsps := []*nbdb.LogicalSwitchPort{}
		for _, uuid := range pgArg.policyPorts {
			lsps = append(lsps, &nbdb.LogicalSwitchPort{UUID: uuid})
		}
		allAcls := []*nbdb.ACL{}
		for _, acls := range pgArg.policyAcls {
			allAcls = append(allAcls, acls...)
		}
		if pgArg.lspEgressRefCnt > 0 {
			allAcls = append(allAcls, pgArg.egressAcls...)
		}
		if pgArg.lspIngressRefCnt > 0 {
			allAcls = append(allAcls, pgArg.ingressAcls...)
		}

		pg := libovsdbops.BuildPortGroup(
			pgArg.hashName,
			pgArg.name,
			lsps,
			allAcls,
		)
		pg.UUID = pg.Name + "-UUID"
		pg.ExternalIDs[sharePortGroupExtIdKey] = pgArg.name
		pg.ExternalIDs[namespaceExtIdKey] = pgArg.namespace

		for _, acl := range allAcls {
			data = append(data, acl)
		}
		data = append(data, pg)
	}
	return data
}

func addPolicyData(networkPolicy *knet.NetworkPolicy, policyPorts []string, peerNamespaces []string, tcpPeerPorts []int32, denyLogSeverity, allowLogSeverity nbdb.ACLSeverity, pgArgMap map[string]*pgArgType) []libovsdb.TestData {
	pgName := getSharedPortGroupName(networkPolicy)
	pgHash := hashedPortGroup(pgName)
	pgArg, ok := pgArgMap[pgHash]
	if !ok || pgArg.deleted {
		leftoverAcls := map[string][]*nbdb.ACL{}
		if ok && pgArg.deleted {
			leftoverAcls = pgArg.leftoverAcls
		}
		egressAcls, ingressAcls := getSharedNMDefaultDenyAcls(pgName, denyLogSeverity, allowLogSeverity)
		newPgArg := &pgArgType{
			namespace:        networkPolicy.Namespace,
			hashName:         pgHash,
			name:             pgName,
			leftoverAcls:     leftoverAcls,
			policyAcls:       map[string][]*nbdb.ACL{},
			ingressAcls:      ingressAcls,
			egressAcls:       egressAcls,
			lspEgressRefCnt:  0,
			lspIngressRefCnt: 0,
			deleted:          false,
		}
		pgArgMap[pgHash] = newPgArg
		pgArg = newPgArg
	}
	pgArg.policyPorts = policyPorts

	if _, ok := pgArg.policyAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]; !ok {
		policyTypeIngress, policyTypeEgress := getPolicyType(networkPolicy)
		if policyTypeIngress {
			pgArg.lspIngressRefCnt++
			delete(pgArg.leftoverAcls, ingressDefaultDenySuffix)
		}
		if policyTypeEgress {
			pgArg.lspEgressRefCnt++
			delete(pgArg.leftoverAcls, egressDefaultDenySuffix)
		}
	}
	policyAcls := []*nbdb.ACL{}

	for i := range networkPolicy.Spec.Ingress {
		policyAcls = append(policyAcls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name, pgHash,
			peerNamespaces, tcpPeerPorts, allowLogSeverity, knet.PolicyTypeIngress, false)...)
	}
	for i := range networkPolicy.Spec.Egress {
		policyAcls = append(policyAcls, getGressACLs(i, networkPolicy.Namespace, networkPolicy.Name, pgHash,
			peerNamespaces, tcpPeerPorts, allowLogSeverity, knet.PolicyTypeEgress, false)...)
	}

	pgArg.policyAcls[networkPolicy.Namespace+"_"+networkPolicy.Name] = policyAcls
	leftoverAcls := pgArg.leftoverAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]
	newLeftoverAcls := []*nbdb.ACL{}
	for _, leftoverAcl := range leftoverAcls {
		foundEqual := false
		for _, policyAcl := range policyAcls {
			if reflect.DeepEqual(*policyAcl, *leftoverAcl) {
				foundEqual = true
				break
			}
		}
		if !foundEqual {
			newLeftoverAcls = append(newLeftoverAcls, leftoverAcl)
		}
	}
	if len(newLeftoverAcls) == 0 {
		delete(pgArg.leftoverAcls, networkPolicy.Namespace+"_"+networkPolicy.Name)
	} else {
		pgArg.leftoverAcls[networkPolicy.Namespace+"_"+networkPolicy.Name] = newLeftoverAcls
	}
	return getSharedPortGroupPolicyData(pgArgMap)
}

func delPolicyData(networkPolicy *knet.NetworkPolicy, pgArgMap map[string]*pgArgType) []libovsdb.TestData {
	pgName := getSharedPortGroupName(networkPolicy)
	pgHash := hashedPortGroup(pgName)
	pgArg, ok := pgArgMap[pgHash]
	if ok {
		policyTypeIngress, policyTypeEgress := getPolicyType(networkPolicy)
		if _, ok := pgArg.policyAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]; ok {
			if policyTypeEgress {
				pgArg.lspEgressRefCnt--
				if pgArg.lspEgressRefCnt == 0 {
					pgArg.leftoverAcls[egressDefaultDenySuffix] = pgArg.egressAcls
				}
			}
			if policyTypeIngress {
				pgArg.lspIngressRefCnt--
				if pgArg.lspIngressRefCnt == 0 {
					pgArg.leftoverAcls[ingressDefaultDenySuffix] = pgArg.ingressAcls
				}
			}
		}

		leftoverAcls, ok := pgArg.leftoverAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]
		if !ok {
			pgArg.leftoverAcls[networkPolicy.Namespace+"_"+networkPolicy.Name] = pgArg.policyAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]
		} else {
			leftoverAcls = append(leftoverAcls, pgArg.policyAcls[networkPolicy.Namespace+"_"+networkPolicy.Name]...)
			pgArg.leftoverAcls[networkPolicy.Namespace+"_"+networkPolicy.Name] = leftoverAcls
		}
		delete(pgArg.policyAcls, networkPolicy.Namespace+"_"+networkPolicy.Name)

		if len(pgArg.policyAcls) == 0 {
			// when the portGroup is deleted, don't remove it from pgArgMap, as we'd need its leftoverAcls.
			pgArg.deleted = true
		}
	}

	return getSharedPortGroupPolicyData(pgArgMap)
}

func getSharedNMDefaultDenyAcls(readableSharedPortGroupName string, denylogSeverity, allowlogSeverity nbdb.ACLSeverity) ([]*nbdb.ACL, []*nbdb.ACL) {
	direction := nbdb.ACLDirectionFromLport
	options := map[string]string{
		"apply-after-lb": "true",
	}
	egressAcls := []*nbdb.ACL{}
	ingressAcls := []*nbdb.ACL{}
	pgHashName := hashedPortGroup(readableSharedPortGroupName)
	egressDenyACL := libovsdbops.BuildACL(
		pgHashName+"_"+"egressDefaultDeny",
		direction,
		types.DefaultDenyPriority,
		"inport == @"+pgHashName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denylogSeverity,
		denylogSeverity != "",
		map[string]string{
			sharePortGroupExtIdKey:           pgHashName,
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		options,
	)
	egressDenyACL.UUID = *egressDenyACL.Name + "-egressDenyACL-UUID"
	egressAcls = append(egressAcls, egressDenyACL)

	egressAllowACL := libovsdbops.BuildACL(
		pgHashName+"_ARPallowPolicy",
		direction,
		types.DefaultAllowPriority,
		"inport == @"+pgHashName+" && (arp || nd)",
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		allowlogSeverity,
		allowlogSeverity != "",
		map[string]string{
			sharePortGroupExtIdKey:           pgHashName,
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
		},
		map[string]string{
			"apply-after-lb": "true",
		},
	)
	egressAllowACL.UUID = *egressAllowACL.Name + "-egressAllowACL-UUID"
	egressAcls = append(egressAcls, egressAllowACL)

	ingressDenyACL := libovsdbops.BuildACL(
		pgHashName+"_"+"ingressDefaultDeny",
		nbdb.ACLDirectionToLport,
		types.DefaultDenyPriority,
		"outport == @"+pgHashName,
		nbdb.ACLActionDrop,
		types.OvnACLLoggingMeter,
		denylogSeverity,
		denylogSeverity != "",
		map[string]string{
			sharePortGroupExtIdKey:           pgHashName,
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressDenyACL.UUID = *ingressDenyACL.Name + "-ingressDenyACL-UUID"
	ingressAcls = append(ingressAcls, ingressDenyACL)

	ingressAllowACL := libovsdbops.BuildACL(
		pgHashName+"_ARPallowPolicy",
		nbdb.ACLDirectionToLport,
		types.DefaultAllowPriority,
		"outport == @"+pgHashName+" && (arp || nd)",
		nbdb.ACLActionAllow,
		types.OvnACLLoggingMeter,
		allowlogSeverity,
		allowlogSeverity != "",
		map[string]string{
			sharePortGroupExtIdKey:           pgHashName,
			defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeIngress),
		},
		nil,
	)
	ingressAllowACL.UUID = *ingressAllowACL.Name + "-ingressAllowACL-UUID"
	ingressAcls = append(ingressAcls, ingressAllowACL)
	return egressAcls, ingressAcls
}

func getAddressSetName(namespace, name string, policyType knet.PolicyType, idx int) string {
	direction := strings.ToLower(string(policyType))
	return fmt.Sprintf("%s.%s.%s.%d", namespace, name, direction, idx)
}

func eventuallyExpectNoAddressSets(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectNoAddressSet(asName)
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectNoAddressSet(asName)
	}
}

func eventuallyExpectAddressSetsWithIP(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy, ip string) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(asName, []string{ip})
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectAddressSetWithIPs(asName, []string{ip})
	}
}

func eventuallyExpectEmptyAddressSetsExist(fakeOvn *FakeOVN, networkPolicy *knet.NetworkPolicy) {
	for i := range networkPolicy.Spec.Ingress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeIngress, i)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(asName)
	}
	for i := range networkPolicy.Spec.Egress {
		asName := getAddressSetName(networkPolicy.Namespace, networkPolicy.Name, knet.PolicyTypeEgress, i)
		fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(asName)
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

func getPortNetworkPolicy(policyName, namespace, labelName, labelVal string, tcpPort int32, ingress, egress bool) *knet.NetworkPolicy {
	tcpProtocol := v1.ProtocolTCP
	var ingressRules []knet.NetworkPolicyIngressRule
	if ingress {
		ingressRules = []knet.NetworkPolicyIngressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
			}},
		}}
	}
	var egressRules []knet.NetworkPolicyEgressRule
	if egress {
		egressRules = []knet.NetworkPolicyEgressRule{{
			Ports: []knet.NetworkPolicyPort{{
				Port:     &intstr.IntOrString{IntVal: tcpPort},
				Protocol: &tcpProtocol,
			}},
		}}
	}
	return newNetworkPolicy(policyName, namespace,
		metav1.LabelSelector{
			MatchLabels: map[string]string{
				labelName: labelVal,
			},
		},
		ingressRules,
		egressRules,
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
		pgMap     map[string]*pgArgType

		gomegaFormatMaxLength int
		logicalSwitch         *nbdb.LogicalSwitch
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		pgMap = map[string]*pgArgType{}

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
		ginkgo.It("only cleans up address sets owned by network policy", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				// namespace-owned address set, should stay
				fakeOvn.asf.NewAddressSet("namespace", []net.IP{net.ParseIP("1.1.1.1")})
				// netpol-owned address set for existing netpol, should stay
				fakeOvn.asf.NewAddressSet(fmt.Sprintf("namespace1.%s.egress.0", netPolicyName1), []net.IP{net.ParseIP("1.1.1.2")})
				// netpol-owned address set for non-exisitng netpol, should be deleted
				fakeOvn.asf.NewAddressSet("namespace1.not-existing.egress.0", []net.IP{net.ParseIP("1.1.1.3")})
				// egressQoS-owned address set, should stay
				fakeOvn.asf.NewAddressSet(types.EgressQoSRulePrefix+"namespace", []net.IP{net.ParseIP("1.1.1.4")})
				// hybridNode-owned address set, should stay
				fakeOvn.asf.NewAddressSet(types.HybridRoutePolicyPrefix+"node", []net.IP{net.ParseIP("1.1.1.5")})
				// egress firewall-owned address set, should stay
				fakeOvn.asf.NewAddressSet("test.dns.name", []net.IP{net.ParseIP("1.1.1.6")})

				fakeOvn.start()
				err := fakeOvn.controller.syncNetworkPolicies([]interface{}{networkPolicy})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.asf.ExpectAddressSetWithIPs("namespace", []string{"1.1.1.1"})
				fakeOvn.asf.ExpectAddressSetWithIPs(fmt.Sprintf("namespace1.%s.egress.0", netPolicyName1), []string{"1.1.1.2"})
				fakeOvn.asf.EventuallyExpectNoAddressSet("namespace1.not-existing.egress.0")
				fakeOvn.asf.ExpectAddressSetWithIPs(types.EgressQoSRulePrefix+"namespace", []string{"1.1.1.4"})
				fakeOvn.asf.ExpectAddressSetWithIPs(types.HybridRoutePolicyPrefix+"node", []string{"1.1.1.5"})
				fakeOvn.asf.ExpectAddressSetWithIPs("test.dns.name", []string{"1.1.1.6"})
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
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				policyExpectedData := addPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, "", "", pgMap)
				expectedData := initialDB.NBData
				expectedData = append(expectedData, policyExpectedData...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("upgrading a networkPolicy with shared port group", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				namespace2 := *newNamespace(namespaceName2)
				networkPolicy := getMatchLabelsNetworkPolicy(netPolicyName1, namespace1.Name,
					namespace2.Name, "", true, true)
				// start with stale ACLs
				gressPolicyInitialData := legacyGetPolicyData(networkPolicy, nil, []string{namespace2.Name},
					nil, "", true)
				defaultDenyInitialData := legacyGetDefaultData(networkPolicy, nil, "", true)
				initialData := []libovsdb.TestData{}
				initialData = append(initialData, gressPolicyInitialData...)
				initialData = append(initialData, defaultDenyInitialData...)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, "", "", pgMap)
				// TODO: test server does not garbage collect ACLs, so we just expect policy to be removed
				// as port groups have been deleted
				defaultDenyInitialData = legacyGetDefaultData(networkPolicy, nil, "", false)
				expectedData = append(expectedData, defaultDenyInitialData[:len(defaultDenyInitialData)-2]...)
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
				initialData := addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
				startOvn(libovsdb.TestSetup{NBData: initialData}, []v1.Namespace{namespace1, namespace2},
					[]knet.NetworkPolicy{*networkPolicy}, nil, nil)

				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName1)
				fakeOvn.asf.ExpectEmptyAddressSet(namespaceName2)

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, "", "", pgMap)
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

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, "", "", pgMap)
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
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("during execution", func() {
		ginkgo.It("correctly creates and deletes a networkpolicy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Create a second NP
				ginkgo.By("Creating and deleting another policy that references that pod")
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1, true, true)

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData2 := addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
				expectedData2 = append(expectedData2, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

				// Delete the second network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Delete(context.TODO(), networkPolicy2.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData3 := delPolicyData(networkPolicy2, pgMap)
				expectedData3 = append(expectedData3, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData3...))

				// Delete the first network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData4 := delPolicyData(networkPolicy, pgMap)
				expectedData4 = append(expectedData4, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData4))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries creating a network policy allowing a port to a local pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
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
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1, true, true)
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

				expectedDataWithPolicy2 := addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
				expectedDataWithPolicy2 = append(expectedDataWithPolicy2, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
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
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, true)
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Check networkPolicy applied to a pod data")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
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
				networkPolicy2 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum+1, true, true)
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

				expectedData = delPolicyData(networkPolicy, pgMap)
				expectedData = addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
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
				networkPolicy := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, true)
				egressOptions := map[string]string{
					"apply-after-lb": "true",
				}
				pgHashName := hashedPortGroup(getSharedPortGroupName(networkPolicy))
				aclName := getARPAllowACLName(pgHashName)
				leftOverACLFromUpgrade1 := libovsdbops.BuildACL(
					aclName,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+pgHashName+" && (arp)", // invalid ACL match; won't be cleaned up
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
						sharePortGroupExtIdKey:           pgHashName,
					},
					egressOptions,
				)
				leftOverACLFromUpgrade1.UUID = *leftOverACLFromUpgrade1.Name + "-egressAllowACL-UUID1"

				aclName = getARPAllowACLName(networkPolicy.Namespace)
				leftOverACLFromUpgrade2 := libovsdbops.BuildACL(
					aclName,
					nbdb.ACLDirectionFromLport,
					types.DefaultAllowPriority,
					"inport == @"+pgHashName+" && "+arpAllowPolicyMatch,
					nbdb.ACLActionAllow,
					types.OvnACLLoggingMeter,
					"",
					false,
					map[string]string{
						defaultDenyPolicyTypeACLExtIdKey: string(knet.PolicyTypeEgress),
						sharePortGroupExtIdKey:           pgHashName,
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
					"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1: failed to create network local policies"))
				// ensure the default PGs and ACLs were removed via rollback from add failure
				expectedData := []libovsdb.TestData{}
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				// note stale leftovers from previous upgrades won't be cleanedup
				expectedData = append(expectedData, leftOverACLFromUpgrade1, leftOverACLFromUpgrade2)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				ginkgo.By("Deleting the network policy that failed to create and ensuring we don't panic")
				err = fakeOvn.controller.deleteNetworkPolicy(networkPolicy)
				// I1207 policy.go:1093] Deleting network policy abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1
				// I1207 policy.go:1106] Deleting policy abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1 that is already deleted
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
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, true)
				// This is not yet going to be created
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1, true, true)
				egressPGName := legacyDefaultDenyPortGroupName("leftover1", egressDefaultDenySuffix)
				ingressPGName := legacyDefaultDenyPortGroupName("leftover1", ingressDefaultDenySuffix)
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

				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy1},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy1, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
				expectedData = addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				egressOptions = map[string]string{
					"apply-after-lb": "true",
				}
				// since test server doesn't garbage collect dereferenced acls, they will stay in the test db after they
				// were deleted. Even though they are derefenced from the port group at this point, they will be updated
				// as all the other ACLs.
				// Update deleted leftOverACL1FromUpgrade and leftOverACL2FromUpgrade to match on expected data
				// Once our test server can delete such acls, this part should be deleted
				// start of db hack
				leftOverACL3FromUpgrade.Options = egressOptions
				newDefaultDenyEgressACLName := "youknownothingjonsnowyouknownothingjonsnowyouknownothingjonsnow" // trims it according to RFC1123
				leftOverACL3FromUpgrade.Name = &newDefaultDenyEgressACLName
				expectedData = append(expectedData, leftOverACL3FromUpgrade)
				expectedData = append(expectedData, leftOverACL4FromUpgrade)
				leftOverACL1FromUpgrade.Options = egressOptions
				expectedData = append(expectedData, leftOverACL2FromUpgrade)
				expectedData = append(expectedData, leftOverACL1FromUpgrade)
				// end of db hack

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
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, longNamespace.Name, labelName, labelVal, portNum, true, true)
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, longNamespace.Name, labelName, labelVal, portNum+1, true, true)

				longLeftOverNameSpaceName := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"  // namespace is >45 characters long
				longLeftOverNameSpaceName2 := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxy1" // namespace is >45 characters long
				egressPGName := legacyDefaultDenyPortGroupName(longLeftOverNameSpaceName, egressDefaultDenySuffix)
				ingressPGName := legacyDefaultDenyPortGroupName(longLeftOverNameSpaceName, ingressDefaultDenySuffix)
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

				startOvn(initialDB, []v1.Namespace{longNamespace}, []knet.NetworkPolicy{*networkPolicy1},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				ginkgo.By("Creating a network policy that applies to a pod")
				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Get(context.TODO(), networkPolicy1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(longNamespace.Name, []string{nPodTest.podIP})
				expectedData := addPolicyData(networkPolicy1, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)

				// Create a second NP
				ginkgo.By("Creating another policy that references that pod")
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Create(context.TODO(), networkPolicy2, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Get(context.TODO(), networkPolicy2.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData = addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				egressOptions := map[string]string{
					"apply-after-lb": "true",
				}
				// since test server doesn't garbage collect dereferenced acls, they will stay in the test db after they
				// were deleted. Even though they are derefenced from the port group at this point, they will be updated
				// as all the other ACLs.
				// Update deleted leftOverACL1FromUpgrade and leftOverACL2FromUpgrade to match on expected data
				// Once our test server can delete such acls, this part should be deleted
				// start of db hack
				leftOverACL4FromUpgrade.Options = egressOptions
				leftOverACL3FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName + "blah_ARPall")  // trims it according to RFC1123
				leftOverACL4FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName2 + "_networkpol") // trims it according to RFC1123
				leftOverACL5FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName2 + "_networkpol") // trims it according to RFC1123
				leftOverACL6FromUpgrade.Name = utilpointer.StringPtr(longLeftOverNameSpaceName62 + "_")          // name stays the same here since its no-op
				expectedData = append(expectedData, leftOverACL3FromUpgrade)
				expectedData = append(expectedData, leftOverACL4FromUpgrade)
				expectedData = append(expectedData, leftOverACL5FromUpgrade)
				expectedData = append(expectedData, leftOverACL6FromUpgrade)

				longLeftOverIngressName := longLeftOverNameSpaceName + "_ARPallowPo"
				longLeftOverEgressName := longLeftOverNameSpaceName + "_ARPallowPo"
				leftOverACL2FromUpgrade.Name = &longLeftOverIngressName
				leftOverACL1FromUpgrade.Name = &longLeftOverEgressName
				leftOverACL1FromUpgrade.Options = egressOptions
				expectedData = append(expectedData, leftOverACL2FromUpgrade)
				expectedData = append(expectedData, leftOverACL1FromUpgrade)
				// end of db hack
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// in stable state, after all stale acls were updated and cleanedup invoke sync again to re-enact a master restart
				ginkgo.By("Trigger another syncNetworkPolicies run and ensure nothing has changed in the DB")
				fakeOvn.controller.syncNetworkPolicies(nil)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				//???
				//ginkgo.By("Simulate the initial re-add of all network policies during upgrade and ensure we are stable")
				//fakeOvn.controller.networkPolicies.Delete(getPolicyKey(networkPolicy1))
				//err = fakeOvn.controller.addNetworkPolicy(networkPolicy1)
				//// TODO: FIX ME
				//gomega.Expect(err).To(gomega.HaveOccurred())
				//gomega.Expect(err.Error()).To(gomega.ContainSubstring("failed to create Network Policy " +
				//	"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijk/networkpolicy1: " +
				//	"failed to create default deny port groups: unexpectedly found multiple results for provided predicate"))
				//
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("updates shared port group and acls for a deleted policy in syncNetworkPolicies", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace(namespaceName1)
				nPodTest := getTestPod(namespace1.Name, nodeName)
				networkPolicy1 := getPortNetworkPolicy(netPolicyName1, namespace1.Name, labelName, labelVal, portNum, true, false)
				networkPolicy2 := getPortNetworkPolicy(netPolicyName2, namespace1.Name, labelName, labelVal, portNum+1, false, true)

				expectedData := addPolicyData(networkPolicy1, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
				expectedData = addPolicyData(networkPolicy2, []string{nPodTest.portUUID}, nil, []int32{portNum + 1}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				initialDB.NBData = expectedData
				startOvn(initialDB, []v1.Namespace{namespace1}, []knet.NetworkPolicy{*networkPolicy1},
					[]testPod{nPodTest}, map[string]string{labelName: labelVal})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy1.Namespace).
					Get(context.TODO(), networkPolicy1.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy2.Namespace).
					Get(context.TODO(), networkPolicy2.Name, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.HaveOccurred())

				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// TODO: test server does not garbage collect ACLs, so we just expect policy to be removed
				expectedData = delPolicyData(networkPolicy2, pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})
				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{namespace2.Name}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.EventuallyExpectNoAddressSet(namespaceName2)
				expectedData = addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, "", "", pgMap)
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
				expectedData := addPolicyData(networkPolicy, nil, []string{namespace2.Name}, nil, "", "", pgMap)
				expectedData = append(expectedData, initialDB.NBData...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// delete namespace2
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Delete(context.TODO(), namespace2.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData = addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
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

				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
				fakeOvn.asf.EventuallyExpectEmptyAddressSetExist(namespaceName1)

				expectedData = addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
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
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
				expectedData1 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData1...))

				// Delete pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(nPodTest.namespace).
					Delete(context.TODO(), nPodTest.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectedData2 := append(expectedData, getExpectedDataPodsAndSwitches([]testPod{}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData2...))

				// After deleting the pod all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
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
				eventuallyExpectAddressSetsWithIP(fakeOvn, networkPolicy, nPodTest.podIP)
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName2, []string{nPodTest.podIP})

				_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Get(context.TODO(), networkPolicy.Name, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectedData := addPolicyData(networkPolicy, nil, []string{}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Update namespace labels
				namespace2.ObjectMeta.Labels = map[string]string{"labels": "test"}
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Namespaces().
					Update(context.TODO(), &namespace2, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// After updating the namespace all address sets should be empty
				eventuallyExpectEmptyAddressSetsExist(fakeOvn, networkPolicy)
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

				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy)

				// TODO: test server does not garbage collect ACLs, so we just expect policy to be removed
				expectedData = delPolicyData(networkPolicy, pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
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
				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, []string{}, nil, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceName1, []string{nPodTest.podIP})

				ginkgo.By("inject transient problem, nbdb is down")
				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// Delete network policy
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("delete policy, sleep long enough for TransactWithRetry to fail, causing NP Add to fail")
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

				eventuallyExpectNoAddressSets(fakeOvn, networkPolicy)

				// TODO: test server does not garbage collect ACLs, so we just expect policy to be removed
				expectedData = delPolicyData(networkPolicy, pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(libovsdb.HaveData(expectedData...))

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
				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				expectedData = delPolicyData(networkPolicy, pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

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

				expectedData := addPolicyData(networkPolicy, []string{nPodTest.portUUID}, nil, []int32{portNum}, "", "", pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))

				// Delete the network policy
				ginkgo.By("Deleting that network policy")
				err = fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicy.Namespace).
					Delete(context.TODO(), networkPolicy.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// TODO: test server does not garbage collect ACLs, so we just expect policy & deny portgroups to be removed
				expectedData = delPolicyData(networkPolicy, pgMap)
				expectedData = append(expectedData, getExpectedDataPodsAndSwitches([]testPod{nPodTest}, []string{nodeName})...)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
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
			pgMap = map[string]*pgArgType{}
			originalACLLogSeverity := fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice)
			originalNamespace = *newNamespace(namespaceName1)
			originalNamespace.Annotations = map[string]string{util.AclLoggingAnnotation: originalACLLogSeverity}
		})

		table.DescribeTable("ACL logging for network policies reacts to severity updates", func(networkPolicies ...*knet.NetworkPolicy) {
			ginkgo.By("Provisioning the system with an initial empty policy, we know deterministically the names of the default deny ACLs")
			initialDenyAllPolicy := newNetworkPolicy("emptyPol", namespaceName1, metav1.LabelSelector{}, nil, nil)
			// originalACLLogSeverity.Deny == nbdb.ACLSeverityAlert
			initialExpectedData := addPolicyData(initialDenyAllPolicy, nil, []string{}, nil, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice, pgMap)

			app.Action = func(ctx *cli.Context) error {
				startOvn(libovsdb.TestSetup{}, []v1.Namespace{originalNamespace}, []knet.NetworkPolicy{*initialDenyAllPolicy},
					nil, nil)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialExpectedData...))

				// create network policies for given Entry
				for i := range networkPolicies {
					ginkgo.By("Creating new network policy")
					_, err := fakeOvn.fakeClient.KubeClient.NetworkingV1().NetworkPolicies(networkPolicies[i].GetNamespace()).
						Create(context.TODO(), networkPolicies[i], metav1.CreateOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					initialExpectedData = addPolicyData(networkPolicies[i], nil, []string{}, nil, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice, pgMap)
				}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(initialExpectedData))

				// update namespace log severity
				updatedLogSeverity := nbdb.ACLSeverityDebug
				gomega.Expect(
					updateNamespaceACLLogSeverity(&originalNamespace, updatedLogSeverity, updatedLogSeverity)).To(gomega.Succeed(),
					"should have managed to update the ACL logging severity within the namespace")

				// update expected data log severity
				expectedData := addPolicyData(initialDenyAllPolicy, nil, []string{}, nil, updatedLogSeverity, nbdb.ACLSeverityNotice, pgMap)
				for i := range networkPolicies {
					expectedData = addPolicyData(networkPolicies[i], nil, []string{}, nil, updatedLogSeverity, nbdb.ACLSeverityNotice, pgMap)
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

				expectedData := addPolicyData(newPolicy, nil, []string{}, nil, desiredLogSeverity, desiredLogSeverity, pgMap)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdb.HaveData(expectedData...))
				return nil
			}
			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})

func asMatch(addressSets []string) string {
	hashedNames := make([]string, 0, len(addressSets))
	for _, as := range addressSets {
		v4HashedName, _ := addressset.MakeAddressSetHashNames(as)
		hashedNames = append(hashedNames, v4HashedName)
	}
	sort.Strings(hashedNames)
	var match string
	for i, n := range hashedNames {
		if i > 0 {
			match += ", "
		}
		match += fmt.Sprintf("$%s", n)
	}
	return match
}

func buildExpectedACL(gp *gressPolicy, pgName string, as []string, aclLogging *ACLLoggingLevels) *nbdb.ACL {
	name := getGressPolicyACLName(gp.policyNamespace, gp.policyName, gp.idx)
	asMatch := asMatch(as)
	match := fmt.Sprintf("ip4.src == {%s}", asMatch)
	if gp.policyType == knet.PolicyTypeIngress {
		match = fmt.Sprintf("(%s || (%s.src == %s && %s.dst == {%s}))", match, "ip4", types.V4OVNServiceHairpinMasqueradeIP, "ip4", asMatch)
	}
	match += fmt.Sprintf(" && outport == @%s", pgName)
	gpDirection := string(knet.PolicyTypeIngress)
	externalIds := map[string]string{
		l4MatchACLExtIdKey:     "None",
		ipBlockCIDRACLExtIdKey: "false",
		namespaceExtIdKey:      gp.policyNamespace,
		policyACLExtIdKey:      gp.policyName,
		policyTypeACLExtIdKey:  gpDirection,
		gpDirection + "_num":   fmt.Sprintf("%d", gp.idx),
	}
	acl := libovsdbops.BuildACL(name, nbdb.ACLDirectionToLport, types.DefaultAllowPriority, match,
		nbdb.ACLActionAllowRelated, types.OvnACLLoggingMeter, aclLogging.Allow, true, externalIds, nil)
	return acl
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

	ginkgo.It("computes match strings from address sets correctly", func() {
		const (
			pgName string = "pg-name"
		)
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		asFactory = addressset.NewFakeAddressSetFactory()
		config.IPv4Mode = true
		config.IPv6Mode = false

		policy := &knet.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				UID:       apimachinerytypes.UID("testing"),
				Name:      "policy",
				Namespace: "testing",
			},
		}

		gp := newGressPolicy(knet.PolicyTypeIngress, 0, policy.Namespace, policy.Name)
		err := gp.ensurePeerAddressSet(asFactory)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		asName := gp.peerAddressSet.GetName()

		one := "testing.policy.ingress.1"
		two := "testing.policy.ingress.2"
		three := "testing.policy.ingress.3"
		four := "testing.policy.ingress.4"
		five := "testing.policy.ingress.5"
		six := "testing.policy.ingress.6"
		defaultAclLogging := ACLLoggingLevels{
			nbdb.ACLSeverityInfo,
			nbdb.ACLSeverityInfo,
		}

		gomega.Expect(gp.addNamespaceAddressSet(one)).To(gomega.BeTrue())
		expected := buildExpectedACL(gp, pgName, []string{asName, one}, &defaultAclLogging)
		actual := gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.addNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// address sets should be alphabetized
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two, three}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// re-adding an existing set is a no-op
		gomega.Expect(gp.addNamespaceAddressSet(three)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, one, two, three, four}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// now delete a set
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, three, four}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is a no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		// add and delete some more...
		gomega.Expect(gp.addNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, three, four, five}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(three)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, four, five}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(one)).To(gomega.BeFalse())

		gomega.Expect(gp.addNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, two, four, five, six}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(two)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four, five, six}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(five)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four, six}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(six)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName, four}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeTrue())
		expected = buildExpectedACL(gp, pgName, []string{asName}, &defaultAclLogging)
		actual = gp.buildLocalPodACLs(pgName, &defaultAclLogging)
		gomega.Expect(actual).To(libovsdb.ConsistOfIgnoringUUIDs(expected))

		// deleting again is no-op
		gomega.Expect(gp.delNamespaceAddressSet(four)).To(gomega.BeFalse())
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
