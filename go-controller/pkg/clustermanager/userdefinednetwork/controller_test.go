package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/testing"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1fakeclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("User Defined Network Controller", func() {
	var (
		cs *util.OVNClusterManagerClientset
		f  *factory.WatchFactory
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	})

	AfterEach(func() {
		if f != nil {
			f.Shutdown()
		}
	})

	newTestController := func(renderNADStub RenderNetAttachDefManifest, objects ...runtime.Object) *Controller {
		cs = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		f, err = factory.NewClusterManagerWatchFactory(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Start()).To(Succeed())

		return New(cs.NetworkAttchDefClient, f.NADInformer(),
			cs.UserDefinedNetworkClient, f.UserDefinedNetworkInformer(), f.ClusterUserDefinedNetworkInformer(),
			renderNADStub, f.PodCoreInformer(), f.NamespaceInformer(),
		)
	}

	Context("manager", func() {
		Context("reconcile UDN CR", func() {
			It("should create NAD successfully", func() {
				udn := testUDN()
				expectedNAD := testNAD()
				c := newTestController(renderNadStub(expectedNAD), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionReady",
					Message: "NetworkAttachmentDefinition has been created",
				}}))

				nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				Expect(nad).To(Equal(expectedNAD))
			})

			It("should fail when NAD render fail", func() {
				udn := testUDN()
				renderErr := errors.New("render NAD fails")
				c := newTestController(failRenderNadStub(renderErr), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: "failed to generate NetworkAttachmentDefinition: " + renderErr.Error(),
				}}))

				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(kerrors.IsNotFound(err)).To(BeTrue())
			})
			It("should fail when NAD create fail", func() {
				udn := testUDN()
				c := newTestController(noopRenderNadStub(), udn)

				expectedError := errors.New("create NAD error")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("create", "network-attachment-definitions", func(action testing.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, expectedError
				})

				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: "failed to create NetworkAttachmentDefinition: create NAD error",
				}}))

				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(kerrors.IsNotFound(err)).To(BeTrue())
			})

			It("should fail when foreign NAD exist", func() {
				udn := testUDN()
				foreignNad := testNAD()
				foreignNad.ObjectMeta.OwnerReferences = nil
				c := newTestController(noopRenderNadStub(), udn, foreignNad)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: "foreign NetworkAttachmentDefinition with the desired name already exist [test/test]",
				}}))
			})
			It("should reconcile mutated NAD", func() {
				udn := testUDN()
				expectedNAD := testNAD()
				c := newTestController(renderNadStub(expectedNAD), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionReady",
					Message: "NetworkAttachmentDefinition has been created",
				}}))

				mutatedNAD := expectedNAD.DeepCopy()
				mutatedNAD.Spec.Config = "MUTATED"
				mutatedNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Update(context.Background(), mutatedNAD, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() *netv1.NetworkAttachmentDefinition {
					updatedNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return updatedNAD
				}).Should(Equal(expectedNAD))
			})
			It("should fail when update mutated NAD fails", func() {
				udn := testUDN()
				expectedNAD := testNAD()
				c := newTestController(renderNadStub(expectedNAD), udn)

				expectedErr := errors.New("update error")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("update", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
					obj := action.(testing.UpdateAction).GetObject()
					nad := obj.(*netv1.NetworkAttachmentDefinition)
					if nad.Spec.Config == expectedNAD.Spec.Config {
						return true, nil, expectedErr
					}
					return false, nad, nil
				})

				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionReady",
					Message: "NetworkAttachmentDefinition has been created",
				}}))
				actualNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(actualNAD).To(Equal(expectedNAD))

				mutatedNAD := expectedNAD.DeepCopy()
				mutatedNAD.Spec.Config = "MUTATED"
				mutatedNAD, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Update(context.Background(), mutatedNAD, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() []metav1.Condition {
					udn, err = cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: "failed to update NetworkAttachmentDefinition: " + expectedErr.Error(),
				}}))

				Eventually(func() *netv1.NetworkAttachmentDefinition {
					updatedNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return updatedNAD
				}).Should(Equal(mutatedNAD))
			})

			It("given primary UDN, should fail when primary NAD already exist", func() {
				primaryUDN := testUDN()
				primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer2
				primaryUDN.Spec.Layer2 = &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary}

				primaryNAD := primaryNetNAD()
				c := newTestController(noopRenderNadStub(), primaryUDN, primaryNAD)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					updatedUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(updatedUDN.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: `primary network already exist in namespace "test": "primary-net-1"`,
				}}))
			})
			It("given primary UDN, should fail when unmarshal primary NAD fails", func() {
				primaryUDN := testUDN()
				primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer3
				primaryUDN.Spec.Layer3 = &udnv1.Layer3Config{Role: udnv1.NetworkRolePrimary}

				primaryNAD := primaryNetNAD()
				primaryNAD.Name = "another-primary-net"
				primaryNAD.Spec.Config = "!@#$"
				c := newTestController(noopRenderNadStub(), primaryUDN, primaryNAD)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					updatedUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(updatedUDN.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: `failed to validate no primary network exist: unmarshal failed [test/another-primary-net]: invalid character '!' looking for beginning of value`,
				}}))
			})

			It("should add finalizer to UDN", func() {
				udn := testUDN()
				udn.Finalizers = nil
				c := newTestController(noopRenderNadStub(), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []string {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return udn.Finalizers
				}).Should(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))
			})
			It("should fail when add finalizer to UDN fails", func() {
				udn := testUDN()
				udn.Finalizers = nil
				c := newTestController(noopRenderNadStub(), udn)

				expectedErr := errors.New("update UDN error")
				cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("update", "userdefinednetworks", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
					return true, nil, expectedErr
				})

				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					updatedUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(updatedUDN.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkReady",
					Status:  "False",
					Reason:  "SyncError",
					Message: `failed to add finalizer to UserDefinedNetwork: ` + expectedErr.Error(),
				}}))
			})

			It("when UDN is being deleted, NAD exist, 2 pods using UDN, should delete NAD once no pod uses the network", func() {
				var err error
				nad := testNAD()
				udn := testUDN()
				c := newTestController(renderNadStub(nad), udn)
				Expect(c.Run()).To(Succeed())

				testOVNPodAnnot := map[string]string{util.OvnPodAnnotationName: `{"default": {"role":"primary"}, "test/test": {"role": "secondary"}}`}
				pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}
				pod1, err = cs.KubeClient.CoreV1().Pods(udn.Namespace).Create(context.Background(), pod1, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}
				pod2, err = cs.KubeClient.CoreV1().Pods(udn.Namespace).Create(context.Background(), pod2, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("mark UDN for deletion")
				udn.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
				udn, err = cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Update(context.Background(), udn, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(udn.DeletionTimestamp.IsZero()).To(BeFalse())

				// user short interval to make the controller re-enqueue requests
				c.networkInUseRequeueInterval = 50 * time.Millisecond

				assertFinalizersPresent(cs.UserDefinedNetworkClient, cs.NetworkAttchDefClient, udn, pod1, pod2)

				Expect(cs.KubeClient.CoreV1().Pods(udn.Namespace).Delete(context.Background(), pod1.Name, metav1.DeleteOptions{})).To(Succeed())

				assertFinalizersPresent(cs.UserDefinedNetworkClient, cs.NetworkAttchDefClient, udn, pod2)

				Expect(cs.KubeClient.CoreV1().Pods(udn.Namespace).Delete(context.Background(), pod2.Name, metav1.DeleteOptions{})).To(Succeed())

				Eventually(func() []string {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return udn.Finalizers
				}).Should(BeEmpty(), "should remove finalizer on UDN following deletion and not being used")
				nad, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(kerrors.IsNotFound(err)).To(BeTrue())
			})
		})
	})

	Context("UserDefinedNetwork object sync", func() {
		It("should fail when NAD owner-reference is malformed", func() {
			udn := testUDN()
			mutatedNAD := testNAD()
			mutatedNAD.ObjectMeta.OwnerReferences = []metav1.OwnerReference{{Kind: "DifferentKind"}}
			c := newTestController(noopRenderNadStub(), udn, mutatedNAD)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).To(Equal(errors.New("foreign NetworkAttachmentDefinition with the desired name already exist [test/test]")))
		})

		It("when UDN is being deleted, should not remove finalizer from non managed NAD", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			unmanagedNAD := testNAD()
			unmanagedNAD.OwnerReferences[0].UID = "99"
			c := newTestController(noopRenderNadStub(), udn, unmanagedNAD)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())

			unmanagedNAD, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), unmanagedNAD.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			expectedFinalizers := testNAD().Finalizers

			Expect(unmanagedNAD.Finalizers).To(Equal(expectedFinalizers))
		})
		It("when UDN is being deleted, and NAD exist, should delete NAD", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			c := newTestController(noopRenderNadStub(), udn, nad)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())

			nad, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			Expect(err).To(HaveOccurred())
			Expect(kerrors.IsNotFound(err)).To(BeTrue())
		})
		It("when UDN is being deleted, and NAD exist, should fail when remove NAD finalizer fails", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			c := newTestController(noopRenderNadStub(), udn, nad)

			expectedErr := errors.New("update NAD error")
			cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("update", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when UDN is being deleted, and NAD exist w/o finalizer, should remove finalizer from UDN", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			nad.Finalizers = nil
			c := newTestController(noopRenderNadStub(), udn, nad)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, and NAD not exist, should remove finalizer from UDN", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			c := newTestController(noopRenderNadStub(), udn)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, should fail removing finalizer from UDN when patch fails", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			nad.Finalizers = nil
			c := newTestController(noopRenderNadStub(), udn, nad)

			expectedErr := errors.New("update UDN error")
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("update", "userdefinednetworks", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when UDN is being deleted, NAD exist, pod exist, should delete NAD when network not being used", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod1", Namespace: udn.Namespace,
					Annotations: map[string]string{util.OvnPodAnnotationName: `{ 
                          "default": {"role":"primary", "mac_address":"0a:58:0a:f4:02:03"},
						  "test/another-network": {"role": "secondary","mac_address":"0a:58:0a:f4:02:01"} 
                         }`,
					},
				},
			}
			c := newTestController(renderNadStub(nad), udn, nad, pod)

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())

			Expect(udn.Finalizers).To(BeEmpty())

			nad, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			Expect(err).To(HaveOccurred())
			Expect(kerrors.IsNotFound(err)).To(BeTrue())
		})

		DescribeTable("when UDN is being deleted, NAD exist, should not remove finalizers when",
			func(podOvnAnnotations map[string]string, expectedErr error) {
				var objs []runtime.Object
				udn := testsUDNWithDeletionTimestamp(time.Now())
				nad := testNAD()
				for podName, ovnAnnotValue := range podOvnAnnotations {
					objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: podName, Namespace: udn.Namespace,
						Annotations: map[string]string{util.OvnPodAnnotationName: ovnAnnotValue},
					}})
				}
				objs = append(objs, udn, nad)
				c := newTestController(renderNadStub(nad), objs...)

				_, err := c.syncUserDefinedNetwork(udn)
				Expect(err).To(MatchError(ContainSubstring(expectedErr.Error())))

				actual, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(actual.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
					"finalizer should remain until no pod uses the network")

				actualUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(actualUDN.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
					"finalizer should remain until no pod uses the network")
				Expect(err).NotTo(HaveOccurred())
			},
			Entry("pod connected to user-defined-network primary network",
				map[string]string{
					"test-pod": `{"default":{"role":"infrastructure-locked", "mac_address":"0a:58:0a:f4:02:03"},` +
						`"test/test":{"role": "primary","mac_address":"0a:58:0a:f4:02:01"}}`,
				},
				errors.New("network in use by the following pods: [test/test-pod]"),
			),
			Entry("pod connected to default primary network, and user-defined-network secondary network",
				map[string]string{
					"test-pod": `{"default":{"role":"primary", "mac_address":"0a:58:0a:f4:02:03"},` +
						`"test/test":{"role": "secondary","mac_address":"0a:58:0a:f4:02:01"}}`,
				},
				errors.New("network in use by the following pods: [test/test-pod]"),
			),
			Entry("1 pod connected to network, 1 pod has invalid annotation",
				map[string]string{
					"test-pod": `{"default":{"role":"primary", "mac_address":"0a:58:0a:f4:02:03"},` +
						`"test/test":{"role": "secondary","mac_address":"0a:58:0a:f4:02:01"}}`,
					"test-pod-invalid-ovn-annot": `invalid`,
				},
				errors.New("failed to unmarshal pod annotation [test/test-pod-invalid-ovn-annot]"),
			),
		)
	})

	Context("UserDefinedNetwork status update", func() {
		DescribeTable("should update status, when",
			func(nad *netv1.NetworkAttachmentDefinition, syncErr error, expectedStatus *udnv1.UserDefinedNetworkStatus) {
				udn := testUDN()
				c := newTestController(noopRenderNadStub(), udn)

				Expect(c.updateUserDefinedNetworkStatus(udn, nad, syncErr)).To(Succeed(), "should update status successfully")

				assertUserDefinedNetworkStatus(cs.UserDefinedNetworkClient, udn, expectedStatus)
			},
			Entry("NAD exist",
				testNAD(),
				nil,
				&udnv1.UserDefinedNetworkStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "NetworkReady",
							Status:  "True",
							Reason:  "NetworkAttachmentDefinitionReady",
							Message: "NetworkAttachmentDefinition has been created",
						},
					},
				},
			),
			Entry("NAD is being deleted",
				testNADWithDeletionTimestamp(time.Now()),
				nil,
				&udnv1.UserDefinedNetworkStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "NetworkReady",
							Status:  "False",
							Reason:  "NetworkAttachmentDefinitionDeleted",
							Message: "NetworkAttachmentDefinition is being deleted",
						},
					},
				},
			),
			Entry("sync error occurred",
				testNAD(),
				errors.New("sync error"),
				&udnv1.UserDefinedNetworkStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "NetworkReady",
							Status:  "False",
							Reason:  "SyncError",
							Message: "sync error",
						},
					},
				},
			),
		)

		It("should update status according to sync errors", func() {
			udn := testUDN()
			c := newTestController(noopRenderNadStub(), udn)

			nad := testNAD()
			syncErr := errors.New("sync error")
			Expect(c.updateUserDefinedNetworkStatus(udn, nad, syncErr)).To(Succeed(), "should update status successfully")

			expectedStatus := &udnv1.UserDefinedNetworkStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "NetworkReady",
						Status:  "False",
						Reason:  "SyncError",
						Message: syncErr.Error(),
					},
				},
			}
			assertUserDefinedNetworkStatus(cs.UserDefinedNetworkClient, udn, expectedStatus)

			anotherSyncErr := errors.New("another sync error")
			Expect(c.updateUserDefinedNetworkStatus(udn, nad, anotherSyncErr)).To(Succeed(), "should update status successfully")

			expectedUpdatedStatus := &udnv1.UserDefinedNetworkStatus{
				Conditions: []metav1.Condition{
					{
						Type:    "NetworkReady",
						Status:  "False",
						Reason:  "SyncError",
						Message: anotherSyncErr.Error(),
					},
				},
			}
			assertUserDefinedNetworkStatus(cs.UserDefinedNetworkClient, udn, expectedUpdatedStatus)
		})

		It("should fail when client update status fails", func() {
			c := newTestController(noopRenderNadStub())

			expectedError := errors.New("test err")
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("patch", "userdefinednetworks/status", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedError
			})

			udn := testUDN()
			nad := testNAD()
			Expect(c.updateUserDefinedNetworkStatus(udn, nad, nil)).To(MatchError(expectedError))
		})
	})

	Context("ClusterUserDefinedNetwork object sync", func() {
		It("should fail", func() {
			c := newTestController(noopRenderNadStub())
			_, err := c.syncClusterUDN(nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("ClusterUserDefinedNetwork status update", func() {
		It("should fail", func() {
			c := newTestController(noopRenderNadStub())
			err := c.updateClusterUDNStatus(nil, nil, nil)
			Expect(err).To(HaveOccurred())
		})
	})
})

func assertUserDefinedNetworkStatus(udnClient udnclient.Interface, udn *udnv1.UserDefinedNetwork, expectedStatus *udnv1.UserDefinedNetworkStatus) {
	actualUDN, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	normalizeConditions(actualUDN.Status.Conditions)

	Expect(actualUDN.Status).To(Equal(*expectedStatus))
}

func assertFinalizersPresent(
	udnClient udnclient.Interface,
	nadClient netv1clientset.Interface,
	udn *udnv1.UserDefinedNetwork,
	pods ...*corev1.Pod,
) {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Namespace+"/"+pod.Name)
	}
	expectedConditionMsg := fmt.Sprintf(`failed to delete NetworkAttachmentDefinition [%s/%s]: network in use by the following pods: %v`,
		udn.Namespace, udn.Name, podNames)

	Eventually(func() []metav1.Condition {
		updatedUDN, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return normalizeConditions(updatedUDN.Status.Conditions)
	}).Should(Equal([]metav1.Condition{{
		Type:    "NetworkReady",
		Status:  "False",
		Reason:  "SyncError",
		Message: expectedConditionMsg,
	}}))
	udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(udn.Finalizers).To(ConsistOf("k8s.ovn.org/user-defined-network-protection"))
	nad, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(nad.Finalizers).To(ConsistOf("k8s.ovn.org/user-defined-network-protection"))
}

func normalizeConditions(conditions []metav1.Condition) []metav1.Condition {
	for i := range conditions {
		t := metav1.NewTime(time.Time{})
		conditions[i].LastTransitionTime = t
	}
	return conditions
}

func testUDN() *udnv1.UserDefinedNetwork {
	return &udnv1.UserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "test",
			UID:        "1",
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
		},
	}
}

