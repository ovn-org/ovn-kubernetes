// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package routeadvertisements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	frrfake "github.com/metallb/frr-k8s/pkg/client/clientset/versioned/fake"
	frrlisters "github.com/metallb/frr-k8s/pkg/client/listers/api/v1beta1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	ctesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	eiptypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	eiplisters "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressip/v1/apis/listers/egressip/v1"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	rafake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned/fake"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	userdefinednetworkv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// failingEIPLister injects an operational failure after VTEP validation.
type failingEIPLister struct {
	eiplisters.EgressIPLister
	err error
}

// List injects a retryable cache failure while leaving other lister behavior intact.
func (l failingEIPLister) List(labels.Selector) ([]*eiptypes.EgressIP, error) {
	return nil, l.err
}

type testRA struct {
	Name                     string
	TargetVRF                string
	NetworkSelector          map[string]string
	NodeSelector             map[string]string
	FRRConfigurationSelector map[string]string
	SelectsDefault           bool
	AdvertisePods            bool
	AdvertiseEgressIPs       bool
	Status                   *metav1.ConditionStatus
}

func (tra testRA) RouteAdvertisements() *ratypes.RouteAdvertisements {
	ra := &ratypes.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: tra.Name,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF:                tra.TargetVRF,
			Advertisements:           []ratypes.AdvertisementType{},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: metav1.LabelSelector{},
		},
	}
	if tra.AdvertisePods {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.PodNetwork)
	}
	if tra.AdvertiseEgressIPs {
		ra.Spec.Advertisements = append(ra.Spec.Advertisements, ratypes.EgressIP)
	}
	if tra.NetworkSelector != nil {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
			ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
				NetworkSelector: metav1.LabelSelector{
					MatchLabels: tra.NetworkSelector,
				},
			},
		})
	}
	if tra.SelectsDefault {
		ra.Spec.NetworkSelectors = append(ra.Spec.NetworkSelectors, apitypes.NetworkSelector{
			NetworkSelectionType: apitypes.DefaultNetwork,
		})
	}
	if tra.NodeSelector != nil {
		ra.Spec.NodeSelector = metav1.LabelSelector{
			MatchLabels: tra.NodeSelector,
		}
	}
	if tra.FRRConfigurationSelector != nil {
		ra.Spec.FRRConfigurationSelector = metav1.LabelSelector{
			MatchLabels: tra.FRRConfigurationSelector,
		}
	}
	if tra.Status != nil {
		ra.Status.Conditions = []metav1.Condition{{Type: "Accepted", Status: *tra.Status}}
	}
	return ra
}

var (
	nodePrimaryAddr = map[string]string{
		"node":  "1.0.1.100/24",
		"node1": "1.0.1.101/24",
		"node2": "1.0.2.100/24",
	}
	nodePrimaryAddrIPv6 = map[string]string{
		"node":  "fd03::ffff:0100:0050/64",
		"node1": "fd03::ffff:0100:0051/64",
		"node2": "fd03::ffff:0200:0050/64",
	}
)

type testNamespace struct {
	Name   string
	Labels map[string]string
}

func (tn testNamespace) Namespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   tn.Name,
			Labels: tn.Labels,
		},
	}
}

type testNode struct {
	Name                      string
	Generation                int
	Labels                    map[string]string
	PrimaryAddressAnnotation  string
	DPUHostAddressAnnotation  string
	SubnetsAnnotation         string
	TunnelIDsAnnotation       string
	L3GatewayConfigAnnotation string
	ChassisIDAnnotation       string
	VTEPIPs                   map[string][]string
	RawVTEPAnnotation         *string
}

func (tn testNode) Node() *corev1.Node {
	primaryAddressAnnotation := tn.PrimaryAddressAnnotation
	if primaryAddressAnnotation == "" {
		primaryAddressAnnotation = "{\"ipv4\":\"" + nodePrimaryAddr[tn.Name] + "\", \"ipv6\":\"" + nodePrimaryAddrIPv6[tn.Name] + "\"}"
	}
	annotations := map[string]string{
		"k8s.ovn.org/node-subnets": tn.SubnetsAnnotation,
		util.OvnNodeIfAddr:         primaryAddressAnnotation,
	}
	if tn.TunnelIDsAnnotation != "" {
		annotations[types.UDNLayer2NodeGRLRPTunnelIDAnnotation] = tn.TunnelIDsAnnotation
	}
	if tn.DPUHostAddressAnnotation != "" {
		annotations[util.OVNNodePrimaryDPUHostAddr] = tn.DPUHostAddressAnnotation
	}
	if tn.L3GatewayConfigAnnotation != "" {
		annotations[util.OvnNodeL3GatewayConfig] = tn.L3GatewayConfigAnnotation
		annotations[util.OvnNodeChassisID] = "chassis-" + tn.Name
	}
	if tn.ChassisIDAnnotation != "" {
		annotations[util.OvnNodeChassisID] = tn.ChassisIDAnnotation
	}
	if tn.RawVTEPAnnotation != nil {
		annotations[util.OVNNodeVTEPs] = *tn.RawVTEPAnnotation
	} else if len(tn.VTEPIPs) > 0 {
		vteps := make(map[string]util.VTEPNodeAnnotation, len(tn.VTEPIPs))
		for vtepName, ips := range tn.VTEPIPs {
			vteps[vtepName] = util.VTEPNodeAnnotation{IPs: ips}
		}
		vtepJSON, _ := json.Marshal(vteps)
		annotations[util.OVNNodeVTEPs] = string(vtepJSON)
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tn.Name,
			Labels:      tn.Labels,
			Generation:  int64(tn.Generation),
			Annotations: annotations,
		},
	}
}

// TestDeleteIncompleteVTEPConfigurations checks that partial deletion failures remain retryable
// without preventing cleanup of other rejected nodes.
func TestDeleteIncompleteVTEPConfigurations(t *testing.T) {
	for _, unsafeInput := range []bool{false, true} {
		t.Run(fmt.Sprintf("unsafeInput=%t", unsafeInput), func(t *testing.T) {
			g := gomega.NewWithT(t)
			client := frrfake.NewSimpleClientset()
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			for _, name := range []string{"source", "fail-a", "fail-b", "delete", "keep"} {
				cfg := generatedFRRConfig(name, "invalid-node", true).FRRConfiguration()
				cfg.Namespace = "test"
				if name == "source" {
					cfg.Annotations = nil
				}
				if name == "keep" {
					cfg.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"] = "valid-node"
				}
				_, err := client.ApiV1beta1().FRRConfigurations("test").Create(context.Background(), cfg, metav1.CreateOptions{})
				g.Expect(err).NotTo(gomega.HaveOccurred(), "creating FRRConfiguration %s", name)
				g.Expect(indexer.Add(cfg)).To(gomega.Succeed(), "indexing FRRConfiguration %s", name)
			}
			failureA, failureB := errors.New("delete A failed"), errors.New("delete B failed")
			client.PrependReactor("delete", "frrconfigurations", func(action ctesting.Action) (bool, runtime.Object, error) {
				switch action.(ctesting.DeleteAction).GetName() {
				case "fail-a":
					return true, nil, failureA
				case "fail-b":
					return true, nil, failureB
				}
				return false, nil, nil
			})
			c := &Controller{frrClient: client, frrLister: frrlisters.NewFRRConfigurationLister(indexer)}
			updated, remaining, err := c.deleteIncompleteVTEPConfigurations("ra", sets.New("invalid-node"), unsafeInput)
			g.Expect(updated).To(gomega.BeTrue(), "successful deletion must be recorded despite other failures")
			g.Expect(remaining).To(gomega.Equal(3), "failed deletions and the valid configuration must remain")
			g.Expect(errors.Is(err, failureA)).To(gomega.BeTrue(), "cleanup must retain the first deletion error")
			g.Expect(errors.Is(err, failureB)).To(gomega.BeTrue(), "cleanup must retain the second deletion error")
			g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "cleanup failures must trigger retries")
			objects, listErr := client.ApiV1beta1().FRRConfigurations("test").List(context.Background(), metav1.ListOptions{})
			g.Expect(listErr).NotTo(gomega.HaveOccurred(), "listing configurations after cleanup")
			names := []string{}
			for _, cfg := range objects.Items {
				names = append(names, cfg.Name)
			}
			g.Expect(names).To(gomega.ConsistOf("source", "fail-a", "fail-b", "keep"), "cleanup must delete only eligible configurations whose deletion succeeds")
		})
	}
}

// TestConflictingVTEPNodes covers ownership continuity, ambiguous claims, and
// idempotence after rejected configurations are removed.
func TestConflictingVTEPNodes(t *testing.T) {
	const prefix = "100.64.0.1/32"
	config := func(ra, node, vrf, ip string) *frrapi.FRRConfiguration {
		return &frrapi.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:        generateName + node,
				Labels:      map[string]string{types.OvnRouteAdvertisementsKey: ra},
				Annotations: map[string]string{types.OvnRouteAdvertisementsKey: ra + "/source/" + node},
			},
			Spec: frrapi.FRRConfigurationSpec{
				NodeSelector: metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/hostname": node}},
				BGP:          frrapi.BGPConfig{Routers: []frrapi.Router{{VRF: vrf, Prefixes: []string{ip}}}},
			},
		}
	}
	mismatchedAnnotation := config("other", "a", "", prefix)
	mismatchedAnnotation.Labels[types.OvnRouteAdvertisementsKey] = "ra"

	for _, tt := range []struct {
		name        string
		claims      map[string]sets.Set[string]
		otherClaims map[string]sets.Set[string]
		existing    []*frrapi.FRRConfiguration
		rejected    []string
		message     string
	}{
		{name: "cross RA bootstrap excludes local claimant", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, rejected: []string{"a"}, message: "no unique established advertiser"},
		{name: "preserves owner from another RA", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("other", "b", "", prefix)}, rejected: []string{"a"}, message: "preserving established advertiser b"},
		{name: "preserves local owner against another RA", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, message: "preserving established advertiser a"},
		{name: "cross RA ambiguous established claims", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("other", "b", "", prefix)}, rejected: []string{"a"}, message: "no unique established advertiser"},
		{name: "cross RA IPv6 ownership", claims: map[string]sets.Set[string]{"a": sets.New("fd00::1/128")}, otherClaims: map[string]sets.Set[string]{"b": sets.New("fd00::1/128")}, existing: []*frrapi.FRRConfiguration{config("other", "b", "", "fd00::1/128")}, rejected: []string{"a"}, message: "preserving established advertiser b"},
		{name: "same node shared across RAs", claims: map[string]sets.Set[string]{"a": sets.New(prefix)}, otherClaims: map[string]sets.Set[string]{"a": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("other", "a", "", prefix)}},
		{name: "deleted RA is not ownership evidence", existing: []*frrapi.FRRConfiguration{config("deleted", "a", "", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "remote-only conflict does not reject local advertisements", claims: map[string]sets.Set[string]{"a": sets.New("100.64.0.2/32")}, otherClaims: map[string]sets.Set[string]{"a": sets.New(prefix), "b": sets.New(prefix)}},
		{name: "bootstrap excludes both", rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "preserves established node", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, rejected: []string{"b"}, message: "preserving established advertiser a"},
		{name: "multiple configurations for one node count once", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("ra", "a", "", prefix)}, rejected: []string{"b"}, message: "preserving established advertiser a"},
		{name: "both established excludes both", existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix), config("ra", "b", "", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "mismatched RA annotation is not ownership evidence", existing: []*frrapi.FRRConfiguration{mismatchedAnnotation}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "non-default VRF is not ownership evidence", existing: []*frrapi.FRRConfiguration{config("ra", "a", "blue", prefix)}, rejected: []string{"a", "b"}, message: "no unique established advertiser"},
		{name: "former claimant is not an owner", claims: map[string]sets.Set[string]{"b": sets.New(prefix), "c": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}, rejected: []string{"b", "c"}, message: "no unique established advertiser"},
		{name: "handover after old claim disappears", claims: map[string]sets.Set[string]{"b": sets.New(prefix)}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", prefix)}},
		{name: "IPv6 ownership", claims: map[string]sets.Set[string]{"a": sets.New("fd00::1/128"), "b": sets.New("fd00::1/128")}, existing: []*frrapi.FRRConfiguration{config("ra", "a", "", "fd00::1/128")}, rejected: []string{"b"}, message: "preserving established advertiser a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := tt.claims
			if claims == nil {
				claims = map[string]sets.Set[string]{"a": sets.New(prefix), "b": sets.New(prefix)}
			}
			g := gomega.NewWithT(t)
			rejected, err := conflictingVTEPNodes("ra", map[string]map[string]sets.Set[string]{"ra": claims, "other": tt.otherClaims}, tt.existing)
			g.Expect(rejected.UnsortedList()).To(gomega.ConsistOf(tt.rejected), "rejected nodes for claims %v", claims)
			if tt.message == "" {
				g.Expect(err).NotTo(gomega.HaveOccurred(), "non-conflicting claims must be accepted")
			} else {
				g.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(tt.message)), "conflict must identify the ownership decision")
			}
			// Simulate deletion of rejected nodes' configurations and a second
			// reconciliation with unchanged claims (including after restart).
			retained := []*frrapi.FRRConfiguration{}
			for _, cfg := range tt.existing {
				if !rejected.Has(cfg.Spec.NodeSelector.MatchLabels["kubernetes.io/hostname"]) {
					retained = append(retained, cfg)
				}
			}
			again, _ := conflictingVTEPNodes("ra", map[string]map[string]sets.Set[string]{"ra": claims, "other": tt.otherClaims}, retained)
			g.Expect(again).To(gomega.Equal(rejected), "withdrawal must not change the rejection decision on the next reconcile")
		})
	}
}

