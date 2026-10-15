// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestCreateNodeShellDeletesFailedPod(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Name = "node-shell"
		pod.Status.Phase = corev1.PodFailed
		return false, nil, nil
	})

	shell, err := (&kubeNodeAccess{}).createNodeShell(client, "ovn-kubernetes", "node-1", "shell-image")
	if err == nil || shell != nil {
		t.Fatalf("createNodeShell() = %v, %v, want nil shell and error", shell, err)
	}
	pods, err := client.CoreV1().Pods("ovn-kubernetes").List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 0 {
		t.Fatalf("Pods after createNodeShell() = %v, %v, want none", pods.Items, err)
	}
}

func TestNodeShellRetriesFailedCleanup(t *testing.T) {
	client := fake.NewSimpleClientset()
	created := 0
	client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created++
		pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Name = fmt.Sprintf("node-shell-%d", created)
		if created == 1 {
			pod.Status.Phase = corev1.PodFailed
		} else {
			pod.Status.Phase = corev1.PodRunning
		}
		return false, nil, nil
	})
	deleteErr := errors.New("delete failed")
	deletes := 0
	client.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		deletes++
		if deletes == 1 {
			return true, nil, deleteErr
		}
		return false, nil, nil
	})
	provider := &kubeNodeAccess{nodeShells: map[string]*corev1.Pod{}}
	create := func() (*corev1.Pod, error) {
		return provider.createNodeShell(client, "ovn-kubernetes", "node-1", "shell-image")
	}

	if _, err := provider.nodeShellWithClient(client, "node-1", create); !errors.Is(err, deleteErr) {
		t.Fatalf("nodeShellWithClient() error = %v, want %v", err, deleteErr)
	}
	shell, err := provider.nodeShellWithClient(client, "node-1", create)
	if err != nil || shell == nil || shell.Name != "node-shell-2" {
		t.Fatalf("nodeShellWithClient() = %v, %v, want replacement Pod", shell, err)
	}
	pods, err := client.CoreV1().Pods("ovn-kubernetes").List(t.Context(), metav1.ListOptions{})
	if err != nil || deletes != 2 || len(pods.Items) != 1 || pods.Items[0].Name != "node-shell-2" {
		t.Fatalf("retry left Pods %v after %d deletes, error %v", pods.Items, deletes, err)
	}
}

func TestDisruptsKubelet(t *testing.T) {
	for _, test := range []struct {
		cmd  []string
		want bool
	}{
		{cmd: []string{"systemctl", "stop", "kubelet.service"}, want: true},
		{cmd: []string{"systemctl", "restart", "kubelet"}, want: true},
		{cmd: []string{"systemctl", "restart", "sshd"}, want: false},
	} {
		if got := disruptsKubelet(test.cmd); got != test.want {
			t.Errorf("disruptsKubelet(%q) = %t, want %t", test.cmd, got, test.want)
		}
	}
}
