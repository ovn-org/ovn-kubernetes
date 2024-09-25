package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	informerfactory "k8s.io/client-go/informers"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/testing"
	"k8s.io/utils/pointer"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1fakeclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	netv1informerfactory "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"
	netv1Informer "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	udninformerfactory "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions"
	udninformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/informers/externalversions/userdefinednetwork/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

var _ = Describe("User Defined Network Controller", func() {
	var (
		udnClient   *udnfakeclient.Clientset
		nadClient   *netv1fakeclientset.Clientset
		udnInformer udninformer.UserDefinedNetworkInformer
		nadInformer netv1Informer.NetworkAttachmentDefinitionInformer
		kubeClient  *fake.Clientset
		podInformer corev1informer.PodInformer
	)

	BeforeEach(func() {
		udnClient = udnfakeclient.NewSimpleClientset()
		udnInformer = udninformerfactory.NewSharedInformerFactory(udnClient, 15).K8s().V1().UserDefinedNetworks()
		nadClient = netv1fakeclientset.NewSimpleClientset()
		nadInformer = netv1informerfactory.NewSharedInformerFactory(nadClient, 15).K8sCniCncfIo().V1().NetworkAttachmentDefinitions()

		kubeClient = fake.NewSimpleClientset()
		sharedInformer := informerfactory.NewSharedInformerFactoryWithOptions(kubeClient, 15)
		podInformer = sharedInformer.Core().V1().Pods()
	})

	Context("controller", func() {
		var f *factory.WatchFactory

		BeforeEach(func() {
			// Restore global default values before each testcase
			Expect(config.PrepareTestConfig()).To(Succeed())
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true

			fakeClient := &util.OVNClusterManagerClientset{
				KubeClient:               kubeClient,
				NetworkAttchDefClient:    nadClient,
				UserDefinedNetworkClient: udnClient,
			}
			var err error
			f, err = factory.NewClusterManagerWatchFactory(fakeClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(f.Start()).To(Succeed())
		})

		AfterEach(func() {
			f.Shutdown()
		})

		It("should create NAD successfully", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedNAD := testNAD()
			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), renderNadStub(expectedNAD), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(udn.Status.Conditions)
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionReady",
				Message: "NetworkAttachmentDefinition has been created",
			}}))

			nad, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			Expect(nad).To(Equal(expectedNAD))
		})

		It("should fail when NAD render fail", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			renderErr := errors.New("render NAD fails")

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), failRenderNadStub(renderErr), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(udn.Status.Conditions)
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "False",
				Reason:  "SyncError",
				Message: "failed to generate NetworkAttachmentDefinition: " + renderErr.Error(),
			}}))

			_, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
			Expect(kerrors.IsNotFound(err)).To(BeTrue())
		})
		It("should fail when NAD create fail", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedError := errors.New("create NAD error")
			nadClient.PrependReactor("create", "network-attachment-definitions", func(action testing.Action) (handled bool, ret runtime.Object, err error) {
				return true, nil, expectedError
			})

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Status.Conditions
			}).ShouldNot(BeEmpty())

			Expect(normalizeConditions(udn.Status.Conditions)).To(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "False",
				Reason:  "SyncError",
				Message: "failed to create NetworkAttachmentDefinition: create NAD error",
			}}))

			_, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
			Expect(kerrors.IsNotFound(err)).To(BeTrue())
		})

		It("should fail when foreign NAD exist", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			foreignNad := testNAD()
			foreignNad.ObjectMeta.OwnerReferences = nil
			foreignNad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), foreignNad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Status.Conditions
			}).ShouldNot(BeEmpty())

			Expect(normalizeConditions(udn.Status.Conditions)).To(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "False",
				Reason:  "SyncError",
				Message: "foreign NetworkAttachmentDefinition with the desired name already exist [test/test]",
			}}))
		})
		It("should reconcile mutated NAD", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedNAD := testNAD()

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), renderNadStub(expectedNAD), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Status.Conditions
			}).ShouldNot(BeEmpty())
			Expect(normalizeConditions(udn.Status.Conditions)).To(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionReady",
				Message: "NetworkAttachmentDefinition has been created",
			}}))

			mutatedNAD := expectedNAD.DeepCopy()
			p := []byte(`[{"op":"replace","path":"/spec/config","value":"MUTATED"}]`)
			mutatedNAD, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Patch(context.Background(), mutatedNAD.Name, types.JSONPatchType, p, metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() *netv1.NetworkAttachmentDefinition {
				updatedNAD, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return updatedNAD
			}).Should(Equal(expectedNAD))
		})
		It("should fail when update mutated NAD fails", func() {
			expectedNAD := testNAD()

			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), renderNadStub(expectedNAD), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Status.Conditions
			}).ShouldNot(BeEmpty())
			Expect(normalizeConditions(udn.Status.Conditions)).To(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionReady",
				Message: "NetworkAttachmentDefinition has been created",
			}}))

			actualNAD, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(actualNAD).To(Equal(expectedNAD))

			expectedErr := errors.New("update error")
			nadClient.PrependReactor("update", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			mutatedNAD := expectedNAD.DeepCopy()
			p := []byte(`[{"op":"replace","path":"/spec/config","value":"MUTATED"}]`)
			mutatedNAD, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Patch(context.Background(), mutatedNAD.Name, types.JSONPatchType, p, metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() []metav1.Condition {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(udn.Status.Conditions)
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "False",
				Reason:  "SyncError",
				Message: "failed to update NetworkAttachmentDefinition: " + expectedErr.Error(),
			}}))

			Eventually(func() *netv1.NetworkAttachmentDefinition {
				updatedNAD, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return updatedNAD
			}).Should(Equal(mutatedNAD))
		})

		It("given primary UDN, should fail when primary NAD already exist", func() {
			targetNs := "test"

			primaryNAD := primaryNetNAD()
			primaryNAD, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(targetNs).Create(context.Background(), primaryNAD, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			primaryUDN := testUDN()
			primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer2
			primaryUDN.Spec.Layer2 = &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary}
			primaryUDN, err = udnClient.K8sV1().UserDefinedNetworks(targetNs).Create(context.Background(), primaryUDN, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				updatedUDN, err := udnClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
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
			targetNs := "test"

			primaryNAD := primaryNetNAD()
			primaryNAD.Name = "another-primary-net"
			primaryNAD.Spec.Config = "!@#$"
			primaryNAD, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(targetNs).Create(context.Background(), primaryNAD, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			primaryUDN := testUDN()
			primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer3
			primaryUDN.Spec.Layer3 = &udnv1.Layer3Config{Role: udnv1.NetworkRolePrimary}
			primaryUDN, err = udnClient.K8sV1().UserDefinedNetworks(targetNs).Create(context.Background(), primaryUDN, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				updatedUDN, err := udnClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
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
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []string {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Finalizers
			}).Should(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))
		})
		It("should fail when add finalizer to UDN fails", func() {
			udn := testUDN()
			udn.Finalizers = nil
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedErr := errors.New("update UDN error")
			udnClient.PrependReactor("update", "userdefinednetworks", func(action testing.Action) (handled bool, obj runtime.Object, err error) {
				return true, nil, expectedErr
			})

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), noopRenderNadStub(), f.PodCoreInformer(), nil)
			Expect(c.Run()).To(Succeed())

			Eventually(func() []metav1.Condition {
				updatedUDN, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(updatedUDN.Status.Conditions)
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkReady",
				Status:  "False",
				Reason:  "SyncError",
				Message: `failed to add finalizer to UserDefinedNetwork: ` + expectedErr.Error(),
			}}))
		})

		It("when UDN is being deleted, NAD exist, 2 pods using UDN, should remove finalizers once no pod uses the network", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad, err := nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), testNAD(), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			podMeta := metav1.ObjectMeta{
				Namespace:   udn.Namespace,
				Annotations: map[string]string{util.OvnPodAnnotationName: `{"default": {"role":"primary"}, "test/test": {"role": "secondary"}}`},
			}

			podInf := f.PodCoreInformer()
			var testPods []corev1.Pod
			for i := 0; i < 2; i++ {
				pod := corev1.Pod{ObjectMeta: podMeta}
				pod.Name = fmt.Sprintf("pod-%d", i)
				Expect(podInf.Informer().GetIndexer().Add(&pod)).Should(Succeed())
				testPods = append(testPods, pod)
			}

			c := New(nadClient, f.NADInformer(), udnClient, f.UserDefinedNetworkInformer(), renderNadStub(nad), podInf, nil)
			// user short interval to make the controller re-enqueue requests
			c.networkInUseRequeueInterval = 50 * time.Millisecond
			Expect(c.Run()).To(Succeed())

			assertFinalizersPresent(udnClient, nadClient, udn, testPods...)

			Expect(podInf.Informer().GetIndexer().Delete(&testPods[0])).To(Succeed())

			assertFinalizersPresent(udnClient, nadClient, udn, testPods[1])

			Expect(podInf.Informer().GetIndexer().Delete(&testPods[1])).To(Succeed())

			Eventually(func() []string {
				udn, err = udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return udn.Finalizers
			}).Should(BeEmpty(), "should remove finalizer on UDN following deletion and not being used")
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(nad.Finalizers).To(BeEmpty(), "should remove finalizer on NAD following deletion and not being used")
		})
	})

	Context("UserDefinedNetwork object sync", func() {
		It("should fail when NAD owner-reference is malformed", func() {
			udn := testUDN()
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			c := New(nadClient, nadInformer, udnClient, udnInformer, renderNadStub(nad), nil, nil)

			mutetedNAD := nad.DeepCopy()
			mutetedNAD.ObjectMeta.OwnerReferences = []metav1.OwnerReference{{Kind: "DifferentKind"}}
			mutetedNAD, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), mutetedNAD, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			_, err = c.syncUserDefinedNetwork(udn, mutetedNAD)
			Expect(err).To(Equal(errors.New("foreign NetworkAttachmentDefinition with the desired name already exist [test/test]")))
		})

		It("when UDN is being deleted, should not remove finalizer from non managed NAD", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			unmanagedNAD := testNAD()
			unmanagedNAD.OwnerReferences[0].UID = "99"
			unmanagedNAD, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), unmanagedNAD, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			_, err = c.syncUserDefinedNetwork(udn, unmanagedNAD)
			Expect(err).ToNot(HaveOccurred())

			expectedFinalizers := testNAD().Finalizers

			Expect(unmanagedNAD.Finalizers).To(Equal(expectedFinalizers))
		})
		It("when UDN is being deleted, and NAD exist, should remove finalizer from NAD", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			_, err = c.syncUserDefinedNetwork(udn, nad)
			Expect(err).ToNot(HaveOccurred())
			Expect(nad.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, and NAD exist, should fail when remove NAD finalizer fails", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedErr := errors.New("update NAD error")
			nadClient.PrependReactor("update", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			_, err = c.syncUserDefinedNetwork(udn, nad)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when UDN is being deleted, and NAD exist w/o finalizer, should remove finalizer from UDN", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			nad.Finalizers = nil
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			_, err = c.syncUserDefinedNetwork(udn, nad)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, and NAD not exist, should remove finalizer from UDN", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			_, err = c.syncUserDefinedNetwork(udn, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, should fail removing finalizer from UDN when patch fails", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			nad.Finalizers = nil
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			expectedErr := errors.New("update UDN error")
			udnClient.PrependReactor("update", "userdefinednetworks", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			_, err = c.syncUserDefinedNetwork(udn, nad)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when UDN is being deleted, NAD exist, pod exist, should remove finalizers when network not being used", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			nad := testNAD()
			nad, err = nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Create(context.Background(), nad, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

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
			Expect(podInformer.Informer().GetIndexer().Add(pod)).Should(Succeed())
			c := New(nadClient, nadInformer, udnClient, udnInformer, renderNadStub(nad), podInformer, nil)

			nad, err = c.syncUserDefinedNetwork(udn, nad)
			Expect(err).ToNot(HaveOccurred())

			Expect(nad.Finalizers).To(BeEmpty())
			Expect(udn.Finalizers).To(BeEmpty())
		})

		DescribeTable("when UDN is being deleted, NAD exist, should not remove finalizers when",
			func(podOvnAnnotations map[string]string, expectedErr error) {
				udn := testsUDNWithDeletionTimestamp(time.Now())
				Expect(udnInformer.Informer().GetIndexer().Add(udn)).To(Succeed())

				nad := testNAD()
				Expect(nadInformer.Informer().GetIndexer().Add(nad)).To(Succeed())

				for podName, ovnAnnotValue := range podOvnAnnotations {
					pod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name: podName, Namespace: udn.Namespace,
							Annotations: map[string]string{util.OvnPodAnnotationName: ovnAnnotValue},
						},
					}
					Expect(podInformer.Informer().GetIndexer().Add(pod)).Should(Succeed())
				}

				c := New(nadClient, nadInformer, udnClient, udnInformer, renderNadStub(nad), podInformer, nil)

				_, err := c.syncUserDefinedNetwork(udn, nad)
				Expect(err).To(MatchError(ContainSubstring(expectedErr.Error())))

				actual, _, err := nadInformer.Informer().GetIndexer().Get(nad)
				Expect(err).NotTo(HaveOccurred())
				Expect(actual.(*netv1.NetworkAttachmentDefinition).Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
					"finalizer should remain until no pod uses the network")

				actualUDN, _, err := udnInformer.Informer().GetIndexer().Get(udn)
				Expect(actualUDN.(*udnv1.UserDefinedNetwork).Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
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
				udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

				Expect(c.updateUserDefinedNetworkStatus(udn, nad, syncErr)).To(Succeed(), "should update status successfully")

				assertUserDefinedNetworkStatus(udnClient, udn, expectedStatus)
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
			udn, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Create(context.Background(), udn, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

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
			assertUserDefinedNetworkStatus(udnClient, udn, expectedStatus)

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
			assertUserDefinedNetworkStatus(udnClient, udn, expectedUpdatedStatus)
		})

		It("should fail when client update status fails", func() {
			c := New(nadClient, nadInformer, udnClient, udnInformer, noopRenderNadStub(), podInformer, nil)

			expectedError := errors.New("test err")
			udnClient.PrependReactor("patch", "userdefinednetworks/status", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedError
			})

			udn := testUDN()
			nad := testNAD()
			Expect(c.updateUserDefinedNetworkStatus(udn, nad, nil)).To(MatchError(expectedError))
		})
	})
})

func assertUserDefinedNetworkStatus(udnClient *udnfakeclient.Clientset, udn *udnv1.UserDefinedNetwork, expectedStatus *udnv1.UserDefinedNetworkStatus) {
	actualUDN, err := udnClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	normalizeConditions(actualUDN.Status.Conditions)

	Expect(actualUDN.Status).To(Equal(*expectedStatus))
}

func assertFinalizersPresent(
	udnClient *udnfakeclient.Clientset,
	nadClient *netv1fakeclientset.Clientset,
	udn *udnv1.UserDefinedNetwork,
	pods ...corev1.Pod,
) {
	var podNames []string
	for _, pod := range pods {
		podNames = append(podNames, pod.Namespace+"/"+pod.Name)
	}
	expectedConditionMsg := fmt.Sprintf(`failed to verify NAD not in use [%s/%s]: network in use by the following pods: %v`,
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
	return func(udn *udnv1.UserDefinedNetwork) (*netv1.NetworkAttachmentDefinition, error) {
		return nad, err
	}
}