// TestGetNodeVTEPPrefixes checks selection isolation, canonical host prefixes,
// and enforcement of each VTEP's configured CIDRs.
func TestGetNodeVTEPPrefixes(t *testing.T) {
	cidrs := map[string][]*net.IPNet{
		"vtep":  {ovntest.MustParseIPNet("100.64.0.0/16"), ovntest.MustParseIPNet("fd00::/64")},
		"other": {ovntest.MustParseIPNet("192.0.2.0/24")},
	}
	for _, tt := range []struct {
		name      string
		raw       string
		ips       []string
		prefixes  []string
		wantError bool
	}{
		{name: "canonical dual-stack host prefixes", ips: []string{"100.64.0.1", "fd00:0:0:0::1"}, prefixes: []string{"100.64.0.1/32", "fd00::1/128"}},
		{name: "wrong-shaped unselected entry", raw: `{"vtep":{"ips":["100.64.0.1"]},"unselected":"invalid"}`, prefixes: []string{"100.64.0.1/32"}},
		{name: "wrong-shaped unselected IPs", raw: `{"vtep":{"ips":["100.64.0.1"]},"unselected":{"ips":42}}`, prefixes: []string{"100.64.0.1/32"}},
		{name: "wrong-shaped selected entry", raw: `{"vtep":"invalid"}`, wantError: true},
		{name: "wrong-shaped selected IPs", raw: `{"vtep":{"ips":42}}`, wantError: true},
		{name: "malformed annotation JSON", raw: `{"vtep":`, wantError: true},
		{name: "IPv4 outside the CIDRs", ips: []string{"198.51.100.1"}, wantError: true},
		{name: "IPv6 outside the CIDRs", ips: []string{"fd01::1"}, wantError: true},
		{name: "address in another VTEP's CIDR", ips: []string{"192.0.2.1"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := (&testNode{Name: "node", VTEPIPs: map[string][]string{"vtep": tt.ips, "unselected": {"not-an-ip"}}}).Node()
			if tt.raw != "" {
				node.Annotations[util.OVNNodeVTEPs] = tt.raw
			}
			prefixes, err := getNodeVTEPPrefixes(node, cidrs)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError {
				gomega.NewWithT(t).Expect(prefixes.UnsortedList()).To(gomega.ConsistOf(tt.prefixes), "advertised prefixes for annotation %s", node.Annotations[util.OVNNodeVTEPs])
			}
		})
	}
}

