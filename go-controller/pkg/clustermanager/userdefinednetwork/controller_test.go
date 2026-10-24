// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package userdefinednetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	netv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netv1clientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	netv1fakeclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/fake"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/allocator/id"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/clustermanager/userdefinednetwork/template"
	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	apitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	udnfakeclient "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned/fake"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepinformer "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/informers/externalversions/vtep/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("User Defined Network Controller", func() {
	var (
		cs *util.OVNClusterManagerClientset
		f  *factory.WatchFactory
		nm networkmanager.Controller
	)

	BeforeEach(func() {
		// Restore global default values before each testcase
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		// Enable EVPN for EVPN-related tests
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		metrics.RegisterClusterManagerFunctional()
		// satisfy EVPN LGW restriction, otherwise no effect
		config.Gateway.Mode = config.GatewayModeLocal
	})

	AfterEach(func() {
		if nm != nil {
			nm.Stop()
			nm = nil
		}
		if f != nil {
			f.Shutdown()
			f = nil
		}
	})

	newTestController := func(renderNADStub RenderNetAttachDefManifest, objects ...runtime.Object) *Controller {
		cs = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		f, err = factory.NewClusterManagerWatchFactory(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Start()).To(Succeed())

		networkManager, err := networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, f, cs, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
		Expect(err).NotTo(HaveOccurred())
		var vtepInformer vtepinformer.VTEPInformer
		if util.IsEVPNEnabled() {
			vtepInformer = f.VTEPInformer()
		}
		return New(cs.NetworkAttchDefClient, f.NADInformer(),
			cs.UserDefinedNetworkClient, f.UserDefinedNetworkInformer(), f.ClusterUserDefinedNetworkInformer(),
			renderNADStub, networkManager.Interface(), f.PodCoreInformer(), f.NamespaceInformer(), vtepInformer, f.RouteAdvertisementsInformer(), nil,
		)
	}

	// newTestControllerWithNetworkManager creates a controller with a started NetworkManager.
	newTestControllerWithNetworkManager := func(renderNADStub RenderNetAttachDefManifest, objects ...runtime.Object) *Controller {
		cs = util.GetOVNClientset(objects...).GetClusterManagerClientset()
		var err error
		f, err = factory.NewClusterManagerWatchFactory(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(f.Start()).To(Succeed())

		nm, err = networkmanager.NewForCluster(&networkmanager.FakeControllerManager{}, f, cs, nil, id.NewTunnelKeyAllocator("TunnelKeys"))
		Expect(err).NotTo(HaveOccurred())
		// Start NetworkManager - it will process existing NADs and cache their VIDs
		Expect(nm.Start()).To(Succeed())

		var vtepInformer vtepinformer.VTEPInformer
		if util.IsEVPNEnabled() {
			vtepInformer = f.VTEPInformer()
		}
		return New(cs.NetworkAttchDefClient, f.NADInformer(),
			cs.UserDefinedNetworkClient, f.UserDefinedNetworkInformer(), f.ClusterUserDefinedNetworkInformer(),
			renderNADStub, nm.Interface(), f.PodCoreInformer(), f.NamespaceInformer(), vtepInformer, f.RouteAdvertisementsInformer(), nil,
		)
	}

	expectTransportCondition := func(cudnName string, status metav1.ConditionStatus, reason, message string) {
		Eventually(func() bool {
			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			for _, cond := range cudn.Status.Conditions {
				if cond.Type == ConditionTypeTransportAccepted && cond.Status == status && cond.Reason == reason {
					if message == "" || cond.Message == message {
						return true
					}
				}
			}
			return false
		}, 2*time.Second, 100*time.Millisecond).Should(BeTrue())
	}

	// markCUDNForDeletion marks a CUDN for deletion by setting its DeletionTimestamp to the current time.
	// The controller may concurrently update the CUDN from a stale lister copy (e.g. re-adding the
	// finalizer), and the fake client performs no resourceVersion conflict checks, so such an update
	// silently clears the DeletionTimestamp. Retry until a fresh Get observes the mark persisted.
	markCUDNForDeletion := func(name string) {
		GinkgoHelper()
		now := metav1.Now()
		Eventually(func() bool {
			cudnObj, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if !cudnObj.DeletionTimestamp.IsZero() {
				return true
			}
			cudnObj.DeletionTimestamp = &now
			_, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudnObj, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			return false
		}, 2*time.Second, 50*time.Millisecond).Should(BeTrue(), "DeletionTimestamp should persist on ClusterUserDefinedNetwork %q", name)
	}

	Context("manager", func() {
		var c *Controller
		AfterEach(func() {
			if c != nil {
				c.Shutdown()
				c = nil
			}
		})
		Context("reconcile UDN CR", func() {
			It("should preserve NAD-local annotations", func() {
				udn := testSecondaryUDN()
				nad := testNAD()
				nad.Annotations = map[string]string{
					"example.com/partition-name":    "local-partition",
					ovntypes.OvnNetworkIDAnnotation: "6",
				}
				desiredNAD := testNAD()
				desiredNAD.Annotations = map[string]string{"parent": "value"}
				c = newTestController(renderNadStub(desiredNAD), udn, nad, testNamespace(udn.Namespace))
				Expect(c.Run()).To(Succeed())

				Eventually(func(g Gomega) {
					actualNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(actualNAD.Annotations).To(Equal(map[string]string{
						"parent":                        "value",
						"example.com/partition-name":    "local-partition",
						ovntypes.OvnNetworkIDAnnotation: "6",
					}))
				}, 2*time.Second).Should(Succeed())
			})

			It("should report annotation reconciliation failures", func() {
				udn := testSecondaryUDN()
				udn.Annotations = map[string]string{"example.com/team": "networking"}
				nad, err := template.RenderNetAttachDefManifest(udn, udn.Namespace)
				Expect(err).NotTo(HaveOccurred(), "render the existing NAD")
				nad.Annotations = nil
				c = newTestController(template.RenderNetAttachDefManifest, udn, nad, testNamespace(udn.Namespace))
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("patch", "network-attachment-definitions",
					func(action testing.Action) (bool, runtime.Object, error) {
						if action.(testing.PatchAction).GetPatchType() == types.ApplyPatchType {
							return true, nil, errors.New("apply error")
						}
						return false, nil, nil
					})
				Expect(c.Run()).To(Succeed(), "start the controller")

				Eventually(func(g Gomega) {
					actualUDN, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).
						Get(context.Background(), udn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred(), "get the UDN status")
					g.Expect(actualUDN.Status.Conditions).To(ContainElement(And(
						HaveField("Type", "NetworkCreated"),
						HaveField("Status", metav1.ConditionFalse),
						HaveField("Message", ContainSubstring("apply error")),
					)), "report the annotation apply failure")
				}, 2*time.Second).Should(Succeed())
			})

			It("should create NAD successfully", func() {
				udn := testPrimaryUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn, testNamespace("test"))
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

			It("should fail when required namespace label is missing for primary network", func() {
				udn := testPrimaryUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn, invalidTestNamespace("test"))
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "SyncError",
					Message: "invalid primary network state for namespace \"test\": a valid primary user defined network or network attachment definition custom resource, and required namespace label \"k8s.ovn.org/primary-user-defined-network\" must both be present",
				}}))

				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})

			It("should NOT fail when required namespace label is missing for secondary network", func() {
				udn := testSecondaryUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn, invalidTestNamespace("test"))
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
				udn := testPrimaryUDN()
				renderErr := errors.New("render NAD fails")
				c = newTestController(failRenderNadStub(renderErr), udn, testNamespace("test"))
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
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})
			It("should fail when NAD create fail", func() {
				udn := testPrimaryUDN()
				c = newTestController(noopRenderNadStub(), udn, testNamespace("test"))

				expectedError := errors.New("create NAD error")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("create", "network-attachment-definitions", func(testing.Action) (handled bool, ret runtime.Object, err error) {
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
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})

			It("should fail when foreign NAD exist", func() {
				udn := testPrimaryUDN()
				foreignNad := testNAD()
				foreignNad.ObjectMeta.OwnerReferences = nil
				c = newTestController(noopRenderNadStub(), udn, foreignNad, testNamespace("test"))
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
				udn := testPrimaryUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn, testNamespace("test"))
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
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Update(context.Background(), mutatedNAD, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() *netv1.NetworkAttachmentDefinition {
					updatedNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return updatedNAD
				}).Should(Equal(expectedNAD))
			})
			It("should fail when update mutated NAD fails", func() {
				udn := testPrimaryUDN()
				expectedNAD := testNAD()
				c = newTestController(renderNadStub(expectedNAD), udn, testNamespace("test"))

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
				primaryUDN := testPrimaryUDN()
				primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer2
				primaryUDN.Spec.Layer2 = &udnv1.Layer2Config{Role: udnv1.NetworkRolePrimary}

				primaryNAD := primaryNetNAD()
				c = newTestController(noopRenderNadStub(), primaryUDN, primaryNAD, testNamespace("test"))
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
				primaryUDN := testPrimaryUDN()
				primaryUDN.Spec.Topology = udnv1.NetworkTopologyLayer3
				primaryUDN.Spec.Layer3 = &udnv1.Layer3Config{Role: udnv1.NetworkRolePrimary}

				primaryNAD := primaryNetNAD()
				primaryNAD.Name = "another-primary-net"
				primaryNAD.Spec.Config = "!@#$"
				c = newTestController(noopRenderNadStub(), primaryUDN, primaryNAD, testNamespace("test"))
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
				udn := testPrimaryUDN()
				udn.Finalizers = nil
				c = newTestController(noopRenderNadStub(), udn, testNamespace("test"))
				Expect(c.Run()).To(Succeed())

				Eventually(func() []string {
					udn, err := cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return udn.Finalizers
				}).Should(Equal([]string{"k8s.ovn.org/user-defined-network-protection"}))
			})
			It("should fail when add finalizer to UDN fails", func() {
				udn := testPrimaryUDN()
				udn.Finalizers = nil
				c = newTestController(noopRenderNadStub(), udn, testNamespace("test"))

				expectedErr := errors.New("update UDN error")
				cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("update", "userdefinednetworks", func(testing.Action) (bool, runtime.Object, error) {
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
				udn := testPrimaryUDN()
				udn.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})

				testOVNPodAnnot := map[string]string{ovntypes.OvnPodAnnotationName: `{"default": {"role":"primary"}, "test/test": {"role": "secondary"}}`}
				pod1 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}
				pod2 := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: udn.Namespace, Annotations: testOVNPodAnnot}}

				c = newTestController(renderNadStub(nad), udn, nad, pod1, pod2, testNamespace("test"))
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
				_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nad.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsNotFound(err)).To(BeTrue())
			})

			It("should publish the ID-derived VRF name of a primary UDN", func() {
				udn := testPrimaryUDN()
				udn.Spec.Layer3.Subnets = []udnv1.Layer3Subnet{{CIDR: "10.20.0.0/16"}}

				c = newTestController(template.RenderNetAttachDefManifest, udn, testNamespace(udn.Namespace))
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					udn, err = cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(udn.Status.Conditions)
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created",
				}}))
				Expect(udn.Status.VRFName).To(BeNil(),
					"the VRF name depends on the network ID and must not be published before the NAD is annotated with it")

				By("annotating the NAD with the network ID")
				Eventually(func() error {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if nad.Annotations == nil {
						nad.Annotations = map[string]string{}
					}
					nad.Annotations[ovntypes.OvnNetworkIDAnnotation] = "9"
					_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Update(context.Background(), nad, metav1.UpdateOptions{})
					return err
				}).Should(Succeed())

				Eventually(func() string {
					var err error
					udn, err = cs.UserDefinedNetworkClient.K8sV1().UserDefinedNetworks(udn.Namespace).Get(context.Background(), udn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return ptr.Deref(udn.Status.VRFName, "")
				}).Should(Equal("mp9-udn-vrf"), "status should expose the ID-derived VRF name once the network ID is known")
			})

			It("should derive the published VRF name from the UDN sync outcome", func() {
				udn := testPrimaryUDN()
				udn.Status.VRFName = ptr.To("mp9-udn-vrf")
				nadWithoutID := testNAD()
				nadWithID := testNAD()
				nadWithID.Annotations = map[string]string{ovntypes.OvnNetworkIDAnnotation: "8"}

				Expect(udnVRFName(udn, nadWithID, nil, true)).To(Equal("mp8-udn-vrf"),
					"the annotated network ID must drive the published name")
				Expect(udnVRFName(udn, nadWithoutID, nil, true)).To(Equal("mp9-udn-vrf"),
					"the published name must be preserved while the NAD is not annotated with the network ID yet")
				Expect(udnVRFName(udn, nil, errors.New("sync failed"), true)).To(Equal("mp9-udn-vrf"),
					"a failed sync must not clear the published name while the NAD remains")
				Expect(udnVRFName(udn, nil, errors.New("sync failed"), false)).To(BeEmpty(),
					"a sync that failed after the NAD was deleted must clear the name, since the released network ID may be reallocated")
				Expect(udnVRFName(udn, nil, nil, false)).To(BeEmpty(),
					"the name must clear once the NAD is gone, since the released network ID may be reallocated")

				udn.Spec.Layer3.Role = udnv1.NetworkRoleSecondary
				Expect(udnVRFName(udn, nadWithID, nil, true)).To(BeEmpty(),
					"the VRF name is published only for primary networks")
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
				cudn.Spec.Network = udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{
					Subnets: udnv1.DualStackCIDRs{"10.10.10.0/24"},
				}}
				objs = append(objs, cudn)

				c = newTestController(template.RenderNetAttachDefManifest, objs...)
				Expect(c.Run()).To(Succeed())

				expectedNsNADs := map[string]*netv1.NetworkAttachmentDefinition{}
				for _, nsName := range testNamespaces {
					nad := testClusterUdnNAD(cudn.Name, nsName)
					networkName := ovntypes.CUDNPrefix + cudn.Name
					nadName := nsName + "/" + cudn.Name
					nad.Spec.Config = `{"cniVersion":"1.1.0","name":"` + networkName + `","netAttachDefName":"` + nadName + `","role":"","subnets":"10.10.10.0/24","topology":"layer2","type":"ovn-k8s-cni-overlay"}`
					expectedNsNADs[nsName] = nad
				}

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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

			It("should report an error when spec.uplinks is set but the Uplink feature is disabled", func() {
				config.OVNKubernetesFeature.EnableUplink = false
				nsName := "red"
				cudn := testLayer2SecondaryClusterUDN("test-uplink-disabled", nsName)
				cudn.Spec.Uplinks = []string{"tenant-blue"}

				c = newTestController(template.RenderNetAttachDefManifest, testNamespace(nsName), cudn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "NetworkAttachmentDefinitionSyncError",
					Message: "spec.uplinks is set but the Uplink feature is disabled, ignoring this CUDN",
				}}), "status should report the Uplink feature is disabled")

				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "NAD must not be created when the Uplink feature is disabled")
			})

			It("should publish the VRF name of a primary CUDN whose name fits the device name length limit", func() {
				nsName := "red"
				cudn := testLayer3PrimaryClusterUDN("tenant-red", nsName)

				c = newTestController(template.RenderNetAttachDefManifest, testNamespace(nsName), cudn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() string {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return ptr.Deref(cudn.Status.VRFName, "")
				}).Should(Equal("tenant-red"), "status should expose the CUDN name as the VRF name")
			})

			It("should publish the ID-derived VRF name of a primary CUDN whose name exceeds the device name length limit", func() {
				nsName := "red"
				cudn := testLayer3PrimaryClusterUDN("tenant-with-a-very-long-name", nsName)

				c = newTestController(template.RenderNetAttachDefManifest, testNamespace(nsName), cudn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [red]",
				}}))
				Expect(cudn.Status.VRFName).To(BeNil(),
					"the VRF name depends on the network ID and must not be published before the NAD is annotated with it")

				By("annotating the NAD with the network ID")
				Eventually(func() error {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if nad.Annotations == nil {
						nad.Annotations = map[string]string{}
					}
					nad.Annotations[ovntypes.OvnNetworkIDAnnotation] = "7"
					_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Update(context.Background(), nad, metav1.UpdateOptions{})
					return err
				}).Should(Succeed())

				Eventually(func() string {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return ptr.Deref(cudn.Status.VRFName, "")
				}).Should(Equal("mp7-udn-vrf"), "status should expose the ID-derived VRF name once the network ID is known")
			})

			// The fake clientset applies status patches with merge semantics and
			// never removes omitted fields, so the SSA-based clearing of
			// status.vrfName cannot be observed end to end here; exercise the
			// name-derivation branches directly instead.
			It("should derive the published VRF name from the CUDN sync outcome", func() {
				cudn := testLayer3PrimaryClusterUDN("tenant-with-a-very-long-name")
				cudn.Status.VRFName = ptr.To("mp7-udn-vrf")
				nadWithoutID := *testClusterUdnNAD(cudn.Name, "red")
				nadWithID := nadWithoutID
				nadWithID.Annotations = map[string]string{ovntypes.OvnNetworkIDAnnotation: "8"}
				nadWithReservedID := nadWithoutID
				nadWithReservedID.Annotations = map[string]string{ovntypes.OvnNetworkIDAnnotation: "0"}

				Expect(clusterUDNVRFName(cudn, []netv1.NetworkAttachmentDefinition{nadWithID}, nil, true)).To(Equal("mp8-udn-vrf"),
					"the annotated network ID must drive the published name")
				Expect(clusterUDNVRFName(cudn, []netv1.NetworkAttachmentDefinition{nadWithoutID}, nil, true)).To(Equal("mp7-udn-vrf"),
					"the published name must be preserved while the NADs are not annotated with the network ID yet")
				Expect(clusterUDNVRFName(cudn, []netv1.NetworkAttachmentDefinition{nadWithReservedID}, nil, true)).To(Equal("mp7-udn-vrf"),
					"an annotation carrying the reserved default-network ID must be treated as unknown, not published")
				nadWithMalformedID := nadWithoutID
				nadWithMalformedID.Annotations = map[string]string{ovntypes.OvnNetworkIDAnnotation: "not-a-number"}
				Expect(clusterUDNVRFName(cudn, []netv1.NetworkAttachmentDefinition{nadWithMalformedID}, nil, true)).To(Equal("mp7-udn-vrf"),
					"a malformed network-id annotation must be treated as unknown, not published")
				nadWithOutOfRangeID := nadWithoutID
				nadWithOutOfRangeID.Annotations = map[string]string{ovntypes.OvnNetworkIDAnnotation: fmt.Sprintf("%d", ovntypes.MaxNetworks)}
				Expect(clusterUDNVRFName(cudn, []netv1.NetworkAttachmentDefinition{nadWithOutOfRangeID}, nil, true)).To(Equal("mp7-udn-vrf"),
					"an annotation beyond the allocator's range must be treated as unknown, not published")
				Expect(clusterUDNVRFName(cudn, nil, errors.New("sync failed"), true)).To(Equal("mp7-udn-vrf"),
					"a failed sync must not clear the published name while NADs remain")
				Expect(clusterUDNVRFName(cudn, nil, errors.New("sync failed"), false)).To(BeEmpty(),
					"a sync that failed after deleting the last NAD must clear the name, since the released network ID may be reallocated")
				Expect(clusterUDNVRFName(cudn, nil, nil, false)).To(BeEmpty(),
					"the name must clear once a successful sync leaves no NADs, since the released network ID may be reallocated")

				Expect(len(util.GetCUDNVRFName(cudn.Name, ovntypes.MaxNetworks-1))).To(BeNumerically("<=", ovntypes.MaxInterfaceNameLength),
					"the highest allocatable network ID must derive a name within the device name length limit")

				secondaryCUDN := testLayer2SecondaryClusterUDN("tenant-with-a-very-long-name")
				Expect(clusterUDNVRFName(secondaryCUDN, []netv1.NetworkAttachmentDefinition{nadWithID}, nil, true)).To(BeEmpty(),
					"the VRF name is published only for primary networks")
			})

			It("should not publish a VRF name for a secondary CUDN", func() {
				nsName := "red"
				cudn := testLayer2SecondaryClusterUDN("tenant-red", nsName)

				c = newTestController(template.RenderNetAttachDefManifest, testNamespace(nsName), cudn)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [red]",
				}}))
				Expect(cudn.Status.VRFName).To(BeNil(), "the VRF name is published only for Uplink-attached CUDNs")
			})

			It("should allocate VID for EVPN network NAD", func() {
				testNs := testNamespace("evpn-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-test]",
				}}))

				// Verify VID was allocated in the NAD config
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should be allocated for EVPN MAC-VRF (first available after 0,1 reserved)")
				}).Should(Succeed())
			})

			It("should allocate VID for EVPN network NAD with IP-VRF only", func() {
				testNs := testNamespace("evpn-ipvrf-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNIPVRFClusterUDN("evpn-ipvrf-cudn", vtep.Name, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-ipvrf-test]",
				}}))

				// Verify VID was allocated in the NAD config (IP-VRF only, no MAC-VRF)
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, ipVID := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(0), "MAC-VRF should not be present for IP-VRF only config")
					g.Expect(ipVID).To(Equal(2), "VID should be allocated for EVPN IP-VRF only (first available after 0,1 reserved)")
				}).Should(Succeed())
			})

			It("should allocate separate VIDs for EVPN network with both MAC-VRF and IP-VRF (symmetric IRB)", func() {
				testNs := testNamespace("evpn-irb-test")
				vtep := testVTEP("vtep-test")
				cudn := testSymmetricIRBClusterUDN("evpn-irb-cudn", vtep.Name, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					var err error
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-irb-test]",
				}}))

				// Verify both VIDs were allocated with different values
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, ipVID := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "MAC-VRF should get VID 2 (first available)")
					g.Expect(ipVID).To(Equal(3), "IP-VRF should get VID 3")
				}).Should(Succeed())
			})

			It("should allocate different VIDs for multiple EVPN networks", func() {
				testNs := testNamespace("evpn-multi-test")
				vtep := testVTEP("vtep-test")
				cudn1 := testEVPNClusterUDN("evpn-cudn-1", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				cudn2 := testEVPNClusterUDN("evpn-cudn-2", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 300}}, testNs.Name)
				cudn2.UID = "2" // Different UID for second CUDN

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn1, cudn2, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for both NADs to be created and have VIDs, and verify they are different
				Eventually(func(g Gomega) {
					nad1, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), "evpn-cudn-1", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					nad2, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), "evpn-cudn-2", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					vid1, _ := evpnVIDsFromNAD(nad1)
					vid2, _ := evpnVIDsFromNAD(nad2)
					g.Expect(vid1).To(BeNumerically(">", 0), "NAD 1 should have VID allocated")
					g.Expect(vid2).To(BeNumerically(">", 0), "NAD 2 should have VID allocated")
					// VIDs should be different from each other
					// Note: Order is non-deterministic due to concurrent CUDN processing
					g.Expect(vid1).NotTo(Equal(vid2), "VIDs should be different for different networks")
				}).Should(Succeed())
			})

			It("should reject EVPN network with duplicate VNI on the same VTEP", func() {
				testNs := testNamespace("evpn-vni-conflict-test")
				vtep := testVTEP("vtep-test")
				cudn1 := testEVPNClusterUDN("evpn-vni-1", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				cudn2 := testEVPNClusterUDN("evpn-vni-2", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				cudn2.UID = "2"

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn1, cudn2, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				Eventually(func(g Gomega) {
					cudn1Updated, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-vni-1", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					cudn2Updated, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "evpn-vni-2", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())

					conditionsHaveVNIConflict := func(conditions []metav1.Condition) bool {
						for _, c := range conditions {
							if c.Type == "NetworkCreated" && c.Status == "False" && strings.Contains(c.Message, "VNI") {
								return true
							}
						}
						return false
					}
					cudn1Conflict := conditionsHaveVNIConflict(cudn1Updated.Status.Conditions)
					cudn2Conflict := conditionsHaveVNIConflict(cudn2Updated.Status.Conditions)
					g.Expect(cudn1Conflict || cudn2Conflict).To(BeTrue(), "one CUDN should report VNI conflict")
				}).Should(Succeed())
			})

			It("should allow same VNI on different VTEPs", func() {
				testNs := testNamespace("evpn-diff-vtep-test")
				vtep1 := testVTEP("vtep-1")
				vtep2 := testVTEP("vtep-2")
				cudn1 := testEVPNClusterUDN("evpn-vtep1-net", &udnv1.EVPNConfig{VTEP: vtep1.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				cudn2 := testEVPNClusterUDN("evpn-vtep2-net", &udnv1.EVPNConfig{VTEP: vtep2.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				cudn2.UID = "2"

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn1, cudn2, testNs, vtep1, vtep2)
				Expect(c.Run()).To(Succeed())

				// Both NADs should be created successfully
				Eventually(func(g Gomega) {
					nad1, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), "evpn-vtep1-net", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					nad2, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), "evpn-vtep2-net", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					vid1, _ := evpnVIDsFromNAD(nad1)
					vid2, _ := evpnVIDsFromNAD(nad2)
					g.Expect(vid1).To(BeNumerically(">", 0))
					g.Expect(vid2).To(BeNumerically(">", 0))
				}).Should(Succeed())
			})

			It("should release VID when EVPN CUDN is deleted", func() {
				testNs := testNamespace("evpn-delete-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-delete-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for CUDN to be processed and NAD created with VID
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "First CUDN should get VID 2 (first available)")
				}).Should(Succeed())

				// Verify VID is allocated in the controller's allocator
				Expect(c.vidAllocator.GetID("evpn-delete-cudn/macvrf")).To(BeNumerically(">=", 0), "VID should be allocated")

				// Trigger deletion by setting DeletionTimestamp and processing
				markCUDNForDeletion(cudn.Name)

				// Wait for finalizer to be removed (indicating deletion was processed)
				Eventually(func(g Gomega) {
					updatedCUDN, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(updatedCUDN.Finalizers).To(BeEmpty(), "Finalizer should be removed after deletion")
					// Verify VID is released from the allocator
					g.Expect(c.vidAllocator.GetID("evpn-delete-cudn/macvrf")).To(Equal(-1), "VID should be released after deletion")
				}).Should(Succeed())
			})

			It("should release both MAC-VRF and IP-VRF VIDs when symmetric IRB CUDN is deleted", func() {
				testNs := testNamespace("evpn-irb-delete-test")
				vtep := testVTEP("vtep-irb-delete")
				cudn := testSymmetricIRBClusterUDN("evpn-irb-delete", vtep.Name, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for CUDN to be processed and NAD created with both VIDs
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, ipVID := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "MAC-VRF VID should be allocated (first available)")
					g.Expect(ipVID).To(Equal(3), "IP-VRF VID should be allocated")
				}).Should(Succeed())

				// Verify both VIDs are allocated in the controller's allocator
				Expect(c.vidAllocator.GetID("evpn-irb-delete/macvrf")).To(Equal(2), "MAC-VRF VID should be allocated (first available)")
				Expect(c.vidAllocator.GetID("evpn-irb-delete/ipvrf")).To(Equal(3), "IP-VRF VID should be allocated")

				// Trigger deletion
				markCUDNForDeletion(cudn.Name)

				// Wait for finalizer to be removed and verify both VIDs are released
				Eventually(func(g Gomega) {
					updatedCUDN, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(updatedCUDN.Finalizers).To(BeEmpty(), "Finalizer should be removed after deletion")
					// Verify both VIDs are released from the allocator
					g.Expect(c.vidAllocator.GetID("evpn-irb-delete/macvrf")).To(Equal(-1), "MAC-VRF VID should be released after deletion")
					g.Expect(c.vidAllocator.GetID("evpn-irb-delete/ipvrf")).To(Equal(-1), "IP-VRF VID should be released after deletion")
				}).Should(Succeed())
			})

			It("should preserve allocated VID when EVPN CUDN is updated", func() {
				testNs := testNamespace("evpn-update-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-update-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for initial VID allocation
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should be allocated (first available)")
				}).Should(Succeed())

				// Update CUDN (trigger reconciliation)
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				cudn.Annotations = map[string]string{"updated": "true"}
				_, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Ensure VID remains the same after reconciliation
				Consistently(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should remain consistent after CUDN update")
				}, 500*time.Millisecond, 50*time.Millisecond).Should(Succeed())
			})

			It("should continue startup and allocate new VID when all NADs are corrupted", func() {
				// VID recovery failures no longer block startup to prevent DoS attacks
				// via malicious NADs. Instead, the CUDN is enqueued for reconciliation
				// and a new VID is allocated.
				testNs := testNamespace("evpn-all-corrupted-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-all-corrupted", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				// Create a corrupted NAD owned by the CUDN - NetworkManager will fail to parse it
				corruptedNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 100}})
				corruptedNAD.Spec.Config = `{"transport":"evpn", invalid json - corrupted`

				// Use started NetworkManager - it will fail to parse the corrupted NAD
				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep, corruptedNAD)

				// Controller should start successfully (VID recovery failure logged but not fatal)
				Expect(c.Run()).To(Succeed())

				// The CUDN is enqueued for reconciliation and gets a new VID
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "Should allocate new VID since recovery failed (first available)")
				}).Should(Succeed())
			})

			It("should continue startup and allocate new VID when VID recovery encounters a conflict", func() {
				// VID conflicts during recovery no longer block startup.
				// Instead, the CUDN is enqueued for reconciliation and gets a new VID.
				testNs := testNamespace("evpn-vid-conflict-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-conflict", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				// Create a NAD with VID 5 for MAC-VRF
				existingNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 5}})

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep, existingNAD)

				// Pre-reserve VID 5 for a DIFFERENT key to create a conflict during recovery
				Expect(c.vidAllocator.ReserveID("conflicting-network/macvrf", 5)).To(Succeed())

				// Controller should start successfully despite the conflict
				Expect(c.Run()).To(Succeed())

				// Recovery fails due to conflict, CUDN is enqueued for reconciliation and gets a new VID
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "Should allocate new VID since 5 is taken by another network (first available)")
				}).Should(Succeed())
			})

			It("should continue startup and preserve MAC-VRF VID when only IP-VRF VID recovery encounters a conflict", func() {
				// When IP-VRF VID conflicts but MAC-VRF VID is available:
				// - MAC-VRF recovery succeeds (VID reserved in allocator)
				// - IP-VRF recovery fails (conflict)
				// - CUDN is enqueued for reconciliation
				// - MAC-VRF VID is preserved (already in allocator), IP-VRF gets new VID
				testNs := testNamespace("evpn-ipvrf-conflict-test")
				vtep := testVTEP("vtep-test")
				cudn := testSymmetricIRBClusterUDN("evpn-ipvrf-conflict", vtep.Name, testNs.Name)

				// Create a symmetric IRB NAD with both MAC-VRF (VID 3) and IP-VRF (VID 7)
				existingNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{
					VTEP:   vtep.Name,
					MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 3},
					IPVRF:  &ovncnitypes.VRFConfig{VNI: 200, VID: 7},
				})

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep, existingNAD)

				// Pre-reserve VID 7 for IP-VRF of a DIFFERENT network to create a conflict
				Expect(c.vidAllocator.ReserveID("other-network/ipvrf", 7)).To(Succeed())

				// Controller should start successfully
				Expect(c.Run()).To(Succeed())

				// MAC-VRF VID 3 was successfully reserved during recovery.
				// IP-VRF VID 7 conflicted, so during reconciliation it gets new VID 2 (first available).
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, ipVID := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(3), "MAC-VRF VID should be preserved (recovery succeeded)")
					g.Expect(ipVID).To(Equal(2), "IP-VRF gets new VID (first available, 0,1 reserved, 7 is taken)")
				}).Should(Succeed())
			})

			It("should continue startup and preserve IP-VRF VID when only MAC-VRF VID recovery encounters a conflict", func() {
				// When MAC-VRF VID conflicts but IP-VRF VID is available:
				// - MAC-VRF recovery fails (conflict)
				// - IP-VRF recovery succeeds (VID reserved in allocator)
				// - CUDN is enqueued for reconciliation
				// - MAC-VRF gets new VID, IP-VRF VID is preserved
				testNs := testNamespace("evpn-macvrf-conflict-test")
				vtep := testVTEP("vtep-test")
				cudn := testSymmetricIRBClusterUDN("evpn-macvrf-conflict", vtep.Name, testNs.Name)

				// Create a symmetric IRB NAD with both MAC-VRF (VID 3) and IP-VRF (VID 7)
				existingNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{
					VTEP:   vtep.Name,
					MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 3},
					IPVRF:  &ovncnitypes.VRFConfig{VNI: 200, VID: 7},
				})

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep, existingNAD)

				// Pre-reserve VID 3 for a DIFFERENT network to create a conflict during recovery
				Expect(c.vidAllocator.ReserveID("other-network/macvrf", 3)).To(Succeed())

				// Controller should start successfully
				Expect(c.Run()).To(Succeed())

				// IP-VRF VID 7 was successfully reserved during recovery.
				// MAC-VRF VID 3 conflicted, so during reconciliation it gets new VID 2 (first available).
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, ipVID := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "MAC-VRF gets new VID (first available, 0,1 reserved, 3 is already taken)")
					g.Expect(ipVID).To(Equal(7), "IP-VRF VID should be preserved (recovery succeeded)")
				}).Should(Succeed())
			})

			It("should not fail startup when CUDN exists but has no NADs yet", func() {
				vtep := testVTEP("vtep-test")
				// Create a CUDN without any NADs (namespace doesn't match selector)
				cudnWithNoNADs := testEVPNClusterUDN("evpn-no-nads", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, "nonexistent-ns")

				c = newTestControllerWithNetworkManager(renderNadStub(nil), cudnWithNoNADs, vtep)

				Expect(c.Run()).To(Succeed(), "Controller should start even when CUDN has no NADs")

				// No VID should be allocated since there are no NADs
				Expect(c.vidAllocator.GetID("evpn-no-nads/macvrf")).To(Equal(-1), "No VID should be allocated for CUDN without NADs")
			})

			It("should recover VIDs from NetworkManager cache at startup", func() {
				// This tests the production startup recovery path where:
				// 1. NetworkManager is started and processes existing NADs
				// 2. UDN controller starts and recovers VIDs from NetworkManager's cache
				testNs := testNamespace("evpn-nm-recovery-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-nm-recovery", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				// Create an existing NAD with VID 42 (simulating a previous controller run)
				existingNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 42}})

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep, existingNAD)
				Expect(c.Run()).To(Succeed())

				// VID should be recovered from NetworkManager cache at startup
				Eventually(func() int {
					return c.vidAllocator.GetID("evpn-nm-recovery/macvrf")
				}).Should(Equal(42), "VID 42 should be recovered from NetworkManager cache at startup")
			})

			It("should recover VIDs in deterministic order based on CUDN creation timestamp", func() {
				// When two CUDNs have NADs claiming the same VID, the older CUDN wins.
				// This ensures deterministic behavior across restarts.
				testNs1 := testNamespace("evpn-order-test-1")
				testNs2 := testNamespace("evpn-order-test-2")
				vtep := testVTEP("vtep-test")

				// Create two CUDNs with different creation timestamps and unique UIDs
				olderCUDN := testEVPNClusterUDN("aaa-older-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs1.Name)
				olderCUDN.UID = "older-uid-1"
				olderCUDN.CreationTimestamp = metav1.NewTime(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

				newerCUDN := testEVPNClusterUDN("zzz-newer-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 300}}, testNs2.Name)
				newerCUDN.UID = "newer-uid-2"
				newerCUDN.CreationTimestamp = metav1.NewTime(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))

				// Both NADs claim VID 42 - this simulates a conflict scenario
				olderNAD := testEVPNClusterUdnNADOwnedByCUDN(olderCUDN, testNs1.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 42}})
				newerNAD := testEVPNClusterUdnNADOwnedByCUDN(newerCUDN, testNs2.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 300, VID: 42}})

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest,
					olderCUDN, newerCUDN, testNs1, testNs2, vtep, olderNAD, newerNAD)
				Expect(c.Run()).To(Succeed())

				// The older CUDN should win the VID 42, regardless of alphabetical name order
				// (newerCUDN has name "zzz-newer-cudn" which comes after "aaa-older-cudn" alphabetically,
				// but olderCUDN should still win because it was created first)
				Eventually(func() int {
					return c.vidAllocator.GetID("aaa-older-cudn/macvrf")
				}).Should(Equal(42), "Older CUDN should keep VID 42")

				// The newer CUDN loses the conflict and gets a new VID during reconciliation
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs2.Name).Get(context.Background(), newerCUDN.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "Newer CUDN should get new VID (first available) since older CUDN won VID 42")
				}).Should(Succeed())
			})

			It("should return error when VID pool is exhausted", func() {
				testNs := testNamespace("evpn-exhaustion-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-exhaust-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)

				// Exhaust all available VIDs (2-4094) before starting the controller (0,1 already reserved)
				for i := 2; i < MaxEVPNVIDs; i++ {
					err := c.vidAllocator.ReserveID(fmt.Sprintf("exhaust-key-%d", i), i)
					Expect(err).NotTo(HaveOccurred(), "should allocate VID %d", i)
				}

				// Now start the controller - the EVPN CUDN should fail to get a VID
				Expect(c.Run()).To(Succeed())

				// Verify the pool is exhausted
				_, err := c.vidAllocator.AllocateID("one-more-key")
				Expect(err).To(HaveOccurred(), "VID pool should be exhausted")

				// The CUDN should report a sync error because VID allocation failed
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "NetworkAttachmentDefinitionSyncError",
					Message: "failed to allocate EVPN IDs: failed to allocate VID for MAC-VRF: failed to allocate the id for the resource evpn-exhaust-cudn/macvrf",
				}}), "should report VID allocation failure in status")

				// Verify NAD was not created
				_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "NAD should not be created when VID allocation fails")
			})

			It("should allocate VID after pool is freed up", func() {
				testNs := testNamespace("evpn-free-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-free-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)

				// Exhaust all VIDs except one (starting from 2, since 0,1 already reserved)
				for i := 2; i < MaxEVPNVIDs-1; i++ {
					err := c.vidAllocator.ReserveID(fmt.Sprintf("exhaust-key-%d", i), i)
					Expect(err).NotTo(HaveOccurred())
				}

				// Start controller - it should successfully allocate the last available VID
				Expect(c.Run()).To(Succeed())

				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-free-test]",
				}}), "should successfully create network with last available VID")

				// Verify the VID was allocated
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(MaxEVPNVIDs-1), "should get the last available VID")
				}).Should(Succeed())
			})

			It("should fail to start if VID 0 is already reserved by another resource", func() {
				// This tests the defensive check that VID 0 (reserved per IEEE 802.1Q)
				// must be reservable during controller initialization.
				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest)

				// Reserve VID 0 with a DIFFERENT key (simulating corruption/bug)
				Expect(c.vidAllocator.ReserveID("some-other-key", 0)).To(Succeed())

				// Run should fail because initializeController can't reserve VID 0
				err := c.Run()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("failed to reserve VID 0"))
			})

			It("should allocate new VID when namespace and NAD are created at runtime", func() {
				// Scenario: Allocator has no VID for this key, namespace/NAD created at runtime
				// This can happen when:
				// - CUDN exists but had no matching namespaces at startup (no NADs to recover)
				// - Admin later creates a namespace
				// - Controller reconciles and allocates a new VID
				//
				// 1. Controller starts with CUDN but NO matching namespaces (no NADs created)
				// 2. Allocator has NO VID for this key after startup
				// 3. Namespace is created at runtime
				// 4. Controller reconciles and allocates VID 2 (first available, 0,1 reserved)
				vtep := testVTEP("vtep-test")

				// Namespace that doesn't exist at startup
				const runtimeNsName = "runtime-ns-test"

				// CUDN with selector matching a namespace that doesn't exist yet
				cudn := testEVPNClusterUDN("evpn-runtime-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, runtimeNsName)

				// Start controller - no NADs to recover, allocator empty for this key
				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, vtep)
				Expect(c.Run()).To(Succeed())

				// Create namespace at runtime (NAD will be created by controller)
				testNs := testNamespace(runtimeNsName)
				_, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), testNs, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Controller reconciles and allocates VID 2 (first available, 0,1 reserved)
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should be allocated (first available, 0,1 reserved)")
				}).Should(Succeed())
			})

			It("should allocate new VID when existing NAD has VID taken by another CUDN", func() {
				// Scenario: Allocator has no VID for this key, but NAD's VID is taken by another CUDN
				// This can happen when:
				// - CUDN-A had no matching namespaces at startup
				// - CUDN-B had a NAD with VID 42 that was recovered
				// - Someone manually creates NAD for CUDN-A with VID 42 (collision)
				//
				// 1. Controller starts with CUDN but NO matching namespaces
				// 2. VID 42 is already reserved by a different CUDN
				// 3. Namespace and NAD with VID 42 are created at runtime
				// 4. Controller reconciles
				// 5. VID 42 can't be reserved (taken) -> new VID allocated
				vtep := testVTEP("vtep-test")

				// Namespace that doesn't exist at startup
				const runtimeNsName = "runtime-conflict-test"

				cudn := testEVPNClusterUDN("evpn-runtime-conflict", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, runtimeNsName)

				// Start controller - no NADs to recover, allocator empty for this key
				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, vtep)

				// VID 42 is already reserved by another CUDN (simulates collision)
				Expect(c.vidAllocator.ReserveID("another-cudn/macvrf", 42)).To(Succeed())

				Expect(c.Run()).To(Succeed())

				// Create namespace and NAD with VID 42 at runtime (collision with another CUDN)
				testNs := testNamespace(runtimeNsName)
				runtimeNAD := testEVPNClusterUdnNADOwnedByCUDN(cudn, testNs.Name, &ovncnitypes.EVPNConfig{VTEP: vtep.Name, MACVRF: &ovncnitypes.VRFConfig{VNI: 100, VID: 42}})

				_, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), testNs, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Create(context.Background(), runtimeNAD, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Controller reconciles - VID 42 is taken, must allocate new VID
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should be newly allocated since 42 is taken by another CUDN (first available)")
				}).Should(Succeed())
			})

			It("should revert manual NAD VID change when allocator already has VID for this key", func() {
				// This tests the case where:
				// - Allocator has VID 2 for this key (from initial NAD creation, first available)
				// - Someone manually changes NAD to VID 42
				// - Allocator's VID 2 should win, NAD reverted to 2
				// Note: Whether VID 42 is free or taken doesn't matter - the allocator's
				// existing VID takes precedence because ReserveID fails when key already has a VID.
				testNs := testNamespace("evpn-vid-manual-change-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-manual-change-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for initial NAD creation (will get VID 2, first available)
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "Initial VID should be 2 (first available)")
				}).Should(Succeed())

				// Now manually update the NAD with VID 42
				nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(setNADEVPNVIDs(nad, 42, 0)).To(Succeed())
				_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Update(context.Background(), nad, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// The NAD update triggers reconciliation. The allocator already has VID 2
				// for this key, so NAD is reverted to 2.
				Eventually(func(g Gomega) {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					macVID, _ := evpnVIDsFromNAD(nad)
					g.Expect(macVID).To(Equal(2), "VID should be reverted to allocator's VID")
				}).Should(Succeed())
			})

			It("should report VTEPNotFound when EVPN CUDN references non-existent VTEP", func() {
				testNs := testNamespace("evpn-vtep-missing-test")
				cudn := testEVPNClusterUDN("evpn-vtep-missing", &udnv1.EVPNConfig{VTEP: "default", MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs)
				Expect(c.Run()).To(Succeed())

				// CUDN should report VTEPNotFound status
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  ReasonVTEPNotFound,
					Message: "Cannot create network: VTEP 'default' does not exist. Create the VTEP CR first or update the CUDN to reference an existing VTEP.",
				}}), "should report VTEPNotFound in status")

				// TransportAccepted should also report VTEPNotFound
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonVTEPNotFound,
					`VTEP "default" referenced by EVPN configuration does not exist.`)

				// NAD should not be created when VTEP is missing
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "NAD should not be created when VTEP is missing")
			})

			It("should create NAD when VTEP exists for EVPN CUDN", func() {
				testNs := testNamespace("evpn-vtep-exists-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-vtep-exists", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// CUDN should succeed when VTEP exists
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-vtep-exists-test]",
				}}), "should succeed when VTEP exists")

				// NAD should be created
				Eventually(func() error {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					return err
				}).Should(Succeed(), "NAD should be created when VTEP exists")

				// TransportAccepted should report RA missing (VTEP passes but no RA)
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonEVPNRouteAdvertisementsIsMissing,
					"No RouteAdvertisements CR is advertising the pod networks.")
			})

			It("should automatically reconcile CUDN when VTEP is created after CUDN", func() {
				testNs := testNamespace("evpn-vtep-transition-test")
				vtepName := "default"
				cudn := testEVPNClusterUDN("evpn-vtep-transition", &udnv1.EVPNConfig{VTEP: vtepName, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				// Start controller WITHOUT the VTEP - CUDN references non-existent VTEP
				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs)
				Expect(c.Run()).To(Succeed())

				// Step 1: CUDN should initially report VTEPNotFound
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  ReasonVTEPNotFound,
					Message: "Cannot create network: VTEP '" + vtepName + "' does not exist. Create the VTEP CR first or update the CUDN to reference an existing VTEP.",
				}}), "should initially report VTEPNotFound")

				// TransportAccepted should also report VTEPNotFound
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonVTEPNotFound,
					fmt.Sprintf("VTEP %q referenced by EVPN configuration does not exist.", vtepName))

				// NAD should NOT exist yet
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "NAD should not be created when VTEP is missing")

				// Step 2: Create the VTEP dynamically - this should trigger VTEPNotifier
				vtep := testVTEP(vtepName)
				_, err = cs.VTEPClient.K8sV1().VTEPs().Create(context.Background(), vtep, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Step 3: CUDN should be automatically reconciled and succeed
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-vtep-transition-test]",
				}}), "should succeed after VTEP is created")

				// TransportAccepted should transition to RA missing (VTEP now passes but no RA)
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonEVPNRouteAdvertisementsIsMissing,
					"No RouteAdvertisements CR is advertising the pod networks.")

				// NAD should now be created
				Eventually(func() error {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					return err
				}).Should(Succeed(), "NAD should be created after VTEP is created")
			})

			It("should only re-queue EVPN CUDNs when VTEP changes, not non-EVPN CUDNs", func() {
				testNs := testNamespace("vtep-filter-test")
				vtep := testVTEP("vtep-filter")

				// Create a non-EVPN CUDN (Layer2 without EVPN transport)
				nonEvpnCUDN := testClusterUDN("non-evpn-cudn", testNs.Name)
				nonEvpnCUDN.UID = "non-evpn-uid"

				// Create an EVPN CUDN that references the VTEP
				evpnCUDN := testEVPNClusterUDN("evpn-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
				evpnCUDN.UID = "evpn-uid"

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, nonEvpnCUDN, evpnCUDN, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Wait for EVPN NAD to be created
				Eventually(func() error {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), evpnCUDN.Name, metav1.GetOptions{})
					return err
				}).Should(Succeed())

				// ReconcileVTEP should iterate over all CUDNs but only match the EVPN one
				// This covers the non-EVPN path in cudnReferencesVTEP
				err := c.ReconcileVTEP(vtep.Name)
				Expect(err).NotTo(HaveOccurred())
			})

			It("should report VTEPNotFound when VTEP is deleted after CUDN creation", func() {
				testNs := testNamespace("evpn-vtep-delete-test")
				vtep := testVTEP("vtep-to-delete")
				cudn := testEVPNClusterUDN("evpn-vtep-delete", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// Step 1: Verify NAD is created successfully when VTEP exists
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [evpn-vtep-delete-test]",
				}}), "should initially succeed when VTEP exists")

				Eventually(func() error {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					return err
				}).Should(Succeed(), "NAD should be created when VTEP exists")

				// TransportAccepted should report RA missing (VTEP passes but no RA)
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonEVPNRouteAdvertisementsIsMissing,
					"No RouteAdvertisements CR is advertising the pod networks.")

				// Step 2: Delete the VTEP - this should trigger VTEPNotifier
				err := cs.VTEPClient.K8sV1().VTEPs().Delete(context.Background(), vtep.Name, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Step 3: CUDN should be re-reconciled and report VTEPNotFound
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  ReasonVTEPNotFound,
					Message: "Cannot create network: VTEP '" + vtep.Name + "' does not exist. Create the VTEP CR first or update the CUDN to reference an existing VTEP.",
				}}), "should report VTEPNotFound after VTEP is deleted")

				// TransportAccepted should also report VTEPNotFound
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonVTEPNotFound,
					fmt.Sprintf("VTEP %q referenced by EVPN configuration does not exist.", vtep.Name))
			})

			// Integration tests for VTEP Accepted gating (VTEP not accepted blocks
			// NAD creation, VTEP transition to accepted creates NAD, VTEP becomes
			// not-accepted after NAD exists freezes NADs) are in
			// clustermanager_test.go where both the VTEP and UDN controllers run.

			It("should fail when EVPN transport is requested but EVPN feature is disabled", func() {
				// Disable EVPN feature flag for this test.
				// No defer needed - BeforeEach resets config via PrepareTestConfig().
				config.OVNKubernetesFeature.EnableEVPN = false

				testNs := testNamespace("evpn-disabled-test")
				vtep := testVTEP("vtep-test")
				cudn := testEVPNClusterUDN("evpn-disabled-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

				c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
				Expect(c.Run()).To(Succeed())

				// CUDN should report error with message about EVPN flag
				Eventually(func() []metav1.Condition {
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  ReasonEVPNConfigError,
					Message: "EVPN transport requested but EVPN feature is not enabled",
				}}), "should report error when EVPN flag is disabled")

				// TransportAccepted should also report the config error
				expectTransportCondition(cudn.Name, metav1.ConditionFalse, ReasonEVPNConfigError,
					"EVPN transport requested but EVPN feature is not enabled")

				// NAD should not be created when EVPN is disabled
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "NAD should not be created when EVPN is disabled")
			})

			It("should update CUDN annotations and preserve NAD-local annotations", func() {
				testNamespaces := []string{"red", "blue"}
				var objs []runtime.Object
				for _, nsName := range testNamespaces {
					objs = append(objs, testNamespace(nsName))
				}
				cudn := testClusterUDN("test", testNamespaces...)
				cudn.Spec.Network = udnv1.NetworkSpec{Topology: udnv1.NetworkTopologyLayer2, Layer2: &udnv1.Layer2Config{
					Subnets: udnv1.DualStackCIDRs{"10.10.10.0/24"},
				}}
				cudn.Annotations = map[string]string{"foo": "bar"}

				objs = append(objs, cudn)
				networkName := ovntypes.CUDNPrefix + cudn.Name
				expectedNsNADs := map[string]*netv1.NetworkAttachmentDefinition{}
				for _, nsName := range testNamespaces {
					nad := testClusterUdnNAD(cudn.Name, nsName)
					nadName := nsName + "/" + cudn.Name
					nad.Spec.Config = `{"cniVersion":"1.1.0","name":"` + networkName + `","netAttachDefName":"` + nadName + `","role":"","subnets":"10.10.10.0/24","topology":"layer2","type":"ovn-k8s-cni-overlay"}`
					nad.Annotations = map[string]string{
						"foo":                             "bar",
						ovntypes.OvnNetworkNameAnnotation: networkName,
						ovntypes.OvnNetworkIDAnnotation:   "6",
					}
					expectedNsNADs[nsName] = nad.DeepCopy()
					objs = append(objs, nad)
				}

				c = newTestController(template.RenderNetAttachDefManifest, objs...)
				Expect(c.Run()).To(Succeed())

				By("adding distinct annotations to the CUDN-owned NADs")
				for _, nsName := range testNamespaces {
					nad, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					nad.Annotations["example.com/partition-name"] = nsName + "-partition"
					nad.Annotations["foo2"] = "nad-local"
					_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().
						NetworkAttachmentDefinitions(nsName).Update(context.Background(), nad, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					Eventually(func(g Gomega) {
						cachedNAD, err := c.nadLister.NetworkAttachmentDefinitions(nsName).Get(cudn.Name)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(cachedNAD.Annotations["example.com/partition-name"]).To(Equal(nsName + "-partition"))
					}, 2*time.Second).Should(Succeed())
				}

				By("updating CUDN with a new annotation")
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				updatedCUDN := cudn.DeepCopy()
				updatedCUDN.Annotations = map[string]string{"foo": "bar", "foo2": "bar2"}
				_, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), updatedCUDN, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				for testNamespace, expectedNAD := range expectedNsNADs {
					expectedNAD.Annotations = map[string]string{
						"foo":                             "bar",
						"foo2":                            "bar2",
						"example.com/partition-name":      testNamespace + "-partition",
						ovntypes.OvnNetworkNameAnnotation: networkName,
						ovntypes.OvnNetworkIDAnnotation:   "6",
					}

					Eventually(func(g Gomega) {
						actualNAD, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().
							NetworkAttachmentDefinitions(testNamespace).
							Get(context.Background(), cudn.Name, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(actualNAD.Annotations).To(Equal(expectedNAD.Annotations), "NAD should have updated annotations and preserve local annotations")
					}).Should(Succeed())
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
					for _, nsName := range connectedNsNames {
						ns := testNamespace(nsName)
						ns.Labels[testLabelKey] = testLabelValue
						testObjs = append(testObjs, ns)
					}
					By("create CUDN selecting namespaces with test label")
					cudn := testClusterUDN(cudnName)
					cudn.Spec = udnv1.ClusterUserDefinedNetworkSpec{NamespaceSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key:      testLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{testLabelValue},
					}}},
						Network: udnv1.NetworkSpec{
							Topology: udnv1.NetworkTopologyLayer3,
							Layer3: &udnv1.Layer3Config{
								Role: udnv1.NetworkRolePrimary,
							},
						}}
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
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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
					newNsNames := []string{"black", "gray"}
					for _, nsName := range newNsNames {
						ns := testNamespace(nsName)
						ns.Labels[testLabelKey] = newNsLabelValue
						_, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					By("add new label to CR namespace-selector")
					cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					cudn.Spec.NamespaceSelector.MatchExpressions[0].Values = append(cudn.Spec.NamespaceSelector.MatchExpressions[0].Values, newNsLabelValue)
					cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					Expect(cudn.Spec.NamespaceSelector.MatchExpressions).To(Equal([]metav1.LabelSelectorRequirement{{
						Key:      testLabelKey,
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{testLabelValue, newNsLabelValue},
					}}))

					Eventually(func() []metav1.Condition {
						var err error
						cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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
					Expect(err).NotTo(HaveOccurred())
					Expect(cudn.Spec.NamespaceSelector.MatchExpressions).To(Equal([]metav1.LabelSelectorRequirement{{
						Key: testLabelKey, Operator: metav1.LabelSelectorOpIn, Values: []string{""},
					}}))

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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
							Name:        "pod-0",
							Namespace:   nsName,
							Annotations: map[string]string{ovntypes.OvnPodAnnotationName: `{"default": {"role":"primary"}, "` + nsName + `/` + cudnName + `": {"role": "secondary"}}`}},
						}
						pod, err := cs.KubeClient.CoreV1().Pods(nsName).Create(context.Background(), pod, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
						testPods = append(testPods, *pod)
					}

					// deleteNAD checks pods through the informer cache; make sure
					// the pods are visible there before triggering the deletion.
					// This doesn't fix the potential race where pod is created right after the CUDN is deleted
					// or when pod informer cache lags, but that is also not the point of this test.
					// Since we can only do best-effort check for the pods, we need to make sure informer cache
					// is synced before we delete the CUDN.
					Eventually(func(g Gomega) {
						for _, pod := range testPods {
							_, err := c.podInformer.Lister().Pods(pod.Namespace).Get(pod.Name)
							g.Expect(err).NotTo(HaveOccurred())
						}
					}, 2*time.Second).Should(Succeed())

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
						ns.Labels[testLabelKey] = testLabelValue
						_, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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

				It("when new namespace is created without required UDN label, it should not create NAD", func() {
					By("create new namespaces with test label")
					newNsNames := []string{"black", "gray"}
					for _, nsName := range newNsNames {
						ns := invalidTestNamespace(nsName)
						ns.Labels[testLabelKey] = testLabelValue
						_, err := cs.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
						Expect(err).NotTo(HaveOccurred())
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
					}).Should(Or(
						Equal([]metav1.Condition{{
							Type:   "NetworkCreated",
							Status: "False",
							Reason: "NetworkAttachmentDefinitionSyncError",
							Message: "invalid primary network state for namespace \"black\": a valid primary user defined network or network attachment definition " +
								"custom resource, and required namespace label \"k8s.ovn.org/primary-user-defined-network\" must both be present\ninvalid primary " +
								"network state for namespace \"gray\": a valid primary user defined network or network attachment definition custom resource, and " +
								"required namespace label \"k8s.ovn.org/primary-user-defined-network\" must both be present",
						}}),
						Equal([]metav1.Condition{{
							Type:   "NetworkCreated",
							Status: "False",
							Reason: "NetworkAttachmentDefinitionSyncError",
							Message: "invalid primary network state for namespace \"gray\": a valid primary user defined network or network attachment definition " +
								"custom resource, and required namespace label \"k8s.ovn.org/primary-user-defined-network\" must both be present\ninvalid primary " +
								"network state for namespace \"black\": a valid primary user defined network or network attachment definition custom resource, and " +
								"required namespace label \"k8s.ovn.org/primary-user-defined-network\" must both be present",
						}})),
						"status should report NAD failed in existing and new test namespaces")
					for _, nsName := range newNsNames {
						nads, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).List(context.Background(), metav1.ListOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(nads.Items).To(BeEmpty())
					}
				})

				It("when existing namespace is labeled with matching label, should create NAD in newly labeled matching namespaces", func() {
					By("add test label to tests disconnected namespaces")
					for _, nsName := range disconnectedNsNames {
						p := fmt.Sprintf(`[{"op": "add", "path": "./metadata/labels/%s", "value": %q}]`, testLabelKey, testLabelValue)
						ns, err := cs.KubeClient.CoreV1().Namespaces().Patch(context.Background(), nsName, types.JSONPatchType, []byte(p), metav1.PatchOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns.Labels).To(Equal(map[string]string{
							testLabelKey:                       testLabelValue,
							"kubernetes.io/metadata.name":      nsName,
							ovntypes.RequiredUDNNamespaceLabel: "",
						}))
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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
						p := `[{"op": "replace", "path": "./metadata/labels", "value": {}}]`
						ns, err := cs.KubeClient.CoreV1().Namespaces().Patch(context.Background(), nsName, types.JSONPatchType, []byte(p), metav1.PatchOptions{})
						Expect(err).NotTo(HaveOccurred())
						Expect(ns.Labels).To(BeEmpty())
					}

					Eventually(func() []metav1.Condition {
						cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
						Expect(err).NotTo(HaveOccurred())
						return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
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
				for _, nsName := range staleNADsNsNames {
					ns := testNamespace(nsName)
					ns.Labels["test.io"] = "stale"
					testObjs = append(testObjs, ns)
				}
				connectedNsNames := []string{"green", "yellow"}
				connectedLabel := map[string]string{"test.io": "connected"}
				for _, nsName := range connectedNsNames {
					ns := testNamespace(nsName)
					ns.Labels["test.io"] = "connected"
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
					return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
				}, 50*time.Millisecond).Should(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [green, yellow]",
				}}), "status should report NAD created in test labeled namespaces")

				for _, nsName := range staleNADsNsNames {
					_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(nsName).Get(context.Background(), cudn.Name, metav1.GetOptions{})
					Expect(err).To(HaveOccurred())
					Expect(apierrors.IsNotFound(err)).To(BeTrue())
				}
			})
		})
	})

	Context("UserDefinedNetwork object sync", func() {
		It("should fail when NAD owner-reference is malformed", func() {
			udn := testPrimaryUDN()
			mutatedNAD := testNAD()
			mutatedNAD.ObjectMeta.OwnerReferences = []metav1.OwnerReference{{Kind: "DifferentKind"}}
			c := newTestController(noopRenderNadStub(), udn, mutatedNAD, testNamespace("test"))

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).To(Equal(errors.New("foreign NetworkAttachmentDefinition with the desired name already exist [test/test]")))
		})

		It("when UDN is being deleted, should not remove finalizer from non managed NAD", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			unmanagedNAD := testNAD()
			unmanagedNAD.OwnerReferences[0].UID = "99"
			c := newTestController(noopRenderNadStub(), udn, unmanagedNAD, testNamespace("test"))

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
			c := newTestController(noopRenderNadStub(), udn, nad, testNamespace("test"))

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())

			_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
		It("when UDN is being deleted, and NAD exist, should fail when remove NAD finalizer fails", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			c := newTestController(noopRenderNadStub(), udn, nad, testNamespace("test"))

			expectedErr := errors.New("update NAD error")
			cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("update", "network-attachment-definitions", func(testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when UDN is being deleted, and NAD exist w/o finalizer, should remove finalizer from UDN", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			nad.Finalizers = nil
			c := newTestController(noopRenderNadStub(), udn, nad, testNamespace("test"))

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, and NAD not exist, should remove finalizer from UDN", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			c := newTestController(noopRenderNadStub(), udn, testNamespace("test"))

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())
			Expect(udn.Finalizers).To(BeEmpty())
		})
		It("when UDN is being deleted, should fail removing finalizer from UDN when patch fails", func() {
			udn := testsUDNWithDeletionTimestamp(time.Now())
			nad := testNAD()
			nad.Finalizers = nil
			c := newTestController(noopRenderNadStub(), udn, nad, testNamespace("test"))

			expectedErr := errors.New("update UDN error")
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("update", "userdefinednetworks", func(testing.Action) (bool, runtime.Object, error) {
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
					Annotations: map[string]string{ovntypes.OvnPodAnnotationName: `{ 
                          "default": {"role":"primary", "mac_address":"0a:58:0a:f4:02:03"},
						  "test/another-network": {"role": "secondary","mac_address":"0a:58:0a:f4:02:01"} 
                         }`,
					},
				},
			}
			c := newTestController(renderNadStub(nad), udn, nad, pod, testNamespace("test"))

			_, err := c.syncUserDefinedNetwork(udn)
			Expect(err).ToNot(HaveOccurred())

			Expect(udn.Finalizers).To(BeEmpty())

			_, err = cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(udn.Namespace).Get(context.Background(), nad.Name, metav1.GetOptions{})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		DescribeTable("when UDN is being deleted, NAD exist, should not remove finalizers when",
			func(podOvnAnnotations map[string]string, expectedErr error) {
				var objs []runtime.Object
				udn := testsUDNWithDeletionTimestamp(time.Now())
				nad := testNAD()
				for podName, ovnAnnotValue := range podOvnAnnotations {
					objs = append(objs, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
						Name: podName, Namespace: udn.Namespace,
						Annotations: map[string]string{ovntypes.OvnPodAnnotationName: ovnAnnotValue},
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
				udn := testPrimaryUDN()
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
			udn := testPrimaryUDN()
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
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("patch", "userdefinednetworks/status", func(testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedError
			})

			udn := testPrimaryUDN()
			nad := testNAD()
			Expect(c.updateUserDefinedNetworkStatus(udn, nad, nil)).To(MatchError(expectedError))
		})
	})

	Context("ClusterUserDefinedNetwork object sync", func() {
		It("should succeed given no CR", func() {
			c := newTestController(noopRenderNadStub())
			_, err := c.syncClusterUDN(nil, nil)
			Expect(err).To(Not(HaveOccurred()))
		})
		It("should succeed when no namespace match namespace-selector", func() {
			cudn := testClusterUDN("test", "red")
			c := newTestController(noopRenderNadStub(), cudn)

			nads, err := c.syncClusterUDN(cudn, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())
		})
		It("should add finalizer to CR", func() {
			cudn := &udnv1.ClusterUserDefinedNetwork{Spec: udnv1.ClusterUserDefinedNetworkSpec{
				NamespaceSelector: metav1.LabelSelector{}}}
			c := newTestController(noopRenderNadStub(), cudn)

			nads, err := c.syncClusterUDN(cudn, nil)
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

			_, err := c.syncClusterUDN(cudn, nil)
			Expect(err).To(MatchError(expectedErr))
		})

		It("when namespace without pods is being deleted, should delete NAD in that namespace", func() {
			const cudnName = "test-network"
			testNs := testNamespace("blue")
			cudn := testClusterUDN(cudnName, testNs.Name)
			expectedNAD := testClusterUdnNAD(cudnName, testNs.Name)
			c := newTestController(renderNadStub(expectedNAD), cudn, testNs)
			Expect(c.Run()).To(Succeed())

			By("verify NAD is created in namespace")
			Eventually(func() error {
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudnName, metav1.GetOptions{})
				return err
			}).Should(Succeed())

			By("mark namespace as terminating")
			testNs.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			_, err := cs.KubeClient.CoreV1().Namespaces().Update(context.Background(), testNs, metav1.UpdateOptions{})
			Expect(err).ToNot(HaveOccurred())

			By("verify NAD is deleted")
			Eventually(func() bool {
				_, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNs.Name).Get(context.Background(), cudnName, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue(), "NAD should be deleted when namespace is terminating")
		})

		It("when CR is deleted, CR has no finalizer, should succeed", func() {
			deletedCUDN := testClusterUDN("test", "blue")
			deletedCUDN.Finalizers = []string{}
			deletedCUDN.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			c := newTestController(noopRenderNadStub(), deletedCUDN)

			nads, err := c.syncClusterUDN(deletedCUDN, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(nads).To(BeEmpty())
		})
		It("when CR is deleted, should remove finalizer from CR", func() {
			deletedCUDN := testClusterUDN("test", "blue")
			deletedCUDN.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			c := newTestController(noopRenderNadStub(), deletedCUDN)

			nads, err := c.syncClusterUDN(deletedCUDN, nil)
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

				nads, err := c.syncClusterUDN(cudn, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(nads).To(ConsistOf(*expectedNAD))

				By("mark CR for deletion")
				cudn.DeletionTimestamp = &metav1.Time{Time: time.Now()}
				cudn, err = cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Update(context.Background(), cudn, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(cudn.DeletionTimestamp.IsZero()).To(BeFalse())
			})

			It("should delete NAD", func() {
				nads, err := c.syncClusterUDN(cudn, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(nads).To(BeEmpty())
				Expect(cudn.Finalizers).To(BeEmpty())

				nadList, err := cs.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(testNsName).List(context.Background(), metav1.ListOptions{})
				Expect(err).ToNot(HaveOccurred())
				Expect(nadList.Items).To(BeEmpty())
			})
			It("should fail remove NAD finalizer when update NAD fails", func() {
				expectedErr := errors.New("test err")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("update", "network-attachment-definitions", func(testing.Action) (bool, runtime.Object, error) {
					return true, nil, expectedErr
				})

				_, err := c.syncClusterUDN(cudn, nil)
				Expect(err).To(MatchError(expectedErr))
			})
			It("should fail remove NAD finalizer when delete NAD fails", func() {
				expectedErr := errors.New("test err")
				cs.NetworkAttchDefClient.(*netv1fakeclientset.Clientset).PrependReactor("delete", "network-attachment-definitions", func(testing.Action) (bool, runtime.Object, error) {
					return true, nil, expectedErr
				})

				_, err := c.syncClusterUDN(cudn, nil)
				Expect(err).To(MatchError(expectedErr))
			})
		})
	})

	Context("ClusterUserDefinedNetwork status update", func() {
		It("should succeed given no CR", func() {
			c := newTestController(noopRenderNadStub())
			Expect(c.updateClusterUDNStatus(nil, nil, nil, false)).To(Succeed())
		})
		It("should fail when CR apply status fails", func() {
			cudn := testClusterUDN("test")
			c := newTestController(noopRenderNadStub(), cudn)

			expectedErr := errors.New("test patch error")
			cs.UserDefinedNetworkClient.(*udnfakeclient.Clientset).PrependReactor("patch", "clusteruserdefinednetworks", func(testing.Action) (bool, runtime.Object, error) {
				return true, nil, expectedErr
			})

			Expect(c.updateClusterUDNStatus(cudn, nil, nil, false)).ToNot(Succeed())
		})
		It("should reflect active namespaces", func() {
			testNsNames := []string{"red", "green"}

			cudn := testClusterUDN("test", testNsNames...)
			c := newTestController(noopRenderNadStub(), cudn)

			var testNADs []netv1.NetworkAttachmentDefinition
			for _, nsName := range testNsNames {
				testNADs = append(testNADs, *testClusterUdnNAD(cudn.Name, nsName))
			}

			Expect(c.updateClusterUDNStatus(cudn, testNADs, nil, false)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(filterTransportConditions(cudn.Status.Conditions))).To(ConsistOf([]metav1.Condition{
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

			Expect(c.updateClusterUDNStatus(cudn, testNADs, nil, false)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(filterTransportConditions(cudn.Status.Conditions))).To(ConsistOf([]metav1.Condition{
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
			Expect(c.updateClusterUDNStatus(cudn, testNADs, testErr, false)).To(Succeed())

			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(normalizeConditions(filterTransportConditions(cudn.Status.Conditions))).To(ConsistOf([]metav1.Condition{
				{
					Type:    "NetworkCreated",
					Status:  "False",
					Reason:  "NetworkAttachmentDefinitionSyncError",
					Message: "test sync NAD error",
				},
			}))
		})
	})

	newCUDNWithTransport := func(name string, labels map[string]string, transport udnv1.TransportOption) *udnv1.ClusterUserDefinedNetwork {
		cudn := &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: labels,
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role:    udnv1.NetworkRolePrimary,
						Subnets: []udnv1.Layer3Subnet{{CIDR: "10.100.0.0/16"}},
					},
				},
			},
		}
		switch transport {
		case udnv1.TransportOptionNoOverlay:
			cudn.Spec.Network.Transport = udnv1.TransportOptionNoOverlay
			cudn.Spec.Network.NoOverlay = &udnv1.NoOverlayConfig{
				OutboundSNAT: udnv1.SNATEnabled,
				Routing:      udnv1.RoutingManaged,
			}
		case udnv1.TransportOptionEVPN:
			cudn.Spec.Network.Transport = udnv1.TransportOptionEVPN
			cudn.Spec.Network.EVPN = &udnv1.EVPNConfig{
				VTEP:  "test-vtep",
				IPVRF: &udnv1.VRFConfig{VNI: 100},
			}
		}
		return cudn
	}

	createAcceptedRA := func(name string, matchLabels map[string]string) {
		ra := &ratypes.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ratypes.RouteAdvertisementsSpec{
				NetworkSelectors: []apitypes.NetworkSelector{{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{MatchLabels: matchLabels},
					},
				}},
				Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
			},
		}
		_, err := cs.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ra.Status = ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}},
		}
		_, err = cs.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	createNotAcceptedRA := func(name string, matchLabels map[string]string) {
		ra := &ratypes.RouteAdvertisements{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: ratypes.RouteAdvertisementsSpec{
				NetworkSelectors: []apitypes.NetworkSelector{{
					NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{MatchLabels: matchLabels},
					},
				}},
				Advertisements: []ratypes.AdvertisementType{ratypes.PodNetwork},
			},
		}
		_, err := cs.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), ra, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ra.Status = ratypes.RouteAdvertisementsStatus{
			Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionFalse}},
		}
		_, err = cs.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().UpdateStatus(context.Background(), ra, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	expectNoTransportCondition := func(cudnName string) {
		Consistently(func() bool {
			cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudnName, metav1.GetOptions{})
			if err != nil {
				return false
			}
			for _, cond := range cudn.Status.Conditions {
				if cond.Type == "TransportAccepted" {
					return false // Found TransportAccepted condition, fail the check
				}
			}
			return true // No TransportAccepted condition found, as expected
		}, 500*time.Millisecond, 100*time.Millisecond).Should(BeTrue())
	}

	Context("CUDN Transport Validation", func() {
		var c *Controller

		AfterEach(func() {
			if c != nil {
				c.Shutdown()
			}
		})

		It("should not update status for empty transport (default OVN overlay)", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, "")
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			Expect(c.Run()).To(Succeed())
			expectNoTransportCondition("test-cudn")
		})

		It("should update status to True with NoOverlayTransportAccepted when RA is accepted", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionNoOverlay)
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			createAcceptedRA("test-ra", map[string]string{"app": "test"})
			Expect(c.Run()).To(Succeed())
			expectTransportCondition("test-cudn", metav1.ConditionTrue, ReasonNoOverlayTransportAccepted, "Transport has been configured as 'no-overlay'.")
		})

		It("should update status to False when no RouteAdvertisements exists", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionNoOverlay)
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			Expect(c.Run()).To(Succeed())
			expectTransportCondition("test-cudn", metav1.ConditionFalse, ReasonNoOverlayRouteAdvertisementsIsMissing, "No RouteAdvertisements CR is advertising the pod networks.")
		})

		It("should update status to False when RouteAdvertisements exists but not accepted", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionNoOverlay)
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			createNotAcceptedRA("test-ra", map[string]string{"app": "test"})
			Expect(c.Run()).To(Succeed())
			expectTransportCondition("test-cudn", metav1.ConditionFalse, ReasonNoOverlayRouteAdvertisementsNotAccepted, "RouteAdvertisements CR test-ra advertises the pod subnets, but its status is not accepted.")
		})

		It("should update status to True with EVPNTransportAccepted when RA is accepted", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionEVPN)
			c = newTestController(template.RenderNetAttachDefManifest, cudn, testVTEP("test-vtep"))
			createAcceptedRA("test-ra", map[string]string{"app": "test"})
			Expect(c.Run()).To(Succeed())
			expectTransportCondition("test-cudn", metav1.ConditionTrue, ReasonEVPNTransportAccepted, "Transport has been configured as 'EVPN'.")
		})

		It("should not update status when condition already matches", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionNoOverlay)
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			createAcceptedRA("test-ra", map[string]string{"app": "test"})
			Expect(c.Run()).To(Succeed())

			var initialResourceVersion string
			Eventually(func() bool {
				updatedCUDN, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "test-cudn", metav1.GetOptions{})
				if err != nil {
					return false
				}
				for _, cond := range updatedCUDN.Status.Conditions {
					if cond.Type == "TransportAccepted" && cond.Status == metav1.ConditionTrue {
						initialResourceVersion = updatedCUDN.ResourceVersion
						return true
					}
				}
				return false
			}, 2*time.Second, 100*time.Millisecond).Should(BeTrue())

			cudnObj, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "test-cudn", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = c.syncClusterUDN(cudnObj, nil)
			Expect(err).NotTo(HaveOccurred())

			Consistently(func() string {
				updatedCUDN, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), "test-cudn", metav1.GetOptions{})
				if err != nil {
					return ""
				}
				return updatedCUDN.ResourceVersion
			}, 500*time.Millisecond, 100*time.Millisecond).Should(Equal(initialResourceVersion))
		})

		It("should update status from False to True when RouteAdvertisements is created after CUDN (NoOverlay)", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionNoOverlay)
			c = newTestController(template.RenderNetAttachDefManifest, cudn)
			Expect(c.Run()).To(Succeed())

			// Verify initial status is False (no RA exists)
			expectTransportCondition("test-cudn", metav1.ConditionFalse, ReasonNoOverlayRouteAdvertisementsIsMissing, "No RouteAdvertisements CR is advertising the pod networks.")

			// Now create the RouteAdvertisements CR
			createAcceptedRA("test-ra", map[string]string{"app": "test"})

			// Verify status updates to True
			expectTransportCondition("test-cudn", metav1.ConditionTrue, ReasonNoOverlayTransportAccepted, "Transport has been configured as 'no-overlay'.")
		})

		It("should update status from False to True when RouteAdvertisements is created after CUDN (EVPN)", func() {
			cudn := newCUDNWithTransport("test-cudn", map[string]string{"app": "test"}, udnv1.TransportOptionEVPN)
			c = newTestController(template.RenderNetAttachDefManifest, cudn, testVTEP("test-vtep"))
			Expect(c.Run()).To(Succeed())

			// Verify initial status is False (no RA exists)
			expectTransportCondition("test-cudn", metav1.ConditionFalse, ReasonEVPNRouteAdvertisementsIsMissing, "No RouteAdvertisements CR is advertising the pod networks.")

			// Now create the RouteAdvertisements CR
			createAcceptedRA("test-ra", map[string]string{"app": "test"})

			// Verify status updates to True
			expectTransportCondition("test-cudn", metav1.ConditionTrue, ReasonEVPNTransportAccepted, "Transport has been configured as 'EVPN'.")
		})
	})

	Context("CUDN metrics", func() {
		var c *Controller

		BeforeEach(func() {
			metrics.ResetCUDNCount()
		})

		AfterEach(func() {
			if c != nil {
				c.Shutdown()
			}
		})

		It("should increment CUDN count on create with default transport", func() {
			baseVal, _ := getCUDNCountMetricValue("Primary", "Layer3", "Default")

			testNs := testNamespace("metrics-ns")
			cudn := testLayer3PrimaryClusterUDN("metrics-cudn", testNs.Name)
			cudn.Finalizers = nil
			c = newTestController(template.RenderNetAttachDefManifest, cudn, testNs)
			Expect(c.Run()).To(Succeed())
			DeferCleanup(metrics.DeleteCUDNCondition, cudn.Name)

			Eventually(func() []metav1.Condition {
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkCreated",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionCreated",
				Message: "NetworkAttachmentDefinition has been created in following namespaces: [metrics-ns]",
			}}))

			val, found := getCUDNCountMetricValue("Primary", "Layer3", "Default")
			Expect(found).To(BeTrue(), "CUDN count metric should exist for default transport")
			Expect(val).To(Equal(baseVal + 1))
		})

		It("should increment CUDN count on create with EVPN transport", func() {
			baseVal, _ := getCUDNCountMetricValue("Primary", "Layer2", "EVPN")

			testNs := testNamespace("metrics-evpn-ns")
			vtep := testVTEP("vtep-metrics")
			cudn := testEVPNClusterUDN("metrics-evpn-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)
			cudn.Finalizers = nil
			cudn.Spec.Network.Layer2.Role = udnv1.NetworkRolePrimary

			c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
			Expect(c.Run()).To(Succeed())
			DeferCleanup(metrics.DeleteCUDNCondition, cudn.Name)

			Eventually(func() []metav1.Condition {
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkCreated",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionCreated",
				Message: "NetworkAttachmentDefinition has been created in following namespaces: [metrics-evpn-ns]",
			}}))

			val, found := getCUDNCountMetricValue("Primary", "Layer2", "EVPN")
			Expect(found).To(BeTrue(), "CUDN count metric should exist for EVPN transport")
			Expect(val).To(Equal(baseVal + 1))
		})

		It("should decrement CUDN count on delete", func() {
			testNs := testNamespace("metrics-del-ns")
			cudn := testLayer2SecondaryClusterUDN("metrics-del-cudn", testNs.Name)
			cudn.Finalizers = nil
			c = newTestController(template.RenderNetAttachDefManifest, cudn, testNs)
			Expect(c.Run()).To(Succeed())
			DeferCleanup(metrics.DeleteCUDNCondition, cudn.Name)

			Eventually(func() []metav1.Condition {
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				return normalizeConditions(filterTransportConditions(cudn.Status.Conditions))
			}).Should(Equal([]metav1.Condition{{
				Type:    "NetworkCreated",
				Status:  "True",
				Reason:  "NetworkAttachmentDefinitionCreated",
				Message: "NetworkAttachmentDefinition has been created in following namespaces: [metrics-del-ns]",
			}}))

			beforeVal, found := getCUDNCountMetricValue("Secondary", "Layer2", "Default")
			Expect(found).To(BeTrue())

			markCUDNForDeletion(cudn.Name)

			expected := beforeVal - 1
			Eventually(func() float64 {
				val, _ := getCUDNCountMetricValue("Secondary", "Layer2", "Default")
				return val
			}, 2*time.Second, 50*time.Millisecond).Should(Equal(expected), "CUDN count metric should decrement by exactly one after deletion")
		})

		It("should record NetworkCreated=True condition metric on successful CUDN creation", func() {
			testNs := testNamespace("cond-ns")
			cudn := testLayer2SecondaryClusterUDN("cond-cudn", testNs.Name)

			c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs)
			Expect(c.Run()).To(Succeed())
			DeferCleanup(metrics.DeleteCUDNCondition, cudn.Name)

			Eventually(func(g Gomega) {
				cudn, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(normalizeConditions(filterTransportConditions(cudn.Status.Conditions))).To(Equal([]metav1.Condition{{
					Type:    "NetworkCreated",
					Status:  "True",
					Reason:  "NetworkAttachmentDefinitionCreated",
					Message: "NetworkAttachmentDefinition has been created in following namespaces: [cond-ns]",
				}}))
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				val, found := getCUDNConditionMetricValue(cudn.Name, "NetworkCreated", "true")
				g.Expect(found).To(BeTrue(), "condition metric should exist")
				g.Expect(val).To(Equal(1.0))
				valFalse, foundFalse := getCUDNConditionMetricValue(cudn.Name, "NetworkCreated", "false")
				g.Expect(foundFalse).To(BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(Equal(0.0))
			}).Should(Succeed(), "NetworkCreated condition metrics should be recorded")
		})

		It("should remove condition metrics on CUDN deletion", func() {
			testNs := testNamespace("cond-del-ns")
			cudn := testLayer2SecondaryClusterUDN("cond-del-cudn", testNs.Name)

			c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs)
			Expect(c.Run()).To(Succeed())

			Eventually(func(g Gomega) {
				val, found := getCUDNConditionMetricValue(cudn.Name, "NetworkCreated", "true")
				g.Expect(found).To(BeTrue(), "condition metric should exist")
				g.Expect(val).To(Equal(1.0))
			}).Should(Succeed(), "NetworkCreated condition metric status=true should be 1 after creation")

			markCUDNForDeletion(cudn.Name)

			Eventually(func() bool {
				_, foundTrue := getCUDNConditionMetricValue(cudn.Name, "NetworkCreated", "true")
				_, foundFalse := getCUDNConditionMetricValue(cudn.Name, "NetworkCreated", "false")
				return foundTrue || foundFalse
			}).Should(BeFalse(), "both condition metric timeseries should be removed after deletion")
		})

		It("should record TransportAccepted condition metric for EVPN CUDN with accepted RA", func() {
			testNs := testNamespace("cond-evpn-ns")
			vtep := testVTEP("cond-vtep")
			cudn := testEVPNClusterUDN("cond-evpn-cudn", &udnv1.EVPNConfig{VTEP: vtep.Name, MACVRF: &udnv1.VRFConfig{VNI: 100}}, testNs.Name)

			c = newTestControllerWithNetworkManager(template.RenderNetAttachDefManifest, cudn, testNs, vtep)
			Expect(c.Run()).To(Succeed())
			DeferCleanup(metrics.DeleteCUDNCondition, cudn.Name)

			createAcceptedRA("cond-ra", cudn.Labels)

			Eventually(func(g Gomega) {
				cudnObj, err := cs.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Get(context.Background(), cudn.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				tc := meta.FindStatusCondition(cudnObj.Status.Conditions, "TransportAccepted")
				g.Expect(tc).NotTo(BeNil(), "TransportAccepted condition should exist")
				g.Expect(tc.Status).To(Equal(metav1.ConditionTrue))
			}).Should(Succeed(), "TransportAccepted status condition should be True")

			Eventually(func(g Gomega) {
				valTrue, found := getCUDNConditionMetricValue(cudn.Name, "TransportAccepted", "true")
				g.Expect(found).To(BeTrue(), "condition metric status=true should exist")
				g.Expect(valTrue).To(Equal(1.0))
				valFalse, foundFalse := getCUDNConditionMetricValue(cudn.Name, "TransportAccepted", "false")
				g.Expect(foundFalse).To(BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(Equal(0.0))
			}).Should(Succeed(), "TransportAccepted condition metrics should be recorded")
		})

	})
})

