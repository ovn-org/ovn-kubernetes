// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"fmt"
	"time"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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

	// Pods for hostname mismatch scenario (vm.kubevirt.io/name label != kubevirt.io/domain annotation)
	hostnameVMName := "real-vm-name"
	hostname := "shared-hostname"
	runningKvSourcePodWithHostname := newKubevirtPodWithHostname(corev1.PodRunning, t0, hostnameVMName, hostname)
	duringMigrationKvTargetPodWithHostname := newKubevirtPodWithHostname(corev1.PodRunning, t4, hostnameVMName, hostname)

	// Pods for long VM name scenario (label truncated to 63 chars)
	longVMName := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters"
	runningKvSourcePodWithLongName := newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName)
	duringMigrationKvTargetPodWithLongName := newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName)

	// Pods for colliding long VM names scenario (different VMs share the same truncated 63-char label)
	collidingLongVMName := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-collider"
	collidingVMPod := newKubevirtPodWithLongName(corev1.PodRunning, t2, collidingLongVMName)

	initWatchFactory := func(pods []corev1.Pod) (*factory.WatchFactory, func()) {
		Expect(config.PrepareTestConfig()).To(Succeed())
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		config.OVNKubernetesFeature.EnableMultiNetwork = true

		fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
		wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient, "test-node")
		Expect(err).ToNot(HaveOccurred())

		for _, pod := range pods {
			_, err := fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Create(context.Background(), &pod, metav1.CreateOptions{})
			Expect(err).ToNot(HaveOccurred())
		}

		Expect(wf.Start()).To(Succeed())
		return wf, wf.Shutdown
	}

	type testParams struct {
		pods                    []corev1.Pod
		expectedError           error
		expectedMigrationStatus *LiveMigrationStatus
	}
	DescribeTable("DiscoverLiveMigrationStatus", func(params testParams) {
		wf, shutdown := initWatchFactory(params.pods)
		defer shutdown()

		currentPod := params.pods[0]
		migrationStatus, err := DiscoverLiveMigrationStatus(wf.PodCoreInformer().Lister(), &currentPod)
		if params.expectedError == nil {
			Expect(err).ToNot(HaveOccurred())
		} else {
			Expect(err).To(MatchError(ContainSubstring(params.expectedError.Error())))
		}

		if params.expectedMigrationStatus == nil {
			Expect(migrationStatus).To(BeNil())
		} else {
			Expect(migrationStatus.State).To(Equal(params.expectedMigrationStatus.State))
			Expect(migrationStatus.TargetPod.Name).To(Equal(params.expectedMigrationStatus.TargetPod.Name))
			if params.expectedMigrationStatus.SourcePod == nil {
				Expect(migrationStatus.SourcePod).To(BeNil())
			} else {
				Expect(migrationStatus.SourcePod.Name).To(Equal(params.expectedMigrationStatus.SourcePod.Name))
			}
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
		Entry("returns nil when there is all the pods are completed (not running vm after migration)",
			testParams{
				pods: []corev1.Pod{completedKubevirtPod(t0), completedKubevirtPod(t1), completedKubevirtPod(t3)},
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
		Entry("returns Migration Ready status when source pod is gone and target has ready annotation",
			testParams{
				pods: []corev1.Pod{readyMigrationKvTargetPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns Migration Ready status when source pod is completed and target has ready annotation",
			testParams{
				pods: []corev1.Pod{readyMigrationKvTargetPod, successfullyMigratedKvSourcePod},
				expectedMigrationStatus: &LiveMigrationStatus{
					TargetPod: &readyMigrationKvTargetPod,
					State:     LiveMigrationTargetDomainReady,
				},
			},
		),
		Entry("returns nil when source pod is gone and target has no ready annotation",
			testParams{
				pods: []corev1.Pod{duringMigrationKvTargetPod},
			},
		),
		Entry("returns err when kubevirt VM has several living pods",
			testParams{
				pods:          []corev1.Pod{runningKvSourcePod, duringMigrationKvTargetPod, yetAnotherDuringMigrationKvTargetPod},
				expectedError: fmt.Errorf("unexpected live migration state at pods"),
			},
		),
		Entry("discovers migration when VM has hostname configured (vmName label != domain annotation)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithHostname, duringMigrationKvTargetPodWithHostname},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithHostname,
					TargetPod: &duringMigrationKvTargetPodWithHostname,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("discovers migration when VM name exceeds 63 chars (vmName label truncated)",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithLongName, duringMigrationKvTargetPodWithLongName},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithLongName,
					TargetPod: &duringMigrationKvTargetPodWithLongName,
					State:     LiveMigrationInProgress,
				},
			},
		),
		Entry("discovers migration when another VM has colliding truncated vmName label",
			testParams{
				pods: []corev1.Pod{runningKvSourcePodWithLongName, duringMigrationKvTargetPodWithLongName, collidingVMPod},
				expectedMigrationStatus: &LiveMigrationStatus{
					SourcePod: &runningKvSourcePodWithLongName,
					TargetPod: &duringMigrationKvTargetPodWithLongName,
					State:     LiveMigrationInProgress,
				},
			},
		),
	)

	Describe("findPodAnnotation", func() {
		It("allows the oldest VM pod to allocate when no related pod is annotated", func() {
			sourcePod := runningKvSourcePod
			targetPod := duringMigrationKvTargetPod
			wf, shutdown := initWatchFactory([]corev1.Pod{sourcePod, targetPod})
			defer shutdown()

			podAnnotation, err := findPodAnnotation(wf, &sourcePod, ovntypes.DefaultNetworkName)
			Expect(err).ToNot(HaveOccurred())
			Expect(podAnnotation).To(BeNil())
		})

		It("returns an error for newer VM pods until an older pod is annotated", func() {
			sourcePod := runningKvSourcePod
			targetPod := duringMigrationKvTargetPod
			wf, shutdown := initWatchFactory([]corev1.Pod{sourcePod, targetPod})
			defer shutdown()

			podAnnotation, err := findPodAnnotation(wf, &targetPod, ovntypes.DefaultNetworkName)
			Expect(err).To(MatchError(ContainSubstring("missing virtual machine pod annotation")))
			Expect(podAnnotation).To(BeNil())
		})
	})

	Describe("NewVMDescriptionFromPod", func() {
		It("returns nil for non-kubevirt pod", func() {
			pod := nonKubevirtPod()
			desc, err := NewVMDescriptionFromPod(&pod)
			Expect(err).ToNot(HaveOccurred())
			Expect(desc).To(BeNil())
		})

		It("returns correct key and finds owned pods for standard pod (vmName label == domain)", func() {
			sourcePod := runningKvSourcePod
			targetPod := duringMigrationKvTargetPod

			wf, shutdown := initWatchFactory([]corev1.Pod{sourcePod, targetPod, nonKubevirtPod()})
			defer shutdown()

			desc, err := NewVMDescriptionFromPod(&sourcePod)
			Expect(err).ToNot(HaveOccurred())
			Expect(desc).ToNot(BeNil())
			Expect(desc.Key().Name).To(Equal(vmName))

			pods, err := desc.OwnedPods(wf.PodCoreInformer().Lister())
			Expect(err).ToNot(HaveOccurred())
			Expect(pods).To(HaveLen(2))
			for _, p := range pods {
				Expect(p.Annotations[kubevirtv1.DomainAnnotation]).To(Equal(vmName))
			}
		})

		type vmDescriptionParams struct {
			pods           []corev1.Pod
			expectedVMName string
			expectedPods   int
		}
		DescribeTable("returns correct key and finds only owned pods",
			func(params vmDescriptionParams) {
				allPods := append(params.pods, nonKubevirtPod())
				wf, shutdown := initWatchFactory(allPods)
				defer shutdown()

				sourcePod := params.pods[0]
				desc, err := NewVMDescriptionFromPod(&sourcePod)
				Expect(err).ToNot(HaveOccurred())
				Expect(desc).ToNot(BeNil())
				Expect(desc.Key().Name).To(Equal(params.expectedVMName))

				pods, err := desc.OwnedPods(wf.PodCoreInformer().Lister())
				Expect(err).ToNot(HaveOccurred())
				Expect(pods).To(HaveLen(params.expectedPods))
				for _, p := range pods {
					Expect(p.Annotations[kubevirtv1.DomainAnnotation]).To(Equal(params.expectedVMName))
				}
			},
			Entry("when VM has hostname (vmName label != domain annotation)",
				vmDescriptionParams{
					pods: []corev1.Pod{
						newKubevirtPodWithHostname(corev1.PodRunning, t0, hostnameVMName, hostname),
						newKubevirtPodWithHostname(corev1.PodRunning, t4, hostnameVMName, hostname),
						newKubevirtPodWithHostname(corev1.PodRunning, t2, "other-vm", hostname),
					},
					expectedVMName: hostnameVMName,
					expectedPods:   2,
				},
			),
			Entry("when VM name exceeds 63 chars (truncated vmName label)",
				vmDescriptionParams{
					pods: []corev1.Pod{
						newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName),
						newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName),
					},
					expectedVMName: longVMName,
					expectedPods:   2,
				},
			),
			Entry("when multiple VMs share the same truncated vmName label",
				vmDescriptionParams{
					pods: func() []corev1.Pod {
						longVMName1 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm1"
						longVMName2 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm2"
						longVMName3 := "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm3"
						return []corev1.Pod{
							newKubevirtPodWithLongName(corev1.PodRunning, t0, longVMName1),
							newKubevirtPodWithLongName(corev1.PodRunning, t4, longVMName1),
							newKubevirtPodWithLongName(corev1.PodRunning, t2, longVMName2),
							newKubevirtPodWithLongName(corev1.PodRunning, t3, longVMName3),
						}
					}(),
					expectedVMName: "this-is-a-very-long-virtual-machine-name-that-exceeds-sixty-three-characters-vm1",
					expectedPods:   2,
				},
			),
		)

		It("returns error when domain annotation is missing", func() {
			pod := corev1.Pod{
				TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "virt-launcher-broken",
					Namespace: corev1.NamespaceDefault,
					Labels:    map[string]string{kubevirtv1.AppLabel: "virt-launcher"},
				},
			}
			desc, err := NewVMDescriptionFromPod(&pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("missing the mandatory kubevirt annotation"))
			Expect(desc).To(BeNil())
		})
	})
})

func completedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodSucceeded, creationOffset)
}

func failedKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodFailed, creationOffset)
}

func runningKubevirtPod(creationOffset time.Duration) corev1.Pod {
	return newKubevirtPod(corev1.PodRunning, creationOffset)
}

func domainReadyKubevirtPod(creationOffset time.Duration) corev1.Pod {
	virtLauncherPod := newKubevirtPod(corev1.PodRunning, creationOffset)
	virtLauncherPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = "some-timestamp"
	return virtLauncherPod
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

func newKubevirtPod(phase corev1.PodPhase, creationOffset time.Duration) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + vmName + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: vmName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: vmName},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func newKubevirtPodWithHostname(phase corev1.PodPhase, creationOffset time.Duration, vmName, hostname string) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + vmName + "-" + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: vmName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: hostname},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func newKubevirtPodWithLongName(phase corev1.PodPhase, creationOffset time.Duration, longVMName string) corev1.Pod {
	return corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "virt-launcher-" + longVMName[:40] + "-" + rand.String(5),
			Namespace:         corev1.NamespaceDefault,
			Annotations:       map[string]string{kubevirtv1.DomainAnnotation: longVMName},
			Labels:            map[string]string{kubevirtv1.AppLabel: "virt-launcher", kubevirtv1.VirtualMachineNameLabel: longVMName[:63]},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(creationOffset)},
		},
		Spec: corev1.PodSpec{},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}
