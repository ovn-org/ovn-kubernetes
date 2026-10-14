// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	networkconnectv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1"
	networkconnectfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/clusternetworkconnect/v1/apis/clientset/versioned/fake"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

func TestUpdateNetworkConnectSubnetAnnotation(t *testing.T) {
	tests := []struct {
		name              string
		cncName           string
		allocatedSubnets  map[string][]*net.IPNet
		expectedSubnetMap map[string]NetworkConnectSubnetAnnotation
		expectError       bool
	}{
		{
			name:    "single IPv4 subnet for one network",
			cncName: "test-cnc",
			allocatedSubnets: map[string][]*net.IPNet{
				"network1": {ovntest.MustParseIPNet("10.0.0.0/24")},
			},
			expectedSubnetMap: map[string]NetworkConnectSubnetAnnotation{
				"network1": {IPv4: "10.0.0.0/24"},
			},
			expectError: false,
		},
		{
			name:    "single IPv6 subnet for one network",
			cncName: "test-cnc",
			allocatedSubnets: map[string][]*net.IPNet{
				"network1": {ovntest.MustParseIPNet("fd00::/64")},
			},
			expectedSubnetMap: map[string]NetworkConnectSubnetAnnotation{
				"network1": {IPv6: "fd00::/64"},
			},
			expectError: false,
		},
		{
			name:    "dual-stack subnets for one network",
			cncName: "test-cnc",
			allocatedSubnets: map[string][]*net.IPNet{
				"network1": {
					ovntest.MustParseIPNet("10.0.0.0/24"),
					ovntest.MustParseIPNet("fd00::/64"),
				},
			},
			expectedSubnetMap: map[string]NetworkConnectSubnetAnnotation{
				"network1": {IPv4: "10.0.0.0/24", IPv6: "fd00::/64"},
			},
			expectError: false,
		},
		{
			name:    "multiple networks with different subnets",
			cncName: "test-cnc",
			allocatedSubnets: map[string][]*net.IPNet{
				"network1": {ovntest.MustParseIPNet("10.0.1.0/24")},
				"network2": {ovntest.MustParseIPNet("10.0.2.0/24")},
				"network3": {
					ovntest.MustParseIPNet("10.0.3.0/24"),
					ovntest.MustParseIPNet("fd00:3::/64"),
				},
			},
			expectedSubnetMap: map[string]NetworkConnectSubnetAnnotation{
				"network1": {IPv4: "10.0.1.0/24"},
				"network2": {IPv4: "10.0.2.0/24"},
				"network3": {IPv4: "10.0.3.0/24", IPv6: "fd00:3::/64"},
			},
			expectError: false,
		},
		{
			name:              "empty allocated subnets",
			cncName:           "test-cnc",
			allocatedSubnets:  map[string][]*net.IPNet{},
			expectedSubnetMap: map[string]NetworkConnectSubnetAnnotation{},
			expectError:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a CNC object
			cnc := &networkconnectv1.ClusterNetworkConnect{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "k8s.ovn.org/v1",
					Kind:       "ClusterNetworkConnect",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.cncName,
				},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{},
			}

			// Create fake client with Apply reactor
			fakeClient := networkconnectfake.NewSimpleClientset(cnc)
			ovntest.AddNetworkConnectApplyReactor(fakeClient)

			// Call the function under test
			err := UpdateNetworkConnectSubnetAnnotation(cnc, fakeClient, tt.allocatedSubnets)

			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Get the updated CNC from the fake client
			updatedCNC, err := fakeClient.K8sV1().ClusterNetworkConnects().Get(
				context.Background(), tt.cncName, metav1.GetOptions{})
			require.NoError(t, err)

			// Verify the annotation was set correctly
			annotationValue, ok := updatedCNC.Annotations[ovnNetworkConnectSubnetAnnotation]
			if len(tt.expectedSubnetMap) == 0 {
				// For empty subnets, we still expect the annotation to be set (as empty JSON object)
				assert.True(t, ok, "expected annotation to be set")
				assert.Equal(t, "{}", annotationValue)
			} else {
				assert.True(t, ok, "expected annotation to be set")

				// Parse the annotation and compare
				var actualSubnetMap map[string]NetworkConnectSubnetAnnotation
				err = json.Unmarshal([]byte(annotationValue), &actualSubnetMap)
				require.NoError(t, err)

				assert.Equal(t, tt.expectedSubnetMap, actualSubnetMap)
			}
		})
	}
}

