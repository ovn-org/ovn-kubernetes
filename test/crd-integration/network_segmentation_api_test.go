// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	controllerruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario"
	testscenariocudn "github.com/ovn-kubernetes/ovn-kubernetes/test/crd-integration/testscenario/cudn"
)

var _ = Describe("Network Segmentation: API validations", func() {
	DescribeTable("api-server should reject invalid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})

			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).To(HaveOccurred(), "should fail to create invalid CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, mismatch topology and config", testscenariocudn.MismatchTopologyConfig),
		Entry("ClusterUserDefinedNetwork, localnet, invalid role", testscenariocudn.LocalnetInvalidRole),
		Entry("ClusterUserDefinedNetwork, localnet, invalid physicalNetworkName", testscenariocudn.LocalnetInvalidPhyNetName),
		Entry("ClusterUserDefinedNetwork, localnet, invalid subnets", testscenariocudn.LocalnetInvalidSubnets),
		Entry("ClusterUserDefinedNetwork, localnet, invalid mtu", testscenariocudn.LocalnetInvalidMTU),
		Entry("ClusterUserDefinedNetwork, localnet, invalid vlan", testscenariocudn.LocalnetInvalidVLAN),
		Entry("ClusterUserDefinedNetwork, localnet, invalid macSecurity", testscenariocudn.LocalnetInvalidMACSecurity),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNInvalid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNInvalid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNInvalid),
		Entry("ClusterUserDefinedNetwork, no-overlay, invalid", testscenariocudn.NoOverlayInvalid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3InvalidSubnets),
	)

	DescribeTable("api-server should reject invalid CRs",
		func(updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(updateScenarios)
			})
			for _, s := range updateScenarios {
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By(s.Description + ": creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
				By(s.Description + ": applying update to CR should fail")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(HaveOccurred(), "should fail to update CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Later3InvalidSubnetsUpdate),
	)

	DescribeTable("api-server should accept valid CRs",
		func(scenarios []testscenario.ValidateCRScenario) {
			DeferCleanup(func() {
				cleanupValidateCRsTest(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				obj, err := testscenario.ValidateScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				err = k8sClient.Create(ctx, obj)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet", testscenariocudn.LocalnetValid),
		Entry("ClusterUserDefinedNetwork, layer2", testscenariocudn.Layer2CUDNValid),
		Entry("ClusterUserDefinedNetwork, evpn", testscenariocudn.EVPNCUDNValid),
		Entry("UserDefinedNetwork, layer2", testscenariocudn.Layer2UDNValid),
		Entry("ClusterUserDefinedNetwork, no-overlay, valid", testscenariocudn.NoOverlayValid),
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnets),
	)

	DescribeTable("api-server should reject invalid updates",
		func(scenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(scenarios)
			})
			for _, s := range scenarios {
				By(s.Description)
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By("Creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")

				By("Updating CR")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(HaveOccurred(), "should fail to update CR")
				Expect(err.Error()).To(ContainSubstring(s.ExpectedErr))
			}
		},
		Entry("ClusterUserDefinedNetwork, localnet, mac-security", testscenariocudn.LocalnetUpdatesRejected),
		Entry("ClusterUserDefinedNetwork, layer2, mac-security", testscenariocudn.Layer2CUDNUpdatesRejected),
		Entry("UserDefinedNetwork, layer2, mac-security", testscenariocudn.Layer2UDNUpdatesRejected),
	)

	DescribeTable("api-server should accept valid CRs",
		func(updateScenarios []testscenario.UpdateCRScenario) {
			DeferCleanup(func() {
				cleanupUpdateCRScenario(updateScenarios)
			})
			for _, s := range updateScenarios {
				By(s.Description + ": parsing objects")
				initObject, updateObject, err := testscenario.UpdateCRScenarioToObject(s)
				Expect(err).ToNot(HaveOccurred(), "must convert scenario to kubernetes object")
				By(s.Description + ": creating initial CR")
				err = k8sClient.Create(ctx, initObject)
				Expect(err).NotTo(HaveOccurred(), "should create valid CR successfully")
				By(s.Description + ": applying update to CR should succeed")
				// resource version must get set on update, therefore retrieve the newly created object and update the spec
				err = k8sClient.Get(ctx, types.NamespacedName{Namespace: initObject.GetNamespace(), Name: initObject.GetName()}, initObject)
				Expect(err).NotTo(HaveOccurred(), "should get valid CR from KAPI server")
				updateObject.SetResourceVersion(initObject.GetResourceVersion())
				err = k8sClient.Update(ctx, updateObject)
				Expect(err).To(BeNil(), "should not fail to update CR")
			}
		},
		Entry("ClusterUserDefinedNetwork, layer3, multi-subnets", testscenariocudn.Layer3ValidSubnetsUpdates),
	)
})

func cleanupValidateCRsTest(scenarios []testscenario.ValidateCRScenario) {
	objs, err := testscenario.ValidateScenariosToObjects(scenarios)
	Expect(err).NotTo(HaveOccurred(), "must convert manifest to object")
	for _, o := range objs {
		err = k8sClient.Delete(ctx, o)
		err = controllerruntimeclient.IgnoreNotFound(err)
		Expect(err).NotTo(HaveOccurred(), "expected the object to be deleted")
	}
	// Verify each named resource is gone individually — a global "no resources found"
	// check is not parallel-safe since other concurrent tests may have live CUDNs.
	for _, o := range objs {
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, o)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected the object to be deleted")
	}
}

func cleanupUpdateCRScenario(updateScenarios []testscenario.UpdateCRScenario) {
	scenarios := make([]testscenario.ValidateCRScenario, 0, len(updateScenarios))
	for _, updateScenario := range updateScenarios {
		scenarios = append(scenarios, updateScenario.ValidateCRScenario)
	}
	cleanupValidateCRsTest(scenarios)
}
