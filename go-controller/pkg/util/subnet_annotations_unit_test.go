package util

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net"
	"testing"
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
			mapRes, e := createSubnetAnnotation(tc.inpAnnotName, defSubList)
			if tc.expectedErr {
				assert.Error(t, e)
			}
			t.Log(mapRes[tc.inpAnnotName], e)
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
						"k8s.ovn.org/node-subnets":      "{\"default\":\"10.244.0.0/24\"}",
						"k8s.ovn.org/node-join-subnets": "{\"default\":\"100.64.0.0/29\"}",
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
						"k8s.ovn.org/node-subnets":      "{\"default\":\"10.244.0.0/24\"}",
						"k8s.ovn.org/node-join-subnets": "{\"default\":\"100.64.0.0/29\"}",
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
						"k8s.ovn.org/node-subnets":      "{\"default\": [\"10.244.0.0/24\", \"fd02:0:0:2::2895/64\"]}",
						"k8s.ovn.org/node-join-subnets": "{\"default\": [\"100.64.0.0/29\", \"fd99::10/125\"]}",
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
						"k8s.ovn.org/node-subnets":      "{\"default\": [\"10.244.0.0/24\", \"\"fd02:0:0:2::2895/64\"]}", //added the extra \" in front of fd02: to cause json.Unmarshal error
						"k8s.ovn.org/node-join-subnets": "{\"default\": [\"100.64.0.0/29\", \"fd99::10/125\"]}",
					},
				},
			},
		},
		// TODO: need a test case for verifying "annotation has no default network error"
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ipList, e := parseSubnetAnnotation(&tc.inpNode, tc.annName)
			if tc.errExpected {
				t.Log(e)
				assert.Error(t, e)
			} else {
				t.Log(ipList)
				assert.Greater(t, len(ipList), 0)
			}
		})
	}
}