// assertConditionReportNetworkInUse checks conditions reflect network consumers.
func assertConditionReportNetworkInUse(conditions []metav1.Condition, messageNADPods map[string]string) error {
	// In order make this check fit Eventually clause in a way it could wait for the expected condition state
	// Gomega Expect is not being used; as they would make Eventually fail immediately.
	// In addition, Gomega equality matcher cannot be used since condition message namespaces order is inconsistent.

	conditions = filterTransportConditions(conditions)
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
	GinkgoHelper()

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
	GinkgoHelper()

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

func filterTransportConditions(conditions []metav1.Condition) []metav1.Condition {
	var filtered []metav1.Condition
	for _, cond := range conditions {
		if cond.Type != "TransportAccepted" {
			filtered = append(filtered, cond)
		}
	}
	return filtered
}

func testPrimaryUDN() *udnv1.UserDefinedNetwork {
	return &udnv1.UserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "test",
			UID:        "1",
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
		},
		Spec: udnv1.UserDefinedNetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role: udnv1.NetworkRolePrimary,
			},
		},
	}
}

func testSecondaryUDN() *udnv1.UserDefinedNetwork {
	return &udnv1.UserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  "test",
			UID:        "1",
			Finalizers: []string{"k8s.ovn.org/user-defined-network-protection"},
		},
		Spec: udnv1.UserDefinedNetworkSpec{
			Topology: udnv1.NetworkTopologyLayer3,
			Layer3: &udnv1.Layer3Config{
				Role: udnv1.NetworkRoleSecondary,
			},
		},
	}
}

