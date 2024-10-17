package udn

import (
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

var _ = Describe("PrimaryNetworkSpec", func() {
	table.DescribeTable("should return true, given",
		func(spec specGetter) {
			Expect(IsPrimaryNetwork(spec)).To(BeTrue())
		},
		table.Entry("udn crd spec, l3, primary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		table.Entry("udn crd spec, l2, primary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		table.Entry("cluster-udn spec, l3, primary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		table.Entry("cluster-udn spec, l2, primary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
	)
	table.DescribeTable("should return false, given",
		func(spec specGetter) {
			Expect(IsPrimaryNetwork(spec)).To(BeFalse())
		},
		table.Entry("udn crd spec, l3, secondary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		table.Entry("udn crd spec, l2, secondary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		table.Entry("cluster-udn spec, l3, secondary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		table.Entry("cluster-udn spec, l2, secondary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
	)
})
