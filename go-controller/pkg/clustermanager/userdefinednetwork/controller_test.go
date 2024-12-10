package userdefinednetwork

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/testing"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1fakeclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	udnfakeclient "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
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
			renderNADStub, f.PodCoreInformer(), f.NamespaceInformer(), nil,
		)
	}

	Context("manager", func() {
		var c *Controller
		AfterEach(func() {
			if c != nil {
				c.Shutdown()
			}
		})
		Context("reconcile UDN CR", func() {
			It("should create NAD successfully", func() {
				udn := testUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created",
				}}))

				nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				Expect(nad).To(Equal(expectedNAD))
			})

			It("should fail when NAD render fail", func() {
				udn := testUDN()
				renderErr := errors.New("render NAD fails")
				c = newTestController(failRenderNadStub(renderErr), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "SyncError",
					Message: "failed to generate NetworkAttachmentDefinition: " + renderErr.Error(),
				}}))

				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(kerrors.IsNotFound(err)).To(BeTrue())
			})
			It("should fail when NAD create fail", func() {
				udn := testUDN()
				c = newTestController(noopRenderNadStub(), udn)

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
					Type:    "NetworkCreated",
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
				c = newTestController(noopRenderNadStub(), udn, foreignNad)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "SyncError",
					Message: "foreign NetworkAttachmentDefinition with the desired name already exist [test/test]",
				}}))
			})
			It("should reconcile mutated NAD", func() {
				udn := testUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
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
				c = newTestController(renderNadStub(expectedNAD), udn)

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
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
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
					Type:    "NetworkCreated",
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
				c = newTestController(noopRenderNadStub(), primaryUDN, primaryNAD)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					updatedUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(updatedUDN.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
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
				c = newTestController(noopRenderNadStub(), primaryUDN, primaryNAD)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					updatedUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(primaryUDN.Namespace).Get(context.Background(), primaryUDN.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(updatedUDN.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "SyncError",
					Message: `failed to validate no primary network exist: unmarshal failed [test/another-primary-net]: invalid character '!' looking for beginning of value`,
				}}))
			})

			It("should add finalizer to UDN", func() {
				udn := testUDN()
				udn.Finalizers = nil
				c = newTestController(noopRenderNadStub(), udn)
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
				c = newTestController(noopRenderNadStub(), udn)

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
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "SyncError",
					Message: `failed to add finalizer to UserDefinedNetwork: ` + expectedErr.Error(),
				}}))
			})

			It("when UDN is being deleted, NAD exist, 2 pods using UDN, should delete NAD once no pod uses the network", func() {
				var err error
				nad := testNAD()
				udn := testUDN()
				udn.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})

				testOVNPodAnnot := map[string]string{util.OvnPodAnnotationName: `{"default": {"role":"primary"}, "test/test": {"role": "secondary"}}`}
				pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}
				pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}

				c = newTestController(renderNadStub(nad), udn, nad, pod1, pod2)
				// user short interval to make the controller re-enqueue requests
				c.networkInUseRequeueInterval = 50 * time.Millisecond
				Expect(c.Run()).To(Succeed())

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

		Context("reconcile CUDN CR", func() {
			It("should create NAD according to spec in each namespace that applies to namespace selector", func() {
				testNamespaces := []string{"red", "blue"}
				var objs []runtime.Object
				for _, nsName := range testNamespaces {
					objs = append(objs, testNamespace(nsName))
				}
				cudn := testClusterUDN("test", testNamespaces...)
				cudn.Spec.Network = udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{}}
				objs = append(objs, cudn)

				c = newTestController(template.RenderNetAttachDefManifest, objs...)
				Expect(c.Run()).To(Succeed())

				expectedNsNADs := map[string]*netv1.NetworkAttachmentDefinition{}
				for _, nsName := range testNamespaces {
					nad := testClusterUdnNAD(cudn.Name, nsName)
					networkName := "cluster.udn." + cudn.Name
					nadName := nsName + "/" + cudn.Name
					nad.Spec.Config = `{"cniVersion":"1.0.0","name":"` + networkName + `","netAttachDefName":"` + nadName + `","role":"","topology":"layer2","type":"ovn-k8s-cni-overlay"}`
					expectedNsNADs[nsName] = nad
				}

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(cudn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [blue, red]",
				}}), "status should reflect NAD exist in test namespaces")
				for testNamespace, expectedNAD := range expectedNsNADs {
					actualNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNamespace).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(actualNAD).To(Equal(expectedNAD), "NAD should exist in test namespaces")
				}
			})

			When("CR exist, and few connected & disconnected namespaces", func() {
				const (
					cudnName       = "global-network"
					testLabelKey   = "test.io"
					testLabelValue = "emea"
				)
				var connectedNsNames []string
				var disconnectedNsNames []string

				BeforeEach(func() {
					var testObjs []runtime.Object
					By("create test namespaces")
					disconnectedNsNames = []string{"red", "blue"}
					for _, nsName := range disconnectedNsNames {
						testObjs = append(testObjs, testNamespace(nsName))
					}
					By("create test namespaces with tests label")
					connectedNsNames = []string{"green", "yellow"}
					testLabelEmea := map[string]string{testLabelKey: testLabelValue}
					for _, nsName := range connectedNsNames {
						ns := testNamespace(nsName)
						ns.Labels = testLabelEmea
						testObjs = append(testObjs, ns)
					}
					By("create CUDN selecting namespaces with test label")
					cudn := testClusterUDN(cudnName)
					cudn.Spec = udnv1.ClusterUserDefinedNetworkSpec{NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      testLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{testLabelValue},
					}}}}
					testObjs = append(testObjs, cudn)

					By("start test controller")
					c = newTestController(renderNadStub(testClusterUdnNAD(cudnName, "")), testObjs...)
					// user short interval to make the controller re-enqueue requests when network in use
					c.networkInUseRequeueInterval = 50 * time.Millisecond
					Expect(c.Run()).To(Succeed())

					Eventually(func() []metav1.Condition {
						var err error
						cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: [green, yellow]",
					}}), "status should report NAD created in test labeled namespaces")
					for _, nsName := range connectedNsNames {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(Equal([]netv1.NetworkAttachmentDefinition{*testClusterUdnNAD(cudnName, nsName)}),
							"NAD should exist in test labeled namespaces")
					}
				})

				It("should reconcile mutated NADs", func() {
					for _, nsName := range connectedNsNames {
						p := []byte(`[{"op":"replace","path":"/spec/config","value":"MUTATED"}]`)
						nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Patch(context.Background(), cudnName, types.JSONPatchType, p, metav1.PatchOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nad.Spec.Config).To(Equal("MUTATED"))
					}

					for _, nsName := range connectedNsNames {
						Eventually(func() *netv1.NetworkAttachmentDefinition {
							nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudnName, metav1.GetOptions{})
							Expect(err).NotTo(HaveOccurred())
							return nad
						}).Should(Equal(testClusterUdnNAD(cudnName, nsName)))
					}
				})

				It("when CR selector has selection added, should create NAD in matching namespaces", func() {
					By("create test new namespaces with new selection label")
					newNsLabelValue := "us"
					newNsLabel := map[string]string{testLabelKey: newNsLabelValue}
					newNsNames := []string{"black", "gray"}
					for _, nsName := range newNsNames {
						ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName, Labels: newNsLabel}}
						ns, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					By("add new label to CR namespace-selector")
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					cudn.Spec.NamespaceSelector.MatchExpressions[0].Values = append(cudn.Spec.NamespaceSelector.MatchExpressions[0].Values, newNsLabelValue)
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
					Expect(cudn.Spec.NamespaceSelector.MatchExpressions).To(Equal([]metav1.LabelSelectorRequirement{{
						Key:      testLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{testLabelValue, newNsLabelValue},
					}}))

					Eventually(func() []metav1.Condition {
						var err error
						cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: [black, gray, green, yellow]",
					}}), "status should report NAD exist in existing and new labeled namespaces")
					for _, nsName := range append(connectedNsNames, newNsNames...) {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(Equal([]netv1.NetworkAttachmentDefinition{*testClusterUdnNAD(cudnName, nsName)}),
							"NAD should exist in existing and new labeled namespaces")
					}
				})

				It("when CR selector has selection removed, should delete stale NADs in previously matching namespaces", func() {
					By("remove test label value from namespace-selector")
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					cudn.Spec.NamespaceSelector.MatchExpressions[0].Values = []string{""}
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
					Expect(cudn.Spec.NamespaceSelector.MatchExpressions).To(Equal([]metav1.LabelSelectorRequirement{{
						Key: testLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{""},
					}}))

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: []",
					}}))
					for _, nsName := range connectedNsNames {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(BeEmpty(),
							"stale NADs should not exist in previously matching namespaces")
					}
				})

				It("when CR is being deleted, NADs used by pods, should not remove finalizers until no pod uses the network", func() {
					var testPods []corev1.Pod
					for _, nsName := range connectedNsNames {
						pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
							Name:        fmt.Sprintf("pod-0"),
							Namespace:   nsName,
							Annotations: map[string]string{util.OvnPodAnnotationName: `{"default": {"role":"primary"}, "` + nsName + `/` + cudnName + `": {"role": "secondary"}}`}},
						}
						pod, err := cs.KubeClient.CoreV1().Pods(nsName).Create(context.Background(), pod, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						testPods = append(testPods, *pod)
					}

					By("mark CR for deletion")
					p := fmt.Sprintf(`[{"op": "replace", "path": "./metadata/deletionTimestamp", "value": %q }]`, "2024-01-01T00:00:00Z")
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Patch(context.Background(), cudnName, types.JSONPatchType, []byte(p), metav1.PatchOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(cudn.DeletionTimestamp.IsZero()).To(BeFalse())

					expectedMessageNADPods := map[string]string{
						"green/global-network":  "green/pod-0",
						"yellow/global-network": "yellow/pod-0",
					}
					Eventually(func(g Gomega) {
						var err error
						cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						// specify Gomega in order to tolerate errors until timeout
						g.Expect(assertConditionReportNetworkInUse(cudn.Status.Conditions, expectedMessageNADPods)).To(Succeed())
					}).Should(Succeed())
					Expect(cudn.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
						"should not remove finalizer from CR when being used by pods")

					remainingPod := &testPods[0]
					podToDelete := testPods[1:]

					By("delete pod, leaving one pod in single target namespace")
					for _, pod := range podToDelete {
						Expect(cs.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).ToNot(HaveOccurred())
					}

					remainingPodKey := fmt.Sprintf("%s/%s", remainingPod.Namespace, remainingPod.Name)
					remainingNADKey := fmt.Sprintf("%s/%s", remainingPod.Namespace, cudnName)
					remainingNADPod := map[string]string{remainingNADKey: remainingPodKey}
					Eventually(func(g Gomega) {
						var err error
						cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						// specify Gomega making eventually tolerate error until timeout
						g.Expect(assertConditionReportNetworkInUse(cudn.Status.Conditions, remainingNADPod)).To(Succeed())
					}).Should(Succeed())
					Expect(cudn.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}),
						"should not remove finalizer from CR when being used by pods")

					By("delete remaining pod")
					Expect(cs.KubeClient.CoreV1().Pods(remainingPod.Namespace).Delete(context.Background(), remainingPod.Name, metav1.DeleteOptions{})).ToNot(HaveOccurred())

					Eventually(func() []string {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return cudn.Finalizers
					}).Should(BeEmpty(), "should remove finalizer from CR when no pod uses the network")
				})

				It("when new namespace is created with matching label, should create NAD in newly created namespaces", func() {
					By("create new namespaces with test label")
					newNsNames := []string{"black", "gray"}
					for _, nsName := range newNsNames {
						ns := testNamespace(nsName)
						ns.Labels = map[string]string{testLabelKey: testLabelValue}
						ns, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: [black, gray, green, yellow]",
					}}), "status should report NAD created in existing and new test namespaces")
					for _, nsName := range append(connectedNsNames, newNsNames...) {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(Equal([]netv1.NetworkAttachmentDefinition{*testClusterUdnNAD(cudnName, nsName)}), "NAD should exist in existing nad new test namespaces")
					}
				})

				It("when existing namespace is labeled with matching label, should create NAD in newly labeled matching namespaces", func() {
					By("add test label to tests disconnected namespaces")
					for _, nsName := range disconnectedNsNames {
						p := fmt.Sprintf(`[{"op": "add", "path": "./metadata/labels", "value": {%q: %q}}]`, testLabelKey, testLabelValue)
						ns, err := cs.KubeClient.CoreV1().Namespaces().Patch(context.Background(), nsName, types.JSONPatchType, []byte(p), metav1.PatchOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns.Labels).To(Equal(map[string]string{testLabelKey: testLabelValue}))
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: [blue, green, red, yellow]",
					}}), "status should report NAD created in existing and new test namespaces")
					for _, nsName := range append(connectedNsNames, disconnectedNsNames...) {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(Equal([]netv1.NetworkAttachmentDefinition{*testClusterUdnNAD(cudnName, nsName)}), "NAD should exist in existing nad new test namespaces")
					}
				})

				It("when existing namespace's matching label removed, should delete stale NADs in previously matching namespaces", func() {
					connectedNsName := connectedNsNames[0]
					staleNADNsNames := connectedNsNames[1:]

					By("remove label from few connected namespaces")
					for _, nsName := range staleNADNsNames {
						p := fmt.Sprintf(`[{"op": "replace", "path": "./metadata/labels", "value": {}}]`)
						ns, err := cs.KubeClient.CoreV1().Namespaces().Patch(context.Background(), nsName, types.JSONPatchType, []byte(p), metav1.PatchOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns.Labels).To(BeEmpty())
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(cudn.Status.Conditions)
					}).Should(Equal([]metav1.Condition{{
						Type:    "NetworkCreated",
						Status:  "True",
						Reason:  "NetworkAttachmentDefinitionCreated",
						Message: "NetworkAttachmentDefinition has been created in following namespaces: [" + connectedNsName + "]",
					}}), "status should report NAD created in label namespace only")

					nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(connectedNsName).List(context.Background(), metav1.ListOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(nads.Items).To(Equal([]netv1.NetworkAttachmentDefinition{*testClusterUdnNAD(cudnName, connectedNsName)}),
						"NAD should exist in matching namespaces only")

					for _, nsName := range staleNADNsNames {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).ToNot(HaveOccurred())
						Expect(nads.Items).To(BeEmpty(), "no NAD should exist in non matching namespaces")
					}
				})
			})

			It("when started, CR exist, stale NADs exist, should deleted stale NADs", func() {
				var testObjs []runtime.Object
				staleNADsNsNames := []string{"red", "blue"}
				staleLabel := map[string]string{"test.io": "stale"}
				for _, nsName := range staleNADsNsNames {
					ns := testNamespace(nsName)
					ns.SetLabels(staleLabel)
					testObjs = append(testObjs, ns)
				}
				connectedNsNames := []string{"green", "yellow"}
				connectedLabel := map[string]string{"test.io": "connected"}
				for _, nsName := range connectedNsNames {
					ns := testNamespace(nsName)
					ns.SetLabels(connectedLabel)
					testObjs = append(testObjs, ns)
				}
				cudn := testClusterUDN("test")
				cudn.Spec = udnv1.ClusterUserDefinedNetworkSpec{NamespaceSelector: metav1.LabelSelector{
					MatchLabels: connectedLabel,
				}}
				testObjs = append(testObjs, cudn)
				for _, nsName := range append(staleNADsNsNames, connectedNsNames...) {
					testObjs = append(testObjs, testClusterUdnNAD(cudn.Name, nsName))
				}
				c = newTestController(renderNadStub(testClusterUdnNAD(cudn.Name, "")), testObjs...)
				Expect(c.Run()).Should(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(cudn.Status.Conditions)
				}, 50*time.Millisecond).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [green, yellow]",
				}}), "status should report NAD created in test labeled namespaces")

				for _, nsName := range staleNADsNsNames {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).To(HaveOccurred())
					Expect(kerrors.IsNotFound(err)).To(Equal(true))
				}
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
							Type:    "NetworkCreated",
							Status:  "True",
							Reason:  "NetworkAttachmentDefinitionCreated",
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
							Type:    "NetworkCreated",
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
							Type:    "NetworkCreated",
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
						Type:    "NetworkCreated",
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
						Type:    "NetworkCreated",
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
		It("should succeed given no CR", func() {
			c := newTestController(noopRenderNadStub())
			_, err := c.syncClusterUDN(nil)
			Expect(err).To(Not(HaveOccurred()))
		})
		It("should succeed when no namespace match namespace-selector", func() {
			cudn := testClusterUDN("test", "red")
			c := newTestController(noopRenderNadStub(), cudn)

			nads, err := c.syncClusterUDN(cudn)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())
		})
		It("should add finalizer to CR", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{}}}
			c := newTestController(noopRenderNadStub(), cudn)

			nads, err := c.syncClusterUDN(cudn)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())

			cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(cudn.Finalizers).To(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))
		})
		It("should fail when update NAD fails", func() {
			expectedErr := errors.New("test err")
			c := newTestController(failRenderNadStub(expectedErr), testNamespace("blue"))

			cudn := testClusterUDN("test", "blue")

			_, err := c.syncClusterUDN(cudn)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when CR is deleted, CR has no finalizer, should succeed", func() {
			deletedCUDN := testClusterUDN("test", "blue")
			deletedCUDN.Finalizers = []string{}
			deletedCUDN.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			c := newTestController(noopRenderNadStub(), deletedCUDN)

			nads, err := c.syncClusterUDN(deletedCUDN)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())
		})
		It("when CR is deleted, should remove finalizer from CR", func() {
			deletedCUDN := testClusterUDN("test", "blue")
			deletedCUDN.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			c := newTestController(noopRenderNadStub(), deletedCUDN)

			nads, err := c.syncClusterUDN(deletedCUDN)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())
			Expect(deletedCUDN.Finalizers).To(BeEmpty())
		})
		Context("CR is being deleted, associate NADs exists", func() {
			const testNsName = "blue"
			var c *Controller
			var cudn *udnv1.ClusterUserDefinedNetwork

			BeforeEach(func() {
				testNs := testNamespace(testNsName)
				cudn = testClusterUDN("test", testNs.Name)
				expectedNAD := testClusterUdnNAD(cudn.Name, testNs.Name)
				c = newTestController(renderNadStub(expectedNAD), cudn, testNs, expectedNAD)

				nads, err := c.syncClusterUDN(cudn)
				Expect(err).ToNot(HaveOccurred())
				Expect(nads).To(ConsistOf(*expectedNAD))

				By("mark CR for deletion")
				cudn.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(cudn.DeletionTimestamp.IsZero()).To(BeFalse())
			})

			It("should delete NAD", func() {
				nads, err := c.syncClusterUDN(cudn)
				Expect(err).ToNot(HaveOccurred())
				Expect(nads).To(BeEmpty())
				Expect(cudn.Finalizers).To(BeEmpty())

				nadList, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNsName).List(context.Background(), metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(nadList.Items).To(BeEmpty())
			})
			It("should fail remove NAD finalizer when update NAD fails", func() {
				expectedErr := errors.New("test err")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("update", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
					return true, nil, expectedErr
				})

				_, err := c.syncClusterUDN(cudn)
				Expect(err).To(MatchError(expectedErr))
			})
			It("should fail remove NAD finalizer when delete NAD fails", func() {
				expectedErr := errors.New("test err")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("delete", "network-attachment-definitions", func(action testing.Action) (bool, runtime.Object, error) {
					return true, nil, expectedErr
				})

				_, err := c.syncClusterUDN(cudn)
				Expect(err).To(MatchError(expectedErr))
			})
		})
	})

	Context("ClusterUserDefinedNetwork status update", func() {
		It("should succeed given no CR", func() {
			c := newTestController(noopRenderNadStub())
			Expect(c.updateClusterUDNStatus(nil, nil, nil)).To(Succeed())
		})
		It("should fail when CR apply status fails", func() {
			cudn := testClusterUDN("test")
			c := newTestController(noopRenderNadStub(), cudn)

			expectedErr := errors.New("test patch error")
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("patch", "clusteruserdefinednetworks", func(action testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			Expect(c.updateClusterUDNStatus(cudn, nil, nil)).ToNot(Succeed())
		})
		It("should reflect active namespaces", func() {
			testNsNames := []string{"red", "green"}

			cudn := testClusterUDN("test", testNsNames...)
			c := newTestController(noopRenderNadStub(), cudn)

			var testNADs []netv1.NetworkAttachmentDefinition
			for _, nsName := range testNsNames {
				testNADs = append(testNADs, *testClusterUdnNAD(cudn.Name, nsName))
			}

			Expect(c.updateClusterUDNStatus(cudn, testNADs, nil)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(cudn.Status.Conditions)).To(ConsistOf([]metav1.Condition{
				{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [green, red]",
				},
			}))
		})
		It("should reflect deleted NADs", func() {
			const nsRed = "red"
			const nsGreen = "green"
			cudn := testClusterUDN("test", nsRed, nsGreen)
			c := newTestController(noopRenderNadStub(), cudn)

			nadRed := *testClusterUdnNAD(cudn.Name, nsRed)
			testNADs := []netv1.NetworkAttachmentDefinition{nadRed}

			nadGreen := *testClusterUdnNAD(cudn.Name, nsGreen)
			nadGreen.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			testNADs = append(testNADs, nadGreen)

			Expect(c.updateClusterUDNStatus(cudn, testNADs, nil)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(cudn.Status.Conditions)).To(ConsistOf([]metav1.Condition{
				{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "NetworkAttachmentDefinitionDeleted",
					Message: "NetworkAttachmentDefinition are being deleted: [green/test]",
				},
			}))
		})
		It("should reflect NAD sync state", func() {
			testNsNames := []string{"red", "green"}

			cudn := testClusterUDN("test", testNsNames...)
			c := newTestController(noopRenderNadStub(), cudn)

			var testNADs []netv1.NetworkAttachmentDefinition
			for _, nsName := range testNsNames {
				testNADs = append(testNADs, *testClusterUdnNAD(cudn.Name, nsName))
			}

			testErr := errors.New("test sync NAD error")
			Expect(c.updateClusterUDNStatus(cudn, testNADs, testErr)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(cudn.Status.Conditions)).To(ConsistOf([]metav1.Condition{
				{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "NetworkAttachmentDefinitionSyncError",
					Message: "test sync NAD error",
				},
			}))
		})
	})
})

