package kube

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkAttachmentDefinition describes a network configuration for CNI
type NetworkAttachmentDefinition struct {
	metav1.TypeMeta `json:",inline"`
	// Note that ObjectMeta is mandatory, as an object
	// name is required
	Metadata metav1.ObjectMeta `json:"metadata,omitempty" description:"standard object metadata"`

	// Specification describing how to invoke a CNI plugin to
	// add or remove network attachments for a Pod.
	// In the absence of valid keys in a Spec, the runtime (or
	// meta-plugin) should load and execute a CNI .configlist
	// or .config (in that order) file on-disk whose JSON
	// “name” key matches this Network object’s name.
	// +optional
	Spec NetworkAttachmentDefinitionSpec `json:"spec"`
}

type NetworkAttachmentDefinitionSpec struct {
	// Config contains a standard JSON-encoded CNI configuration
	// or configuration list which defines the plugin chain to
	// execute.  If present, this key takes precedence over
	// ‘Plugin’.
	// +optional
	Config string `json:"config"`
}