func TestGetDPUHostGatewayNextHopsUsesDPUHostAddressAnnotation(t *testing.T) {
	g := gomega.NewWithT(t)
	previousGatewayMode := config.Gateway.Mode
	t.Cleanup(func() { config.Gateway.Mode = previousGatewayMode })
	config.Gateway.Mode = config.GatewayModeShared

	c := &Controller{}
	selected := &selectedNetworks{networkUplinks: map[string]string{}}

	nextHops, err := c.getDPUHostGatewayNextHops(&corev1.Node{}, selected, types.DefaultNetworkName)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(nextHops).To(gomega.BeNil(), "a node without the annotation is not a DPU host")

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Annotations: map[string]string{util.OVNNodePrimaryDPUHostAddr: "invalid"},
	}}
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeTrue(), "a malformed DPU-host annotation is a configuration error")

	node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"not-a-cidr"}`
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeTrue(), "an invalid DPU-host CIDR is a configuration error")

	for _, annotation := range []string{
		`{"ipv4":"fd00::1/64"}`,
		`{"ipv6":"192.0.2.1/24"}`,
	} {
		node.Annotations[util.OVNNodePrimaryDPUHostAddr] = annotation
		_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
		g.Expect(errors.Is(err, errConfig)).To(gomega.BeTrue(),
			"a CIDR in the wrong address-family field is a configuration error: %s", annotation)
	}

	node.Annotations[util.OVNNodePrimaryDPUHostAddr] = `{"ipv4":"172.18.255.254/16"}`
	_, err = c.getDPUHostGatewayNextHops(node, selected, types.DefaultNetworkName)
	g.Expect(errors.Is(err, errPending)).To(gomega.BeTrue(), "a DPU host waits for its gateway state")
}

type testPod struct {
	Name      string
	Namespace string
	Node      string
}

func (tp testPod) Pod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tp.Name,
			Namespace: tp.Namespace,
		},
		Spec: corev1.PodSpec{
			NodeName: tp.Node,
		},
	}
}

type testPrefixSelector struct {
	Prefix string
	LE     uint32
	GE     uint32
}

type testNeighbor struct {
	ASN                    uint32
	Address                string
	Interface              string
	DualStackAddressFamily *bool
	Advertise              []string
	NextHopV4              string
	NextHopV6              string
	Receive                []testPrefixSelector
}

func (tn testNeighbor) Neighbor() frrapi.Neighbor {
	n := frrapi.Neighbor{
		ASN:       tn.ASN,
		Address:   tn.Address,
		Interface: tn.Interface,
		ToAdvertise: frrapi.Advertise{
			Allowed: frrapi.AllowedOutPrefixes{
				Mode:     frrapi.AllowRestricted,
				Prefixes: tn.Advertise,
			},
			NextHop: frrapi.NextHop{
				IPv4: tn.NextHopV4,
				IPv6: tn.NextHopV6,
			},
		},
	}
	if tn.DualStackAddressFamily != nil {
		n.DualStackAddressFamily = *tn.DualStackAddressFamily
	}
	if len(tn.Receive) > 0 {
		prefixSelectors := make([]frrapi.PrefixSelector, 0, len(tn.Receive))
		for _, ps := range tn.Receive {
			prefixSelectors = append(prefixSelectors, frrapi.PrefixSelector{
				Prefix: ps.Prefix,
				LE:     ps.LE,
				GE:     ps.GE,
			})
		}
		n.ToReceive = frrapi.Receive{
			Allowed: frrapi.AllowedInPrefixes{
				Mode:     frrapi.AllowRestricted,
				Prefixes: prefixSelectors,
			},
		}
	}

	return n
}

type testRouter struct {
	ASN       uint32
	VRF       string
	Prefixes  []string
	Neighbors []*testNeighbor
	Imports   []string
}

func (tr testRouter) Router() frrapi.Router {
	r := frrapi.Router{
		ASN:      tr.ASN,
		VRF:      tr.VRF,
		Prefixes: tr.Prefixes,
	}
	for _, n := range tr.Neighbors {
		r.Neighbors = append(r.Neighbors, n.Neighbor())
	}
	for _, vrf := range tr.Imports {
		r.Imports = append(r.Imports, frrapi.Import{VRF: vrf})
	}
	return r
}

type testFRRConfig struct {
	Name         string
	Namespace    string
	Generation   int
	Labels       map[string]string
	Annotations  map[string]string
	Routers      []*testRouter
	NodeSelector map[string]string
	OwnUpdate    bool
	RawConfig    string
}

func (tf testFRRConfig) FRRConfiguration() *frrapi.FRRConfiguration {
	f := &frrapi.FRRConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tf.Name,
			Namespace:   tf.Namespace,
			Labels:      tf.Labels,
			Annotations: tf.Annotations,
			Generation:  int64(tf.Generation),
		},
		Spec: frrapi.FRRConfigurationSpec{
			NodeSelector: metav1.LabelSelector{
				MatchLabels: tf.NodeSelector,
			},
		},
	}
	for _, r := range tf.Routers {
		f.Spec.BGP.Routers = append(f.Spec.BGP.Routers, r.Router())
	}
	rawConfig := tf.RawConfig
	if rawConfig == "" {
		rawConfig = tf.generateUnicastRawConfig()
	}
	if rawConfig != "" {
		f.Spec.Raw.Config = rawConfig
		f.Spec.Raw.Priority = rawConfigPriority
	}
	if tf.OwnUpdate {
		f.ManagedFields = append(f.ManagedFields, metav1.ManagedFieldsEntry{
			Manager: fieldManager,
			Time:    &metav1.Time{Time: time.Now()},
		})
	}
	return f
}

func (tf testFRRConfig) generateUnicastRawConfig() string {
	var buf strings.Builder
	routers := make(map[string]*testRouter, len(tf.Routers))
	for _, r := range tf.Routers {
		routers[r.VRF] = r
	}
	for _, vrf := range slices.Sorted(maps.Keys(routers)) {
		r := routers[vrf]
		if len(r.Neighbors) == 0 {
			continue
		}
		if r.VRF == "" {
			fmt.Fprintf(&buf, "router bgp %d\n", r.ASN)
		} else {
			fmt.Fprintf(&buf, "router bgp %d vrf %s\n", r.ASN, r.VRF)
		}
		for _, n := range r.Neighbors {
			switch {
			case n.Address == "" && n.Interface != "":
				// unnumbered neighbors are included in both address families
				fmt.Fprintf(&buf, " address-family ipv4 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Interface)
				fmt.Fprintf(&buf, " address-family ipv6 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Interface)
			case utilnet.IsIPv6String(n.Address):
				fmt.Fprintf(&buf, " address-family ipv6 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Address)
			default:
				fmt.Fprintf(&buf, " address-family ipv4 unicast\n  neighbor %s allowas-in origin\n exit-address-family\n", n.Address)
			}
		}
		buf.WriteString("exit\n!\n")
	}
	return buf.String()
}

type testEIP struct {
	Name              string
	Generation        int
	NamespaceSelector map[string]string
	EIPs              map[string]string
}

func (te testEIP) EgressIP() *eiptypes.EgressIP {
	eip := eiptypes.EgressIP{
		ObjectMeta: metav1.ObjectMeta{
			Name:       te.Name,
			Generation: int64(te.Generation),
		},
		Spec: eiptypes.EgressIPSpec{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: te.NamespaceSelector,
			},
		},
		Status: eiptypes.EgressIPStatus{
			Items: []eiptypes.EgressIPStatusItem{},
		},
	}
	for node, ip := range te.EIPs {
		eip.Status.Items = append(eip.Status.Items, eiptypes.EgressIPStatusItem{Node: node, EgressIP: ip})
	}
	return &eip
}

type testNAD struct {
	Name                  string
	Namespace             string
	Network               string
	Subnet                string
	Labels                map[string]string
	Annotations           map[string]string
	IsSecondary           bool
	Topology              string
	Transport             string
	OwnUpdate             bool
	EVPNVTEPName          string
	EVPNMACVRFVNI         int32
	EVPNMACVRFRouteTarget string
	EVPNIPVRFVNI          int32
	EVPNIPVRFRouteTarget  string
}

func (tn testNAD) NAD() *nadtypes.NetworkAttachmentDefinition {
	if tn.Annotations == nil {
		tn.Annotations = map[string]string{}
	}
	tn.Annotations[types.OvnNetworkNameAnnotation] = tn.Network
	nad := &nadtypes.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tn.Name,
			Namespace:   tn.Namespace,
			Labels:      tn.Labels,
			Annotations: tn.Annotations,
		},
	}
	if strings.HasPrefix(tn.Network, types.CUDNPrefix) {
		ownerRef := *metav1.NewControllerRef(
			&metav1.ObjectMeta{Name: tn.Network},
			userdefinednetworkv1.SchemeGroupVersion.WithKind("ClusterUserDefinedNetwork"),
		)
		nad.ObjectMeta.OwnerReferences = []metav1.OwnerReference{ownerRef}
	}

	// Build the config as a map to properly marshal EVPN config
	cniConfig := map[string]interface{}{
		"cniVersion":       "1.1.0",
		"name":             tn.Network,
		"type":             config.CNI.Plugin,
		"netAttachDefName": tn.Namespace + "/" + tn.Name,
	}

	if tn.Topology != "" {
		cniConfig["topology"] = tn.Topology
	}
	if tn.Subnet != "" {
		cniConfig["subnets"] = tn.Subnet
	}
	if tn.Topology != "" && !tn.IsSecondary {
		cniConfig["role"] = "primary"
	}
	if tn.Transport != "" {
		cniConfig["transport"] = tn.Transport
	}

	// Add EVPN configuration if present
	if tn.EVPNMACVRFVNI != 0 || tn.EVPNIPVRFVNI != 0 {
		evpnConfig := map[string]interface{}{}
		if tn.EVPNVTEPName != "" {
			evpnConfig["vtep"] = tn.EVPNVTEPName
		}
		if tn.EVPNMACVRFVNI != 0 {
			macvrf := map[string]interface{}{
				"vni": tn.EVPNMACVRFVNI,
			}
			if tn.EVPNMACVRFRouteTarget != "" {
				macvrf["routeTarget"] = tn.EVPNMACVRFRouteTarget
			}
			evpnConfig["macVRF"] = macvrf
		}
		if tn.EVPNIPVRFVNI != 0 {
			ipvrf := map[string]interface{}{
				"vni": tn.EVPNIPVRFVNI,
			}
			if tn.EVPNIPVRFRouteTarget != "" {
				ipvrf["routeTarget"] = tn.EVPNIPVRFRouteTarget
			}
			evpnConfig["ipVRF"] = ipvrf
		}
		cniConfig["evpn"] = evpnConfig
	}

	configBytes, _ := json.Marshal(cniConfig)
	nad.Spec.Config = string(configBytes)

	if tn.OwnUpdate {
		nad.ManagedFields = append(nad.ManagedFields, metav1.ManagedFieldsEntry{
			Manager: fieldManager,
			Time:    &metav1.Time{Time: time.Now()},
		})
	}
	return nad
}

type Fake interface {
	PrependReactor(verb, resource string, reaction ctesting.ReactionFunc)
}

var count = uint32(0)

// source
// https://stackoverflow.com/questions/68794562/kubernetes-fake-client-doesnt-handle-generatename-in-objectmeta/68794563#68794563
func addGenerateNameReactor[T Fake](client any) {
	fake := client.(Fake)
	fake.PrependReactor(
		"create",
		"*",
		func(action ctesting.Action) (handled bool, ret runtime.Object, err error) {
			ret = action.(ctesting.CreateAction).GetObject()
			meta, ok := ret.(metav1.Object)
			if !ok {
				return
			}

			if meta.GetName() == "" && meta.GetGenerateName() != "" {
				meta.SetName(meta.GetGenerateName() + fmt.Sprintf("%d", atomic.AddUint32(&count, 1)))
			}

			return
		},
	)
}

func init() {
	// set this once at the beginning to avoid races that happen because we
	// cannot stop the NAD informer properly (the api we use was generated with
	// an old codegen and the informer has no shutdown method)
	config.IPv4Mode = true

	// Set feature flags before any test calls RegisterClusterManagerFunctional().
	// The sync.Once in registration captures whichever flags are set on the first call.
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEVPN = true

	// Disable WatchListClient feature gate for tests.
	// Fake clientsets from third-party libraries don't yet support WatchList semantics
	// introduced in K8s 1.35, causing informers to hang waiting for bookmark events.
	// See: https://github.com/kubernetes/kubernetes/issues/135895
	os.Setenv("KUBE_FEATURE_WatchListClient", "false")
}

// TestController_reconcile checks generated configuration, withdrawal, NAD
// references, and status against selected networks and nodes.
func TestController_reconcile(t *testing.T) {
	metrics.RegisterClusterManagerFunctional()

	frrNamespace := "frrNamespace"
	tests := []struct {
		name                 string
		ra                   *testRA
		otherRAs             []*testRA
		frrConfigs           []*testFRRConfig
		nads                 []*testNAD
		nodes                []*testNode
		namespaces           []*testNamespace
		pods                 []*testPod
		eips                 []*testEIP
		vteps                []*vtepv1.VTEP
		reconcile            string
		transport            string
		gatewayMode          config.GatewayMode
		dynamicUDN           bool
		layer2TransitRouter  bool
		ipv6                 bool
		wantErr              bool
		eipListError         error
		expectAcceptedStatus metav1.ConditionStatus
		// expectAcceptedMessageContains, when set, is a substring the Accepted
		// condition message must contain
		expectAcceptedMessageContains string
		expectFRRConfigs              []*testFRRConfig
		expectNADAnnotations          map[string]map[string]string
	}{
		{
			name: "adds the validated marker to a generated FRRConfiguration that predates input validation",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "frrConfig-generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
		},
		{
			name: "reconciles pod+eip RouteAdvertisement for a single FRR config, node and default network and target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles dual-stack pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24", "fd01::/64"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles IPv6-only pod RouteAdvertisement with an unnumbered interface neighbor",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"fd01::/64\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"fd01::/64"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile a neighbor with neither address nor interface",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile a neighbor with neither address nor interface in an IPv6-only cluster",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"fd01::/64\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "reconciles dual-stack pod+eip RouteAdvertisement for a single FRR config, node and default network and target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
							{ASN: 1, Address: "fd02::ffff:100:64"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}"}},
			eips:                 []*testEIP{{Name: "eipv4", EIPs: map[string]string{"node": "1.0.1.1"}}, {Name: "eipv6", EIPs: map[string]string{"node": "fd03::ffff:100:101"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24", "fd01::/64", "fd03::ffff:100:101/128"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
							{ASN: 1, Address: "fd02::ffff:100:64", Advertise: []string{"fd01::/64", "fd03::ffff:100:101/128"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles pod RouteAdvertisement for a single FRR config, node, non default networks and default target VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.1.0/24"}},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2", Subnet: "1.5.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_red\":\"1.2.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.2.0.0/24", "1.3.0.0/24", "1.4.0.0/16", "1.5.0.0/16"}, Imports: []string{"black", "blue", "green", "red"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.0.0/24", "1.3.0.0/24", "1.4.0.0/16", "1.5.0.0/16"}},
						}},
						{ASN: 1, VRF: "black", Imports: []string{"default"}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
						{ASN: 1, VRF: "green", Imports: []string{"default"}},
						{ASN: 1, VRF: "red", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "black": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "(layer3) reconciles eip RouteAdvertisement for a single FRR config, node, non default network and non default target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default"},
				{Name: "red", Namespace: "red", Network: "cluster_udn_red", Topology: "layer3", Subnet: "1.2.0.0/16"},
				{Name: "blue", Namespace: "blue", Network: "cluster_udn_blue", Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_red\":\"1.2.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
			},
			namespaces: []*testNamespace{
				{Name: "default", Labels: map[string]string{"selected": "default"}},
				{Name: "red", Labels: map[string]string{"selected": "red"}},
				{Name: "blue", Labels: map[string]string{"selected": "blue"}},
			},
			eips: []*testEIP{
				{Name: "eip1", EIPs: map[string]string{"node": "172.100.0.16"}, NamespaceSelector: map[string]string{"selected": "blue"}}, // secondary interface EIP also advertised
				{Name: "eip2", EIPs: map[string]string{"node": "1.0.1.2"}, NamespaceSelector: map[string]string{"selected": "red"}},       // namespace served by unselected network, ignored
				{Name: "eip3", EIPs: map[string]string{"node": "1.0.1.3"}, NamespaceSelector: map[string]string{"selected": "blue"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.1.3/32", "172.100.0.16/32"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.3/32", "172.100.0.16/32"}},
						}, Imports: []string{"blue"}},
						{ASN: 1, VRF: "blue", Imports: []string{"red"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "(layer2) fails to reconcile eip RouteAdvertisement for a single FRR config, node, non default networks and non default target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "green", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2", Subnet: "1.5.0.0/16"},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\""},
			},
			namespaces: []*testNamespace{
				{Name: "default", Labels: map[string]string{"selected": "default"}},
				{Name: "green", Labels: map[string]string{"selected": "green"}},
				{Name: "black", Labels: map[string]string{"selected": "black"}},
			},
			eips: []*testEIP{
				{Name: "eip1", EIPs: map[string]string{"node": "172.100.0.17"}, NamespaceSelector: map[string]string{"selected": "green"}}, // secondary interface EIP also advertised
				{Name: "eip2", EIPs: map[string]string{"node": "1.0.1.4"}, NamespaceSelector: map[string]string{"selected": "black"}},      // namespace served by unselected network, ignored
				{Name: "eip3", EIPs: map[string]string{"node": "1.0.1.5"}, NamespaceSelector: map[string]string{"selected": "green"}},
			},
			reconcile: "ra",
			// EgressIP advertisements for Layer2 UDNs is not supported yet.
			expectAcceptedStatus: metav1.ConditionFalse,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"green": {}},
		},
		{
			name: "reconciles a RouteAdvertisement updating the generated FRRConfigurations if needed",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"2.0.1.1", "2.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles a deleted RouteAdvertisement",
			frrConfigs: []*testFRRConfig{
				{
					Name:         "generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/default/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			reconcile: "ra",
		},
		{
			name: "reconciles a RouteAdvertisement for multiple selected FRR configs, nodes and networks on auto target VRF",
			ra: &testRA{
				Name:                     "ra",
				AdvertisePods:            true,
				TargetVRF:                "auto",
				FRRConfigurationSelector: map[string]string{"selected": "true"},
				NetworkSelector:          map[string]string{"selected": "true"},
				SelectsDefault:           true,
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3"}, // not selected
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "black", Namespace: "black", Network: util.GenerateCUDNNetworkName("black"), Topology: "layer2"}, // not selected
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:         "frrConfig-node1",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node1"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.2.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "frrConfig-node2",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node2"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "another-frrConfig-node2",
					Namespace:    frrNamespace,
					Labels:       map[string]string{"selected": "true"},
					NodeSelector: map[string]string{"node": "node2"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.0.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.2.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{ // not selected
					Name:      "another-frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 3, VRF: "blue", Prefixes: []string{"3.0.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 3, Address: "3.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{Name: "node1", Labels: map[string]string{"selected": "true", "node": "node1"}, SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_red\":\"1.2.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
				{Name: "node2", Labels: map[string]string{"selected": "true", "node": "node2"}, SubnetsAnnotation: "{\"default\":\"1.1.2.0/24\", \"cluster_udn_red\":\"1.2.2.0/24\", \"cluster_udn_blue\":\"1.3.2.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-node1/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.1.0/24"}},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.2.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.1.0/24"}},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-node2/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.2.0/24"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/another-frrConfig-node2/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.2.2.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.2.2.0/24"}},
						}},
						{ASN: 1, VRF: "green", Prefixes: []string{"1.4.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles RouteAdvertisements status even when no other updates are required",
			ra:   &testRA{Name: "ra", AdvertisePods: true, AdvertiseEgressIPs: true, SelectsDefault: true, Status: ptr.To(metav1.ConditionFalse)},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:         "frrConfig-generated",
					Namespace:    frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "default", Namespace: "ovn-kubernetes", Network: "default", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			eips:                 []*testEIP{{Name: "eip", EIPs: map[string]string{"node": "1.0.1.1"}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.0.1.1/32", "1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.0.1.1/32", "1.1.0.0/24"}},
						}},
					}},
			},
		},
		{
			name:      "reconciles pod RouteAdvertisement for default network in no-overlay mode with ToReceive routes",
			ra:        &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			transport: types.NetworkTransportNoOverlay,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Receive: []testPrefixSelector{{Prefix: "1.2.0.0/16"}}},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}, Receive: []testPrefixSelector{{Prefix: "1.1.0.0/16", LE: 24, GE: 24}}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:        "reconciles pod RouteAdvertisement for DPU host default network with next-hop",
			ra:          &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			gatewayMode: config.GatewayModeShared,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                      "node",
					DPUHostAddressAnnotation:  `{"ipv4":"172.18.255.254/16"}`,
					SubnetsAnnotation:         "{\"default\":\"1.1.0.0/24\"}",
					L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16"],"next-hops":["172.18.0.1"]}}`,
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24"}, NextHopV4: "172.18.255.254"},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:        "reconciles dual-stack pod RouteAdvertisement for DPU host with an unnumbered interface neighbor and next-hops",
			ra:          &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			gatewayMode: config.GatewayModeShared,
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                      "node",
					DPUHostAddressAnnotation:  `{"ipv4":"172.18.255.254/16","ipv6":"fc00:f853:ccd:e793::4/64"}`,
					SubnetsAnnotation:         "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}",
					L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16","fc00:f853:ccd:e793::4/64"],"next-hops":["172.18.0.1","fc00:f853:ccd:e793::1"]}}`,
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "enp4s0f0np0", Advertise: []string{"1.1.0.0/24", "fd01::/64"}, NextHopV4: "172.18.255.254", NextHopV6: "fc00:f853:ccd:e793::4"},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:      "reconciles pod RouteAdvertisement for CUDN in no-overlay mode with ToReceive routes including CUDN pod subnets",
			ra:        &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"selected": "true"}},
			transport: types.NetworkTransportNoOverlay,
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue-ns", Network: types.CUDNPrefix + "blue", Topology: "layer3", Subnet: "10.10.0.0/16/24", Transport: types.NetworkTransportNoOverlay, Labels: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\",\"" + types.CUDNPrefix + "blue\":\"10.10.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "10.10.1.0/24"}, VRF: "", Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							// ToReceive should include pod subnets from all no-overlay networks in alphabetical order: cluster-udn-blue (10.10.0.0/16) then default (1.1.0.0/16)
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.1.0.0/24", "10.10.1.0/24"}, Receive: []testPrefixSelector{
								{Prefix: "10.10.0.0/16", LE: 24, GE: 24},
								{Prefix: "1.1.0.0/16", LE: 24, GE: 24},
							}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{
				"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"blue":    {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "fails to reconcile a secondary network",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", IsSecondary: true, Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile an non-cluster UDN",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name:                 "fails to reconcile pod network if node selector is not empty",
			ra:                   &testRA{Name: "ra", AdvertisePods: true, NodeSelector: map[string]string{"selected": "true"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if no FRRConfiguration is selected for selected node",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NodeSelector: map[string]string{"selected-by": "RouteAdvertisements"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:         "frrConfig",
					Namespace:    frrNamespace,
					NodeSelector: map[string]string{"selected-by": "FRRConfiguration"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", Labels: map[string]string{"selected-by": "FRRConfiguration"}},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", Labels: map[string]string{"selected-by": "RouteAdvertisements"}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile when subnet annotation is missing from node",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile when subnet annotation is missing for network",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"red\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if a selectd FRRConfiguration has no matching VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if not all VRFs were matched with 'auto' target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "cluster_udn_red", Topology: "layer3", Labels: map[string]string{"selected": "true"}},
				{Name: "blue", Namespace: "blue", Network: "cluster_udn_blue", Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if EgressIP is advertised with 'auto' target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertiseEgressIPs: true, NetworkSelector: map[string]string{"selected": "true"}},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: "red", Topology: "layer3", Labels: map[string]string{"selected": "true"}},
			},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"red\":\"1.1.0.0/24\""}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile if dual-stack address family is set",
			ra:   &testRA{Name: "ra", AdvertisePods: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", DualStackAddressFamily: ptr.To(true)},
						}},
					},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "excludes self-neighbor from per-node FRRConfiguration",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig-base",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.1"}, // self for node
							{ASN: 1, Address: "10.0.0.2"}, // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					SubnetsAnnotation:        "{\"default\":\"1.1.0.0/24\"}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"10.0.0.1/24\"}",
				},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-base/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "10.0.0.2", Advertise: []string{"1.1.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "excludes self-neighbor with a non-canonical IPv6 address from per-node FRRConfiguration",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig-base",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "FD00:0:0:0:0:0:0:1"}, // self for node
							{ASN: 1, Address: "fd00::2"},            // other node
						}},
					},
				},
			},
			nodes: []*testNode{
				{
					Name:                     "node",
					SubnetsAnnotation:        "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"]}",
					PrimaryAddressAnnotation: "{\"ipv4\":\"10.0.0.1/24\",\"ipv6\":\"fd00::1/64\"}",
				},
			},
			ipv6:                 true,
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-base/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.0.0/24", "fd01::/64"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "fd00::2", Advertise: []string{"fd01::/64"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"default": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile EVPN-enabled network to default VRF",
			ra:   &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "evpn-net", Namespace: "test", Network: util.GenerateCUDNNetworkName("evpn-net"),
					Topology: "layer2", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "rejects EVPN MAC-VRF network with a newline in the route target",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1\n"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid MAC-VRF route target",
		},
		{
			name: "rejects EVPN IP-VRF network with a VNI above the maximum",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer3", Subnet: "10.1.0.0/16/24", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: maxVNI + 1},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_red\":\"10.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid IP-VRF VNI",
		},
		{
			name: "rejects EVPN MAC-VRF network with a negative VNI",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: -1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid MAC-VRF VNI",
		},
		{
			name: "rejects EVPN network whose VRF name is not a valid interface name",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red\nx"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid VRF name",
		},
		{
			name: "rejects RouteAdvertisements with a newline in the target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red\nx", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red\nx", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid TargetVRF",
		},
		{
			name: "rejects FRRConfiguration neighbor with a newline in the interface name",
			ra:   &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Interface: "eth0\nx"},
						}},
					},
				},
			},
			nodes:                         []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:                     "ra",
			expectAcceptedStatus:          metav1.ConditionFalse,
			expectAcceptedMessageContains: "invalid neighbor",
		},
		{
			name: "reconciles EVPN MAC-VRF l2 network with a specific target VRF without a VRF router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles EVPN MAC-VRF l2 network with auto target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles EVPN IP-VRF network with auto target and creates a router",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "reconciles EVPN IP-VRF with router ASN from another FRRConfiguration",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, SelectsDefault: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfigGlobal",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
				{
					Name:      "frrConfigVRF",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65100, VRF: "blue"},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\",\"cluster_udn_blue\":\"10.2.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigGlobal/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
`,
					Routers: []*testRouter{
						{ASN: 65000, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"1.1.0.0/24"}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigVRF/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `vrf blue
 vni 2000
exit-vrf
!
router bgp 65100 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65100, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile MACVRF EVPN without global router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile IPVRF EVPN without global router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNIPVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "fails to reconcile EVPN with global router but no neighbors",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_red\":\"10.1.1.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "reconciles EVPN when global router is in a different FRRConfiguration than VRF router",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfigGlobal",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						// Global router with neighbors - provides ASN and neighbors for EVPN
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
				{
					Name:      "frrConfigVRF",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						// VRF-specific router - matches the target VRF
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigGlobal/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
`,
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfigVRF/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"10.1.0.0/16"}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises VTEP IP /32 in default-VRF router prefixes for EVPN IP-VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises dual-stack VTEP IPs to an unnumbered interface neighbor for EVPN IP-VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Interface: "enp4s0f0np0"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16", "fd64::/48"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1", "fd64::1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor enp4s0f0np0 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor enp4s0f0np0 activate
  neighbor enp4s0f0np0 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32", "fd64::1/128"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Interface: "enp4s0f0np0", Advertise: []string{"100.64.0.1/32", "fd64::1/128"},
								Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}, {Prefix: "fd64::/48", LE: 128, GE: 128}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises VTEP IP /128 in default-VRF router prefixes for IPv6 EVPN IP-VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "fd00::1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue6", Namespace: "blue6", Network: util.GenerateCUDNNetworkName("blue6"),
					Topology: "layer3", Subnet: "fd02::/48", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep6", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep6"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"fd64::/64"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue6\":\"fd02::100/64\"}", VTEPIPs: map[string][]string{"my-vtep6": {"fd64::1"}}}},
			reconcile:            "ra",
			ipv6:                 true,
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv6 unicast
  neighbor fd00::1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor fd00::1 activate
  neighbor fd00::1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue6
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue6
 address-family l2vpn evpn
  advertise ipv6 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue6", Prefixes: []string{"fd02::/64"}},
						{ASN: 65000, Prefixes: []string{"fd64::1/128"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "fd00::1", Advertise: []string{"fd64::1/128"}, Receive: []testPrefixSelector{{Prefix: "fd64::/64", LE: 128, GE: 128}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue6": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile EVPN target VRF when cloned default-VRF neighbor has dual-stack address family set",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", DualStackAddressFamily: ptr.To(true)},
						}},
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
		},
		{
			name: "advertises VTEP IP /32 in default-VRF router prefixes for EVPN MAC-VRF with target VRF",
			ra:   &testRA{Name: "ra", TargetVRF: "red", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"),
					Topology: "layer2", Subnet: "10.1.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNMACVRFVNI: 1000, EVPNMACVRFRouteTarget: "65000:1000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
  vni 1000
   route-target import 65000:1000
   route-target export 65000:1000
  exit-vni
 exit-address-family
exit
!
router bgp 65000 vrf red
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "red", Prefixes: []string{"10.1.0.0/16"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"10.1.0.0/16"}},
						}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "deduplicates VTEP IPs when multiple EVPN networks reference the same VTEP",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "shared-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"),
					Topology: "layer3", Subnet: "10.3.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "shared-vtep", EVPNIPVRFVNI: 3000, EVPNIPVRFRouteTarget: "65000:3000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "shared-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\",\"cluster_udn_green\":\"10.3.1.0/24\"}", VTEPIPs: map[string][]string{"shared-vtep": {"100.64.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
vrf green
 vni 3000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
router bgp 65000 vrf green
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:3000
  route-target export 65000:3000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, VRF: "green", Prefixes: []string{"10.3.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"blue":  {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "advertises VTEP IPs per node across multiple nodes",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
				{Name: "node2", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.2.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.2"}}},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.2.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.2/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.2/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "advertises both VTEP IPs when EVPN networks reference different VTEPs",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "vtep-a", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"),
					Topology: "layer3", Subnet: "10.3.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "vtep-b", EVPNIPVRFVNI: 3000, EVPNIPVRFRouteTarget: "65000:3000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vtep-a"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vtep-b"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"200.10.0.0/16"}},
				},
			},
			nodes:                []*testNode{{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\",\"cluster_udn_green\":\"10.3.1.0/24\"}", VTEPIPs: map[string][]string{"vtep-a": {"100.64.0.1"}, "vtep-b": {"200.10.0.1"}}}},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
vrf green
 vni 3000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
router bgp 65000 vrf green
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:3000
  route-target export 65000:3000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, VRF: "green", Prefixes: []string{"10.3.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32", "200.10.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32", "200.10.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}, {Prefix: "200.10.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{
				"blue":  {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
				"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"},
			},
		},
		{
			name: "skips node without VTEP annotation and succeeds for node with annotation",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node1", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
				{Name: "node2", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.2.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node1"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node1"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.1.0/24"}},
						{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1", Advertise: []string{"100.64.0.1/32"}, Receive: []testPrefixSelector{{Prefix: "100.64.0.0/16", LE: 32, GE: 32}}},
						}},
					},
				},
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node2"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					RawConfig: `router bgp 65000
 address-family ipv4 unicast
  neighbor 192.168.1.1 allowas-in origin
 exit-address-family
 address-family l2vpn evpn
  neighbor 192.168.1.1 activate
  neighbor 192.168.1.1 allowas-in origin
  advertise-all-vni
 exit-address-family
exit
!
vrf blue
 vni 2000
exit-vrf
!
router bgp 65000 vrf blue
 address-family l2vpn evpn
  advertise ipv4 unicast
  route-target import 65000:2000
  route-target export 65000:2000
 exit-address-family
exit
!
`,
					Routers: []*testRouter{
						{ASN: 65000, VRF: "blue", Prefixes: []string{"10.2.2.0/24"}},
					},
				},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name: "fails to reconcile when VTEP annotation is malformed",
			ra:   &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 65000, Neighbors: []*testNeighbor{
							{ASN: 65000, Address: "192.168.1.1"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"),
					Topology: "layer3", Subnet: "10.2.0.0/16", Labels: map[string]string{"selected": "true"},
					EVPNVTEPName: "my-vtep", EVPNIPVRFVNI: 2000, EVPNIPVRFRouteTarget: "65000:2000"},
			},
			vteps: []*vtepv1.VTEP{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "my-vtep"},
					Spec:       vtepv1.VTEPSpec{CIDRs: []vtepv1.CIDR{"100.64.0.0/16"}},
				},
			},
			nodes: []*testNode{
				{Name: "node", SubnetsAnnotation: "{\"cluster_udn_blue\":\"10.2.1.0/24\"}", RawVTEPAnnotation: ptr.To("not-json")},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionFalse,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation, ignores nodes where the selected network is not allocated and advertises dual-stack subnets where it is",
			dynamicUDN: true,
			ipv6:       true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
							{ASN: 1, Address: "fd02::ffff:100:64"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16,fd01:3::/48", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network and has subnets
				// allocated for it, "node2" does not and must not block the
				// RouteAdvertisements from being accepted, not even with its
				// stale empty subnet entry for the network
				{Name: "node", SubnetsAnnotation: "{\"default\":[\"1.1.0.0/24\",\"fd01::/64\"], \"cluster_udn_blue\":[\"1.3.0.0/24\",\"fd01:3::/64\"]}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":[\"1.1.1.0/24\",\"fd01:0:0:1::/64\"], \"cluster_udn_blue\":[]}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.3.0.0/24", "fd01:3::/64"}, Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
							{ASN: 1, Address: "fd02::ffff:100:64", Advertise: []string{"fd01:3::/64"}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation and 'auto' target VRF, only requires matching the networks allocated on each node",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}, {Name: "red"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" is only active for network blue: with 'auto' target
				// VRF, network red not matching any router VRF on it must not
				// block the RouteAdvertisements from being accepted
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.3.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation, withdraws a network from nodes where it is no longer active even if still allocated",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network; "node2" does not
				// but still has a subnet allocated for it, as during the
				// deletion grace period after its last workload left: only
				// "node" must be advertised
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\", \"cluster_udn_blue\":\"1.3.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.3.0.0/24"}, Imports: []string{"blue"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
						{ASN: 1, VRF: "blue", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation, advertises a layer2 network from nodes where it is active and its tunnel ID is allocated",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "green"}},
			pods:       []*testPod{{Name: "pod", Namespace: "green", Node: "node"}},
			nodes: []*testNode{
				// layer2 networks have no per-node subnets: "node" runs a pod
				// attached to the network and has its gateway router LRP
				// tunnel ID allocated; "node2" still has a tunnel ID
				// allocated but no workloads, and must not be advertised
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}", TunnelIDsAnnotation: "{\"cluster_udn_green\":\"5\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}", TunnelIDsAnnotation: "{\"cluster_udn_green\":\"6\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.4.0.0/16"}, Imports: []string{"green"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
						{ASN: 1, VRF: "green", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:                "with dynamic UDN allocation and transit router, advertises an active layer2 network without a tunnel ID allocation and skips inactive nodes",
			dynamicUDN:          true,
			layer2TransitRouter: true,
			ra:                  &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "green"}},
			pods:       []*testPod{{Name: "pod", Namespace: "green", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network; "node2" does not.
				// Neither has a tunnel ID allocation, and only "node" must be
				// advertised.
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.4.0.0/16"}, Imports: []string{"green"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.4.0.0/16"}},
						}},
						{ASN: 1, VRF: "green", Imports: []string{"default"}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation, does not advertise a layer2 network from an active node until its tunnel ID is allocated",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "green", Namespace: "green", Network: util.GenerateCUDNNetworkName("green"), Topology: "layer2", Subnet: "1.4.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "green"}},
			pods:       []*testPod{{Name: "pod", Namespace: "green", Node: "node"}},
			nodes: []*testNode{
				// "node" runs a pod attached to the network but has no tunnel
				// ID allocated for it yet: it must not be advertised before
				// the network is rendered on it, that is, once the tunnel ID
				// allocation lands on the node annotations
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs:     []*testFRRConfig{},
			expectNADAnnotations: map[string]map[string]string{"green": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
		{
			name:       "with dynamic UDN allocation and 'auto' target VRF, skips FRRConfigurations that only match networks not allocated on the node",
			dynamicUDN: true,
			ra:         &testRA{Name: "ra", TargetVRF: "auto", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}},
			frrConfigs: []*testFRRConfig{
				{
					Name:      "frrConfig-blue",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
				{
					Name:      "frrConfig-red",
					Namespace: frrNamespace,
					Routers: []*testRouter{
						{ASN: 1, VRF: "red", Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100"},
						}},
					},
				},
			},
			nads: []*testNAD{
				{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}},
				{Name: "red", Namespace: "red", Network: util.GenerateCUDNNetworkName("red"), Topology: "layer3", Subnet: "1.2.0.0/16", Labels: map[string]string{"selected": "true"}},
			},
			namespaces: []*testNamespace{{Name: "blue"}, {Name: "red"}},
			pods:       []*testPod{{Name: "pod", Namespace: "blue", Node: "node"}},
			nodes: []*testNode{
				// "node" is only active for network blue: the source
				// FRRConfiguration carrying only network red's VRF must be
				// skipped for it instead of blocking the RouteAdvertisements
				// from being accepted
				{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"},
				{Name: "node2", SubnetsAnnotation: "{\"default\":\"1.1.1.0/24\"}"},
			},
			reconcile:            "ra",
			expectAcceptedStatus: metav1.ConditionTrue,
			expectFRRConfigs: []*testFRRConfig{
				{
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig-blue/node"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node"},
					Routers: []*testRouter{
						{ASN: 1, VRF: "blue", Prefixes: []string{"1.3.0.0/24"}, Neighbors: []*testNeighbor{
							{ASN: 1, Address: "1.0.0.100", Advertise: []string{"1.3.0.0/24"}},
						}},
					}},
			},
			expectNADAnnotations: map[string]map[string]string{"blue": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}, "red": {types.OvnRouteAdvertisementsKey: "[\"ra\"]"}},
		},
	}
	// Seed generated configurations to verify that invalid updates withdraw
	// existing advertisements while preserving configurations for valid nodes.
	for _, base := range slices.Clone(tests) {
		if base.name != "advertises VTEP IPs per node across multiple nodes" {
			continue
		}
		for _, scenario := range []struct{ selected, established bool }{{}, {selected: true}, {selected: true, established: true}} {
			cross := base
			cross.name = fmt.Sprintf("cross RA different VTEPs selected=%t established=%t", scenario.selected, scenario.established)
			cross.nodes = nil
			for i, node := range base.nodes {
				copy := *node
				copy.VTEPIPs = maps.Clone(node.VTEPIPs)
				if i == 1 {
					copy.VTEPIPs["other-vtep"] = []string{"100.64.0.1"}
				}
				cross.nodes = append(cross.nodes, &copy)
			}
			otherNAD := *base.nads[0]
			otherNAD.Name, otherNAD.Namespace = "red", "red"
			otherNAD.Network = util.GenerateCUDNNetworkName("red")
			otherNAD.Labels = map[string]string{"selected": "other"}
			otherNAD.EVPNVTEPName = "other-vtep"
			cross.nads = append(slices.Clone(base.nads), &otherNAD)
			otherVTEP := base.vteps[0].DeepCopy()
			otherVTEP.Name = "other-vtep"
			cross.vteps = append(slices.Clone(base.vteps), otherVTEP)
			if scenario.selected {
				cross.otherRAs = []*testRA{{Name: "other", AdvertisePods: true, TargetVRF: "auto", NetworkSelector: map[string]string{"selected": "other"}}}
				cross.expectAcceptedStatus = metav1.ConditionFalse
				cross.expectAcceptedMessageContains = "no unique established advertiser"
				cross.expectFRRConfigs = base.expectFRRConfigs[1:]
			}
			if scenario.established {
				owner := &testFRRConfig{
					Name: generateName + "other-owner", Namespace: frrNamespace,
					Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "other"},
					Annotations:  map[string]string{types.OvnRouteAdvertisementsKey: "other/frrConfig/node2", types.OvnRouteAdvertisementsValidatedKey: "true"},
					NodeSelector: map[string]string{"kubernetes.io/hostname": "node2"},
					Routers:      []*testRouter{{ASN: 65000, Prefixes: []string{"100.64.0.1/32"}}},
				}
				cross.frrConfigs = append(slices.Clone(base.frrConfigs), owner)
				cross.expectFRRConfigs = append(slices.Clone(cross.expectFRRConfigs), owner)
				cross.expectAcceptedMessageContains = "preserving established advertiser node2"
			}
			tests = append(tests, cross)
		}
		// A conflicting claim outside an EgressIP RA's node selection must
		// not suppress its selected node's VTEP advertisement.
		subset := base
		subset.name = "ignores VTEP collision outside RA node selection"
		ra := *base.ra
		ra.AdvertisePods = false
		ra.AdvertiseEgressIPs = true
		ra.TargetVRF = "blue"
		ra.NodeSelector = map[string]string{"selected": "true"}
		subset.ra = &ra
		source := *base.frrConfigs[0]
		source.Routers = append(slices.Clone(source.Routers), &testRouter{ASN: 65000, VRF: "blue"})
		subset.frrConfigs = []*testFRRConfig{&source}
		subset.nodes = nil
		for i, node := range base.nodes {
			copy := *node
			copy.RawVTEPAnnotation = ptr.To(`{"my-vtep":{"ips":["100.64.0.1"]}}`)
			if i == 0 {
				copy.Labels = map[string]string{"selected": "true"}
			}
			subset.nodes = append(subset.nodes, &copy)
		}
		subset.namespaces = []*testNamespace{{Name: "blue", Labels: map[string]string{"selected": "blue"}}}
		subset.eips = []*testEIP{{Name: "eip", EIPs: map[string]string{"node1": "192.0.2.1"}, NamespaceSelector: map[string]string{"selected": "blue"}}}
		expected := *base.expectFRRConfigs[0]
		expected.Routers = slices.Clone(expected.Routers)
		router := *expected.Routers[0]
		router.Prefixes = []string{"192.0.2.1/32"}
		expected.Routers[0] = &router
		subset.expectFRRConfigs = []*testFRRConfig{&expected}
		tests = append(tests, subset)
		crossSubset := subset
		crossSubset.name = "detects VTEP collision across disjoint EgressIP RA node selections"
		otherRA := *subset.ra
		otherRA.Name = "other"
		otherRA.NodeSelector = map[string]string{"selected": "false"}
		crossSubset.otherRAs = []*testRA{&otherRA}
		crossSubset.nodes = slices.Clone(subset.nodes)
		otherNode := *subset.nodes[1]
		otherNode.Labels = map[string]string{"selected": "false"}
		crossSubset.nodes[1] = &otherNode
		crossSubset.expectAcceptedStatus = metav1.ConditionFalse
		crossSubset.expectAcceptedMessageContains = "no unique established advertiser"
		crossSubset.expectFRRConfigs = nil
		tests = append(tests, crossSubset)
		for _, invalid := range []struct {
			name             string
			annotation       string
			generationError  bool
			operationalError bool
			noExisting       bool
			ambiguous        bool
			unsafeInput      bool
		}{
			{name: "withdraws invalid VTEP and unvalidated configurations on unsafe input", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`, unsafeInput: true},
			{name: "withdraws malformed VTEP advertisements", annotation: "not-json"},
			{name: "withdraws invalid node and retries operational generation failure", annotation: "not-json", operationalError: true},
			{name: "withdraws only invalid node on unrelated generation failure", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`, generationError: true},
			{name: "withdraws extra same-family advertisements", annotation: `{"my-vtep":{"ips":["100.64.0.1","100.64.0.99"]}}`},
			{name: "withdraws invalid IP advertisements", annotation: `{"my-vtep":{"ips":["not-an-ip"]}}`},
			{name: "withdraws injected prefix advertisements", annotation: `{"my-vtep":{"ips":["100.64.0.1/24"]}}`},
			{name: "withdraws out-of-range advertisements", annotation: `{"my-vtep":{"ips":["192.0.2.1"]}}`},
			{name: "bootstrap collision excludes both nodes", annotation: `{"my-vtep":{"ips":["100.64.0.2"]}}`, noExisting: true},
			{name: "ambiguous established collision withdraws both nodes", annotation: `{"my-vtep":{"ips":["100.64.0.1"]}}`, ambiguous: true},
			{name: "preserves established advertiser on duplicate VTEP", annotation: `{"my-vtep":{"ips":["100.64.0.2"]}}`},
		} {
			tt := base
			tt.name = invalid.name
			tt.expectAcceptedStatus = metav1.ConditionFalse
			tt.nodes = nil
			for _, node := range base.nodes {
				copy := *node
				tt.nodes = append(tt.nodes, &copy)
			}
			tt.nodes[0].RawVTEPAnnotation = ptr.To(invalid.annotation)
			tt.expectFRRConfigs = base.expectFRRConfigs[1:]
			if invalid.operationalError {
				ra := *base.ra
				ra.AdvertiseEgressIPs = true
				ra.TargetVRF = "blue"
				tt.ra = &ra
				tt.eipListError = errors.New("EgressIP cache lookup failed")
				tt.wantErr = true
			}
			if invalid.generationError || invalid.unsafeInput || invalid.operationalError {
				if invalid.generationError {
					tt.nodes[1].SubnetsAnnotation = "not-json"
				}
				tt.expectFRRConfigs = base.expectFRRConfigs[1:]
				tt.nads = nil
				for _, nad := range base.nads {
					copy := *nad
					copy.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: `["ra"]`}
					tt.nads = append(tt.nads, &copy)
				}
			}
			if strings.Contains(invalid.name, "preserves established advertiser") {
				tt.expectAcceptedMessageContains = "preserving established advertiser node2, excluding nodes [node1]"
			}
			if invalid.noExisting || invalid.ambiguous {
				tt.expectFRRConfigs = nil
				tt.expectAcceptedMessageContains = "no unique established advertiser"
			}
			if invalid.ambiguous {
				tt.nodes[1].RawVTEPAnnotation = ptr.To(`{"my-vtep":{"ips":["100.64.0.1"]}}`)
			}
			tt.frrConfigs = slices.Clone(base.frrConfigs)
			if invalid.unsafeInput {
				source := *base.frrConfigs[0]
				source.Routers = []*testRouter{{ASN: 65000, Neighbors: []*testNeighbor{{ASN: 65000, Interface: unsafeInputValue}}}}
				tt.frrConfigs[0] = &source
				unvalidated := *base.expectFRRConfigs[1]
				unvalidated.Name = generateName + "unvalidated"
				unvalidated.Namespace = frrNamespace
				unvalidated.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: "ra/old-source/node2"}
				tt.frrConfigs = append(tt.frrConfigs, &unvalidated)
				tt.expectAcceptedMessageContains = "invalid neighbor"
			}
			for i, generated := range base.expectFRRConfigs {
				if invalid.noExisting {
					continue
				}
				copy := *generated
				if invalid.ambiguous && i == 1 {
					copy.Routers = slices.Clone(generated.Routers)
					router := *copy.Routers[1]
					router.Prefixes = []string{"100.64.0.1/32"}
					copy.Routers[1] = &router
				}
				copy.Annotations = maps.Clone(copy.Annotations)
				copy.Annotations[types.OvnRouteAdvertisementsValidatedKey] = "true"
				copy.Name = fmt.Sprintf("%sexisting-%d", generateName, i)
				copy.Namespace = frrNamespace
				tt.frrConfigs = append(tt.frrConfigs, &copy)
			}
			tests = append(tests, tt)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()
			previousLayer2TransitRouter := config.Layer2UsesTransitRouter
			config.Layer2UsesTransitRouter = tt.layer2TransitRouter
			t.Cleanup(func() { config.Layer2UsesTransitRouter = previousLayer2TransitRouter })

			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
				{
					CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
					HostSubnetLength: 24,
				},
				{
					CIDR:             ovntest.MustParseIPNet("fd01::/48"),
					HostSubnetLength: 64,
				},
			}
			config.Default.Transport = tt.transport
			if tt.ipv6 {
				config.IPv6Mode = true
				defer func() { config.IPv6Mode = false }()
			}
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableEVPN = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = tt.dynamicUDN
			config.OVNKubernetesFeature.EnableDynamicUDNAllocation = tt.dynamicUDN
			// satisfy EVPN LGW restriction, otherwise no effect
			config.Gateway.Mode = config.GatewayModeLocal
			if tt.gatewayMode != "" {
				config.Gateway.Mode = tt.gatewayMode
			}

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
			addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)

			// create test objects
			if tt.ra != nil {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, otherRA := range tt.otherRAs {
				_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), otherRA.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred(), "creating competing RouteAdvertisements %s", otherRA.Name)
			}

			for _, frrConfig := range tt.frrConfigs {
				_, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrConfig.Namespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			defaultNADName := config.Default.ClusterDefaultNetworkNAD
			var defaultNAD *nadtypes.NetworkAttachmentDefinition
			for _, nad := range tt.nads {
				n, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				if nad.Name == defaultNADName.Name && nad.Namespace == defaultNADName.Namespace {
					defaultNAD = n
				}
			}

			for _, node := range tt.nodes {
				_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, namespace := range tt.namespaces {
				_, err := fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace.Namespace(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, pod := range tt.pods {
				_, err := fakeClientset.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.Pod(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, eip := range tt.eips {
				_, err := fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), eip.EgressIP(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			for _, vtep := range tt.vteps {
				_, err := fakeClientset.VTEPClient.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
			g.Expect(err).ToNot(gomega.HaveOccurred())

			c := NewController(nm.Interface(), wf, fakeClientset)

			// prime the default network NAD namespace
			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: config.Default.ClusterDefaultNetworkNAD.Namespace,
				},
			}
			_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// prime the default network NAD
			if defaultNAD == nil {
				defaultNAD, err = util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				// update it with the annotation that network manager would set
				defaultNAD.Annotations = map[string]string{types.OvnNetworkNameAnnotation: types.DefaultNetworkName}
				_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			// wait for caches to sync
			hasSynced := []cache.InformerSynced{
				wf.RouteAdvertisementsInformer().Informer().HasSynced,
				wf.FRRConfigurationsInformer().Informer().HasSynced,
				wf.NADInformer().Informer().HasSynced,
				wf.NodeCoreInformer().Informer().HasSynced,
				wf.EgressIPInformer().Informer().HasSynced,
			}
			if config.Gateway.Mode == config.GatewayModeLocal {
				hasSynced = append(hasSynced, wf.VTEPInformer().Informer().HasSynced)
			}
			cache.WaitForCacheSync(context.Background().Done(), hasSynced...)

			err = nm.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// we just need the inital sync
			nm.Stop()

			// Clean up condition metric timeseries from the global Prometheus registry
			// so they don't leak into subsequent subtests.
			t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition(tt.reconcile) })

			if tt.eipListError != nil {
				c.eipLister = failingEIPLister{EgressIPLister: c.eipLister, err: tt.eipListError}
			}
			reconcileErr := c.reconcile(tt.reconcile)
			if (reconcileErr != nil) != tt.wantErr {
				t.Fatalf("Controller.reconcile() error = %v, wantErr %v", reconcileErr, tt.wantErr)
			}
			if tt.eipListError != nil {
				g.Expect(errors.Is(reconcileErr, tt.eipListError)).To(gomega.BeTrue(), "operational cause must remain retryable")
				g.Expect(errors.Is(reconcileErr, errConfig)).To(gomega.BeFalse(), "validation must not mask the operational failure")
			}

			// verify RA status and condition metric are set as expected
			if tt.ra != nil && tt.eipListError == nil {
				ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), tt.reconcile, metav1.GetOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
				accepted := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				g.Expect(accepted).NotTo(gomega.BeNil())
				g.Expect(accepted.Status).To(gomega.Equal(tt.expectAcceptedStatus), accepted.Message)
				if tt.expectAcceptedMessageContains != "" {
					g.Expect(accepted.Message).To(gomega.ContainSubstring(tt.expectAcceptedMessageContains))
				}
				// verify condition metric is set as expected
				expectedTrue := 0.0
				expectedFalse := 1.0
				if tt.expectAcceptedStatus == metav1.ConditionTrue {
					expectedTrue = 1.0
					expectedFalse = 0.0
				}
				valTrue, found := getRAConditionMetricValue(tt.reconcile, "Accepted", "true")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=true should exist")
				g.Expect(valTrue).To(gomega.Equal(expectedTrue))
				valFalse, found := getRAConditionMetricValue(tt.reconcile, "Accepted", "false")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(gomega.Equal(expectedFalse))
			} else {
				_, foundTrue := getRAConditionMetricValue(tt.reconcile, "Accepted", "true")
				_, foundFalse := getRAConditionMetricValue(tt.reconcile, "Accepted", "false")
				g.Expect(foundTrue || foundFalse).To(gomega.BeFalse(), "condition metrics should not exist for deleted RA")
			}

			// verify FRRConfigurations have been created/updated/deleted as expected
			actualFRRConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			var actualFRRConfigKeys []string
			actualFRRConfigLabels := map[string]map[string]string{}
			actualFRRConfigSpecs := map[string]*frrapi.FRRConfigurationSpec{}
			for _, frrConfig := range actualFRRConfigs.Items {
				if _, generated := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]; generated {
					g.Expect(frrConfig.Annotations).To(gomega.HaveKeyWithValue(types.OvnRouteAdvertisementsValidatedKey, "true"),
						"generated FRRConfiguration %s must carry the validated marker", frrConfig.Name)
					actualFRRConfigKeys = append(actualFRRConfigKeys, frrConfig.Annotations[types.OvnRouteAdvertisementsKey])
					actualFRRConfigLabels[frrConfig.Annotations[types.OvnRouteAdvertisementsKey]] = frrConfig.Labels
					actualFRRConfigSpecs[frrConfig.Annotations[types.OvnRouteAdvertisementsKey]] = &frrConfig.Spec
				}
			}

			var expectedRRConfigKeys []string
			expectedFRRConfigLabels := map[string]map[string]string{}
			expectedFRRConfigSpecs := map[string]*frrapi.FRRConfigurationSpec{}
			for _, frrConfig := range tt.expectFRRConfigs {
				expectedFRRConfig := frrConfig.FRRConfiguration()
				expectedRRConfigKeys = append(expectedRRConfigKeys, expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey])
				expectedFRRConfigLabels[expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]] = expectedFRRConfig.Labels
				expectedFRRConfigSpecs[expectedFRRConfig.Annotations[types.OvnRouteAdvertisementsKey]] = &expectedFRRConfig.Spec
			}

			g.Expect(actualFRRConfigKeys).To(gomega.ConsistOf(expectedRRConfigKeys))
			g.Expect(actualFRRConfigLabels).To(gomega.Equal(expectedFRRConfigLabels))
			g.Expect(actualFRRConfigSpecs).To(gomega.Equal(expectedFRRConfigSpecs))

			// verify NADs have been annotated as expected
			actualNADs, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions("").List(context.Background(), metav1.ListOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			actualNADAnnotations := map[string]map[string]string{}
			for _, actualNAD := range actualNADs.Items {
				if len(actualNAD.Annotations) != 0 {
					actualNADAnnotations[actualNAD.Name] = actualNAD.Annotations
				}
			}
			for nad, annotations := range tt.expectNADAnnotations {
				for k, v := range annotations {
					g.Expect(actualNADAnnotations[nad]).To(gomega.HaveKeyWithValue(k, v))
				}
			}
		})
	}
}

// unsafeInputTest is a reconcile test fixture: a RouteAdvertisements
// selecting the default network whose source FRRConfiguration carries the
// unsafe neighbor interface name unsafeInputValue, plus the given
// FRRConfigurations generated earlier for it.
type unsafeInputTest struct {
	fakeClientset *util.OVNClusterManagerClientset
	c             *Controller
	// failDelete makes deleting the FRRConfiguration "frrConfig-unvalidated"
	// fail, failNADPatch makes patching NADs fail
	failDelete, failNADPatch atomic.Bool
	// frrConfigNames returns the names of all FRRConfigurations
	frrConfigNames func() []string
	// defaultNADAnnotation returns the default NAD RouteAdvertisements
	// annotation, seeded with ["ra","other"]
	defaultNADAnnotation func() string
}

const unsafeInputValue = "eth0\nx"

func newUnsafeInputTest(t *testing.T, g *gomega.WithT, generated ...*testFRRConfig) *unsafeInputTest {
	t.Helper()
	previousDefault, previousFeatures, previousGateway := config.Default, config.OVNKubernetesFeature, config.Gateway
	t.Cleanup(func() {
		config.Default, config.OVNKubernetesFeature, config.Gateway = previousDefault, previousFeatures, previousGateway
	})
	frrNamespace := "frrNamespace"
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
			HostSubnetLength: 24,
		},
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEVPN = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.Gateway.Mode = config.GatewayModeLocal

	tt := &unsafeInputTest{fakeClientset: util.GetOVNClientset().GetClusterManagerClientset()}
	fakeClientset := tt.fakeClientset
	addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)
	fakeClientset.FRRClient.(Fake).PrependReactor("delete", "frrconfigurations",
		func(action ctesting.Action) (bool, runtime.Object, error) {
			if tt.failDelete.Load() && action.(ctesting.DeleteAction).GetName() == "frrConfig-unvalidated" {
				return true, nil, errors.New("injected delete failure")
			}
			return false, nil, nil
		})
	fakeClientset.NetworkAttchDefClient.(Fake).PrependReactor("patch", "network-attachment-definitions",
		func(_ ctesting.Action) (bool, runtime.Object, error) {
			if tt.failNADPatch.Load() {
				return true, nil, errors.New("injected NAD patch failure")
			}
			return false, nil, nil
		})

	ra := &testRA{Name: "ra", AdvertisePods: true, SelectsDefault: true}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	source := &testFRRConfig{
		Name:      "frrConfig",
		Namespace: frrNamespace,
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Interface: unsafeInputValue},
			}},
		},
	}
	_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).Create(context.Background(), source.FRRConfiguration(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	for _, frrConfig := range generated {
		_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	node := &testNode{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\"}"}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: config.Default.ClusterDefaultNetworkNAD.Namespace}}, metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
	g.Expect(err).ToNot(gomega.HaveOccurred())

	tt.c = NewController(nm.Interface(), wf, fakeClientset)

	// prime the default network NAD, annotated as advertised by this
	// RouteAdvertisements and by another one
	defaultNAD, err := util.EnsureDefaultNetworkNAD(tt.c.nadLister, tt.c.nadClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defaultNAD.Annotations = map[string]string{
		types.OvnNetworkNameAnnotation:  types.DefaultNetworkName,
		types.OvnRouteAdvertisementsKey: "[\"ra\",\"other\"]",
	}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(wf.Shutdown)

	cache.WaitForCacheSync(
		context.Background().Done(),
		wf.RouteAdvertisementsInformer().Informer().HasSynced,
		wf.FRRConfigurationsInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
	)

	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition("ra") })

	tt.frrConfigNames = func() []string {
		frrConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		var names []string
		for _, frrConfig := range frrConfigs.Items {
			names = append(names, frrConfig.Name)
		}
		return names
	}
	tt.defaultNADAnnotation = func() string {
		nad, err := fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Get(context.Background(), defaultNAD.Name, metav1.GetOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return nad.Annotations[types.OvnRouteAdvertisementsKey]
	}
	return tt
}

// generatedFRRConfig returns an FRRConfiguration generated for the test
// RouteAdvertisements on the given node, carrying the validated marker
// annotation or not.
func generatedFRRConfig(name, node string, validated bool) *testFRRConfig {
	annotations := map[string]string{types.OvnRouteAdvertisementsKey: "ra/frrConfig/" + node}
	if validated {
		annotations[types.OvnRouteAdvertisementsValidatedKey] = "true"
	}
	return &testFRRConfig{
		Name:         name,
		Namespace:    "frrNamespace",
		Labels:       map[string]string{types.OvnRouteAdvertisementsKey: "ra"},
		Annotations:  annotations,
		NodeSelector: map[string]string{"kubernetes.io/hostname": node},
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.0.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Interface: "eth0", Advertise: []string{"1.1.0.0/24"}},
			}},
		},
		RawConfig: "router bgp 1\n address-family ipv4 unicast\n  neighbor eth0 allowas-in origin\n exit-address-family\nexit\n!\n",
	}
}

func expectRANotAccepted(g *gomega.WithT, tt *unsafeInputTest) {
	ra, err := tt.fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), "ra", metav1.GetOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	accepted := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil())
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), accepted.Message)
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"))
	g.Expect(accepted.Message).To(gomega.ContainSubstring("invalid neighbor"))
}

// TestController_reconcileDeletesFRRConfigurationsOnUnsafeInput verifies that
// a RouteAdvertisements rejected for an unsafe FRR input value gets the
// FRRConfigurations generated for it before input validation existed deleted
// and, once none is left, its reference removed from the NAD annotations,
// that both are retried when they fail, and that the source FRRConfiguration
// and the references to other RouteAdvertisements are kept.
func TestController_reconcileDeletesFRRConfigurationsOnUnsafeInput(t *testing.T) {
	g := gomega.NewWithT(t)
	tt := newUnsafeInputTest(t, g,
		generatedFRRConfig("frrConfig-unvalidated", "node", false),
		generatedFRRConfig("frrConfig-unvalidated2", "node2", false),
	)

	// deleting one FRRConfiguration fails: the other one is still deleted,
	// the error is returned for a retry and names the validation reason, the
	// NAD reference is kept
	tt.failDelete.Store(true)
	err := tt.c.reconcile("ra")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("injected delete failure"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("frrConfig-unvalidated"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid neighbor"))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "a failed deletion must be retried")
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", "frrConfig-unvalidated"))
	g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal("[\"ra\",\"other\"]"))

	// deletion succeeds on retry but removing the reference from the NAD
	// fails: the error is returned for a retry and names the validation
	// reason, the generated FRRConfigurations are gone
	tt.failDelete.Store(false)
	tt.failNADPatch.Store(true)
	err = tt.c.reconcile("ra")
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("injected NAD patch failure"))
	g.Expect(err.Error()).To(gomega.ContainSubstring("invalid neighbor"))
	g.Expect(errors.Is(err, errConfig)).To(gomega.BeFalse(), "a failed NAD update must be retried")
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig"))
	g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal("[\"ra\",\"other\"]"))

	// everything succeeds on retry: the source FRRConfiguration is kept, the
	// NAD only references the other RouteAdvertisements and this one is not
	// accepted
	tt.failNADPatch.Store(false)
	err = tt.c.reconcile("ra")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig"))
	g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal("[\"other\"]"))
	expectRANotAccepted(g, tt)
}

// TestController_reconcileKeepsValidatedFRRConfigurationsOnUnsafeInput
// verifies that a RouteAdvertisements rejected for an unsafe FRR input value
// keeps the FRRConfigurations generated for it from validated input, and with
// them its reference in the NAD annotations.
func TestController_reconcileKeepsValidatedFRRConfigurationsOnUnsafeInput(t *testing.T) {
	g := gomega.NewWithT(t)
	tt := newUnsafeInputTest(t, g,
		generatedFRRConfig("frrConfig-unvalidated", "node", false),
		generatedFRRConfig("frrConfig-validated", "node2", true),
	)

	err := tt.c.reconcile("ra")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(tt.frrConfigNames()).To(gomega.ConsistOf("frrConfig", "frrConfig-validated"))
	g.Expect(tt.defaultNADAnnotation()).To(gomega.Equal("[\"ra\",\"other\"]"))
	expectRANotAccepted(g, tt)
}

// TestController_reconcileOnNetworkActivity verifies that the controller
// reconciles on network activity changes notified by the network manager: a
// network going active on a node that still has its subnet allocated, as
// after a pod is recreated during the deletion grace period, updates no
// object the controller watches.
func TestController_reconcileOnNetworkActivity(t *testing.T) {
	g := gomega.NewWithT(t)

	frrNamespace := "frrNamespace"
	config.Default.ClusterSubnets = []config.CIDRNetworkEntry{
		{
			CIDR:             ovntest.MustParseIPNet("1.1.0.0/16"),
			HostSubnetLength: 24,
		},
	}
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEgressIP = true
	config.OVNKubernetesFeature.EnableEVPN = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableDynamicUDNAllocation = true
	config.Gateway.Mode = config.GatewayModeLocal

	fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
	addGenerateNameReactor[*frrfake.Clientset](fakeClientset.FRRClient)

	ra := &testRA{Name: "ra", AdvertisePods: true, NetworkSelector: map[string]string{"selected": "true"}}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	frrConfig := &testFRRConfig{
		Name:      "frrConfig",
		Namespace: frrNamespace,
		Routers: []*testRouter{
			{ASN: 1, Prefixes: []string{"1.1.1.0/24"}, Neighbors: []*testNeighbor{
				{ASN: 1, Address: "1.0.0.100"},
			}},
		},
	}
	_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrConfig.Namespace).Create(context.Background(), frrConfig.FRRConfiguration(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nad := &testNAD{Name: "blue", Namespace: "blue", Network: util.GenerateCUDNNetworkName("blue"), Topology: "layer3", Subnet: "1.3.0.0/16", Labels: map[string]string{"selected": "true"}}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Create(context.Background(), nad.NAD(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	// the node still has a subnet allocated for the network, as during the
	// deletion grace period, but no workloads attached to it
	node := &testNode{Name: "node", SubnetsAnnotation: "{\"default\":\"1.1.0.0/24\", \"cluster_udn_blue\":\"1.3.0.0/24\"}"}
	_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node.Node(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	for _, namespace := range []string{"blue", config.Default.ClusterDefaultNetworkNAD.Namespace} {
		_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
	}

	wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	nm, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, wf, fakeClientset, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
	g.Expect(err).ToNot(gomega.HaveOccurred())

	c := NewController(nm.Interface(), wf, fakeClientset)

	// prime the default network NAD
	defaultNAD, err := util.EnsureDefaultNetworkNAD(c.nadLister, c.nadClient)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defaultNAD.Annotations = map[string]string{types.OvnNetworkNameAnnotation: types.DefaultNetworkName}
	_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(defaultNAD.Namespace).Update(context.Background(), defaultNAD, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	err = wf.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer wf.Shutdown()

	cache.WaitForCacheSync(
		context.Background().Done(),
		wf.RouteAdvertisementsInformer().Informer().HasSynced,
		wf.FRRConfigurationsInformer().Informer().HasSynced,
		wf.NADInformer().Informer().HasSynced,
		wf.NodeCoreInformer().Informer().HasSynced,
		wf.EgressIPInformer().Informer().HasSynced,
	)

	// keep the network manager running so that pod events reach its trackers
	err = nm.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer nm.Stop()

	err = c.Start()
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer c.Stop()

	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition("ra") })

	generatedFRRConfigs := func() []string {
		frrConfigs, err := fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(frrNamespace).List(context.Background(), metav1.ListOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		var generated []string
		for _, frrConfig := range frrConfigs.Items {
			if key, ok := frrConfig.Annotations[types.OvnRouteAdvertisementsKey]; ok {
				generated = append(generated, key)
			}
		}
		return generated
	}

	// the network is not active on the node: nothing is advertised
	raAccepted := func() bool {
		ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), "ra", metav1.GetOptions{})
		if err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(ra.Status.Conditions, "Accepted")
	}
	g.Eventually(raAccepted, 5*time.Second).Should(gomega.BeTrue())
	g.Expect(generatedFRRConfigs()).To(gomega.BeEmpty())

	// the initial reconcile cached which networks the RouteAdvertisements
	// selects
	networkName := util.GenerateCUDNNetworkName("blue")
	g.Expect(c.getRAsForNetwork(networkName)).To(gomega.ConsistOf("ra"))
	g.Expect(c.getRAsForNetwork(util.GenerateCUDNNetworkName("red"))).To(gomega.BeEmpty())

	// scheduling a pod on the node activates the network there without
	// updating any object the controller watches: the network manager
	// notification must trigger the reconcile that advertises the node
	pod := &testPod{Name: "pod", Namespace: "blue", Node: "node"}
	_, err = fakeClientset.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.Pod(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())

	g.Eventually(generatedFRRConfigs, 5*time.Second).Should(gomega.ConsistOf("ra/frrConfig/node"))

	// deleting the RouteAdvertisements removes it from the cache
	err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Delete(context.Background(), "ra", metav1.DeleteOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Eventually(func() []string { return c.getRAsForNetwork(networkName) }, 5*time.Second).Should(gomega.BeEmpty())
}

// TestCrossRASelectionEvents checks that claims are reevaluated on RA changes
// without relying on a generated FRRConfiguration event to wake other RAs.
func TestCrossRASelectionEvents(t *testing.T) {
	g := gomega.NewWithT(t)
	oldFeatures := config.OVNKubernetesFeature
	t.Cleanup(func() { config.OVNKubernetesFeature = oldFeatures })
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableRouteAdvertisements = true
	config.OVNKubernetesFeature.EnableEVPN = true
	clients := util.GetOVNClientset().GetClusterManagerClientset()
	wf, err := factory.NewClusterManagerWatchFactory(clients)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating the watch factory")
	g.Expect(wf.Start()).To(gomega.Succeed(), "starting informers for RA selection events")
	t.Cleanup(wf.Shutdown)
	var mu sync.Mutex
	seen := sets.New[string]()
	c := &Controller{}
	c.raController = controllerutil.NewController("cross RA events", &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
		Threadiness:    1,
		Informer:       wf.RouteAdvertisementsInformer().Informer(),
		Lister:         wf.RouteAdvertisementsInformer().Lister().List,
		ObjNeedsUpdate: c.raNeedsUpdate,
		OnDelete:       c.onRADelete,
		Reconcile: func(name string) error {
			mu.Lock()
			defer mu.Unlock()
			seen.Insert(name)
			return nil
		},
	})
	g.Expect(controllerutil.Start(c.raController)).To(gomega.Succeed(), "starting the RA event controller")
	t.Cleanup(func() { controllerutil.Stop(c.raController) })
	readSeen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return sets.List(seen)
	}
	reset := func() {
		mu.Lock()
		defer mu.Unlock()
		seen = sets.New[string]()
	}
	client := clients.RouteAdvertisementsClient.K8sV1().RouteAdvertisements()
	_, err = client.Create(context.Background(), (&testRA{Name: "a"}).RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating initial RouteAdvertisements a")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a"}), "initial RA creation must enqueue reconciliation")
	reset()
	b, err := client.Create(context.Background(), (&testRA{Name: "b"}).RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "creating competing RouteAdvertisements b")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "new claims must wake existing RAs")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.Equal([]string{"a", "b"}), "both affected RAs must remain in the observed reconciliation set")
	reset()
	b.Generation++ // The fake client does not increment generation for spec changes.
	b.Spec.NodeSelector.MatchLabels = map[string]string{"selected": "true"}
	b, err = client.Update(context.Background(), b, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "changing RouteAdvertisements b node selection")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "selection changes must wake other RAs")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.Equal([]string{"a", "b"}), "both affected RAs must remain in the observed reconciliation set")
	reset()
	b.Status.Status = "Accepted"
	_, err = client.UpdateStatus(context.Background(), b, metav1.UpdateOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred(), "updating RouteAdvertisements b status")
	g.Consistently(readSeen, 100*time.Millisecond).Should(gomega.BeEmpty(), "status writes must not create a loop")
	g.Expect(client.Delete(context.Background(), "b", metav1.DeleteOptions{})).To(gomega.Succeed(), "deleting the competing RA")
	g.Eventually(readSeen, 5*time.Second).Should(gomega.Equal([]string{"a", "b"}), "deleting a claim must wake surviving RAs")
}

// TestUpdates verifies which informer changes enqueue dependent RAs and which
// controller-owned or irrelevant updates must remain quiet.
func TestUpdates(t *testing.T) {
	testRAs := []*testRA{
		{
			Name:                     "ra1",
			FRRConfigurationSelector: map[string]string{"select": "1"},
			NetworkSelector:          map[string]string{"select": "1"},
			AdvertiseEgressIPs:       true,
			AdvertisePods:            true,
		},
		{
			Name:                     "ra2",
			FRRConfigurationSelector: map[string]string{"select": "2"},
			NetworkSelector:          map[string]string{"select": "2"},
			NodeSelector:             map[string]string{"select": "2"},
		},
		{
			Name:                     "ra3",
			AdvertiseEgressIPs:       true,
			FRRConfigurationSelector: map[string]string{"select": "3"},
			NetworkSelector:          map[string]string{"select": "3"},
			NodeSelector:             map[string]string{"select": "3"},
		},
	}

	tests := []struct {
		name              string
		oldObject         any
		newObject         any
		expectedReconcile []string
	}{
		{
			name:              "reconciles all RAs when an FRRConfig gets created",
			newObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig gets deleted",
			oldObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig labels get updated",
			oldObject:         &testFRRConfig{Labels: map[string]string{"select": "1"}},
			newObject:         &testFRRConfig{Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig annotation changes",
			oldObject:         &testFRRConfig{Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "A"}},
			newObject:         &testFRRConfig{},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when an FRRConfig spec changes",
			oldObject:         &testFRRConfig{Generation: 1},
			newObject:         &testFRRConfig{Generation: 2},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles a deleted RA referenced from FRRConfig",
			newObject:         &testFRRConfig{Labels: map[string]string{types.OvnRouteAdvertisementsKey: "ra4"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3", "ra4"},
		},
		{
			name:      "does not reconcile an irrelevant update of FRRConfig",
			oldObject: &testFRRConfig{Annotations: map[string]string{"irrelevant": "irrelevant"}},
			newObject: &testFRRConfig{Annotations: map[string]string{"irrelevant": "still-irrelevant"}},
		},
		{
			name:              "reconciles all RAs on own generated FRRConfig update",
			oldObject:         &testFRRConfig{Name: generateName + "owner", Generation: 1},
			newObject:         &testFRRConfig{Name: generateName + "owner", Generation: 2, OwnUpdate: true},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:      "ignores status-only own generated FRRConfig update",
			oldObject: &testFRRConfig{Name: generateName + "owner", Generation: 1},
			newObject: &testFRRConfig{Name: generateName + "owner", Generation: 1, OwnUpdate: true},
		},
		{
			name:      "does not reconcile own update of FRRConfig",
			oldObject: &testFRRConfig{Generation: 1},
			newObject: &testFRRConfig{Generation: 2, OwnUpdate: true},
		},
		{
			name:      "does not reconcile own update of FRRConfig",
			oldObject: &testFRRConfig{Generation: 1},
			newObject: &testFRRConfig{Generation: 2, OwnUpdate: true},
		},
		{
			name:              "reconciles all RAs on new NAD",
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on deleted NAD",
			oldObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when NAD labels change",
			oldObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when NAD annotation changes",
			oldObject:         &testNAD{Name: "net", Namespace: "net", OwnUpdate: true, Labels: map[string]string{"select": "2"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
			newObject:         &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles a deleted RA referenced from NAD",
			newObject:         &testNAD{Name: "net", Namespace: "net", Network: "net", Topology: "layer3", Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra4\"]"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3", "ra4"},
		},
		{
			name:      "does not reconcile own update of NAD",
			oldObject: &testNAD{Name: "net", Namespace: "net", Labels: map[string]string{"select": "2"}},
			newObject: &testNAD{Name: "net", Namespace: "net", OwnUpdate: true, Labels: map[string]string{"select": "2"}, Annotations: map[string]string{types.OvnRouteAdvertisementsKey: "[\"ra2\"]"}},
		},
		{
			name:      "does not reconcile a new unsupported (secondary) NAD",
			newObject: &testNAD{Name: "net", Namespace: "net", Network: "net", IsSecondary: true, Topology: "layer3", Labels: map[string]string{"select": "2"}},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on new EIP with status",
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on deleted EIP with status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated EIP status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip2"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated EIP status",
			oldObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}},
			newObject:         &testEIP{Name: "eip", EIPs: map[string]string{"node": "ip"}, NamespaceSelector: map[string]string{"selected": "true"}},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:              "reconciles all RAs that advertise EIPs on updated namespace labels",
			oldObject:         &testNamespace{Name: "ns1", Labels: map[string]string{"selected": "true"}},
			newObject:         &testNamespace{Name: "ns1"},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:      "does not reconcile RAs on new EIP with no status",
			newObject: &testEIP{Name: "eip"},
		},
		{
			// TODO shouldn't happen but needs FIX in controller utility which
			// does not call filter predicate on deletes
			name:              "reconciles all RAs that advertise EIPs on deleted EIP",
			oldObject:         &testEIP{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra3"},
		},
		{
			name:      "does not reconcile RAs on updated EIP with no status update",
			oldObject: &testEIP{Name: "eip", Generation: 1, EIPs: map[string]string{"node": "ip"}},
			newObject: &testEIP{Name: "eip", Generation: 2, EIPs: map[string]string{"node": "ip"}},
		},
		{
			name:              "reconciles all RAs on new Node",
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on deleted Node",
			oldObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node labels",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", Labels: map[string]string{"select": "1"}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node subnet annotation",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", SubnetsAnnotation: "subnets"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node primary address annotation",
			oldObject:         &testNode{Name: "eip", PrimaryAddressAnnotation: "old"},
			newObject:         &testNode{Name: "eip", PrimaryAddressAnnotation: "new"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when the Node becomes a DPU host",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", DPUHostAddressAnnotation: `{"ipv4":"172.18.255.254/16"}`},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs when the Node stops being a DPU host",
			oldObject:         &testNode{Name: "eip", DPUHostAddressAnnotation: `{"ipv4":"172.18.255.254/16"}`},
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name: "reconciles all RAs on updated Node L3 gateway annotation",
			oldObject: &testNode{
				Name:                      "eip",
				L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:4c:e6:00","ip-addresses":["172.18.255.254/16"],"next-hops":["172.18.0.1"]}}`,
			},
			newObject: &testNode{
				Name:                      "eip",
				L3GatewayConfigAnnotation: `{"default":{"mode":"shared","mac-address":"52:54:00:d4:ac:00","ip-addresses":["172.18.255.253/16"],"next-hops":["172.18.0.1"]}}`,
			},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on updated Node chassis ID annotation",
			oldObject:         &testNode{Name: "eip", ChassisIDAnnotation: "old"},
			newObject:         &testNode{Name: "eip", ChassisIDAnnotation: "new"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on new Node VTEP annotation",
			oldObject:         &testNode{Name: "eip"},
			newObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on removed Node VTEP annotation",
			oldObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			newObject:         &testNode{Name: "eip"},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:              "reconciles all RAs on changed Node VTEP annotation",
			oldObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.1"}}},
			newObject:         &testNode{Name: "eip", VTEPIPs: map[string][]string{"my-vtep": {"100.64.0.2"}}},
			expectedReconcile: []string{"ra1", "ra2", "ra3"},
		},
		{
			name:      "does not reconcile RAs on node irrelevant change",
			oldObject: &testNode{Name: "eip", Generation: 1},
			newObject: &testNode{Name: "eip", Generation: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			gMaxLength := format.MaxLength
			format.MaxLength = 0
			defer func() { format.MaxLength = gMaxLength }()

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			config.OVNKubernetesFeature.EnableEgressIP = true
			config.OVNKubernetesFeature.EnableEVPN = true

			fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()

			wf, err := factory.NewClusterManagerWatchFactory(fakeClientset)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			reconciled := []string{}
			reconciledMutex := sync.Mutex{}
			reconcile := func(ra string) error {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				reconciled = append(reconciled, ra)
				return nil
			}
			matchReconciledRAs := func(g gomega.Gomega, expected []string) {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				g.Expect(reconciled).To(gomega.ConsistOf(expected))
			}
			resetReconciles := func() {
				reconciledMutex.Lock()
				defer reconciledMutex.Unlock()
				reconciled = []string{}
			}

			c := NewController(networkmanager.Default().Interface(), wf, fakeClientset)
			config := &controllerutil.ControllerConfig[ratypes.RouteAdvertisements]{
				RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
				Reconcile:      reconcile,
				Threadiness:    1,
				Informer:       wf.RouteAdvertisementsInformer().Informer(),
				Lister:         wf.RouteAdvertisementsInformer().Lister().List,
				ObjNeedsUpdate: raNeedsUpdate,
			}
			c.raController = controllerutil.NewController("", config)

			err = c.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer c.Stop()

			createObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Create(context.Background(), t.FRRConfiguration(), metav1.CreateOptions{})
				case *testNAD:
					_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Create(context.Background(), t.NAD(), metav1.CreateOptions{})
				case *testEIP:
					_, err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Create(context.Background(), t.EgressIP(), metav1.CreateOptions{})
				case *testNode:
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), t.Node(), metav1.CreateOptions{})
				case *testNamespace:
					_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Create(context.Background(), t.Namespace(), metav1.CreateOptions{})
				}
				return err
			}
			updateObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					_, err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Update(context.Background(), t.FRRConfiguration(), metav1.UpdateOptions{})
				case *testNAD:
					_, err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Update(context.Background(), t.NAD(), metav1.UpdateOptions{})
				case *testEIP:
					_, err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Update(context.Background(), t.EgressIP(), metav1.UpdateOptions{})
				case *testNode:
					_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), t.Node(), metav1.UpdateOptions{})
				case *testNamespace:
					_, err = fakeClientset.KubeClient.CoreV1().Namespaces().Update(context.Background(), t.Namespace(), metav1.UpdateOptions{})
				}
				return err
			}
			deleteObj := func(obj any) error {
				var err error
				switch t := obj.(type) {
				case *testFRRConfig:
					err = fakeClientset.FRRClient.ApiV1beta1().FRRConfigurations(t.Namespace).Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNAD:
					err = fakeClientset.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(t.Namespace).Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testEIP:
					err = fakeClientset.EgressIPClient.K8sV1().EgressIPs().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNode:
					err = fakeClientset.KubeClient.CoreV1().Nodes().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				case *testNamespace:
					err = fakeClientset.KubeClient.CoreV1().Namespaces().Delete(context.Background(), t.Name, metav1.DeleteOptions{})
				}
				return err
			}

			if tt.oldObject != nil {
				err = createObj(tt.oldObject)
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// since we haven't created the RAs yet, this should not reconcile anything
			g.Consistently(matchReconciledRAs).WithArguments([]string{}).Should(gomega.Succeed())

			var raNames []string
			for _, t := range testRAs {
				raNames = append(raNames, t.Name)
				_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), t.RouteAdvertisements(), metav1.CreateOptions{})
				g.Expect(err).ToNot(gomega.HaveOccurred())
			}

			// creating the testRAs, should reconcile them
			g.Eventually(matchReconciledRAs).WithArguments(raNames).Should(gomega.Succeed())
			g.Consistently(matchReconciledRAs).WithArguments(raNames).Should(gomega.Succeed())
			// reset for the actual test
			resetReconciles()

			switch {
			case tt.newObject != nil && tt.oldObject == nil:
				err = createObj(tt.newObject)
			case tt.newObject != nil:
				err = updateObj(tt.newObject)
			default:
				err = deleteObj(tt.oldObject)
			}
			g.Expect(err).ToNot(gomega.HaveOccurred())

			g.Eventually(matchReconciledRAs).WithArguments(tt.expectedReconcile).Should(gomega.Succeed())
			g.Consistently(matchReconciledRAs).WithArguments(tt.expectedReconcile).Should(gomega.Succeed())
		})
	}
}

