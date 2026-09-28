// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Kube", func() {

	Describe("Taint node operations", func() {
		var kube Kube
		var existingNodeTaints []corev1.Taint
		var node *corev1.Node

		BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			kube = Kube{
				KClient: fakeClient,
			}
		})

		JustBeforeEach(func() {
			// create the node with the specified taints just before the tests
			newNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-node",
				},
				Spec: corev1.NodeSpec{
					Taints: existingNodeTaints,
				},
			}

			var err error
			node, err = kube.KClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node).NotTo(BeZero())
		})
	})

	Describe("SetAnnotationsOnPod", func() {
		var kube Kube

		BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			kube = Kube{
				KClient: fakeClient,
			}
		})

		Context("With a pod having annotations", func() {
			var (
				pod                 *corev1.Pod
				existingAnnotations map[string]string
			)

			BeforeEach(func() {
				existingAnnotations = map[string]string{"foo": "foofoo", "bar": "barbar", "baz": "bazbaz"}

				// create the pod
				newPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:   "default",
						Name:        "my-pod",
						Annotations: existingAnnotations,
					},
				}

				var err error
				pod, err = kube.KClient.CoreV1().Pods(newPod.Namespace).Create(context.TODO(), newPod, metav1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(pod).NotTo(BeZero())
			})

			Context("With adding additional annotations", func() {
				var newAnnotations map[string]interface{}

				BeforeEach(func() {
					newAnnotations = map[string]interface{}{"foobar": "foobarfoobar", "foobarbaz": "foobarbazfoobarbaz"}

					// update the annotations
					err := kube.SetAnnotationsOnPod(pod.Namespace, pod.Name, newAnnotations)
					Expect(err).ToNot(HaveOccurred())

					// load the updated pod
					pod, err = kube.KClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(pod).ToNot(BeZero())
				})

				It("Should add the new annotations", func() {
					for newAnnotationKey := range newAnnotations {
						_, found := pod.Annotations[newAnnotationKey]
						Expect(found).To(BeTrue())
					}
				})

				It("Should keep the existing annotations", func() {
					for existingAnnotationKey := range existingAnnotations {
						_, found := pod.Annotations[existingAnnotationKey]
						Expect(found).To(BeTrue())
					}
				})
			})
		})
	})

	Describe("PatchPodStatusAnnotations", func() {
		var kube Kube

		BeforeEach(func() {
			kube = Kube{
				KClient: fake.NewSimpleClientset(),
			}
		})

		It("adds annotations without dropping existing ones", func() {
			_, err := kube.KClient.CoreV1().Pods("default").Create(context.TODO(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:   "default",
					Name:        "my-pod",
					Annotations: map[string]string{"existing": "value"},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			pod, err := kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldPod := pod.DeepCopy()
			newPod := pod.DeepCopy()
			newPod.Annotations["added"] = "new-value"

			err = kube.PatchPodStatusAnnotations(context.Background(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).To(HaveKeyWithValue("existing", "value"))
			Expect(pod.Annotations).To(HaveKeyWithValue("added", "new-value"))
		})

		It("can create and remove annotations", func() {
			_, err := kube.KClient.CoreV1().Pods("default").Create(context.TODO(), &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "my-pod",
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			pod, err := kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldPod := pod.DeepCopy()
			newPod := pod.DeepCopy()
			newPod.Annotations = map[string]string{"ovn": "value"}

			err = kube.PatchPodStatusAnnotations(context.Background(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).To(HaveKeyWithValue("ovn", "value"))

			oldPod = pod.DeepCopy()
			newPod = pod.DeepCopy()
			delete(newPod.Annotations, "ovn")

			err = kube.PatchPodStatusAnnotations(context.Background(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			pod, err = kube.KClient.CoreV1().Pods("default").Get(context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(pod.Annotations).ToNot(HaveKey("ovn"))
		})

		It("adds a resourceVersion guard when creating a missing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).Fake.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Pod{}, nil
			})

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					ResourceVersion: "7",
				},
			}
			newPod := oldPod.DeepCopy()
			newPod.Annotations = map[string]string{"ovn": "value"}

			err := kube.PatchPodStatusAnnotations(context.Background(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(3))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/resourceVersion"))
			Expect(patchOps[0].Value).To(Equal("7"))
			Expect(patchOps[1].Op).To(Equal("add"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations"))
			Expect(patchOps[2].Op).To(Equal("add"))
			Expect(patchOps[2].Path).To(Equal("/metadata/annotations/ovn"))
		})

		It("guards the annotation map instead of resourceVersion when adding a key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).Fake.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Pod{}, nil
			})

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					ResourceVersion: "11",
					Annotations:     map[string]string{"existing": "value"},
				},
			}
			newPod := oldPod.DeepCopy()
			newPod.Annotations["ovn"] = "new-value"

			err := kube.PatchPodStatusAnnotations(context.TODO(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(2))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/annotations"))
			Expect(patchOps[0].Value).To(Equal(map[string]interface{}{
				"existing": "value",
			}))
			Expect(patchOps[1].Op).To(Equal("add"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations/ovn"))
		})

		It("uses a per-key guard when updating an existing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).Fake.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Pod{}, nil
			})

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					ResourceVersion: "9",
					Annotations:     map[string]string{"ovn": "old"},
				},
			}
			newPod := oldPod.DeepCopy()
			newPod.Annotations["ovn"] = "new"

			err := kube.PatchPodStatusAnnotations(context.Background(), oldPod, newPod)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(2))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[0].Value).To(Equal("old"))
			Expect(patchOps[1].Op).To(Equal("replace"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[1].Value).To(Equal("new"))
		})

		DescribeTable("does not patch a same-name replacement pod", func(annotationKey string) {
			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:       "default",
					Name:            "my-pod",
					UID:             "old-uid",
					ResourceVersion: "7",
					Annotations:     map[string]string{"ovn": "old"},
				},
			}
			replacementPod := oldPod.DeepCopy()
			replacementPod.UID = "replacement-uid"
			replacementPod.ResourceVersion = "8"
			_, err := kube.KClient.CoreV1().Pods("default").Create(
				context.TODO(), replacementPod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred(), "creating the replacement pod should succeed")

			newPod := oldPod.DeepCopy()
			newPod.Annotations[annotationKey] = "new"
			err = kube.PatchPodStatusAnnotations(context.TODO(), oldPod, newPod)
			Expect(err).To(HaveOccurred(), "the UID guard must reject the old pod's annotation patch")

			pod, err := kube.KClient.CoreV1().Pods("default").Get(
				context.TODO(), "my-pod", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred(), "reading the replacement pod should succeed")
			Expect(pod).To(Equal(replacementPod), "the rejected patch must leave the replacement unchanged")
		},
			Entry("when updating an existing annotation", "ovn"),
			Entry("when adding a new annotation", "new-annotation"),
		)
	})

	Describe("PatchNodeStatusAnnotations", func() {
		var kube Kube

		BeforeEach(func() {
			kube = Kube{
				KClient: fake.NewSimpleClientset(),
			}
		})

		It("adds annotations without dropping existing ones", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: map[string]string{"existing": "value"},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := node.DeepCopy()
			newNode := node.DeepCopy()
			newNode.Annotations["added"] = "new-value"

			err = kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err = kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Annotations).To(HaveKeyWithValue("existing", "value"))
			Expect(node.Annotations).To(HaveKeyWithValue("added", "new-value"))
		})

		It("can create and remove annotations", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-node",
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := node.DeepCopy()
			newNode := node.DeepCopy()
			newNode.Annotations = map[string]string{"ovn": "value"}

			err = kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err = kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Annotations).To(HaveKeyWithValue("ovn", "value"))

			oldNode = node.DeepCopy()
			newNode = node.DeepCopy()
			delete(newNode.Annotations, "ovn")

			err = kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err = kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Annotations).ToNot(HaveKey("ovn"))
		})

		It("adds a resourceVersion guard when creating a missing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).Fake.PrependReactor("patch", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Node{}, nil
			})

			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "my-node",
					ResourceVersion: "7",
				},
			}
			newNode := oldNode.DeepCopy()
			newNode.Annotations = map[string]string{"ovn": "value"}

			err := kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(3))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/resourceVersion"))
			Expect(patchOps[0].Value).To(Equal("7"))
			Expect(patchOps[1].Op).To(Equal("add"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations"))
			Expect(patchOps[2].Op).To(Equal("add"))
			Expect(patchOps[2].Path).To(Equal("/metadata/annotations/ovn"))
		})

		It("uses a per-key guard when updating an existing annotation key", func() {
			var patchOps []jsonPatchOp
			kube.KClient.(*fake.Clientset).Fake.PrependReactor("patch", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
				patchAction := action.(ktesting.PatchAction)
				Expect(patchAction.GetSubresource()).To(Equal("status"))
				Expect(json.Unmarshal(patchAction.GetPatch(), &patchOps)).To(Succeed())
				return true, &corev1.Node{}, nil
			})

			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "my-node",
					ResourceVersion: "9",
					Annotations:     map[string]string{"ovn": "old"},
				},
			}
			newNode := oldNode.DeepCopy()
			newNode.Annotations["ovn"] = "new"

			err := kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			Expect(patchOps).To(HaveLen(2))
			Expect(patchOps[0].Op).To(Equal("test"))
			Expect(patchOps[0].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[0].Value).To(Equal("old"))
			Expect(patchOps[1].Op).To(Equal("replace"))
			Expect(patchOps[1].Path).To(Equal("/metadata/annotations/ovn"))
			Expect(patchOps[1].Value).To(Equal("new"))
		})

		It("adds annotations when oldNode has empty non-nil annotation map", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: map[string]string{},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := node.DeepCopy()
			if oldNode.Annotations == nil {
				oldNode.Annotations = map[string]string{}
			}
			newNode := oldNode.DeepCopy()
			newNode.Annotations["k8s.ovn.org/node-subnets"] = `{"default":"10.128.0.0/23"}`

			err = kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err = kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Annotations).To(HaveKeyWithValue("k8s.ovn.org/node-subnets", `{"default":"10.128.0.0/23"}`))
		})

		It("returns an error when old and new node names differ", func() {
			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node-a",
					Annotations: map[string]string{"ovn": "value"},
				},
			}
			newNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node-b",
					Annotations: map[string]string{"ovn": "updated"},
				},
			}

			err := kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("different nodes"))
			Expect(err.Error()).To(ContainSubstring("node-a"))
			Expect(err.Error()).To(ContainSubstring("node-b"))

			actions := kube.KClient.(*fake.Clientset).Actions()
			for _, a := range actions {
				Expect(a.GetVerb()).ToNot(Equal("patch"), "no patch should be sent for mismatched node names")
			}
		})

		It("does not send trimmed node status fields via annotations patch", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "my-node",
					Annotations: map[string]string{"existing": "value"},
				},
				Status: corev1.NodeStatus{
					Images:          []corev1.ContainerImage{{Names: []string{"img:latest"}, SizeBytes: 100}},
					VolumesAttached: []corev1.AttachedVolume{{Name: "vol1", DevicePath: "/dev/sda"}},
					VolumesInUse:    []corev1.UniqueVolumeName{"vol1"},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			serverNode, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := serverNode.DeepCopy()
			oldNode.Status.Images = nil
			oldNode.Status.VolumesAttached = nil
			oldNode.Status.VolumesInUse = nil
			newNode := oldNode.DeepCopy()
			newNode.Annotations["ovn"] = "subnet-data"

			err = kube.PatchNodeStatusAnnotations(context.TODO(), oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Annotations).To(HaveKeyWithValue("ovn", "subnet-data"))
			Expect(node.Status.Images).To(HaveLen(1))
			Expect(node.Status.VolumesAttached).To(HaveLen(1))
			Expect(node.Status.VolumesInUse).To(HaveLen(1))
		})
	})

	Describe("PatchNodeStatus", func() {
		var kube Kube

		BeforeEach(func() {
			kube.KClient = fake.NewSimpleClientset()
		})

		It("updates a node condition without clobbering other status fields", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
				Status: corev1.NodeStatus{
					Images:          []corev1.ContainerImage{{Names: []string{"img:latest"}, SizeBytes: 100}},
					VolumesAttached: []corev1.AttachedVolume{{Name: "vol1", DevicePath: "/dev/sda"}},
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue, Reason: "NoRouteCreated"},
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
				Status: corev1.NodeStatus{
					Images:          nil,
					VolumesAttached: nil,
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue, Reason: "NoRouteCreated"},
					},
				},
			}
			newNode := oldNode.DeepCopy()
			newNode.Status.Conditions[0].Status = corev1.ConditionFalse
			newNode.Status.Conditions[0].Reason = "RouteCreated"

			err = kube.PatchNodeStatus(oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Status.Conditions).To(HaveLen(1))
			Expect(node.Status.Conditions[0].Status).To(Equal(corev1.ConditionFalse))
			Expect(node.Status.Conditions[0].Reason).To(Equal("RouteCreated"))
			Expect(node.Status.Images).To(HaveLen(1), "trimmed field should not be overwritten on server")
			Expect(node.Status.VolumesAttached).To(HaveLen(1), "trimmed field should not be overwritten on server")
		})

		It("does not overwrite additionally trimmed status fields", func() {
			_, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
				Spec: corev1.NodeSpec{
					Taints:   []corev1.Taint{{Key: "node-role.kubernetes.io/master", Effect: corev1.TaintEffectNoSchedule}},
					PodCIDRs: []string{"10.128.0.0/23"},
				},
				Status: corev1.NodeStatus{
					Capacity:    corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(4, resource.DecimalSI)},
					Allocatable: corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(3, resource.DecimalSI)},
					DaemonEndpoints: corev1.NodeDaemonEndpoints{
						KubeletEndpoint: corev1.DaemonEndpoint{Port: 10250},
					},
					NodeInfo: corev1.NodeSystemInfo{KernelVersion: "5.14.0"},
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
					},
					Config: &corev1.NodeConfigStatus{
						Active: &corev1.NodeConfigSource{
							ConfigMap: &corev1.ConfigMapNodeConfigSource{
								Name:      "kubelet-config",
								Namespace: "kube-system",
							},
						},
					},
					RuntimeHandlers: []corev1.NodeRuntimeHandler{
						{Name: "runc"},
					},
					Features: &corev1.NodeFeatures{},
				},
			}, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())

			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "my-node"},
				Spec:       corev1.NodeSpec{Taints: nil, PodCIDRs: nil},
				Status: corev1.NodeStatus{
					Capacity:        nil,
					Allocatable:     nil,
					DaemonEndpoints: corev1.NodeDaemonEndpoints{},
					NodeInfo:        corev1.NodeSystemInfo{KernelVersion: "5.14.0"},
					Config:          nil,
					RuntimeHandlers: nil,
					Features:        nil,
					Conditions: []corev1.NodeCondition{
						{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
					},
				},
			}
			newNode := oldNode.DeepCopy()
			newNode.Status.Conditions[0].Reason = "KubeletReady-Updated"

			err = kube.PatchNodeStatus(oldNode, newNode)
			Expect(err).ToNot(HaveOccurred())

			node, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), "my-node", metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node.Status.Conditions[0].Reason).To(Equal("KubeletReady-Updated"))
			Expect(node.Status.Capacity).NotTo(BeNil(), "Capacity must not be overwritten by trimmed patch")
			Expect(node.Status.Allocatable).NotTo(BeNil(), "Allocatable must not be overwritten by trimmed patch")
			Expect(node.Status.DaemonEndpoints.KubeletEndpoint.Port).To(Equal(int32(10250)), "DaemonEndpoints must not be overwritten")
			Expect(node.Status.NodeInfo.KernelVersion).To(Equal("5.14.0"), "NodeInfo must not be overwritten")
			Expect(node.Spec.Taints).To(HaveLen(1), "Taints must not be overwritten by trimmed patch")
			Expect(node.Spec.PodCIDRs).To(HaveLen(1), "PodCIDRs must not be overwritten by trimmed patch")
			Expect(node.Status.Config).NotTo(BeNil(), "Status.Config must not be overwritten by trimmed patch")
			Expect(node.Status.RuntimeHandlers).To(HaveLen(1), "RuntimeHandlers must not be overwritten by trimmed patch")
			Expect(node.Status.Features).NotTo(BeNil(), "Features must not be overwritten by trimmed patch")
		})
	})
})
