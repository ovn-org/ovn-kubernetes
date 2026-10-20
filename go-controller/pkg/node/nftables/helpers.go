// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package nftables

import (
	"context"

	"sigs.k8s.io/knftables"
)

const OVNKubernetesNFTablesName = "ovn-kubernetes"

// OVNKubernetesEgressIPNFTablesName is a netdev table required for dropping
// incoming ARP requests for an EgressIP during node or ovnkube-controller restart.
const OVNKubernetesEgressIPNFTablesName = "ovn-kubernetes-egressip"

var nftHelper knftables.Interface
var nftEgressIPHelper knftables.Interface

// SetNFTablesHelper sets the nftables interface to use.
func SetNFTablesHelper(helper knftables.Interface) {
	nftHelper = helper
}

// SetFakeNFTablesHelper creates a fake knftables.Interface
func SetFakeNFTablesHelper() *knftables.Fake {
	fake := knftables.NewFake(knftables.InetFamily, OVNKubernetesNFTablesName)
	tx := fake.NewTransaction()
	tx.Add(&knftables.Table{})
	_ = fake.Run(context.TODO(), tx)

	nftHelper = fake
	return fake
}

// GetNFTablesHelper returns the configured nftables interface, creating a real
// interface if none has been configured.
func GetNFTablesHelper() (knftables.Interface, error) {
	if nftHelper == nil {
		nft, err := knftables.New(knftables.InetFamily, OVNKubernetesNFTablesName)
		if err != nil {
			return nil, err
		}
		tx := nft.NewTransaction()
		tx.Add(&knftables.Table{})
		err = nft.Run(context.TODO(), tx)
		if err != nil {
			return nil, err
		}

		nftHelper = nft
	}
	return nftHelper, nil
}

// SetFakeEgressIPNFTablesHelper creates a fake knftables.Interface for egress IP with NetDev family
func SetFakeEgressIPNFTablesHelper() *knftables.Fake {
	fake := knftables.NewFake(knftables.NetDevFamily, OVNKubernetesEgressIPNFTablesName)
	tx := fake.NewTransaction()
	tx.Add(&knftables.Table{})
	_ = fake.Run(context.TODO(), tx)

	nftEgressIPHelper = fake
	return fake
}

// GetEgressIPNFTablesHelper returns a knftables.Interface for egress IP with NetDev family.
// If SetFakeEgressIPNFTablesHelper has not been called, it will create a "real" knftables.Interface
func GetEgressIPNFTablesHelper() (knftables.Interface, error) {
	if nftEgressIPHelper == nil {
		nft, err := knftables.New(knftables.NetDevFamily, OVNKubernetesEgressIPNFTablesName)
		if err != nil {
			return nil, err
		}
		nftEgressIPHelper = nft
	}
	return nftEgressIPHelper, nil
}

// CleanupNFTables cleans up all ovn-kubernetes NFTables data, on ovnkube-node daemonset
// deletion.
func CleanupNFTables() {
	nft, _ := GetNFTablesHelper()
	if nft == nil {
		return
	}
	tx := nft.NewTransaction()
	tx.Delete(&knftables.Table{})
	_ = nft.Run(context.Background(), tx)
}