func testsUDNWithDeletionTimestamp(ts time.Time) *udnv1.UserDefinedNetwork {
	udn := testPrimaryUDN()
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
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
				},
			},
		},
		Spec: netv1.NetworkAttachmentDefinitionSpec{},
	}
}

func invalidTestNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/metadata.name": name,
			},
		},
		Spec: corev1.NamespaceSpec{},
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
	ns := invalidTestNamespace(name)
	ns.ObjectMeta.Labels[ovntypes.RequiredUDNNamespaceLabel] = ""
	return ns
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

// testLayer3PrimaryClusterUDN returns a CUDN with Layer3 Primary topology and default transport.
func testLayer3PrimaryClusterUDN(name string, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	cudn := testClusterUDN(name, targetNamespaces...)
	cudn.Spec.Network = udnv1.NetworkSpec{
		Topology: udnv1.NetworkTopologyLayer3,
		Layer3: &udnv1.Layer3Config{
			Role:    udnv1.NetworkRolePrimary,
			Subnets: []udnv1.Layer3Subnet{{CIDR: "10.100.0.0/16"}},
		},
	}
	return cudn
}

// testLayer2SecondaryClusterUDN returns a CUDN with Layer2 Secondary topology and default transport.
func testLayer2SecondaryClusterUDN(name string, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	cudn := testClusterUDN(name, targetNamespaces...)
	cudn.Spec.Network = udnv1.NetworkSpec{
		Topology: udnv1.NetworkTopologyLayer2,
		Layer2: &udnv1.Layer2Config{
			Role:    udnv1.NetworkRoleSecondary,
			Subnets: udnv1.DualStackCIDRs{"10.20.0.0/16"},
		},
	}
	return cudn
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
					BlockOwnerDeletion: ptr.To(true),
					Controller:         ptr.To(true),
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
	return func(client.Object, string, ...template.RenderOption) (*netv1.NetworkAttachmentDefinition, error) {
		return nad, err
	}
}

