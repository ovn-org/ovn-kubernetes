package util

import (
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestCreateSubnetAnnotation(t *testing.T) {
	// TODO: test has to be enhanced to compare outputs in a future iteration
	tests := []struct {
		desc              string
		inpAnnotName      string
		inpDefaultSubnets []string
		expectedErr       bool
	}{
		{
			desc:              "non-zero length annotation name and subnet list size of ONE provided as input",
			inpAnnotName:      ovnNodeSubnets,
			inpDefaultSubnets: []string{"192.168.1.12/24"},
		},
		{
			desc:              "empty annotation name provided as input",
			inpAnnotName:      "",
			inpDefaultSubnets: []string{"192.168.1.12/24"},
		},
		{
			desc:              "non-zero length annotation name and subnet list size greater than ONE provided as input",
			inpAnnotName:      ovnNodeSubnets,
			inpDefaultSubnets: []string{"192.168.1.12/24", "fd02:0:0:2::2895/64"},
		},
		{
			desc:              "subnet list of size 0 provided as input",
			inpAnnotName:      ovnNodeSubnets,
			inpDefaultSubnets: []string{},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			var defSubList []*net.IPNet
			for _, item := range tc.inpDefaultSubnets {
				_, ipnet, err := net.ParseCIDR(item)
				if err != nil {
					t.Fail()
				}
				defSubList = append(defSubList, ipnet)
			}
			mapRes := map[string]string{}
			e := updateSubnetAnnotation(mapRes, tc.inpAnnotName, types.DefaultNetworkName, defSubList)
			if tc.expectedErr {
				assert.Error(t, e)
			}
			t.Log(mapRes[tc.inpAnnotName], e)
		})
	}
}

func TestSetSubnetAnnotation(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{})
	k := &kube.Kube{KClient: fakeClient}
	testAnnotator := kube.NewNodeAnnotator(k, "")
	tests := []struct {
		desc             string
		inpNodeAnnotator kube.Annotator
		inpAnnotName     string
		inpDefSubnetIps  []*net.IPNet
		errExp           bool
	}{
		{
			desc:             "tests function coverage, success path",
			inpNodeAnnotator: testAnnotator,
			inpAnnotName:     ovnNodeSubnets,
			inpDefSubnetIps:  ovntest.MustParseIPNets("192.168.1.12/24"),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			err := setSubnetAnnotation(tc.inpNodeAnnotator, tc.inpAnnotName, tc.inpDefSubnetIps)
			t.Log(err)
			if tc.errExp {
				assert.NotNil(t, err)
			}
		})
	}
}

func TestParseSubnetAnnotation(t *testing.T) {
	tests := []struct {
		desc        string
		inpNode     v1.Node
		annName     string
		errExpected bool
	}{
		{
			desc:        "incorrect annotation",
			annName:     "blah",
			errExpected: true,
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
		},
		{
			desc:    "correct annotation with one subnet",
			annName: ovnNodeSubnets,
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
		},
		{
			desc:    "parse as dual-stack",
			annName: ovnNodeSubnets,
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\": [\"10.244.0.0/24\", \"fd02:0:0:2::2895/64\"]}",
					},
				},
			},
		},
		{
			desc:        "error:cannot parse as single or dual stack",
			annName:     ovnNodeSubnets,
			errExpected: true,
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\": [\"10.244.0.0/24\", \"\"fd02:0:0:2::2895/64\"]}", //added the extra \" in front of fd02: to cause json.Unmarshal error
					},
				},
			},
		},
		{
			desc:        "error: annotation has no default network",
			annName:     ovnNodeSubnets,
			errExpected: true,
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{}",
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ipListMap, e := parseSubnetAnnotation(tc.inpNode.Annotations, tc.annName)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			} else {
				ipList := ipListMap[types.DefaultNetworkName]
				t.Log(ipList)
				assert.Greater(t, len(ipList), 0)
			}
		})
	}
}

func TestNodeSubnetAnnotationChanged(t *testing.T) {
	tests := []struct {
		desc    string
		oldNode *v1.Node
		newNode *v1.Node
		result  bool
	}{
		{
			desc: "true: annotation changed",
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
			result: true,
		},
		{
			desc: "true: annotation's value changed",
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.2.0/24\"}",
					},
				},
			},
			result: true,
		},
		{
			desc: "false: annotation didn't change",
			newNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
			oldNode: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
			result: false,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			result := NodeSubnetAnnotationChanged(tc.oldNode, tc.newNode)
			assert.Equal(t, tc.result, result)
		})
	}
}

func TestCreateNodeHostSubnetAnnotation(t *testing.T) {
	tests := []struct {
		desc            string
		inpDefSubnetIps []*net.IPNet
		outExp          map[string]string
		errExp          bool
	}{
		{
			desc:            "success path, valid default subnets",
			inpDefSubnetIps: ovntest.MustParseIPNets("192.168.1.12/24"),
			outExp: map[string]string{
				"k8s.ovn.org/node-subnets": "{\"default\":[\"192.168.1.12/24\"]}",
			},
		},
		{
			desc:   "success path, inpDefSubnetIps is nil",
			outExp: map[string]string{},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := UpdateNodeHostSubnetAnnotation(nil, tc.inpDefSubnetIps, types.DefaultNetworkName)
			t.Log(res, err)
			if tc.errExp {
				assert.NotNil(t, err)
			} else {
				assert.True(t, reflect.DeepEqual(res, tc.outExp))
			}
		})
	}
}

