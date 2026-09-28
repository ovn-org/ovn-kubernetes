// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	controllerutil "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepfake "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned/fake"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func newVTEP(name string, mode vtepv1.VTEPMode, cidrs ...string) *vtepv1.VTEP {
	dsCIDRs := make([]vtepv1.CIDR, len(cidrs))
	for i, c := range cidrs {
		dsCIDRs[i] = vtepv1.CIDR(c)
	}
	return &vtepv1.VTEP{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: vtepv1.VTEPSpec{
			CIDRs: dsCIDRs,
			Mode:  mode,
		},
	}
}

func newCUDNWithEVPN(name, vtepName string) *udnv1.ClusterUserDefinedNetwork {
	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{},
			Network: udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Subnets: []udnv1.Layer3Subnet{
						{CIDR: "10.0.0.0/16"},
					},
				},
				EVPN: &udnv1.EVPNConfig{
					VTEP:  vtepName,
					IPVRF: &udnv1.VRFConfig{VNI: 100},
				},
			},
		},
	}
}

func getVTEPFinalizers(client *vtepfake.Clientset, vtepName string) ([]string, error) {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	// NotFound means the object was garbage-collected after its finalizers
	// were cleared, so treat it as an empty finalizer list.
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}
	return vtep.Finalizers, nil
}

// newNodeWithVTEPAnnotation creates a node with the k8s.ovn.org/vteps annotation.
// vtepIPs is a map of VTEP name to list of IPs discovered on this node.
func newNodeWithVTEPAnnotation(name string, vtepIPs map[string][]string) *corev1.Node {
	vteps := make(map[string]util.VTEPNodeAnnotation, len(vtepIPs))
	for vtepName, ips := range vtepIPs {
		vteps[vtepName] = util.VTEPNodeAnnotation{IPs: ips}
	}
	annotation, _ := json.Marshal(vteps)
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				util.OVNNodeVTEPs: string(annotation),
			},
		},
	}
}

// countStatusWrites returns the number of status-subresource writes recorded by
// the fake clientset. updateStatusCondition is the only code path that writes
// the status subresource (via ApplyStatus, recorded as a "patch" with
// subresource "status"). Filtering on the subresource isolates these from both
// the test's own condition-polling get calls and the finalizer Apply in
// ensureFinalizer (a "patch" on the main resource, subresource ""), which can
// fire more than once per VTEP while the lister cache lags and is unrelated to
// the status convergence we want to test.
func countStatusWrites(client *vtepfake.Clientset) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" && action.GetSubresource() == "status" {
			count++
		}
	}
	return count
}

func getVTEPCondition(client *vtepfake.Clientset, vtepName, conditionType string) (*metav1.Condition, error) {
	vtep, err := client.K8sV1().VTEPs().Get(context.Background(), vtepName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get VTEP %s: %w", vtepName, err)
	}
	return meta.FindStatusCondition(vtep.Status.Conditions, conditionType), nil
}

