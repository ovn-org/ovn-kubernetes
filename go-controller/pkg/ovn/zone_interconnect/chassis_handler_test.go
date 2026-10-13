// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package zoneinterconnect

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var _ = ginkgo.Describe("Zone Interconnect Chassis Operations", func() {
	var (
		app             *cli.App
		libovsdbCleanup *libovsdbtest.Context
		testNode1       corev1.Node
		testNode2       corev1.Node
		testNode3       corev1.Node
		testNode4       corev1.Node
		testNode5       corev1.Node
		node1Chassis    sbdb.Chassis
		node2Chassis    sbdb.Chassis
		node3Chassis    sbdb.Chassis
		node4Chassis    sbdb.Chassis
		node5Chassis    sbdb.Chassis
		node5Encap      sbdb.Encap
		initialSBDB     []libovsdbtest.TestData
	)

	const (
		clusterIPNet   string = "10.1.0.0"
		clusterCIDR    string = clusterIPNet + "/16"
		joinSubnetCIDR string = "100.64.0.0/16/19"
		vlanID                = 1024
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		gomega.Expect(config.PrepareTestConfig()).To(gomega.Succeed())

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		libovsdbCleanup = nil

		node1Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6", Hostname: "node1", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6"}
		node2Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7", Hostname: "node2", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7"}
		node3Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8", Hostname: "node3", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8"}
		node4Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac9", Hostname: "node4", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac9"}
		node5Chassis = sbdb.Chassis{Name: "cb9ec8fa-b409-4ef3-9f42-d9283c47aaca", Hostname: "node5", UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aac9a",
			Encaps: []string{"cb9ec8fa-b409-4ef3-9f42-d9283c47aacb"}}
		node5Encap = sbdb.Encap{ChassisName: "cb9ec8fa-b409-4ef3-9f42-d9283c47aaca", IP: "10.0.0.16", Type: "geneve",
			UUID: "cb9ec8fa-b409-4ef3-9f42-d9283c47aacb"}

		testNode1 = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node1",
				Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": "cb9ec8fa-b409-4ef3-9f42-d9283c47aac6",
					"k8s.ovn.org/node-encap-ips": "[\"10.0.0.10\"]"},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
			},
		}
		testNode2 = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node2",
				Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": "cb9ec8fa-b409-4ef3-9f42-d9283c47aac7",
					"k8s.ovn.org/node-encap-ips": "[\"10.0.0.11\"]"},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}},
			},
		}
		testNode3 = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node3",
				Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": "cb9ec8fa-b409-4ef3-9f42-d9283c47aac8",
					"k8s.ovn.org/node-encap-ips": "[\"10.0.0.12\"]"},
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}},
			},
		}
		testNode4 = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node4",
				Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": "cb9ec8fa-b409-4ef3-9f42-d9283c47aac9",
					"k8s.ovn.org/node-encap-ips": "[\"10.0.0.14\", \"10.0.0.15\"]"},
			},
		}
		testNode5 = corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node5",
				Annotations: map[string]string{"k8s.ovn.org/node-chassis-id": "cb9ec8fa-b409-4ef3-9f42-d9283c47aaca",
					"k8s.ovn.org/node-encap-ips": "[\"10.0.0.11\"]"},
			},
		}

		initialSBDB = []libovsdbtest.TestData{
			&node1Chassis, &node2Chassis, &node5Chassis, &node5Encap}
	})

	ginkgo.AfterEach(func() {
		if libovsdbCleanup != nil {
			libovsdbCleanup.Cleanup()
		}
	})

	ginkgo.It("chassis is-remote check", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddLocalZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			err = zoneChassisHandler.AddLocalZoneNode(&testNode2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node2Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			err = zoneChassisHandler.AddRemoteZoneNode(&testNode3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node3Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("encap dst-port check", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			config.Default.EncapPort = 9880

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encapIP, err := util.ParseNodeEncapIPsAnnotation(&testNode3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encap := &sbdb.Encap{
				Type: "geneve",
				IP:   encapIP[0],
			}
			err = libovsdbOvnSBClient.Get(context.Background(), encap)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(encap.Options["dst_port"]).Should(gomega.Equal("9880"))
			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Add multiple encap records", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode4)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encapIP, err := util.ParseNodeEncapIPsAnnotation(&testNode4)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encap1 := &sbdb.Encap{
				Type: "geneve",
				IP:   encapIP[0],
			}

			encap2 := &sbdb.Encap{
				Type: "geneve",
				IP:   encapIP[1],
			}

			err = libovsdbOvnSBClient.Get(context.Background(), encap1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = libovsdbOvnSBClient.Get(context.Background(), encap2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node4Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.Encaps).To(gomega.HaveLen(2))
			gomega.Expect(nodeCh.Encaps).To(gomega.ContainElements(string(encap1.UUID)), string(encap2.UUID))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Update encap record when chassis exists", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode5)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encapIP, err := util.ParseNodeEncapIPsAnnotation(&testNode5)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			encap := &sbdb.Encap{
				Type: "geneve",
				IP:   encapIP[0],
			}

			err = libovsdbOvnSBClient.Get(context.Background(), encap)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node5Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.Encaps).To(gomega.HaveLen(1))
			gomega.Expect(nodeCh.Encaps).To(gomega.ContainElements(string(encap.UUID)))

			err = libovsdbOvnSBClient.Get(context.Background(), &node5Encap)
			gomega.Expect(err).To(gomega.SatisfyAny(gomega.BeNil(), gomega.MatchError(libovsdbclient.ErrNotFound)))

			return nil
		}
		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Move chassis zone", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddLocalZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			// Move the node1 chassis to remote
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			// Move the chassis back to local zone
			err = zoneChassisHandler.AddLocalZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Delete remote zone node", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddLocalZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			// Call DeleteRemoteZoneNode for local zone node.  The chassis entry should be still there
			// as its not a remote zone node.
			err = zoneChassisHandler.DeleteRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			// Make the testNode1 as remote zone
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			// Call DeleteRemoteZoneNode for remote zone node.  The chassis entry should be deleted
			err = zoneChassisHandler.DeleteRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Check the SB Chassis.
			_, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).To(gomega.HaveOccurred())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Delete remote zone node with no chassis id", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})

			// Add testNode1 as a remote node
			err = zoneChassisHandler.AddRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			// Remove chassis ID from the node (unfortunate action from cluster admin)
			testNode1.Annotations = make(map[string]string)

			// Call DeleteRemoteZoneNode for remote zone node.  The chassis entry should still be deleted
			err = zoneChassisHandler.DeleteRemoteZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			// Check the SB Chassis.
			_, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).To(gomega.HaveOccurred())

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})

	ginkgo.It("Sync nodes", func() {
		app.Action = func(ctx *cli.Context) error {
			dbSetup := libovsdbtest.TestSetup{
				SBData: initialSBDB,
			}

			_, err := config.InitConfig(ctx, nil, nil)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			var libovsdbOvnSBClient libovsdbclient.Client
			_, libovsdbOvnSBClient, libovsdbCleanup, err = libovsdbtest.NewNBSBTestHarness(dbSetup)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			zoneChassisHandler := NewZoneChassisHandler(libovsdbOvnSBClient, "node1", func() ([]*corev1.Node, error) {
				return []*corev1.Node{&testNode1, &testNode2, &testNode3, &testNode4, &testNode5}, nil
			})
			err = zoneChassisHandler.AddLocalZoneNode(&testNode1)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			// Check the SB Chassis.
			nodeCh, err := libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			err = zoneChassisHandler.AddRemoteZoneNode(&testNode2)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node2Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			err = zoneChassisHandler.AddRemoteZoneNode(&testNode3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node3Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			// Call ICHandler SyncNodes function removing the testNode3 from the list of nodes.
			// Chassis record for testNode3 should be cleaned up SyncNodes.
			var kNodes []interface{}
			kNodes = append(kNodes, &testNode1)
			kNodes = append(kNodes, &testNode2)
			err = zoneChassisHandler.SyncNodes(kNodes)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node3Chassis)
			gomega.Expect(err).To(gomega.HaveOccurred())
			gomega.Expect(nodeCh).To(gomega.BeNil())

			// chassis entries for testNode1 and testNode2 should be present
			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node1Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "false"))

			nodeCh, err = libovsdbops.GetChassis(libovsdbOvnSBClient, &node2Chassis)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(nodeCh.OtherConfig).Should(gomega.HaveKeyWithValue("is-remote", "true"))

			return nil
		}

		err := app.Run([]string{
			app.Name,
			"-cluster-subnets=" + clusterCIDR,
			"-init-cluster-manager",
			"-zone-join-switch-subnets=" + joinSubnetCIDR,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	})
})
