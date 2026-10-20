// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	v1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestNewNodeAdmissionWebhook(t *testing.T) {
	defaultAnnotations := make(map[string]checkNodeAnnot)
	maps.Copy(defaultAnnotations, commonNodeAnnotationChecks)
	maps.Copy(defaultAnnotations, interconnectNodeAnnotationChecks)
	hoAnnotations := make(map[string]checkNodeAnnot)
	maps.Copy(hoAnnotations, defaultAnnotations)
	maps.Copy(hoAnnotations, hybridOverlayNodeAnnotationChecks)
	tests := []struct {
		name                string
		enableHybridOverlay bool

		expectedKeys []string
	}{
		{
			name:         "should contain common and interconnect annotations",
			expectedKeys: slices.Collect(maps.Keys(defaultAnnotations)),
		},
		{
			name:                "should contain common, interconnect, and hybrid overlay annotations in hybrid overlay",
			enableHybridOverlay: true,
			expectedKeys:        slices.Collect(maps.Keys(hoAnnotations)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewNodeAdmissionWebhook(listers.NewNodeLister(cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})), tt.enableHybridOverlay); !got.annotationKeys.HasAll(tt.expectedKeys...) {
				t.Errorf("NewNodeAdmissionWebhook() = %v, want %v", got.annotationKeys, tt.expectedKeys)
			}
		})
	}
}

var nodeName = "fakeNode"
var userName = fmt.Sprintf("%s:%s", csrapprover.NamePrefix, nodeName)
var additionalNamePrefix = "system:foobar"
var additionalUserName = fmt.Sprintf("%s:%s", additionalNamePrefix, nodeName)

func TestNodeAdmission_ValidateUpdate(t *testing.T) {
	adm := NewNodeAdmissionWebhook(listers.NewNodeLister(cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})), false)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      *corev1.Node
		newObj      *corev1.Node
		expectedErr error
	}{
		{
			name: "allow if user is not ovnkube-node and not changing ovnkube-node annotations",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:nodes:node",
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   nodeName,
					Labels: map[string]string{"key": "old"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   nodeName,
					Labels: map[string]string{"key": "new"},
				},
			},
			expectedErr: nil,
		},
		{
			name: "error out if different user tries to set ovnkube-node annotations",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:nodes:node",
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "old"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "new"},
				},
			},
			expectedErr: fmt.Errorf("user %q is not allowed to set the following annotations on node: %q: %v", "system:nodes:node", nodeName, []string{util.OVNNodeHostCIDRs}),
		},
		{
			name: "error out if the request is not in context",
			ctx:  context.TODO(),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{"new": "value"},
				},
			},
			expectedErr: errors.New("admission.Request not found in context"),
		},
		{
			name: "ovnkube-node cannot modify annotations on different nodes",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName + "_rougeOne",
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "old"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "new"},
				},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to modify nodes %q annotations", nodeName+"_rougeOne", nodeName),
		},
		{
			name: "ovnkube-node cannot modify annotations that do not belong to it",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs + "bad": "old"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs + "bad": "new"},
				},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to set the following annotations: %v", nodeName, []string{util.OVNNodeHostCIDRs + "bad"}),
		},
		{
			name: "ovnkube-node can add util.OvnNodeChassisID",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeChassisID: "chassisID"}},
			},
		},
		{
			name: "ovnkube-node cannot remove util.OvnNodeChassisID",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeChassisID: "chassisID"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s cannot be removed", userName, util.OvnNodeChassisID, nodeName, util.OvnNodeChassisID),
		},
		{
			name: "ovnkube-node cannot change util.OvnNodeChassisID once set",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeChassisID: "chassisID"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeChassisID: "chassisIDInvalid"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s cannot be changed once set", userName, util.OvnNodeChassisID, nodeName, util.OvnNodeChassisID),
		},
		{
			name: "ovnkube-node cannot add util.OvnNodeZoneName with a non-node-name value",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: "old-zone"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to %s, it cannot be removed", userName, util.OvnNodeZoneName, nodeName, util.OvnNodeZoneName, nodeName),
		},
		{
			name: "ovnkube-node can set util.OvnNodeManagementPort",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeManagementPort: `{"PfId":1,"FuncId":1}`},
				},
			},
		},
		{
			name: "ovnkube-node can set util.OvnNodeGatewayMtuSupport",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeGatewayMtuSupport: "false"},
				},
			},
		},
		{
			name: "ovnkube-node can add util.OvnNodeZoneName with <nodeName> value",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: nodeName},
				},
			},
		},
		{
			name: "ovnkube-node cannot add util.OvnNodeZoneName with invalid value",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: "LocalInvalidZone"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to %s, it cannot be removed", userName, util.OvnNodeZoneName, nodeName, util.OvnNodeZoneName, nodeName),
		},
		{
			name: "ovnkube-node cannot change util.OvnNodeZoneName to anything else than <nodeName>",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: nodeName},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: nodeName + "_rougeOne"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to %s, it cannot be removed", userName, util.OvnNodeZoneName, nodeName, util.OvnNodeZoneName, nodeName),
		},
		{
			name: "ovnkube-node can migrate util.OvnNodeZoneName from legacy global to <nodeName>",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: "global"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: nodeName},
				},
			},
		},
		{
			name: "ovnkube-node can update another annotation while legacy global zone is unchanged",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						util.OvnNodeZoneName:  "global",
						util.OVNNodeHostCIDRs: "old",
					},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
					Annotations: map[string]string{
						util.OvnNodeZoneName:  "global",
						util.OVNNodeHostCIDRs: "new",
					},
				},
			},
		},
		{
			name: "ovnkube-node cannot modify anything other than annotations",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "old"},
					Labels:      map[string]string{"key": "old"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OVNNodeHostCIDRs: "new"},
					Labels:      map[string]string{"key": "new"},
				},
			},
			expectedErr: fmt.Errorf("ovnkube-node on node: %q is not allowed to modify anything other than annotations", nodeName),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if err != tt.expectedErr && err.Error() != tt.expectedErr.Error() {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}