func testEVPNClusterUDN(name string, evpnCfg *udnv1.EVPNConfig, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
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
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRoleSecondary,
					Subnets: udnv1.DualStackCIDRs{"10.10.10.0/24"},
				},
				Transport: udnv1.TransportOptionEVPN,
				EVPN:      evpnCfg,
			},
		},
	}
}

// testEVPNClusterUdnNADWithEVPNCfg creates an EVPN NAD with the given EVPN configuration.
func testEVPNClusterUdnNADWithEVPNCfg(name, namespace string, evpnCfg *ovncnitypes.EVPNConfig) *netv1.NetworkAttachmentDefinition {
	nad := testClusterUdnNAD(name, namespace)
	evpnJSON, err := json.Marshal(evpnCfg)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal EVPN config: %v", err))
	}
	nad.Spec.Config = fmt.Sprintf(`{"cniVersion":"1.1.0","name":"cluster_udn_%s","type":"ovn-k8s-cni-overlay","netAttachDefName":"%s/%s","topology":"layer2","role":"primary","subnets":"10.10.0.0/16","transport":"evpn","evpn":%s}`, name, namespace, name, evpnJSON)
	return nad
}

// testEVPNClusterUdnNADOwnedByCUDN creates an EVPN NAD owned by the given CUDN.
func testEVPNClusterUdnNADOwnedByCUDN(cudn *udnv1.ClusterUserDefinedNetwork, namespace string, evpnCfg *ovncnitypes.EVPNConfig) *netv1.NetworkAttachmentDefinition {
	nad := testEVPNClusterUdnNADWithEVPNCfg(cudn.Name, namespace, evpnCfg)
	nad.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion:         "k8s.ovn.org/v1",
			Kind:               "ClusterUserDefinedNetwork",
			Name:               cudn.Name,
			UID:                cudn.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
		},
	}
	return nad
}