var _ = ginkgo.Describe("VTEP Controller", func() {
	var (
		controller    *Controller
		fakeVTEP      *vtepfake.Clientset
		fakeClientset *util.OVNClusterManagerClientset
		wf            *factory.WatchFactory
		fakeRecorder  *record.FakeRecorder
	)

	// startWithReconcile builds and starts the controller. When reconcile is
	// non-nil it replaces the VTEP sub-controller's reconcile func, letting a
	// test wrap controller.reconcileVTEP with an invocation counter to assert
	// the cross-VTEP re-queue loop converges instead of spinning forever. The
	// config mirrors the one built in NewController; only Reconcile differs. It
	// is swapped before controller.Start() so no reconcile is missed; the
	// sub-controller built in NewController is discarded unstarted.
	startWithReconcile := func(reconcile func(context.Context, string) error, objects ...runtime.Object) {
		vtepObjects := []runtime.Object{}
		otherObjects := []runtime.Object{}
		for _, obj := range objects {
			switch obj.(type) {
			case *vtepv1.VTEP:
				vtepObjects = append(vtepObjects, obj)
			default:
				otherObjects = append(otherObjects, obj)
			}
		}

		fakeVTEP = vtepfake.NewSimpleClientset(vtepObjects...)
		ovntest.AddVTEPApplyReactor(fakeVTEP)
		fakeRecorder = record.NewFakeRecorder(100)

		fakeClientset = util.GetOVNClientset(otherObjects...).GetClusterManagerClientset()
		fakeClientset.VTEPClient = fakeVTEP

		var err error
		wf, err = factory.NewClusterManagerWatchFactory(fakeClientset)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		controller = NewController(wf, fakeClientset, fakeRecorder)

		if reconcile != nil {
			controller.vtepController = controllerutil.NewController(
				"clustermanager-vtep-controller",
				&controllerutil.ControllerConfig[vtepv1.VTEP]{
					RateLimiter:    workqueue.DefaultTypedControllerRateLimiter[string](),
					Informer:       wf.VTEPInformer().Informer(),
					Lister:         wf.VTEPInformer().Lister().List,
					Reconcile:      reconcile,
					ObjNeedsUpdate: vtepNeedsUpdate,
					Threadiness:    1,
				},
			)
		}

		err = wf.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		err = controller.Start()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	start := func(objects ...runtime.Object) {
		startWithReconcile(nil, objects...)
	}

	markVTEPForDeletion := func(name string) {
		ginkgo.GinkgoHelper()
		v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), name, metav1.GetOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		now := metav1.Now()
		v.DeletionTimestamp = &now
		_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	ginkgo.BeforeEach(func() {
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableRouteAdvertisements = true
		config.OVNKubernetesFeature.EnableEVPN = true
		metrics.RegisterClusterManagerFunctional()
		config.Gateway.Mode = config.GatewayModeLocal
	})

	ginkgo.AfterEach(func() {
		if controller != nil {
			controller.Stop()
		}
		if wf != nil {
			wf.Shutdown()
		}
	})

	ginkgo.Context("Managed mode gate", func() {
		ginkgo.It("sets Accepted=False for a VTEP with mode Managed", func() {
			vtep := newVTEP("managed-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)
			ginkgo.DeferCleanup(metrics.DeleteVTEPCondition, "managed-vtep")

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "managed-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal("ManagedModeNotSupported")),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring("ManagedModeNotSupported"),
			))
		})

		ginkgo.It("sets Accepted=True for a VTEP with mode Unmanaged", func() {
			vtep := newVTEP("unmanaged-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)
			ginkgo.DeferCleanup(metrics.DeleteVTEPCondition, "unmanaged-vtep")

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "unmanaged-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))
		})

		ginkgo.It("sets Accepted=False when mode changes from Unmanaged to Managed", func() {
			vtep := newVTEP("mode-change-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-change-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			vtep, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "mode-change-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtep.Spec.Mode = vtepv1.VTEPModeManaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-change-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal("ManagedModeNotSupported")),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring("ManagedModeNotSupported"),
			))
		})

		ginkgo.It("sets Accepted=True when mode changes from Managed to Unmanaged", func() {
			vtep := newVTEP("mode-recover-vtep", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-recover-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal("ManagedModeNotSupported")),
			))

			vtep, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "mode-recover-vtep", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtep.Spec.Mode = vtepv1.VTEPModeUnmanaged
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), vtep, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "mode-recover-vtep", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal("Allocated")),
			))
		})
	})

	ginkgo.Context("Finalizer management", func() {
		ginkgo.It("adds finalizer to a new VTEP", func() {
			vtep := newVTEP("finalize-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "finalize-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("removes finalizer and allows deletion when no CUDNs reference the VTEP", func() {
			vtep := newVTEP("delete-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "delete-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			markVTEPForDeletion("delete-vtep")

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "delete-vtep")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("blocks deletion when a CUDN references the VTEP", func() {
			cudn := newCUDNWithEVPN("test-cudn", "blocked-vtep")
			vtep := newVTEP("blocked-vtep", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			markVTEPForDeletion("blocked-vtep")

			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "blocked-vtep")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete the CUDN — the VTEP should now be garbage-collected
			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "test-cudn", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() bool {
				_, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "blocked-vtep", metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
		})
	})

	ginkgo.Context("Cross-VTEP CIDR overlap validation", func() {
		ginkgo.It("sets Accepted=True when VTEPs have non-overlapping CIDRs", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("sets Accepted=False on both VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-b")),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-a")),
			))
		})

		ginkgo.It("converges without infinite re-queue loop when VTEPs overlap", func() {
			// Overlapping VTEPs re-queue each other in validateCIDRsAcrossVTEPs,
			// so this test asserts two convergence properties once both settle:
			//
			//  1. Reconciles stop. The number of reconciles is the direct signal
			//     for loop termination: an infinite ping-pong would keep the
			//     counter climbing forever. We wrap reconcileVTEP with an
			//     invocation counter for this - counting reconciles rather than
			//     status writes is what actually catches the loop, because once
			//     Accepted=False/CIDROverlap is set, updateStatusCondition skips
			//     the API write, so a no-op re-queue loop would spin without
			//     producing any status writes to observe.
			//
			//  2. Status writes stop. This guards against redundant ApplyStatus
			//     calls (API spam) while the two VTEPs re-queue each other. We
			//     count only status-subresource writes (via countStatusWrites)
			//     so the test's own condition-polling get calls and the finalizer
			//     Apply in ensureFinalizer (which can fire more than once per
			//     VTEP while the lister cache lags) don't skew the total.
			//
			// Neither count is deterministic: with no nodes present, a VTEP that
			// reconciles before the other is visible in the lister briefly writes
			// Accepted=True before flipping to False once the overlap is seen. So
			// we first wait for both counts to quiesce (two consecutive equal
			// readings) before asserting they stay constant. A genuine re-queue
			// loop would never quiesce, so the Eventually would time out.
			var reconcileCount atomic.Int64

			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			startWithReconcile(func(ctx context.Context, key string) error {
				reconcileCount.Add(1)
				return controller.reconcileVTEP(ctx, key)
			}, vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			var reconcileSettled int64
			var statusSettled int
			gomega.Eventually(func() bool {
				reconciles := reconcileCount.Load()
				statuses := countStatusWrites(fakeVTEP)
				if reconciles > reconcileSettled || statuses > statusSettled {
					// Log each burst so a run that fails to quiesce leaves a
					// trail showing the counts still climbing.
					fmt.Fprintf(ginkgo.GinkgoWriter, "not yet settled: reconciles %d -> %d, status writes %d -> %d\n",
						reconcileSettled, reconciles, statusSettled, statuses)
				}
				stable := reconciles == reconcileSettled && statuses == statusSettled
				reconcileSettled = reconciles
				statusSettled = statuses
				return stable
			}).WithTimeout(5 * time.Second).WithPolling(500 * time.Millisecond).Should(gomega.BeTrue())

			gomega.Consistently(func() int64 {
				return reconcileCount.Load()
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(reconcileSettled))

			gomega.Consistently(func() int {
				return countStatusWrites(fakeVTEP)
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(statusSettled))
		})

		ginkgo.It("emits a CIDROverlap warning event when VTEPs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonCIDROverlap),
			))
		})

		ginkgo.It("sets Accepted=False on all three VTEPs when CIDRs overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionFalse),
					gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				))
			}
		})

		ginkgo.It("updates conflict message when a new overlapping VTEP joins an existing conflict", func() {
			// vtep-b and vtep-c overlap via vtep-b's /8
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepB, vtepC)

			// vtep-b's message should mention vtep-c but not vtep-a (doesn't exist yet)
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
				gomega.HaveField("Message", gomega.ContainSubstring("vtep-c")),
				gomega.HaveField("Message", gomega.Not(gomega.ContainSubstring("vtep-a"))),
			))

			// Create vtep-a which also overlaps with vtep-b
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepA, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// vtep-b's message should now mention both vtep-a and vtep-c
			gomega.Eventually(func() (string, error) {
				cond, err := getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
				if err != nil {
					return "", err
				}
				if cond == nil {
					return "", nil
				}
				return cond.Message, nil
			}).WithTimeout(5 * time.Second).Should(gomega.And(
				gomega.ContainSubstring("vtep-a"),
				gomega.ContainSubstring("vtep-c"),
			))
		})

		ginkgo.It("sets Accepted=False when a new overlapping VTEP is created after startup", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			start(vtepA)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Create an overlapping VTEP
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			_, err := fakeVTEP.K8sV1().VTEPs().Create(context.Background(), vtepB, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a mask expansion causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Expand vtep-a's mask from /24 to /16, now it contains 10.0.1.0/24
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("sets Accepted=False on both when a newly appended CIDR causes overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Append a new CIDR to vtep-a that overlaps with vtep-b
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = append(v.Spec.CIDRs, "10.1.0.0/16")
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both should eventually be Accepted=False
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
		})

		ginkgo.It("clears Accepted=False when overlapping CIDR is removed from the list", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/24", "10.1.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Remove the overlapping CIDR from vtep-a, keeping only the non-overlapping one
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"10.0.0.0/24"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("clears Accepted=False when user edits CIDRs to remove overlap", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Change vtep-a's CIDR so it no longer overlaps with vtep-b
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"192.168.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("clears Accepted=False only when all conflicts are resolved", func() {
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.1.0.0/24")
			vtepC := newVTEP("vtep-c", vtepv1.VTEPModeUnmanaged, "10.2.0.0/24")
			start(vtepA, vtepB, vtepC)

			// All three should be Accepted=False due to overlap
			for _, name := range []string{"vtep-a", "vtep-b", "vtep-c"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))
			}

			// Delete vtep-c: vtep-a and vtep-b still overlap, both stay Accepted=False
			markVTEPForDeletion("vtep-c")

			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(2 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			// Delete vtep-b: vtep-a is the only one left, no more conflicts
			markVTEPForDeletion("vtep-b")

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("Node VTEP IP validation", func() {
		ginkgo.It("sets Accepted=True when a node has a VTEP IP in the annotation", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{"vtep-discover": {"100.64.0.5"}})
			vtep := newVTEP("vtep-discover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-discover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False when a node has no vteps annotation", func() {
			nodeNoAnnotation := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "bare-node"},
			}
			vtep := newVTEP("vtep-skip", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeNoAnnotation)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-skip", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("sets Accepted=False when a node has no entry for this VTEP", func() {
			node := newNodeWithVTEPAnnotation("node-nomatch", map[string][]string{"other-vtep": {"10.0.0.1"}})
			vtep := newVTEP("vtep-nomatch", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
				gomega.HaveField("Message", gomega.ContainSubstring("node-nomatch")),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("sets Accepted=False when a node has an empty IP list for the VTEP", func() {
			node := newNodeWithVTEPAnnotation("node-empty", map[string][]string{"vtep-empty": {}})
			vtep := newVTEP("vtep-empty", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-empty", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonAllocationFailed),
			))
		})

		ginkgo.It("does not fire duplicate events when failure state is unchanged", func() {
			// Test both AllocationFailed and CIDROverlap dedup in a single scenario:
			// vtep-a and vtep-b overlap, and vtep-a also has a missing node annotation.
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-b": {"10.0.0.1"}})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "10.0.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.1.0/24")
			start(vtepA, vtepB, node)

			// vtep-a should be Accepted=False due to CIDR overlap (checked before node validation)
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
			))

			// vtep-b should also be Accepted=False due to CIDR overlap
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonCIDROverlap)),
			))

			// Drain all events from initial convergence. We expect one
			// CIDROverlap event per VTEP (2 total), but a 3rd is possible:
			//  1. vtep-a reconciles first → overlap → event #1 → patches status
			//  2. vtep-b reconciles → overlap → event #2 → patches status.
			//     If the informer cache hasn't synced vtep-a's status yet,
			//     validateCIDRsAcrossVTEPs re-queues vtep-a.
			//  3. vtep-a re-reconciles → lister.Get still returns the stale
			//     object (no conditions) → dedup guard sees existingCond==nil
			//     → fires event #3 (duplicate).
			// This is a benign race between the worker and the async informer
			// cache sync. In steady state the guard works correctly.
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			// Re-reconcile both — no new events since the lister is now in sync
			controller.vtepController.Reconcile("vtep-a")
			controller.vtepController.Reconcile("vtep-b")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())

			// Resolve the overlap by changing vtep-a's CIDRs to non-overlapping
			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-a", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"192.168.0.0/16"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// vtep-b should recover to Accepted=True now that the overlap is gone
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// vtep-a should now fail with AllocationFailed (node has no entry for it)
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Drain AllocationFailed events. We expect 1, but a 2nd is
			// possible: vtep-b's re-queue (from the else-if conflict-resolved
			// path) may re-queue vtep-a while the lister still shows the old
			// CIDROverlap condition, causing the dedup guard to miss the match.
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			// Re-reconcile vtep-a — no new event since AllocationFailed state is unchanged
			controller.vtepController.Reconcile("vtep-a")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())
		})

		ginkgo.It("sets Accepted=True when multiple nodes have VTEP IPs", func() {
			node1 := newNodeWithVTEPAnnotation("node-1", map[string][]string{"vtep-multi": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-2", map[string][]string{"vtep-multi": {"100.64.0.2"}})
			node3 := newNodeWithVTEPAnnotation("node-3", map[string][]string{"vtep-multi": {"100.64.0.3"}})
			vtep := newVTEP("vtep-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-multi", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False when some nodes are missing the VTEP entry", func() {
			nodeGood := newNodeWithVTEPAnnotation("node-good", map[string][]string{"vtep-partial": {"100.64.0.1"}})
			nodeBad := newNodeWithVTEPAnnotation("node-bad", map[string][]string{"other-vtep": {"10.0.0.1"}})
			vtep := newVTEP("vtep-partial", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, nodeGood, nodeBad)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-partial", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("sets Accepted=True for both VTEPs when a node has entries for multiple VTEPs", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"vtep-a": {"100.64.0.1"},
				"vtep-b": {"10.0.0.1"},
			})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			start(vtepA, vtepB, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("sets Accepted=False for one VTEP when node only has the other VTEP entry", func() {
			node := newNodeWithVTEPAnnotation("node-1", map[string][]string{
				"vtep-a": {"100.64.0.1"},
			})
			vtepA := newVTEP("vtep-a", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b", vtepv1.VTEPModeUnmanaged, "10.0.0.0/8")
			start(vtepA, vtepB, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-a", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-b", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))
		})

		ginkgo.It("recovers from AllocationFailed when VTEP CIDRs are expanded to match node IPs", func() {
			node := newNodeWithVTEPAnnotation("node-expand", map[string][]string{"vtep-expand": {"200.10.0.5"}})
			vtep := newVTEP("vtep-expand", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-expand", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("IPv6 CIDR rejection for EVPN VTEPs", func() {
		ginkgo.It("sets Accepted=False when an EVPN CUDN references a VTEP with IPv6 CIDRs", func() {
			vtep := newVTEP("vtep-v6", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6": {"fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-v6", "vtep-v6")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			gomega.Eventually(fakeRecorder.Events).Should(gomega.Receive(
				gomega.ContainSubstring(reasonEVPNIPv6NotSupported),
			))

			// Drain any remaining events, then verify dedup guard prevents duplicates
			gomega.Eventually(func() bool {
				select {
				case <-fakeRecorder.Events:
					return false
				default:
					return true
				}
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())

			controller.vtepController.Reconcile("vtep-v6")
			gomega.Consistently(fakeRecorder.Events).WithTimeout(2 * time.Second).ShouldNot(gomega.Receive())
		})

		ginkgo.It("sets Accepted=True when a VTEP has only IPv4 CIDRs and is referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-v4", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v4": {"100.64.0.1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-v4", "vtep-v4")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("allows IPv6 CIDRs on VTEPs not referenced by any EVPN CUDN", func() {
			vtep := newVTEP("vtep-v6-no-evpn", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-no-evpn": {"fd00::1"}})
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-no-evpn", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("rejects when VTEP has dual-stack CIDRs and is referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-ds", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24", "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-ds": {"100.64.0.1", "fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-ds", "vtep-ds")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		ginkgo.It("transitions to IPv6NotSupported when an EVPN CUDN is created referencing a VTEP with IPv6 CIDRs", func() {
			vtep := newVTEP("vtep-v6-late", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-late": {"fd00::1"}})
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-late", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			cudn := newCUDNWithEVPN("cudn-evpn-late", "vtep-v6-late")
			_, err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Create(
				context.Background(), cudn, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-late", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		ginkgo.It("transitions to IPv6NotSupported when an IPv6 CIDR is appended to a VTEP referenced by an EVPN CUDN", func() {
			vtep := newVTEP("vtep-v4-append", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v4-append": {"100.64.0.1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-append", "vtep-v4-append")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4-append", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-v4-append", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"100.64.0.0/24", "fd00::/64"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v4-append", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))
		})

		ginkgo.It("recovers from IPv6NotSupported when the IPv6 CIDR is removed from the VTEP", func() {
			vtep := newVTEP("vtep-ds-remove", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24", "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-ds-remove": {"100.64.0.1", "fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-remove", "vtep-ds-remove")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds-remove", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			v, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-ds-remove", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			v.Spec.CIDRs = []vtepv1.CIDR{"100.64.0.0/24"}
			_, err = fakeVTEP.K8sV1().VTEPs().Update(context.Background(), v, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-ds-remove", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("recovers from IPv6NotSupported when the EVPN CUDN is deleted", func() {
			vtep := newVTEP("vtep-v6-recover", vtepv1.VTEPModeUnmanaged, "fd00::/64")
			node := newNodeWithVTEPAnnotation("node1", map[string][]string{"vtep-v6-recover": {"fd00::1"}})
			cudn := newCUDNWithEVPN("cudn-evpn-recover", "vtep-v6-recover")
			start(vtep, node, cudn)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonEVPNIPv6NotSupported)),
			))

			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-evpn-recover", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-v6-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("CUDN watch for finalizer re-evaluation", func() {
		ginkgo.It("unblocks VTEP deletion when the referencing CUDN is deleted", func() {
			cudn := newCUDNWithEVPN("cudn-ref", "vtep-cudn-del")
			vtep := newVTEP("vtep-cudn-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Simulate deletion of the VTEP (sets DeletionTimestamp, blocked by finalizer)
			markVTEPForDeletion("vtep-cudn-del")

			// Finalizer should remain because CUDN still references the VTEP
			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-cudn-del")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete the CUDN -- this should trigger the CUDN controller
			// which re-queues the deleting VTEP
			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-ref", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Now the VTEP's finalizer should be removed and the object
			// should be garbage-collected (the GC reactor deletes objects
			// whose DeletionTimestamp is set and finalizers are empty).
			gomega.Eventually(func() bool {
				_, err := fakeVTEP.K8sV1().VTEPs().Get(context.Background(), "vtep-cudn-del", metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}).WithTimeout(5 * time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("keeps VTEP blocked until all referencing CUDNs are deleted", func() {
			cudn1 := newCUDNWithEVPN("cudn-one", "vtep-multi-ref")
			cudn2 := newCUDNWithEVPN("cudn-two", "vtep-multi-ref")
			vtep := newVTEP("vtep-multi-ref", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, cudn1, cudn2)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			markVTEPForDeletion("vtep-multi-ref")

			// Delete first CUDN -- VTEP should still be blocked by cudn-two
			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-one", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Delete second CUDN -- now VTEP should be unblocked
			err = fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-two", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-multi-ref")
			}).WithTimeout(5 * time.Second).ShouldNot(gomega.ContainElement(finalizerVTEP))
		})

		ginkgo.It("does not re-queue VTEPs when a non-EVPN CUDN is deleted", func() {
			evpnCUDN := newCUDNWithEVPN("cudn-evpn", "vtep-norequeue")
			nonEVPNCUDN := &udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{Name: "cudn-plain"},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					NamespaceSelector: metav1.LabelSelector{},
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Subnets: []udnv1.Layer3Subnet{{CIDR: "10.0.0.0/16"}},
						},
					},
				},
			}
			vtep := newVTEP("vtep-norequeue", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep, evpnCUDN, nonEVPNCUDN)

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			// Request VTEP deletion
			markVTEPForDeletion("vtep-norequeue")

			// Delete the non-EVPN CUDN -- should NOT unblock the VTEP
			err := fakeClientset.UserDefinedNetworkClient.K8sV1().ClusterUserDefinedNetworks().Delete(
				context.Background(), "cudn-plain", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain blocked (the EVPN CUDN still references it)
			gomega.Consistently(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "vtep-norequeue")
			}).WithTimeout(3 * time.Second).Should(gomega.ContainElement(finalizerVTEP))
		})

	})

	ginkgo.Context("Node watch for VTEP IP re-validation", func() {
		ginkgo.It("re-validates VTEP when node's vteps annotation is added", func() {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-late"}}
			vtep := newVTEP("vtep-node-add", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			// Node has no vteps annotation yet — VTEP should be AllocationFailed
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-add", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Simulate ovnkube-node writing the vteps annotation
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-late", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-node-add": {IPs: []string{"100.64.0.10"}},
			})
			n.Annotations = map[string]string{util.OVNNodeVTEPs: string(vtepAnnotation)}
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Node controller detects vteps annotation change, re-queues VTEP
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-add", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("re-validates VTEP when node's vteps annotation changes", func() {
			node := newNodeWithVTEPAnnotation("node-change", map[string][]string{"vtep-node-change": {"100.64.0.1"}})
			vtep := newVTEP("vtep-node-change", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-change", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Change the node's vteps annotation to a different IP
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-change", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-node-change": {IPs: []string{"100.64.0.99"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True after re-validation
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-change", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("re-validates VTEP when a node is deleted", func() {
			node1 := newNodeWithVTEPAnnotation("node-keep", map[string][]string{"vtep-node-del": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-remove", map[string][]string{"vtep-node-del": {"100.64.0.2"}})
			vtep := newVTEP("vtep-node-del", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-del", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Delete node-remove
			err := fakeClientset.KubeClient.CoreV1().Nodes().Delete(
				context.Background(), "node-remove", metav1.DeleteOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True (node-keep still has a valid entry)
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-node-del", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("recovers from AllocationFailed when missing VTEP entry is added to node", func() {
			// Node starts without an entry for this VTEP
			node := newNodeWithVTEPAnnotation("node-nomatch", map[string][]string{"other-vtep": {"192.168.1.10"}})
			vtep := newVTEP("vtep-nomatch-fix", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: add the VTEP entry
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-nomatch", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"other-vtep":       {IPs: []string{"192.168.1.10"}},
				"vtep-nomatch-fix": {IPs: []string{"100.64.0.20"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-nomatch-fix", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("recovers from AllocationFailed when empty IP list is populated", func() {
			// Node starts with an empty IP list for this VTEP
			node := newNodeWithVTEPAnnotation("node-empty-fix", map[string][]string{"vtep-recover": {}})
			vtep := newVTEP("vtep-recover", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
			))

			// Fix the node: populate the IP list
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-empty-fix", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-recover": {IPs: []string{"100.64.0.5"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-recover", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})

		ginkgo.It("transitions to AllocationFailed when VTEP entry is removed from node", func() {
			node := newNodeWithVTEPAnnotation("node-regress", map[string][]string{"vtep-regress": {"100.64.0.5"}})
			vtep := newVTEP("vtep-regress", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-regress", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Remove the VTEP entry from the node annotation
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-regress", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"other-vtep": {IPs: []string{"192.168.1.10"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-regress", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionFalse),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocationFailed)),
				gomega.HaveField("Message", gomega.ContainSubstring("node-regress")),
			))
		})

		ginkgo.It("does not issue any VTEP API update when vteps annotation change is unrelated", func() {
			node := newNodeWithVTEPAnnotation("node-stable", map[string][]string{"vtep-stable": {"100.64.0.1"}})
			vtep := newVTEP("vtep-stable", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-stable", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Controller is idle after initial reconcile settled; safe to add reactor.
			var patchCount atomic.Int32
			fakeVTEP.PrependReactor("patch", "vteps", func(_ ktesting.Action) (bool, runtime.Object, error) {
				patchCount.Add(1)
				return false, nil, nil
			})

			// Update node: add a different VTEP entry (vtep-stable unchanged)
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-stable", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-stable":  {IPs: []string{"100.64.0.1"}},
				"vtep-another": {IPs: []string{"10.0.0.50"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// The diff guards should prevent any VTEP status API calls
			gomega.Consistently(func() int32 {
				return patchCount.Load()
			}).WithTimeout(3 * time.Second).Should(gomega.Equal(int32(0)))
		})

		ginkgo.It("reconciles multiple VTEPs when a single node's vteps annotation changes", func() {
			node := newNodeWithVTEPAnnotation("node-shared", map[string][]string{
				"vtep-a-multi": {"100.64.0.1"},
				"vtep-b-multi": {"200.10.0.1"},
			})
			vtepA := newVTEP("vtep-a-multi", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			vtepB := newVTEP("vtep-b-multi", vtepv1.VTEPModeUnmanaged, "200.10.0.0/16")
			start(vtepA, vtepB, node)

			for _, name := range []string{"vtep-a-multi", "vtep-b-multi"} {
				gomega.Eventually(func() (*metav1.Condition, error) {
					return getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
				}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
					gomega.HaveField("Status", metav1.ConditionTrue),
					gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
				))
			}

			// Change the node IPs for both VTEPs
			n, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-shared", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-a-multi": {IPs: []string{"100.64.0.99"}},
				"vtep-b-multi": {IPs: []string{"200.10.0.99"}},
			})
			n.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Both VTEPs should remain Accepted after re-validation
			gomega.Consistently(func() bool {
				for _, name := range []string{"vtep-a-multi", "vtep-b-multi"} {
					cond, err := getVTEPCondition(fakeVTEP, name, conditionTypeAccepted)
					if err != nil || cond == nil || cond.Status != metav1.ConditionTrue {
						return false
					}
				}
				return true
			}).WithTimeout(3 * time.Second).Should(gomega.BeTrue())
		})

		ginkgo.It("validates a dynamically created node with vteps annotation already set", func() {
			vtep := newVTEP("vtep-dynnode", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep)

			// No nodes exist yet — VTEP should be Accepted
			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-dynnode", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))

			// Dynamically create a node that already has the vteps annotation
			node := newNodeWithVTEPAnnotation("node-late", map[string][]string{"vtep-dynnode": {"100.64.0.77"}})
			_, err := fakeClientset.KubeClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// VTEP should remain Accepted=True after the new node is validated
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-dynnode", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))
		})

		ginkgo.It("re-validates VTEP when one of many nodes changes vteps annotation", func() {
			node1 := newNodeWithVTEPAnnotation("node-m1", map[string][]string{"vtep-mnodes": {"100.64.0.1"}})
			node2 := newNodeWithVTEPAnnotation("node-m2", map[string][]string{"vtep-mnodes": {"100.64.0.2"}})
			node3 := newNodeWithVTEPAnnotation("node-m3", map[string][]string{"vtep-mnodes": {"100.64.0.3"}})
			vtep := newVTEP("vtep-mnodes", vtepv1.VTEPModeUnmanaged, "100.64.0.0/16")
			start(vtep, node1, node2, node3)

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-mnodes", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			// Change only node-m2's VTEP IP
			n2, err := fakeClientset.KubeClient.CoreV1().Nodes().Get(context.Background(), "node-m2", metav1.GetOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			vtepAnnotation, _ := json.Marshal(map[string]util.VTEPNodeAnnotation{
				"vtep-mnodes": {IPs: []string{"100.64.0.22"}},
			})
			n2.Annotations[util.OVNNodeVTEPs] = string(vtepAnnotation)
			_, err = fakeClientset.KubeClient.CoreV1().Nodes().Update(context.Background(), n2, metav1.UpdateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Condition should remain Accepted=True throughout
			gomega.Consistently(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "vtep-mnodes", conditionTypeAccepted)
			}).WithTimeout(3 * time.Second).Should(gomega.SatisfyAll(
				gomega.HaveField("Status", metav1.ConditionTrue),
				gomega.HaveField("Reason", gomega.Equal(reasonAllocated)),
			))
		})
	})

	ginkgo.Context("VTEP condition metrics", func() {
		ginkgo.It("should record Accepted=True condition metric for an Unmanaged VTEP", func() {
			vtep := newVTEP("metrics-unmanaged", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)
			ginkgo.DeferCleanup(metrics.DeleteVTEPCondition, "metrics-unmanaged")

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "metrics-unmanaged", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionTrue))

			gomega.Eventually(func(g gomega.Gomega) {
				valTrue, found := getVTEPConditionMetricValue("metrics-unmanaged", "Accepted", "true")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=true should exist")
				g.Expect(valTrue).To(gomega.Equal(1.0))
				valFalse, found := getVTEPConditionMetricValue("metrics-unmanaged", "Accepted", "false")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(gomega.Equal(0.0))
			}).Should(gomega.Succeed(), "Accepted condition metrics should be recorded")
		})

		ginkgo.It("should record Accepted=False condition metric for a Managed VTEP", func() {
			vtep := newVTEP("metrics-managed", vtepv1.VTEPModeManaged, "100.64.0.0/24")
			start(vtep)
			ginkgo.DeferCleanup(metrics.DeleteVTEPCondition, "metrics-managed")

			gomega.Eventually(func() (*metav1.Condition, error) {
				return getVTEPCondition(fakeVTEP, "metrics-managed", conditionTypeAccepted)
			}).WithTimeout(5 * time.Second).Should(gomega.HaveField("Status", metav1.ConditionFalse))

			gomega.Eventually(func(g gomega.Gomega) {
				valTrue, found := getVTEPConditionMetricValue("metrics-managed", "Accepted", "true")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=true should exist")
				g.Expect(valTrue).To(gomega.Equal(0.0))
				valFalse, found := getVTEPConditionMetricValue("metrics-managed", "Accepted", "false")
				g.Expect(found).To(gomega.BeTrue(), "condition metric status=false should exist")
				g.Expect(valFalse).To(gomega.Equal(1.0))
			}).Should(gomega.Succeed(), "Accepted condition metrics should be recorded")
		})

		ginkgo.It("should remove condition metrics on VTEP deletion", func() {
			vtep := newVTEP("metrics-delete", vtepv1.VTEPModeUnmanaged, "100.64.0.0/24")
			start(vtep)

			gomega.Eventually(func(g gomega.Gomega) {
				_, found := getVTEPConditionMetricValue("metrics-delete", "Accepted", "true")
				g.Expect(found).To(gomega.BeTrue(), "condition metric should exist before deletion")
			}).Should(gomega.Succeed())

			gomega.Eventually(func() ([]string, error) {
				return getVTEPFinalizers(fakeVTEP, "metrics-delete")
			}).WithTimeout(5 * time.Second).Should(gomega.ContainElement(finalizerVTEP))

			markVTEPForDeletion("metrics-delete")

			gomega.Eventually(func() bool {
				_, foundTrue := getVTEPConditionMetricValue("metrics-delete", "Accepted", "true")
				_, foundFalse := getVTEPConditionMetricValue("metrics-delete", "Accepted", "false")
				return foundTrue || foundFalse
			}).Should(gomega.BeFalse(), "condition metrics should be removed after deletion")
		})
	})
})

var _ = ginkgo.Describe("vtepNameInMessage", func() {
	ginkgo.It("returns false for empty brackets", func() {
		gomega.Expect(vtepNameInMessage("CIDRs overlap with VTEPs: []", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("returns false when no brackets present", func() {
		gomega.Expect(vtepNameInMessage("no brackets here", "vtep-a")).To(gomega.BeFalse())
	})

	ginkgo.It("matches single entry", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in two entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeFalse())
	})

	ginkgo.It("matches in three entries", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-a, vtep-b, vtep-c]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-c")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-d")).To(gomega.BeFalse())
	})

	ginkgo.It("does not substring match", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match in multi-entry list", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-aa, vtep-bb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-a")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-aa")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
	})

	ginkgo.It("does not substring match vtep-b against vtep-bb and vtep-bbb", func() {
		msg := "CIDRs overlap with VTEPs: [vtep-b, vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bb, vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeTrue())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())

		msg = "CIDRs overlap with VTEPs: [vtep-bbb]"
		gomega.Expect(vtepNameInMessage(msg, "vtep-b")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bb")).To(gomega.BeFalse())
		gomega.Expect(vtepNameInMessage(msg, "vtep-bbb")).To(gomega.BeTrue())
	})
})

func getVTEPConditionMetricValue(nameLabel, conditionLabel, statusLabel string) (float64, bool) {
	metricName := prometheus.BuildFQName(ovntypes.MetricOvnkubeNamespace, ovntypes.MetricOvnkubeSubsystemClusterManager, "vtep_condition")
	return ovntest.GetConditionMetricValue(metricName, nameLabel, conditionLabel, statusLabel)
}
