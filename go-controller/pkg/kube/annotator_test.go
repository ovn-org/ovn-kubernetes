package kube

import (
	"context"
	"fmt"

	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kubeMocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube/mocks"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Annotator", func() {

	Describe("Node annotator", func() {
		Context("with annotation changes", func() {
			const (
				deleteKey string = "deleteKey"
				nodeName  string = "TestNode"
			)
			initialAnnotations := map[string]string{"initialKey": "initialVal", deleteKey: "val"}
			expectedAnnotations := map[string]interface{}{"key1": "val1", "key2": "val2", deleteKey: nil}

			fakeClient := fake.NewSimpleClientset()
			kube := &Kube{
				KClient: fakeClient,
			}
			newNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        nodeName,
					Annotations: initialAnnotations,
				},
			}
			node, err := kube.KClient.CoreV1().Nodes().Create(context.TODO(), newNode, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(node).NotTo(BeZero())

			nodeAnnot := NewNodeAnnotator(kube, nodeName)

			It("should not run if there are no changes", func() {
				fakeKubeInterface := kubeMocks.Interface{}
				fakeKubeInterface.On("SetAnnotationsOnNode", mock.Anything, mock.Anything, mock.Anything).Return(fmt.Errorf("should not be called"))
				nodeAnnot := NewNodeAnnotator(&fakeKubeInterface, "")
				err := nodeAnnot.Run()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should successfully set/delete all provided annotations", func() {
				for key, val := range expectedAnnotations {
					if val != nil {
						err := nodeAnnot.Set(key, val)
						Expect(err).ToNot(HaveOccurred())
					} else {
						nodeAnnot.Delete(key)
					}
				}
			})

			It("should cache all provided annotations", func() {
				for key := range expectedAnnotations {
					_, ok := nodeAnnot.(*nodeAnnotator).changes[key]
					Expect(ok).To(BeTrue())
				}
			})

			It("should set the deleted key value to nil", func() {
				val, ok := nodeAnnot.(*nodeAnnotator).changes[deleteKey]
				Expect(ok).To(BeTrue())
				Expect(val).To(BeNil())
			})

			It("should update nodes annotations", func() {
				err := nodeAnnot.Run()
				Expect(err).ToNot(HaveOccurred())

				node, err := kube.GetNode(nodeName)
				Expect(err).ToNot(HaveOccurred())

				// should contain initial annotations
				// deleteKey annotation should not exist
				for key, val := range initialAnnotations {
					if key != deleteKey {
						Expect(node.Annotations[key]).To(Equal(val))
					} else {
						_, ok := node.Annotations[key]
						Expect(ok).To(BeFalse())
					}
				}

				// should contain new annotations
				for key, val := range expectedAnnotations {
					if key != deleteKey {
						Expect(node.Annotations[key]).To(Equal(val))
					} else {
						_, ok := node.Annotations[key]
						Expect(ok).To(BeFalse())
					}
				}
			})
		})
	})
})
