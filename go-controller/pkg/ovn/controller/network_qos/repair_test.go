// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package networkqos

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	nqostype "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Capture the predicate because garbage collection of unreferenced QoS rows can
// otherwise hide incorrect ownership filtering in the resulting database state.
type qosPredicateClient struct {
	libovsdbclient.Client
	predicate libovsdbops.QoSPredicate
}

func (c *qosPredicateClient) WhereCache(predicate interface{}) libovsdbclient.ConditionalAPI {
	if p, ok := predicate.(libovsdbops.QoSPredicate); ok {
		c.predicate = p
	}
	return c.Client.WhereCache(predicate)
}

var _ = Describe("NetworkQoS repair", func() {
	It("deletes only stale QoS rows owned by this NetworkQoS controller", func() {
		policy := &nqostype.NetworkQoS{
			ObjectMeta: metav1.ObjectMeta{Namespace: nqosNamespace, Name: nqosName},
		}
		policyKey := joinMetaNamespaceAndName(policy.Namespace, policy.Name, ":")
		clientset := util.GetOVNClientset(policy)
		initEnv(clientset, nil)
		client := &qosPredicateClient{Client: nbClient}
		controller, err := NewController(defaultControllerName, &util.DefaultNetInfo{}, client,
			util.EventRecorder(clientset.KubeClient), clientset.NetworkQoSClient,
			watchFactory.NetworkQoSInformer(), watchFactory.NamespaceCoreInformer(),
			watchFactory.PodCoreInformer(), watchFactory.NADInformer(), nil,
			defaultAddrsetFactory, "node1")
		Expect(err).NotTo(HaveOccurred())
		defer controller.shutdown()
		Expect(watchFactory.Start()).To(Succeed())

		Expect(controller.repairNetworkQoSes()).To(Succeed())
		Expect(client.predicate).NotTo(BeNil())
		for _, tc := range []struct {
			name       string
			controller string
			ownerType  string
			objectName string
			keep       bool
		}{
			{"stale", defaultControllerName, string(libovsdbops.NetworkQoSOwnerType), "deleted:policy", false},
			{"missing-object-name", defaultControllerName, string(libovsdbops.NetworkQoSOwnerType), "", false},
			{"existing", defaultControllerName, string(libovsdbops.NetworkQoSOwnerType), policyKey, true},
			{"other-controller", streamControllerName, string(libovsdbops.NetworkQoSOwnerType), "deleted:policy", true},
			{"other-owner-type", defaultControllerName, string(libovsdbops.EgressQoSOwnerType), "deleted:policy", true},
			{"missing-owner-type", defaultControllerName, "", "", true},
			{"unowned", "", "", "", true},
		} {
			qos := &nbdb.QoS{
				UUID:      tc.name,
				Direction: nbdb.QoSDirectionFromLport,
				Match:     "ip4",
				Priority:  100,
				Action:    map[string]int{nbdb.QoSActionDSCP: 10},
				ExternalIDs: map[string]string{
					libovsdbops.OwnerControllerKey.String(): tc.controller,
					libovsdbops.OwnerTypeKey.String():       tc.ownerType,
					libovsdbops.ObjectNameKey.String():      tc.objectName,
				},
			}
			Expect(client.predicate(qos)).To(Equal(!tc.keep), tc.name)
		}
	})
})
