//go:build linux
// +build linux

package nftables

import (
	"context"

	"sigs.k8s.io/knftables"
)

const OVNKubernetesNFTablesName = "ovn-kubernetes"

var nftHelper knftables.Interface

// SetFakeNFTablesHelper creates a fake knftables.Interface
func SetFakeNFTablesHelper() *knftables.Fake {
	fake := knftables.NewFake(knftables.InetFamily, OVNKubernetesNFTablesName)
	tx := fake.NewTransaction()
	tx.Add(&knftables.Table{})
	_ = fake.Run(context.TODO(), tx)

	nftHelper = fake
	return fake
}

// GetNFTablesHelper returns a knftables.Interface. If SetFakeNFTablesHelper has not been
// called, it will create a "real" knftables.Interface
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
