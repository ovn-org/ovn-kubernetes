// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package infraprovider

import (
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/engine/portalloc"
	"k8s.io/kubernetes/test/e2e/framework"
)

// ComposedProvider shares cluster behavior; providers add infrastructure capabilities separately.
type ComposedProvider struct {
	api.NodeAccess
	api.ExternalContainerProvider
	name           string
	primaryNetwork string
	hostPort       *portalloc.PortAllocator
}

func NewComposedProvider(name, primaryNetwork string, nodeAccess api.NodeAccess, external api.ExternalContainerProvider) *ComposedProvider {
	return &ComposedProvider{
		NodeAccess:                nodeAccess,
		ExternalContainerProvider: external,
		name:                      name,
		primaryNetwork:            primaryNetwork,
		hostPort:                  portalloc.New(1024, 65535),
	}
}

func (p *ComposedProvider) Name() string {
	return p.name
}

func (p *ComposedProvider) PrimaryNetwork() (api.Network, error) {
	return p.GetNetwork(p.primaryNetwork)
}

func (p *ComposedProvider) GetDefaultTimeoutContext() *framework.TimeoutContext {
	return framework.NewTimeoutContext()
}

func (p *ComposedProvider) GetK8HostPort() uint16 {
	return p.hostPort.Allocate()
}
