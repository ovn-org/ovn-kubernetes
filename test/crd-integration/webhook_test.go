// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package crdintegration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

// Exercise the deployed exclusion as well as the validator: a spoofed self-pod
// label must not prevent protected metadata from reaching admission at all.
func TestPodWebhookSelfExclusion(t *testing.T) {
	template, err := os.ReadFile(filepath.Join(crdDir(), "..", "charts", "ovnkube-identity", "templates", "webhook.yaml"))
	if err != nil {
		t.Fatalf("read Helm template for pod webhook match condition: %v", err)
	}
	_, podWebhook, found := strings.Cut(string(template), "name: ovn-kubernetes-admission-webhook-pod\n")
	if !found {
		t.Fatal("missing pod webhook")
	}
	_, expression, found := strings.Cut(podWebhook, "expression: >-")
	if !found {
		t.Fatal("missing match condition")
	}
	expression, _, _ = strings.Cut(expression, "\n    rules:")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatalf("create CEL environment for pod webhook match condition: %v", err)
	}
	ast, issues := env.Compile(strings.TrimSpace(expression))
	if issues.Err() != nil {
		t.Fatalf("compile pod webhook match condition: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("create CEL program for pod webhook match condition: %v", err)
	}
	pod := func(labels, annotations map[string]string) map[string]any {
		metadata := map[string]any{}
		if labels != nil {
			metadata["labels"] = labels
		}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata}
	}
	evaluate := func(t *testing.T, current, old any, invoke bool) {
		t.Helper()
		result, _, err := program.Eval(map[string]any{"request": map[string]any{"namespace": "ovn-kubernetes"}, "object": current, "oldObject": old})
		if err != nil {
			t.Fatalf("evaluate pod webhook match condition: %v", err)
		}
		if result != types.Bool(invoke) {
			t.Fatalf("invoke = %v, want %v", result, invoke)
		}
	}
	self := map[string]string{"name": "ovnkube-identity"}
	t.Run("bootstrap self pod", func(t *testing.T) { evaluate(t, pod(self, nil), nil, false) })
	t.Run("ordinary pod", func(t *testing.T) { evaluate(t, pod(nil, nil), nil, true) })
	for _, key := range []string{"k8s.ovn.org/pod-networks", "k8s.ovn.org/dpu.connection-details", "k8s.ovn.org/dpu.connection-status"} {
		t.Run(key, func(t *testing.T) {
			annotated := pod(self, map[string]string{key: ""})
			evaluate(t, annotated, nil, true)
			evaluate(t, annotated, annotated, true)
			evaluate(t, pod(self, nil), annotated, true)
		})
	}
}

// TestDefaultNetworkAnnotationPolicy checks that missing annotation maps do not
// reject unrelated updates while the selected default network stays immutable.
func TestDefaultNetworkAnnotationPolicy(t *testing.T) {
	template, err := os.ReadFile(filepath.Join(crdDir(), "..", "charts", "ovnkube-control-plane", "templates", "rbac-ovnkube-cluster-manager.yaml"))
	if err != nil {
		t.Fatalf("read Helm template for default-network annotation policy: %v", err)
	}
	_, policy, found := strings.Cut(string(template), "name: default-network-annotation\n")
	if !found {
		t.Fatal("missing default-network annotation policy")
	}
	_, expression, found := strings.Cut(policy, "- expression: >\n")
	if !found {
		t.Fatal("missing validation expression")
	}
	expression, _, _ = strings.Cut(expression, "\n      message:")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	if err != nil {
		t.Fatalf("create CEL environment for default-network annotation policy: %v", err)
	}
	ast, issues := env.Compile(strings.TrimSpace(expression))
	if issues.Err() != nil {
		t.Fatalf("compile default-network annotation policy: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("create CEL program for default-network annotation policy: %v", err)
	}
	pod := func(annotations map[string]string) map[string]any {
		metadata := map[string]any{}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata}
	}
	const key = "v1.multus-cni.io/default-network"
	for _, tc := range []struct {
		name     string
		old, new map[string]string
		allowed  bool
	}{
		{name: "both maps absent", allowed: true},
		{name: "empty maps", old: map[string]string{}, new: map[string]string{}, allowed: true},
		{name: "add unrelated annotation", new: map[string]string{"example.com/note": "value"}, allowed: true},
		{name: "remove unrelated annotation", old: map[string]string{"example.com/note": "value"}, allowed: true},
		{name: "retain network", old: map[string]string{key: "blue"}, new: map[string]string{key: "blue", "example.com/note": "value"}, allowed: true},
		{name: "add network", new: map[string]string{key: "blue"}},
		{name: "add empty network", new: map[string]string{key: ""}},
		{name: "change network", old: map[string]string{key: "blue"}, new: map[string]string{key: "red"}},
		{name: "remove network", old: map[string]string{key: "blue"}, new: map[string]string{"example.com/note": "value"}},
		{name: "remove entire map", old: map[string]string{key: "blue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := program.Eval(map[string]any{"object": pod(tc.new), "oldObject": pod(tc.old)})
			if err != nil {
				t.Fatalf("evaluate default-network annotation policy: %v", err)
			}
			if result != types.Bool(tc.allowed) {
				t.Fatalf("allowed = %v, want %v", result, tc.allowed)
			}
		})
	}
}
