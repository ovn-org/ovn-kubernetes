package ovnwebhook

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/exp/maps"
	v1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNewNodeAdmissionWebhook(t *testing.T) {
	icAnnotations := make(map[string]checkNodeAnnot)
	maps.Copy(icAnnotations, commonNodeAnnotationChecks)
	maps.Copy(icAnnotations, interconnectNodeAnnotationChecks)
	hoAnnotations := make(map[string]checkNodeAnnot)
	maps.Copy(hoAnnotations, commonNodeAnnotationChecks)
	maps.Copy(hoAnnotations, hybridOverlayNodeAnnotationChecks)
	tests := []struct {
		name                string
		enableInterconnect  bool
		enableHybridOverlay bool

		expectedKeys []string
	}{
		{
			name:         "should only contain common annotation in non-IC",
			expectedKeys: maps.Keys(commonNodeAnnotationChecks),
		},
		{
			name:               "should contain common and IC annotations in IC",
			enableInterconnect: true,
			expectedKeys:       maps.Keys(icAnnotations),
		},
		{
			name:                "should contain common and hybrid overlay annotations in hybrid overlay ",
			enableHybridOverlay: true,
			expectedKeys:        maps.Keys(hoAnnotations),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NewNodeAdmissionWebhook(tt.enableInterconnect, tt.enableHybridOverlay); !got.annotationKeys.HasAll(tt.expectedKeys...) {
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
	adm := NewNodeAdmissionWebhook(false, false)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      runtime.Object
		newObj      runtime.Object
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
			name: "ovnkube-node can add util.OvnNodeZoneName with \"global\" value",
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
					Annotations: map[string]string{util.OvnNodeZoneName: "global"},
				},
			},
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
			name: "ovnkube-node can set util.OvnNodeEncapIp",
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
					Annotations: map[string]string{util.OvnNodeEncapIp: "192.168.122.156"},
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
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to global or %s, it cannot be removed", userName, util.OvnNodeZoneName, nodeName, util.OvnNodeZoneName, nodeName),
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
					Annotations: map[string]string{util.OvnNodeZoneName: "global"},
				},
			},
			newObj: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: map[string]string{util.OvnNodeZoneName: nodeName + "_rougeOne"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to global or %s, it cannot be removed", userName, util.OvnNodeZoneName, nodeName, util.OvnNodeZoneName, nodeName),
		},
		{
			name: "ovnkube-node can change util.OvnNodeZoneName to <nodeName>",
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
			if !reflect.DeepEqual(err, tt.expectedErr) {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}
func TestNodeAdmission_ValidateUpdateIC(t *testing.T) {
	adm := NewNodeAdmissionWebhook(true, false)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      runtime.Object
		newObj      runtime.Object
		expectedErr error
	}{
		{
			name: "ovnkube-node cannot set util.OvnNodeMigratedZoneName to anything else than <nodeName>",
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
					Annotations: map[string]string{util.OvnNodeMigratedZoneName: "global"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to %s, it cannot be removed", userName, util.OvnNodeMigratedZoneName, nodeName, util.OvnNodeMigratedZoneName, nodeName),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if !reflect.DeepEqual(err, tt.expectedErr) {
				t.Errorf("ValidateUpdateIC() error = %v, wantErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}

func TestNodeAdmission_ValidateUpdateHybridOverlay(t *testing.T) {
	adm := NewNodeAdmissionWebhook(false, true)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      runtime.Object
		newObj      runtime.Object
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
			if !reflect.DeepEqual(err, tt.expectedErr) {
				t.Errorf("ValidateUpdateIC() error = %v, wantErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}

func TestNodeAdmission_ValidateUpdateExtraUsers(t *testing.T) {
	extraUser := "system:serviceaccount:ovnkube-cluster-manager"
	adm := NewNodeAdmissionWebhook(true, false, extraUser)
	tests := []struct {
		name        string
		ctx         context.Context
		oldObj      runtime.Object
		newObj      runtime.Object
		expectedErr error
	}{
		{
			name: "extra user cannot set util.OvnNodeMigratedZoneName to anything else than <nodeName>",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: extraUser,
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
					Annotations: map[string]string{util.OvnNodeMigratedZoneName: "global"},
				},
			},
			expectedErr: fmt.Errorf("user: %q is not allowed to set %s on node %q: %s can only be set to %s, it cannot be removed", extraUser, util.OvnNodeMigratedZoneName, nodeName, util.OvnNodeMigratedZoneName, nodeName),
		},
		{
			name: "extra user can set util.OvnNodeMigratedZoneName to <nodeName>",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: extraUser,
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
					Annotations: map[string]string{util.OvnNodeMigratedZoneName: nodeName},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adm.ValidateUpdate(tt.ctx, tt.oldObj, tt.newObj)
			if !reflect.DeepEqual(err, tt.expectedErr) {
				t.Errorf("ValidateUpdateIC() error = %v, wantErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}
