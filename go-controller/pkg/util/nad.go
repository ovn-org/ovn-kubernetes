// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"fmt"

	nadtypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclientset "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadlisters "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

// EnsureDefaultNetworkNAD ensures that a NAD exists for the default network at
// the location configured by the cluster-default-nad option. This allows users
// to customize the primary UDN attachments with static IPs, and/or MAC address
// requests, by using the multus-cni `default network` feature.
func EnsureDefaultNetworkNAD(ctx context.Context, nadLister nadlisters.NetworkAttachmentDefinitionLister, nadClient nadclientset.Interface) (*nadtypes.NetworkAttachmentDefinition, error) {
	namespace, name := config.Default.ClusterDefaultNetworkNAD.Namespace, config.Default.ClusterDefaultNetworkNAD.Name
	nad, err := nadLister.NetworkAttachmentDefinitions(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if nad != nil {
		return nad, nil
	}
	return nadClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
		ctx,
		&nadtypes.NetworkAttachmentDefinition{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: nadtypes.NetworkAttachmentDefinitionSpec{
				Config: fmt.Sprintf("{\"cniVersion\": \"%s\", \"name\": \"ovn-kubernetes\", \"type\": \"%s\"}", config.CNISpecVersion, config.CNI.Plugin),
			},
		},
		// note we don't set ourselves as field manager for this create as we
		// want to process the resulting event that would otherwise be filtered
		// out in nadNeedsUpdate
		metav1.CreateOptions{},
	)
}
