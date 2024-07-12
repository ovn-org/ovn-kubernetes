package kube

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var _ = Describe("Kube", func() {

	Describe("Taint node operations", func() {
		var kube Kube
		var existingNodeTaints []v1.Taint
		var taint v1.Taint
		var node *v1.Node

		BeforeEach(func() {
			fakeClient := fake.NewSimpleClientset()
			kube = Kube{
				KClient: fakeClient,
			}
			taint = v1.Taint{Key: "my-taint-key", Value: "my-taint-value", Effect: v1.TaintEffectNoSchedule}
		})

		JustBeforeEach(func() {
			// create the node with the specified taints just before the tests
			newNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "my-node",
				},
				Spec: v1.NodeSpec{
					Taints: existingNodeTaints,
				},
			}

			var err error
			node, err = kube.KClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node).NotTo(BeZero())
		})

		Context("with a node not having the taint", func() {

			BeforeEach(func() {
				existingNodeTaints = make([]v1.Taint, 0)
			})

			Context("SetTaintOnNode", func() {
				It("should add the taint to the node", func() {
					err := kube.SetTaintOnNode(node.Name, &taint)
					Expect(err).ToNot(HaveOccurred())

					loadedNode, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(loadedNode.Spec.Taints).To(ContainElement(taint))
				})
			})

			Context("RemoveTaintFromNode", func() {
				It("should remove nothing from the node", func() {
					err := kube.RemoveTaintFromNode(node.Name, &taint)
					Expect(err).ToNot(HaveOccurred())

					loadedNode, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(loadedNode.Spec.Taints).To(HaveLen(len(node.Spec.Taints)))
				})
			})
		})

		Context("with a node having the same taint already", func() {

			BeforeEach(func() {
				existingNodeTaints = []v1.Taint{taint}
			})

			Context("SetTaintOnNode", func() {
				It("should update the taint of the node if effect differs", func() {
					updatedTaint := taint.DeepCopy()
					updatedTaint.Effect = v1.TaintEffectPreferNoSchedule

					err := kube.SetTaintOnNode(node.Name, updatedTaint)
					Expect(err).ToNot(HaveOccurred())

					loadedNode, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(loadedNode.Spec.Taints).To(ContainElement(*updatedTaint))
				})

				It("should update nothing if taint is the same", func() {
					err := kube.SetTaintOnNode(node.Name, &taint)
					Expect(err).ToNot(HaveOccurred())

					updatedNode, err := kube.GetNode(node.Name)
					Expect(err).ToNot(HaveOccurred())
					Expect(updatedNode.Spec.Taints).To(Equal([]v1.Taint{taint}))
				})
			})

			Context("RemoveTaintFromNode", func() {
				It("should remove the taint from the node", func() {
					err := kube.RemoveTaintFromNode(node.Name, &taint)
					Expect(err).ToNot(HaveOccurred())

					loadedNode, err := kube.KClient.CoreV1().Nodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())
					Expect(loadedNode.Spec.Taints).NotTo(ContainElement(taint))
					Expect(loadedNode.Spec.Taints).To(HaveLen(len(node.Spec.Taints) - 1))
				})
			})
		})

		Context("with references to a node which doesn't exist", func() {

			Context("SetTaintOnNode", func() {

				It("should return an error, if node doesn't exist", func() {
					nodeName := "targaryen"
					err := kube.SetTaintOnNode(nodeName, &taint)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(Equal("nodes \"targaryen\" not found"))
				})
			})
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
				pod                 *v1.Pod
				existingAnnotations map[string]string
			)

			BeforeEach(func() {
				existingAnnotations = map[string]string{"foo": "foofoo", "bar": "barbar", "baz": "bazbaz"}

				// create the pod
				newPod := &v1.Pod{
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
})