// assertConditionReportNetworkInUse checks conditions reflect network consumers.
func assertConditionReportNetworkInUse(conditions []metav1.Condition, messageNADPods map[string]string) error {
	// In order make this check fit Eventually clause in a way it could wait for the expected condition state
	// Gomega Expect is not being used; as they would make Eventually fail immediately.
	// In addition, Gomega equality matcher cannot be used since condition message namespaces order is inconsistent.

	if len(conditions) != 1 {
		return fmt.Errorf("expeced conditions to have len 1, got: %d", len(conditions))
	}

	c := conditions[0]
	if c.Type != "NetworkCreated" ||
		c.Status != metav1.ConditionFalse ||
		c.Reason != "NetworkAttachmentDefinitionSyncError" {

		return fmt.Errorf("got condition in unexpected state: %+v", c)
	}

	for nadKey, podKey := range messageNADPods {
		expectedToken := fmt.Sprintf("failed to delete NetworkAttachmentDefinition [%s]: network in use by the following pods: [%s]", nadKey, podKey)
		if !strings.Contains(c.Message, expectedToken) {
			return fmt.Errorf("condition message dosent contain expected token %q, got: %q", expectedToken, c.Message)
		}
	}

	return nil
}

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
		Type:    "NetworkCreated",
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

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/metadata.name": name,
			},
		},
	}
}

func testClusterUDN(name string, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Labels:     map[string]string{"k8s.ovn.org/user-defined-network": ""},
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
			Name:       name,
			UID:        "1",
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      corev1.LabelMetadataName,
					Operator: metav1.LabelSelectorOpIn,
					Values:   targetNamespaces,
				},
			}},
			Network: udnv1.NetworkSpec{},
		},
	}
}

func testClusterUdnNAD(name, namespace string) *netv1.NetworkAttachmentDefinition {
	return &netv1.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Labels:     map[string]string{"k8s.ovn.org/user-defined-network": ""},
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         udnv1.SchemeGroupVersion.String(),
					Kind:               "ClusterUserDefinedNetwork",
					Name:               name,
					UID:                "1",
					BlockOwnerDeletion: pointer.Bool(true),
					Controller:         pointer.Bool(true),
				},
			},
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{},
	}
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