func testSymmetricIRBClusterUDN(name string, vtepName string, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	return testEVPNClusterUDN(name, &udnv1.EVPNConfig{
		VTEP:   vtepName,
		MACVRF: &udnv1.VRFConfig{VNI: 100},
		IPVRF:  &udnv1.VRFConfig{VNI: 200},
	}, targetNamespaces...)
}

func testEVPNIPVRFClusterUDN(name string, vtepName string, targetNamespaces ...string) *udnv1.ClusterUserDefinedNetwork {
	cudn := testEVPNClusterUDN(name, &udnv1.EVPNConfig{
		VTEP:  vtepName,
		IPVRF: &udnv1.VRFConfig{VNI: 200},
	}, targetNamespaces...)
	cudn.Spec.Network.Topology = udnv1.NetworkTopologyLayer3
	cudn.Spec.Network.Layer2 = nil
	cudn.Spec.Network.Layer3 = &udnv1.Layer3Config{Role: udnv1.NetworkRoleSecondary}
	return cudn
}

func testVTEP(name string) *vtepv1.VTEP {
	return &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("vtep-" + name),
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: []vtepv1.CIDR{"100.64.0.0/24"},
			Mode:  vtepv1.VTEPModeManaged,
		},
		Status: vtepv1.VTEPStatus{
			Conditions: []metav1.Condition{{
				Type:               "Accepted",
				Status:             metav1.ConditionTrue,
				Reason:             "Allocated",
				Message:            "VTEP allocation succeeded",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// evpnVIDsFromNAD extracts MAC-VRF and IP-VRF VIDs from a NAD config.
// Returns (macVID, ipVID) where 0 indicates the VRF is not present or has no VID.
func evpnVIDsFromNAD(nad *netv1.NetworkAttachmentDefinition) (macVID, ipVID int) {
	if nad == nil {
		return 0, 0
	}
	var netConf ovncnitypes.NetConf
	if err := json.Unmarshal([]byte(nad.Spec.Config), &netConf); err != nil {
		return 0, 0
	}
	if netConf.EVPN == nil {
		return 0, 0
	}
	if netConf.EVPN.MACVRF != nil {
		macVID = netConf.EVPN.MACVRF.VID
	}
	if netConf.EVPN.IPVRF != nil {
		ipVID = netConf.EVPN.IPVRF.VID
	}
	return macVID, ipVID
}

// setNADEVPNVIDs modifies the MAC-VRF and/or IP-VRF VIDs in a NAD config.
// Pass 0 to leave a VID unchanged. This is used in tests to set specific VIDs
// without rewriting the entire config.
func setNADEVPNVIDs(nad *netv1.NetworkAttachmentDefinition, macVID, ipVID int) error {
	var netConf ovncnitypes.NetConf
	if err := json.Unmarshal([]byte(nad.Spec.Config), &netConf); err != nil {
		return err
	}
	if netConf.EVPN == nil {
		return fmt.Errorf("NAD has no EVPN config")
	}
	if macVID > 0 {
		if netConf.EVPN.MACVRF == nil {
			return fmt.Errorf("NAD has no EVPN MAC-VRF config")
		}
		netConf.EVPN.MACVRF.VID = macVID
	}
	if ipVID > 0 {
		if netConf.EVPN.IPVRF == nil {
			return fmt.Errorf("NAD has no EVPN IP-VRF config")
		}
		netConf.EVPN.IPVRF.VID = ipVID
	}
	configBytes, err := json.Marshal(netConf)
	if err != nil {
		return err
	}
	nad.Spec.Config = string(configBytes)
	return nil
}

// getCUDNCountMetricValue returns the current gauge value for the CUDN count metric with the given labels.
func getCUDNCountMetricValue(role, topology, transport string) (float64, bool) {
	metricName := prometheus.BuildFQName(ovntypes.MetricOvnkubeNamespace, ovntypes.MetricOvnkubeSubsystemClusterManager, "cluster_user_defined_networks")
	mf := ovntest.FindMetricFamily(metricName)
	if mf == nil {
		return 0, false
	}
	for _, metric := range mf.GetMetric() {
		if ovntest.MetricLabelValue(metric.GetLabel(), "role") == role &&
			ovntest.MetricLabelValue(metric.GetLabel(), "topology") == topology &&
			ovntest.MetricLabelValue(metric.GetLabel(), "transport") == transport {
			return metric.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func getCUDNConditionMetricValue(nameLabel, conditionLabel, statusLabel string) (float64, bool) {
	metricName := prometheus.BuildFQName(ovntypes.MetricOvnkubeNamespace, ovntypes.MetricOvnkubeSubsystemClusterManager, "cluster_user_defined_network_condition")
	return ovntest.GetConditionMetricValue(metricName, nameLabel, conditionLabel, statusLabel)
}
