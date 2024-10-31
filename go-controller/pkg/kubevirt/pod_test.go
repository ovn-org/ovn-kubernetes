package kubevirt

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	kubevirtv1 "kubevirt.io/api/core/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const vmName = "test-vm"

var _ = Describe("Kubevirt Pod", func() {
	const (
		t0 = time.Duration(0)
		t1 = time.Duration(1)
		t2 = time.Duration(2)
		t3 = time.Duration(3)
		t4 = time.Duration(4)
	)
	runningKvSourcePod := runningKubevirtPod(t0)
	successfullyMigratedKvSourcePod := completedKubevirtPod(t1)

	failedMigrationKvTargetPod := failedKubevirtPod(t2)
	successfulMigrationKvTargetPod := runningKubevirtPod(t3)
	anotherFailedMigrationKvTargetPod := failedKubevirtPod(t4)
	duringMigrationKvTargetPod := runningKubevirtPod(t4)
	yetAnotherDuringMigrationKvTargetPod := runningKubevirtPod(t4)
	readyMigrationKvTargetPod := domainReadyKubevirtPod(t4)

	type testParams struct {
		pods                    []corev1.Pod
		expectedError           error
		expectedMigrationStatus *LiveMigrationStatus
	}
	DescribeTable("DiscoverLiveMigrationStatus", func(params testParams) {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableInterconnect = true

		fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
		wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
		Expect(err).ToNot(HaveOccurred())

		for _, pod := range params.pods {
			_, err := fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), &pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(wf.Start()).To(Succeed())
		defer wf.Shutdown()

		currentPod := params.pods[0]
		migrationStatus, err := DiscoverLiveMigrationStatus(wf, &currentPod)
		if params.expectedError == nil {
			Expect(err).To(BeNil())
		} else {
			Expect(err).To(MatchError(ContainSubstring(params.expectedError.Error())))
		}

		if params.expectedMigrationStatus == nil {
			Expect(migrationStatus).To(BeNil())
		} else {
			Expect(migrationStatus.State).To(Equal(params.expectedMigrationStatus.State))
			Expect(migrationStatus.SourcePod.Name).To(Equal(params.expectedMigrationStatus.SourcePod.Name))
			Expect(migrationStatus.TargetPod.Name).To(Equal(params.expectedMigrationStatus.TargetPod.Name))
		}
	},
		Entry("returns nil when pod is not kubevirt related",
			testParams{
				pods: []corev1.Pod{nonKubevirtPod()},
			},
		),
		Entry("returns nil when migration was not performed",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod},
			},
		),
		Entry("returns nil when there is no active migration",
			testParams{
				pods: []corev1.Pod{successfullyMigratedKvSourcePod, successfulMigrationKvTargetPod},
			},
		),
		Entry("returns nil when there is no active migration (multiple migrations)",
			testParams{
				pods: []corev1.Pod{successfullyMigratedKvSourcePod, failedMigrationKvTargetPod, successfulMigrationKvTargetPod},
			},
		),
		Entry("returns Migration in progress status when 2 pods are running, target pod is not yet ready",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, duringMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &duringMigrationKvTargetPod,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("returns Migration Failed status when latest target pod failed",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &failedMigrationKvTargetPod,
					State:     LiveMigrationFailed,
				},
			},
		),
		Entry("returns Migration Failed status when latest target pod failed (multiple migrations)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod, anotherFailedMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &anotherFailedMigrationKvTargetPod,
					State:     LiveMigrationFailed,
				},
			},
		),
		Entry("returns Migration Ready status when latest target pod is ready",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns Migration Ready status when latest target pod is ready (multiple migrations)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePod, failedMigrationKvTargetPod, readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePod,
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns err when kubevirt VM has several living pods and target pod failed",
			testParams{
				pods:          []corev1.Pod{runningKvSourcePod, successfulMigrationKvTargetPod, anotherFailedMigrationKvTargetPod},
				expectedError: fmt.Errorf("unexpected live migration state: should have a single living pod"),
			},
		),
		Entry("returns err when kubevirt VM has several living pods",
			testParams{
				pods:          []corev1.Pod{runningKvSourcePod, duringMigrationKvTargetPod, yetAnotherDuringMigrationKvTargetPod},
				expectedError: fmt.Errorf("unexpected live migration state at pods"),
			},
		),
	)
})

func completedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodSucceeded, nil, creationOffset)
}

func failedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodFailed, nil, creationOffset)
}

func runningKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodRunning, nil, creationOffset)
}

func domainReadyKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodRunning, map[string]string{kubevirtv1.MigrationTargetReadyTimestamp: "some-timestamp"}, creationOffset)
}

func nonKubevirtPod() corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-pod",
			Namespace: corev1.NamespaceDefault,
		},
		Spec: corev1.PodSpec{},
	}
}
func newKubevirtPod(phase corev1.PodPhase, annotations map[string]string, creationOffset time.Duration) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + vmName + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       annotations,
			Labels:            map[string]string{kubevirtv1.VirtualMachineNameLabel: vmName},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}
