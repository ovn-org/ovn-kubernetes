// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package dnsnameresolver

import (
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	addressset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

// DNSNameResolver provides address sets for DNS-backed EgressFirewall rules.
type DNSNameResolver interface {
	// Add returns the address set that the caller uses in an EgressFirewall ACL.
	Add(namespace, dnsName string) (addressset.AddressSet, error)
	// Delete removes DNS entries owned by namespace and releases unused address sets.
	Delete(namespace string) error
	// Run starts the resolver's background refresh loop.
	Run() error
	// Shutdown stops the background refresh loop.
	Shutdown()
	// DeleteStaleAddrSets removes resolver-owned address sets without ACL references.
	DeleteStaleAddrSets(nbClient libovsdbclient.Client) error
}

// DNSNameResolverAddCompleter is implemented by resolvers that protect an
// address set between Add returning and the caller finishing the ACL
// transaction that references it. Call CompleteAdd once per successful Add.
type DNSNameResolverAddCompleter interface {
	// CompleteAdd releases the pending Add-to-ACL handoff after the transaction
	// completes, including when the transaction fails.
	CompleteAdd(namespace, dnsName string)
}
