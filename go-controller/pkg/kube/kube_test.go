package kube

import (
	"context"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var _ = Describe("Kube", func() {
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
				var newAnnotations map[string]string

				BeforeEach(func() {
					newAnnotations = map[string]string{"foobar": "foobarfoobar", "foobarbaz": "foobarbazfoobarbaz"}

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
