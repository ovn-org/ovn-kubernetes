package ops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func TestDeleteChassis(t *testing.T) {
	uuid := "b9998337-2498-4d1e-86e6-fc0417abb2f0"
	uuid2 := "b9998337-2498-4d1e-86e6-fc0417abb2f1"
	uuid3 := "b9998337-2498-4d1e-86e6-fc0417abb2f2"
	fakeDatapathUUID := "datapath-uuid"
	tests := []struct {
		desc             string
		chassis          *sbdb.Chassis
		chassisPredicate chassisPredicate
		initialDB        []libovsdbtest.TestData
		expectedDB       []libovsdbtest.TestData
	}{
		{
			desc:    "delete chassis and chassis private",
			chassis: &sbdb.Chassis{Name: "test"},
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test"},
				&sbdb.ChassisPrivate{Name: "test"},
				&sbdb.Chassis{Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
			},
		},
		{
			desc:    "delete chassis and igmp group by chassis UUID",
			chassis: &sbdb.Chassis{UUID: uuid},
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid, Name: "test"},
				&sbdb.IGMPGroup{Address: "1.1.1.1", Chassis: &uuid, Datapath: &fakeDatapathUUID},
				&sbdb.IGMPGroup{Address: "1.1.1.2", Chassis: &uuid, Datapath: &fakeDatapathUUID},

				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
		},
		{
			desc:    "delete chassis and igmp group by chassis Name",
			chassis: &sbdb.Chassis{Name: "test"},
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid, Name: "test"},
				&sbdb.IGMPGroup{Address: "1.1.1.1", Chassis: &uuid, Datapath: &fakeDatapathUUID},
				&sbdb.IGMPGroup{Address: "1.1.1.2", Chassis: &uuid, Datapath: &fakeDatapathUUID},

				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
		},
		{
			desc:    "delete chassis and chassis private by UUID",
			chassis: &sbdb.Chassis{UUID: uuid},
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid, Name: "test"},
				&sbdb.ChassisPrivate{Name: "test"},
			},
			expectedDB: []libovsdbtest.TestData{},
		},
		{
			desc:    "delete chassis when chassis private and igmp group do not exist",
			chassis: &sbdb.Chassis{Name: "test"},
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test"},
				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid2, Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
			},
		},
		{
			desc:    "delete chassis private when chassis does not exist",
			chassis: &sbdb.Chassis{Name: "test"},
			initialDB: []libovsdbtest.TestData{
				&sbdb.ChassisPrivate{Name: "test"},
				&sbdb.Chassis{Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test2"},
				&sbdb.ChassisPrivate{Name: "test2"},
			},
		},
		{
			desc:             "delete chassis and chassis private by predicate",
			chassisPredicate: func(c *sbdb.Chassis) bool { return c.Hostname == "testNode" },
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test", Hostname: "testNode"},
				&sbdb.ChassisPrivate{Name: "test"},
				&sbdb.Chassis{Name: "test2", Hostname: "testNode"},
				&sbdb.ChassisPrivate{Name: "test2"},
				&sbdb.Chassis{Name: "test3", Hostname: "testNode3"},
				&sbdb.ChassisPrivate{Name: "test3"},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test3", Hostname: "testNode3"},
				&sbdb.ChassisPrivate{Name: "test3"},
			},
		},
		{
			desc:             "delete chassis and igmp group by predicate",
			chassisPredicate: func(c *sbdb.Chassis) bool { return c.Hostname == "testNode" },
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid, Hostname: "testNode", Name: "test"},
				&sbdb.IGMPGroup{Address: "1.1.1.1", Chassis: &uuid, Datapath: &fakeDatapathUUID},
				&sbdb.IGMPGroup{Address: "1.1.1.2", Chassis: &uuid, Datapath: &fakeDatapathUUID},
				&sbdb.Chassis{UUID: uuid2, Hostname: "testNode", Name: "test2"},
				&sbdb.IGMPGroup{Chassis: &uuid2, Datapath: &fakeDatapathUUID},
				&sbdb.Chassis{UUID: uuid3, Hostname: "testNode3", Name: "test3"},
				&sbdb.IGMPGroup{Chassis: &uuid3, Datapath: &fakeDatapathUUID},
			},
			expectedDB: []libovsdbtest.TestData{
				&sbdb.Chassis{UUID: uuid3, Hostname: "testNode3", Name: "test3"},
				&sbdb.IGMPGroup{Chassis: &uuid3, Datapath: &fakeDatapathUUID},
			},
		},
		{
			desc:             "delete chassis by predicate when chassis private does not exist",
			chassisPredicate: func(c *sbdb.Chassis) bool { return c.Hostname == "testNode" },
			initialDB: []libovsdbtest.TestData{
				&sbdb.Chassis{Name: "test", Hostname: "testNode"},
			},
			expectedDB: []libovsdbtest.TestData{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			fakeDatapath := &sbdb.DatapathBinding{
				UUID: fakeDatapathUUID,
			}
			dbSetup := libovsdbtest.TestSetup{
				SBData: append(tt.initialDB, fakeDatapath),
			}
			sbClient, cleanup, err := libovsdbtest.NewSBTestHarness(dbSetup, nil)
			if err != nil {
				t.Fatalf("%s: failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			if tt.chassis != nil {
				err = DeleteChassis(sbClient, tt.chassis)
			} else if tt.chassisPredicate != nil {
				err = DeleteChassisWithPredicate(sbClient, tt.chassisPredicate)
			}

			if err != nil {
				t.Fatal(fmt.Errorf("%s: got unexpected error: %v", tt.desc, err))
			}

			expectedDB := append(tt.expectedDB, fakeDatapath)
			matcher := libovsdbtest.HaveDataIgnoringUUIDs(expectedDB)
			match, err := matcher.Match(sbClient)
			if err != nil {
				t.Fatalf("%s: matcher error: %v", tt.desc, err)
			}
			if !match {
				t.Fatalf("%s: DB state did not match: %s", tt.desc, matcher.FailureMessage(sbClient))
			}
		})
	}
}
