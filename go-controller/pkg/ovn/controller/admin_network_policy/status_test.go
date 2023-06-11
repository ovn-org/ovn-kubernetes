package adminnetworkpolicy

import (
	"context"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
	anpfake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

var alwaysReady = func() bool { return true }

func createTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	nbGlobal := &nbdb.NBGlobal{Name: zone}
	ops, err := nbClient.Create(nbGlobal)
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func deleteTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	p := func(nbGlobal *nbdb.NBGlobal) bool {
		return true
	}

	ops, err := nbClient.WhereCache(p).Delete()
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

var initialANP = anpapi.AdminNetworkPolicy{
	ObjectMeta: metav1.ObjectMeta{
		Name: "harry-potter",
	},
	Spec: anpapi.AdminNetworkPolicySpec{
		Subject:  anpapi.AdminNetworkPolicySubject{},
		Priority: 20,
		Ingress:  []anpapi.AdminNetworkPolicyIngressRule{},
		Egress:   []anpapi.AdminNetworkPolicyEgressRule{},
	},
}

var initialBANP = anpapi.BaselineAdminNetworkPolicy{
	ObjectMeta: metav1.ObjectMeta{
		Name: "jon-snow",
	},
	Spec: anpapi.BaselineAdminNetworkPolicySpec{
		Subject: anpapi.AdminNetworkPolicySubject{},
		Ingress: []anpapi.BaselineAdminNetworkPolicyIngressRule{},
		Egress:  []anpapi.BaselineAdminNetworkPolicyEgressRule{},
	},
}

func newANPController() (*Controller, error) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	config.PrepareTestConfig()
	config.OVNKubernetesFeature.EnableAdminNetworkPolicy = true
	nbClient, _, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{}, nil)
	if err != nil {
		return nil, err
	}
	fakeClient := &util.OVNClientset{
		KubeClient: fake.NewSimpleClientset(),
		ANPClient: anpfake.NewSimpleClientset(
			&anpapi.AdminNetworkPolicyList{
				Items: []anpapi.AdminNetworkPolicy{initialANP},
			},
			&anpapi.BaselineAdminNetworkPolicyList{
				Items: []anpapi.BaselineAdminNetworkPolicy{initialBANP},
			},
		),
	}
	watcher, err := factory.NewMasterWatchFactory(fakeClient.GetMasterClientset())
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	err = watcher.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	nbZoneFailed := false
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls util.GetNBZone().
	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	addressSetFactory := addressset.NewOvnAddressSetFactory(nbClient, config.IPv4Mode, config.IPv6Mode)
	recorder := record.NewFakeRecorder(10)
	controller, err := NewController(
		"default-network-controller",
		nbClient,
		fakeClient.ANPClient,
		watcher.ANPInformer(),
		watcher.BANPInformer(),
		watcher.NamespaceCoreInformer(),
		watcher.PodCoreInformer(),
		addressSetFactory,
		nil, // we don't care about pods in this test
		"global",
		recorder,
	)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
	controller.anpCacheSynced = alwaysReady
	controller.banpCacheSynced = alwaysReady
	controller.anpNamespaceSynced = alwaysReady
	controller.anpPodSynced = alwaysReady
	return controller, nil
}

func TestAddOrUpdateAdminNetworkPolicyStatus(t *testing.T) {
	anpName := "harry-potter"
	banpName := "jon-snow"
	message := "you know nothing jon snow"
	zone := "targaryen"
	g := gomega.NewGomegaWithT(t)
	controller, err := newANPController()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	err = controller.updateANPStatusToNotReady(&initialANP, zone, message)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() int {
		latestANP, err := controller.anpLister.Get(anpName)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return len(latestANP.Status.Conditions)
	}).Should(gomega.Equal(1))
	anp, err := controller.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), anpName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(anp.Status.Conditions[0].Type).To(gomega.Equal(policyReadyStatusType + zone))
	g.Expect(anp.Status.Conditions[0].Message).To(gomega.Equal(message))
	g.Expect(anp.Status.Conditions[0].Reason).To(gomega.Equal(policyNotReadyReason))
	g.Expect(anp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionFalse))

	err = controller.updateANPStatusToReady(anp, controller.zone)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() int {
		latestANP, err := controller.anpLister.Get(anpName)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return len(latestANP.Status.Conditions)
	}).Should(gomega.Equal(2))
	anp, err = controller.anpClientSet.PolicyV1alpha1().AdminNetworkPolicies().Get(context.TODO(), anpName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(anp.Status.Conditions[1].Type).To(gomega.Equal(policyReadyStatusType + controller.zone))
	g.Expect(anp.Status.Conditions[1].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
	g.Expect(anp.Status.Conditions[1].Reason).To(gomega.Equal(policyReadyReason))
	g.Expect(anp.Status.Conditions[1].Status).To(gomega.Equal(metav1.ConditionTrue))

	err = controller.updateBANPStatusToNotReady(&initialBANP, zone, message)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() int {
		latestBANP, err := controller.banpLister.Get(banpName)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return len(latestBANP.Status.Conditions)
	}).Should(gomega.Equal(1))
	banp, err := controller.anpClientSet.PolicyV1alpha1().BaselineAdminNetworkPolicies().Get(context.TODO(), banpName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(banp.Status.Conditions[0].Type).To(gomega.Equal(policyReadyStatusType + zone))
	g.Expect(banp.Status.Conditions[0].Message).To(gomega.Equal(message))
	g.Expect(banp.Status.Conditions[0].Reason).To(gomega.Equal(policyNotReadyReason))
	g.Expect(banp.Status.Conditions[0].Status).To(gomega.Equal(metav1.ConditionFalse))

	err = controller.updateBANPStatusToReady(banp, controller.zone)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Eventually(func() int {
		latestBANP, err := controller.banpLister.Get(banpName)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return len(latestBANP.Status.Conditions)
	}, "2s").Should(gomega.Equal(2))
	banp, err = controller.anpClientSet.PolicyV1alpha1().BaselineAdminNetworkPolicies().Get(context.TODO(), banpName, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(banp.Status.Conditions[1].Type).To(gomega.Equal(policyReadyStatusType + controller.zone))
	g.Expect(banp.Status.Conditions[1].Message).To(gomega.Equal("Setting up OVN DB plumbing was successful"))
	g.Expect(banp.Status.Conditions[1].Reason).To(gomega.Equal(policyReadyReason))
	g.Expect(banp.Status.Conditions[1].Status).To(gomega.Equal(metav1.ConditionTrue))
}