func TestNodeAdmission_ValidateUpdateHybridOverlay(t *testing.T) {
	adm := NewNodeAdmissionWebhook(listers.NewNodeLister(cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})), true)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      *corev1.Node
		newObj      *corev1.Node
		expectedErr error
	}{
		{
			name: "ovnkube-node can set HybridOverlayDRMAC in hybrid overlay environments",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{hotypes.HybridOverlayDRMAC: "0a:58:0a:80:00:05"},
				},
			},
		},
		{
			name: "ovnkube-node can set HybridOverlayDRIP in hybrid overlay environments",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: userName,
				}},
			}),
			oldObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeName,
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{hotypes.HybridOverlayDRIP: "192.168.0.3"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if err != tt.expectedErr && err.Error() != tt.expectedErr.Error() {
				t.Errorf("ValidateUpdateIC() error = %v, wantErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}

func TestNodeAdmissionChassisIDUniqueness(t *testing.T) {
	for _, tt := range []struct {
		name      string
		chassisID string
		otherNode string
		wantError bool
	}{
		{name: "unique", chassisID: "new-chassis", otherNode: "other"},
		{name: "duplicate", chassisID: "claimed-chassis", otherNode: "other", wantError: true},
		{name: "same node in cache", chassisID: "claimed-chassis", otherNode: nodeName},
		{name: "empty", otherNode: "other", wantError: true},
		{name: "whitespace", chassisID: " ", otherNode: "other", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			err := indexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tt.otherNode, Annotations: map[string]string{util.OvnNodeChassisID: "claimed-chassis"}}})
			if err != nil {
				t.Fatal(err)
			}
			adm := NewNodeAdmissionWebhook(listers.NewNodeLister(indexer), false)
			oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
			newNode := oldNode.DeepCopy()
			newNode.Annotations = map[string]string{util.OvnNodeChassisID: tt.chassisID}
			ctx := admission.NewContextWithRequest(context.Background(), admission.Request{AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: userName}}})
			_, err = adm.ValidateUpdate(ctx, oldNode, newNode)
			if (err != nil) != tt.wantError {
				t.Fatalf("ValidateUpdate error = %v, wantError %t", err, tt.wantError)
			}
		})
	}
}
