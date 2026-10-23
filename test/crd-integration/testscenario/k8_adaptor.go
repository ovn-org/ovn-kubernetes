// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testscenario

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file converts testscenario ValidateCRScenario / UpdateCRScenario to controller runtime client object

// ValidateScenarioToObject converts a scenario to controller runtime object.
func ValidateScenarioToObject(scenario ValidateCRScenario) (client.Object, error) {
	object, err := manifestToObject(scenario.Manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to convert scenario %q to object: %w", scenario.Description, err)
	}
	return object, nil
}

// ValidateScenariosToObjects converts multiple scenario to controller runtime objects.
func ValidateScenariosToObjects(scenarios []ValidateCRScenario) ([]client.Object, error) {
	var objects []client.Object
	for _, scenario := range scenarios {
		object, err := ValidateScenarioToObject(scenario)
		if err != nil {
			return nil, fmt.Errorf("failed to generate object from scenario with description %q, err: %w", scenario.Description, err)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

// UpdateCRScenarioToObject converts an update scenario to controller runtime objects. First arg returned is the initial object
// that is created and the second object is the object used to update the initial object.
func UpdateCRScenarioToObject(scenario UpdateCRScenario) (client.Object, client.Object, error) {
	initialObject, err := manifestToObject(scenario.InitialManifest)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert the initial manifest to object: %w", err)
	}
	updateObject, err := manifestToObject(scenario.Manifest)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert the update manifest to object: %w", err)
	}
	return initialObject, updateObject, err
}

// manifestToObject decodes a single Kubernetes YAML or JSON document to a Kubernetes Object
func manifestToObject(manifest string) (client.Object, error) {
	obj := &unstructured.Unstructured{}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	if err := decoder.Decode(obj); err != nil {
		return nil, fmt.Errorf("failed to decode Kubernetes manifest from string:\n%q\nerr: %w", manifest, err)
	}
	return obj, nil
}