func getRAConditionMetricValue(nameLabel, conditionLabel, statusLabel string) (float64, bool) {
	metricName := prometheus.BuildFQName(types.MetricOvnkubeNamespace, types.MetricOvnkubeSubsystemClusterManager, "route_advertisement_condition")
	return ovntest.GetConditionMetricValue(metricName, nameLabel, conditionLabel, statusLabel)
}

// TestController_updateRAStatus verifies that the 'Accepted' condition is
// refreshed when consecutive reconciles fail for different reasons, so that
// the status message never gets stale.
func TestController_updateRAStatus(t *testing.T) {
	g := gomega.NewWithT(t)

	fakeClientset := util.GetOVNClientset().GetClusterManagerClientset()
	c := &Controller{raClient: fakeClientset.RouteAdvertisementsClient}

	raName := "ra"
	tra := &testRA{Name: raName}
	_, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tra.RouteAdvertisements(), metav1.CreateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	t.Cleanup(func() { metrics.DeleteRouteAdvertisementCondition(raName) })

	getRA := func() *ratypes.RouteAdvertisements {
		ra, err := fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Get(context.Background(), raName, metav1.GetOptions{})
		g.Expect(err).ToNot(gomega.HaveOccurred())
		return ra
	}

	// first reconcile fails with error A
	err = c.updateRAStatus(getRA(), false, fmt.Errorf("%w: error A", errConfig))
	g.Expect(err).ToNot(gomega.HaveOccurred(), "first reconcile with error A should update the status")
	accepted := meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should be set after the first reconcile")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should be false after error A")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"), "errConfig should be reported as ConfigurationError")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error A"), "the message should report error A")

	// rewind the condition transition time one hour into the past, so we can
	// tell whether subsequent refreshes preserve or bump it: condition
	// timestamps have one-second resolution and the whole test runs within a
	// single second, so against a "now" timestamp an unchanged and a wrongly
	// re-stamped transition time would look identical
	rewound := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	ra := getRA()
	meta.FindStatusCondition(ra.Status.Conditions, "Accepted").LastTransitionTime = rewound
	_, err = fakeClientset.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
	g.Expect(err).ToNot(gomega.HaveOccurred(), "rewinding the condition transition time should succeed")

	// second reconcile fails with error B for the same reason: the message
	// must be refreshed
	err = c.updateRAStatus(getRA(), false, fmt.Errorf("%w: error B", errConfig))
	g.Expect(err).ToNot(gomega.HaveOccurred(), "second reconcile with error B should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after error B")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should stay false after error B")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationError"), "errConfig should still be reported as ConfigurationError")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error B"), "the message should be refreshed to error B")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a message-only refresh should preserve the transition time")

	// third reconcile fails with a different reason: reason and message must
	// be refreshed
	err = c.updateRAStatus(getRA(), false, errors.Join(errInvalidVTEP, fmt.Errorf("%w: error C", errPending)))
	g.Expect(err).ToNot(gomega.HaveOccurred(), "third reconcile with error C should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after error C")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionFalse), "the Accepted condition should stay false after error C")
	g.Expect(accepted.Reason).To(gomega.Equal("ConfigurationPending"), "errPending should be reported as ConfigurationPending")
	g.Expect(accepted.Message).To(gomega.ContainSubstring("error C"), "the message should be refreshed to error C")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a reason/message refresh should preserve the transition time")

	// fourth reconcile fails with the same error: the status must not be
	// applied again
	countStatusPatches := func() int {
		patches := 0
		for _, action := range fakeClientset.RouteAdvertisementsClient.(*rafake.Clientset).Actions() {
			if action.GetVerb() == "patch" {
				patches++
			}
		}
		return patches
	}
	patches := countStatusPatches()
	err = c.updateRAStatus(getRA(), false, errors.Join(errInvalidVTEP, fmt.Errorf("%w: error C", errPending)))
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a reconcile with an unchanged error should succeed")
	g.Expect(countStatusPatches()).To(gomega.Equal(patches), "an unchanged status must not be re-applied")

	// fifth reconcile fails with the same error but had FRRConfig/NAD updates:
	// the status must be applied even though the condition is unchanged
	err = c.updateRAStatus(getRA(), true, errors.Join(errInvalidVTEP, fmt.Errorf("%w: error C", errPending)))
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a reconcile with updates should update the status")
	g.Expect(countStatusPatches()).To(gomega.Equal(patches+1), "hadUpdates should apply the status even when the condition is unchanged")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally("==", rewound.Time), "a hadUpdates apply should preserve the transition time of an unchanged condition")

	// sixth reconcile succeeds: the condition status flips and the transition
	// time must be bumped
	err = c.updateRAStatus(getRA(), false, nil)
	g.Expect(err).ToNot(gomega.HaveOccurred(), "a successful reconcile should update the status")
	accepted = meta.FindStatusCondition(getRA().Status.Conditions, "Accepted")
	g.Expect(accepted).NotTo(gomega.BeNil(), "the Accepted condition should still be set after a successful reconcile")
	g.Expect(accepted.Status).To(gomega.Equal(metav1.ConditionTrue), "the Accepted condition should be true after a successful reconcile")
	g.Expect(accepted.LastTransitionTime.Time).To(gomega.BeTemporally(">", rewound.Time), "a status flip should bump the transition time")
}