func TestUpdateNetworkConnectRouterTunnelKeyAnnotation(t *testing.T) {
	tests := []struct {
		name           string
		cncName        string
		tunnelID       int
		expectedTunnel string
		existingAnnots map[string]string
	}{
		{
			name:           "set tunnel key on existing CNC",
			cncName:        "test-cnc",
			tunnelID:       12345,
			expectedTunnel: "12345",
		},
		{
			name:           "set tunnel key with zero value",
			cncName:        "test-cnc-zero",
			tunnelID:       0,
			expectedTunnel: "0",
		},
		{
			name:           "set large tunnel key",
			cncName:        "test-cnc-large",
			tunnelID:       2147483647, // max int32
			expectedTunnel: "2147483647",
		},
		{
			name:           "update existing tunnel key",
			cncName:        "test-cnc-update",
			tunnelID:       99999,
			expectedTunnel: "99999",
			existingAnnots: map[string]string{
				OvnConnectRouterTunnelKeyAnnotation: "11111",
			},
		},
		{
			name:           "preserve other annotations when setting tunnel key",
			cncName:        "test-cnc-preserve",
			tunnelID:       54321,
			expectedTunnel: "54321",
			existingAnnots: map[string]string{
				"other-annotation": "other-value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cnc := &networkconnectv1.ClusterNetworkConnect{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "k8s.ovn.org/v1",
					Kind:       "ClusterNetworkConnect",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:        tt.cncName,
					Annotations: tt.existingAnnots,
				},
				Spec: networkconnectv1.ClusterNetworkConnectSpec{},
			}
			fakeClient := networkconnectfake.NewSimpleClientset(cnc)
			ovntest.AddNetworkConnectApplyReactor(fakeClient)

			err := UpdateNetworkConnectRouterTunnelKeyAnnotation(tt.cncName, fakeClient, tt.tunnelID)
			require.NoError(t, err)

			updatedCNC, err := fakeClient.K8sV1().ClusterNetworkConnects().Get(
				context.Background(), tt.cncName, metav1.GetOptions{})
			require.NoError(t, err)

			// Verify the tunnel key annotation was set correctly
			tunnelValue, ok := updatedCNC.Annotations[OvnConnectRouterTunnelKeyAnnotation]
			assert.True(t, ok, "expected tunnel key annotation to be set")
			assert.Equal(t, tt.expectedTunnel, tunnelValue)

			// Verify other annotations are preserved
			for k, v := range tt.existingAnnots {
				if k != OvnConnectRouterTunnelKeyAnnotation {
					assert.Equal(t, v, updatedCNC.Annotations[k],
						"expected annotation %s to be preserved", k)
				}
			}
		})
	}
}

func TestBothAnnotationsCanCoexist(t *testing.T) {
	cncName := "test-cnc-coexist"

	// Create a CNC object
	cnc := &networkconnectv1.ClusterNetworkConnect{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "k8s.ovn.org/v1",
			Kind:       "ClusterNetworkConnect",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: cncName,
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{},
	}

	// Create fake client with Apply reactor
	fakeClient := networkconnectfake.NewSimpleClientset(cnc)
	ovntest.AddNetworkConnectApplyReactor(fakeClient)

	// First, set the subnet annotation
	allocatedSubnets := map[string][]*net.IPNet{
		"network1": {ovntest.MustParseIPNet("10.0.0.0/24")},
	}
	err := UpdateNetworkConnectSubnetAnnotation(cnc, fakeClient, allocatedSubnets)
	require.NoError(t, err)

	// Then, set the tunnel key annotation
	err = UpdateNetworkConnectRouterTunnelKeyAnnotation(cncName, fakeClient, 12345)
	require.NoError(t, err)

	// Get the final CNC
	finalCNC, err := fakeClient.K8sV1().ClusterNetworkConnects().Get(
		context.Background(), cncName, metav1.GetOptions{})
	require.NoError(t, err)

	// Verify both annotations exist
	_, hasSubnetAnnot := finalCNC.Annotations[ovnNetworkConnectSubnetAnnotation]
	_, hasTunnelAnnot := finalCNC.Annotations[OvnConnectRouterTunnelKeyAnnotation]

	assert.True(t, hasSubnetAnnot, "expected subnet annotation to be present")
	assert.True(t, hasTunnelAnnot, "expected tunnel key annotation to be present")

	// Verify the tunnel key value
	assert.Equal(t, "12345", finalCNC.Annotations[OvnConnectRouterTunnelKeyAnnotation])

	// Verify the subnet annotation value
	var subnetMap map[string]NetworkConnectSubnetAnnotation
	err = json.Unmarshal([]byte(finalCNC.Annotations[ovnNetworkConnectSubnetAnnotation]), &subnetMap)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.0/24", subnetMap["network1"].IPv4)
}

