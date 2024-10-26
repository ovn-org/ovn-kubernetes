package nat

import (
	"fmt"
	"net"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

type natSync struct {
	initialNATs []*nbdb.NAT
	finalNATs   []*nbdb.NAT
	pods        podsNetInfo
}

const (
	egressIPName                 = "eip1"
	egressIP                     = "10.10.10.10"
	nat1UUID                     = "nat-1-UUID"
	nat2UUID                     = "nat-2-UUID"
	pod1V4CIDRStr                = "10.128.0.5/32"
	pod1V6CIDRStr                = "2001:0000:130F:0000:0000:09C0:876A:130B/128"
	pod1Namespace                = "ns1"
	pod1Name                     = "pod1"
	pod2V4CIDRStr                = "10.128.0.6/32"
	pod2V6CIDRStr                = "2001:0000:130F:0000:0000:09C0:876A:130A/128"
	pod2Namespace                = "ns1"
	pod2Name                     = "pod2"
	defaultNetworkControllerName = "default-network-controller"
)

var (
	pod1V4IPNet  = testing.MustParseIPNet(pod1V4CIDRStr)
	pod1V6IPNet  = testing.MustParseIPNet(pod1V6CIDRStr)
	pod2V4IPNet  = testing.MustParseIPNet(pod2V4CIDRStr)
	pod2V6IPNet  = testing.MustParseIPNet(pod2V6CIDRStr)
	legacyExtIDs = map[string]string{legacyEIPNameExtIDKey: egressIPName}
	pod1V4ExtIDs = getEgressIPNATDbIDs(egressIPName, pod1Namespace, pod1Name, ipFamilyValueV4, defaultNetworkControllerName).GetExternalIDs()
	pod1V6ExtIDs = getEgressIPNATDbIDs(egressIPName, pod1Namespace, pod1Name, ipFamilyValueV6, defaultNetworkControllerName).GetExternalIDs()
)

var _ = ginkgo.Describe("NAT Syncer", func() {

	ginkgo.Context("EgressIP", func() {

		ginkgotable.DescribeTable("egress NATs", func(sync natSync) {
			performTest(defaultNetworkControllerName, sync.initialNATs, sync.finalNATs, sync.pods)
		}, ginkgotable.Entry("converts legacy IPv4 NATs", natSync{
			initialNATs: []*nbdb.NAT{getSNAT(nat1UUID, pod1V4CIDRStr, egressIP, legacyExtIDs)},
			finalNATs:   []*nbdb.NAT{getSNAT(nat1UUID, pod1V4CIDRStr, egressIP, pod1V4ExtIDs)},
			pods: podsNetInfo{
				{
					[]net.IP{pod1V4IPNet.IP},
					pod1Namespace,
					pod1Name,
				},
				{
					[]net.IP{pod2V4IPNet.IP},
					pod2Namespace,
					pod2Name,
				},
			},
		}),
			ginkgotable.Entry("converts legacy IPv6 NATs", natSync{
				initialNATs: []*nbdb.NAT{getSNAT(nat1UUID, pod1V6CIDRStr, egressIP, legacyExtIDs)},
				finalNATs:   []*nbdb.NAT{getSNAT(nat1UUID, pod1V6CIDRStr, egressIP, pod1V6ExtIDs)},
				pods: podsNetInfo{
					{
						[]net.IP{pod1V6IPNet.IP},
						pod1Namespace,
						pod1Name,
					},
					{
						[]net.IP{pod2V6IPNet.IP},
						pod2Namespace,
						pod2Name,
					},
				},
			}),
			ginkgotable.Entry("converts legacy dual stack NATs", natSync{
				initialNATs: []*nbdb.NAT{getSNAT(nat1UUID, pod1V4CIDRStr, egressIP, legacyExtIDs), getSNAT(nat2UUID, pod1V6CIDRStr, egressIP, legacyExtIDs)},
				finalNATs:   []*nbdb.NAT{getSNAT(nat1UUID, pod1V4CIDRStr, egressIP, pod1V4ExtIDs), getSNAT(nat2UUID, pod1V6CIDRStr, egressIP, pod1V6ExtIDs)},
				pods: podsNetInfo{
					{
						[]net.IP{pod1V4IPNet.IP, pod1V6IPNet.IP},
						pod1Namespace,
						pod1Name,
					},
					{
						[]net.IP{pod2V4IPNet.IP, pod2V6IPNet.IP},
						pod2Namespace,
						pod2Name,
					},
				},
			}),
			ginkgotable.Entry("doesn't alter NAT with correct external IDs", natSync{
				initialNATs: []*nbdb.NAT{getSNAT(nat1UUID, pod1V6CIDRStr, egressIP, pod1V6ExtIDs)},
				finalNATs:   []*nbdb.NAT{getSNAT(nat1UUID, pod1V6CIDRStr, egressIP, pod1V6ExtIDs)},
				pods: podsNetInfo{
					{
						[]net.IP{pod1V4IPNet.IP, pod1V6IPNet.IP},
						pod1Namespace,
						pod1Name,
					},
					{
						[]net.IP{pod2V4IPNet.IP, pod2V6IPNet.IP},
						pod2Namespace,
						pod2Name,
					},
				},
			}),
		)
	})
})

func performTest(controllerName string, initialNATS, finalNATs []*nbdb.NAT, pods podsNetInfo) {
	// build LSPs
	var lspDB []libovsdbtest.TestData
	podToIPs := map[string][]net.IP{}
	for _, podInfo := range pods {
		key := composeNamespaceName(podInfo.namespace, podInfo.name)
		entry, ok := podToIPs[key]
		if !ok {
			entry = []net.IP{}
		}
		podToIPs[key] = append(entry, podInfo.ip...)
	}
	var lspUUIDs []string
	for namespaceName, ips := range podToIPs {
		namespace, name := decomposeNamespaceName(namespaceName)
		lsp := getLSP(namespace, name, ips...)
		lspUUIDs = append(lspUUIDs, lsp.UUID)
		lspDB = addLSPToTestData(lspDB, lsp)
	}
	// build switch
	ls := getLS(lspUUIDs)
	// build cluster router
	router := getRouterWithNATs(getNATsUUIDs(initialNATS)...)
	// build initial DB
	initialDB := addLSsToTestData([]libovsdbtest.TestData{router}, ls)
	initialDB = append(initialDB, lspDB...)
	initialDB = addNATToTestData(initialDB, initialNATS...)

	// build expected DB
	expectedDB := addLSsToTestData([]libovsdbtest.TestData{router}, ls)
	expectedDB = addNATToTestData(expectedDB, finalNATs...)
	expectedDB = append(expectedDB, lspDB...)

	dbSetup := libovsdbtest.TestSetup{NBData: initialDB}
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer cleanup.Cleanup()

	syncer := NewNATSyncer(nbClient, controllerName)
	config.IPv4Mode = false
	config.IPv6Mode = false

	gomega.Expect(syncer.Sync()).Should(gomega.Succeed())
	gomega.Eventually(nbClient).Should(libovsdbtest.HaveData(expectedDB))
}

func getRouterWithNATs(policies ...string) *nbdb.LogicalRouter {
	if policies == nil {
		policies = make([]string, 0)
	}
	return &nbdb.LogicalRouter{
		UUID: "router-UUID",
		Name: "router",
		Nat:  policies,
	}
}

func getLS(ports []string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		UUID:  "switch-UUID",
		Name:  "switch",
		Ports: ports,
	}
}

