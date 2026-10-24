// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

var _ = ginkgo.Describe("EgressIP preemptive cleanup on label removal", func() {
	const (
		nodeName        = "node1"
		egressIPName    = "egressip-test"
		podNamespace    = "test-ns"
		podName         = "test-pod"
		podIP           = "10.128.0.10"
		egressIP        = "192.168.1.100"
		controllerName  = types.DefaultNetworkControllerName
	)

	ginkgo.It("should remove SNAT rules and return nil on success", func() {
		config.PrepareTestConfig()
		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				// NAT rule that should be deleted
				&nbdb.NAT{
					UUID:       "nat-uuid-1",
					LogicalIP:  podIP,
					ExternalIP: egressIP,
					Type:       nbdb.NATTypeSNAT,
					LogicalPort: func() *string {
						lp := "k8s-" + nodeName
						return &lp
					}(),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerTypeKey.String():       string(libovsdbops.EgressIPOwnerType),
						libovsdbops.OwnerControllerKey.String(): controllerName,
					},
				},
				// Another NAT rule that should be deleted
				&nbdb.NAT{
					UUID:       "nat-uuid-2",
					LogicalIP:  "10.128.0.11",
					ExternalIP: "192.168.1.101",
					Type:       nbdb.NATTypeSNAT,
					LogicalPort: func() *string {
						lp := "k8s-" + nodeName
						return &lp
					}(),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerTypeKey.String():       string(libovsdbops.EgressIPOwnerType),
						libovsdbops.OwnerControllerKey.String(): controllerName,
					},
				},
				// NAT rule for different node that should NOT be deleted
				&nbdb.NAT{
					UUID:       "nat-uuid-3",
					LogicalIP:  "10.128.0.12",
					ExternalIP: "192.168.1.102",
					Type:       nbdb.NATTypeSNAT,
					LogicalPort: func() *string {
						lp := "k8s-node2"
						return &lp
					}(),
					ExternalIDs: map[string]string{
						libovsdbops.OwnerTypeKey.String():       string(libovsdbops.EgressIPOwnerType),
						libovsdbops.OwnerControllerKey.String(): controllerName,
					},
				},
			},
		}

		fakeOvn, err := NewFakeOVN(false, dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Call preemptiveEgressNodeCleanup
		err = fakeOvn.controller.eIPC.preemptiveEgressNodeCleanup(nodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "preemptive cleanup should succeed")

		// Verify that NAT rules for node1 were deleted
		natList := []nbdb.NAT{}
		err = fakeOvn.controller.nbClient.List(fakeOvn.controller.controllerCtx, &natList)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Should only have the NAT for node2 left
		gomega.Expect(natList).To(gomega.HaveLen(1), "should have deleted node1 NATs but kept node2 NAT")
		gomega.Expect(natList[0].UUID).To(gomega.Equal("nat-uuid-3"), "should only have node2 NAT remaining")
	})

	ginkgo.It("should return error when transaction fails", func() {
		config.PrepareTestConfig()
		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		// Setup with invalid state to trigger transaction failure
		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}

		fakeOvn, err := NewFakeOVN(false, dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Close the DB client to force transaction failure
		fakeOvn.controller.nbClient.Close()

		// Call preemptiveEgressNodeCleanup - should return error
		err = fakeOvn.controller.eIPC.preemptiveEgressNodeCleanup(nodeName)
		gomega.Expect(err).To(gomega.HaveOccurred(), "should return error when transaction fails")
	})

	ginkgo.It("should not error when no NATs exist for node", func() {
		config.PrepareTestConfig()
		app := cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		dbSetup := libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{},
		}

		fakeOvn, err := NewFakeOVN(false, dbSetup)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		// Call preemptiveEgressNodeCleanup with no existing NATs
		err = fakeOvn.controller.eIPC.preemptiveEgressNodeCleanup(nodeName)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "should not error when no NATs exist")
	})
})
