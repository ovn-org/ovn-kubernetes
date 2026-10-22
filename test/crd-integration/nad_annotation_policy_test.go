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

func TestNADAllocationAnnotationPolicy(t *testing.T) {
	template, err := os.ReadFile(filepath.Join(crdDir(), "..", "charts", "ovnkube-control-plane", "templates", "nad-annotation-ownership.yaml"))
	if err != nil {
		t.Fatalf("read NAD annotation policy: %v", err)
	}
	_, expression, found := strings.Cut(string(template), "- expression: >-\n")
	if !found {
		t.Fatal("missing validation expression")
	}
	expression, _, _ = strings.Cut(expression, "\n      message:")
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatalf("create CEL environment: %v", err)
	}
	ast, issues := env.Compile(strings.TrimSpace(expression))
	if issues.Err() != nil {
		t.Fatalf("compile NAD annotation policy: %v", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatalf("create CEL program: %v", err)
	}
	nad := func(annotations map[string]string) map[string]any {
		metadata := map[string]any{}
		if annotations != nil {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata}
	}
	const clusterManager = "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager"
	for _, annotation := range []struct {
		key, value, replacement string
	}{
		{key: "k8s.ovn.org/network-id", value: "1", replacement: "2"},
		{key: "k8s.ovn.org/tunnel-keys", value: "[16711684,16715779]", replacement: "[16711684,16715780]"},
	} {
		t.Run(annotation.key, func(t *testing.T) {
			key := annotation.key
			otherKey, otherValue := "k8s.ovn.org/network-id", "1"
			if key == otherKey {
				otherKey, otherValue = "k8s.ovn.org/tunnel-keys", "[16711684,16715779]"
			}
			for _, user := range []string{
				clusterManager,
				"alice",
				"system:admin",
				"system:ovn-node:node1",
				"system:serviceaccount:ovn-kubernetes:ovnkube-node",
				"system:serviceaccount:other:ovnkube-cluster-manager",
			} {
				t.Run(user, func(t *testing.T) {
					for _, tc := range []struct {
						name      string
						operation string
						old, new  map[string]string
						allowed   bool
					}{
						{name: "create without annotations", operation: "CREATE", allowed: true},
						{name: "create with empty map", operation: "CREATE", new: map[string]string{}, allowed: true},
						{name: "create with unrelated annotation", operation: "CREATE", new: map[string]string{"example.com/note": "value"}, allowed: true},
						{name: "create with key", operation: "CREATE", new: map[string]string{key: annotation.value}},
						{name: "create with empty key", operation: "CREATE", new: map[string]string{key: ""}},
						{name: "update without annotations", operation: "UPDATE", allowed: true},
						{name: "add key", operation: "UPDATE", new: map[string]string{key: annotation.value}},
						{name: "add empty key", operation: "UPDATE", new: map[string]string{key: ""}},
						{name: "change key", operation: "UPDATE", old: map[string]string{key: annotation.value}, new: map[string]string{key: annotation.replacement}},
						{name: "remove key", operation: "UPDATE", old: map[string]string{key: annotation.value}, new: map[string]string{}},
						{name: "remove entire map", operation: "UPDATE", old: map[string]string{key: annotation.value}},
						{name: "remove empty key", operation: "UPDATE", old: map[string]string{key: ""}},
						{name: "retain valid key", operation: "UPDATE", old: map[string]string{key: annotation.value}, new: map[string]string{key: annotation.value, "example.com/note": "value"}, allowed: true},
						{name: "retain corrupt key", operation: "UPDATE", old: map[string]string{key: "invalid"}, new: map[string]string{key: "invalid", "example.com/note": "value"}, allowed: true},
						{name: "retain both annotations", operation: "UPDATE", old: map[string]string{key: annotation.value, otherKey: otherValue}, new: map[string]string{key: annotation.value, otherKey: otherValue, "example.com/note": "value"}, allowed: true},
						{name: "change one annotation while retaining the other", operation: "UPDATE", old: map[string]string{key: annotation.value, otherKey: otherValue}, new: map[string]string{key: annotation.replacement, otherKey: otherValue}},
						{name: "remove one annotation while retaining the other", operation: "UPDATE", old: map[string]string{key: annotation.value, otherKey: otherValue}, new: map[string]string{otherKey: otherValue}},
						{name: "repair corrupt key", operation: "UPDATE", old: map[string]string{key: "invalid"}, new: map[string]string{key: annotation.value}},
					} {
						t.Run(tc.name, func(t *testing.T) {
							var old any
							if tc.operation == "UPDATE" {
								old = nad(tc.old)
							}
							result, _, err := program.Eval(map[string]any{
								"object": nad(tc.new), "oldObject": old,
								"request": map[string]any{"operation": tc.operation, "userInfo": map[string]any{"username": user}},
							})
							if err != nil {
								t.Fatalf("evaluate NAD annotation policy: %v", err)
							}
							allowed := tc.allowed || user == clusterManager
							if result != types.Bool(allowed) {
								t.Fatalf("allowed = %v, want %v", result, allowed)
							}
						})
					}
				})
			}
		})
	}
}