func getLSP(podNamespace, podName string, ips ...net.IP) *nbdb.LogicalSwitchPort {
	var ipsStr []string
	for _, ip := range ips {
		ipsStr = append(ipsStr, ip.String())
	}
	return &nbdb.LogicalSwitchPort{
		UUID:        fmt.Sprintf("%s-%s-UUID", podNamespace, podName),
		Addresses:   append([]string{"0a:58:0a:f4:00:04"}, ipsStr...),
		ExternalIDs: map[string]string{"namespace": podNamespace, "pod": "true"},
		Name:        util.GetLogicalPortName(podNamespace, podName),
	}
}

func getNATsUUIDs(nats []*nbdb.NAT) []string {
	natUUIDs := make([]string, 0)
	for _, nat := range nats {
		natUUIDs = append(natUUIDs, nat.UUID)
	}
	return natUUIDs
}

func addLSsToTestData(data []libovsdbtest.TestData, lss ...*nbdb.LogicalSwitch) []libovsdbtest.TestData {
	for _, ls := range lss {
		data = append(data, ls)
	}
	return data
}

func addLSPToTestData(data []libovsdbtest.TestData, lsps ...*nbdb.LogicalSwitchPort) []libovsdbtest.TestData {
	for _, lsp := range lsps {
		data = append(data, lsp)
	}
	return data
}

func addNATToTestData(data []libovsdbtest.TestData, nats ...*nbdb.NAT) []libovsdbtest.TestData {
	for _, nat := range nats {
		data = append(data, nat)
	}
	return data
}

func composeNamespaceName(namespace, name string) string {
	return fmt.Sprintf("%s/%s", namespace, name)
}

func decomposeNamespaceName(str string) (string, string) {
	split := strings.Split(str, "/")
	return split[0], split[1]
}

func getSNAT(uuid, logicalIP, extIP string, extIDs map[string]string) *nbdb.NAT {
	port := "node-1"
	return &nbdb.NAT{
		UUID:        uuid,
		ExternalIDs: extIDs,
		ExternalIP:  extIP,
		LogicalIP:   logicalIP,
		LogicalPort: &port,
		Type:        nbdb.NATTypeSNAT,
	}
}
