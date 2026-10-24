// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	frrtypes "github.com/metallb/frr-k8s/api/v1beta1"
	frrclient "github.com/metallb/frr-k8s/pkg/client/clientset/versioned"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

var _ = ginkgo.Describe("BGP: untimed managed fields", feature.RouteAdvertisements, ginkgo.Serial, ginkgo.Label("Serial"), func() {
	f := wrappedTestFramework("untimed-managed-fields")
	ginkgo.It("reconciles an FRRConfiguration with a managed-fields entry without a timestamp", func(ctx ginkgo.SpecContext) {
		const markerLabel = "test.k8s.ovn.org/untimed-manager"
		const untimedManager = "e2e-untimed"
		client, err := frrclient.NewForConfig(f.ClientConfig())
		framework.ExpectNoError(err, "create FRR client")
		configs := client.ApiV1beta1().FRRConfigurations(deploymentconfig.Get().FRRK8sNamespace())
		cfg, err := configs.Create(ctx, &frrtypes.FRRConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "ovnk-generated-untimed-",
				Labels:       map[string]string{markerLabel: f.Namespace.Name},
			},
		}, metav1.CreateOptions{})
		framework.ExpectNoError(err, "create isolated empty FRRConfiguration")
		ginkgo.DeferCleanup(func(ctx context.Context) {
			err := configs.Delete(ctx, cfg.Name, metav1.DeleteOptions{})
			if !apierrors.IsNotFound(err) {
				framework.ExpectNoError(err, "delete test FRRConfiguration")
			}
		})

		restartCounts := func() (map[string]int32, error) {
			pods, err := f.ClientSet.CoreV1().Pods(deploymentconfig.Get().OVNKubernetesNamespace()).List(ctx,
				metav1.ListOptions{LabelSelector: "name=ovnkube-control-plane"})
			if err != nil {
				return nil, err
			}
			counts := map[string]int32{}
			for _, pod := range pods.Items {
				for _, status := range pod.Status.ContainerStatuses {
					if status.Name == "ovnkube-cluster-manager" {
						if !status.Ready {
							return nil, fmt.Errorf("cluster manager in pod %s is not ready", pod.Name)
						}
						counts[string(pod.UID)] = status.RestartCount
					}
				}
			}
			if len(counts) == 0 {
				return nil, fmt.Errorf("no cluster-manager container statuses found")
			}
			return counts, nil
		}
		before, err := restartCounts()
		framework.ExpectNoError(err, "record healthy cluster-manager containers before the event")

		ginkgo.By("adding an untimed manager and marking the configuration as stale generated state")
		// Preserve ownership of an unchanged label, so the API server does not
		// replace this entry with the timestamp of the update below.
		cfg.ManagedFields = append(cfg.ManagedFields, metav1.ManagedFieldsEntry{
			Manager:    untimedManager,
			Operation:  metav1.ManagedFieldsOperationApply,
			APIVersion: "frrk8s.metallb.io/v1beta1",
			FieldsType: "FieldsV1",
			FieldsV1: &metav1.FieldsV1{Raw: []byte(fmt.Sprintf(
				`{"f:metadata":{"f:labels":{"f:%s":{}}}}`, markerLabel))},
		})
		// This unique, nonexistent owner makes cleanup observable without
		// changing any actual BGP sessions or existing advertisements.
		cfg.Labels[types.OvnRouteAdvertisementsKey] = cfg.Name
		cfg.Annotations = map[string]string{types.OvnRouteAdvertisementsKey: cfg.Name + "/template/node"}
		updated, err := configs.Update(ctx, cfg, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "update configuration with an untimed managed-fields entry")
		gomega.Expect(updated.ManagedFields).To(gomega.ContainElement(gomega.SatisfyAll(
			gomega.HaveField("Manager", gomega.Equal(untimedManager)),
			gomega.HaveField("Time", gomega.BeNil()),
		)), "the API response must retain the timestamp-free entry used by this regression")

		ginkgo.By("waiting for the controller to process the event and remove the stale configuration")
		gomega.Eventually(ctx, func() bool {
			_, err := configs.Get(ctx, cfg.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, time.Minute, time.Second).Should(gomega.BeTrue(),
			"the route-advertisements controller must reconcile the untimed entry without panicking")
		gomega.Consistently(ctx, restartCounts, 10*time.Second, time.Second).Should(gomega.Equal(before),
			"cluster-manager pods must remain ready without restarting or being replaced")
	})
})
