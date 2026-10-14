// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	v1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	hotypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
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
			if got := NewNodeAdmissionWebhook(tt.enableHybridOverlay); !got.annotationKeys.HasAll(tt.expectedKeys...) {
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
	adm := NewNodeAdmissionWebhook(false)
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
	adm := NewNodeAdmissionWebhook(true)
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

func TestNodeAdmission_ControllerAllocationsAreNotNodeWritable(t *testing.T) {
	const clusterManagerUser = "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager"
	adm := NewNodeAdmissionWebhook(false, clusterManagerUser, userName)
	users := []struct {
		name         string
		username     string
		nodeIdentity bool
	}{
		{name: "cluster manager", username: clusterManagerUser},
		{name: "kubelet", username: "system:node:" + nodeName},
		{name: "ordinary user", username: "alice"},
		// Explicitly trusting a node identity must not grant controller ownership.
		{name: "own ovnkube node also in trusted users", username: userName, nodeIdentity: true},
		{name: "other ovnkube node", username: userName + "-other", nodeIdentity: true},
	}
	annotations := []struct {
		key          string
		value        string
		changedValue string
	}{
		{util.OvnNodeID, "1", "2"},
		{types.UDNLayer2NodeGRLRPTunnelIDAnnotation, `{"blue":"1"}`, `{"blue":"2"}`},
	}
	for _, annotation := range annotations {
		for _, user := range users {
			for _, operation := range []string{"add", "change", "remove", "unchanged"} {
				for _, subresource := range []string{"", "status"} {
					t.Run(annotation.key+"/"+user.name+"/"+operation+"/"+subresource, func(t *testing.T) {
						oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
							Name: nodeName, Annotations: map[string]string{annotation.key: annotation.value},
						}}
						newNode := oldNode.DeepCopy()
						switch operation {
						case "add":
							oldNode.Annotations = nil
						case "change":
							newNode.Annotations[annotation.key] = annotation.changedValue
						case "remove":
							delete(newNode.Annotations, annotation.key)
						}
						ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
							AdmissionRequest: v1.AdmissionRequest{
								UserInfo:    authenticationv1.UserInfo{Username: user.username},
								SubResource: subresource,
							},
						})
						oldSnapshot, newSnapshot := oldNode.DeepCopy(), newNode.DeepCopy()
						_, err := adm.ValidateUpdate(ctx, oldNode, newNode)
						// Non-node writers are authorized by the admission policy.
						wantDenied := user.nodeIdentity && operation != "unchanged"
						if user.name == "other ovnkube node" {
							wantDenied = true
						}
						if (err != nil) != wantDenied {
							t.Fatalf("admission error = %v, want denied = %t", err, wantDenied)
						}
						if !reflect.DeepEqual(oldNode, oldSnapshot) || !reflect.DeepEqual(newNode, newSnapshot) {
							t.Fatal("admission modified its input nodes")
						}
					})
				}
			}
		}
	}
}

func TestNodeAdmission_UnchangedAllocations(t *testing.T) {
	adm := NewNodeAdmissionWebhook(false)
	for _, username := range []string{"system:node:" + nodeName, "alice", userName} {
		t.Run(username, func(t *testing.T) {
			oldNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					util.OvnNodeID: "invalid",
					types.UDNLayer2NodeGRLRPTunnelIDAnnotation: "invalid",
					util.OVNNodeHostCIDRs:                      "old",
				},
			}}
			newNode := oldNode.DeepCopy()
			if username == userName {
				newNode.Annotations[util.OVNNodeHostCIDRs] = "new"
			} else {
				newNode.Annotations["example.com/unrelated"] = "value"
				newNode.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			}
			ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{Username: username}},
			})
			if _, err := adm.ValidateUpdate(ctx, oldNode, newNode); err != nil {
				t.Fatalf("unrelated update with unchanged allocations rejected: %v", err)
			}
		})
	}
}
