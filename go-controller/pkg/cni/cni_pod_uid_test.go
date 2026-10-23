// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	kubemocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/kube/mocks"
	v1mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestCmdDelDPUPodUIDMismatch(t *testing.T) {
	podResource := schema.GroupResource{Resource: "pods"}
	invalidErr := apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "pod",
		field.ErrorList{field.Invalid(field.NewPath("metadata"), nil, "test failed")})
	forbiddenErr := apierrors.NewForbidden(podResource, "pod", errors.New("forbidden"))

	for _, secondary := range []bool{false, true} {
		for _, unprivileged := range []bool{false, true} {
			for _, tc := range []struct {
				name            string
				listerReplaced  bool
				replacedOnRetry bool
				listerDeleted   bool
				patchReplaced   bool
				apiDeleted      bool
				patchErr        error
				wantErr         bool
			}{
				{name: "successful cleanup"},
				{name: "replacement before update", listerReplaced: true},
				{name: "replacement during patch with stale informer", patchReplaced: true, wantErr: true},
				{name: "informer observes replacement on retry", patchReplaced: true, replacedOnRetry: true},
				{name: "deleted before update", listerDeleted: true, apiDeleted: true},
				{name: "deleted before patch", apiDeleted: true},
				{name: "deleted after failed patch with stale informer", apiDeleted: true, patchErr: invalidErr, wantErr: true},
				{name: "same UID invalid", patchErr: invalidErr, wantErr: true},
				{name: "same UID conflict", patchErr: apierrors.NewConflict(podResource, "pod", errors.New("conflict")), wantErr: true},
				{name: "unrelated error", patchErr: forbiddenErr, wantErr: true},
			} {
				t.Run(fmt.Sprintf("secondary=%t/unprivileged=%t/%s", secondary, unprivileged, tc.name), func(t *testing.T) {
					require.NoError(t, config.PrepareTestConfig())
					t.Cleanup(func() { require.NoError(t, config.PrepareTestConfig()) })
					config.OvnKubeNode.Mode = ovntypes.NodeModeDPUHost
					config.UnprivilegedMode = unprivileged
					originalOps := podRequestInterfaceOps
					stub := &podRequestInterfaceOpsStub{}
					podRequestInterfaceOps = stub
					t.Cleanup(func() { podRequestInterfaceOps = originalOps })

					pr := &PodRequest{
						Command: CNIDel, PodNamespace: "namespace", PodName: "pod", PodUID: "old-uid",
						SandboxID: "old-sandbox", Netns: "old-netns", IfName: "eth0",
						netName: ovntypes.DefaultNetworkName, nadName: ovntypes.DefaultNetworkName,
						CNIConf: &ovncnitypes.NetConf{DeviceID: "0000:05:00.4"},
					}
					if secondary {
						pr.netName, pr.nadName, pr.IfName = "secondary", "namespace/secondary", "net1"
					}
					oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Namespace: pr.PodNamespace, Name: pr.PodName, UID: "old-uid", ResourceVersion: "1",
						Annotations: map[string]string{
							"k8s.v1.cni.cncf.io/networks": `[{"name":"secondary","namespace":"namespace"}]`,
						},
					}}
					var err error
					oldPod.Annotations, err = util.MarshalPodDPUConnDetails(oldPod.Annotations,
						&util.DPUConnectionDetails{SandboxId: pr.SandboxID, VfNetdevName: "old-vf"}, pr.nadName)
					require.NoError(t, err)
					snapshot := oldPod.DeepCopy()
					replacement := oldPod.DeepCopy()
					replacement.UID = "new-uid"
					replacement.ResourceVersion = "2"
					replacement.Annotations, err = util.MarshalPodDPUConnDetails(replacement.Annotations,
						&util.DPUConnectionDetails{SandboxId: "new-sandbox", VfNetdevName: "new-vf"}, pr.nadName)
					require.NoError(t, err)

					client := fake.NewSimpleClientset()
					if !tc.apiDeleted {
						apiPod := oldPod
						if tc.listerReplaced {
							apiPod = replacement
						}
						require.NoError(t, client.Tracker().Add(apiPod))
					}
					podLister := &v1mocks.PodLister{}
					namespaceLister := &v1mocks.PodNamespaceLister{}
					podLister.On("Pods", pr.PodNamespace).Return(namespaceLister)
					// cmdDel first captures the old sandbox's connection details.
					namespaceLister.On("Get", pr.PodName).Return(oldPod, nil).Once()
					switch {
					case tc.listerReplaced:
						namespaceLister.On("Get", pr.PodName).Return(replacement, nil)
					case tc.listerDeleted:
						namespaceLister.On("Get", pr.PodName).Return(nil, apierrors.NewNotFound(podResource, pr.PodName))
					case tc.replacedOnRetry:
						namespaceLister.On("Get", pr.PodName).Return(oldPod, nil).Once()
						namespaceLister.On("Get", pr.PodName).Return(replacement, nil).Once()
					default:
						namespaceLister.On("Get", pr.PodName).Return(oldPod, nil)
					}
					patchCalls := 0
					client.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
						patchCalls++
						if tc.patchErr != nil {
							return true, nil, tc.patchErr
						}
						if tc.patchReplaced {
							require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, pr.PodNamespace))
						}
						handled, obj, patchErr := ktesting.ObjectReaction(client.Tracker())(action)
						// Match the apiserver's Invalid classification of failed JSON
						// Patch tests; the fake tracker otherwise returns raw errors.
						if patchErr != nil && !apierrors.IsNotFound(patchErr) {
							patchErr = apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, pr.PodName,
								field.ErrorList{field.Invalid(field.NewPath("metadata"), nil, patchErr.Error())})
						}
						return handled, obj, patchErr
					})

					response, err := pr.cmdDel(&ClientSet{kclient: client, podLister: podLister}, nil)
					if tc.wantErr {
						require.Error(t, err)
						if tc.patchReplaced {
							require.ErrorContains(t, err, "is invalid", "a stale informer cannot classify replacement")
						}
						require.Nil(t, response)
						require.Empty(t, stub.unconfiguredInterfaces)
					} else {
						require.NoError(t, err)
						require.NotNil(t, response)
						wantInfo := &PodInterfaceInfo{IsDPUHostMode: true, NetdevName: "old-vf"}
						if unprivileged {
							require.Nil(t, response.Result)
							require.Equal(t, wantInfo, response.PodIFInfo)
							require.Empty(t, stub.unconfiguredInterfaces)
						} else {
							require.NotNil(t, response.Result)
							require.Equal(t, []*PodInterfaceInfo{wantInfo}, stub.unconfiguredInterfaces)
						}
					}
					if tc.listerReplaced || tc.listerDeleted {
						require.Zero(t, patchCalls)
					} else {
						require.Positive(t, patchCalls)
						if tc.patchReplaced && !tc.wantErr {
							require.Equal(t, 1, patchCalls, "confirmed replacement must not retry")
						}
						if tc.wantErr && (tc.patchReplaced || util.IsPodAnnotationUpdateRetryable(tc.patchErr)) {
							require.Equal(t, 1, patchCalls, "a stale informer must not cause the same patch to be repeated")
						}
					}
					if tc.listerReplaced || tc.patchReplaced {
						got, getErr := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), pr.PodNamespace, pr.PodName)
						require.NoError(t, getErr)
						require.Equal(t, replacement, got, "replacement annotations must be untouched")
					}
					require.Equal(t, snapshot, oldPod, "informer snapshot must not be mutated")
					namespaceLister.AssertExpectations(t)
				})
			}
		}
	}
}

func TestUpdatePodDPUConnDetailsRejectsReplacement(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleting=%t", deleting), func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "namespace", Name: "pod", UID: "old-uid"}}
			replacement := pod.DeepCopy()
			replacement.UID = "new-uid"
			podLister := &v1mocks.PodLister{}
			namespaceLister := &v1mocks.PodNamespaceLister{}
			podLister.On("Pods", pod.Namespace).Return(namespaceLister)
			namespaceLister.On("Get", pod.Name).Return(replacement, nil)
			kube := &kubemocks.Interface{}
			pr := &PodRequest{PodNamespace: pod.Namespace, PodName: pod.Name, nadKey: ovntypes.DefaultNetworkName}
			details := &util.DPUConnectionDetails{SandboxId: "old-sandbox", VfNetdevName: "old-vf"}
			if deleting {
				details = nil
			}
			err := pr.updatePodDPUConnDetailsWithRetry(kube, podLister, pod, details)
			require.True(t, apierrors.IsNotFound(err), "expected not found for the original pod, got: %v", err)
			require.ErrorContains(t, err, `expected UID "old-uid", found "new-uid"`)
			require.Empty(t, kube.Calls, "no annotation write may target the replacement")
		})
	}
}