func testsUDNWithDeletionTimestamp(ts time.Time) *udnv1.UserDefinedNetwork {
	udn := testUDN()
	deletionTimestamp := metav1.NewTime(ts)
	udn.DeletionTimestamp = &deletionTimestamp
	return udn
}

func testNAD() *netv1.NetworkAttachmentDefinition {
	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "test",
			Labels:     map[string]string{"k8s.ovn.org/user-defined-network": ""},
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         udnv1.SchemeGroupVersion.String(),
					Kind:               "UserDefinedNetwork",
					Name:               "test",
					UID:                "1",
					BlockOwnerDeletion: pointer.Bool(true),
					Controller:         pointer.Bool(true),
				},
			},
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{},
	}
}

func primaryNetNAD() *netv1.NetworkAttachmentDefinition {
	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "primary-net-1",
			Namespace: "test",
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{
			Config: `{"type":"ovn-k8s-cni-overlay","role": "primary"}`,
		},
	}
}

func testNADWithDeletionTimestamp(ts time.Time) *netv1.NetworkAttachmentDefinition {
	nad := testNAD()
	nad.DeletionTimestamp = &metav1.Time{Time: ts}
	return nad
}

func noopRenderNadStub() RenderNetAttachDefManifest {
	return newRenderNadStub(nil, nil)
}

func renderNadStub(nad *netv1.NetworkAttachmentDefinition) RenderNetAttachDefManifest {
	return newRenderNadStub(nad, nil)
}

func failRenderNadStub(err error) RenderNetAttachDefManifest {
	return newRenderNadStub(nil, err)
}

func newRenderNadStub(nad *netv1.NetworkAttachmentDefinition, err error) RenderNetAttachDefManifest {
	return func(obj client.Object, targetNamespace string) (*netv1.NetworkAttachmentDefinition, error) {
		return nad, err
	}
}