func TestUpdateNetworkConnectSubnetAnnotation_PreservesExistingAnnotations(t *testing.T) {
	cncName := "test-cnc-preserve"

	// Create a CNC object with existing annotations
	cnc := &networkconnectv1.ClusterNetworkConnect{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "k8s.ovn.org/v1",
			Kind:       "ClusterNetworkConnect",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: cncName,
			Annotations: map[string]string{
				"existing-annotation":               "existing-value",
				OvnConnectRouterTunnelKeyAnnotation: "99999",
			},
		},
		Spec: networkconnectv1.ClusterNetworkConnectSpec{},
	}

	// Create fake client with Apply reactor
	fakeClient := networkconnectfake.NewSimpleClientset(cnc)
	ovntest.AddNetworkConnectApplyReactor(fakeClient)

	// Set the subnet annotation
	allocatedSubnets := map[string][]*net.IPNet{
		"network1": {ovntest.MustParseIPNet("10.0.0.0/24")},
	}
	err := UpdateNetworkConnectSubnetAnnotation(cnc, fakeClient, allocatedSubnets)
	require.NoError(t, err)

	// Get the updated CNC
	updatedCNC, err := fakeClient.K8sV1().ClusterNetworkConnects().Get(
		context.Background(), cncName, metav1.GetOptions{})
	require.NoError(t, err)

	// Verify existing annotations are preserved
	assert.Equal(t, "existing-value", updatedCNC.Annotations["existing-annotation"])
	assert.Equal(t, "99999", updatedCNC.Annotations[OvnConnectRouterTunnelKeyAnnotation])

	// Verify the new subnet annotation was added
	assert.Contains(t, updatedCNC.Annotations, ovnNetworkConnectSubnetAnnotation)
}

func TestParseNetworkConnectTunnelKeyAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotation  *string
		expectedKey int
		wantError   bool
	}{
		{name: "no annotation", expectedKey: 0},
		{name: "empty annotation", annotation: ptr.To(""), expectedKey: 0},
		{name: "allocated key", annotation: ptr.To("16715779"), expectedKey: 16715779},
		{name: "unparseable key", annotation: ptr.To("notanumber"), wantError: true},
		{name: "integer overflow", annotation: ptr.To("18446744073709551616"), wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cnc := &networkconnectv1.ClusterNetworkConnect{
				ObjectMeta: metav1.ObjectMeta{Name: "cnc1"},
			}
			if tt.annotation != nil {
				cnc.Annotations = map[string]string{OvnConnectRouterTunnelKeyAnnotation: *tt.annotation}
			}

			key, err := ParseNetworkConnectTunnelKeyAnnotation(cnc)
			if tt.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectedKey, key)
		})
	}
}

func TestParseNetworkOwner(t *testing.T) {
	tests := []struct {
		name             string
		owner            string
		expectedTopology string
		expectedID       int
		expectError      bool
	}{
		{
			name:             "layer3 topology with ID 1",
			owner:            "layer3_1",
			expectedTopology: ovntypes.Layer3Topology,
			expectedID:       1,
			expectError:      false,
		},
		{
			name:             "layer3 topology with ID 100",
			owner:            "layer3_100",
			expectedTopology: ovntypes.Layer3Topology,
			expectedID:       100,
			expectError:      false,
		},
		{
			name:             "layer2 topology with ID 1",
			owner:            "layer2_1",
			expectedTopology: ovntypes.Layer2Topology,
			expectedID:       1,
			expectError:      false,
		},
		{
			name:             "layer2 topology with ID 50",
			owner:            "layer2_50",
			expectedTopology: ovntypes.Layer2Topology,
			expectedID:       50,
			expectError:      false,
		},
		{
			name:        "unknown topology type",
			owner:       "unknown_1",
			expectError: true,
		},
		{
			name:        "invalid format - no underscore",
			owner:       "layer31",
			expectError: true,
		},
		{
			name:        "invalid format - empty",
			owner:       "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			topology, networkID, err := ParseNetworkOwner(tt.owner)
			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedTopology, topology)
				assert.Equal(t, tt.expectedID, networkID)
			}
		})
	}
}
