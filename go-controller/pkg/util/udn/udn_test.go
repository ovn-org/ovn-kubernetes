package udn

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
)

var _ = Describe("PrimaryNetworkSpec", func() {
	DescribeTable("should return true, given",
		func(spec specGetter) {
			Expect(IsPrimaryNetwork(spec)).To(BeTrue())
		},
		Entry("udn crd spec, l3, primary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		Entry("udn crd spec, l2, primary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		Entry("cluster-udn spec, l3, primary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
		Entry("cluster-udn spec, l2, primary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRolePrimary,
				},
			},
		),
	)
	DescribeTable("should return false, given",
		func(spec specGetter) {
			Expect(IsPrimaryNetwork(spec)).To(BeFalse())
		},
		Entry("udn crd spec, l3, secondary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		Entry("udn crd spec, l2, secondary",
			&udnv1.UserDefinedNetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		Entry("cluster-udn spec, l3, secondary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer3,
				Layer3: &udnv1.Layer3Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
		Entry("cluster-udn spec, l2, secondary",
			&udnv1.NetworkSpec{
				Topology: udnv1.NetworkTopologyLayer2,
				Layer2: &udnv1.Layer2Config{
					Role: udnv1.NetworkRoleSecondary,
				},
			},
		),
	)
})