func TestSetNodeHostSubnetAnnotation(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{})
	k := &kube.Kube{KClient: fakeClient}
	testAnnotator := kube.NewNodeAnnotator(k, "")

	tests := []struct {
		desc             string
		inpNodeAnnotator kube.Annotator
		inpDefSubnetIps  []*net.IPNet
		errExp           bool
	}{
		{
			desc:             "tests function coverage, success path",
			inpNodeAnnotator: testAnnotator,
			inpDefSubnetIps:  ovntest.MustParseIPNets("192.168.1.12/24"),
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			err := SetNodeHostSubnetAnnotation(tc.inpNodeAnnotator, tc.inpDefSubnetIps)
			t.Log(err)
			if tc.errExp {
				assert.NotNil(t, err)
			}
		})
	}
}

func TestDeleteNodeHostSubnetAnnotation(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(&v1.NodeList{})
	k := &kube.Kube{KClient: fakeClient}
	testAnnotator := kube.NewNodeAnnotator(k, "")

	tests := []struct {
		desc             string
		inpNodeAnnotator kube.Annotator
	}{
		{
			desc:             "function code coverage",
			inpNodeAnnotator: testAnnotator,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			DeleteNodeHostSubnetAnnotation(tc.inpNodeAnnotator)
		})
	}
}

func TestParseNodeHostSubnetAnnotation(t *testing.T) {
	tests := []struct {
		desc    string
		inpNode v1.Node
		errExp  bool
	}{
		{
			desc: "tests function coverage, success path",
			inpNode: v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "testNode",
					Annotations: map[string]string{
						"k8s.ovn.org/node-subnets": "{\"default\":\"10.244.0.0/24\"}",
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, err := ParseNodeHostSubnetAnnotation(&tc.inpNode, types.DefaultNetworkName)
			t.Log(res, err)
			if tc.errExp {
				assert.NotNil(t, err)
			} else {
				assert.NotNil(t, res)
			}
		})
	}
}

//func NodeSubnetAnnotationChangedForNetwork(oldNode, newNode *v1.Node, netName string) bool {
//	var oldSubnets, newSubnets map[string]json.RawMessage
//
//	if err := json.Unmarshal([]byte(oldNode.Annotations[ovnNodeSubnets]), &oldSubnets); err != nil {
//		klog.Errorf("Failed to unmarshal old node %s annotation: %v", oldNode.Name, err)
//		return true
//	}
//	if err := json.Unmarshal([]byte(newNode.Annotations[ovnNodeSubnets]), &newSubnets); err != nil {
//		klog.Errorf("Failed to unmarshal new node %s annotation: %v", newNode.Name, err)
//		return true
//	}
//	return !bytes.Equal(oldSubnets[netName], newSubnets[netName])
//}

const networksCount = 4096

func BenchmarkParseNodeHostSubnetAnnotation(b *testing.B) {
	var oldNode, newNode v1.Node
	oldNode.Annotations = make(map[string]string)
	newNode.Annotations = make(map[string]string)

	oldNode.Annotations[ovnNodeSubnets] = "{\"default\":\"10.244.0.0/24\""
	newNode.Annotations[ovnNodeSubnets] = "{\"default\":\"10.244.0.0/24\""
	for n := 0; n < networksCount; n++ {
		net := fmt.Sprintf(", \"net-%d\":[\"10.243.0.0/24\"]", n)
		oldNode.Annotations[ovnNodeSubnets] += net
		newNode.Annotations[ovnNodeSubnets] += net
	}
	oldNode.Annotations[ovnNodeSubnets] += "}"
	newNode.Annotations[ovnNodeSubnets] += "}"

	//klog.Errorf("%s", oldNode.Annotations[ovnNodeSubnets])
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		assert.False(b, NodeSubnetAnnotationChangedForNetwork(&oldNode, &newNode, fmt.Sprintf("net-%d", rand.Int63n(networksCount))))
	}
}

func nodeSubnetChanged(oldNode, node *v1.Node, netName string) bool {
	oldSubnets, _ := ParseNodeHostSubnetAnnotation(oldNode, netName)
	newSubnets, _ := ParseNodeHostSubnetAnnotation(node, netName)
	return !reflect.DeepEqual(oldSubnets, newSubnets)
}

func BenchmarkParseNodeHostSubnetAnnotationOld(b *testing.B) {
	var oldNode, newNode v1.Node
	oldNode.Annotations = make(map[string]string)
	newNode.Annotations = make(map[string]string)

	oldNode.Annotations[ovnNodeSubnets] = "{\"default\":\"10.244.0.0/24\""
	newNode.Annotations[ovnNodeSubnets] = "{\"default\":\"10.244.0.0/24\""
	for n := 0; n < networksCount; n++ {
		net := fmt.Sprintf(", \"net-%d\":\"10.243.0.0/24\"", n)
		oldNode.Annotations[ovnNodeSubnets] += net
		newNode.Annotations[ovnNodeSubnets] += net
	}
	oldNode.Annotations[ovnNodeSubnets] += "}"
	newNode.Annotations[ovnNodeSubnets] += "}"

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		assert.False(b, nodeSubnetChanged(&oldNode, &newNode, fmt.Sprintf("net-%d", rand.Int63n(networksCount))))
	}
}
