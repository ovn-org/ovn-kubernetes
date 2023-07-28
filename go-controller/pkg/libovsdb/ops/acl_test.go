package ops

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestCreateOrUpdateACL(t *testing.T) {
	aclName := "acl1"
	aclSev := nbdb.ACLSeverityInfo
	aclMeter := types.OvnACLLoggingMeter
	initialACL := &nbdb.ACL{
		UUID:        buildNamedUUID(),
		Action:      nbdb.ACLActionAllow,
		Direction:   nbdb.ACLDirectionToLport,
		ExternalIDs: map[string]string{"key": "value"},
		Log:         true,
		Match:       "match",
		Meter:       &aclMeter,
		Name:        &aclName,
		Options:     map[string]string{"key": "value"},
		Priority:    1,
		Severity:    &aclSev,
	}

	tests := []struct {
		desc       string
		initialACL *nbdb.ACL
		finalACL   *nbdb.ACL
	}{
		{
			desc:       "updates Severity to empty",
			initialACL: initialACL,
			finalACL: &nbdb.ACL{
				Action:      nbdb.ACLActionAllow,
				Direction:   nbdb.ACLDirectionToLport,
				ExternalIDs: map[string]string{"key": "value"},
				Log:         true,
				Match:       "match",
				Meter:       &aclMeter,
				Name:        &aclName,
				Options:     map[string]string{"key": "value"},
				Priority:    1,
				Severity:    nil,
			},
		},
		{
			desc:       "updates Name to empty",
			initialACL: initialACL,
			finalACL: &nbdb.ACL{
				Action:      nbdb.ACLActionAllow,
				Direction:   nbdb.ACLDirectionToLport,
				ExternalIDs: map[string]string{"key": "value"},
				Log:         true,
				Match:       "match",
				Meter:       &aclMeter,
				Name:        nil,
				Options:     map[string]string{"key": "value"},
				Priority:    1,
				Severity:    &aclSev,
			},
		},
		{
			desc:       "updates Options to empty",
			initialACL: initialACL,
			finalACL: &nbdb.ACL{
				Action:      nbdb.ACLActionAllow,
				Direction:   nbdb.ACLDirectionToLport,
				ExternalIDs: map[string]string{"key": "value"},
				Log:         true,
				Match:       "match",
				Meter:       &aclMeter,
				Name:        &aclName,
				Options:     nil,
				Priority:    1,
				Severity:    &aclSev,
			},
		},
		{
			desc:       "updates ExternalIDs to empty",
			initialACL: initialACL,
			finalACL: &nbdb.ACL{
				Action:      nbdb.ACLActionAllow,
				Direction:   nbdb.ACLDirectionToLport,
				ExternalIDs: nil,
				Log:         true,
				Match:       "match",
				Meter:       &aclMeter,
				Name:        &aclName,
				Options:     map[string]string{"key": "value"},
				Priority:    1,
				Severity:    &aclSev,
			},
		},
		{
			desc:       "updates Tiers to tier2",
			initialACL: initialACL,
			finalACL: &nbdb.ACL{
				Action:      nbdb.ACLActionAllow,
				Direction:   nbdb.ACLDirectionToLport,
				ExternalIDs: nil,
				Log:         true,
				Match:       "match",
				Meter:       &aclMeter,
				Name:        &aclName,
				Options:     map[string]string{"key": "value"},
				Priority:    1,
				Severity:    &aclSev,
				Tier:        2, // default tier
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					tt.initialACL,
				},
			}, nil)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			t.Cleanup(cleanup.Cleanup)

			// test update with UUID set
			initialACLs, err := FindACLs(nbClient, []*nbdb.ACL{{
				ExternalIDs: tt.initialACL.ExternalIDs,
			}})
			if err != nil {
				t.Fatalf("test: \"%s\" failed to find initial ACL: %v", tt.desc, err)
			}
			if len(initialACLs) != 1 {
				t.Fatalf("test: \"%s\" found %d intitial ACls, expected 1", tt.desc, len(initialACLs))
			}
			tt.finalACL.UUID = initialACLs[0].UUID

			err = CreateOrUpdateACLs(nbClient, tt.finalACL)
			if err != nil {
				t.Fatalf("test: \"%s\" failed to set up test harness: %v", tt.desc, err)
			}
			matcher := libovsdbtest.HaveData([]libovsdbtest.TestData{tt.finalACL})
			success, err := matcher.Match(nbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tt.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tt.desc, err))
			}
		})
	}
}
