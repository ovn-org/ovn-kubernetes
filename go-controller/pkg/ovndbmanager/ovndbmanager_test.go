package ovndbmanager

import (
	"fmt"
	"strings"
	"testing"
)

type mockRes struct {
	res    string
	stderr string
	err    error
	called bool
}

const status_template = `87f0
Name: OVN_Northbound
Cluster ID: f832 (f832bbff-e28c-4656-83f0-075e91a7ab8f)
Server ID: 87f0 (87f0d686-8a8d-4585-9513-45efac449101)
Address: ssl:10.1.1.185:9643
Status: cluster member
Role: %s
Term: 4
Leader: bbf6
Vote: unknown

Election timer: %s
Log: [19418, 26772]
Entries not yet committed: 0
Entries not yet applied: 0
Connections: ->bbf6 ->ad31 <-bbf6 <-ad31
Disconnections: 1
Servers:
    87f0 (87f0 at ssl:10.1.1.185:9643) (self)
    bbf6 (bbf6 at ssl:10.1.1.218:9643) last msg 2757 ms ago
    ad31 (ad31 at ssl:10.1.1.211:9643) last msg 153868958 ms ago`

func TestElectionTimer(t *testing.T) {
	var mockCalls map[string]*mockRes
	var unexpectedKeys []string
	mock := func(args ...string) (string, string, error) {
		key := keyForArgs(args...)
		res, ok := mockCalls[key]
		if !ok {
			unexpectedKeys = append(unexpectedKeys, key)
			return "", "key not found", fmt.Errorf("key not found")
		}
		res.called = true
		return res.res, res.stderr, res.err
	}

	db := &dbProperties{
		appCtl:                mock,
		dbName:                "OVN_Northbound",
		clusterStatusRetryCnt: &nbDbRetryCnt,
	}
	tests := []struct {
		desc         string
		mockCalls    map[string]*mockRes
		timeout      int
		role         string
		currentTimer string
	}{
		{
			"Follower, not trying to change",
			map[string]*mockRes{},
			1000,
			"follower",
			"10000",
		},
		{
			"leader, timer doesn't change",
			map[string]*mockRes{},
			1000,
			"leader",
			"1000",
		},
		{
			"leader, timer must change",
			map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "2000"): {
					res:    "change of election timer initiated",
					stderr: "",
					err:    nil,
				},
			},
			2000,
			"leader",
			"1500",
		},
		{
			"leader, timer must change but desired is more than double",
			map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "3000"): {
					res:    "change of election timer initiated",
					stderr: "",
					err:    nil,
				},
			},
			5000,
			"leader",
			"1500",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			mockCalls = map[string]*mockRes{
				keyForArgs("cluster/status", "OVN_Northbound"): {
					res:    fmt.Sprintf(status_template, tc.role, tc.currentTimer),
					stderr: "",
					err:    nil,
				},
			}
			for k, v := range tc.mockCalls {
				mockCalls[k] = v
			}

			unexpectedKeys = make([]string, 0)
			db.electionTimer = tc.timeout
			ensureElectionTimeout(db)
			for k, c := range tc.mockCalls {
				if !c.called {
					t.Errorf("Expecting call with args %s", k)
				}
			}
			if len(unexpectedKeys) > 0 {
				t.Errorf("Received unexpected calls %v", unexpectedKeys)
			}
		})
	}
}

func keyForArgs(args ...string) string {
	return strings.Join(args, "-")
}
