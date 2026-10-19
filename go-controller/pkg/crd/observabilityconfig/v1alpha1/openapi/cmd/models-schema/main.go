// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// This file should be auto-generated after https://github.com/kubernetes/kubernetes/pull/138893 is solved
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/observabilityconfig/v1alpha1/openapi"
)

// Outputs openAPI schema JSON containing the schema definitions in zz_generated.openapi.go.
func main() {
	err := output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed: %v", err) // nolint:errcheck
		os.Exit(1)
	}
}

func output() error {
	refFunc := func(name string) spec.Ref {
		return spec.MustCreateRef(fmt.Sprintf("#/definitions/%s", name))
	}
	defs := openapi.GetOpenAPIDefinitions(refFunc)
	schemaDefs := make(map[string]spec.Schema, len(defs))
	for k, v := range defs {
		// Replace top-level schema with v2 if a v2 schema is embedded
		// so that the output of this program is always in OpenAPI v2.
		// This is done by looking up an extension that marks the embedded v2
		// schema, and, if the v2 schema is found, make it the resulting schema for
		// the type.
		if schema, ok := v.Schema.Extensions[common.ExtensionV2Schema]; ok {
			if v2Schema, isOpenAPISchema := schema.(spec.Schema); isOpenAPISchema {
				schemaDefs[k] = v2Schema
				continue
			}
		}

		schemaDefs[k] = v.Schema
	}
	data, err := json.Marshal(&spec.Swagger{
		SwaggerProps: spec.SwaggerProps{
			Definitions: schemaDefs,
			Info: &spec.Info{
				InfoProps: spec.InfoProps{
					Title:   "Kubernetes",
					Version: "unversioned",
				},
			},
			Swagger: "2.0",
		},
	})
	if err != nil {
		return fmt.Errorf("error serializing api definitions: %w", err)
	}
	os.Stdout.Write(data) // nolint:errcheck
	return nil
}
